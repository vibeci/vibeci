package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/alert"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/resolve"
	"github.com/vibeci/vibeci/internal/source"
	"github.com/vibeci/vibeci/internal/state"
	"github.com/vibeci/vibeci/internal/vers"
)

// Target is an upstream version a patch-mode fork can move to.
type Target struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
}

// SourceStore returns the store of upstream source trees of a patch-mode
// repo, in <data_dir>/sources/<repo>.
func SourceStore(cfg *config.Config, g *gitx.Git, repo *config.Repo, logger *slog.Logger) (*source.Store, error) {
	upAuth, err := ResolveAuth(cfg.DataDir, repo.Name, "upstream", repo.Upstream.Auth)
	if err != nil {
		return nil, err
	}
	main := source.Spec{URL: repo.Upstream.URL, Auth: upAuth, Full: repo.Upstream.Fetch == "full"}
	var subs []source.Spec
	for i, ps := range repo.Patches.Sources {
		a, err := ResolveAuth(cfg.DataDir, repo.Name, fmt.Sprintf("source-%d", i+1), ps.Auth)
		if err != nil {
			return nil, err
		}
		sp := source.Spec{Path: ps.Path, URL: ps.URL, Auth: a, Full: ps.Fetch == "full", RevisionFile: ps.RevisionFile}
		if ps.RevisionRegex != "" {
			if sp.RevisionRegex, err = regexp.Compile(ps.RevisionRegex); err != nil {
				return nil, fmt.Errorf("patches.sources[%d].revision_regex: %w", i, err)
			}
		}
		subs = append(subs, sp)
	}
	return source.NewStore(g, sourcesDir(cfg.DataDir, repo.Name), main, subs, logger)
}

func sourcesDir(dataDir, repo string) string { return filepath.Join(dataDir, "sources", repo) }

// ReadPin returns the upstream version the fork pins at rev.
func ReadPin(ctx context.Context, mirror *gitx.Repo, rev string, p *config.Patches) (string, error) {
	data, found, err := mirror.CatBlob(ctx, rev+":"+p.VersionFile, 1<<20)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the fork has no %s (patches.version_file) at %s", p.VersionFile, short(rev))
	}
	var re *regexp.Regexp
	if p.VersionRegex != "" {
		if re, err = regexp.Compile(p.VersionRegex); err != nil {
			return "", err
		}
	}
	v, err := vers.Read(data, re)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p.VersionFile, err)
	}
	if !vers.Valid(v) {
		return "", fmt.Errorf("%s pins %q, which is not a usable version", p.VersionFile, clip(v, 80))
	}
	return v, nil
}

// FetchVersion reads a version from url: the first group of the first
// match of regex in the response, or the whole response trimmed.
func FetchVersion(ctx context.Context, url, regex string) (string, error) {
	var re *regexp.Regexp
	if regex != "" {
		var err error
		if re, err = regexp.Compile(regex); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "vibeci")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upstream.version_url: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("upstream.version_url: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream.version_url: %s returned HTTP %d: %s", url, resp.StatusCode, clip(strings.TrimSpace(string(body)), 200))
	}
	v, err := vers.Read(body, re)
	if err != nil {
		return "", fmt.Errorf("upstream.version_url: %s: %w", url, err)
	}
	if !vers.Valid(v) {
		return "", fmt.Errorf("upstream.version_url: %s returned %q, which is not a usable version (check upstream.version_regex)", url, clip(v, 80))
	}
	return v, nil
}

// TargetVersion returns the upstream version a patch-mode fork follows:
// the one upstream.version_url names, or the newest version among the
// tags matching upstream.tags (release candidates only if the glob
// contains "-").
func TargetVersion(ctx context.Context, repo *config.Repo, up *source.Mirror) (*Target, error) {
	u := repo.Upstream
	f := vers.Format(u.TagFormat)
	if u.VersionURL != "" {
		v, err := FetchVersion(ctx, u.VersionURL, u.VersionRegex)
		if err != nil {
			return nil, err
		}
		return VersionTarget(ctx, repo, up, v)
	}
	tags, err := up.Tags(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	byVersion := map[string]string{}
	var versions []string
	for name := range tags {
		if ok, _ := path.Match(u.Tags, name); !ok {
			continue
		}
		v, ok := f.Version(name)
		if !ok || !vers.Valid(v) {
			continue
		}
		if prev, dup := byVersion[v]; !dup || name < prev {
			if !dup {
				versions = append(versions, v)
			}
			byVersion[v] = name
		}
	}
	v := vers.Latest(versions, strings.Contains(u.Tags, "-"))
	if v == "" {
		return nil, fmt.Errorf("upstream %s has no tag matching %q (tag_format %q)", u.URL, u.Tags, u.TagFormat)
	}
	return &Target{Version: v, Tag: byVersion[v], Commit: tags[byVersion[v]]}, nil
}

// VersionTarget returns version v of upstream (its tag must exist).
func VersionTarget(ctx context.Context, repo *config.Repo, up *source.Mirror, v string) (*Target, error) {
	if !vers.Valid(v) {
		return nil, fmt.Errorf("%q is not a usable version", v)
	}
	tag := vers.Format(repo.Upstream.TagFormat).Tag(v)
	tags, err := up.Tags(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	if tags[tag] == "" {
		return nil, fmt.Errorf("upstream %s has no tag %s for version %s (yet)", repo.Upstream.URL, tag, v)
	}
	return &Target{Version: v, Tag: tag, Commit: tags[tag]}, nil
}

// oncePatches performs one round for a patch-mode fork: it reads the
// version the fork pins and the version upstream offers, and if that is
// newer it updates the patches, verifies them and pushes.
func (s *syncRun) oncePatches(ctx context.Context) (bool, error) {
	repo, st, out := s.repo, s.st, s.out
	s.base, s.tip = "", ""
	snap, err := s.fetchFork(ctx)
	if err != nil {
		return false, err
	}
	mirror, forkHead, base := snap.mirror, snap.forkHead, snap.base
	out.ForkHead, st.LastForkHead = forkHead, forkHead
	pin, err := ReadPin(ctx, mirror, base, repo.Patches)
	if err != nil {
		return false, err
	}
	out.Pinned, st.PinnedVersion = pin, pin
	store, err := SourceStore(s.Cfg, s.G, repo, s.logger())
	if err != nil {
		return false, err
	}
	up, err := store.Main(ctx)
	if err != nil {
		return false, err
	}
	target, err := TargetVersion(ctx, repo, up)
	if err != nil {
		return false, err
	}
	out.Version, out.Upstream = target.Version, target.Commit
	st.UpstreamVersion, st.LastUpstreamTip = target.Version, target.Commit
	s.base, s.tip = base, target.Commit

	if held, err := s.checkPinRewrite(ctx, up, pin, target); err != nil || held {
		return false, err
	}
	if c := vers.Compare(target.Version, pin); c <= 0 {
		out.Status = StatusUpToDate
		out.Message = "fork is at upstream " + pin
		if c < 0 {
			out.Message += ", newer than upstream's current " + target.Version
		}
		if base != forkHead {
			out.Message += " (proposal pending on " + repo.Push.Branch + ")"
		}
		s.recovered(ctx)
		return false, nil
	}
	if s.backingOff(base, target.Commit) || s.cachedDryRun(base, target.Commit) {
		return false, nil
	}

	jobID, jobDir := s.newJob()
	job, err := s.newPatchJob(ctx, store, up, snap, pin, target, jobID, jobDir)
	if err != nil {
		return false, &jobError{err: err, label: target.Version}
	}
	res, err := job.Run(ctx)
	s.addUsage(job.Usage())
	if err != nil {
		return false, &jobError{err: err, label: target.Version}
	}
	if ok, err := mirror.IsAncestor(ctx, base, res.Commit); err != nil || !ok {
		return false, fmt.Errorf("internal error: result %s does not contain %s", short(res.Commit), short(base))
	}
	out.Commit, out.Target, out.Patches = res.Commit, job.ToCommit, &res.Stats
	what := describePatchResult(res, repo)

	if s.dry() {
		st.Proposal = &state.Proposal{Base: base, Upstream: target.Commit, Target: job.ToCommit, Commit: res.Commit, JobID: jobID, Time: s.now()}
		if err := mirror.UpdateRef(ctx, "refs/vibeci/proposed/"+repo.Fork.Branch, res.Commit); err != nil {
			return false, err
		}
		out.Status = StatusDryRun
		out.Message = "dry run: would push " + what
		s.cleanupJob(jobDir)
		return false, nil
	}

	pushed, err := s.push(ctx, mirror, snap.forkAuth, forkHead, snap.syncTip, res.Commit)
	if err != nil {
		return false, pushHint(err)
	}
	if !pushed {
		out.Status = StatusRaced
		out.Message = "the fork changed while the update was being prepared; it will be redone next cycle"
		return false, nil
	}
	now := s.now()
	st.LastPushed, st.LastSuccess, st.Proposal = res.Commit, now, nil
	if repo.Push.Mode != "branch" {
		st.LastForkHead = res.Commit
	}
	st.LastMerged, st.LastMergedTag, st.PinnedVersion = job.ToCommit, target.Tag, target.Version
	out.Status = StatusSynced
	out.Merges++
	dest := repo.Fork.Branch
	if repo.Push.Mode == "branch" {
		dest = repo.Push.Branch
	}
	out.Message = fmt.Sprintf("pushed %s to %s: %s", short(res.Commit), dest, what)
	s.pushed = append(s.pushed, out.Message)
	s.recovered(ctx)
	var rewritten, dropped []string
	for _, p := range res.Patches {
		switch p.Status {
		case resolve.PatchAgent:
			rewritten = append(rewritten, p.Path)
		case resolve.PatchDropped:
			dropped = append(dropped, p.Path)
		}
	}
	s.alert(ctx, alert.Event{
		Kind: "synced", Severity: "info",
		Title:   fmt.Sprintf("%s: updated to upstream %s", repo.Name, target.Version),
		Message: out.Message,
		Commits: []string{res.Commit},
		Details: map[string]any{"version": target.Version, "from_version": pin, "tag": target.Tag, "upstream_commit": job.ToCommit,
			"patches": res.Stats, "rewritten": rewritten, "dropped": dropped, "agent_models": res.Models(), "checks": res.Verify.Summary()},
	})
	s.cleanupJob(jobDir)
	s.maintain(ctx, mirror)
	// The source copies are rebuilt for the next version: cheaper than
	// pruning partial clones, which keep every blob they ever fetched.
	if err := RemoveAll(sourcesDir(s.Cfg.DataDir, repo.Name)); err != nil {
		s.log.Warn("removing source copies", "err", err)
	}
	return false, nil
}

// newPatchJob fetches both upstream versions and prepares the job that
// moves the fork from pin to target.
func (s *syncRun) newPatchJob(ctx context.Context, store *source.Store, up *source.Mirror, snap *snapshot, pin string, target *Target, jobID, jobDir string) (*resolve.PatchJob, error) {
	repo := s.repo
	commit, err := up.FetchTag(ctx, target.Tag)
	if err != nil {
		return nil, fmt.Errorf("fetching upstream %s: %w", target.Tag, err)
	}
	newSnap, err := store.Snapshot(ctx, target.Version, commit)
	if err != nil {
		return nil, err
	}
	oldTag := vers.Format(repo.Upstream.TagFormat).Tag(pin)
	old := func(ctx context.Context) (*source.Snapshot, error) {
		c, err := up.FetchTag(ctx, oldTag)
		if err != nil {
			return nil, fmt.Errorf("fetching upstream %s (the pinned version): %w", oldTag, err)
		}
		return store.Snapshot(ctx, pin, c)
	}
	roles := s.Cfg.RolesFor(repo)
	var models []llm.Client
	for _, name := range roles.Resolve {
		if m := s.Models[name]; m != nil {
			models = append(models, m)
		}
	}
	return &resolve.PatchJob{
		ID: jobID, Repo: repo, DataDir: s.Cfg.DataDir, JobDir: jobDir,
		G: s.G, Mirror: snap.mirror, Sandbox: s.Sandbox,
		ForkHead: snap.base, From: pin, To: target.Version, ToTag: target.Tag, ToCommit: commit,
		New: newSnap, Old: old,
		Models: models, Auditor: s.Models[roles.Audit],
		BlockOn: repo.Review.BlockOn, MinConfidence: repo.Review.MinConfidence,
		Logger: s.logger().With("job", jobID), // the job adds repo= itself
	}, nil
}

// checkPinRewrite detects the upstream tag of the version VibeCI moved
// the fork to pointing at another commit afterwards.
func (s *syncRun) checkPinRewrite(ctx context.Context, up *source.Mirror, pin string, target *Target) (held bool, err error) {
	st := s.st
	lm, tag := st.LastMerged, st.LastMergedTag
	if lm == "" || tag == "" {
		st.RewriteHold = nil
		return false, nil
	}
	if v, ok := vers.Format(s.repo.Upstream.TagFormat).Version(tag); !ok || v != pin {
		// The pin changed since VibeCI set it: there is nothing to protect.
		st.LastMerged, st.LastMergedTag, st.RewriteHold = "", "", nil
		return false, nil
	}
	now := target.Commit
	if tag != target.Tag {
		tags, err := up.Tags(ctx, tag)
		if err != nil {
			return false, fmt.Errorf("upstream: %w", err)
		}
		now = tags[tag]
	}
	if now == "" || now == lm {
		s.rewriteResolved()
		return false, nil
	}
	what := fmt.Sprintf("upstream tag %s was moved from %s to %s after the fork was updated to it", tag, short(lm), short(now))
	return s.holdRewrite(ctx, lm, now, what), nil
}

func describePatchResult(res *resolve.PatchResult, repo *config.Repo) string {
	st := res.Stats
	counts := fmt.Sprintf("%d patch(es): %d unchanged, %d shifted, %d refreshed", st.Total, st.Exact, st.Shifted, st.Refreshed)
	if st.Agent > 0 {
		counts += fmt.Sprintf(", %d rewritten by %s", st.Agent, strings.Join(res.Models(), "/"))
	}
	if st.Dropped > 0 {
		counts += fmt.Sprintf(", %d dropped", st.Dropped)
	}
	parts := []string{fmt.Sprintf("update from upstream %s to %s", res.From, res.To), counts}
	if res.Verify != nil && len(res.Verify.Results) > 0 {
		parts = append(parts, "checks passed ("+res.Verify.Summary()+")")
	} else if len(repo.Verify) == 0 {
		parts = append(parts, "no checks configured")
	}
	return strings.Join(parts, "; ")
}

// PatchesReport is the result of Runner.Patches.
type PatchesReport struct {
	Repo     string `json:"repo"`
	ForkHead string `json:"fork_head"`
	Tag      string `json:"tag"`
	Upstream string `json:"upstream"`
	*resolve.PatchReport
}

// Patches reports how the fork's patches apply to upstream version
// (default: the version a sync would move to), without the agent,
// committing or pushing.
func (r *Runner) Patches(ctx context.Context, repo *config.Repo, version string) (*PatchesReport, error) {
	if !repo.PatchMode() {
		return nil, fmt.Errorf("repo %s is not a patch-mode fork (repos[].patches is not set)", repo.Name)
	}
	unlock, err := r.Store.Lock(repo.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := r.Store.Load(repo.Name)
	if err != nil {
		return nil, err
	}
	s := &syncRun{Runner: r, parent: ctx, repo: repo, st: st, out: &Outcome{Repo: repo.Name}, log: r.logger().With("repo", repo.Name), started: r.now()}
	snap, err := s.fetchFork(ctx)
	if err != nil {
		return nil, err
	}
	pin, err := ReadPin(ctx, snap.mirror, snap.base, repo.Patches)
	if err != nil {
		return nil, err
	}
	store, err := SourceStore(r.Cfg, r.G, repo, r.logger())
	if err != nil {
		return nil, err
	}
	up, err := store.Main(ctx)
	if err != nil {
		return nil, err
	}
	var target *Target
	if version != "" {
		target, err = VersionTarget(ctx, repo, up, version)
	} else {
		target, err = TargetVersion(ctx, repo, up)
	}
	if err != nil {
		return nil, err
	}
	jobID := newJobID(repo.Name, r.now())
	jobDir := filepath.Join(r.Cfg.DataDir, "jobs", jobID)
	defer os.RemoveAll(jobDir)
	job, err := s.newPatchJob(ctx, store, up, snap, pin, target, jobID, jobDir)
	if err != nil {
		return nil, err
	}
	rep, err := job.Analyze(ctx)
	if err != nil {
		return nil, err
	}
	return &PatchesReport{Repo: repo.Name, ForkHead: snap.base, Tag: target.Tag, Upstream: job.ToCommit, PatchReport: rep}, nil
}

// errPatchMode is returned by commands that only apply to merge-mode forks.
func errPatchMode(what string, repo *config.Repo) error {
	return errors.New(what + " does not apply to " + repo.Name + ": it is a patch-mode fork (repos[].patches), which follows upstream versions instead of merging upstream commits")
}
