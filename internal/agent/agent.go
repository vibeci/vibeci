// Package agent runs an LLM tool-use loop with budgets, context trimming and
// an audit transcript. Tools are plain Go functions; the agent never gets
// capabilities beyond the tools it is handed.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vibeci/vibeci/internal/llm"
)

// Tool is a function exposed to the model.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	// Terminal tools end the loop when they return a non-error Result.
	Terminal bool
	// Run executes the tool. Return Result{IsError: true} for failures the
	// model should see and react to; return a Go error only for
	// infrastructure failures that must abort the run.
	Run func(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result of a tool call.
type Result struct {
	Output  string
	IsError bool
}

// Errorf builds an error Result.
func Errorf(format string, args ...any) Result {
	return Result{Output: fmt.Sprintf(format, args...), IsError: true}
}

// OK builds a success Result.
func OK(s string) Result { return Result{Output: s} }

// Config of a run.
type Config struct {
	Name     string // for logs and transcripts, e.g. "resolve"
	Client   llm.Client
	System   string
	Tools    []*Tool
	MaxTurns int
	Timeout  time.Duration
	// MaxToolOutput caps a single tool result in bytes (default 40000).
	MaxToolOutput int
	// MaxNudges is how many times the model may stop without calling a tool
	// before the run fails (default 3).
	MaxNudges int
	// ToolChoice for the first turn (e.g. "tool:submit_review").
	ToolChoice string
	// MaxInputTokens aborts the run when cumulative input tokens exceed it
	// (0 = unlimited).
	MaxInputTokens int
	Transcript     io.Writer
	Logger         *slog.Logger
}

// Outcome of a run.
type Outcome struct {
	Terminal string          // name of the terminal tool that ended the run
	Input    json.RawMessage // its input
	Output   string          // its result output
	Turns    int
	Usage    llm.Usage
	Messages []llm.Message
}

// ErrBudget is returned when a turn, time or token budget is exhausted.
var ErrBudget = errors.New("agent budget exhausted")

// ErrNoResult is returned when the model refuses or stops without calling a
// terminal tool.
var ErrNoResult = errors.New("agent produced no result")

// Inconclusive reports whether err means the model could not finish the
// task (as opposed to an infrastructure failure worth retrying later).
func Inconclusive(err error) bool {
	return errors.Is(err, ErrBudget) || errors.Is(err, ErrNoResult)
}

// Run executes the loop starting from the given user content.
func Run(ctx context.Context, cfg Config, initial []llm.Block) (*Outcome, error) {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 50
	}
	if cfg.MaxToolOutput <= 0 {
		cfg.MaxToolOutput = 40000
	}
	if cfg.MaxNudges <= 0 {
		cfg.MaxNudges = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	tr := newTranscript(cfg.Transcript)
	tools := map[string]*Tool{}
	var specs []llm.Tool
	for _, t := range cfg.Tools {
		tools[t.Name] = t
		specs = append(specs, llm.Tool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	var terminalNames []string
	for _, t := range cfg.Tools {
		if t.Terminal {
			terminalNames = append(terminalNames, t.Name)
		}
	}

	out := &Outcome{}
	msgs := []llm.Message{{Role: llm.User, Content: initial}}
	tr.write("start", map[string]any{"agent": cfg.Name, "model": cfg.Client.Name(), "system": cfg.System, "initial": initial})
	nudges := 0
	toolChoice := cfg.ToolChoice

	for turn := 1; ; turn++ {
		if turn > cfg.MaxTurns {
			tr.write("abort", map[string]any{"reason": "max turns"})
			return out, fmt.Errorf("%w: %d turns", ErrBudget, cfg.MaxTurns)
		}
		if err := ctx.Err(); err != nil {
			tr.write("abort", map[string]any{"reason": err.Error()})
			if errors.Is(err, context.DeadlineExceeded) {
				return out, fmt.Errorf("%w: timeout", ErrBudget)
			}
			return out, err
		}
		if cfg.MaxInputTokens > 0 && out.Usage.InputTokens > cfg.MaxInputTokens {
			tr.write("abort", map[string]any{"reason": "token budget"})
			return out, fmt.Errorf("%w: %d input tokens", ErrBudget, out.Usage.InputTokens)
		}
		trimContext(msgs, cfg.System, cfg.Client.ContextWindow())

		resp, err := cfg.Client.Complete(ctx, &llm.Request{System: cfg.System, Messages: msgs, Tools: specs, ToolChoice: toolChoice})
		if err != nil {
			tr.write("error", map[string]any{"error": err.Error()})
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return out, fmt.Errorf("%w: timeout", ErrBudget)
			}
			return out, fmt.Errorf("llm call: %w", err)
		}
		toolChoice = ""
		out.Turns = turn
		out.Usage.Add(resp.Usage)
		tr.write("response", map[string]any{"turn": turn, "stop": resp.StopReason, "raw_stop": resp.RawStop, "usage": resp.Usage, "content": redactReasoning(resp.Content)})
		msgs = append(msgs, llm.Message{Role: llm.Assistant, Content: resp.Content})
		out.Messages = msgs

		uses := resp.ToolUses()
		if len(uses) == 0 {
			if resp.StopReason == llm.StopRefusal {
				return out, fmt.Errorf("%w: model refused: %s", ErrNoResult, truncate(resp.TextContent(), 500))
			}
			nudges++
			if nudges > cfg.MaxNudges {
				return out, fmt.Errorf("%w: model stopped without calling %s", ErrNoResult, strings.Join(terminalNames, "/"))
			}
			note := "Continue working using the tools."
			if resp.StopReason == llm.StopMaxTokens {
				note = "Your reply hit the output token limit. Continue, using smaller steps."
			}
			if len(terminalNames) > 0 {
				note += fmt.Sprintf(" When you are done you must call %s; plain text replies are not read by anyone.", strings.Join(terminalNames, " or "))
			}
			msgs = append(msgs, llm.Message{Role: llm.User, Content: []llm.Block{llm.Text(note)}})
			continue
		}

		var results []llm.Block
		var finished *Outcome
		for _, u := range uses {
			res, err := runTool(ctx, tools, u)
			if err != nil {
				tr.write("abort", map[string]any{"reason": "tool infrastructure error", "tool": u.Name, "error": err.Error()})
				return out, fmt.Errorf("tool %s: %w", u.Name, err)
			}
			output := capOutput(res.Output, cfg.MaxToolOutput)
			tr.write("tool", map[string]any{"turn": turn, "name": u.Name, "input": json.RawMessage(u.Input), "is_error": res.IsError, "output": output})
			cfg.Logger.Debug("tool call", "agent", cfg.Name, "tool", u.Name, "error", res.IsError, "bytes", len(res.Output))
			if t := tools[u.Name]; t != nil && t.Terminal && !res.IsError && finished == nil {
				finished = &Outcome{Terminal: u.Name, Input: u.Input, Output: res.Output}
				// Remaining tool calls in this turn are not executed.
				results = append(results, llm.ToolResult(u.ID, output, false))
				continue
			}
			if finished != nil {
				results = append(results, llm.ToolResult(u.ID, "not executed: the run already finished", true))
				continue
			}
			results = append(results, llm.ToolResult(u.ID, output, res.IsError))
		}
		msgs = append(msgs, llm.Message{Role: llm.User, Content: results})
		out.Messages = msgs
		if finished != nil {
			out.Terminal, out.Input, out.Output = finished.Terminal, finished.Input, finished.Output
			tr.write("finish", map[string]any{"tool": out.Terminal, "turns": out.Turns, "usage": out.Usage})
			return out, nil
		}
		if resp.StopReason == llm.StopMaxTokens {
			msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, llm.Text("Note: your previous reply was cut off by the output token limit; keep tool inputs smaller (e.g. edit files in pieces)."))
		}
	}
}

func runTool(ctx context.Context, tools map[string]*Tool, u llm.Block) (res Result, err error) {
	t := tools[u.Name]
	if t == nil {
		return Errorf("unknown tool %q", u.Name), nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(u.Input, &probe) == nil {
		if bad, ok := probe["__invalid_json__"]; ok {
			return Errorf("your tool input was not valid JSON (it may have been cut off by the output limit): %s", truncate(string(bad), 300)), nil
		}
	}
	defer func() {
		if r := recover(); r != nil {
			res, err = Errorf("tool panicked: %v", r), nil
		}
	}()
	return t.Run(ctx, u.Input)
}

// Decode unmarshals tool input strictly into v.
func Decode(input json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(input)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}
	return nil
}

func capOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 200
	return s[:head] + fmt.Sprintf("\n\n[... %d bytes omitted ...]\n\n", len(s)-head-tail) + s[len(s)-tail:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// trimContext elides old tool results when the conversation approaches the
// context window. The most recent results are kept intact.
func trimContext(msgs []llm.Message, system string, window int) {
	if window <= 0 {
		return
	}
	budget := window * 6 / 10
	if llm.EstimateMessages(system, msgs) <= budget {
		return
	}
	const keepRecent = 8
	var idx [][2]int
	for i, m := range msgs {
		for j, b := range m.Content {
			if b.Type == llm.ToolResultBlock && len(b.Text) > 600 {
				idx = append(idx, [2]int{i, j})
			}
		}
	}
	for k := 0; k < len(idx)-keepRecent; k++ {
		i, j := idx[k][0], idx[k][1]
		b := &msgs[i].Content[j]
		b.Text = b.Text[:300] + "\n[... older tool output elided to save context; re-run the tool if you need it ...]"
		if llm.EstimateMessages(system, msgs) <= budget {
			return
		}
	}
}

// redactReasoning drops opaque reasoning payloads from transcripts (they are
// large, encrypted, and useless to a human reader).
func redactReasoning(blocks []llm.Block) []llm.Block {
	out := make([]llm.Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == llm.ReasoningBlock {
			var th struct {
				Thinking string `json:"thinking"`
			}
			json.Unmarshal(b.Raw, &th)
			out = append(out, llm.Block{Type: llm.ReasoningBlock, Format: b.Format, Text: th.Thinking})
			continue
		}
		out = append(out, b)
	}
	return out
}

type transcript struct {
	mu sync.Mutex
	w  io.Writer
}

func newTranscript(w io.Writer) *transcript { return &transcript{w: w} }

func (t *transcript) write(kind string, fields map[string]any) {
	if t.w == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["kind"] = kind
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	t.w.Write(append(b, '\n'))
}
