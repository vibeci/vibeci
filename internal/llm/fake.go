package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Fake is a scripted Client for tests. Each call pops the next response; a
// Func can instead compute responses from the request.
type Fake struct {
	mu        sync.Mutex
	NameValue string
	Responses []*Response
	Func      func(req *Request) (*Response, error)
	Requests  []*Request
}

func (f *Fake) Name() string {
	if f.NameValue == "" {
		return "fake"
	}
	return f.NameValue
}

func (f *Fake) ContextWindow() int { return 200000 }

func (f *Fake) Complete(_ context.Context, req *Request) (*Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *req
	cp.Messages = append([]Message(nil), req.Messages...)
	f.Requests = append(f.Requests, &cp)
	if f.Func != nil {
		return f.Func(req)
	}
	if len(f.Responses) == 0 {
		return nil, fmt.Errorf("fake: no scripted responses left")
	}
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	return r, nil
}

// ToolCall builds a scripted tool_use response.
func ToolCall(id, name string, input any) *Response {
	raw, _ := json.Marshal(input)
	return &Response{StopReason: StopToolUse, Content: []Block{{Type: ToolUseBlock, ID: id, Name: name, Input: raw}}}
}

// Say builds a scripted text-only response.
func Say(text string) *Response {
	return &Response{StopReason: StopEndTurn, Content: []Block{Text(text)}}
}
