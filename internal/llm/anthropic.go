package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
)

// Anthropic implements the Anthropic Messages API, as served by Anthropic
// and by compatible gateways (which often expect a bearer token instead of
// x-api-key).
type Anthropic struct {
	t      transport
	spec   ProviderSpec
	opts   ModelOptions
	stream bool
}

// NewAnthropic returns a Messages API client.
func NewAnthropic(spec ProviderSpec, opts ModelOptions, logger *slog.Logger) *Anthropic {
	return &Anthropic{t: newTransport(spec, opts.Name, logger), spec: spec, opts: opts, stream: spec.Stream}
}

func (a *Anthropic) Name() string       { return a.opts.Name }
func (a *Anthropic) ContextWindow() int { return a.opts.ContextWindow }

type antMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type antRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	System       []map[string]any  `json:"system,omitempty"`
	Messages     []antMessage      `json:"messages"`
	Tools        []map[string]any  `json:"tools,omitempty"`
	ToolChoice   map[string]any    `json:"tool_choice,omitempty"`
	Thinking     map[string]any    `json:"thinking,omitempty"`
	OutputConfig map[string]any    `json:"output_config,omitempty"`
	Temperature  *float64          `json:"temperature,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

func (a *Anthropic) buildRequest(req *Request) ([]byte, error) {
	maxTokens := a.opts.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}
	ar := antRequest{
		Model:     a.opts.Model,
		MaxTokens: maxTokens,
		Stream:    a.stream,
	}
	cacheCtl := map[string]any{"type": "ephemeral"}
	if req.System != "" {
		sys := map[string]any{"type": "text", "text": req.System}
		if a.opts.PromptCache {
			sys["cache_control"] = cacheCtl
		}
		ar.System = []map[string]any{sys}
	}
	for _, t := range req.Tools {
		ar.Tools = append(ar.Tools, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": rawObject(t.InputSchema),
		})
	}
	thinking := a.opts.thinkingOn()
	switch a.opts.Thinking {
	case "adaptive":
		ar.Thinking = map[string]any{"type": "adaptive"}
	case "enabled":
		budget := a.opts.ThinkingBudget
		if budget <= 0 {
			budget = min(8000, maxTokens/2)
		}
		ar.Thinking = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if a.opts.Effort != "" {
		ar.OutputConfig = map[string]any{"effort": a.opts.Effort}
	}
	if a.opts.Temperature != nil && !thinking {
		ar.Temperature = a.opts.Temperature
	}
	if len(req.Tools) > 0 {
		choice := req.ToolChoice
		// Extended thinking only supports auto/none tool choice.
		if thinking && choice != ToolChoiceNone {
			choice = ToolChoiceAuto
		}
		switch {
		case choice == ToolChoiceAny:
			ar.ToolChoice = map[string]any{"type": "any"}
		case choice == ToolChoiceNone:
			ar.ToolChoice = map[string]any{"type": "none"}
		case strings.HasPrefix(choice, "tool:"):
			ar.ToolChoice = map[string]any{"type": "tool", "name": strings.TrimPrefix(choice, "tool:")}
		}
	}

	for _, m := range req.Messages {
		am := antMessage{Role: string(m.Role)}
		// tool_result blocks must precede any other content in a user turn.
		blocks := orderToolResultsFirst(m.Content)
		for _, b := range blocks {
			var raw []byte
			var err error
			switch b.Type {
			case TextBlock:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				raw, err = json.Marshal(map[string]any{"type": "text", "text": b.Text})
			case ToolUseBlock:
				raw, err = json.Marshal(map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": rawObject(b.Input)})
			case ToolResultBlock:
				tr := map[string]any{"type": "tool_result", "tool_use_id": b.ID, "content": nonEmpty(b.Text, "(no output)")}
				if b.IsError {
					tr["is_error"] = true
				}
				raw, err = json.Marshal(tr)
			case ReasoningBlock:
				if b.Format != "anthropic" || len(b.Raw) == 0 {
					continue
				}
				raw = b.Raw
			}
			if err != nil {
				return nil, err
			}
			am.Content = append(am.Content, raw)
		}
		if len(am.Content) == 0 {
			am.Content = append(am.Content, json.RawMessage(`{"type":"text","text":"(empty)"}`))
		}
		ar.Messages = append(ar.Messages, am)
	}

	// Rolling prompt cache breakpoint on the last content block, so each
	// agent turn re-reads the previous prefix from cache.
	if a.opts.PromptCache && len(ar.Messages) > 0 {
		last := &ar.Messages[len(ar.Messages)-1]
		i := len(last.Content) - 1
		var blk map[string]any
		if err := json.Unmarshal(last.Content[i], &blk); err == nil {
			if t, _ := blk["type"].(string); t == "text" || t == "tool_result" || t == "tool_use" {
				blk["cache_control"] = cacheCtl
				if raw, err := json.Marshal(blk); err == nil {
					last.Content[i] = raw
				}
			}
		}
	}
	return json.Marshal(ar)
}

func orderToolResultsFirst(blocks []Block) []Block {
	hasResult := false
	for _, b := range blocks {
		if b.Type == ToolResultBlock {
			hasResult = true
			break
		}
	}
	if !hasResult {
		return blocks
	}
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == ToolResultBlock {
			out = append(out, b)
		}
	}
	for _, b := range blocks {
		if b.Type != ToolResultBlock {
			out = append(out, b)
		}
	}
	return out
}

func (a *Anthropic) headers() map[string]string {
	h := map[string]string{"anthropic-version": "2023-06-01"}
	if a.spec.Auth == "bearer" {
		h["Authorization"] = "Bearer " + a.spec.APIKey
	} else {
		h["x-api-key"] = a.spec.APIKey
	}
	if a.stream {
		h["Accept"] = "text/event-stream"
	}
	return h
}

// Complete implements Client.
func (a *Anthropic) Complete(ctx context.Context, req *Request) (*Response, error) {
	body, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(a.spec.BaseURL, "/") + "/messages"
	return a.t.call(ctx, func(ctx context.Context) (*Response, error) {
		resp, err := a.t.post(ctx, url, body, a.headers())
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if a.stream {
			return parseAnthropicStream(resp.Body)
		}
		var ar antResponse
		if err := readJSON(resp.Body, &ar); err != nil {
			return nil, err
		}
		return ar.toResponse()
	})
}

type antUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
}

func (u antUsage) toUsage() Usage {
	return Usage{InputTokens: u.InputTokens + u.CacheCreation + u.CacheRead, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheCreation}
}

type antResponse struct {
	Model      string            `json:"model"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      antUsage          `json:"usage"`
}

func (ar *antResponse) toResponse() (*Response, error) {
	out := &Response{Model: ar.Model, RawStop: ar.StopReason, StopReason: antStop(ar.StopReason), Usage: ar.Usage.toUsage()}
	for _, raw := range ar.Content {
		b, ok, err := antBlock(raw)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Content = append(out.Content, b)
		}
	}
	return out, nil
}

func antBlock(raw json.RawMessage) (Block, bool, error) {
	var head struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Block{}, false, fmt.Errorf("bad content block: %w", err)
	}
	switch head.Type {
	case "text":
		return Text(head.Text), true, nil
	case "tool_use":
		return Block{Type: ToolUseBlock, ID: head.ID, Name: head.Name, Input: rawObject(head.Input)}, true, nil
	case "thinking", "redacted_thinking":
		return Block{Type: ReasoningBlock, Format: "anthropic", Raw: append(json.RawMessage(nil), raw...)}, true, nil
	}
	return Block{}, false, nil
}

func antStop(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence", "pause_turn":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return StopMaxTokens
	case "refusal":
		return StopRefusal
	}
	return StopOther
}

type antStreamBlock struct {
	typ       string
	start     json.RawMessage
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	input     strings.Builder
}

// permanentAnthropicError reports error types that retrying cannot fix.
func permanentAnthropicError(typ string) bool {
	switch typ {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "request_too_large", "billing_error":
		return true
	}
	return false
}

func parseAnthropicStream(r io.Reader) (*Response, error) {
	blocks := map[int]*antStreamBlock{}
	out := &Response{}
	var usage antUsage
	done := false
	err := readSSE(r, func(ev sseEvent) error {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ev.Data, &head); err != nil {
			return nil // ignore malformed keepalives
		}
		switch head.Type {
		case "message_start":
			var v struct {
				Message struct {
					Model string   `json:"model"`
					Usage antUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return err
			}
			out.Model = v.Message.Model
			usage = v.Message.Usage
		case "content_block_start":
			var v struct {
				Index        int             `json:"index"`
				ContentBlock json.RawMessage `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return err
			}
			var t struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			json.Unmarshal(v.ContentBlock, &t)
			sb := &antStreamBlock{typ: t.Type, start: v.ContentBlock}
			sb.text.WriteString(t.Text)
			blocks[v.Index] = sb
		case "content_block_delta":
			var v struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					Signature   string `json:"signature"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return err
			}
			sb := blocks[v.Index]
			if sb == nil {
				return transient(errors.New("anthropic stream: delta for unknown block"))
			}
			switch v.Delta.Type {
			case "text_delta":
				sb.text.WriteString(v.Delta.Text)
			case "thinking_delta":
				sb.thinking.WriteString(v.Delta.Thinking)
			case "signature_delta":
				sb.signature.WriteString(v.Delta.Signature)
			case "input_json_delta":
				sb.input.WriteString(v.Delta.PartialJSON)
			}
		case "message_delta":
			var v struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage antUsage `json:"usage"`
			}
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return err
			}
			out.RawStop = v.Delta.StopReason
			usage.OutputTokens = max(usage.OutputTokens, v.Usage.OutputTokens)
			usage.InputTokens = max(usage.InputTokens, v.Usage.InputTokens)
			usage.CacheRead = max(usage.CacheRead, v.Usage.CacheRead)
			usage.CacheCreation = max(usage.CacheCreation, v.Usage.CacheCreation)
		case "message_stop":
			done = true
			return errStopStream
		case "error":
			var v struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(ev.Data, &v)
			// The request was accepted, so an error inside the stream is
			// transient unless its type says otherwise. Gateways send types
			// of their own.
			return &APIError{Status: 200, Message: v.Error.Type + ": " + v.Error.Message, Retryable: !permanentAnthropicError(v.Error.Type)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, transient(errors.New("anthropic stream ended before message_stop"))
	}
	idx := make([]int, 0, len(blocks))
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		sb := blocks[i]
		switch sb.typ {
		case "text":
			out.Content = append(out.Content, Text(sb.text.String()))
		case "tool_use":
			var t struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			json.Unmarshal(sb.start, &t)
			input := strings.TrimSpace(sb.input.String())
			if input == "" {
				input = "{}"
			}
			b := Block{Type: ToolUseBlock, ID: t.ID, Name: t.Name, Input: json.RawMessage(input)}
			if !json.Valid(b.Input) {
				// Truncated by max_tokens; keep the raw text so the agent can
				// report a precise error back to the model.
				b.Input, _ = json.Marshal(map[string]string{"__invalid_json__": input})
			}
			out.Content = append(out.Content, b)
		case "thinking":
			raw, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": sb.thinking.String(), "signature": sb.signature.String()})
			out.Content = append(out.Content, Block{Type: ReasoningBlock, Format: "anthropic", Raw: raw})
		case "redacted_thinking":
			out.Content = append(out.Content, Block{Type: ReasoningBlock, Format: "anthropic", Raw: append(json.RawMessage(nil), sb.start...)})
		}
	}
	out.StopReason = antStop(out.RawStop)
	out.Usage = usage.toUsage()
	return out, nil
}
