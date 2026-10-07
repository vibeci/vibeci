package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/e2e/scenario"
)

func greetRepo(g *scenario.Greet) map[string]any {
	return map[string]any{
		"name":        "greet",
		"fork":        map[string]any{"url": g.ForkBare, "branch": "main"},
		"upstream":    map[string]any{"url": "file://" + g.Upstream.Dir, "tags": "v*", "tag_format": "v{version}"},
		"description": scenario.GreetDescription,
		"patches": map[string]any{
			"series":       "patches/series",
			"version_file": "upstream-version.txt",
			"update_files": map[string]string{"revision.txt": "1\n"},
			"verify_tree":  "full",
		},
		"sandbox": map[string]any{"profile": "go"},
		"verify":  g.Verify(),
	}
}

// TestGreetPatchFork moves a patch-mode fork to a new upstream version:
// one patch only moved, one conflicts and is rewritten by the patch agent,
// one is part of upstream now and is dropped.
func TestGreetPatchFork(t *testing.T) {
	h := newHarness(t, "greet")
	g := scenario.NewGreet(t, h.dir)
	h.fake.Script = g.Script()
	h.writeConfig(greetRepo(g))
	forkBefore := g.ForkHead(t)

	var rep struct {
		Pinned   string         `json:"pinned"`
		Version  string         `json:"version"`
		Tag      string         `json:"tag"`
		Upstream string         `json:"upstream"`
		Counts   map[string]int `json:"counts"`
		Patches  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Failed []struct {
				File string `json:"file"`
				Hunk int    `json:"hunk"`
			} `json:"failed"`
		} `json:"patches"`
	}
	h.must(0, "patches", "-json").json(t, &rep)
	var statuses []string
	for _, p := range rep.Patches {
		statuses = append(statuses, p.Name+"="+p.Status)
	}
	if rep.Pinned != "1.0.0" || rep.Version != "1.1.0" || rep.Tag != "v1.1.0" || rep.Upstream != g.V2 ||
		strings.Join(statuses, " ") != "0001-disable-usage-reports.patch=shifted 0002-friendlier-greeting.patch=failed 0003-trim-names.patch=dropped" {
		t.Fatalf("patches report: %+v", rep)
	}
	if f := rep.Patches[1].Failed; len(f) != 1 || f[0].File != "greet.go" || f[0].Hunk != 1 {
		t.Errorf("failed hunks: %+v", f)
	}
	if r := h.must(0, "patches", "-version", "1.0.0"); !strings.Contains(r.stdout, "3 patch(es): 3 exact") {
		t.Errorf("patches at the pinned version:\n%s", r.stdout)
	}

	out := h.run(0)
	if out.Status != "synced" || out.Version != "1.1.0" || out.Pinned != "1.0.0" || out.Patches == nil ||
		out.Patches.Total != 3 || out.Patches.Shifted != 1 || out.Patches.Agent != 1 || out.Patches.Dropped != 1 {
		t.Fatalf("sync: %+v", out)
	}
	if out.Commit != g.ForkHead(t) || out.Commit == forkBefore {
		t.Fatalf("fork head %s, outcome commit %s", g.ForkHead(t), out.Commit)
	}
	if g.Show(t, "upstream-version.txt") != "1.1.0\n" || g.Show(t, "revision.txt") != "1\n" || g.Has(t, "patches/0003-trim-names.patch") ||
		strings.Contains(g.Show(t, "patches/series"), "0003") || !strings.Contains(g.Show(t, "README.md"), "private edition") {
		t.Errorf("fork after the update: version %q, revision %q, series:\n%s", g.Show(t, "upstream-version.txt"), g.Show(t, "revision.txt"), g.Show(t, "patches/series"))
	}
	if p := g.Show(t, "patches/0002-friendlier-greeting.patch"); !strings.HasPrefix(p, "From: Fork Maintainer") || !strings.Contains(p, "nice to meet you") {
		t.Errorf("rewritten patch:\n%s", p)
	}

	// The updated series applies to upstream 1.1.0 and the result works.
	work := filepath.Join(h.dir, "check-"+randSuffix())
	g.Apply(t, work, "v1.1.0")
	goRun(t, work, "vet", "./...")
	goRun(t, work, "test", "./...")
	if got := strings.TrimSpace(goRun(t, work, "run", "./cmd/greet", " ada ")); got != "Hello, ada, nice to meet you!" {
		t.Errorf("greet = %q", got)
	}

	as := h.alerts()
	if countAlerts(as, "synced", "updated to upstream 1.1.0") != 1 || len(as) != 1 {
		t.Errorf("alerts: %+v", as)
	}
	if !liveLLM() && (h.fake.Count("patch") == 0 || h.fake.Count("audit") != 1 || h.fake.Count("triage") != 0) {
		t.Errorf("model calls: %+v", h.fake.Calls())
	}

	// Up to date afterwards: the release candidate v1.2.0-rc1 does not count.
	if out := h.run(0); out.Status != "up-to-date" || out.Version != "1.1.0" {
		t.Fatalf("second sync: %+v", out)
	}
	var st struct {
		Repos map[string]struct {
			Pinned   string `json:"pinned_version"`
			Upstream string `json:"upstream_version"`
		} `json:"repos"`
	}
	h.must(0, "status", "-json").json(t, &st)
	if s := st.Repos["greet"]; s.Pinned != "1.1.0" || s.Upstream != "1.1.0" {
		t.Errorf("status: %+v", st)
	}
	if r := h.must(0, "status"); !strings.Contains(r.stdout, "pinned upstream 1.1.0") {
		t.Errorf("status:\n%s", r.stdout)
	}
	if r := h.vibeci("review"); r.code != 1 || !strings.Contains(r.stderr, "patch-mode fork") {
		t.Errorf("review of a patch-mode fork: exit %d, %q", r.code, r.stderr)
	}
}
