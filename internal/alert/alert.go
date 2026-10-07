// Package alert delivers events to webhooks (generic JSON, ntfy, Discord,
// Slack). Delivery is best effort: failures are logged, never fatal.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/vibeci/vibeci/internal/config"
)

// Event is something worth telling a human about.
type Event struct {
	Kind     string         `json:"kind"`     // blocked, suspicious, failed, rewritten, error, synced, recovered
	Severity string         `json:"severity"` // critical, warning, info
	Repo     string         `json:"repo"`
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Commits  []string       `json:"commits,omitempty"`
	JobID    string         `json:"job_id,omitempty"`
	Time     time.Time      `json:"time"`
	Details  map[string]any `json:"details,omitempty"`
}

// Sink is one webhook.
type Sink struct {
	Name    string
	Type    string
	URL     string
	Headers map[string]string
	Events  []string
}

// Dispatcher fans events out to sinks.
type Dispatcher struct {
	Sinks  []*Sink
	Client *http.Client
	Logger *slog.Logger
}

// New resolves alert configs (including secrets).
func New(cfgs []*config.Alert, logger *slog.Logger) (*Dispatcher, error) {
	d := &Dispatcher{Client: &http.Client{Timeout: 20 * time.Second}, Logger: logger}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	for i, c := range cfgs {
		u, err := c.URL.Resolve()
		if err != nil {
			return nil, fmt.Errorf("alerts[%d] url: %w", i, err)
		}
		s := &Sink{Name: c.Name, Type: c.Type, URL: u, Events: c.Events, Headers: map[string]string{}}
		if s.Name == "" {
			s.Name = fmt.Sprintf("%s-%d", c.Type, i)
		}
		for k, v := range c.Headers {
			val, err := v.Resolve()
			if err != nil {
				return nil, fmt.Errorf("alerts[%d] header %s: %w", i, k, err)
			}
			s.Headers[k] = val
		}
		d.Sinks = append(d.Sinks, s)
	}
	return d, nil
}

func (s *Sink) wants(kind string) bool {
	return slices.Contains(s.Events, "*") || slices.Contains(s.Events, kind)
}

// Send delivers ev to every interested sink.
func (d *Dispatcher) Send(ctx context.Context, ev Event) {
	if d == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	ev.Title = sanitize(ev.Title, 200)
	ev.Message = sanitize(ev.Message, 6000)
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 20 * time.Second}
	}
	d.Logger.Info("alert", "kind", ev.Kind, "repo", ev.Repo, "title", ev.Title)
	for _, s := range d.Sinks {
		if !s.wants(ev.Kind) {
			continue
		}
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = d.deliver(ctx, s, ev); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		}
		if err != nil {
			d.Logger.Warn("alert delivery failed", "sink", s.Name, "err", err)
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, s *Sink, ev Event) error {
	body, contentType, headers := render(s.Type, ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "vibeci/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func render(typ string, ev Event) (body []byte, contentType string, headers map[string]string) {
	headers = map[string]string{}
	text := ev.Message
	if len(ev.Commits) > 0 {
		text += "\n\nCommits: " + strings.Join(shortList(ev.Commits, 10), ", ")
	}
	if ev.JobID != "" {
		text += "\nJob: " + ev.JobID
	}
	switch typ {
	case "ntfy":
		headers["Title"] = asciiOnly(fmt.Sprintf("[%s] %s", ev.Repo, ev.Title))
		headers["Priority"] = map[string]string{"critical": "5", "warning": "4"}[ev.Severity]
		if headers["Priority"] == "" {
			headers["Priority"] = "3"
		}
		headers["Tags"] = map[string]string{"critical": "rotating_light", "warning": "warning"}[ev.Severity]
		if headers["Tags"] == "" {
			headers["Tags"] = "white_check_mark"
		}
		return []byte(text), "text/plain; charset=utf-8", headers
	case "discord":
		content := fmt.Sprintf("**[%s] %s**\n%s", ev.Repo, ev.Title, text)
		if len(content) > 1990 {
			content = content[:1990] + "…"
		}
		b := marshal(map[string]any{
			"username":         "VibeCI",
			"content":          content,
			"allowed_mentions": map[string]any{"parse": []string{}}, // never ping from attacker-influenced text
		})
		return b, "application/json", headers
	case "slack":
		esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
		content := fmt.Sprintf("*[%s] %s*\n%s", esc.Replace(ev.Repo), esc.Replace(ev.Title), esc.Replace(text))
		if len(content) > 3500 {
			content = content[:3500] + "…"
		}
		b := marshal(map[string]any{"text": content})
		return b, "application/json", headers
	default:
		return marshal(ev), "application/json", headers
	}
}

func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSpace(buf.Bytes())
}

func shortList(shas []string, n int) []string {
	var out []string
	for i, s := range shas {
		if i == n {
			out = append(out, fmt.Sprintf("(+%d more)", len(shas)-n))
			break
		}
		if len(s) > 12 {
			s = s[:12]
		}
		out = append(out, s)
	}
	return out
}

// sanitize strips control and bidi characters (alert text can quote
// attacker-controlled content) and truncates.
func sanitize(s string, n int) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), r == '\u200b', r == '\u2060', r == '\ufeff':
			// drop
		default:
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if len(out) > n {
		out = out[:n] + "…"
	}
	return out
}

func asciiOnly(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
