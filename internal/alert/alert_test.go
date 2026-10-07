package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRenderAndDeliver(t *testing.T) {
	var mu sync.Mutex
	got := map[string]*http.Request{}
	bodies := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = r
		bodies[r.URL.Path] = string(b)
		mu.Unlock()
	}))
	defer srv.Close()
	d := &Dispatcher{Client: srv.Client(), Sinks: []*Sink{
		{Name: "g", Type: "generic", URL: srv.URL + "/generic", Events: []string{"*"}},
		{Name: "n", Type: "ntfy", URL: srv.URL + "/ntfy", Events: []string{"blocked"}, Headers: map[string]string{"Authorization": "Bearer tk"}},
		{Name: "d", Type: "discord", URL: srv.URL + "/discord", Events: []string{"blocked"}},
		{Name: "s", Type: "slack", URL: srv.URL + "/slack", Events: []string{"failed"}},
	}}
	d.Send(context.Background(), Event{Kind: "blocked", Severity: "critical", Repo: "demo", Title: "Blocked \u202emalicious\u202c commit", Message: "@everyone exfil <!channel>", Commits: []string{"0123456789abcdef0123"}})

	if len(got) != 3 {
		t.Fatalf("expected generic, ntfy, discord deliveries; got %v", keys(got))
	}
	var ev Event
	json.Unmarshal([]byte(bodies["/generic"]), &ev)
	if strings.ContainsRune(ev.Title, '\u202e') {
		t.Error("bidi control not stripped")
	}
	if r := got["/ntfy"]; r.Header.Get("Priority") != "5" || r.Header.Get("Authorization") != "Bearer tk" || !strings.Contains(r.Header.Get("Title"), "[demo]") {
		t.Errorf("ntfy headers: %v", r.Header)
	}
	if !strings.Contains(bodies["/discord"], `"parse":[]`) {
		t.Errorf("discord must disable mentions: %s", bodies["/discord"])
	}
}

func keys(m map[string]*http.Request) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSlackEscaping(t *testing.T) {
	body, _, _ := render("slack", Event{Repo: "r", Title: "t", Message: "<!channel> & <@U123>"})
	var v struct{ Text string }
	json.Unmarshal(body, &v)
	if strings.Contains(v.Text, "<!channel>") || !strings.Contains(v.Text, "&lt;!channel&gt; &amp; &lt;@U123&gt;") {
		t.Errorf("slack escaping: %s", body)
	}
}
