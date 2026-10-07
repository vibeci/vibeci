// Package llm is a provider-neutral chat/tool-use client with adapters for
// the Anthropic Messages API and the OpenAI Responses and Chat Completions
// APIs, as served by the vendors themselves and by compatible gateways and
// servers. It uses only the standard library.
package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// Role of a message author.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// BlockType identifies a content block.
type BlockType string

const (
	TextBlock       BlockType = "text"
	ToolUseBlock    BlockType = "tool_use"
	ToolResultBlock BlockType = "tool_result"
	// ReasoningBlock is opaque provider reasoning state (Anthropic thinking
	// blocks with signatures, OpenAI encrypted reasoning items). It must be
	// replayed verbatim to the same provider format and is dropped for
	// others.
	ReasoningBlock BlockType = "reasoning"
)

// Block is one piece of message content.
type Block struct {
	Type BlockType `json:"type"`
	Text string    `json:"text,omitempty"`
	// ID is the tool call id for tool_use, and the id being answered for
	// tool_result.
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	// Format and Raw carry reasoning blocks.
	Format string          `json:"format,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
}

// Text returns a text block.
func Text(s string) Block { return Block{Type: TextBlock, Text: s} }

// ToolResult returns a tool_result block answering tool call id.
func ToolResult(id, output string, isError bool) Block {
	return Block{Type: ToolResultBlock, ID: id, Text: output, IsError: isError}
}

// Message is a conversation turn.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
}

// Tool is a function the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Tool choice values for Request.ToolChoice. A specific tool is requested
// with "tool:<name>". Providers that cannot force a tool (e.g. Anthropic with
// extended thinking) fall back to "auto".
const (
	ToolChoiceAuto = "auto"
	ToolChoiceAny  = "any"
	ToolChoiceNone = "none"
)

// Request is a single model call.
type Request struct {
	System     string
	Messages   []Message
	Tools      []Tool
	ToolChoice string
	// MaxTokens overrides the model's configured output cap when > 0.
	MaxTokens int
}

// StopReason is why generation stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage counts tokens.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	Calls            int `json:"calls,omitempty"`
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.Calls += o.Calls
}

// Response is a model reply.
type Response struct {
	Content    []Block
	StopReason StopReason
	RawStop    string
	Usage      Usage
	Model      string
}

// ToolUses returns the tool_use blocks in order.
func (r *Response) ToolUses() []Block {
	var out []Block
	for _, b := range r.Content {
		if b.Type == ToolUseBlock {
			out = append(out, b)
		}
	}
	return out
}

// TextContent concatenates the text blocks.
func (r *Response) TextContent() string {
	var sb strings.Builder
	for _, b := range r.Content {
		if b.Type == TextBlock {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// Client calls one configured model.
type Client interface {
	Complete(ctx context.Context, req *Request) (*Response, error)
	// Name is the configured model name (for logs and provenance).
	Name() string
	// ContextWindow is the model's context size in tokens.
	ContextWindow() int
}

// ModelOptions are per-model generation settings.
type ModelOptions struct {
	Name           string
	Model          string
	MaxTokens      int
	ContextWindow  int
	Thinking       string // "", "off", "adaptive", "enabled"
	ThinkingBudget int
	Effort         string
	Temperature    *float64
	PromptCache    bool
}

func (o ModelOptions) thinkingOn() bool {
	return o.Thinking == "adaptive" || o.Thinking == "enabled"
}

// ProviderSpec is a provider with secrets already resolved.
type ProviderSpec struct {
	Type    string
	BaseURL string
	Auth    string
	APIKey  string

	Headers        map[string]string
	TimeoutSeconds int
	MaxRetries     int
	Stream         bool
	MaxTokensField string
}

// EstimateTokens is a cheap, provider-independent token estimate (~4 bytes
// per token for code and English).
func EstimateTokens(s string) int { return len(s)/4 + 1 }

// EstimateMessages estimates the tokens used by a conversation.
func EstimateMessages(system string, msgs []Message) int {
	n := EstimateTokens(system)
	for _, m := range msgs {
		for _, b := range m.Content {
			n += EstimateTokens(b.Text) + len(b.Input)/4 + len(b.Raw)/8 + 4
		}
	}
	return n
}

// rawObject returns raw if it is a JSON object, else "{}".
func rawObject(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	return json.RawMessage("{}")
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
