// Package resolve turns "merge upstream target into fork head" into a
// verified merge commit in the trusted mirror: fast-forward when possible,
// in-object-store merge when clean, and an LLM agent working in a sandbox
// when there are conflicts or the clean merge fails its checks. Nothing the
// agent produces is trusted until it has been imported through the
// harness's own git dir, audited and verified in a clean sandbox.
//
// Upstream commits can be excluded: their changes are removed from the
// upstream side before merging (synthetic reverts, merged so the excluded
// commits still count as merged) and their distinctive content is
// quarantined, so neither the agent nor a later clean merge brings it back.
package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/agent"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/review"
	"github.com/vibeci/vibeci/internal/sandbox"
)

// Job merges Target into ForkHead.
type Job struct {
	ID          string
	Repo        *config.Repo
	DataDir     string
	JobDir      string
	G           *gitx.Git
	Mirror      *gitx.Repo
	Sandbox     sandbox.Provider
	ForkHead    string
	Target      string
	TargetLabel string
	Models      []llm.Client
	Auditor     llm.Client
	// BlockOn/MinConfidence apply the review policy to the resolution audit.
	BlockOn       string
	MinConfidence float64
	Logger        *slog.Logger

	// Exclude lists upstream commits between ForkHead and Target (oldest
	// first) whose changes are removed before merging (ModeMerge only).
	Exclude []Exclusion
	// Quarantine lists commits whose distinctive content must not appear in
	// the result (typically everything excluded earlier). Exclude and
	// ModeRemove commits are added automatically.
	Quarantine []Quarantined
	// Mode is ModeMerge, ModeRemove or ModeRestore.
	Mode string
	// Reason explains ModeRemove/ModeRestore to the agent and in the commit
	// message.
	Reason string

	MergeBase    string
	baseline     string
	baselineTree map[string]gitx.TreeEntry
	oursTree     map[string]gitx.TreeEntry
	theirsTree   map[string]gitx.TreeEntry
	keepOurs     []string
	conflicted   []string
	usage        llm.Usage

	upstream   string       // Target as requested, before exclusions
	modeCommit *gitx.Commit // the commit removed or restored
	removed    []string     // excluded commits removed from the upstream side
	subResults []*Result

	// Sub-jobs remove one excluded commit from the upstream side when its
	// revert conflicts with later upstream changes.
	prefix string       // name prefix of the sub-job's files in JobDir
	sub    *gitx.Commit // the excluded commit
	subWhy string
	parent *Job

	fps         map[string]*fingerprint
	commits     map[string]*gitx.Commit
	exceptLines map[string]map[string]bool
	exceptBlobs map[string]map[string]bool

	// env is passed to prefetch and verify commands.
	env map[string]string
}

// Result describes the produced commit.
type Result struct {
	Commit      string
	FastForward bool
	Conflicts   []string
	KeptOurs    []string
	AgentModel  string
	AgentTurns  int
	Summary     string
	Verify      *VerifyReport
	Audit       *review.Verdict
	NovelLines  int
	// Untracked lists files the agent created but did not declare (left out).
	Untracked []string
	// Removed lists the upstream commits whose changes the result removes.
	Removed []string
	// Restored is the commit re-applied by ModeRestore.
	Restored string
	// ExclusionAgents counts excluded commits whose removal needed the agent.
	ExclusionAgents int
	Usage           llm.Usage
}

// ErrAuditRejected means the security audit flagged lines written by the
// merge agent. The job stops: a manipulated agent must not get retries.
var ErrAuditRejected = errors.New("merge resolution rejected by the security audit")

// FailedError means the job could not produce an acceptable result, e.g.
// every model failed to produce an acceptable merge.
type FailedError struct {
	// What failed (default "could not produce an acceptable merge").
	What     string
	Attempts []string
}

func (e *FailedError) Error() string {
	what := e.What
	if what == "" {
		what = "could not produce an acceptable merge"
	}
	return what + ": " + strings.Join(e.Attempts, "; ")
}

// infraError aborts the job instead of escalating to the next model.
type infraError struct{ err error }

func (e *infraError) Error() string { return e.err.Error() }
func (e *infraError) Unwrap() error { return e.err }

// Usage returns the LLM usage accumulated so far (also after failures).
func (j *Job) Usage() llm.Usage { return j.usage }

func (j *Job) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.Default()
	}
	return j.Logger
}

// path returns the location of a job file (sub-jobs share the job dir).
func (j *Job) path(name string) string {
	return filepath.Join(j.JobDir, j.prefix+name)
}

func (j *Job) label() string {
	if j.TargetLabel != "" {
		return j.TargetLabel
	}
	if j.upstream != "" {
		return short(j.upstream)
	}
	return short(j.Target)
}

// Run produces the merge commit (not pushed).
func (j *Job) Run(ctx context.Context) (*Result, error) {
	if err := os.MkdirAll(j.JobDir, 0o755); err != nil {
		return nil, err
	}
	if j.fps == nil {
		j.fps = map[string]*fingerprint{}
	}
	if j.commits == nil {
		j.commits = map[string]*gitx.Commit{}
	}
	j.upstream = j.Target
	switch j.Mode {
	case ModeMerge:
		for _, x := range j.Exclude {
			j.quarantine(x.Commit, j.ForkHead)
		}
		if len(j.Exclude) > 0 {
			t, err := j.cleanTarget(ctx)
			if err != nil {
				return nil, err
			}
			j.Target = t
		}
	case ModeRemove, ModeRestore:
		if len(j.Exclude) > 0 {
			return nil, errors.New("internal error: exclusions only apply to upstream merges")
		}
		if !gitx.IsHex(j.Target) {
			return nil, fmt.Errorf("invalid commit %q", j.Target)
		}
		c, err := j.commitInfo(ctx, j.Target)
		if err != nil {
			return nil, err
		}
		if ok, err := j.Mirror.IsAncestor(ctx, c.SHA, j.ForkHead); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("commit %s is not part of the fork", short(c.SHA))
		}
		j.modeCommit = c
		if j.Mode == ModeRemove {
			j.quarantine(c.SHA, "")
		} else {
			kept := j.Quarantine[:0:0]
			for _, q := range j.Quarantine {
				if q.Commit != c.SHA {
					kept = append(kept, q)
				}
			}
			j.Quarantine = kept
		}
		if j.Target, err = j.synthetic(ctx, c, j.Mode == ModeRestore); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown job mode %q", j.Mode)
	}
	res, err := j.merge(ctx)
	if res != nil {
		res.Removed = j.removed
		res.ExclusionAgents = len(j.subResults)
		switch j.Mode {
		case ModeRemove:
			res.Removed = []string{j.modeCommit.SHA}
		case ModeRestore:
			res.Restored = j.modeCommit.SHA
		}
		res.Usage = j.usage
	}
	return res, err
}

// quarantine adds sha unless it is already quarantined (an existing entry
// is made stricter when except differs).
func (j *Job) quarantine(sha, except string) {
	for i, q := range j.Quarantine {
		if q.Commit == sha {
			if q.Except != except {
				j.Quarantine[i].Except = ""
			}
			return
		}
	}
	j.Quarantine = append(j.Quarantine, Quarantined{Commit: sha, Except: except})
}

func (j *Job) merge(ctx context.Context) (*Result, error) {
	var err error
	if j.MergeBase, err = j.Mirror.MergeBase(ctx, j.ForkHead, j.Target); err != nil {
		return nil, err
	}
	if j.MergeBase == "" {
		return nil, errors.New("fork and upstream share no history")
	}
	if j.oursTree, err = j.Mirror.LsTree(ctx, j.ForkHead); err != nil {
		return nil, err
	}
	if j.theirsTree, err = j.Mirror.LsTree(ctx, j.Target); err != nil {
		return nil, err
	}
	if j.sub == nil {
		j.keepOurs = j.keepOursPaths(j.oursTree, j.theirsTree)
	}

	// Fast-forward only to unmodified upstream commits (which are not
	// verified, like upstream's own history) or within a sub-job.
	canFF := (j.Mode == ModeMerge && len(j.removed) == 0) || j.sub != nil
	if canFF && j.MergeBase == j.ForkHead && j.keepOursUnchanged() {
		vs, err := j.violations(ctx, j.Target)
		if err != nil {
			return nil, err
		}
		if len(vs) == 0 {
			j.logger().Info("fast-forward", "repo", j.Repo.Name, "target", short(j.Target))
			return &Result{Commit: j.Target, FastForward: true, Verify: &VerifyReport{Stage: "verify"}}, nil
		}
		j.logger().Warn("fast-forward would bring back excluded content; merging instead", "repo", j.Repo.Name, "violations", len(vs))
	}

	mt, err := j.Mirror.MergeTree(ctx, j.ForkHead, j.Target)
	if err != nil {
		return nil, err
	}
	if j.baseline, err = j.applyKeepOursTree(ctx, mt.Tree); err != nil {
		return nil, err
	}
	if j.baselineTree, err = j.Mirror.LsTree(ctx, j.baseline); err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, p := range j.keepOurs {
		keep[p] = true
	}
	for _, p := range mt.ConflictedPaths() {
		if !keep[p] {
			j.conflicted = append(j.conflicted, p)
		}
	}
	keptOurs := j.changedKeepOurs()
	withKept := func(res *Result, err error) (*Result, error) {
		if res != nil {
			res.KeptOurs = keptOurs
		}
		return res, err
	}

	if len(j.conflicted) == 0 {
		vs, err := j.violations(ctx, j.baseline)
		if err != nil {
			return nil, err
		}
		if len(vs) > 0 {
			j.logger().Warn("clean merge contains excluded content; starting agent", "repo", j.Repo.Name, "violations", len(vs))
			return withKept(j.runAgents(ctx, nil, vs))
		}
		commit, err := j.commit(ctx, j.baseline, "", nil, "")
		if err != nil {
			return nil, err
		}
		rep, err := j.cleanVerify(ctx, commit, 0)
		if err != nil {
			return nil, err
		}
		if rep.Passed() {
			j.logger().Info("clean merge verified", "repo", j.Repo.Name, "commit", short(commit), "checks", rep.Summary())
			return &Result{Commit: commit, KeptOurs: keptOurs, Verify: rep}, nil
		}
		j.logger().Warn("clean merge fails checks; starting agent", "repo", j.Repo.Name, "checks", rep.Summary())
		return withKept(j.runAgents(ctx, rep, nil))
	}
	j.logger().Info("merge has conflicts; starting agent", "repo", j.Repo.Name, "conflicts", len(j.conflicted))
	return withKept(j.runAgents(ctx, nil, nil))
}

// keepOursUnchanged reports whether keep_ours would not alter a
// fast-forward (the fork's version of every protected path equals upstream's).
func (j *Job) keepOursUnchanged() bool {
	for _, p := range j.keepOurs {
		o, inO := j.oursTree[p]
		t, inT := j.theirsTree[p]
		if inO != inT || o.OID != t.OID || o.Mode != t.Mode {
			return false
		}
	}
	return true
}

// changedKeepOurs lists protected paths where upstream's change was dropped.
func (j *Job) changedKeepOurs() []string {
	var out []string
	for _, p := range j.keepOurs {
		o, inO := j.oursTree[p]
		t, inT := j.theirsTree[p]
		if inO != inT || o.OID != t.OID {
			out = append(out, p)
		}
	}
	return out
}

func (j *Job) runAgents(ctx context.Context, verifyFailure *VerifyReport, vs []Violation) (*Result, error) {
	if len(j.Models) == 0 {
		return nil, &FailedError{Attempts: []string{"no resolve models configured"}}
	}
	var attempts []string
	for i, m := range j.Models {
		res, err := j.runAgent(ctx, i, m, verifyFailure, vs)
		if err == nil {
			res.Usage = j.usage
			return res, nil
		}
		var ie *infraError
		if errors.Is(err, ErrAuditRejected) || errors.As(err, &ie) || ctx.Err() != nil {
			return nil, err
		}
		j.logger().Warn("merge agent failed", "repo", j.Repo.Name, "model", m.Name(), "err", err)
		attempts = append(attempts, fmt.Sprintf("%s: %v", m.Name(), err))
	}
	return nil, &FailedError{Attempts: attempts}
}

type submitState struct {
	rounds        int
	warnedDropped bool
	result        *Result
}

func (j *Job) transcript(name string) io.WriteCloser {
	dir := filepath.Join(j.JobDir, "transcripts")
	os.MkdirAll(dir, 0o755)
	f, err := os.Create(filepath.Join(dir, j.prefix+name+".jsonl"))
	if err != nil {
		return nopWriteCloser{}
	}
	return f
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func (j *Job) runAgent(ctx context.Context, attempt int, model llm.Client, verifyFailure *VerifyReport, vs []Violation) (*Result, error) {
	ws, err := j.prepareMerge(ctx, fmt.Sprintf("work-%d", attempt))
	if err != nil {
		return nil, &infraError{err}
	}
	cache := j.path("cache-agent")
	var prefetchNote string
	if len(j.Repo.Prefetch) > 0 {
		rep, err := j.prefetch(ctx, ws, cache)
		if err != nil {
			return nil, &infraError{err}
		}
		if !rep.Passed() {
			prefetchNote = "Dependency prefetch FAILED before you started (this is often caused by the conflicts themselves):\n" + rep.Failure(4000)
		}
	}
	sb, err := j.newSandbox(ctx, j.Repo.Sandbox.Profile, ws, cache)
	if err != nil {
		return nil, &infraError{err}
	}
	defer closeSandbox(ctx, sb)

	tw := j.transcript(fmt.Sprintf("resolve-%d-%s", attempt, sanitize(model.Name())))
	defer tw.Close()
	st := &submitState{}
	tools := j.agentTools(ws, sb, cache, st, model, tw)
	brief := j.brief(ctx, ws, sb, verifyFailure, vs, prefetchNote)
	system := resolverSystem
	if j.sub != nil {
		system = excludeSystem
	}

	out, err := agent.Run(ctx, agent.Config{
		Name:          "resolve",
		Client:        model,
		System:        system,
		Tools:         tools,
		MaxTurns:      j.Repo.Resolve.MaxTurns,
		Timeout:       j.Repo.Resolve.Timeout.Duration,
		MaxToolOutput: 30000,
		Transcript:    tw,
		Logger:        j.logger(),
	}, []llm.Block{llm.Text(brief)})
	if out != nil {
		j.usage.Add(out.Usage)
	}
	if err != nil {
		return nil, err
	}
	if out.Terminal == "give_up" {
		var a struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(out.Input, &a)
		return nil, fmt.Errorf("agent gave up: %s", clip(a.Reason, 1000))
	}
	if st.result == nil {
		return nil, errors.New("agent finished without an accepted submission")
	}
	st.result.AgentModel = model.Name()
	st.result.AgentTurns = out.Turns
	return st.result, nil
}

func (j *Job) agentTools(ws *workspace, sb sandbox.Sandbox, cache string, st *submitState, model llm.Client, tw io.Writer) []*agent.Tool {
	ft := &fileTools{dir: ws.Dir}
	tools := []*agent.Tool{
		shellTool(sb, j.Repo.Resolve.CommandTimeout.Duration),
		{Name: "read_file", Description: "Read a file in /workspace with line numbers (or list a directory). Use start_line/end_line for large files.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string","description":"relative to /workspace"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
			Run:    ft.read},
		{Name: "write_file", Description: "Create or overwrite a file in /workspace with the given content.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run:    ft.write},
		{Name: "edit_file", Description: "Replace an exact string in a file. old_string must match exactly once unless replace_all is set. Prefer this over write_file for large files.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
			Run:    ft.edit},
		{Name: "list_conflicts", Description: "List the files that were conflicted and whether conflict markers remain in them.",
			Schema: js(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return agent.OK(j.conflictStatus(ws)), nil
			}},
	}
	if len(j.Repo.Verify) > 0 {
		tools = append(tools, &agent.Tool{
			Name: "run_checks", Description: "Run the configured checks (the same commands the harness runs on the final merge) in your sandbox and report the results.",
			Schema: js(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				rep, err := runCommands(ctx, sb, "verify", j.Repo.Verify, j.env)
				if err != nil {
					return agent.Result{}, err
				}
				if rep.Passed() {
					return agent.OK("all checks passed: " + rep.Summary()), nil
				}
				return agent.OK(rep.Failure(15000)), nil
			}})
	}
	if len(j.Repo.Prefetch) > 0 {
		tools = append(tools, &agent.Tool{
			Name: "fetch_dependencies", Description: "Re-run the project's dependency fetch commands (with network access, in a separate sandbox) after changing dependency manifests.",
			Schema: js(`{"type":"object","properties":{}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				rep, err := j.prefetch(ctx, ws, cache)
				if err != nil {
					return agent.Result{}, err
				}
				if rep.Passed() {
					return agent.OK("dependencies fetched: " + rep.Summary()), nil
				}
				return agent.OK(rep.Failure(10000)), nil
			}})
	}
	tools = append(tools,
		&agent.Tool{
			Name:        "submit",
			Description: "Submit the resolved merge. The harness imports the working tree, audits it, and re-runs the checks in a clean sandbox; problems are reported back to you.",
			Schema:      js(`{"type":"object","properties":{"summary":{"type":"string","description":"how each conflict was resolved and any noteworthy decisions"},"created_files":{"type":"array","items":{"type":"string"},"description":"new files you intentionally created that must be part of the merge"},"confirm_dropped_fork_changes":{"type":"boolean","description":"set after a warning, if dropping those lines was intentional"}},"required":["summary"]}`),
			Terminal:    true,
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return j.submit(ctx, ws, st, model, tw, in)
			},
		},
		&agent.Tool{
			Name:        "give_up",
			Description: "Abandon the merge when it cannot be completed correctly. Explain precisely why.",
			Schema:      js(`{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`),
			Terminal:    true,
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return agent.OK("acknowledged"), nil
			},
		},
	)
	return tools
}

func (j *Job) conflictStatus(ws *workspace) string {
	if len(ws.Conflicts) == 0 {
		return "There were no textual conflicts in this merge."
	}
	unresolved := map[string]bool{}
	for _, p := range markerFiles(ws.Dir, ws.Conflicts) {
		unresolved[p] = true
	}
	root, _ := os.OpenRoot(ws.Dir)
	var sb strings.Builder
	for _, c := range ws.Conflicts {
		status := "resolved (no markers)"
		if unresolved[c.Path] {
			status = "UNRESOLVED (markers present)"
		} else if root != nil {
			if _, err := root.Lstat(c.Path); err != nil {
				status = "deleted"
			}
		}
		fmt.Fprintf(&sb, "%s  [%s] %s\n", c.Path, c.Describe(), status)
	}
	if root != nil {
		root.Close()
	}
	return sb.String()
}

func (j *Job) submit(ctx context.Context, ws *workspace, st *submitState, model llm.Client, tw io.Writer, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Summary        string   `json:"summary"`
		CreatedFiles   []string `json:"created_files"`
		ConfirmDropped bool     `json:"confirm_dropped_fork_changes"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	st.rounds++
	if st.rounds > j.Repo.Resolve.MaxRounds {
		return agent.Result{}, fmt.Errorf("%d submissions rejected; giving up on this model", st.rounds-1)
	}
	if m := markerFiles(ws.Dir, ws.Conflicts); len(m) > 0 {
		return agent.Errorf("not accepted: conflict markers remain in: %s", strings.Join(m, ", ")), nil
	}
	tree, untracked, err := j.importTree(ctx, ws, a.CreatedFiles, st.rounds)
	if err != nil {
		return agent.Errorf("not accepted: %v", err), nil
	}
	untrackedNote := ""
	if len(untracked) > 0 {
		list := untracked
		if len(list) > 30 {
			list = append(list[:30:30], fmt.Sprintf("(+%d more)", len(untracked)-30))
		}
		untrackedNote = "\nNote: these untracked files were NOT included (list them in created_files if they belong in the merge): " + strings.Join(list, ", ")
	}
	rep, err := j.audit(ctx, tree, j.conflictList(ws))
	if err != nil {
		return agent.Result{}, &infraError{err}
	}
	if len(rep.Markers) > 0 {
		return agent.Errorf("not accepted: conflict markers remain in: %s", strings.Join(rep.Markers, ", ")), nil
	}
	if len(rep.Gitlinks) > 0 {
		return agent.Errorf("not accepted: new submodule entries (nested .git directories?) at: %s. Remove them.", strings.Join(rep.Gitlinks, ", ")), nil
	}
	if rep.Novel > j.Repo.Resolve.MaxNovelLines {
		return agent.Errorf("not accepted: you wrote %d lines that exist in neither side of the merge (limit %d). Keep the resolution minimal.", rep.Novel, j.Repo.Resolve.MaxNovelLines), nil
	}
	if vs, err := j.violations(ctx, tree); err != nil {
		return agent.Result{}, &infraError{err}
	} else if len(vs) > 0 {
		return agent.Errorf("not accepted: the result contains code from upstream commits that are excluded from this fork (blocked by the security review or by an operator):\n%sRemove it. Where other code depends on it, adapt or remove that code instead of keeping the excluded lines.", violationText(vs, 40)), nil
	}
	if files, n := rep.droppedTotal(); n >= 5 && !a.ConfirmDropped && !st.warnedDropped {
		st.warnedDropped = true
		var sample []string
		for _, f := range rep.Files {
			for _, l := range f.Dropped {
				if len(sample) < 12 {
					sample = append(sample, f.Path+": "+clip(l, 160))
				}
			}
		}
		who, keep := "the fork added", "The fork's changes must be preserved unless upstream now provides the same behaviour."
		if j.sub != nil {
			who, keep = "later upstream commits added", "Later upstream changes must be kept unless they depend on the excluded commit."
		}
		return agent.Errorf("not accepted yet: %d lines that %s are missing from the result, in %s.\nExamples:\n  %s\n%s Restore them, or if dropping them is intentional, call submit again with confirm_dropped_fork_changes=true and explain why in the summary.",
			n, who, strings.Join(files, ", "), strings.Join(sample, "\n  "), keep), nil
	}

	var verdict *review.Verdict
	if rep.Novel > 0 {
		v, usage, err := review.Audit(ctx, j.Auditor, rep.novelReport(80000), tw, j.logger())
		j.usage.Add(usage)
		if err != nil {
			return agent.Result{}, &infraError{err}
		}
		verdict = v
		j.logger().Info("resolution audit", "repo", j.Repo.Name, "verdict", v.Verdict, "confidence", v.Confidence, "novel_lines", rep.Novel)
		if v.Blocks(j.BlockOn, j.MinConfidence) {
			return agent.Result{}, fmt.Errorf("%w: %s", ErrAuditRejected, v.Summary)
		}
	}

	commit, err := j.commit(ctx, tree, a.Summary, conflictPaths(ws), model.Name())
	if err != nil {
		return agent.Result{}, &infraError{err}
	}
	vr, err := j.cleanVerify(ctx, commit, st.rounds)
	if err != nil {
		return agent.Result{}, &infraError{err}
	}
	if !vr.Passed() {
		return agent.Errorf("not accepted: the merge was imported, but the checks failed in a clean sandbox:\n%s%s\nFix the problem and submit again.", vr.Failure(12000), untrackedNote), nil
	}
	st.result = &Result{
		Commit:     commit,
		Conflicts:  conflictPaths(ws),
		Summary:    a.Summary,
		Verify:     vr,
		Audit:      verdict,
		NovelLines: rep.Novel,
		Untracked:  untracked,
	}
	return agent.OK("accepted: " + vr.Summary()), nil
}

// conflictList returns the trusted conflict set (from merge-tree) with the
// workspace's descriptive codes where available.
func (j *Job) conflictList(ws *workspace) []Conflict {
	codes := map[string]string{}
	for _, c := range ws.Conflicts {
		codes[c.Path] = c.Code
	}
	var out []Conflict
	for _, p := range j.conflicted {
		out = append(out, Conflict{Path: p, Code: codes[p]})
	}
	return out
}

func conflictPaths(ws *workspace) []string {
	var out []string
	for _, c := range ws.Conflicts {
		out = append(out, c.Path)
	}
	return out
}

// commit creates the merge commit object (parents: fork head, target).
func (j *Job) commit(ctx context.Context, tree, summary string, conflicts []string, model string) (string, error) {
	parents := []string{j.ForkHead, j.Target}
	if j.sub != nil {
		return j.Mirror.CommitTree(ctx, tree, parents, j.exclusionMessage(j.sub, j.subWhy, summary, model, conflicts))
	}
	var sb strings.Builder
	branch := j.Repo.Fork.Branch
	switch j.Mode {
	case ModeRemove:
		c := j.modeCommit
		fmt.Fprintf(&sb, "Remove upstream commit %s (%s) from %s\n\n", short(c.SHA), clip(c.Subject, 80), branch)
		fmt.Fprintf(&sb, "Upstream commit %s is excluded from the fork: its changes are\nremoved and kept out of later merges.\n", c.SHA)
		if r := strings.TrimSpace(j.Reason); r != "" {
			sb.WriteString("Reason: " + wrap(clip(r, 1500), 72) + "\n")
		}
	case ModeRestore:
		c := j.modeCommit
		fmt.Fprintf(&sb, "Restore upstream commit %s (%s) in %s\n\n", short(c.SHA), clip(c.Subject, 80), branch)
		fmt.Fprintf(&sb, "Upstream commit %s had been excluded and is allowed again: its\nchanges are re-applied.\n", c.SHA)
		if r := strings.TrimSpace(j.Reason); r != "" {
			sb.WriteString("Reason: " + wrap(clip(r, 1500), 72) + "\n")
		}
	default:
		fmt.Fprintf(&sb, "Merge upstream %s into %s\n\n", j.label(), branch)
		fmt.Fprintf(&sb, "Upstream: %s @ %s\n", j.Repo.Upstream.URL, j.upstream)
		if len(j.Exclude) > 0 {
			sb.WriteString("Excluded upstream commits (their changes are not merged):\n")
			for _, x := range j.Exclude {
				subject := ""
				if c := j.commits[x.Commit]; c != nil {
					subject = clip(c.Subject, 60)
				}
				fmt.Fprintf(&sb, "  %s %s\n", short(x.Commit), subject)
			}
		}
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(&sb, "Conflicts resolved by VibeCI (%s): %s\n", model, strings.Join(limitList(conflicts, 20), ", "))
	}
	if kept := j.changedKeepOurs(); len(kept) > 0 {
		fmt.Fprintf(&sb, "Kept fork version (keep_ours): %s\n", strings.Join(kept, ", "))
	}
	if s := strings.TrimSpace(summary); s != "" {
		sb.WriteString("\n" + wrap(clip(s, 4000), 72) + "\n")
	}
	fmt.Fprintf(&sb, "\n%s: %s\n", TrailerJob, j.ID)
	switch j.Mode {
	case ModeRemove:
		fmt.Fprintf(&sb, "%s: %s\n", TrailerExcluded, j.modeCommit.SHA)
	case ModeRestore:
		fmt.Fprintf(&sb, "%s: %s\n", TrailerRestored, j.modeCommit.SHA)
	default:
		for _, x := range j.Exclude {
			fmt.Fprintf(&sb, "%s: %s\n", TrailerExcluded, x.Commit)
		}
	}
	return j.Mirror.CommitTree(ctx, tree, parents, sb.String())
}

func (j *Job) gitLog(ctx context.Context, args ...string) string {
	out, _ := j.G.Run(ctx, gitx.Opts{GitDir: j.Mirror.GitDir, MaxOut: 64 << 10, Truncate: true, LiteralPathspecs: true},
		append([]string{"log", "--no-color", "--format=%h %s", "-n", "60"}, args...)...)
	return indentBlock(string(out))
}

func (j *Job) brief(ctx context.Context, ws *workspace, sb sandbox.Sandbox, verifyFailure *VerifyReport, vs []Violation, prefetchNote string) string {
	if j.sub != nil {
		return j.excludeBrief(ctx, ws, sb, vs, prefetchNote)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Fork: %s (%s, branch %s)\n", j.Repo.Name, j.Repo.Fork.URL, j.Repo.Fork.Branch)
	label := j.TargetLabel
	if label == "" || j.Mode != ModeMerge {
		label = "branch " + j.Repo.Upstream.Branch
	}
	fmt.Fprintf(&b, "Upstream: %s (%s)\n", j.Repo.Upstream.URL, label)
	if d := strings.TrimSpace(j.Repo.Description); d != "" {
		fmt.Fprintf(&b, "What the fork changes, according to its maintainer:\n%s\n", d)
	}
	branch := j.Repo.Fork.Branch
	switch j.Mode {
	case ModeRemove:
		c := j.modeCommit
		fmt.Fprintf(&b, "\nTask: REMOVE upstream commit %s (%q) from the fork's %s (%s). The fork merged it earlier; it is now excluded.\n", c.SHA, clip(c.Subject, 120), branch, short(j.ForkHead))
		if r := strings.TrimSpace(j.Reason); r != "" {
			fmt.Fprintf(&b, "Why: %s\n", clip(r, 1500))
		}
		b.WriteString("The ref \"upstream\" is a synthetic commit that undoes it (the tree of its parent, on top of it), and the ref \"excluded\" is the commit itself; merging \"upstream\" removes the commit's changes. Where later fork or upstream code depends on the removed code, remove or minimally adapt that code. Do not re-introduce the removed lines: the harness rejects results that contain them.\n")
	case ModeRestore:
		c := j.modeCommit
		fmt.Fprintf(&b, "\nTask: RESTORE upstream commit %s (%q) in the fork's %s (%s). It had been excluded from the fork and is now allowed again.\n", c.SHA, clip(c.Subject, 120), branch, short(j.ForkHead))
		if r := strings.TrimSpace(j.Reason); r != "" {
			fmt.Fprintf(&b, "Why: %s\n", clip(r, 1500))
		}
		b.WriteString("The ref \"upstream\" is a synthetic commit with the commit's tree on top of its parent, and the ref \"restored\" is the commit itself; merging \"upstream\" re-applies the commit's changes. Keep the fork's later changes and adapt the restored code to them where needed.\n")
	default:
		fmt.Fprintf(&b, "\nMerging upstream %s into the fork's %s (%s); merge base %s.\n", short(j.upstream), branch, short(j.ForkHead), short(j.MergeBase))
		fmt.Fprintf(&b, "\nUpstream changes being merged (first-parent, newest first, max 60):\n%s\n", j.gitLog(ctx, "--first-parent", j.Target, "^"+j.ForkHead))
		fmt.Fprintf(&b, "The fork's own commits (not in upstream, newest first, max 60):\n%s\n", j.gitLog(ctx, "--no-merges", j.ForkHead, "^"+j.Target))
		if len(j.Exclude) > 0 {
			b.WriteString("Upstream commits EXCLUDED from this merge (blocked by the security review or by an operator). Their changes were already removed from the upstream side (the ref \"upstream\" includes the synthetic removal commits). Do not re-introduce any of their code: the harness rejects results that contain it.\n")
			for _, x := range j.Exclude {
				subject := ""
				if c := j.commits[x.Commit]; c != nil {
					subject = clip(c.Subject, 100)
				}
				fmt.Fprintf(&b, "  - %s %s\n", short(x.Commit), subject)
				if r := strings.TrimSpace(x.Reason); r != "" {
					fmt.Fprintf(&b, "    reason: %s\n", clip(r, 400))
				}
			}
			b.WriteString("\n")
		}
	}

	if len(ws.Conflicts) > 0 {
		fmt.Fprintf(&b, "Conflicted files (%d):\n", len(ws.Conflicts))
		for _, c := range ws.Conflicts {
			fmt.Fprintf(&b, "  - %s (%s)\n", c.Path, c.Describe())
		}
	} else {
		b.WriteString("The merge has no textual conflicts.\n")
	}
	if kept := j.changedKeepOurs(); len(kept) > 0 {
		fmt.Fprintf(&b, "Paths kept at the fork's version by policy (already handled; leave them alone): %s\n", strings.Join(kept, ", "))
	}
	if verifyFailure != nil {
		fmt.Fprintf(&b, "\nThe merge applied cleanly, but the required checks FAILED in a clean build. Fix the code so they pass (typically fork code that no longer matches upstream changes):\n%s\n", verifyFailure.Failure(12000))
	}
	if len(vs) > 0 {
		fmt.Fprintf(&b, "\nThe merge applied cleanly, but the result contains code from upstream commits that are excluded from this fork:\n%sRemove that code. Where other code depends on it, adapt or remove that code instead of keeping the excluded lines.\n", violationText(vs, 40))
	}
	if len(j.Repo.Verify) > 0 {
		b.WriteString("\nRequired checks (re-run in a clean sandbox on submit):\n")
		for _, c := range j.Repo.Verify {
			fmt.Fprintf(&b, "  - %s: %s\n", c.Name, c.Run)
		}
	} else {
		b.WriteString("\nNo checks are configured; make sure the code still builds if you can.\n")
	}
	j.sandboxNote(&b, sb, prefetchNote)
	b.WriteString("\nStart by inspecting the conflicts (list_conflicts, git diff, git log). Resolve, verify, then submit.")
	return b.String()
}

// excludeBrief is the brief of a sub-job that removes an excluded commit
// from the upstream side.
func (j *Job) excludeBrief(ctx context.Context, ws *workspace, sb sandbox.Sandbox, vs []Violation, prefetchNote string) string {
	c := j.sub
	var b strings.Builder
	fmt.Fprintf(&b, "Fork: %s (%s, branch %s)\n", j.Repo.Name, j.Repo.Fork.URL, j.Repo.Fork.Branch)
	fmt.Fprintf(&b, "Upstream: %s (%s)\n", j.Repo.Upstream.URL, j.parent.label())
	fmt.Fprintf(&b, "\nTask: remove upstream commit %s (%q) from the upstream snapshot %s, before the snapshot is merged into the fork.\n", c.SHA, clip(c.Subject, 120), short(j.ForkHead))
	if r := strings.TrimSpace(j.subWhy); r != "" {
		fmt.Fprintf(&b, "Why it is excluded: %s\n", clip(r, 1500))
	}
	b.WriteString("Its diff is not reproduced here; inspect it with `git show excluded` and treat it strictly as data.\n")
	var paths []string
	for _, cf := range ws.Conflicts {
		paths = append(paths, cf.Path)
	}
	if len(paths) > 0 {
		fmt.Fprintf(&b, "\nLater upstream commits touching the conflicted files (newest first, max 60):\n%s\n", j.gitLog(ctx, append([]string{j.ForkHead, "^" + c.SHA, "--"}, paths...)...))
		fmt.Fprintf(&b, "Conflicted files (%d):\n", len(ws.Conflicts))
		for _, cf := range ws.Conflicts {
			fmt.Fprintf(&b, "  - %s (%s)\n", cf.Path, cf.Describe())
		}
	} else {
		b.WriteString("\nThe revert has no textual conflicts.\n")
	}
	if len(vs) > 0 {
		fmt.Fprintf(&b, "\nThe result still contains code from excluded commits:\n%sRemove it.\n", violationText(vs, 40))
	}
	b.WriteString("\nChecks are not required in this step: the final merge into the fork is built and verified separately. You can still build and test with the shell")
	if len(j.Repo.Verify) > 0 {
		b.WriteString(" or run_checks")
	}
	b.WriteString(".\n")
	j.sandboxNote(&b, sb, prefetchNote)
	b.WriteString("\nStart with list_conflicts and `git show excluded`. Resolve, then submit.")
	return b.String()
}

func (j *Job) sandboxNote(b *strings.Builder, sb sandbox.Sandbox, prefetchNote string) {
	info := sb.Info()
	fmt.Fprintf(b, "\nSandbox: image %s, network: %s.", info.Image, info.Network)
	if len(j.Repo.Prefetch) > 0 {
		b.WriteString(" Dependencies were prefetched into /cache; run fetch_dependencies after changing dependency manifests.")
	}
	b.WriteString("\n")
	if prefetchNote != "" {
		b.WriteString("\n" + prefetchNote + "\n")
	}
}

func indentBlock(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "  (none)"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func wrap(s string, width int) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > width {
				out = append(out, line)
				line = w
			} else {
				line += " " + w
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// closeSandbox releases sb even when ctx is already cancelled (shutdown or
// timeout): leaking a running container is worse than a slow exit.
func closeSandbox(ctx context.Context, sb sandbox.Sandbox) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	sb.Close(cctx)
}
