package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/review"
	"github.com/vibeci/vibeci/internal/state"
)

// Remote state (config state_ref). Each repo's state is also kept in its
// fork repository, as a commit without parents under state_ref whose tree
// holds
//
//	repo.json                    state/repos/<repo>.json
//	reviews/<xx>/<commit>.json   state/reviews/<repo>/<commit>.json
//
// (xx: the first two characters of the commit id; verdicts only of upstream
// commits the fork does not contain yet). Commands merge it into the local
// state before they read that, and push the local state back after they
// change it, with a lease: a concurrent writer is merged (state.Merge3,
// the fork's side winning conflicts), never overwritten. Tree entries this
// version does not know are kept as they are.
//
// Whoever can push to the fork can write the state ref, and VibeCI trusts
// it like its local state.

const (
	// stateFetched holds the fork's copy as last fetched; stateSynced the
	// copy the local state was last merged with or pushed as (the merge
	// base of the next merge).
	stateFetched = "refs/vibeci/state-fetched"
	stateSynced  = "refs/vibeci/state-synced"

	maxStateEntries = 100000
	maxRepoJSON     = 16 << 20
	maxVerdictJSON  = 1 << 20
)

// remoteState is a repo's state in its fork. remote is the commit the
// state ref pointed at when it was last read or written ("" if absent).
type remoteState struct {
	r      *Runner
	repo   *config.Repo
	mirror *gitx.Repo
	auth   *gitx.Auth
	remote string
}

// stateTree is the part of a state tree VibeCI knows: blob ids.
type stateTree struct {
	repo     string            // repo.json ("" if absent)
	verdicts map[string]string // commit id -> its verdict
}

// pullState merges the copy of repo's state in the fork into the local
// state. It returns nil, nil if state_ref is not configured. The caller
// holds the repo lock.
func (r *Runner) pullState(ctx context.Context, repo *config.Repo) (*remoteState, error) {
	if r.Cfg.StateRef == "" {
		return nil, nil
	}
	rs, err := r.openState(ctx, repo)
	if err == nil {
		err = rs.pull(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("loading the state from %s in the fork: %w", r.Cfg.StateRef, err)
	}
	return rs, nil
}

// PullState merges the copy of repo's state in the fork into the local
// state, for commands that only read state. It does nothing if state_ref
// is not configured, and reports false if another process is working on
// the repo (that process pushes the state when it is done).
func (r *Runner) PullState(ctx context.Context, repo *config.Repo) (bool, error) {
	if r.Cfg.StateRef == "" {
		return true, nil
	}
	unlock, err := r.Store.Lock(repo.Name)
	if err != nil {
		return false, nil
	}
	defer unlock()
	if _, err := r.pullState(ctx, repo); err != nil {
		return false, err
	}
	return true, nil
}

// StateRefTip returns the commit the state ref points at in repo's fork
// ("" if it does not exist yet).
func (r *Runner) StateRefTip(ctx context.Context, repo *config.Repo) (string, error) {
	rs, err := r.openState(ctx, repo)
	if err != nil {
		return "", err
	}
	return rs.mirror.LsRemote(ctx, repo.Fork.URL, rs.auth, r.Cfg.StateRef)
}

func (r *Runner) openState(ctx context.Context, repo *config.Repo) (*remoteState, error) {
	mirror, err := r.G.InitBare(ctx, filepath.Join(r.Cfg.DataDir, "mirrors", repo.Name+".git"))
	if err != nil {
		return nil, err
	}
	auth, err := ResolveAuth(r.Cfg.DataDir, repo.Name, "fork", repo.Fork.Auth)
	if err != nil {
		return nil, err
	}
	return &remoteState{r: r, repo: repo, mirror: mirror, auth: auth}, nil
}

// pull merges the fork's copy into the local state if it changed since it
// was last merged or pushed. A missing state ref is recreated from the
// local state by the next push.
func (rs *remoteState) pull(ctx context.Context) error {
	ref := rs.r.Cfg.StateRef
	sha, err := rs.mirror.LsRemote(ctx, rs.repo.Fork.URL, rs.auth, ref)
	if err != nil {
		return err
	}
	synced, err := rs.mirror.ResolveObject(ctx, stateSynced+"^{commit}")
	if err != nil {
		return err
	}
	rs.remote = sha
	if sha == "" || sha == synced {
		return nil
	}
	if err := rs.mirror.Fetch(ctx, rs.repo.Fork.URL, rs.auth, "+"+ref+":"+stateFetched); err != nil {
		return err
	}
	if rs.remote, err = rs.mirror.Resolve(ctx, stateFetched); err != nil {
		return err
	}
	theirs, err := rs.read(ctx, rs.remote)
	if err != nil {
		return fmt.Errorf("%s does not hold VibeCI state (%w); fix or delete the ref", short(rs.remote), err)
	}
	base := &stateTree{}
	if synced != "" {
		if base, err = rs.read(ctx, synced); err != nil {
			return err
		}
	}
	if err := rs.merge(ctx, base, theirs); err != nil {
		return err
	}
	return rs.mirror.UpdateRef(ctx, stateSynced, rs.remote)
}

// read lists the state tree of commit, checking its shape and sizes.
func (rs *remoteState) read(ctx context.Context, commit string) (*stateTree, error) {
	entries, err := rs.mirror.LsTreeSizes(ctx, commit)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxStateEntries {
		return nil, fmt.Errorf("%d entries (more than %d)", len(entries), maxStateEntries)
	}
	t := &stateTree{verdicts: map[string]string{}}
	for _, e := range entries {
		if e.Path == "repo.json" {
			if e.Type != "blob" || e.Size > maxRepoJSON {
				return nil, fmt.Errorf("repo.json is not a file of at most %d bytes", maxRepoJSON)
			}
			t.repo = e.OID
			continue
		}
		if sha, ok := verdictPath(e.Path); ok && e.Type == "blob" && e.Size <= maxVerdictJSON {
			t.verdicts[sha] = e.OID
		}
	}
	return t, nil
}

// verdictPath returns the commit of a "reviews/<xx>/<commit>.json" path.
func verdictPath(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, "reviews/")
	if !ok || len(rest) < 3 || rest[2] != '/' {
		return "", false
	}
	sha, ok := strings.CutSuffix(rest[3:], ".json")
	if !ok || (len(sha) != 40 && len(sha) != 64) || !gitx.IsHex(sha) || sha[:2] != rest[:2] {
		return "", false
	}
	return sha, true
}

func verdictTreePath(sha string) string { return "reviews/" + sha[:2] + "/" + sha + ".json" }

func (rs *remoteState) repoFile() string { return "repos/" + rs.repo.Name + ".json" }

func (rs *remoteState) verdictFile(sha string) string {
	return review.CacheDir(rs.repo.Name) + "/" + sha + ".json"
}

// merge three-way merges theirs (the fork's copy) into the local state,
// with base as the common ancestor.
func (rs *remoteState) merge(ctx context.Context, base, theirs *stateTree) error {
	store := rs.r.Store
	blobs, err := rs.mirror.ReadBlobs(ctx, nonEmpty(base.repo, theirs.repo))
	if err != nil {
		return err
	}
	ours, err := store.ReadFile(rs.repoFile())
	if err != nil {
		return err
	}
	if b := blobs[theirs.repo]; b != nil {
		if err := json.Unmarshal(b, &state.RepoState{}); err != nil {
			return fmt.Errorf("%s does not hold VibeCI state (repo.json: %w); fix or delete the ref", short(rs.remote), err)
		}
	}
	merged, err := state.Merge3(blobs[base.repo], ours, blobs[theirs.repo])
	if err != nil {
		return fmt.Errorf("merging repo.json: %w", err)
	}
	if merged != nil && !state.EqualJSON(merged, ours) {
		st := &state.RepoState{}
		if err := json.Unmarshal(merged, st); err != nil {
			return err
		}
		st.Repo = rs.repo.Name
		if err := store.Save(st); err != nil {
			return err
		}
	}

	// Verdicts, file by file. A local verdict neither side had is kept.
	shas := map[string]bool{}
	for sha := range base.verdicts {
		shas[sha] = true
	}
	for sha := range theirs.verdicts {
		shas[sha] = true
	}
	local, err := rs.localVerdicts(ctx, func(sha string) bool { return shas[sha] })
	if err != nil {
		return err
	}
	var fetch []string
	write := map[string]string{} // commit -> blob to write
	for sha := range shas {
		b, o, t := base.verdicts[sha], local[sha], theirs.verdicts[sha]
		switch {
		case o == t || t == b:
			// keep ours
		case o == b || t != "":
			if t == "" {
				if err := store.Remove(rs.verdictFile(sha)); err != nil {
					return err
				}
				continue
			}
			write[sha] = t
			fetch = append(fetch, t)
		}
	}
	data, err := rs.mirror.ReadBlobs(ctx, fetch)
	if err != nil {
		return err
	}
	for sha, oid := range write {
		if !json.Valid(data[oid]) {
			rs.r.logger().Warn("skipping an invalid verdict in the state ref", "repo", rs.repo.Name, "commit", short(sha))
			continue
		}
		if err := store.WriteFile(rs.verdictFile(sha), data[oid]); err != nil {
			return err
		}
	}
	return nil
}

// localVerdicts returns the blob ids of the cached verdicts of the commits
// keep accepts (all if keep is nil), writing the blobs to the mirror.
func (rs *remoteState) localVerdicts(ctx context.Context, keep func(string) bool) (map[string]string, error) {
	out := map[string]string{}
	if rs.repo.PatchMode() {
		return out, nil
	}
	names, err := rs.r.Store.List(review.CacheDir(rs.repo.Name))
	if err != nil {
		return nil, err
	}
	var shas, paths []string
	for _, n := range names {
		sha, ok := strings.CutSuffix(n, ".json")
		if !ok || (len(sha) != 40 && len(sha) != 64) || !gitx.IsHex(sha) || (keep != nil && !keep(sha)) {
			continue
		}
		p, err := rs.r.Store.Path(rs.verdictFile(sha))
		if err != nil {
			return nil, err
		}
		shas, paths = append(shas, sha), append(paths, p)
	}
	oids, err := rs.mirror.HashFiles(ctx, paths)
	if err != nil {
		return nil, err
	}
	for i, sha := range shas {
		out[sha] = oids[i]
	}
	return out, nil
}

// push writes the local state to the fork. If the state ref changed since
// it was read, the change is merged in first (up to three tries).
func (rs *remoteState) push(ctx context.Context) error {
	ref := rs.r.Cfg.StateRef
	for try := 1; ; try++ {
		commit, err := rs.commit(ctx)
		if err != nil || commit == "" {
			return err
		}
		err = rs.mirror.PushLease(ctx, rs.repo.Fork.URL, rs.auth, commit, ref, rs.remote)
		if err == nil {
			rs.remote = commit
			return rs.mirror.UpdateRef(ctx, stateSynced, commit)
		}
		cur, lerr := rs.mirror.LsRemote(ctx, rs.repo.Fork.URL, rs.auth, ref)
		if lerr != nil || cur == rs.remote || try == 3 {
			return fmt.Errorf("saving the state to %s in the fork: %w", ref, err)
		}
		rs.r.logger().Info("the state in the fork changed meanwhile; merging it", "repo", rs.repo.Name)
		if err := rs.pull(ctx); err != nil {
			return fmt.Errorf("saving the state to %s in the fork: %w", ref, err)
		}
	}
}

// commit builds the state commit to push ("" if the fork has it already):
// the fork's tree with VibeCI's entries replaced by the local state.
func (rs *remoteState) commit(ctx context.Context) (string, error) {
	base, err := rs.mirror.EmptyTree(ctx)
	if err != nil {
		return "", err
	}
	cur := &stateTree{}
	if rs.remote != "" {
		base = rs.remote
		if cur, err = rs.read(ctx, rs.remote); err != nil {
			return "", err
		}
	}
	var entries []gitx.IndexEntry
	p, err := rs.r.Store.Path(rs.repoFile())
	if err != nil {
		return "", err
	}
	if b, err := rs.r.Store.ReadFile(rs.repoFile()); err != nil {
		return "", err
	} else if b != nil {
		oids, err := rs.mirror.HashFiles(ctx, []string{p})
		if err != nil {
			return "", err
		}
		entries = append(entries, gitx.IndexEntry{Path: "repo.json", Mode: "100644", OID: oids[0]})
	} else if cur.repo != "" {
		entries = append(entries, gitx.IndexEntry{Path: "repo.json"})
	}
	keep := rs.keepVerdicts(ctx)
	local, err := rs.localVerdicts(ctx, keep)
	if err != nil {
		return "", err
	}
	for sha, oid := range local {
		entries = append(entries, gitx.IndexEntry{Path: verdictTreePath(sha), Mode: "100644", OID: oid})
	}
	for sha := range cur.verdicts {
		if _, ok := local[sha]; !ok {
			entries = append(entries, gitx.IndexEntry{Path: verdictTreePath(sha)})
		}
	}
	if rs.remote == "" && len(entries) == 0 {
		return "", nil // no state yet
	}
	tree, err := rs.mirror.UpdateTree(ctx, base, entries, rs.mirror.GitDir)
	if err != nil {
		return "", err
	}
	if rs.remote != "" {
		if old, err := rs.mirror.ResolveObject(ctx, rs.remote+"^{tree}"); err != nil || old == tree {
			return "", err
		}
	}
	return rs.mirror.CommitTree(ctx, tree, nil, "VibeCI state of "+rs.repo.Name+"\n")
}

// keepVerdicts returns a filter for the verdicts that go to the fork: those
// of upstream commits the fork did not contain at the last sync, and of
// blocked commits. nil (all) if that cannot be told.
func (rs *remoteState) keepVerdicts(ctx context.Context) func(string) bool {
	st, err := rs.r.Store.Load(rs.repo.Name)
	if err != nil || st.LastUpstreamTip == "" || st.LastForkHead == "" {
		return nil
	}
	pending, err := rs.mirror.RevList(ctx, st.LastUpstreamTip, "^"+st.LastForkHead)
	if err != nil {
		return nil
	}
	keep := make(map[string]bool, len(pending)+len(st.Blocked))
	for _, sha := range pending {
		keep[sha] = true
	}
	for sha := range st.Blocked {
		keep[sha] = true
	}
	return func(sha string) bool { return keep[sha] }
}

// pushState pushes the state after a command changed it, with its own
// time limit (the command's context may have run out).
func (rs *remoteState) pushState(ctx context.Context) error {
	if rs == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	return rs.push(pctx)
}

func nonEmpty(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
