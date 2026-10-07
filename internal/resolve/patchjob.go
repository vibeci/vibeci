package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/patch"
	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/source"
	"github.com/vibeci/vibeci/internal/vers"
)

// TrailerUpstream records the upstream version a patch-mode commit moves
// the fork to.
const TrailerUpstream = "VibeCI-Upstream"

// Patch statuses.
const (
	PatchExact     = "exact"     // applies at its recorded lines; unchanged
	PatchShifted   = "shifted"   // applies at other lines; line numbers updated
	PatchRefreshed = "refreshed" // applied with fuzz or partly upstreamed; hunks regenerated
	PatchAgent     = "agent"     // rewritten by the patch agent
	PatchDropped   = "dropped"   // upstream contains its changes; removed
	PatchFailed    = "failed"    // does not apply (reports only)
)

// PatchJob moves a patch-mode fork from upstream version From to To: it
// applies the fork's patches to the upstream tree at To, rewrites those
// that no longer apply exactly (hunks that only moved or applied with fuzz
// deterministically, the rest with the patch agent in a sandbox), checks
// that the rewritten series re-applies exactly, and commits the patches
// and the new pin on top of the fork head.
type PatchJob struct {
	ID       string
	Repo     *config.Repo
	DataDir  string
	JobDir   string
	G        *gitx.Git
	Mirror   *gitx.Repo // the fork's mirror
	Sandbox  sandbox.Provider
	ForkHead string
	From, To string
	// ToTag and ToCommit are the upstream tag and commit of To.
	ToTag, ToCommit string
	// New is the upstream tree at To. Old returns it at From; it is only
	// fetched when the agent needs context or a binary change is checked.
	New           *source.Snapshot
	Old           func(ctx context.Context) (*source.Snapshot, error)
	Models        []llm.Client
	Auditor       llm.Client
	BlockOn       string
	MinConfidence float64
	Logger        *slog.Logger

	base     *Job
	usage    llm.Usage
	patches  []*patchFile
	series   []*seriesFile
	outcomes []*PatchOutcome
	oldSnap  *source.Snapshot
	oldErr   error
	oldTree  *patch.Tree
	oldAt    int
	allPaths []string
	binCache map[string]bool
}

// PatchOutcome is what happened to one patch.
type PatchOutcome struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	Hunks      int    `json:"hunks"`
	Failed     int    `json:"failed_hunks,omitempty"`
	Model      string `json:"model,omitempty"`
	Turns      int    `json:"turns,omitempty"`
	NovelLines int    `json:"novel_lines,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// PatchStats counts patches by outcome.
type PatchStats struct {
	Total     int `json:"total"`
	Exact     int `json:"exact"`
	Shifted   int `json:"shifted"`
	Refreshed int `json:"refreshed"`
	Agent     int `json:"agent"`
	Dropped   int `json:"dropped"`
}

// PatchResult describes the produced commit.
type PatchResult struct {
	Commit  string
	From    string
	To      string
	Patches []*PatchOutcome
	Stats   PatchStats
	Verify  *VerifyReport
	Usage   llm.Usage
}

// Models returns the distinct models that rewrote patches.
func (r *PatchResult) Models() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range r.Patches {
		if p.Model != "" && !seen[p.Model] {
			seen[p.Model] = true
			out = append(out, p.Model)
		}
	}
	return out
}

// patchFile is one patch of the fork's series.
type patchFile struct {
	index  int
	name   string // as listed in the series, or the fork path for globs
	path   string // path in the fork
	mode   string
	strip  int
	root   string
	data   []byte
	parsed *patch.Patch
	series *seriesFile

	newData []byte
	dropped bool
}

type seriesFile struct {
	path   string
	mode   string
	parsed *patch.Series
	drop   []string
}

// Usage returns the LLM usage accumulated so far (also after failures).
func (j *PatchJob) Usage() llm.Usage { return j.usage }

func (j *PatchJob) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.Default()
	}
	return j.Logger
}

func (j *PatchJob) init() {
	if j.base != nil {
		return
	}
	// The merge job's helpers (sandboxes, prefetch, transcripts) work on
	// the same job directory.
	j.base = &Job{ID: j.ID, Repo: j.Repo, DataDir: j.DataDir, JobDir: j.JobDir, G: j.G, Mirror: j.Mirror, Sandbox: j.Sandbox,
		ForkHead: j.ForkHead, Target: j.ForkHead, Logger: j.Logger,
		env: map[string]string{"VIBECI_UPSTREAM_VERSION": j.To, "VIBECI_PREVIOUS_VERSION": j.From}}
	j.binCache = map[string]bool{}
}

// load reads the fork's patch series.
func (j *PatchJob) load(ctx context.Context) error {
	tree, err := j.Mirror.LsTree(ctx, j.ForkHead)
	if err != nil {
		return err
	}
	read := func(p string) ([]byte, gitx.TreeEntry, error) {
		e, ok := tree[p]
		if !ok {
			return nil, e, fmt.Errorf("%s does not exist in the fork at %s", p, short(j.ForkHead))
		}
		if e.Type != "blob" || e.Mode == "120000" {
			return nil, e, fmt.Errorf("%s is not a regular file in the fork", p)
		}
		data, _, err := j.Mirror.CatBlob(ctx, e.OID, 64<<20)
		return data, e, err
	}
	seen := map[string]bool{}
	for _, set := range j.Repo.Patches.Sets {
		strip := 1
		if set.Strip != nil {
			strip = *set.Strip
		}
		if set.Series != "" {
			data, e, err := read(set.Series)
			if err != nil {
				return fmt.Errorf("patches.series: %w", err)
			}
			s, err := patch.ParseSeries(data)
			if err != nil {
				return fmt.Errorf("%s: %w", set.Series, err)
			}
			sf := &seriesFile{path: set.Series, mode: e.Mode, parsed: s}
			j.series = append(j.series, sf)
			for _, en := range s.Entries {
				st := strip
				if en.Strip >= 0 {
					st = en.Strip
				}
				j.patches = append(j.patches, &patchFile{name: en.Name, path: path.Join(path.Dir(set.Series), en.Name), strip: st, root: set.Root, series: sf})
			}
			continue
		}
		var ps []string
		for p, e := range tree {
			if e.Type == "blob" && e.Mode != "120000" && config.MatchGlob(set.Glob, p) {
				ps = append(ps, p)
			}
		}
		sort.Strings(ps)
		if len(ps) == 0 {
			return fmt.Errorf("patches glob %q matches no file in the fork", set.Glob)
		}
		for _, p := range ps {
			j.patches = append(j.patches, &patchFile{name: p, path: p, strip: strip, root: set.Root})
		}
	}
	for i, pf := range j.patches {
		pf.index = i
		if seen[pf.path] {
			return fmt.Errorf("patch %s is listed twice", pf.path)
		}
		seen[pf.path] = true
		data, e, err := read(pf.path)
		if err != nil {
			return err
		}
		pf.data, pf.mode = data, e.Mode
		if pf.parsed, err = patch.Parse(data); err != nil {
			return fmt.Errorf("patch %s: %w", pf.path, err)
		}
		if len(pf.parsed.Files) == 0 {
			return fmt.Errorf("patch %s contains no diff", pf.path)
		}
		if _, err := patch.TouchedPaths(pf.parsed, pf.strip, pf.root); err != nil {
			return fmt.Errorf("patch %s: %w", pf.path, err)
		}
	}
	if len(j.patches) == 0 {
		return errors.New("the fork's series lists no patches")
	}
	return nil
}

// touched returns every tree path the patches touch.
func (j *PatchJob) touched(only func(*patchFile) bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, pf := range j.patches {
		if only != nil && !only(pf) {
			continue
		}
		parsed := pf.parsed
		if pf.newData != nil {
			if np, err := patch.Parse(pf.newData); err == nil {
				parsed = np
			}
		}
		ps, _ := patch.TouchedPaths(parsed, pf.strip, pf.root)
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// prefetch loads the files the patches touch at To and reports files
// inside sub-repositories without a configured source.
func (j *PatchJob) prefetch(ctx context.Context) error {
	paths := j.touched(nil)
	if err := j.New.Prefetch(ctx, paths); err != nil {
		return err
	}
	subs := map[string]*source.SubmoduleError{}
	var order []string
	for _, p := range paths {
		_, _, err := j.New.Entry(ctx, p)
		var se *source.SubmoduleError
		if errors.As(err, &se) {
			if subs[se.Submodule] == nil {
				order = append(order, se.Submodule)
			}
			subs[se.Submodule] = se
		} else if err != nil && !strings.Contains(err.Error(), "is a directory") {
			return err
		}
	}
	if len(order) == 0 {
		return nil
	}
	var msgs []string
	for _, s := range order {
		msgs = append(msgs, subs[s].Error())
	}
	return &FailedError{What: "the patches touch files in sub-repositories that have no source configured", Attempts: msgs}
}

func (j *PatchJob) options(ctx context.Context, pf *patchFile) patch.Options {
	p := j.Repo.Patches
	return patch.Options{Strip: pf.strip, Root: pf.root, Fuzz: p.Fuzz, IgnoreWhitespace: p.IgnoreWhitespace, DetectApplied: true,
		KeepBinary: func(path string) bool { return j.binaryUnchanged(ctx, path) }}
}

// binaryUnchanged reports whether path is the same at From and To, so a
// binary section written against From still applies.
func (j *PatchJob) binaryUnchanged(ctx context.Context, p string) bool {
	if v, ok := j.binCache[p]; ok {
		return v
	}
	ok := false
	if old, err := j.old(ctx); err == nil {
		m1, o1, e1 := old.Entry(ctx, p)
		m2, o2, e2 := j.New.Entry(ctx, p)
		ok = e1 == nil && e2 == nil && m1 == m2 && o1 == o2
	}
	j.binCache[p] = ok
	return ok
}

func (j *PatchJob) old(ctx context.Context) (*source.Snapshot, error) {
	if j.oldSnap == nil && j.oldErr == nil {
		if j.Old == nil {
			j.oldErr = errors.New("the old upstream version is not available")
		} else if j.oldSnap, j.oldErr = j.Old(ctx); j.oldErr != nil {
			j.logger().Warn("old upstream version unavailable; the agent works without it", "repo", j.Repo.Name, "version", j.From, "err", j.oldErr)
		}
	}
	return j.oldSnap, j.oldErr
}

func hunkCount(p *patch.Patch) int {
	n := 0
	for _, f := range p.Files {
		n += max(1, len(f.Hunks))
	}
	return n
}

// Run produces the commit (not pushed).
func (j *PatchJob) Run(ctx context.Context) (*PatchResult, error) {
	if err := os.MkdirAll(j.JobDir, 0o755); err != nil {
		return nil, err
	}
	j.init()
	if err := j.load(ctx); err != nil {
		return nil, err
	}
	if err := j.prefetch(ctx); err != nil {
		return nil, err
	}
	cfg := j.Repo.Patches
	tree := patch.NewTree(j.New.Bind(ctx))
	res := &PatchResult{From: j.From, To: j.To}
	for _, pf := range j.patches {
		before := tree.Clone()
		ar, err := patch.Apply(pf.parsed, tree, j.options(ctx, pf))
		if err != nil {
			return nil, fmt.Errorf("applying %s: %w", pf.path, err)
		}
		out := &PatchOutcome{Name: pf.name, Path: pf.path, Hunks: hunkCount(pf.parsed), Failed: ar.FailedHunks()}
		j.outcomes = append(j.outcomes, out)
		if out.Failed == 0 {
			np, err := patch.Refresh(pf.parsed, ar, cfg.KeepOffsets)
			if err != nil {
				return nil, fmt.Errorf("refreshing %s: %w", pf.path, err)
			}
			if np == nil {
				if !cfg.Drop() {
					return nil, &FailedError{What: "patch " + pf.path, Attempts: []string{upstreamedProblem(j.To)}}
				}
				out.Status, pf.dropped = PatchDropped, true
				j.logger().Info("patch is part of upstream now; dropping it", "repo", j.Repo.Name, "patch", pf.path)
				continue
			}
			pf.newData = np.Format()
			switch ar.Status() {
			case patch.Exact:
				out.Status = PatchExact
			case patch.Shifted:
				out.Status = PatchShifted
			default:
				out.Status = PatchRefreshed
			}
			continue
		}
		if bin := binaryFailures(ar); len(bin) > 0 {
			return nil, &FailedError{What: "patch " + pf.path, Attempts: []string{fmt.Sprintf("its binary change to %s no longer applies: the file changed upstream, and VibeCI cannot rewrite binary patches. Update the patch by hand", strings.Join(bin, ", "))}}
		}
		j.logger().Info("patch does not apply; starting the patch agent", "repo", j.Repo.Name, "patch", pf.path, "failed_hunks", out.Failed, "hunks", out.Hunks)
		r, err := j.agentPatch(ctx, pf, before, tree, ar)
		if err != nil {
			return nil, err
		}
		for p, st := range r.states {
			tree.Set(p, st)
		}
		out.Model, out.Turns, out.NovelLines, out.Summary = r.model, r.turns, r.novel, clip(r.summary, 2000)
		if r.data == nil {
			out.Status, pf.dropped = PatchDropped, true
		} else {
			out.Status, pf.newData = PatchAgent, r.data
		}
	}
	if err := j.recheck(ctx, tree); err != nil {
		return nil, &infraError{err}
	}
	res.Patches = j.outcomes
	for _, o := range res.Patches {
		res.Stats.Total++
		switch o.Status {
		case PatchExact:
			res.Stats.Exact++
		case PatchShifted:
			res.Stats.Shifted++
		case PatchRefreshed:
			res.Stats.Refreshed++
		case PatchAgent:
			res.Stats.Agent++
		case PatchDropped:
			res.Stats.Dropped++
		}
	}
	commit, err := j.commit(ctx, res)
	if err != nil {
		return nil, err
	}
	res.Commit = commit
	res.Verify = &VerifyReport{Stage: "verify"}
	if len(j.Repo.Verify) > 0 {
		rep, err := j.verify(ctx, commit, tree)
		if err != nil {
			return nil, err
		}
		res.Verify = rep
		if !rep.Passed() {
			return nil, &FailedError{What: fmt.Sprintf("the patches updated for upstream %s fail the verify commands (nothing was pushed)", j.To), Attempts: []string{rep.Failure(6000)}}
		}
	}
	res.Usage = j.usage
	return res, nil
}

func upstreamedProblem(version string) string {
	return fmt.Sprintf("upstream %s already contains all of its changes: remove it from the series, or set patches.drop_upstreamed to true", version)
}

func binaryFailures(ar *patch.Result) []string {
	var out []string
	for _, f := range ar.Files {
		if f.File.Binary && f.Status == patch.Failed {
			out = append(out, f.Path)
		}
	}
	return out
}

// recheck re-applies the rewritten series from scratch, strictly, and
// compares the result with the tree built patch by patch.
func (j *PatchJob) recheck(ctx context.Context, tree *patch.Tree) error {
	cfg := j.Repo.Patches
	check := patch.NewTree(j.New.Bind(ctx))
	for _, pf := range j.patches {
		if pf.dropped {
			continue
		}
		np, err := patch.Parse(pf.newData)
		if err != nil {
			return fmt.Errorf("internal error: the rewritten %s does not parse: %w", pf.path, err)
		}
		ar, err := patch.Apply(np, check, patch.Options{Strip: pf.strip, Root: pf.root, Exact: !cfg.KeepOffsets, IgnoreWhitespace: cfg.IgnoreWhitespace,
			KeepBinary: func(path string) bool { return j.binaryUnchanged(ctx, path) }})
		if err != nil {
			return fmt.Errorf("internal error: re-applying %s: %w", pf.path, err)
		}
		for _, f := range ar.Files {
			if f.Status > patch.Shifted || (f.Status == patch.Shifted && !cfg.KeepOffsets) {
				return fmt.Errorf("internal error: the rewritten %s does not apply exactly to %s (%s: %s)", pf.path, f.Path, f.Status, f.Problem)
			}
		}
	}
	paths := map[string]bool{}
	for _, p := range tree.Paths() {
		paths[p] = true
	}
	for _, p := range check.Paths() {
		paths[p] = true
	}
	for p := range paths {
		a, err := tree.Get(p)
		if err != nil {
			return err
		}
		b, err := check.Get(p)
		if err != nil {
			return err
		}
		if !a.Equal(b) {
			return fmt.Errorf("internal error: re-applying the rewritten series does not reproduce %s", p)
		}
	}
	return nil
}

// commit writes the rewritten patches, the series and the new pin on top
// of the fork head.
func (j *PatchJob) commit(ctx context.Context, res *PatchResult) (string, error) {
	cfg := j.Repo.Patches
	var entries []gitx.IndexEntry
	hash := func(p, mode string, data []byte) error {
		oid, err := j.Mirror.HashBlob(ctx, data)
		if err != nil {
			return err
		}
		if mode == "" {
			mode = "100644"
		}
		entries = append(entries, gitx.IndexEntry{Path: p, Mode: mode, OID: oid})
		return nil
	}
	for _, pf := range j.patches {
		switch {
		case pf.dropped:
			entries = append(entries, gitx.IndexEntry{Path: pf.path})
			if pf.series != nil {
				pf.series.drop = append(pf.series.drop, pf.name)
			}
		case !bytes.Equal(pf.newData, pf.data):
			if err := hash(pf.path, pf.mode, pf.newData); err != nil {
				return "", err
			}
		}
	}
	for _, sf := range j.series {
		if len(sf.drop) > 0 {
			sf.parsed.Remove(sf.drop...)
			if err := hash(sf.path, sf.mode, sf.parsed.Format()); err != nil {
				return "", err
			}
		}
	}
	vdata, found, err := j.Mirror.CatBlob(ctx, j.ForkHead+":"+cfg.VersionFile, 1<<20)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the fork has no %s", cfg.VersionFile)
	}
	var re *regexp.Regexp
	if cfg.VersionRegex != "" {
		re = regexp.MustCompile(cfg.VersionRegex)
	}
	nv, err := vers.Replace(vdata, re, j.To)
	if err != nil {
		return "", fmt.Errorf("%s: %w", cfg.VersionFile, err)
	}
	if err := hash(cfg.VersionFile, "", nv); err != nil {
		return "", err
	}
	for f, content := range cfg.UpdateFiles {
		if err := hash(f, "", []byte(strings.ReplaceAll(content, vers.Placeholder, j.To))); err != nil {
			return "", err
		}
	}
	tree, err := j.Mirror.UpdateTree(ctx, j.ForkHead, entries, j.JobDir)
	if err != nil {
		return "", err
	}
	return j.Mirror.CommitTree(ctx, tree, []string{j.ForkHead}, j.message(res))
}

func (j *PatchJob) message(res *PatchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Update upstream to %s (from %s)\n\n", j.To, j.From)
	s := res.Stats
	shifted := "with updated line numbers"
	if j.Repo.Patches.KeepOffsets {
		shifted = "applying at other lines"
	}
	fmt.Fprintf(&b, "%d patch(es): %d unchanged, %d %s, %d refreshed, %d rewritten by the patch agent, %d dropped.\n", s.Total, s.Exact, s.Shifted, shifted, s.Refreshed, s.Agent, s.Dropped)
	var agent, dropped []string
	for _, o := range res.Patches {
		switch {
		case o.Status == PatchAgent:
			agent = append(agent, fmt.Sprintf("  %s (%s): %s", o.Path, o.Model, wrapIndent(clip(o.Summary, 600), 72, "    ")))
		case o.Status == PatchDropped && o.Model != "":
			dropped = append(dropped, fmt.Sprintf("  %s (judged obsolete by %s): %s", o.Path, o.Model, wrapIndent(clip(o.Summary, 400), 72, "    ")))
		case o.Status == PatchDropped:
			dropped = append(dropped, "  "+o.Path+" (upstream contains its changes)")
		}
	}
	if len(agent) > 0 {
		b.WriteString("\nRewritten by the patch agent:\n" + strings.Join(agent, "\n") + "\n")
	}
	if len(dropped) > 0 {
		b.WriteString("\nDropped:\n" + strings.Join(dropped, "\n") + "\n")
	}
	fmt.Fprintf(&b, "\nUpstream: %s @ %s (%s)\n", j.Repo.Upstream.URL, j.ToTag, j.ToCommit)
	fmt.Fprintf(&b, "\n%s: %s\n%s: %s\n", TrailerJob, j.ID, TrailerUpstream, j.To)
	return b.String()
}

func wrapIndent(s string, width int, indent string) string {
	return strings.ReplaceAll(wrap(s, width), "\n", "\n"+indent)
}

// verify runs the verify commands in a fresh sandbox whose workspace has
// fork/ (the fork at commit), upstream/ (the upstream tree at To: the
// touched files or everything) and patched/ (upstream with the patches).
func (j *PatchJob) verify(ctx context.Context, commit string, tree *patch.Tree) (*VerifyReport, error) {
	dir := j.base.path("verify")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fork := filepath.Join(dir, "fork")
	if err := os.MkdirAll(fork, 0o755); err != nil {
		return nil, err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: fork}, "init", "--quiet"); err != nil {
		return nil, err
	}
	alt := filepath.Join(fork, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(j.Mirror.GitDir, "objects")+"\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: fork}, "checkout", "--quiet", "--detach", commit); err != nil {
		return nil, err
	}

	modes := map[string]string{}
	paths := j.touched(func(pf *patchFile) bool { return !pf.dropped })
	if j.Repo.Patches.VerifyTree == "full" {
		ents, err := j.New.List(ctx, "")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range paths {
			seen[p] = true
		}
		for _, e := range ents {
			modes[e.Path] = e.Mode
			if !seen[e.Path] {
				paths = append(paths, e.Path)
			}
		}
		if err := j.New.Prefetch(ctx, paths); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	src := j.New.Bind(ctx)
	write := func(p string, data []byte, mode string) error {
		if mode == "120000" || mode == "160000" {
			return nil // symlinks and submodule entries are left out
		}
		if d := path.Dir(p); d != "." {
			if err := root.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		perm := os.FileMode(0o644)
		if mode == "100755" {
			perm = 0o755
		}
		return root.WriteFile(p, data, perm)
	}
	for _, p := range paths {
		if m, _, err := j.New.Entry(ctx, p); err == nil && m != "" {
			modes[p] = m
		}
		data, exists, err := src.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := write("upstream/"+p, data, modes[p]); err != nil {
				return nil, err
			}
		}
		st, err := tree.Get(p)
		if err != nil {
			return nil, err
		}
		if st.Exists {
			if err := write("patched/"+p, st.Data, modes[p]); err != nil {
				return nil, err
			}
		}
	}
	for _, d := range []string{"upstream", "patched"} {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	ws := &workspace{Dir: dir, Rel: j.base.relPath(dir)}
	cache := j.base.path("cache-verify")
	if rep, err := j.base.prefetch(ctx, ws, cache); err != nil || !rep.Passed() {
		return rep, err
	}
	sb, err := j.base.newSandbox(ctx, j.Repo.Sandbox.VerifyProfile, ws, cache)
	if err != nil {
		return nil, err
	}
	defer closeSandbox(ctx, sb)
	j.logger().Info("verifying the updated patches", "repo", j.Repo.Name, "commit", short(commit), "files", len(paths))
	return runCommands(ctx, sb, "verify", j.Repo.Verify, j.base.env)
}

// PatchCheck is the dry outcome of one patch (Analyze).
type PatchCheck struct {
	Name   string       `json:"name"`
	Path   string       `json:"path"`
	Status string       `json:"status"` // exact, shifted, refreshed, dropped, failed
	Hunks  int          `json:"hunks"`
	Failed []FailedHunk `json:"failed,omitempty"`
	// Problem explains a failure that is not about hunks.
	Problem string `json:"problem,omitempty"`
}

// FailedHunk describes a hunk that does not apply.
type FailedHunk struct {
	File string `json:"file"`
	Hunk int    `json:"hunk,omitempty"` // 1-based; 0 for a whole-file problem
	Line int    `json:"line,omitempty"` // where it was expected
	// Near is the line where most of its old lines match, Matched how many.
	Near    int    `json:"near,omitempty"`
	Matched int    `json:"matched,omitempty"`
	Total   int    `json:"total,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// PatchReport is what a sync would do with the patches, without the agent.
type PatchReport struct {
	From    string         `json:"pinned"`
	To      string         `json:"version"`
	Counts  map[string]int `json:"counts"`
	Patches []*PatchCheck  `json:"patches"`
}

// Analyze applies the series to To and reports every patch's status
// without running the agent, committing or pushing. Patches after a failed
// one see the tree with the failed patch's applicable hunks applied.
func (j *PatchJob) Analyze(ctx context.Context) (*PatchReport, error) {
	j.init()
	if err := j.load(ctx); err != nil {
		return nil, err
	}
	if err := j.prefetch(ctx); err != nil {
		return nil, err
	}
	tree := patch.NewTree(j.New.Bind(ctx))
	rep := &PatchReport{From: j.From, To: j.To, Counts: map[string]int{}}
	for _, pf := range j.patches {
		ar, err := patch.Apply(pf.parsed, tree, j.options(ctx, pf))
		if err != nil {
			return nil, fmt.Errorf("applying %s: %w", pf.path, err)
		}
		pc := &PatchCheck{Name: pf.name, Path: pf.path, Hunks: hunkCount(pf.parsed)}
		switch {
		case ar.FailedHunks() > 0:
			pc.Status = PatchFailed
			pc.Failed = failedHunks(ar)
		case ar.Upstreamed() && j.Repo.Patches.Drop():
			pc.Status = PatchDropped
		case ar.Upstreamed():
			pc.Status, pc.Problem = PatchFailed, upstreamedProblem(j.To)
		case ar.Status() == patch.Exact:
			pc.Status = PatchExact
		case ar.Status() == patch.Shifted:
			pc.Status = PatchShifted
		default:
			pc.Status = PatchRefreshed
		}
		rep.Counts[pc.Status]++
		rep.Patches = append(rep.Patches, pc)
	}
	return rep, nil
}

func failedHunks(ar *patch.Result) []FailedHunk {
	var out []FailedHunk
	for _, f := range ar.Files {
		if f.Problem != "" {
			out = append(out, FailedHunk{File: f.Path, Problem: f.Problem})
			continue
		}
		for i, h := range f.Hunks {
			if h.Status != patch.Failed {
				continue
			}
			fh := FailedHunk{File: f.Path, Hunk: i + 1, Line: h.Line}
			if h.Near != nil {
				fh.Near, fh.Matched, fh.Total = h.Near.Line, h.Near.Matched, h.Near.Total
			}
			out = append(out, fh)
		}
	}
	return out
}
