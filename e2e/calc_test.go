package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/e2e/scenario"
)

// calcRepo is the repo entry for the calc scenario with file remotes.
func calcRepo(c *scenario.Calc) map[string]any {
	return map[string]any{
		"name":        "calc",
		"fork":        map[string]any{"url": c.ForkBare, "branch": "main"},
		"upstream":    map[string]any{"url": c.Upstream.Dir, "branch": "main"},
		"description": scenario.CalcDescription,
		"keep_ours":   []string{".github/workflows/**"},
		"sandbox":     map[string]any{"profile": "go"},
		"verify":      c.Verify(),
	}
}

// checkCalcResult asserts that the fork's main has every upstream change
// except the malicious commit, keeps the fork's features, and builds.
func checkCalcResult(t *testing.T, c *scenario.Calc, dir string) {
	t.Helper()
	if c.Has(t, "metrics.go") {
		t.Fatal("the malicious metrics.go was merged")
	}
	calc := c.Show(t, "calc.go")
	for _, want := range []string{"func Pow(", "func Sqrt(", "func Quo(", "func Mod("} {
		if !strings.Contains(calc, want) {
			t.Errorf("calc.go lacks %q:\n%s", want, calc)
		}
	}
	if !c.Has(t, "stats.go") || !strings.Contains(c.Show(t, "README.md"), "Operators") {
		t.Error("fork's stats.go or upstream's README change is missing")
	}
	if strings.Contains(c.Show(t, "stats.go"), "Div(") {
		t.Error("stats.go still calls the removed Div")
	}
	work := filepath.Join(dir, "check-"+randSuffix())
	c.Checkout(t, work)
	goRun(t, work, "vet", "./...")
	goRun(t, work, "test", "./...")
	if out := goRun(t, work, "run", "./cmd/calc", "--json", "2", "^", "10"); !strings.Contains(out, `"result":1024`) {
		t.Errorf("calc --json 2 ^ 10 = %q", out)
	}
	if out := goRun(t, work, "run", "./cmd/calc", "7", "%", "3"); strings.TrimSpace(out) != "1" {
		t.Errorf("calc 7 %% 3 = %q", out)
	}
	removeAll(work)
}

// TestCLIContract pins the agent-facing CLI behaviour: exit codes, JSON
// outputs and secret redaction.
func TestCLIContract(t *testing.T) {
	h := newHarness(t, "cli")
	c := scenario.NewCalc(t, h.dir)
	h.fake.Script = c.Script()
	h.writeConfig(calcRepo(c))

	if r := h.vibeci(); r.code != 2 || !strings.Contains(r.stderr, "usage: vibeci") {
		t.Errorf("no command: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := h.vibeci("frobnicate"); r.code != 2 {
		t.Errorf("unknown command: exit %d", r.code)
	}
	if r := h.must(0, "version"); !strings.HasPrefix(r.stdout, "vibeci ") {
		t.Errorf("version: %q", r.stdout)
	}
	if r := h.must(0, "help"); !strings.Contains(r.stdout, "exclude") || !strings.Contains(r.stdout, "patches") || !strings.Contains(r.stdout, "Exit codes") {
		t.Errorf("help: %q", r.stdout)
	}
	if r := h.vibeci("patches"); r.code != 1 || !strings.Contains(r.stderr, "not a patch-mode fork") {
		t.Errorf("patches on a merge-mode fork: exit %d, %q", r.code, r.stderr)
	}

	r := h.must(0, "config")
	var cfg map[string]any
	r.json(t, &cfg)
	if strings.Contains(r.stdout, "e2e-fake-key-do-not-print") && !liveLLM() {
		t.Error("config output leaks an inline secret")
	}
	if !liveLLM() && !strings.Contains(r.stdout, `"[redacted]"`) {
		t.Errorf("inline secret not shown as redacted:\n%s", r.stdout)
	}
	repos := cfg["repos"].([]any)
	review := repos[0].(map[string]any)["review"].(map[string]any)
	if review["on_blocked"] != "exclude" || review["block_on"] != "malicious" {
		t.Errorf("defaults not applied: %v", review)
	}
	if r := h.must(0, "help"); !strings.Contains(r.stdout, "action") || !strings.Contains(r.stdout, "-config may be") {
		t.Errorf("help: %q", r.stdout)
	}

	// -config merges files in order, later ones overriding earlier ones.
	overlay := filepath.Join(h.dir, "overlay.jsonc")
	writeFile(t, overlay, "// local settings\n{\"log_level\": \"debug\", \"state_ref\": \"refs/vibeci/state\"}\n", 0o644)
	var merged map[string]any
	h.must(0, "config", "-config", h.cfg, "-config", overlay).json(t, &merged)
	if merged["log_level"] != "debug" || merged["state_ref"] != "refs/vibeci/state" || len(merged["repos"].([]any)) != 1 {
		t.Errorf("merged config: %v", merged)
	}
	writeFile(t, overlay, "{\"log_levl\": \"debug\"}\n", 0o644)
	if r := h.vibeci("config", "-config", h.cfg, "-config", overlay); r.code != 1 || !strings.Contains(r.stderr, overlay) || !strings.Contains(r.stderr, "log_levl") {
		t.Errorf("unknown key in an overlay: exit %d, %q", r.code, r.stderr)
	}

	var st struct {
		Repos     map[string]map[string]any `json:"repos"`
		Heartbeat any                       `json:"heartbeat"`
	}
	h.must(0, "status", "-json").json(t, &st)
	if _, ok := st.Repos["calc"]; !ok || st.Heartbeat != nil {
		t.Errorf("status before any sync: %+v", st)
	}
	if r := h.vibeci("health"); r.code != 1 {
		t.Errorf("health without a heartbeat: exit %d", r.code)
	}
	if r := h.vibeci("allow"); r.code != 2 {
		t.Errorf("allow without commits: exit %d", r.code)
	}
	if r := h.vibeci("exclude", "deadbeef"); r.code != 1 || !strings.Contains(r.stderr, "not been synced") {
		t.Errorf("exclude before the first sync: exit %d, %q", r.code, r.stderr)
	}
	if r := h.vibeci("allow", "-repo", "nope", "deadbeefdeadbeef"); r.code != 1 || !strings.Contains(r.stderr, "no repo named") {
		t.Errorf("allow unknown repo: exit %d, %q", r.code, r.stderr)
	}
	if r := h.vibeci("allow", "zzz"); r.code != 1 || !strings.Contains(r.stderr, "not a commit id") {
		t.Errorf("allow invalid id: exit %d, %q", r.code, r.stderr)
	}

	var chk struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name  string `json:"name"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"checks"`
	}
	h.must(0, "check", "-json").json(t, &chk)
	if !chk.OK || len(chk.Checks) < 6 {
		t.Errorf("check: %+v", chk)
	}

	// review reports without merging; in exclude mode a blocked commit is
	// excluded rather than blocking, so the exit code is 0.
	var rep struct {
		Total    int      `json:"total"`
		Target   string   `json:"safe_target"`
		Blocked  string   `json:"first_blocked"`
		Excluded []string `json:"excluded"`
		Verdicts []struct {
			Commit  string `json:"commit"`
			Verdict string `json:"verdict"`
		} `json:"verdicts"`
	}
	h.must(0, "review", "-json").json(t, &rep)
	if rep.Total != 4 || rep.Blocked != "" || len(rep.Excluded) != 1 || rep.Excluded[0] != c.Malicious || rep.Target != c.AfterMalicious {
		t.Errorf("review: %+v", rep)
	}

	// Hold mode: the same review needs attention (exit 3).
	h.writeConfig(calcRepo(c), func(cfg map[string]any) {
		cfg["repos"].([]any)[0].(map[string]any)["review"] = map[string]any{"on_blocked": "hold"}
	})
	h.must(3, "review", "-json").json(t, &rep)
	if rep.Blocked != c.Malicious {
		t.Errorf("hold review: %+v", rep)
	}
}

// TestCalc is the main scenario: sync, exclusion, idempotence, and the
// operator's allow/exclude round trip.
func TestCalc(t *testing.T) {
	h := newHarness(t, "calc")
	c := scenario.NewCalc(t, h.dir)
	h.fake.Script = c.Script()
	h.writeConfig(calcRepo(c))
	forkBefore := c.ForkHead(t)

	out := h.run(0)
	if out.Status != "synced" || len(out.Excluded) != 1 || out.Excluded[0] != c.Malicious || out.Commit != c.ForkHead(t) || out.Pending != 0 {
		t.Fatalf("first sync: %+v", out)
	}
	if c.ForkHead(t) == forkBefore {
		t.Fatal("fork did not move")
	}
	checkCalcResult(t, c, h.dir)
	as := h.alerts()
	if countAlerts(as, "blocked", "excluded") != 1 || countAlerts(as, "synced", "") != 1 {
		t.Errorf("alerts: %+v", as)
	}
	for _, a := range as {
		if a.Kind == "blocked" && (len(a.Commits) != 1 || a.Commits[0] != c.Malicious || !strings.Contains(a.Message, "vibeci allow -repo calc")) {
			t.Errorf("blocked alert: %+v", a)
		}
	}
	if !liveLLM() {
		if h.fake.Count("triage") == 0 || h.fake.Count("investigate") != 1 || h.fake.Count("resolve") < 2 {
			t.Errorf("model calls: %+v", h.fake.Calls())
		}
	}

	// Idempotent: nothing new upstream means no model calls.
	calls := len(h.fake.Calls())
	if out := h.run(0); out.Status != "up-to-date" || out.Usage.Calls != 0 {
		t.Fatalf("second sync: %+v", out)
	}
	if !liveLLM() && len(h.fake.Calls()) != calls {
		t.Error("an up-to-date sync called the model")
	}
	h.must(0, "health")

	var st struct {
		Repos map[string]struct {
			LastResult string `json:"last_result"`
			Excluded   map[string]struct {
				Source  string `json:"source"`
				Removed bool   `json:"removed"`
				Verdict string `json:"verdict"`
			} `json:"excluded"`
		} `json:"repos"`
	}
	h.must(0, "status", "-json").json(t, &st)
	if x, ok := st.Repos["calc"].Excluded[c.Malicious]; !ok || !x.Removed || x.Source != "review" || x.Verdict != "malicious" {
		t.Errorf("status: %+v", st.Repos["calc"])
	}
	if r := h.must(0, "status"); !strings.Contains(r.stdout, "excluded upstream commits") {
		t.Errorf("status text: %s", r.stdout)
	}

	// The operator decides it is a false positive: allow re-applies it.
	var ch struct {
		Changes []struct {
			Commit string `json:"commit"`
			Effect string `json:"effect"`
		} `json:"changes"`
	}
	h.must(0, "allow", "-json", "-reason", "e2e: restore it", c.Malicious[:12]).json(t, &ch)
	if len(ch.Changes) != 1 || ch.Changes[0].Effect != "restore-scheduled" || ch.Changes[0].Commit != c.Malicious {
		t.Fatalf("allow: %+v", ch)
	}
	out = h.run(0)
	if out.Status != "synced" || len(out.Restored) != 1 || !c.Has(t, "metrics.go") {
		t.Fatalf("restore: %+v", out)
	}

	// ...and changes its mind: exclude removes it again.
	h.must(0, "exclude", "-json", "-reason", "e2e: remove it again", c.Malicious).json(t, &ch)
	if ch.Changes[0].Effect != "excluded" {
		t.Fatalf("exclude: %+v", ch)
	}
	out = h.run(0)
	if out.Status != "synced" || c.Has(t, "metrics.go") || !strings.Contains(out.Message, "removal of upstream commit") {
		t.Fatalf("removal: %+v", out)
	}
	checkCalcResult(t, c, h.dir)
	if out := h.run(0); out.Status != "up-to-date" {
		t.Fatalf("final sync: %+v", out)
	}
}
