package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/resolve"
	"github.com/vibeci/vibeci/internal/state"
)

// Exclusion sources.
const (
	SourceReview = "review"
	SourceManual = "manual"
	// SourceHistory: recovered from the trailers of the fork's merges after
	// the state had lost it (restoreExclusions).
	SourceHistory = "history"
)

// restoreExclusions re-adds exclusions the state lost (a deleted state ref,
// a new data directory) from the fork's history: each merge VibeCI pushed
// names, in its trailers, the upstream commits it excluded
// (VibeCI-Excluded) or re-applied (VibeCI-Restored). Only first-parent
// merges of head that upstream does not contain count, so upstream cannot
// forge them, and only the final trailer paragraph, which VibeCI writes
// itself. Existing records are left alone.
func (s *syncRun) restoreExclusions(ctx context.Context, mirror *gitx.Repo, head string) error {
	if s.historyRead {
		return nil
	}
	s.historyRead = true
	shas, err := mirror.RevList(ctx, "--first-parent", "--merges", head, "--not", "--glob=refs/vibeci/upstream/*", "--glob=refs/vibeci/upstream-tags/*")
	if err != nil {
		return err
	}
	commits, err := mirror.ReadCommits(ctx, shas)
	if err != nil {
		return err
	}
	excluded := map[string]*gitx.Commit{}
	for i := len(commits) - 1; i >= 0; i-- { // oldest first
		c := commits[i]
		for _, tr := range harnessTrailers(c.Body) {
			switch tr[0] {
			case resolve.TrailerExcluded:
				excluded[tr[1]] = c
			case resolve.TrailerRestored:
				delete(excluded, tr[1])
			}
		}
	}
	for sha, c := range excluded {
		if s.st.Excluded[sha] != nil {
			continue
		}
		s.st.Excluded[sha] = &state.Excluded{Commit: sha, Source: SourceHistory, Since: c.CommitTime, Removed: true,
			Summary: "recorded as excluded in fork commit " + short(c.SHA)}
		s.log.Warn("recovered an exclusion the state had lost from the fork's history", "commit", short(sha), "fork_commit", short(c.SHA))
	}
	return nil
}

// harnessTrailers returns the VibeCI-Excluded and VibeCI-Restored trailers
// of a commit message body: the lines of its last paragraph, if that is a
// trailer block VibeCI wrote (it has a VibeCI-Job line).
func harnessTrailers(body string) [][2]string {
	if i := strings.LastIndex(body, "\n\n"); i >= 0 {
		body = body[i+2:]
	}
	var out [][2]string
	job := false
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			return nil
		}
		switch key {
		case resolve.TrailerJob:
			job = true
		case resolve.TrailerExcluded, resolve.TrailerRestored:
			if (len(val) == 40 || len(val) == 64) && gitx.IsHex(val) {
				out = append(out, [2]string{key, val})
			}
		}
	}
	if !job {
		return nil
	}
	return out
}

// sortedExcluded returns the exclusion records oldest first.
func sortedExcluded(st *state.RepoState) []*state.Excluded {
	list := make([]*state.Excluded, 0, len(st.Excluded))
	for _, e := range st.Excluded {
		list = append(list, e)
	}
	sort.Slice(list, func(a, b int) bool {
		if !list[a].Since.Equal(list[b].Since) {
			return list[a].Since.Before(list[b].Since)
		}
		return list[a].Commit < list[b].Commit
	})
	return list
}

// pendingAction returns an operator-requested removal (a commit excluded
// after the fork merged it) or restore (a removed commit that was allowed
// again), if any is due.
func (s *syncRun) pendingAction(ctx context.Context, snap *snapshot) (*action, error) {
	for _, e := range sortedExcluded(s.st) {
		if !e.Restore && e.Removed {
			continue
		}
		inBase, err := snap.mirror.IsAncestor(ctx, e.Commit, snap.base)
		if err != nil {
			return nil, err
		}
		switch {
		case e.Restore && e.Removed && inBase:
			reason := "allowed again"
			if a := s.st.Allowed[e.Commit]; a != nil && a.Reason != "" {
				reason += ": " + a.Reason
			}
			return &action{mode: resolve.ModeRestore, key: "restore:" + e.Commit, target: e.Commit,
				label: "restore of " + short(e.Commit), reason: reason}, nil
		case e.Restore:
			// Never removed from (or no longer in) the fork: nothing to undo.
			delete(s.st.Excluded, e.Commit)
		case inBase:
			return &action{mode: resolve.ModeRemove, key: "remove:" + e.Commit, target: e.Commit,
				label: "removal of " + short(e.Commit), reason: s.exclusionReason(e.Commit, nil)}, nil
		}
	}
	return nil, nil
}

// quarantine lists the commits whose content must stay out of merges onto
// base: everything removed so far.
func (s *syncRun) quarantine(base string) []resolve.Quarantined {
	var q []resolve.Quarantined
	for _, e := range sortedExcluded(s.st) {
		if e.Removed {
			q = append(q, resolve.Quarantined{Commit: e.Commit, Except: base})
		}
	}
	return q
}

// recordExclusions updates state after a push.
func (s *syncRun) recordExclusions(res *resolve.Result, now time.Time) {
	st := s.st
	for _, sha := range res.Removed {
		e := st.Excluded[sha]
		if e == nil {
			e = &state.Excluded{Commit: sha, Source: SourceReview, Since: now}
			st.Excluded[sha] = e
		}
		if b := st.Blocked[sha]; b != nil && e.Verdict == "" {
			e.Verdict, e.Confidence, e.Summary = b.Verdict, b.Confidence, b.Summary
		}
		e.Removed = true
		delete(st.Blocked, sha)
		if !containsString(s.out.Excluded, sha) {
			s.out.Excluded = append(s.out.Excluded, sha)
		}
	}
	if res.Restored != "" {
		delete(st.Excluded, res.Restored)
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// allowList is the config's allow_commits plus commits allowed with
// `vibeci allow`.
func (s *syncRun) allowList() []string {
	out := append([]string(nil), s.repo.Review.AllowCommits...)
	for sha := range s.st.Allowed {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out
}

// ChangeResult reports what Allow or Exclude did to one commit.
type ChangeResult struct {
	Commit string `json:"commit"`
	// Effect: "allowed", "restore-scheduled", "exclusion-cancelled" (allow);
	// "excluded", "restore-cancelled", "unchanged" (exclude).
	Effect string `json:"effect"`
	Detail string `json:"detail"`
}

var (
	errNoMirror        = errors.New("the repo has not been synced yet (no mirror); run `vibeci run` first or pass full 40/64-character commit ids")
	errNoMirrorExclude = errors.New("the repo has not been synced yet (no mirror); run `vibeci run` first: only commits in upstream's fetched history can be excluded")
)

// resolveCommit turns an operator-supplied hex id into a full commit id.
func (r *Runner) resolveCommit(ctx context.Context, mirror *gitx.Repo, st *state.RepoState, rev string) (string, error) {
	rev = strings.ToLower(strings.TrimSpace(rev))
	if len(rev) < 7 || len(rev) > 64 || !gitx.IsHex(rev) {
		return "", fmt.Errorf("%q is not a commit id (7 to 64 hex characters)", rev)
	}
	if mirror != nil {
		if sha, _ := mirror.ResolveObject(ctx, rev+"^{commit}"); sha != "" {
			return sha, nil
		}
	}
	var match string
	for _, set := range []map[string]bool{keysOf(st.Blocked), keysOf(st.Excluded), keysOf(st.Allowed)} {
		for sha := range set {
			if strings.HasPrefix(sha, rev) {
				if match != "" && match != sha {
					return "", fmt.Errorf("%q is ambiguous", rev)
				}
				match = sha
			}
		}
	}
	if match != "" {
		return match, nil
	}
	if mirror == nil && (len(rev) == 40 || len(rev) == 64) {
		return rev, nil
	}
	if mirror == nil {
		return "", errNoMirror
	}
	return "", fmt.Errorf("commit %s not found in the mirror (fetched fork and upstream history)", rev)
}

func keysOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func (r *Runner) openMirror(repo *config.Repo) *gitx.Repo {
	dir := filepath.Join(r.Cfg.DataDir, "mirrors", repo.Name+".git")
	if r.G == nil {
		return nil
	}
	if _, err := r.G.Run(context.Background(), gitx.Opts{GitDir: dir}, "rev-parse", "--git-dir"); err != nil {
		return nil
	}
	return r.G.Open(dir)
}

// Allow clears upstream commits: the review gate lets them through from
// now on, and commits that were excluded are re-applied by the next sync
// (or simply merged, if they had not been merged yet).
func (r *Runner) Allow(ctx context.Context, repo *config.Repo, revs []string, reason string) ([]ChangeResult, error) {
	if repo.PatchMode() {
		return nil, errPatchMode("allow", repo)
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
	mirror := r.openMirror(repo)
	if mirror != nil && !r.hasUpstream(ctx, mirror) {
		mirror = nil // only the state was fetched (state_ref)
	}
	var out []ChangeResult
	for _, rev := range revs {
		sha, err := r.resolveCommit(ctx, mirror, st, rev)
		if err != nil {
			return nil, err
		}
		res := ChangeResult{Commit: sha, Effect: "allowed", Detail: "the review gate lets it through from now on"}
		if e := st.Excluded[sha]; e != nil {
			if e.Removed {
				e.Restore = true
				res.Effect, res.Detail = "restore-scheduled", "the next sync re-applies its changes to the fork"
			} else {
				delete(st.Excluded, sha)
				res.Effect, res.Detail = "exclusion-cancelled", "it had not been removed yet; it is merged normally"
			}
		}
		st.Allowed[sha] = &state.Allowed{Commit: sha, Reason: reason, Time: r.now()}
		delete(st.Blocked, sha)
		out = append(out, res)
	}
	// The inputs changed: recompute instead of waiting out a backoff or
	// reusing a cached dry run.
	st.Proposal, st.Failure = nil, nil
	return out, r.saveState(ctx, rs, st)
}

// saveState saves st after a command changed it, also to the fork
// (state_ref).
func (r *Runner) saveState(ctx context.Context, rs *remoteState, st *state.RepoState) error {
	if err := r.Store.Save(st); err != nil {
		return err
	}
	if err := rs.pushState(ctx); err != nil {
		return fmt.Errorf("%w; the change is saved only in %s (run the command again)", err, r.Cfg.DataDir)
	}
	return nil
}

// Exclude keeps upstream commits out of the fork: pending ones are left out
// of future merges, and ones the fork already merged are removed by the
// next sync. Their content is quarantined in all later merges.
func (r *Runner) Exclude(ctx context.Context, repo *config.Repo, revs []string, reason string) ([]ChangeResult, error) {
	if repo.PatchMode() {
		return nil, errPatchMode("exclude", repo)
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
	mirror := r.openMirror(repo)
	if mirror == nil || !r.hasUpstream(ctx, mirror) {
		if r.Cfg.StateRef == "" {
			return nil, errNoMirrorExclude
		}
		// A host that keeps its state in the fork may start empty (a CI
		// runner): fetch the fork and upstream now.
		s := &syncRun{Runner: r, parent: ctx, repo: repo, st: st, out: &Outcome{Repo: repo.Name}, log: r.logger().With("repo", repo.Name), started: r.now()}
		snap, err := s.fetch(ctx)
		if err != nil {
			return nil, err
		}
		mirror = snap.mirror
	}
	var out []ChangeResult
	for _, rev := range revs {
		sha, err := r.resolveCommit(ctx, mirror, st, rev)
		if err != nil {
			return nil, err
		}
		cs, err := mirror.ReadCommits(ctx, []string{sha})
		if err != nil {
			return nil, err
		}
		if len(cs[0].Parents) == 0 {
			return nil, fmt.Errorf("commit %s has no parent; it cannot be excluded", short(sha))
		}
		refs, err := r.G.Run(ctx, gitx.Opts{GitDir: mirror.GitDir}, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", sha, "refs/vibeci/upstream", "refs/vibeci/upstream-tags")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(refs)) == "" {
			return nil, fmt.Errorf("commit %s is not part of upstream's fetched history; only upstream commits can be excluded", short(sha))
		}
		res := ChangeResult{Commit: sha}
		switch e := st.Excluded[sha]; {
		case e == nil:
			st.Excluded[sha] = &state.Excluded{Commit: sha, Source: SourceManual, Summary: clip(reason, 1000), Since: r.now()}
			res.Effect, res.Detail = "excluded", "kept out of future merges; if the fork already merged it, the next sync removes it"
		case e.Restore:
			e.Restore = false
			res.Effect, res.Detail = "restore-cancelled", "it stays excluded"
		default:
			res.Effect, res.Detail = "unchanged", "already excluded"
		}
		delete(st.Allowed, sha)
		out = append(out, res)
	}
	st.Proposal, st.Failure = nil, nil
	return out, r.saveState(ctx, rs, st)
}

// hasUpstream reports whether the mirror holds fetched upstream history
// (with state_ref, the mirror can exist for the state alone).
func (r *Runner) hasUpstream(ctx context.Context, mirror *gitx.Repo) bool {
	for _, prefix := range []string{"refs/vibeci/upstream/", "refs/vibeci/upstream-tags/"} {
		if refs, err := mirror.ForEachRef(ctx, prefix, ""); err == nil && len(refs) > 0 {
			return true
		}
	}
	return false
}
