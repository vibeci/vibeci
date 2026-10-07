package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

var testTools = []Tool{{Name: "calc", Description: "math", InputSchema: json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`)}}

func conversation() []Message {
	return []Message{
		{Role: User, Content: []Block{Text("is 9991 prime?")}},
		{Role: Assistant, Content: []Block{
			{Type: ReasoningBlock, Format: "anthropic", Raw: json.RawMessage(`{"type":"thinking","thinking":"","signature":"SIG"}`)},
			{Type: ReasoningBlock, Format: "openai-responses", Raw: json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"ENC"}`)},
			Text("let me check"),
			{Type: ToolUseBlock, ID: "call_1", Name: "calc", Input: json.RawMessage(`{"expr":"9991/97"}`)},
		}},
		{Role: User, Content: []Block{Text("extra note"), ToolResult("call_1", "103", false)}},
	}
}

func TestAnthropicRequestShape(t *testing.T) {
	a := NewAnthropic(ProviderSpec{Auth: "bearer", APIKey: "k"}, ModelOptions{Model: "m", MaxTokens: 100, Thinking: "adaptive", Effort: "high", PromptCache: true}, nil)
	body, err := a.buildRequest(&Request{System: "sys", Messages: conversation(), Tools: testTools, ToolChoice: ToolChoiceAny})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal(body, &v)
	if v["tool_choice"] != nil {
		t.Errorf("forced tool choice must be dropped when thinking: %v", v["tool_choice"])
	}
	if v["thinking"].(map[string]any)["type"] != "adaptive" || v["output_config"].(map[string]any)["effort"] != "high" {
		t.Errorf("thinking/effort missing: %s", body)
	}
	msgs := v["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 3 || asst[0].(map[string]any)["signature"] != "SIG" {
		t.Errorf("assistant content should keep only anthropic reasoning + text + tool_use: %v", asst)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if last[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("tool_result must come first: %v", last)
	}
	if last[1].(map[string]any)["cache_control"] == nil {
		t.Errorf("rolling cache breakpoint missing on last block: %v", last)
	}
	if v["system"].([]any)[0].(map[string]any)["cache_control"] == nil {
		t.Error("system cache breakpoint missing")
	}
}

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"m1","model":"claude-x","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"thinking":"","type":"thinking"},"type":"content_block_start"}

event: content_block_delta
data: {"index":0,"delta":{"signature":"AB","type":"signature_delta"},"type":"content_block_delta"}

event: content_block_delta
data: {"index":0,"delta":{"signature":"CD","type":"signature_delta"},"type":"content_block_delta"}

event: ping
data: {"type": "ping"}

event: content_block_start
data: {"index":1,"content_block":{"text":"","type":"text"},"type":"content_block_start"}

event: content_block_delta
data: {"index":1,"delta":{"text":"hello ","type":"text_delta"},"type":"content_block_delta"}

event: content_block_delta
data: {"index":1,"delta":{"text":"world","type":"text_delta"},"type":"content_block_delta"}

event: content_block_start
data: {"index":2,"content_block":{"id":"toolu_1","name":"calc","input":{},"type":"tool_use"},"type":"content_block_start"}

event: content_block_delta
data: {"index":2,"delta":{"partial_json":"{\"expr\":","type":"input_json_delta"},"type":"content_block_delta"}

event: content_block_delta
data: {"index":2,"delta":{"partial_json":" \"1+1\"}","type":"input_json_delta"},"type":"content_block_delta"}

event: message_delta
data: {"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42,"input_tokens":10},"type":"message_delta"}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicStreamParse(t *testing.T) {
	resp, err := parseAnthropicStream(strings.NewReader(anthropicStream))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || resp.Usage.OutputTokens != 42 || resp.Usage.InputTokens != 10 {
		t.Errorf("stop/usage: %+v", resp)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("blocks: %+v", resp.Content)
	}
	var th map[string]string
	json.Unmarshal(resp.Content[0].Raw, &th)
	if th["signature"] != "ABCD" || th["type"] != "thinking" {
		t.Errorf("thinking block: %s", resp.Content[0].Raw)
	}
	if resp.Content[1].Text != "hello world" {
		t.Errorf("text: %q", resp.Content[1].Text)
	}
	tu := resp.Content[2]
	if tu.Name != "calc" || tu.ID != "toolu_1" || string(tu.Input) != `{"expr": "1+1"}` {
		t.Errorf("tool use: %+v %s", tu, tu.Input)
	}
	// Truncated stream must be retryable.
	_, err = parseAnthropicStream(strings.NewReader(anthropicStream[:400]))
	var re *retryableError
	if err == nil || !errorsAs(err, &re) {
		t.Errorf("truncated stream should be a retryable error, got %v", err)
	}
}

func errorsAs(err error, target any) bool {
	switch tt := target.(type) {
	case **retryableError:
		for e := err; e != nil; {
			if r, ok := e.(*retryableError); ok {
				*tt = r
				return true
			}
			u, ok := e.(interface{ Unwrap() error })
			if !ok {
				return false
			}
			e = u.Unwrap()
		}
	}
	return false
}

func TestAnthropicRetryAndAuth(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers: %v", r.Header)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(529)
			io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		io.WriteString(w, `{"model":"m","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":5}}`)
	}))
	defer srv.Close()
	a := NewAnthropic(ProviderSpec{BaseURL: srv.URL + "/v1", Auth: "bearer", APIKey: "tok", MaxRetries: 2}, ModelOptions{Model: "m", MaxTokens: 10}, nil)
	resp, err := a.Complete(context.Background(), &Request{Messages: []Message{{Role: User, Content: []Block{Text("ping")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TextContent() != "pong" || calls.Load() != 2 || resp.Usage.InputTokens != 8 {
		t.Errorf("resp=%+v calls=%d", resp, calls.Load())
	}
}

// TestStreamErrorRetry: an error event inside an accepted stream is retried
// unless its type is permanent, including types no vendor documents (a
// gateway's own).
func TestStreamErrorRetry(t *testing.T) {
	for _, tc := range []struct {
		typ       string
		wantCalls int32
	}{{"upstream_error", 2}, {"overloaded_error", 2}, {"invalid_request_error", 1}} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if calls.Add(1) == 1 {
				fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"upstream timed out\"}}\n\n", tc.typ)
				return
			}
			io.WriteString(w, anthropicStream)
		}))
		a := NewAnthropic(ProviderSpec{BaseURL: srv.URL, APIKey: "k", MaxRetries: 2, Stream: true}, ModelOptions{Model: "m", MaxTokens: 10}, nil)
		_, err := a.Complete(context.Background(), &Request{Messages: []Message{{Role: User, Content: []Block{Text("x")}}}})
		srv.Close()
		if calls.Load() != tc.wantCalls || (tc.wantCalls == 2) != (err == nil) {
			t.Errorf("%s: calls=%d err=%v", tc.typ, calls.Load(), err)
		}
	}
	for code, want := range map[string]bool{"server_error": true, "upstream_error": true, "timeout": true, "invalid_prompt": false, "image_too_large": false} {
		rr := &respResponse{Status: "failed", Error: &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: code}}
		_, err := rr.toResponse()
		if ae, ok := err.(*APIError); !ok || ae.Retryable != want {
			t.Errorf("response.failed %s: %v (want retryable=%v)", code, err, want)
		}
	}
}

func TestNonRetryableError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		io.WriteString(w, `{"errorCode":"INVALID_ARGUMENT","errorName":"LanguageModelService:InvalidRequest","parameters":{"unsafeParams":"{unrecognizedProperty=bogus}","message":"Request contained an unrecognized field"}}`)
	}))
	defer srv.Close()
	a := NewAnthropic(ProviderSpec{BaseURL: srv.URL, APIKey: "k", MaxRetries: 3}, ModelOptions{Model: "m", MaxTokens: 10}, nil)
	_, err := a.Complete(context.Background(), &Request{Messages: []Message{{Role: User, Content: []Block{Text("x")}}}})
	if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "unrecognized field") {
		t.Errorf("err=%v calls=%d", err, calls.Load())
	}
}

func TestResponsesRequestAndParse(t *testing.T) {
	o := NewResponses(ProviderSpec{}, ModelOptions{Model: "gpt", MaxTokens: 50, Effort: "low"}, nil)
	body, err := o.buildRequest(&Request{System: "sys", Messages: conversation(), Tools: testTools, ToolChoice: ToolChoiceAny})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Input      []map[string]any `json:"input"`
		ToolChoice any              `json:"tool_choice"`
		Store      bool             `json:"store"`
		Include    []string         `json:"include"`
	}
	json.Unmarshal(body, &v)
	if v.ToolChoice != "required" || v.Store || len(v.Include) != 1 {
		t.Errorf("request: %s", body)
	}
	var types []string
	for _, it := range v.Input {
		if ty, ok := it["type"].(string); ok {
			types = append(types, ty)
		} else {
			types = append(types, "role:"+it["role"].(string))
		}
	}
	want := "role:user,reasoning,message,function_call,function_call_output,role:user"
	if strings.Join(types, ",") != want {
		t.Errorf("input order = %v, want %s", types, want)
	}

	stream := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"calc\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt\",\"output\":[" +
		"{\"type\":\"reasoning\",\"id\":\"rs_9\",\"summary\":[],\"encrypted_content\":\"E\",\"status\":\"completed\"}," +
		"{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"calc\",\"arguments\":\"{\\\"expr\\\":\\\"2\\\"}\"}]," +
		"\"usage\":{\"input_tokens\":7,\"output_tokens\":9}}}\n\n"
	var rr respResponse
	if err := parseResponsesStream(strings.NewReader(stream), &rr); err != nil {
		t.Fatal(err)
	}
	resp, err := rr.toResponse()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || len(resp.Content) != 2 || resp.Content[0].Format != "openai-responses" {
		t.Fatalf("resp: %+v", resp)
	}
	if strings.Contains(string(resp.Content[0].Raw), "status") {
		t.Errorf("reasoning replay should drop status: %s", resp.Content[0].Raw)
	}
	if resp.Content[1].ID != "c1" || string(resp.Content[1].Input) != `{"expr":"2"}` {
		t.Errorf("function call: %+v", resp.Content[1])
	}
}

func TestChatRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages  []map[string]any `json:"messages"`
			MaxTokens int              `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		roles := []string{}
		for _, m := range body.Messages {
			roles = append(roles, m["role"].(string))
		}
		if strings.Join(roles, ",") != "system,user,assistant,tool,user" || body.MaxTokens != 77 {
			t.Errorf("roles=%v max=%d", roles, body.MaxTokens)
		}
		io.WriteString(w, `{"model":"x","choices":[{"message":{"content":null,"tool_calls":[{"id":"t9","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"3\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":6}}`)
	}))
	defer srv.Close()
	c := NewChat(ProviderSpec{BaseURL: srv.URL, MaxTokensField: "max_tokens"}, ModelOptions{Model: "x", MaxTokens: 77}, nil)
	resp, err := c.Complete(context.Background(), &Request{System: "s", Messages: conversation(), Tools: testTools})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || resp.ToolUses()[0].ID != "t9" {
		t.Errorf("resp %+v", resp)
	}
}

// TestErrorMessage: error bodies in the shapes providers and gateways use
// become one readable line; anything else is passed through, truncated.
func TestErrorMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`: "invalid_request_error: bad field",
		`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`:                  "rate_limit_exceeded: slow down",
		`{"error":"no such model"}`:               "no such model",
		`{"message":"the request was throttled"}`: "the request was throttled",
		`{"detail":"something else"}`:             `{"detail":"something else"}`,
		"plain text failure\n":                    "plain text failure",
	} {
		if got := errorMessage([]byte(body)); got != want {
			t.Errorf("errorMessage(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestSSEMultiline(t *testing.T) {
	var got []string
	err := readSSE(strings.NewReader(": comment\ndata: a\ndata: b\n\nevent: x\ndata: c\n"), func(ev sseEvent) error {
		got = append(got, fmt.Sprintf("%s=%s", ev.Event, ev.Data))
		return nil
	})
	if err != nil || strings.Join(got, "|") != "=a\nb|x=c" {
		t.Errorf("got %q err %v", got, err)
	}
}
