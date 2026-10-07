package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/internal/llm"
)

func echoTool() *Tool {
	return &Tool{
		Name:   "echo",
		Schema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}}}`),
		Run: func(ctx context.Context, in json.RawMessage) (Result, error) {
			var v struct {
				S string `json:"s"`
			}
			if err := Decode(in, &v); err != nil {
				return Errorf("%v", err), nil
			}
			return OK("echo:" + v.S), nil
		},
	}
}

func submitTool(accept func(string) bool) *Tool {
	return &Tool{
		Name:     "submit",
		Terminal: true,
		Schema:   json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		Run: func(ctx context.Context, in json.RawMessage) (Result, error) {
			var v struct {
				Answer string `json:"answer"`
			}
			if err := Decode(in, &v); err != nil {
				return Errorf("%v", err), nil
			}
			if !accept(v.Answer) {
				return Errorf("rejected: %s", v.Answer), nil
			}
			return OK("accepted"), nil
		},
	}
}

func TestRunToTerminal(t *testing.T) {
	fake := &llm.Fake{Responses: []*llm.Response{
		llm.ToolCall("1", "echo", map[string]string{"s": "hi"}),
		llm.Say("thinking out loud"), // nudge
		llm.ToolCall("2", "submit", map[string]string{"answer": "wrong"}),
		llm.ToolCall("3", "submit", map[string]string{"answer": "right"}),
	}}
	var tr bytes.Buffer
	out, err := Run(context.Background(), Config{
		Name: "t", Client: fake, System: "sys",
		Tools:      []*Tool{echoTool(), submitTool(func(s string) bool { return s == "right" })},
		Transcript: &tr,
	}, []llm.Block{llm.Text("go")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Terminal != "submit" || out.Turns != 4 || !strings.Contains(string(out.Input), "right") {
		t.Fatalf("outcome %+v", out)
	}
	// The rejection must have been shown to the model.
	last := fake.Requests[3].Messages
	found := false
	for _, m := range last {
		for _, b := range m.Content {
			if b.Type == llm.ToolResultBlock && b.IsError && strings.Contains(b.Text, "rejected: wrong") {
				found = true
			}
		}
	}
	if !found {
		t.Error("rejection not fed back to the model")
	}
	if !strings.Contains(tr.String(), `"kind":"finish"`) {
		t.Error("transcript missing finish record")
	}
}

func TestBudgetAndNudges(t *testing.T) {
	fake := &llm.Fake{Func: func(req *llm.Request) (*llm.Response, error) {
		return llm.ToolCall("x", "echo", map[string]string{"s": "loop"}), nil
	}}
	_, err := Run(context.Background(), Config{Client: fake, Tools: []*Tool{echoTool()}, MaxTurns: 3}, []llm.Block{llm.Text("go")})
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("expected budget error, got %v", err)
	}
	quiet := &llm.Fake{Func: func(req *llm.Request) (*llm.Response, error) { return llm.Say("done!"), nil }}
	_, err = Run(context.Background(), Config{Client: quiet, Tools: []*Tool{submitTool(func(string) bool { return true })}, MaxNudges: 2}, []llm.Block{llm.Text("go")})
	if err == nil || !strings.Contains(err.Error(), "without calling submit") {
		t.Fatalf("expected nudge failure, got %v", err)
	}
}

func TestUnknownToolAndBadInput(t *testing.T) {
	fake := &llm.Fake{Responses: []*llm.Response{
		llm.ToolCall("1", "nope", map[string]string{}),
		llm.ToolCall("2", "echo", map[string]any{"s": "a", "extra": 1}),
		{StopReason: llm.StopMaxTokens, Content: []llm.Block{{Type: llm.ToolUseBlock, ID: "3", Name: "echo", Input: json.RawMessage(`{"__invalid_json__":"{\"s\": \"tru"}`)}}},
		llm.ToolCall("4", "submit", map[string]string{"answer": "ok"}),
	}}
	out, err := Run(context.Background(), Config{Client: fake, Tools: []*Tool{echoTool(), submitTool(func(string) bool { return true })}}, []llm.Block{llm.Text("go")})
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type == llm.ToolResultBlock && b.IsError {
				errs = append(errs, b.Text)
			}
		}
	}
	if len(errs) != 3 || !strings.Contains(errs[0], "unknown tool") || !strings.Contains(errs[1], "unknown field") || !strings.Contains(errs[2], "not valid JSON") {
		t.Fatalf("errors: %q", errs)
	}
}

func TestInfrastructureErrorAborts(t *testing.T) {
	boom := &Tool{Name: "boom", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, in json.RawMessage) (Result, error) {
		return Result{}, errors.New("sandbox died")
	}}
	fake := &llm.Fake{Responses: []*llm.Response{llm.ToolCall("1", "boom", map[string]string{})}}
	_, err := Run(context.Background(), Config{Client: fake, Tools: []*Tool{boom}}, []llm.Block{llm.Text("go")})
	if err == nil || !strings.Contains(err.Error(), "sandbox died") {
		t.Fatalf("got %v", err)
	}
}

func TestTrimContext(t *testing.T) {
	big := strings.Repeat("x", 4000)
	var msgs []llm.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, llm.Message{Role: llm.User, Content: []llm.Block{llm.ToolResult("id", big, false)}})
	}
	trimContext(msgs, "", 10000) // budget 6000 tokens ~ 24000 bytes
	elided := 0
	for _, m := range msgs {
		if strings.Contains(m.Content[0].Text, "elided") {
			elided++
		}
	}
	if elided == 0 || strings.Contains(msgs[19].Content[0].Text, "elided") {
		t.Fatalf("elided=%d; newest must stay intact", elided)
	}
}
