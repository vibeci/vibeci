// Package pipeline runs sync cycles. For each fork it fetches both sides,
// reviews new upstream commits, merges the newest safe upstream commit,
// verifies the result, pushes it, records state, and alerts a human only
// when something needs one.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vibeci/vibeci/internal/alert"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/resolve"
	"github.com/vibeci/vibeci/internal/review"
	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/state"
)

// Outcome statuses.
const (
	StatusUpToDate = "up-to-date"
	StatusSynced   = "synced"
	StatusDryRun   = "dry-run"
	StatusBlocked  = "blocked"  // the next upstream commit is blocked; nothing merged
	StatusHeld     = "held"     // upstream rewrote merged history
	StatusBackoff  = "backoff"  // waiting after earlier failures
	StatusRaced    = "raced"    // the fork moved during the sync; retried next cycle
	StatusFailed   = "failed"   // the merge could not be completed
	StatusError    = "error"    // infrastructure problem
	StatusLocked   = "locked"   // another vibeci process is syncing this repo
	StatusDisabled = "disabled" // repo disabled in config
)

// maxCatchUp bounds consecutive merges in one sync when upstream is further
// ahead than review.max_commits_per_run.
const maxCatchUp = 10

// Runner holds the long-lived dependencies of the pipeline.
type Runner struct {
	Cfg     *config.Config
	G       *gitx.Git
	Store   *state.Store
	Sandbox sandbox.Provider
	Alerts  *alert.Dispatcher
	// Models maps configured model names to clients.
	Models map[string]llm.Client
	Logger *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// Options for one sync.
type Options struct {
	// DryRun computes and verifies the merge but never pushes.
	DryRun bool
	// Force ignores failure backoff and cached dry-run results.
	Force bool
}

// Outcome describes one sync of one repo.
type Outcome struct {
	Repo     string   `json:"repo"`
	Status   string   `json:"status"`
	Message  string   `json:"message,omitempty"`
	JobID    string   `json:"job_id,omitempty"`
	ForkHead string   `json:"fork_head,omitempty"`
	Upstream string   `json:"upstream,omitempty"`
	Target   string   `json:"target,omitempty"`
	Commit   string   `json:"commit,omitempty"`
	Blocked  []string `json:"blocked,omitempty"`
	// Excluded lists upstream commits kept out of the fork by this sync
	// (their changes were removed).
	Excluded []string `json:"excluded,omitempty"`
	// Restored lists previously excluded commits re-applied by this sync.
	Restored []string `json:"restored,omitempty"`
	Pending  int      `json:"pending,omitempty"`
	Merges   int      `json:"merges,omitempty"`
	// Patch mode: Version is the upstream version followed, Pinned the
	// version the fork pinned before this sync, Patches what happened to
	// the patches.
	Version  string              `json:"version,omitempty"`
	Pinned   string              `json:"pinned,omitempty"`
	Patches  *resolve.PatchStats `json:"patches,omitempty"`
	Usage    llm.Usage           `json:"usage"`
	Duration string              `json:"duration"`
	// StateError is set if the state could not be saved to the fork
	// (state_ref) after the sync: the next run elsewhere starts from the
	// state saved before.
	StateError string `json:"state_error,omitempty"`
}

// NeedsAttention reports whether a human should look at the outcome.
func (o *Outcome) NeedsAttention() bool {
	switch o.Status {
	case StatusFailed, StatusError, StatusBlocked, StatusHeld:
		return true
	}
	return len(o.Blocked) > 0
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// Cycle syncs every enabled repo (or only the named one), at most
// MaxParallel at a time.
func (r *Runner) Cycle(ctx context.Context, only string, opts Options) []*Outcome {
	var repos []*config.Repo
	for _, repo := range r.Cfg.Repos {
		if only == "" || repo.Name == only {
			repos = append(repos, repo)
		}
	}
	out := make([]*Outcome, len(repos))
	sem := make(chan struct{}, max(1, r.Cfg.MaxParallel))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = r.Sync(ctx, repo, opts)
		}()
	}
	wg.Wait()
	r.WriteHeartbeat(out)
	return out
}

// Heartbeat is written after every cycle (for health checks).
type Heartbeat struct {
	Time     time.Time         `json:"time"`
	Interval string            `json:"interval"`
	Repos    map[string]string `json:"repos"`
}

// WriteHeartbeat records that the daemon is alive (and the last statuses).
func (r *Runner) WriteHeartbeat(outs []*Outcome) {
	hb := Heartbeat{Time: r.now(), Interval: r.Cfg.Interval.String(), Repos: map[string]string{}}
	for _, o := range outs {
		hb.Repos[o.Repo] = o.Status
	}
	if err := r.Store.WriteJSON("heartbeat.json", hb); err != nil {
		r.logger().Warn("writing heartbeat", "err", err)
	}
}

// syncRun is the state of one Sync call.
type syncRun struct {
	*Runner
	parent  context.Context
	repo    *config.Repo
	st      *state.RepoState
	opts    Options
	out     *Outcome
	log     *slog.Logger
	started time.Time

	base, tip string   // failure key of the current attempt
	pushed    []string // messages of the pushes made so far
	// historyRead is set once the fork's history was checked for
	// exclusions the state lacks.
	historyRead bool
}

// Sync brings one fork up to date. It never returns an error: problems are
// reported in the outcome, recorded in state and alerted.
func (r *Runner) Sync(ctx context.Context, repo *config.Repo, opts Options) *Outcome {
	start := r.now()
	out := &Outcome{Repo: repo.Name}
	defer func() { out.Duration = r.now().Sub(start).Round(time.Second).String() }()
	if !repo.IsEnabled() {
		out.Status = StatusDisabled
		return out
	}
	unlock, err := r.Store.Lock(repo.Name)
	if err != nil {
		out.Status, out.Message = StatusLocked, err.Error()
		return out
	}
	defer unlock()
	rs, err := r.pullState(ctx, repo)
	if err != nil {
		out.Status, out.Message = StatusError, err.Error()
		return out
	}
	st, err := r.Store.Load(repo.Name)
	if err != nil {
		out.Status, out.Message = StatusError, "loading state: "+err.Error()
		return out
	}
	s := &syncRun{Runner: r, parent: ctx, repo: repo, st: st, opts: opts, out: out, log: r.logger().With("repo", repo.Name), started: start}
	rctx, cancel := context.WithTimeout(ctx, r.Cfg.RepoTimeout.Duration)
	defer cancel()

	func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic during sync", "panic", p, "stack", string(debug.Stack()))
				s.fail(rctx, fmt.Errorf("internal error: %v", p))
			}
		}()
		for round := 0; round < maxCatchUp; round++ {
			more, err := s.once(rctx)
			if err != nil {
				s.fail(rctx, err)
				return
			}
			if !more || rctx.Err() != nil {
				return
			}
			s.log.Info("upstream is far ahead; merging the next batch", "merged", out.Merges, "pending", out.Pending)
		}
	}()

	if n := len(s.pushed); n > 1 || (n == 1 && out.Message != s.pushed[0]) {
		msg := strings.Join(s.pushed, "; then ")
		if out.Status != StatusSynced && out.Message != "" && out.Message != s.pushed[n-1] {
			msg += "; then: " + out.Message
		}
		out.Message = msg
	}
	st.LastAttempt = start
	st.LastResult = out.Status
	st.LastMessage = clip(out.Message, 2000)
	if err := r.Store.Save(st); err != nil {
		s.log.Error("saving state", "err", err)
	}
	if err := rs.pushState(ctx); err != nil {
		s.log.Error("the state could not be saved to the fork; a run on another host starts from the state saved before", "err", err)
		out.StateError = err.Error()
	}
	lvl := slog.LevelInfo
	if out.NeedsAttention() {
		lvl = slog.LevelWarn
	}
	s.log.Log(ctx, lvl, "sync finished", "status", out.Status, "message", clip(out.Message, 300), "commit", short(out.Commit),
		"input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens)
	return out
}

func (s *syncRun) addUsage(u llm.Usage) {
	s.out.Usage.Add(u)
	s.st.Usage.InputTokens += int64(u.InputTokens)
	s.st.Usage.OutputTokens += int64(u.OutputTokens)
	s.st.Usage.CacheReadTokens += int64(u.CacheReadTokens)
	s.st.Usage.CacheWriteTokens += int64(u.CacheWriteTokens)
	s.st.Usage.Calls += int64(u.Calls)
}

func (s *syncRun) alert(ctx context.Context, ev alert.Event) {
	if s.Alerts == nil {
		return
	}
	ev.Repo = s.repo.Name
	if ev.JobID == "" {
		ev.JobID = s.out.JobID
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	s.Alerts.Send(actx, ev)
}

// snapshot is the fetched state of both sides.
type snapshot struct {
	mirror   *gitx.Repo
	forkAuth *gitx.Auth
	forkHead string
	syncTip  string // branch mode: current proposal branch tip
	base     string // what the merge builds on (fork head or proposal)
	tip      string // upstream commit being tracked
	tipLabel string
	tipTag   string
}

// fetch updates the mirror from both remotes.
func (s *syncRun) fetch(ctx context.Context) (*snapshot, error) {
	repo := s.repo
	snap, err := s.fetchFork(ctx)
	if err != nil {
		return nil, err
	}
	mirror := snap.mirror
	upAuth, err := s.auth("upstream", repo.Upstream.Auth)
	if err != nil {
		return nil, err
	}
	upSpecs := []string{"+refs/tags/*:refs/vibeci/upstream-tags/*"}
	if repo.Upstream.Tags == "" {
		upSpecs = append([]string{"+refs/heads/" + repo.Upstream.Branch + ":refs/vibeci/upstream/" + repo.Upstream.Branch}, upSpecs...)
	}
	if err := mirror.Fetch(ctx, repo.Upstream.URL, upAuth, upSpecs...); err != nil {
		return nil, err
	}
	if snap.tip, snap.tipLabel, snap.tipTag, err = s.upstreamTip(ctx, mirror); err != nil {
		return nil, err
	}
	return snap, nil
}

// fetchFork updates the mirror from the fork and sets the snapshot's fork
// side (fork head, proposal branch tip, base).
func (s *syncRun) fetchFork(ctx context.Context) (*snapshot, error) {
	repo := s.repo
	mirror, err := s.G.InitBare(ctx, filepath.Join(s.Cfg.DataDir, "mirrors", repo.Name+".git"))
	if err != nil {
		return nil, err
	}
	forkAuth, err := s.auth("fork", repo.Fork.Auth)
	if err != nil {
		return nil, err
	}
	snap := &snapshot{mirror: mirror, forkAuth: forkAuth}

	// Fetch both sides into a private ref namespace of the trusted mirror.
	forkRef := "refs/vibeci/fork/" + repo.Fork.Branch
	forkSpecs := []string{"+refs/heads/" + repo.Fork.Branch + ":" + forkRef}
	syncRef := ""
	if repo.Push.Mode == "branch" {
		syncRef = "refs/vibeci/sync/" + repo.Push.Branch
		if snap.syncTip, err = mirror.LsRemote(ctx, repo.Fork.URL, forkAuth, "refs/heads/"+repo.Push.Branch); err != nil {
			return nil, err
		}
		if snap.syncTip != "" {
			forkSpecs = append(forkSpecs, "+refs/heads/"+repo.Push.Branch+":"+syncRef)
		} else if err := mirror.DeleteRef(ctx, syncRef); err != nil {
			return nil, err
		}
	}
	if err := mirror.Fetch(ctx, repo.Fork.URL, forkAuth, forkSpecs...); err != nil {
		return nil, err
	}
	if snap.forkHead, err = mirror.Resolve(ctx, forkRef); err != nil {
		return nil, err
	}
	if snap.syncTip != "" {
		if snap.syncTip, err = mirror.Resolve(ctx, syncRef); err != nil {
			return nil, err
		}
	}
	// In branch mode, build on VibeCI's pending proposal when it still
	// contains the fork head: pushes stay fast-forward and human fixups on
	// the proposal branch are preserved.
	snap.base = snap.forkHead
	if snap.syncTip != "" {
		if ok, err := mirror.IsAncestor(ctx, snap.forkHead, snap.syncTip); err != nil {
			return nil, err
		} else if ok {
			snap.base = snap.syncTip
		}
	}
	return snap, nil
}

// once performs one fetch-review-merge-push round. more reports that
// further work can be done right away (upstream is further ahead, or a
// removal/restore was pushed and the regular merge comes next).
func (s *syncRun) once(ctx context.Context) (more bool, err error) {
	if s.repo.PatchMode() {
		return s.oncePatches(ctx)
	}
	repo, st, out := s.repo, s.st, s.out
	s.base, s.tip = "", ""
	snap, err := s.fetch(ctx)
	if err != nil {
		return false, err
	}
	mirror, forkHead := snap.mirror, snap.forkHead
	base, tip, tipLabel := snap.base, snap.tip, snap.tipLabel
	out.ForkHead, out.Upstream = forkHead, tip
	st.LastForkHead, st.LastUpstreamTip = forkHead, tip
	s.base, s.tip = base, tip

	if held, err := s.checkRewrite(ctx, mirror, forkHead, tip); err != nil || held {
		return false, err
	}
	if err := s.restoreExclusions(ctx, mirror, base); err != nil {
		return false, err
	}
	s.pruneState(ctx, mirror, base, tip)

	// Operator-requested removals and restores come first.
	act, err := s.pendingAction(ctx, snap)
	if err != nil {
		return false, err
	}
	if act != nil {
		s.tip = act.key
		if s.backingOff(base, act.key) || s.cachedDryRun(base, act.key) {
			return false, nil
		}
		jobID, jobDir := s.newJob()
		return s.perform(ctx, snap, act, jobID, jobDir)
	}

	if ok, err := mirror.IsAncestor(ctx, tip, base); err != nil {
		return false, err
	} else if ok {
		out.Pending = 0
		if out.Merges == 0 {
			out.Status = StatusUpToDate
			out.Message = "fork already contains upstream " + tipLabel
			if base != forkHead {
				out.Message += " (proposal pending on " + repo.Push.Branch + ")"
			}
		}
		s.recovered(ctx)
		return false, nil
	}
	if mb, err := mirror.MergeBase(ctx, base, tip); err != nil {
		return false, err
	} else if mb == "" {
		return false, errors.New("fork and upstream share no history (wrong upstream URL?)")
	}
	if s.backingOff(base, tip) || s.cachedDryRun(base, tip) {
		return false, nil
	}

	prevJob := st.LastJob
	jobID, jobDir := s.newJob()
	gate, err := s.reviewGate(ctx, mirror, base, tip, tipLabel, jobDir)
	if _, serr := os.Stat(jobDir); serr != nil && (gate == nil || gate.target == "") {
		// Nothing was written for this job (all verdicts cached).
		out.JobID, st.LastJob = "", prevJob
	}
	if err != nil || gate.target == "" {
		return false, err
	}
	return s.perform(ctx, snap, &action{
		mode: resolve.ModeMerge, key: tip, target: gate.target, label: gate.label,
		exclude: gate.exclude, more: gate.more,
	}, jobID, jobDir)
}

func (s *syncRun) newJob() (id, dir string) {
	id = newJobID(s.repo.Name, s.now())
	s.out.JobID = id
	s.st.LastJob = id
	return id, filepath.Join(s.Cfg.DataDir, "jobs", id)
}

func (s *syncRun) dry() bool { return s.opts.DryRun || s.repo.Push.DryRun }

// backingOff reports (and sets the outcome) when an earlier failure of the
// same attempt, or a streak of failures, asks to wait.
func (s *syncRun) backingOff(base, key string) bool {
	f := s.st.Failure
	if f == nil || s.opts.Force || !s.now().Before(f.NextRetry) {
		return false
	}
	if (f.ForkHead == base && f.Target == key) || f.Streak >= 3 {
		s.out.Status = StatusBackoff
		s.out.Message = fmt.Sprintf("waiting after %d failed attempt(s); next retry %s. Last error: %s", f.Count, f.NextRetry.Format(time.RFC3339), clip(f.Error, 500))
		return true
	}
	return false
}

// cachedDryRun reports (and sets the outcome) when an unchanged dry run
// was already computed.
func (s *syncRun) cachedDryRun(base, key string) bool {
	p := s.st.Proposal
	if !s.dry() || s.opts.Force || p == nil || p.Base != base || p.Upstream != key {
		return false
	}
	s.out.Status, s.out.Commit, s.out.Target, s.out.JobID = StatusDryRun, p.Commit, p.Target, p.JobID
	s.out.Message = "unchanged since the dry run at " + p.Time.Format(time.RFC3339) + "; would push " + short(p.Commit)
	return true
}

// action is one merge VibeCI makes.
type action struct {
	mode    string // resolve.ModeMerge, ModeRemove or ModeRestore
	key     string // identifies the attempt (backoff, dry-run cache)
	target  string
	label   string
	reason  string
	exclude []resolve.Exclusion
	more    bool // further upstream commits can be merged right after
}

// perform runs the merge job for act, verifies and pushes it, and records
// the result.
func (s *syncRun) perform(ctx context.Context, snap *snapshot, act *action, jobID, jobDir string) (bool, error) {
	repo, st, out := s.repo, s.st, s.out
	mirror, base := snap.mirror, snap.base
	out.Target = act.target
	roles := s.Cfg.RolesFor(repo)
	var models []llm.Client
	for _, name := range roles.Resolve {
		if m := s.Models[name]; m != nil {
			models = append(models, m)
		}
	}
	job := &resolve.Job{
		ID: jobID, Repo: repo, DataDir: s.Cfg.DataDir, JobDir: jobDir,
		G: s.G, Mirror: mirror, Sandbox: s.Sandbox,
		ForkHead: base, Target: act.target, TargetLabel: act.label,
		Models: models, Auditor: s.Models[roles.Audit],
		BlockOn: repo.Review.BlockOn, MinConfidence: repo.Review.MinConfidence,
		Mode: act.mode, Reason: act.reason, Exclude: act.exclude,
		Quarantine: s.quarantine(base),
		Logger:     s.logger().With("job", jobID), // the job adds repo= itself
	}
	res, err := job.Run(ctx)
	s.addUsage(job.Usage())
	if err != nil {
		return false, &jobError{err: err, label: act.label}
	}
	ancestors := []string{base}
	if act.mode == resolve.ModeMerge {
		ancestors = append(ancestors, act.target)
	}
	for _, anc := range ancestors {
		if ok, err := mirror.IsAncestor(ctx, anc, res.Commit); err != nil || !ok {
			return false, fmt.Errorf("internal error: result %s does not contain %s", short(res.Commit), short(anc))
		}
	}
	out.Commit = res.Commit
	what := describeResult(res, act, repo)

	if s.dry() {
		st.Proposal = &state.Proposal{Base: base, Upstream: act.key, Target: act.target, Commit: res.Commit, JobID: jobID, Time: s.now()}
		if err := mirror.UpdateRef(ctx, "refs/vibeci/proposed/"+repo.Fork.Branch, res.Commit); err != nil {
			return false, err
		}
		out.Status = StatusDryRun
		out.Message = "dry run: would push " + what
		out.Excluded = append(out.Excluded, res.Removed...)
		s.cleanupJob(jobDir)
		return false, nil
	}

	pushed, err := s.push(ctx, mirror, snap.forkAuth, snap.forkHead, snap.syncTip, res.Commit)
	if err != nil {
		return false, pushHint(err)
	}
	if !pushed {
		out.Status = StatusRaced
		out.Message = "the fork changed while the merge was being prepared; it will be redone next cycle"
		return false, nil
	}
	now := s.now()
	st.LastPushed, st.LastSuccess, st.Proposal = res.Commit, now, nil
	if repo.Push.Mode != "branch" {
		st.LastForkHead = res.Commit
	}
	s.recordExclusions(res, now)
	out.Status = StatusSynced
	out.Merges++
	dest := repo.Fork.Branch
	if repo.Push.Mode == "branch" {
		dest = repo.Push.Branch
	}
	out.Message = fmt.Sprintf("pushed %s to %s: %s", short(res.Commit), dest, what)
	title := fmt.Sprintf("%s: merged upstream %s", repo.Name, act.label)
	switch act.mode {
	case resolve.ModeMerge:
		st.LastMerged, st.LastMergedTag = act.target, ""
		if act.target == snap.tip {
			st.LastMergedTag = snap.tipTag
		}
		out.Pending = 0
		if pending, err := mirror.RevList(ctx, "--count", snap.tip, "^"+res.Commit); err == nil && len(pending) == 1 {
			fmt.Sscan(pending[0], &out.Pending)
		}
		if out.Pending > 0 {
			out.Message += fmt.Sprintf("; %d upstream commit(s) still pending", out.Pending)
		}
	case resolve.ModeRemove:
		title = fmt.Sprintf("%s: removed upstream commit %s", repo.Name, short(act.target))
	case resolve.ModeRestore:
		title = fmt.Sprintf("%s: restored upstream commit %s", repo.Name, short(act.target))
		out.Restored = append(out.Restored, act.target)
	}
	s.pushed = append(s.pushed, out.Message)
	s.recovered(ctx)
	s.alert(ctx, alert.Event{
		Kind: "synced", Severity: "info",
		Title:   title,
		Message: out.Message,
		Commits: []string{res.Commit},
		Details: map[string]any{"target": act.target, "mode": modeName(act.mode), "excluded": res.Removed, "conflicts": res.Conflicts,
			"agent_model": res.AgentModel, "kept_ours": res.KeptOurs, "checks": res.Verify.Summary()},
	})
	s.cleanupJob(jobDir)
	s.maintain(ctx, mirror)
	return act.more || act.mode != resolve.ModeMerge, nil
}

func modeName(m string) string {
	if m == resolve.ModeMerge {
		return "merge"
	}
	return m
}

func describeResult(res *resolve.Result, act *action, repo *config.Repo) string {
	var parts []string
	label := act.label
	switch {
	case act.mode == resolve.ModeRemove:
		parts = append(parts, "removal of upstream commit "+short(act.target))
	case act.mode == resolve.ModeRestore:
		parts = append(parts, "restore of upstream commit "+short(act.target))
	case res.FastForward:
		parts = append(parts, "fast-forward to upstream "+label)
	default:
		parts = append(parts, "merge of upstream "+label)
	}
	switch {
	case res.AgentModel != "" && len(res.Conflicts) > 0:
		parts = append(parts, fmt.Sprintf("%d conflicted file(s) resolved by %s in %d turns", len(res.Conflicts), res.AgentModel, res.AgentTurns))
	case res.AgentModel != "":
		parts = append(parts, fmt.Sprintf("adapted by %s in %d turns", res.AgentModel, res.AgentTurns))
	case !res.FastForward:
		parts[0] = "clean " + parts[0]
	}
	if act.mode == resolve.ModeMerge && len(res.Removed) > 0 {
		var shas []string
		for _, sha := range res.Removed {
			shas = append(shas, short(sha))
		}
		note := fmt.Sprintf("excluded %d upstream commit(s): %s", len(res.Removed), strings.Join(shas, ", "))
		if res.ExclusionAgents > 0 {
			note += fmt.Sprintf(" (%d removal(s) needed the agent)", res.ExclusionAgents)
		}
		parts = append(parts, note)
	}
	if len(res.KeptOurs) > 0 {
		parts = append(parts, fmt.Sprintf("kept the fork's version of %d protected path(s)", len(res.KeptOurs)))
	}
	if res.Verify != nil && len(res.Verify.Results) > 0 {
		parts = append(parts, "checks passed ("+res.Verify.Summary()+")")
	} else if len(repo.Verify) == 0 {
		parts = append(parts, "no checks configured")
	}
	return strings.Join(parts, "; ")
}

// upstreamTip returns the upstream commit to track, a label for humans and
// the tag name in tag mode.
func (s *syncRun) upstreamTip(ctx context.Context, mirror *gitx.Repo) (sha, label, tag string, err error) {
	up := s.repo.Upstream
	if up.Tags != "" {
		name, sha, err := mirror.LatestTag(ctx, "refs/vibeci/upstream-tags/", up.Tags)
		if err != nil {
			return "", "", "", fmt.Errorf("upstream: %w", err)
		}
		return sha, name, name, nil
	}
	sha, err = mirror.Resolve(ctx, "refs/vibeci/upstream/"+up.Branch)
	if err != nil {
		return "", "", "", err
	}
	return sha, up.Branch, "", nil
}

// checkRewrite detects upstream rewriting history the fork already merged.
func (s *syncRun) checkRewrite(ctx context.Context, mirror *gitx.Repo, forkHead, tip string) (held bool, err error) {
	st, repo := s.st, s.repo
	lm := st.LastMerged
	if lm == "" {
		st.RewriteHold = nil
		return false, nil
	}
	if obj, _ := mirror.ResolveObject(ctx, lm+"^{commit}"); obj == "" {
		st.LastMerged, st.LastMergedTag, st.RewriteHold = "", "", nil
		return false, nil
	}
	inFork, err := mirror.IsAncestor(ctx, lm, forkHead)
	if err != nil {
		return false, err
	}
	if !inFork {
		// The fork was reset past it by a human; there is nothing to protect.
		st.LastMerged, st.LastMergedTag, st.RewriteHold = "", "", nil
		return false, nil
	}
	var rewritten bool
	var what string
	if repo.Upstream.Tags != "" {
		// Tag mode: tags may legitimately live on diverging release
		// branches; only a moved tag counts as a rewrite.
		if st.LastMergedTag != "" {
			if now, _ := mirror.ResolveObject(ctx, "refs/vibeci/upstream-tags/"+st.LastMergedTag+"^{commit}"); now != "" && now != lm {
				rewritten = true
				what = fmt.Sprintf("upstream tag %s was moved from %s to %s after the fork merged it", st.LastMergedTag, short(lm), short(now))
			}
		}
	} else {
		inUp, err := mirror.IsAncestor(ctx, lm, tip)
		if err != nil {
			return false, err
		}
		if !inUp {
			rewritten = true
			what = fmt.Sprintf("upstream %s no longer contains %s, which the fork already merged (force-push or history rewrite)", repo.Upstream.Branch, short(lm))
		}
	}
	if !rewritten {
		s.rewriteResolved()
		return false, nil
	}
	return s.holdRewrite(ctx, lm, tip, what), nil
}

// rewriteResolved releases a rewrite hold once upstream is consistent.
func (s *syncRun) rewriteResolved() {
	if s.st.RewriteHold != nil {
		s.log.Info("upstream history is consistent again; releasing hold")
		s.st.RewriteHold = nil
	}
}

// holdRewrite reacts to upstream rewriting what the fork already contains
// (described by what): it alerts and puts the repo on hold, or continues
// if on_upstream_rewrite says so.
func (s *syncRun) holdRewrite(ctx context.Context, oldTip, newTip, what string) (held bool) {
	st, repo := s.st, s.repo
	if repo.OnUpstreamRewrite == "continue" {
		s.log.Warn("upstream rewrote merged history; continuing as configured", "detail", what)
		expect := "expect duplicate commits or conflicts"
		if repo.PatchMode() {
			expect = "the fork's patches are applied to the tag's new commit from now on"
		}
		s.alert(ctx, alert.Event{Kind: "rewritten", Severity: "warning", Title: repo.Name + ": upstream history was rewritten", Message: what + ". Continuing (on_upstream_rewrite: continue); " + expect + "."})
		st.LastMerged, st.LastMergedTag = "", ""
		return false
	}
	if st.RewriteHold == nil {
		st.RewriteHold = &state.RewriteHold{OldTip: oldTip, Since: s.now()}
	}
	st.RewriteHold.NewTip = newTip
	if !st.RewriteHold.Alerted {
		st.RewriteHold.Alerted = true
		s.alert(ctx, alert.Event{Kind: "rewritten", Severity: "critical", Title: repo.Name + ": upstream history was rewritten; syncing is on hold",
			Message: what + ".\nVibeCI will not merge until a human looks at it. Once handled, run `vibeci release -repo " + repo.Name + "` (or set on_upstream_rewrite to continue).",
			Commits: []string{oldTip, newTip}})
	}
	s.out.Status = StatusHeld
	s.out.Message = what + "; on hold since " + st.RewriteHold.Since.Format(time.RFC3339)
	return true
}

// pruneState forgets blocked commits that are no longer pending (merged
// after being allowed, or removed from upstream) and pending exclusions of
// commits that exist on neither side any more.
func (s *syncRun) pruneState(ctx context.Context, mirror *gitx.Repo, base, tip string) {
	for sha := range s.st.Blocked {
		inUp, err1 := mirror.IsAncestor(ctx, sha, tip)
		inBase, err2 := mirror.IsAncestor(ctx, sha, base)
		if (err1 == nil && !inUp) || (err2 == nil && inBase) {
			delete(s.st.Blocked, sha)
		}
	}
	for sha, e := range s.st.Excluded {
		if e.Removed {
			continue
		}
		if obj, _ := mirror.ResolveObject(ctx, sha+"^{commit}"); obj == "" {
			delete(s.st.Excluded, sha)
			continue
		}
		inUp, err1 := mirror.IsAncestor(ctx, sha, tip)
		inBase, err2 := mirror.IsAncestor(ctx, sha, base)
		if err1 == nil && err2 == nil && !inUp && !inBase {
			s.log.Info("excluded commit is no longer in the fork or upstream; forgetting it", "commit", short(sha))
			delete(s.st.Excluded, sha)
		}
	}
	cutoff := s.now().Add(-90 * 24 * time.Hour)
	for sha, t := range s.st.Warned {
		if t.Before(cutoff) {
			delete(s.st.Warned, sha)
		}
	}
}

// gateResult is what the review gate allows to be merged.
type gateResult struct {
	target, label string
	more          bool
	exclude       []resolve.Exclusion
}

// reviewGate reviews the upstream commits between base and tip and returns
// the newest target that can be merged (empty if the very next step is
// blocked) with the commits to exclude from it.
func (s *syncRun) reviewGate(ctx context.Context, mirror *gitx.Repo, base, tip, tipLabel, jobDir string) (*gateResult, error) {
	repo := s.repo
	if !repo.Review.IsEnabled() && len(s.st.Excluded) == 0 {
		return &gateResult{target: tip, label: tipLabel}, nil
	}
	plan, err := review.BuildPlan(ctx, mirror, base, tip, repo.Review.MaxCommitsPerRun)
	if err != nil {
		return nil, err
	}
	if len(plan.Steps) == 0 {
		return nil, errors.New("internal error: empty merge plan")
	}
	verdicts := map[string]*review.Verdict{}
	if repo.Review.IsEnabled() {
		rv := s.reviewer(mirror, jobDir)
		verdicts, err = rv.ReviewCommits(ctx, plan.Commits())
		s.addUsage(rv.Usage)
		if err != nil {
			return nil, fmt.Errorf("review: %w", err)
		}
	}
	d, err := s.decide(ctx, mirror, plan, verdicts)
	if err != nil {
		return nil, err
	}
	s.noteVerdicts(ctx, plan, verdicts, d)
	if d.target == "" {
		s.out.Status = StatusBlocked
		s.out.Pending = plan.Total
		s.out.Message = fmt.Sprintf("the next upstream commit %s is blocked (%s); nothing was merged", short(d.firstBlocked), clip(s.blockSummary(verdicts, d.firstBlocked), 300))
		return &gateResult{}, nil
	}
	label := tipLabel
	if d.target != tip {
		if repo.Upstream.Tags != "" {
			label = short(d.target) + " (towards " + tipLabel + ")"
		} else {
			label = tipLabel + "@" + short(d.target)
		}
	}
	return &gateResult{target: d.target, label: label, more: plan.Limited && d.firstBlocked == "", exclude: d.exclude}, nil
}

// decision is the review gate's verdict on a plan.
type decision struct {
	target       string // newest step that can be merged ("" = none)
	firstBlocked string // first commit that stops merging ("" = none)
	exclude      []resolve.Exclusion
	excluded     map[string]bool
}

// reviewBlocks reports whether the review verdict for sha blocks it (fail
// closed: a missing verdict blocks).
func (s *syncRun) reviewBlocks(verdicts map[string]*review.Verdict, sha string) bool {
	if !s.repo.Review.IsEnabled() {
		return false
	}
	v := verdicts[sha]
	return v == nil || v.Blocks(s.repo.Review.BlockOn, s.repo.Review.MinConfidence)
}

// excludedByState reports whether sha is recorded as excluded (by an
// operator, or by an earlier review whose merge was later undone).
func (s *syncRun) excludedByState(sha string) bool {
	e := s.st.Excluded[sha]
	return e != nil && !e.Restore
}

// decide walks the plan: blocked commits are excluded (on_blocked:
// exclude, or recorded exclusions) or stop the merge right before them
// (on_blocked: hold, or commits that cannot be excluded).
func (s *syncRun) decide(ctx context.Context, mirror *gitx.Repo, plan *review.Plan, verdicts map[string]*review.Verdict) (*decision, error) {
	d := &decision{excluded: map[string]bool{}}
	var cand []string
	for _, sha := range plan.Commits() {
		if s.excludedByState(sha) || s.reviewBlocks(verdicts, sha) {
			cand = append(cand, sha)
		}
	}
	parents := map[string]int{}
	if len(cand) > 0 {
		cs, err := mirror.ReadCommits(ctx, cand)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			parents[c.SHA] = len(c.Parents)
		}
	}
	excludeMode := s.repo.Review.OnBlocked != "hold"
	for _, step := range plan.Steps {
		var stepEx []resolve.Exclusion
		stop := ""
		for _, c := range step.Introduces {
			byState, byReview := s.excludedByState(c), s.reviewBlocks(verdicts, c)
			if !byState && !byReview {
				continue
			}
			if (byState || excludeMode) && parents[c] > 0 {
				stepEx = append(stepEx, resolve.Exclusion{Commit: c, Reason: s.exclusionReason(c, verdicts[c])})
				continue
			}
			stop = c
			break
		}
		if stop != "" {
			d.firstBlocked = stop
			break
		}
		d.exclude = append(d.exclude, stepEx...)
		d.target = step.Commit
	}
	for _, x := range d.exclude {
		d.excluded[x.Commit] = true
	}
	return d, nil
}

func (s *syncRun) exclusionReason(sha string, v *review.Verdict) string {
	if e := s.st.Excluded[sha]; e != nil {
		if e.Source == "manual" {
			if e.Summary != "" {
				return "excluded by an operator: " + e.Summary
			}
			return "excluded by an operator"
		}
		if e.Verdict != "" {
			return fmt.Sprintf("blocked by the security review (%s, confidence %.2f): %s", e.Verdict, e.Confidence, e.Summary)
		}
	}
	if v != nil {
		if v.Stage == review.StageInconclusive {
			return "the security review could not reach a verdict, so the commit is kept out: " + v.Summary
		}
		return fmt.Sprintf("blocked by the security review (%s, confidence %.2f): %s", v.Verdict, v.Confidence, v.Summary)
	}
	return "blocked by the security review (no verdict)"
}

func (s *syncRun) blockSummary(verdicts map[string]*review.Verdict, sha string) string {
	if v := verdicts[sha]; v != nil && s.reviewBlocks(verdicts, sha) {
		return verdictSummary(v)
	}
	if s.excludedByState(sha) {
		return "excluded, but it cannot be removed automatically because it has no parent commit"
	}
	return "no verdict"
}

func (s *syncRun) reviewer(mirror *gitx.Repo, jobDir string) *review.Reviewer {
	repo := s.repo
	roles := s.Cfg.RolesFor(repo)
	return &review.Reviewer{
		Repo: mirror, Triage: s.Models[roles.Triage], Investigate: s.Models[roles.Investigate], Store: s.Store,
		Opts: review.Options{
			RepoName: repo.Name, UpstreamURL: repo.Upstream.URL,
			BatchChars: repo.Review.BatchChars, MaxInvestTurn: repo.Review.InvestigateTurns,
			AllowCommits: s.allowList(), TranscriptDir: filepath.Join(jobDir, "transcripts"),
		},
		Logger: s.logger(), // the reviewer adds repo= itself
	}
}

// ReviewReport lists verdicts for the upstream commits not yet merged.
type ReviewReport struct {
	Repo     string            `json:"repo"`
	Base     string            `json:"base"`
	Upstream string            `json:"upstream"`
	Label    string            `json:"label"`
	Total    int               `json:"total"`
	Limited  bool              `json:"limited"`
	Target   string            `json:"safe_target"`
	Blocked  string            `json:"first_blocked,omitempty"`
	Excluded []string          `json:"excluded,omitempty"`
	Verdicts []*review.Verdict `json:"verdicts"`
	Usage    llm.Usage         `json:"usage"`
}

// Review fetches and reviews pending upstream commits without merging,
// pushing or alerting. Verdicts are cached like in a normal sync.
func (r *Runner) Review(ctx context.Context, repo *config.Repo, maxCommits int) (_ *ReviewReport, err error) {
	if repo.PatchMode() {
		return nil, errPatchMode("review", repo)
	}
	unlock, err := r.Store.Lock(repo.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rs, err := r.pullState(ctx, repo)
	if err != nil {
		return nil, err
	}
	st, err := r.Store.Load(repo.Name)
	if err != nil {
		return nil, err
	}
	s := &syncRun{Runner: r, parent: ctx, repo: repo, st: st, out: &Outcome{Repo: repo.Name}, log: r.logger().With("repo", repo.Name), started: r.now()}
	snap, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.restoreExclusions(ctx, snap.mirror, snap.base); err != nil {
		return nil, err
	}
	// The verdicts are cached: save them to the fork too, whatever happens.
	defer func() {
		if perr := rs.pushState(ctx); perr != nil && err == nil {
			err = perr
		}
	}()
	rep := &ReviewReport{Repo: repo.Name, Base: snap.base, Upstream: snap.tip, Label: snap.tipLabel}
	if ok, err := snap.mirror.IsAncestor(ctx, snap.tip, snap.base); err != nil || ok {
		rep.Target = snap.base
		return rep, err
	}
	if maxCommits <= 0 {
		maxCommits = repo.Review.MaxCommitsPerRun
	}
	plan, err := review.BuildPlan(ctx, snap.mirror, snap.base, snap.tip, maxCommits)
	if err != nil {
		return nil, err
	}
	rv := s.reviewer(snap.mirror, filepath.Join(r.Cfg.DataDir, "jobs", newJobID(repo.Name, r.now())))
	verdicts, err := rv.ReviewCommits(ctx, plan.Commits())
	rep.Usage = rv.Usage
	if err != nil {
		return nil, err
	}
	rep.Total, rep.Limited = plan.Total, plan.Limited
	d, err := s.decide(ctx, snap.mirror, plan, verdicts)
	if err != nil {
		return nil, err
	}
	rep.Target, rep.Blocked = d.target, d.firstBlocked
	for _, x := range d.exclude {
		rep.Excluded = append(rep.Excluded, x.Commit)
	}
	for _, sha := range plan.Commits() {
		rep.Verdicts = append(rep.Verdicts, verdicts[sha])
	}
	return rep, nil
}

func verdictSummary(v *review.Verdict) string {
	if v == nil {
		return "no verdict"
	}
	return fmt.Sprintf("%s, confidence %.2f: %s", v.Verdict, v.Confidence, v.Summary)
}

// noteVerdicts records blocked commits and sends one alert per newly
// blocked or suspicious commit.
func (s *syncRun) noteVerdicts(ctx context.Context, plan *review.Plan, verdicts map[string]*review.Verdict, d *decision) {
	st, repo := s.st, s.repo
	now := s.now()
	type blockedCommit struct {
		v        *review.Verdict
		excluded bool
	}
	var newly []blockedCommit
	for _, sha := range plan.Commits() {
		v := verdicts[sha]
		if d.excluded[sha] && !slices.Contains(s.out.Excluded, sha) {
			s.out.Excluded = append(s.out.Excluded, sha)
		}
		if !s.reviewBlocks(verdicts, sha) {
			delete(st.Blocked, sha)
			if v != nil && v.Verdict != review.Clean && !d.excluded[sha] {
				if _, done := st.Warned[sha]; !done {
					st.Warned[sha] = now
					s.alert(ctx, alert.Event{
						Kind: "suspicious", Severity: "warning",
						Title:   fmt.Sprintf("%s: upstream commit %s looks %s (%.0f%%), not blocking", repo.Name, short(sha), v.Verdict, v.Confidence*100),
						Message: verdictText(v, repo) + fmt.Sprintf("\nPolicy: block_on=%s, min_confidence=%.2f, so this commit is merged.", repo.Review.BlockOn, repo.Review.MinConfidence),
						Commits: []string{sha},
						Details: map[string]any{"verdict": v},
					})
				}
			}
			continue
		}
		if !d.excluded[sha] {
			s.out.Blocked = append(s.out.Blocked, sha)
		}
		b := st.Blocked[sha]
		if b == nil {
			b = &state.Blocked{Commit: sha, FirstSeen: now}
			st.Blocked[sha] = b
		}
		if v != nil {
			b.Verdict, b.Confidence, b.Summary = v.Verdict, v.Confidence, clip(v.Summary, 1000)
		}
		if !b.Alerted {
			b.Alerted = true
			newly = append(newly, blockedCommit{v, d.excluded[sha]})
		}
	}
	for i, bc := range newly {
		if i == 5 {
			var rest []*review.Verdict
			for _, x := range newly[5:] {
				rest = append(rest, x.v)
			}
			s.alert(ctx, alert.Event{Kind: "blocked", Severity: "critical", Title: fmt.Sprintf("%s: %d more upstream commits blocked", repo.Name, len(newly)-5),
				Message: "See `vibeci status -repo " + repo.Name + "` for the full list.", Commits: shasOf(rest)})
			break
		}
		v := bc.v
		if v == nil {
			continue
		}
		allow := fmt.Sprintf("vibeci allow -repo %s %s", repo.Name, v.Commit)
		var title, after string
		if bc.excluded {
			title = fmt.Sprintf("%s: upstream commit %s blocked as %s (%.0f%%) and excluded", repo.Name, short(v.Commit), v.Verdict, v.Confidence*100)
			after = "VibeCI keeps merging upstream without it: its changes are removed before merging and kept out of later merges (the commit stays in the fork's history with its effect reverted)." +
				"\nIf this is a false positive, run `" + allow + "`: the next sync re-applies it."
		} else {
			title = fmt.Sprintf("%s: upstream commit %s blocked as %s (%.0f%%)", repo.Name, short(v.Commit), v.Verdict, v.Confidence*100)
			after = "Nothing after it will be merged."
			if d.target != "" {
				after = fmt.Sprintf("VibeCI merges upstream only up to %s, the newest commit before it.", short(d.target))
			}
			if repo.Review.OnBlocked != "hold" {
				after += " It cannot be excluded automatically because it has no parent commit."
			}
			after += "\nIf this is a false positive, run `" + allow + "` (or add it to review.allow_commits)."
		}
		s.alert(ctx, alert.Event{
			Kind: "blocked", Severity: "critical",
			Title:   title,
			Message: verdictText(v, repo) + "\n" + after,
			Commits: []string{v.Commit},
			Details: map[string]any{"verdict": v, "excluded": bc.excluded},
		})
	}
}

func shasOf(vs []*review.Verdict) []string {
	var out []string
	for _, v := range vs {
		if v != nil {
			out = append(out, v.Commit)
		}
	}
	return out
}

func verdictText(v *review.Verdict, repo *config.Repo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", v.Summary)
	for i, f := range v.Findings {
		if i == 6 {
			fmt.Fprintf(&sb, "(+%d more findings)\n", len(v.Findings)-6)
			break
		}
		fmt.Fprintf(&sb, "- %s [%s]: %s\n", f.File, f.Category, clip(f.Evidence, 240))
	}
	fmt.Fprintf(&sb, "Upstream: %s\nCommit: %s (review stage: %s, model: %s)\n", repo.Upstream.URL, v.Commit, v.Stage, v.Model)
	return sb.String()
}

// push publishes commit. It returns false (and no error) if the remote
// branch moved since it was fetched.
func (s *syncRun) push(ctx context.Context, mirror *gitx.Repo, auth *gitx.Auth, forkHead, syncTip, commit string) (bool, error) {
	repo := s.repo
	if repo.Push.Mode == "branch" {
		ref := "refs/heads/" + repo.Push.Branch
		cur, err := mirror.LsRemote(ctx, repo.Fork.URL, auth, ref)
		if err != nil {
			return false, err
		}
		if cur != syncTip {
			return false, nil
		}
		switch {
		case cur == commit:
		case cur == "":
			err = mirror.PushLease(ctx, repo.Fork.URL, auth, commit, ref, "")
		default:
			ff, aerr := mirror.IsAncestor(ctx, cur, commit)
			if aerr != nil {
				return false, aerr
			}
			if ff {
				err = mirror.Push(ctx, repo.Fork.URL, auth, commit, ref)
				break
			}
			owned, oerr := s.ownedByVibeCI(ctx, mirror, forkHead, cur)
			if oerr != nil {
				return false, oerr
			}
			if !owned {
				return false, fmt.Errorf("branch %s has commits not made by VibeCI and does not contain the fork's current %s; not overwriting it (merge or delete it)", repo.Push.Branch, repo.Fork.Branch)
			}
			err = mirror.PushLease(ctx, repo.Fork.URL, auth, commit, ref, cur)
		}
		if err != nil {
			return false, err
		}
		return true, mirror.UpdateRef(ctx, "refs/vibeci/sync/"+repo.Push.Branch, commit)
	}

	ref := "refs/heads/" + repo.Fork.Branch
	cur, err := mirror.LsRemote(ctx, repo.Fork.URL, auth, ref)
	if err != nil {
		return false, err
	}
	if cur != forkHead {
		return false, nil
	}
	if err := mirror.Push(ctx, repo.Fork.URL, auth, commit, ref); err != nil {
		return false, err
	}
	return true, mirror.UpdateRef(ctx, "refs/vibeci/fork/"+repo.Fork.Branch, commit)
}

// pushHint adds what to do to push errors with a known cause.
func pushHint(err error) error {
	// GitHub: "refusing to allow a GitHub App to create or update workflow
	// `.github/workflows/x.yml` without `workflows` permission" (personal
	// access and OAuth tokens: "without `workflow` scope").
	if strings.Contains(err.Error(), "to create or update workflow") {
		return fmt.Errorf("%w (GitHub refuses commits that change .github/workflows, upstream's included, from a token that may not change workflows; give VibeCI a token that may: README.md, \"Tokens for GitHub\")", err)
	}
	return err
}

// ownedByVibeCI reports whether every first-parent commit on tip that is
// not in the fork was created by VibeCI.
func (s *syncRun) ownedByVibeCI(ctx context.Context, mirror *gitx.Repo, forkHead, tip string) (bool, error) {
	shas, err := mirror.RevList(ctx, "--first-parent", "-n", "1000", tip, "^"+forkHead)
	if err != nil || len(shas) == 0 || len(shas) >= 1000 {
		return false, err
	}
	commits, err := mirror.ReadCommits(ctx, shas)
	if err != nil {
		return false, err
	}
	for _, c := range commits {
		if !strings.HasSuffix(c.Committer, "<"+s.G.Identity.Email+">") || !strings.Contains(c.Body, "\nVibeCI-Job: ") {
			return false, nil
		}
	}
	return true, nil
}

// jobError carries the merge label for alerts.
type jobError struct {
	err   error
	label string
}

func (e *jobError) Error() string { return e.err.Error() }
func (e *jobError) Unwrap() error { return e.err }

// fail classifies err, applies backoff, and alerts.
func (s *syncRun) fail(ctx context.Context, err error) {
	repo, out := s.repo, s.out
	if s.parent.Err() != nil {
		// Shutdown: not the repo's fault.
		out.Status, out.Message = StatusError, "interrupted: "+err.Error()
		return
	}
	label := "upstream"
	var je *jobError
	if errors.As(err, &je) {
		label = "upstream " + je.label
	}
	jobNote := ""
	if out.JobID != "" {
		jobNote = fmt.Sprintf("\nJob %s; transcripts and workspaces are kept in %s for %s.", out.JobID, filepath.Join(s.Cfg.DataDir, "jobs", out.JobID), s.Cfg.KeepJobs.Duration)
	}
	var fe *resolve.FailedError
	kind := "failed"
	var ev alert.Event
	merging, mergeVerb, agentName := "merging", "merge", "merge agent"
	if repo.PatchMode() {
		merging, mergeVerb, agentName = "updating the patches to", "update to", "patch agent"
	}
	switch {
	case errors.Is(err, resolve.ErrAuditRejected):
		ev = alert.Event{Kind: "blocked", Severity: "critical", Title: fmt.Sprintf("%s: %s output rejected by the security audit", repo.Name, agentName),
			Message: fmt.Sprintf("While %s %s, the lines written by the %s were judged malicious: %s\nThis can mean upstream content manipulated the agent (prompt injection). Nothing was pushed.%s", merging, label, agentName, err, jobNote)}
	case errors.As(err, &fe):
		ev = alert.Event{Kind: "failed", Severity: "warning", Title: fmt.Sprintf("%s: could not %s %s", repo.Name, mergeVerb, label),
			Message: err.Error() + jobNote}
	case errors.Is(err, context.DeadlineExceeded):
		ev = alert.Event{Kind: "failed", Severity: "warning", Title: fmt.Sprintf("%s: sync timed out", repo.Name),
			Message: fmt.Sprintf("%s %s did not finish within repo_timeout (%s): %v%s", strings.ToUpper(merging[:1])+merging[1:], label, s.Cfg.RepoTimeout.Duration, err, jobNote)}
	default:
		kind = "error"
		ev = alert.Event{Kind: "error", Severity: "warning", Title: fmt.Sprintf("%s: sync error", repo.Name), Message: err.Error() + jobNote}
	}

	now := s.now()
	f := s.st.Failure
	if f != nil && f.Kind == kind && f.ForkHead == s.base && f.Target == s.tip {
		f.Count++
		f.Streak++
	} else {
		streak := 1
		if f != nil {
			streak = f.Streak + 1
		}
		f = &state.Failure{Kind: kind, ForkHead: s.base, Target: s.tip, Count: 1, Streak: streak, FirstAt: now}
		s.st.Failure = f
	}
	f.LastAt = now
	f.Error = clip(err.Error(), 4000)
	// Relative to the start of the sync, so a one-interval backoff retries
	// on the next cycle.
	f.NextRetry = s.started.Add(backoff(kind, f.Count, s.Cfg.Interval.Duration) - time.Minute)
	out.Status = kind
	if kind == "failed" {
		out.Status = StatusFailed
	}
	out.Message = err.Error()
	threshold := 1
	if kind == "error" {
		threshold = 3 // transient network or API blips are not worth a page
	}
	if !f.Alerted && f.Count >= threshold {
		f.Alerted = true
		ev.Message += fmt.Sprintf("\nNext retry after %s.", f.NextRetry.Format(time.RFC3339))
		s.alert(ctx, ev)
	}
}

func backoff(kind string, count int, interval time.Duration) time.Duration {
	limit := 24 * time.Hour
	if kind == "error" {
		limit = 4 * time.Hour
	}
	d := interval
	for i := 1; i < count && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// recovered clears failure state after a good sync.
func (s *syncRun) recovered(ctx context.Context) {
	f := s.st.Failure
	if f == nil {
		return
	}
	s.st.Failure = nil
	if f.Alerted {
		s.alert(ctx, alert.Event{Kind: "recovered", Severity: "info", Title: s.repo.Name + ": syncing works again",
			Message: fmt.Sprintf("After %d failed attempt(s) since %s. Last error was: %s", f.Streak, f.FirstAt.Format(time.RFC3339), clip(f.Error, 500))})
	}
}

func (s *syncRun) auth(which string, a *config.GitAuth) (*gitx.Auth, error) {
	return ResolveAuth(s.Cfg.DataDir, s.repo.Name, which, a)
}

// ResolveAuth resolves the credentials of one remote ("fork" or "upstream").
func ResolveAuth(dataDir, repoName, which string, a *config.GitAuth) (*gitx.Auth, error) {
	if a == nil {
		return nil, nil
	}
	out := &gitx.Auth{Username: a.Username}
	if a.Token.IsSet() {
		tok, err := a.Token.Resolve()
		if err != nil {
			return nil, fmt.Errorf("%s auth token: %w", which, err)
		}
		out.Token = tok
	}
	if a.SSHKey.IsSet() {
		key, err := a.SSHKey.Resolve()
		if err != nil {
			return nil, fmt.Errorf("%s ssh key: %w", which, err)
		}
		// Keys go to the temp dir (tmpfs in the container), never the data volume.
		p, err := gitx.WriteKeyFile(filepath.Join(os.TempDir(), "vibeci-keys"), repoName+"-"+which, key)
		if err != nil {
			return nil, err
		}
		out.SSHKeyFile = p
		out.KnownHosts = a.KnownHosts
		if out.KnownHosts == "" {
			out.KnownHosts = filepath.Join(dataDir, "known_hosts")
		}
	}
	return out, nil
}

// cleanupJob removes a successful job's workspaces and caches but keeps its
// transcripts until the job directory expires.
func (s *syncRun) cleanupJob(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Name() != "transcripts" {
			if err := RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				s.log.Warn("cleaning job dir", "err", err)
			}
		}
	}
}

// maintain repacks the mirror weekly.
func (s *syncRun) maintain(ctx context.Context, mirror *gitx.Repo) {
	if s.now().Sub(s.st.LastGC) < 7*24*time.Hour {
		return
	}
	s.st.LastGC = s.now()
	if _, err := s.G.Run(ctx, gitx.Opts{GitDir: mirror.GitDir}, "gc", "--quiet", "--prune=14.days.ago"); err != nil {
		s.log.Warn("mirror gc", "err", err)
	}
}

// Release clears a rewrite hold and failure backoff for repo: the next sync
// accepts upstream's current history.
func (r *Runner) Release(ctx context.Context, repo *config.Repo) error {
	unlock, err := r.Store.Lock(repo.Name)
	if err != nil {
		return err
	}
	defer unlock()
	rs, err := r.pullState(ctx, repo)
	if err != nil {
		return err
	}
	st, err := r.Store.Load(repo.Name)
	if err != nil {
		return err
	}
	st.RewriteHold, st.Failure, st.Proposal = nil, nil, nil
	st.LastMerged, st.LastMergedTag = "", ""
	return r.saveState(ctx, rs, st)
}

// GCJobs deletes job directories older than keep.
func (r *Runner) GCJobs(keep time.Duration) {
	dir := filepath.Join(r.Cfg.DataDir, "jobs")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := r.now().Add(-keep)
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || !e.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			r.logger().Warn("removing old job", "job", e.Name(), "err", err)
		}
	}
}

// RemoveAll removes a tree even if it contains read-only directories (Go's
// module cache, for one, makes everything read-only).
func RemoveAll(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(p)
}

func newJobID(repo string, t time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return repo + "-" + t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
