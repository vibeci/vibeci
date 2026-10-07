package llm

import (
	"fmt"
	"log/slog"
)

// New constructs a client for the given provider type.
func New(spec ProviderSpec, opts ModelOptions, logger *slog.Logger) (Client, error) {
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 16000
	}
	if opts.ContextWindow <= 0 {
		opts.ContextWindow = 200000
	}
	switch spec.Type {
	case "anthropic":
		return NewAnthropic(spec, opts, logger), nil
	case "openai-responses":
		return NewResponses(spec, opts, logger), nil
	case "openai-chat":
		return NewChat(spec, opts, logger), nil
	}
	return nil, fmt.Errorf("unknown provider type %q", spec.Type)
}
