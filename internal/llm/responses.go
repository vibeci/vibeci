package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
)

// Responses implements the OpenAI Responses API in stateless mode
// (store=false, encrypted reasoning replayed by the client).
type Responses struct {
	t      transport
	spec   ProviderSpec
	opts   ModelOptions
	stream bool
}

// NewResponses returns an OpenAI Responses API client.
func NewResponses(spec ProviderSpec, opts ModelOptions, logger *slog.Logger) *Responses {
	return &Responses{t: newTransport(spec, opts.Name, logger), spec: spec, opts: opts, stream: spec.Stream}
}

func (o *Responses) Name() string       { return o.opts.Name }
func (o *Responses) ContextWindow() int { return o.opts.ContextWindow }

type respTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type respRequest struct {
	Model           string            `json:"model"`
	Instructions    string            `json:"instructions,omitempty"`
	Input           []json.RawMessage `json:"input"`
	Tools           []respTool        `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Reasoning       map[string]any    `json:"reasoning,omitempty"`
	Include         []string          `json:"include,omitempty"`
	Store           bool              `json:"store"`
	Stream          bool              `json:"stream,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
}

func (o *Responses) reasoningOn() bool {
	return o.opts.Effort != "" || o.opts.thinkingOn()
}

func (o *Responses) buildRequest(req *Request) ([]byte, error) {
	maxTokens := o.opts.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}
	rr := respRequest{
		Model:           o.opts.Model,
		Instructions:    req.System,
		MaxOutputTokens: maxTokens,
		Store:           false,
		Stream:          o.stream,
	}
	if o.reasoningOn() {
		effort := o.opts.Effort
		if effort == "" {
			effort = "medium"
		}
		rr.Reasoning = map[string]any{"effort": effort}
		rr.Include = []string{"reasoning.encrypted_content"}
	} else if o.opts.Temperature != nil {
		rr.Temperature = o.opts.Temperature
	}
	for _, t := range req.Tools {
		rr.Tools = append(rr.Tools, respTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: rawObject(t.InputSchema)})
	}
	if len(req.Tools) > 0 {
		switch {
		case req.ToolChoice == ToolChoiceAny:
			rr.ToolChoice = "required"
		case req.ToolChoice == ToolChoiceNone:
			rr.ToolChoice = "none"
		case strings.HasPrefix(req.ToolChoice, "tool:"):
			rr.ToolChoice = map[string]any{"type": "function", "name": strings.TrimPrefix(req.ToolChoice, "tool:")}
		}
	}
	add := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		rr.Input = append(rr.Input, raw)
		return nil
	}
	for _, m := range req.Messages {
		switch m.Role {
		case User:
			var texts []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case ToolResultBlock:
					if err := add(map[string]any{"type": "function_call_output", "call_id": b.ID, "output": nonEmpty(b.Text, "(no output)")}); err != nil {
						return nil, err
					}
				case TextBlock:
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, map[string]any{"type": "input_text", "text": b.Text})
					}
				}
			}
			if len(texts) > 0 {
				if err := add(map[string]any{"role": "user", "content": texts}); err != nil {
					return nil, err
				}
			}
		case Assistant:
			for _, b := range m.Content {
				var err error
				switch b.Type {
				case ReasoningBlock:
					if b.Format == "openai-responses" && len(b.Raw) > 0 {
						rr.Input = append(rr.Input, b.Raw)
					}
				case TextBlock:
					if strings.TrimSpace(b.Text) != "" {
						err = add(map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": b.Text}}})
					}
				case ToolUseBlock:
					err = add(map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": string(rawObject(b.Input))})
				}
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return json.Marshal(rr)
}

// Complete implements Client.
func (o *Responses) Complete(ctx context.Context, req *Request) (*Response, error) {
	body, err := o.buildRequest(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(o.spec.BaseURL, "/") + "/responses"
	headers := map[string]string{}
	if o.spec.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.spec.APIKey
	}
	if o.stream {
		headers["Accept"] = "text/event-stream"
	}
	return o.t.call(ctx, func(ctx context.Context) (*Response, error) {
		resp, err := o.t.post(ctx, url, body, headers)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var rr respResponse
		if o.stream {
			if err := parseResponsesStream(resp.Body, &rr); err != nil {
				return nil, err
			}
		} else if err := readJSON(resp.Body, &rr); err != nil {
			return nil, err
		}
		return rr.toResponse()
	})
}

type respResponse struct {
	Model  string            `json:"model"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// permanentResponsesError reports failure codes that retrying cannot fix
// (invalid prompts or images, content policy). Others — server_error,
// rate_limit_exceeded, timeouts, proxy-specific codes — are transient.
func permanentResponsesError(code string) bool {
	return strings.HasPrefix(code, "invalid_") || strings.Contains(code, "image") || strings.Contains(code, "content_policy")
}

func (rr *respResponse) toResponse() (*Response, error) {
	if rr.Status == "failed" {
		msg := "response failed"
		if rr.Error != nil {
			msg = rr.Error.Code + ": " + rr.Error.Message
		}
		return nil, &APIError{Status: 200, Message: msg, Retryable: rr.Error == nil || !permanentResponsesError(rr.Error.Code)}
	}
	out := &Response{
		Model: rr.Model,
		Usage: Usage{InputTokens: rr.Usage.InputTokens, OutputTokens: rr.Usage.OutputTokens, CacheReadTokens: rr.Usage.InputTokensDetails.CachedTokens},
	}
	refused := false
	for _, raw := range rr.Output {
		var item struct {
			Type             string          `json:"type"`
			ID               string          `json:"id"`
			CallID           string          `json:"call_id"`
			Name             string          `json:"name"`
			Arguments        string          `json:"arguments"`
			Summary          json.RawMessage `json:"summary"`
			EncryptedContent string          `json:"encrypted_content"`
			Content          []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "reasoning":
			if item.EncryptedContent == "" {
				continue // cannot be replayed statelessly
			}
			summary := item.Summary
			if len(summary) == 0 || string(summary) == "null" {
				summary = json.RawMessage("[]")
			}
			replay, _ := json.Marshal(map[string]any{"type": "reasoning", "id": item.ID, "summary": summary, "encrypted_content": item.EncryptedContent})
			out.Content = append(out.Content, Block{Type: ReasoningBlock, Format: "openai-responses", Raw: replay})
		case "message":
			for _, c := range item.Content {
				switch c.Type {
				case "output_text":
					out.Content = append(out.Content, Text(c.Text))
				case "refusal":
					refused = true
					out.Content = append(out.Content, Text("[refusal] "+c.Refusal))
				}
			}
		case "function_call":
			b := Block{Type: ToolUseBlock, ID: item.CallID, Name: item.Name, Input: json.RawMessage(item.Arguments)}
			if !json.Valid(b.Input) {
				b.Input, _ = json.Marshal(map[string]string{"__invalid_json__": item.Arguments})
			}
			out.Content = append(out.Content, b)
		}
	}
	switch {
	case len(out.ToolUses()) > 0:
		out.StopReason = StopToolUse
	case rr.Status == "incomplete" && rr.IncompleteDetails != nil && rr.IncompleteDetails.Reason == "max_output_tokens":
		out.StopReason = StopMaxTokens
	case refused:
		out.StopReason = StopRefusal
	case rr.Status == "incomplete":
		out.StopReason = StopOther
	default:
		out.StopReason = StopEndTurn
	}
	out.RawStop = rr.Status
	if rr.IncompleteDetails != nil {
		out.RawStop += ":" + rr.IncompleteDetails.Reason
	}
	return out, nil
}

func parseResponsesStream(r io.Reader, rr *respResponse) error {
	var items []json.RawMessage
	got := false
	err := readSSE(r, func(ev sseEvent) error {
		var head struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Item     json.RawMessage `json:"item"`
			Message  string          `json:"message"`
			Code     string          `json:"code"`
		}
		if err := json.Unmarshal(ev.Data, &head); err != nil {
			return nil
		}
		switch head.Type {
		case "response.output_item.done":
			if len(head.Item) > 0 {
				items = append(items, head.Item)
			}
		case "response.completed", "response.incomplete", "response.failed":
			if err := json.Unmarshal(head.Response, rr); err != nil {
				return err
			}
			got = true
			return errStopStream
		case "error":
			return &APIError{Status: 200, Message: head.Code + ": " + head.Message, Retryable: true}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !got {
		return transient(errors.New("responses stream ended without a terminal event"))
	}
	if len(rr.Output) == 0 && len(items) > 0 {
		rr.Output = items
	}
	return nil
}
