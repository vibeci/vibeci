package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
)

func numberedLines(prefix string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

func swapLine(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains("\n"+s, "\n"+old+"\n") {
		t.Fatalf("line %q not found", old)
	}
	return strings.TrimPrefix(strings.Replace("\n"+s, "\n"+old+"\n", "\n"+new+"\n", 1), "\n")
}

func unifiedDiff(t *testing.T, file, before, after string) string {
	t.Helper()
	r := gitx.NewTestRepo(t)
	r.Commit("before", map[string]string{file: before})
	r.Write(map[string]string{file: after})
	return r.Git("diff", "--no-color") + "\n"
}

// newPatchEnv replaces the env's repo with a patch-mode fork pinned at
// upstream 1.0.0, whose patches are exact, shifted and upstreamed at
// 1.1.0. No model is needed for that update.
func newPatchEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.up = gitx.NewTestRepo(t)
	e.up.Git("config", "uploadpack.allowFilter", "true")
	e.up.Git("config", "uploadpack.allowAnySHA1InWant", "true")
	v1 := map[string]string{"a.txt": numberedLines("a", 20), "b.txt": numberedLines("b", 20), "d.txt": numberedLines("d", 20)}
	e.up.Commit("1.0.0", v1)
	e.up.Git("tag", "v1.0.0")
	e.up.Commit("1.1.0", map[string]string{"b.txt": "new1\nnew2\n" + v1["b.txt"], "d.txt": swapLine(t, v1["d.txt"], "d10", "d10 fixed")})
	e.up.Git("tag", "v1.1.0")

	root := t.TempDir()
	e.fork = &gitx.TestRepo{T: t, Dir: filepath.Join(root, "fork.git")}
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", e.fork.Dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	e.forkWork = &gitx.TestRepo{T: t, Dir: filepath.Join(root, "work")}
	if out, err := exec.Command("git", "clone", "-q", e.fork.Dir, e.forkWork.Dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	e.forkWork.Git("checkout", "-q", "-b", "main")
	patchFor := func(file, old, new string) string {
		return unifiedDiff(t, file, v1[file], swapLine(t, v1[file], old, new))
	}
	e.forkWork.Commit("patches", map[string]string{
		"upstream-version.txt": "1.0.0\n",
		"patches/series":       "a.patch\nb.patch\nd.patch\n",
		"patches/a.patch":      patchFor("a.txt", "a10", "a10 patched"),
		"patches/b.patch":      patchFor("b.txt", "b10", "b10 patched"),
		"patches/d.patch":      patchFor("d.txt", "d10", "d10 fixed"),
	})
	e.forkWork.Git("push", "-q", "origin", "HEAD:main")

	e.cfg.Repos = []*config.Repo{{
		Name:     "demo",
		Fork:     config.Remote{URL: e.fork.Dir, Branch: "main"},
		Upstream: config.Remote{URL: "file://" + e.up.Dir, Tags: "v*", TagFormat: "v{version}"},
		Patches:  &config.Patches{Series: "patches/series", VersionFile: "upstream-version.txt"},
		Verify:   []config.Command{{Name: "patched", Run: "grep -qx 'b10 patched' patched/b.txt && grep -qx 'a10 patched' patched/a.txt"}},
	}}
	e.cfg.ApplyDefaults()
	if err := e.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPatchModeSync(t *testing.T) {
	e := newPatchEnv(t)
	ctx := context.Background()
	before := e.forkMain()

	// What a sync would do.
	rep, err := e.runner.Patches(ctx, e.repo(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.From != "1.0.0" || rep.To != "1.1.0" || rep.Tag != "v1.1.0" || rep.Counts["exact"] != 1 || rep.Counts["shifted"] != 1 || rep.Counts["dropped"] != 1 {
		t.Fatalf("patches report: %+v", rep.PatchReport)
	}
	if rep, err := e.runner.Patches(ctx, e.repo(), "1.0.0"); err != nil || rep.Counts["exact"] != 3 {
		t.Fatalf("report at the pinned version: %+v %v", rep, err)
	}
	if _, err := e.runner.Patches(ctx, e.repo(), "9.9.9"); err == nil || !strings.Contains(err.Error(), "no tag v9.9.9") {
		t.Errorf("unknown version: %v", err)
	}

	out := e.sync(Options{DryRun: true})
	if out.Status != StatusDryRun || out.Version != "1.1.0" || out.Pinned != "1.0.0" || e.forkMain() != before {
		t.Fatalf("dry run: %+v", out)
	}
	if out := e.sync(Options{DryRun: true}); out.Status != StatusDryRun || !strings.Contains(out.Message, "unchanged since the dry run") {
		t.Fatalf("cached dry run: %+v", out)
	}

	out = e.sync(Options{})
	if out.Status != StatusSynced || out.Patches == nil || out.Patches.Exact != 1 || out.Patches.Shifted != 1 || out.Patches.Dropped != 1 {
		t.Fatalf("sync: %+v", out)
	}
	if !strings.Contains(out.Message, "update from upstream 1.0.0 to 1.1.0; 3 patch(es): 1 unchanged, 1 shifted, 0 refreshed, 1 dropped; checks passed") {
		t.Errorf("message: %s", out.Message)
	}
	head := e.forkMain()
	if p := parents(e.fork, head); len(p) != 1 || p[0] != before {
		t.Fatalf("parents: %v", p)
	}
	if e.show(head, "upstream-version.txt") != "1.1.0" || e.show(head, "patches/series") != "a.patch\nb.patch" || e.has(head, "patches/d.patch") {
		t.Errorf("tree after the update:\n%s", e.fork.Git("ls-tree", "-r", head))
	}
	tag := e.up.Git("rev-parse", "v1.1.0^{commit}")
	st := e.state()
	if st.PinnedVersion != "1.1.0" || st.UpstreamVersion != "1.1.0" || st.LastMerged != tag || st.LastMergedTag != "v1.1.0" || st.LastPushed != head || st.Proposal != nil {
		t.Errorf("state: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.data, "sources", "demo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("source copies kept after the update: %v", err)
	}
	if e.countKind("synced") != 1 {
		t.Errorf("events: %v", e.eventKinds())
	}

	if out := e.sync(Options{}); out.Status != StatusUpToDate || out.Message != "fork is at upstream 1.1.0" {
		t.Fatalf("second sync: %+v", out)
	}

	// Upstream moves the tag VibeCI updated to: hold until released.
	e.up.Commit("retag", map[string]string{"a.txt": numberedLines("a", 21)})
	e.up.Git("tag", "-f", "v1.1.0")
	out = e.sync(Options{})
	if out.Status != StatusHeld || !strings.Contains(out.Message, "upstream tag v1.1.0 was moved") || e.countKind("rewritten") != 1 {
		t.Fatalf("moved tag: %+v %v", out, e.eventKinds())
	}
	if err := e.runner.Release(ctx, e.repo()); err != nil {
		t.Fatal(err)
	}
	if out := e.sync(Options{}); out.Status != StatusUpToDate {
		t.Fatalf("after release: %+v", out)
	}

	// Merge-mode commands refuse.
	if _, err := e.runner.Review(ctx, e.repo(), 0); err == nil || !strings.Contains(err.Error(), "patch-mode fork") {
		t.Errorf("review: %v", err)
	}
	if _, err := e.runner.Allow(ctx, e.repo(), []string{"HEAD"}, ""); err == nil {
		t.Error("allow accepted")
	}
}

func TestPatchModeVersionURL(t *testing.T) {
	e := newPatchEnv(t)
	var version atomic.Value
	version.Store("1.2.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"versions": [{"name": "x", "version": "%s"}]}`, version.Load())
	}))
	defer srv.Close()
	r := e.repo()
	r.Upstream.Tags, r.Upstream.VersionURL, r.Upstream.VersionRegex = "", srv.URL, `"version": "([^"]+)"`

	out := e.sync(Options{})
	if out.Status != StatusError || !strings.Contains(out.Message, "has no tag v1.2.0") {
		t.Fatalf("missing tag: %+v", out)
	}
	if e.countKind("error") != 0 {
		t.Error("a single transient error must not alert")
	}
	version.Store("1.1.0")
	if out := e.sync(Options{Force: true}); out.Status != StatusSynced || out.Version != "1.1.0" {
		t.Fatalf("sync: %+v", out)
	}
	if e.show(e.forkMain(), "upstream-version.txt") != "1.1.0" {
		t.Error("pin not updated")
	}
	version.Store("1.0.0") // upstream rolled back
	if out := e.sync(Options{}); out.Status != StatusUpToDate || !strings.Contains(out.Message, "newer than upstream's current 1.0.0") {
		t.Fatalf("rollback: %+v", out)
	}
}

func TestPatchModeAgentAndFailure(t *testing.T) {
	e := newPatchEnv(t)
	// b.patch conflicts at 1.2.0; the resolver gives up first.
	e.up.Commit("1.2.0", map[string]string{"b.txt": swapLine(t, "new1\nnew2\n"+numberedLines("b", 20), "b10", "b10 changed")})
	e.up.Git("tag", "v1.2.0")
	e.repo().Verify[0].Run = "grep -qx 'b10 changed patched' patched/b.txt"
	out := e.sync(Options{})
	if out.Status != StatusFailed || out.Version != "1.2.0" || !strings.Contains(out.Message, "could not update patch patches/b.patch for upstream 1.2.0") {
		t.Fatalf("failure: %+v", out)
	}
	if e.countKind("failed") != 1 {
		t.Errorf("events: %v", e.eventKinds())
	}
	e.mu.Lock()
	title := e.events[len(e.events)-1].Title
	e.mu.Unlock()
	if title != "demo: could not update to upstream 1.2.0" {
		t.Errorf("alert title: %q", title)
	}
	if out := e.sync(Options{}); out.Status != StatusBackoff {
		t.Fatalf("expected backoff: %+v", out)
	}

	e.resolver.Func = func(req *llm.Request) (*llm.Response, error) {
		last := req.Messages[len(req.Messages)-1].Content[0]
		if last.Type == llm.ToolResultBlock {
			return llm.ToolCall("s", "submit", map[string]any{"summary": "applied to the changed line"}), nil
		}
		return llm.ToolCall("w", "write_file", map[string]any{"path": "b.txt", "content": swapLine(t, "new1\nnew2\n"+numberedLines("b", 20), "b10", "b10 changed patched")}), nil
	}
	out = e.sync(Options{Force: true})
	if out.Status != StatusSynced || out.Patches.Agent != 1 || !strings.Contains(out.Message, "1 rewritten by resolver") {
		t.Fatalf("agent sync: %+v", out)
	}
	if got := e.show(e.forkMain(), "patches/b.patch"); !strings.Contains(got, "+b10 changed patched") {
		t.Errorf("rewritten patch:\n%s", got)
	}
}
