// Package fakellm is a deterministic stand-in for the Anthropic Messages
// API used by VibeCI's end-to-end tests. It recognizes VibeCI's agent roles
// by the tools a request offers and answers from a Script: commits that
// contain a malicious marker are reported as malicious, and merges are
// resolved by writing the scenario's prepared files. It also collects
// alert webhooks (POST /alerts), so one process serves both.
package fakellm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Script decides the fake model's answers.
type Script struct {
	// MaliciousMarkers: a reviewed commit whose diff contains any of these
	// strings is reported as malicious.
	MaliciousMarkers []string `json:"malicious_markers"`
	// Resolution maps paths to the content the merge agent writes before
	// submitting, whenever VibeCI asks it to resolve a merge.
	Resolution map[string]string `json:"resolution"`
	// Patches maps patch names (as the series lists them) to how the patch
	// agent updates them when VibeCI asks (patch-mode forks).
	Patches map[string]PatchUpdate `json:"patches"`
}

// PatchUpdate is what the fake patch agent does with one patch.
type PatchUpdate struct {
	// Files maps upstream tree paths to the content written before
	// submitting.
	Files map[string]string `json:"files"`
	// Drop submits with drop_patch instead (upstream does what the patch
	// did).
	Drop bool `json:"drop"`
}

// LoadScript reads a JSON script file.
func LoadScript(path string) (Script, error) {
	var s Script
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Call is one request the server answered.
type Call struct {
	Role string    `json:"role"` // triage, investigate, audit, resolve, patch, ping
	Time time.Time `json:"time"`
}

// Server implements http.Handler.
type Server struct {
	Script Script
	// AlertsFile, if set, receives every alert as one JSON line.
	AlertsFile string

	mu     sync.Mutex
	calls  []Call
	alerts []json.RawMessage
}

// Calls returns the requests answered so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Count returns how many requests of a role were answered.
func (s *Server) Count(role string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Role == role {
			n++
		}
	}
	return n
}

// Alerts returns the alert payloads received so far.
func (s *Server) Alerts() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]json.RawMessage(nil), s.alerts...)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		io.WriteString(w, "ok\n")
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		s.messages(w, r)
	case r.URL.Path == "/alerts" && r.Method == http.MethodPost:
		s.alert(w, r)
	case r.URL.Path == "/alerts" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Alerts())
	case r.URL.Path == "/calls":
		writeJSON(w, http.StatusOK, s.Calls())
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) alert(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		http.Error(w, "bad alert", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.alerts = append(s.alerts, json.RawMessage(body))
	if s.AlertsFile != "" {
		if f, err := os.OpenFile(s.AlertsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(compact(body), '\n'))
			f.Close()
		}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func compact(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, _ := json.Marshal(v)
	return out
}

// Wire format (the subset VibeCI sends).
type request struct {
	Model    string    `json:"model"`
	System   []block   `json:"system"`
	Messages []message `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// resultText returns a tool_result's text (string or block-list content).
func (b block) resultText() string {
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var parts []block
	json.Unmarshal(b.Content, &parts)
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") == "" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"type": "error", "error": map[string]string{"type": "authentication_error", "message": "missing API key"}})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": err.Error()}})
		return
	}
	role, content := s.answer(&req)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Role: role, Time: time.Now().UTC()})
	s.mu.Unlock()

	stop := "end_turn"
	for _, b := range content {
		if b.Type == "tool_use" {
			stop = "tool_use"
		}
	}
	usage := map[string]int{"input_tokens": len(body) / 4, "output_tokens": 20 + len(fmt.Sprint(content))/4}
	if req.Stream {
		stream(w, req.Model, content, stop, usage)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "msg_" + randHex(), "type": "message", "role": "assistant", "model": req.Model,
		"content": content, "stop_reason": stop, "usage": usage,
	})
}

func randHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (req *request) has(tool string) bool {
	for _, t := range req.Tools {
		if t.Name == tool {
			return true
		}
	}
	return false
}

func (req *request) firstUserText() string {
	for _, m := range req.Messages {
		if m.Role == "user" {
			var sb strings.Builder
			for _, b := range m.Content {
				sb.WriteString(b.Text)
			}
			return sb.String()
		}
	}
	return ""
}

func toolUse(name string, input any) block {
	raw, _ := json.Marshal(input)
	return block{Type: "tool_use", ID: "toolu_" + randHex(), Name: name, Input: raw}
}

func text(t string) block { return block{Type: "text", Text: t} }

var commitRe = regexp.MustCompile(`(?s)=== COMMIT \d+/\d+ ([0-9a-f]{7,64}) ===\n(.*?)\n=== END COMMIT ([0-9a-f]{7,64}) ===`)

func (s *Server) malicious(content string) (bool, string) {
	for _, m := range s.Script.MaliciousMarkers {
		if m != "" && strings.Contains(content, m) {
			return true, m
		}
	}
	return false, ""
}

// answer picks the reply for a request.
func (s *Server) answer(req *request) (string, []block) {
	first := req.firstUserText()
	switch {
	case req.has("submit_review") && strings.Contains(first, "Lines written by the merge agent"):
		return "audit", []block{toolUse("submit_review", map[string]any{"reviews": []map[string]any{{
			"commit": "resolution", "verdict": "clean", "confidence": 0.9, "summary": "ordinary merge resolution",
		}}})}
	case req.has("submit_review"):
		var reviews []map[string]any
		for _, m := range commitRe.FindAllStringSubmatch(first, -1) {
			sha, body := m[1], m[2]
			v := map[string]any{"commit": sha, "verdict": "clean", "confidence": 0.95, "summary": "ordinary change"}
			if bad, marker := s.malicious(body); bad {
				v = map[string]any{"commit": sha, "verdict": "malicious", "confidence": 0.9, "summary": "decodes and executes a hidden command",
					"findings": []map[string]string{{"file": "?", "category": "remote_code_execution", "evidence": marker, "explanation": "hidden command execution"}}}
			}
			reviews = append(reviews, v)
		}
		return "triage", []block{toolUse("submit_review", map[string]any{"reviews": reviews})}
	case req.has("submit_verdict"):
		if bad, marker := s.malicious(first); bad {
			return "investigate", []block{toolUse("submit_verdict", map[string]any{
				"verdict": "malicious", "confidence": 0.97, "summary": "runs a base64-hidden shell command downloaded from the network at package init",
				"findings": []map[string]string{{"file": "?", "category": "remote_code_execution", "evidence": marker, "explanation": "executes a hidden remote script"}},
			})}
		}
		return "investigate", []block{toolUse("submit_verdict", map[string]any{"verdict": "clean", "confidence": 0.9, "summary": "benign on closer inspection"})}
	case req.has("submit") && req.has("open_file"):
		return "patch", s.patch(req)
	case req.has("submit") && req.has("write_file"):
		return "resolve", s.resolve(req)
	case len(req.Tools) == 0:
		return "ping", []block{text("OK")}
	}
	return "unknown", []block{text("I do not know this task.")}
}

// resolve drives the merge agent: write the prepared files, submit, and
// confirm intentional drops if the harness asks.
func (s *Server) resolve(req *request) []block {
	var lastAssistant *message
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			lastAssistant = &req.Messages[i]
			break
		}
	}
	paths := make([]string, 0, len(s.Script.Resolution))
	for p := range s.Script.Resolution {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	submit := func(confirm bool) block {
		in := map[string]any{"summary": "Applied the prepared resolution: kept the fork's features on top of upstream's changes.", "created_files": paths}
		if confirm {
			in["confirm_dropped_fork_changes"] = true
		}
		return toolUse("submit", in)
	}
	if lastAssistant == nil {
		if len(paths) == 0 {
			return []block{toolUse("give_up", map[string]any{"reason": "the test script has no resolution for this merge"})}
		}
		var out []block
		for _, p := range paths {
			out = append(out, toolUse("write_file", map[string]any{"path": p, "content": s.Script.Resolution[p]}))
		}
		return out
	}
	submitted := false
	for _, b := range lastAssistant.Content {
		if b.Type == "tool_use" && b.Name == "submit" {
			submitted = true
		}
	}
	if !submitted {
		return []block{submit(false)}
	}
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Content {
		if b.Type == "tool_result" && b.IsError {
			t := b.resultText()
			if strings.Contains(t, "confirm_dropped_fork_changes") {
				return []block{submit(true)}
			}
			return []block{toolUse("give_up", map[string]any{"reason": "the prepared resolution was rejected: " + t})}
		}
	}
	return []block{text("done")}
}

// lastAssistant returns the model's previous turn (nil on the first).
func (req *request) lastAssistant() *message {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			return &req.Messages[i]
		}
	}
	return nil
}

// lastError returns the text of a failed tool result in the last message.
func (req *request) lastError() (string, bool) {
	if len(req.Messages) == 0 {
		return "", false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == "tool_result" && b.IsError {
			return b.resultText(), true
		}
	}
	return "", false
}

var patchTaskRe = regexp.MustCompile(`update patch \d+ of \d+, (\S+) \(file `)

// patch drives the patch agent: write the scripted files (or drop the
// patch), submit, and confirm intentional omissions if the harness asks.
func (s *Server) patch(req *request) []block {
	m := patchTaskRe.FindStringSubmatch(req.firstUserText())
	if m == nil {
		return []block{toolUse("give_up", map[string]any{"reason": "the brief names no patch"})}
	}
	u, ok := s.Script.Patches[m[1]]
	if !ok {
		return []block{toolUse("give_up", map[string]any{"reason": "the test script has no update for patch " + m[1]})}
	}
	paths := make([]string, 0, len(u.Files))
	for p := range u.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	submit := func(confirm bool) block {
		in := map[string]any{"summary": "Carried the patch's change over to the new upstream code.", "files": paths}
		if u.Drop {
			in = map[string]any{"summary": "Upstream now does what the patch did.", "drop_patch": true}
		}
		if confirm {
			in["confirm_dropped_changes"] = true
		}
		return toolUse("submit", in)
	}
	last := req.lastAssistant()
	if last == nil {
		if u.Drop || len(paths) == 0 {
			return []block{submit(false)}
		}
		var out []block
		for _, p := range paths {
			out = append(out, toolUse("write_file", map[string]any{"path": p, "content": u.Files[p]}))
		}
		return out
	}
	submitted := false
	for _, b := range last.Content {
		if b.Type == "tool_use" && b.Name == "submit" {
			submitted = true
		}
	}
	if !submitted {
		return []block{submit(false)}
	}
	if t, failed := req.lastError(); failed {
		if strings.Contains(t, "confirm_dropped_changes") {
			return []block{submit(true)}
		}
		return []block{toolUse("give_up", map[string]any{"reason": "the scripted update was rejected: " + t})}
	}
	return []block{text("done")}
}

// stream writes content as Anthropic server-sent events.
func stream(w http.ResponseWriter, model string, content []block, stop string, usage map[string]int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + randHex(), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"usage": map[string]int{"input_tokens": usage["input_tokens"], "output_tokens": 1}}})
	for i, b := range content {
		switch b.Type {
		case "text":
			send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b.Text}})
		case "tool_use":
			send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": map[string]any{}}})
			// Split the JSON like real streams do (on rune boundaries).
			in := []rune(string(b.Input))
			for len(in) > 0 {
				n := min(len(in), 64)
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(in[:n])}})
				in = in[n:]
			}
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}, "usage": map[string]int{"output_tokens": usage["output_tokens"]}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
