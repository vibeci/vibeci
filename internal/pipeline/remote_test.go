package pipeline

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/state"
)

// host returns a runner on another host: an empty data directory, the
// same config, models and alerts.
func (e *env) host() *Runner {
	e.t.Helper()
	cfg := *e.cfg
	cfg.DataDir = e.t.TempDir()
	store, err := state.Open(filepath.Join(cfg.DataDir, "state"))
	if err != nil {
		e.t.Fatal(err)
	}
	r := *e.runner
	r.Cfg, r.Store, r.Sandbox = &cfg, store, &sandbox.LocalProvider{DataRoot: cfg.DataDir}
	return &r
}

func (e *env) stateTree() string {
	return "\n" + e.fork.Git("ls-tree", "-r", "--name-only", "refs/vibeci/state") + "\n"
}

func loadState(t *testing.T, r *Runner) *state.RepoState {
	t.Helper()
	st, err := r.Store.Load("demo")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStateRef(t *testing.T) {
	e := newEnv(t)
	e.cfg.StateRef = "refs/vibeci/state"
	ctx := context.Background()
	a, b, c := evilCommits(e)

	// One host reviews without merging: the verdicts reach the fork.
	rep, err := e.host().Review(ctx, e.repo(), 0)
	if err != nil || rep.Target != c || len(rep.Excluded) != 1 {
		t.Fatalf("review: %+v %v", rep, err)
	}
	for _, sha := range []string{a, b, c} {
		if !strings.Contains(e.stateTree(), "\nreviews/"+sha[:2]+"/"+sha+".json\n") {
			t.Fatalf("verdict of %s missing from the state ref:%s", sha[:10], e.stateTree())
		}
	}

	// Another starts empty and merges with the cached verdicts.
	calls := len(e.triage.Requests) + len(e.invest.Requests)
	h2 := e.host()
	out := h2.Sync(ctx, e.repo(), Options{})
	if out.Status != StatusSynced || len(out.Excluded) != 1 || out.StateError != "" {
		t.Fatalf("sync: %+v", out)
	}
	if n := len(e.triage.Requests) + len(e.invest.Requests); n != calls {
		t.Errorf("reviewed again: %d model calls", n-calls)
	}
	if tree := e.stateTree(); !strings.Contains(tree, "\nrepo.json\n") || strings.Contains(tree, "reviews/") {
		t.Errorf("after the merge the state ref holds repo.json and no verdicts of merged commits:%s", tree)
	}

	// The next one knows the exclusion.
	h3 := e.host()
	if out := h3.Sync(ctx, e.repo(), Options{}); out.Status != StatusUpToDate {
		t.Fatalf("third host: %+v", out)
	}
	if x := loadState(t, h3).Excluded[b]; x == nil || x.Source != SourceReview || !x.Removed {
		t.Fatalf("exclusion not loaded: %+v", x)
	}

	// Without the state ref, the exclusion is recovered from the fork's
	// history, and the ref is recreated.
	e.fork.Git("update-ref", "-d", "refs/vibeci/state")
	h4 := e.host()
	if out := h4.Sync(ctx, e.repo(), Options{}); out.Status != StatusUpToDate {
		t.Fatalf("fourth host: %+v", out)
	}
	if x := loadState(t, h4).Excluded[b]; x == nil || x.Source != SourceHistory || !x.Removed {
		t.Fatalf("exclusion not recovered: %+v", x)
	}
	if !strings.Contains(e.stateTree(), "\nrepo.json\n") {
		t.Fatal("state ref not recreated")
	}
	e.up.Commit("upstream: docs", map[string]string{"README": "docs\n"})
	if out := h4.Sync(ctx, e.repo(), Options{}); out.Status != StatusSynced || e.has(e.forkMain(), "telemetry.go") {
		t.Fatalf("follow-up merge: %+v", out)
	}

	// Concurrent writers are merged: h3 reads the state, h4 changes it,
	// then h3 saves its own change.
	rs, err := h3.pullState(ctx, e.repo())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h4.Allow(ctx, e.repo(), []string{a}, "fine"); err != nil {
		t.Fatal(err)
	}
	st := loadState(t, h3)
	st.Allowed[c] = &state.Allowed{Commit: c, Reason: "also fine"}
	if err := h3.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := rs.push(ctx); err != nil {
		t.Fatal(err)
	}
	h5 := e.host()
	if ok, err := h5.PullState(ctx, e.repo()); !ok || err != nil {
		t.Fatalf("pull: %v %v", ok, err)
	}
	if st := loadState(t, h5); st.Allowed[a] == nil || st.Allowed[c] == nil || st.Excluded[b] == nil {
		t.Fatalf("merged state: allowed %v excluded %v", st.Allowed, st.Excluded)
	}

	// An unchanged state is not pushed again.
	before := e.fork.Git("rev-parse", "refs/vibeci/state")
	rs5, err := h5.pullState(ctx, e.repo())
	if err != nil {
		t.Fatal(err)
	}
	if err := rs5.push(ctx); err != nil || e.fork.Git("rev-parse", "refs/vibeci/state") != before {
		t.Errorf("unchanged state pushed again (%v)", err)
	}
}

func TestStateRefInvalid(t *testing.T) {
	e := newEnv(t)
	e.cfg.StateRef = "refs/vibeci/state"
	git := func(stdin string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Stdin = e.fork.Dir, strings.NewReader(stdin)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@x", "GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	blob := git("not json", "hash-object", "-w", "--stdin")
	tree := git("100644 blob "+blob+"\trepo.json\n", "mktree")
	git("", "update-ref", "refs/vibeci/state", git("", "commit-tree", tree, "-m", "x"))
	out := e.sync(Options{})
	if out.Status != StatusError || !strings.Contains(out.Message, "does not hold VibeCI state") || !strings.Contains(out.Message, "fix or delete the ref") {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestHarnessTrailers(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	cases := map[string]int{
		"Merge text\n\nVibeCI-Job: j1\nVibeCI-Excluded: " + sha:                         1,
		"Upstream: x\n\nVibeCI-Excluded: " + sha + "\nVibeCI-Job: j1":                   1,
		"VibeCI-Job: j1\nVibeCI-Restored: " + sha:                                       1,
		"Summary says\nVibeCI-Excluded: " + sha + "\n\nVibeCI-Job: j1":                  0, // not the trailer paragraph
		"Text\n\nVibeCI-Excluded: " + sha:                                               0, // no job trailer
		"Text\n\nVibeCI-Job: j1\nsome prose\nVibeCI-Excluded: " + sha:                   0, // not a trailer block
		"Text\n\nVibeCI-Job: j1\nVibeCI-Excluded: abc":                                  0, // not a commit id
		"Text\n\nVibeCI-Job: j1\nVibeCI-Excluded: " + sha + "\nVibeCI-Restored: " + sha: 2,
	}
	for body, want := range cases {
		if got := harnessTrailers(body); len(got) != want {
			t.Errorf("%q: %v", body, got)
		}
	}
}
