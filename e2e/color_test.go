package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/e2e/scenario"
)

// TestColor syncs a real-world fork (github.com/fatih/color at v1.13.0 with
// fork patches) to upstream's latest release using real models. Upstream
// renamed what the fork's code calls, so the result only builds if the
// merge adapts the fork's own new file too. Needs network access (upstream
// unless VIBECI_E2E_COLOR_SRC names a local clone; Go modules).
func TestColor(t *testing.T) {
	requireE2E(t)
	if os.Getenv("VIBECI_E2E_OSS") != "1" {
		t.Skip("set VIBECI_E2E_OSS=1 to run the fatih/color test (network, real models)")
	}
	if !liveLLM() {
		t.Skip("the fatih/color test needs real models: set VIBECI_E2E_LLM_CONFIG")
	}
	h := newHarness(t, "color")
	c := scenario.NewColor(t, filepath.Join(h.dir, "scenario"), os.Getenv("VIBECI_E2E_COLOR_SRC"))
	h.writeConfig(map[string]any{
		"name":        "color",
		"fork":        map[string]any{"url": c.ForkBare, "branch": "main"},
		"upstream":    map[string]any{"url": c.Source, "tags": "v*"},
		"description": scenario.ColorDescription,
		"keep_ours":   []string{".github/workflows/**"},
		"sandbox":     map[string]any{"profile": "go", "prefetch_profile": "go-egress"},
		"prefetch":    []map[string]string{{"name": "modules", "run": "go mod download", "timeout": "10m"}},
		"verify": []map[string]string{
			{"name": "build", "run": "go build ./... && go vet ./...", "timeout": "10m"},
			{"name": "test", "run": "go test ./...", "timeout": "10m"},
		},
	})

	out := h.run(0)
	if out.Status != "synced" || out.Pending != 0 || len(out.Excluded) != 0 || len(out.Blocked) != 0 {
		t.Fatalf("sync: %+v", out)
	}
	if !c.Contains(t, c.Target) {
		t.Fatalf("the fork's main does not contain upstream %s (%s)", c.Latest, c.Target)
	}
	if got := c.Show(t, ".github/workflows/go.yml"); got != scenario.ColorWorkflow {
		t.Errorf("the fork's CI workflow was not kept (keep_ours):\n%s", got)
	}
	if !strings.Contains(c.Show(t, "README.md"), "Fork note") {
		t.Error("the fork's README note is missing")
	}
	if !strings.Contains(c.Show(t, "color.go"), "forceColor()") || !c.Has(t, "force.go") || !c.Has(t, "force_test.go") {
		t.Error("the fork's FORCE_COLOR support is missing")
	}
	as := h.alerts()
	if countAlerts(as, "blocked", "") != 0 || countAlerts(as, "synced", "") != 1 {
		t.Errorf("alerts: %+v", as)
	}

	// The result builds, passes its tests, and keeps the fork's behaviour:
	// with stdout not a terminal, FORCE_COLOR turns colors on and NO_COLOR
	// still wins.
	work := filepath.Join(h.dir, "check")
	c.Checkout(t, work)
	goRun(t, work, "vet", "./...")
	goRun(t, work, "test", "./...")
	if err := os.WriteFile(filepath.Join(work, "zz_e2e_init_test.go"), []byte(scenario.ColorInitTest), 0o644); err != nil {
		t.Fatal(err)
	}
	colorInit(t, work, false, "FORCE_COLOR=1")
	colorInit(t, work, false, "CLICOLOR_FORCE=1")
	colorInit(t, work, true, "FORCE_COLOR=1", "NO_COLOR=1")
	colorInit(t, work, true) // upstream behaviour without the fork's variables

	if out := h.run(0); out.Status != "up-to-date" {
		t.Fatalf("second sync: %+v", out)
	}
}

// colorInit runs ColorInitTest in dir with env on top of a clean color
// environment.
func colorInit(t *testing.T, dir string, wantNoColor bool, env ...string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestE2EColorInit$", ".")
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "NO_COLOR" && k != "FORCE_COLOR" && k != "CLICOLOR_FORCE" && k != "TERM" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	want := "0"
	if wantNoColor {
		want = "1"
	}
	cmd.Env = append(cmd.Env, append([]string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=mod", "E2E_WANT_NOCOLOR=" + want}, env...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("NoColor with %v: %v\n%s", env, err, out)
	}
}
