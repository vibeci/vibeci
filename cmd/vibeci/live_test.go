package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/jsonc"
	"github.com/vibeci/vibeci/internal/llm"
)

// TestLiveModels runs a short tool-use conversation against every model of a
// real-model fragment (the format of e2e/llm.example.jsonc; only providers
// and models are read):
//
//	VIBECI_LIVE_LLM_CONFIG=.secrets/e2e-llm.jsonc go test ./cmd/vibeci -run Live -v
//
// It checks a provider adapter end to end (streaming or not, thinking,
// prompt caching, tool calls) in about a minute, without the e2e harness.
func TestLiveModels(t *testing.T) {
	path := os.Getenv("VIBECI_LIVE_LLM_CONFIG")
	if path == "" {
		t.Skip("VIBECI_LIVE_LLM_CONFIG not set")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join("..", "..", path) // relative to the repository root
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frag struct {
		Providers map[string]*config.Provider `json:"providers"`
		Models    map[string]*config.Model    `json:"models"`
		Roles     json.RawMessage             `json:"roles"`
	}
	if err := jsonc.Unmarshal(b, &frag); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Providers: frag.Providers, Models: frag.Models}
	cfg.ApplyDefaults()
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := cfg.Models[name]
		t.Run(name, func(t *testing.T) {
			p := cfg.Providers[m.Provider]
			if p == nil {
				t.Fatalf("unknown provider %q", m.Provider)
			}
			spec, err := providerSpec(p)
			if err != nil {
				t.Fatal(err)
			}
			// Keep the configured features but spend little.
			maxTokens, budget, effort := min(m.MaxTokens, 8000), m.ThinkingBudget, m.Effort
			if budget >= maxTokens {
				budget = maxTokens / 2
			}
			if effort != "" {
				effort = "low"
			}
			c, err := llm.New(spec, llm.ModelOptions{
				Name: name, Model: m.Model, MaxTokens: maxTokens, ContextWindow: m.ContextWindow,
				Thinking: m.Thinking, ThinkingBudget: budget, Effort: effort,
				Temperature: m.Temperature, PromptCache: m.PromptCache == nil || *m.PromptCache,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			runToolLoop(t, c)
		})
	}
}

func runToolLoop(t *testing.T, c llm.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tools := []llm.Tool{{Name: "multiply", Description: "Multiply two integers exactly.", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`)}}
	msgs := []llm.Message{{Role: llm.User, Content: []llm.Block{llm.Text("Use the multiply tool to compute 1234 * 5678, then reply with just the number.")}}}
	sys := strings.Repeat("You are a precise calculator assistant. ", 200) // long enough to be cacheable
	for turn := 0; turn < 4; turn++ {
		resp, err := c.Complete(ctx, &llm.Request{System: sys, Messages: msgs, Tools: tools})
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		t.Logf("turn %d stop=%s usage=%+v blocks=%d", turn, resp.StopReason, resp.Usage, len(resp.Content))
		msgs = append(msgs, llm.Message{Role: llm.Assistant, Content: resp.Content})
		uses := resp.ToolUses()
		if len(uses) == 0 {
			if !strings.Contains(strings.ReplaceAll(resp.TextContent(), ",", ""), "7006652") {
				t.Fatalf("unexpected answer: %q", resp.TextContent())
			}
			return
		}
		var results []llm.Block
		for _, u := range uses {
			var in struct{ A, B int }
			json.Unmarshal(u.Input, &in)
			out, _ := json.Marshal(in.A * in.B)
			results = append(results, llm.ToolResult(u.ID, string(out), false))
		}
		msgs = append(msgs, llm.Message{Role: llm.User, Content: results})
	}
	t.Fatal("no final answer")
}
