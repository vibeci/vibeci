package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
)

// Chat implements the OpenAI Chat Completions API, the lowest common
// denominator served by OpenRouter, vLLM, llama.cpp, Ollama, LiteLLM, etc.
type Chat struct {
	t    transport
	spec ProviderSpec
	opts ModelOptions
}

// NewChat returns a Chat Completions client.
func NewChat(spec ProviderSpec, opts ModelOptions, logger *slog.Logger) *Chat {
	return &Chat{t: newTransport(spec, opts.Name, logger), spec: spec, opts: opts}
}

func (c *Chat) Name() string       { return c.opts.Name }
func (c *Chat) ContextWindow() int { return c.opts.ContextWindow }

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

func strPtr(s string) *string { return &s }

func (c *Chat) buildRequest(req *Request) ([]byte, error) {
	maxTokens := c.opts.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}
	body := map[string]any{"model": c.opts.Model}
	field := c.spec.MaxTokensField
	if field == "" {
		field = "max_tokens"
	}
	body[field] = maxTokens
	if c.opts.Temperature != nil {
		body["temperature"] = *c.opts.Temperature
	}
	if c.opts.Effort != "" {
		body["reasoning_effort"] = c.opts.Effort
	}
	var msgs []chatMessage
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: strPtr(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case User:
			var texts []string
			for _, b := range m.Content {
				switch b.Type {
				case ToolResultBlock:
					out := nonEmpty(b.Text, "(no output)")
					if b.IsError {
						out = "ERROR: " + out
					}
					msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: b.ID, Content: strPtr(out)})
				case TextBlock:
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				}
			}
			if len(texts) > 0 {
				msgs = append(msgs, chatMessage{Role: "user", Content: strPtr(strings.Join(texts, "\n\n"))})
			}
		case Assistant:
			am := chatMessage{Role: "assistant"}
			var texts []string
			for _, b := range m.Content {
				switch b.Type {
				case TextBlock:
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				case ToolUseBlock:
					tc := chatToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(rawObject(b.Input))
					am.ToolCalls = append(am.ToolCalls, tc)
				case ReasoningBlock:
					if b.Format == "openai-chat" {
						var rc struct {
							ReasoningContent string `json:"reasoning_content"`
						}
						json.Unmarshal(b.Raw, &rc)
						am.ReasoningContent = rc.ReasoningContent
					}
				}
			}
			if len(texts) > 0 {
				am.Content = strPtr(strings.Join(texts, "\n\n"))
			}
			msgs = append(msgs, am)
		}
	}
	body["messages"] = msgs
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": rawObject(t.InputSchema)}})
		}
		body["tools"] = tools
		switch {
		case req.ToolChoice == ToolChoiceAny:
			body["tool_choice"] = "required"
		case req.ToolChoice == ToolChoiceNone:
			body["tool_choice"] = "none"
		case strings.HasPrefix(req.ToolChoice, "tool:"):
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": strings.TrimPrefix(req.ToolChoice, "tool:")}}
		}
	}
	return json.Marshal(body)
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          *string        `json:"content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
			ReasoningContent string         `json:"reasoning_content"`
			Refusal          *string        `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// Complete implements Client.
func (c *Chat) Complete(ctx context.Context, req *Request) (*Response, error) {
	body, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.spec.BaseURL, "/") + "/chat/completions"
	headers := map[string]string{}
	if c.spec.APIKey != "" {
		headers["Authorization"] = "Bearer " + c.spec.APIKey
	}
	return c.t.call(ctx, func(ctx context.Context) (*Response, error) {
		resp, err := c.t.post(ctx, url, body, headers)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var cr chatResponse
		if err := readJSON(resp.Body, &cr); err != nil {
			return nil, err
		}
		if len(cr.Choices) == 0 {
			return nil, transient(&APIError{Status: 200, Message: "no choices in response"})
		}
		ch := cr.Choices[0]
		out := &Response{
			Model:   cr.Model,
			RawStop: ch.FinishReason,
			Usage:   Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens, CacheReadTokens: cr.Usage.PromptTokensDetails.CachedTokens},
		}
		if ch.Message.ReasoningContent != "" {
			raw, _ := json.Marshal(map[string]string{"reasoning_content": ch.Message.ReasoningContent})
			out.Content = append(out.Content, Block{Type: ReasoningBlock, Format: "openai-chat", Raw: raw})
		}
		if ch.Message.Content != nil && *ch.Message.Content != "" {
			out.Content = append(out.Content, Text(*ch.Message.Content))
		}
		if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
			out.Content = append(out.Content, Text("[refusal] "+*ch.Message.Refusal))
		}
		for _, tc := range ch.Message.ToolCalls {
			b := Block{Type: ToolUseBlock, ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments)}
			if strings.TrimSpace(tc.Function.Arguments) == "" {
				b.Input = json.RawMessage("{}")
			} else if !json.Valid(b.Input) {
				b.Input, _ = json.Marshal(map[string]string{"__invalid_json__": tc.Function.Arguments})
			}
			out.Content = append(out.Content, b)
		}
		switch {
		case len(out.ToolUses()) > 0:
			out.StopReason = StopToolUse
		case ch.FinishReason == "length":
			out.StopReason = StopMaxTokens
		case ch.FinishReason == "content_filter" || (ch.Message.Refusal != nil && *ch.Message.Refusal != ""):
			out.StopReason = StopRefusal
		default:
			out.StopReason = StopEndTurn
		}
		return out, nil
	})
}
