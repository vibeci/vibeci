package resolve

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
)

// Conflict is one conflicted path as git reports it after the merge.
type Conflict struct {
	Path string
	Code string // porcelain XY: UU, AA, DU, UD, AU, UA, DD
}

// Describe explains the conflict kind.
func (c Conflict) Describe() string {
	switch c.Code {
	case "UU":
		return "both modified"
	case "AA":
		return "both added"
	case "DU":
		return "deleted by the fork, modified upstream"
	case "UD":
		return "modified by the fork, deleted upstream"
	case "AU":
		return "added by the fork"
	case "UA":
		return "added upstream"
	case "DD":
		return "both deleted"
	}
	return c.Code
}

// workspace is a scratch clone in which the merge agent works. It is created
// by the harness (trusted moment), then handed to a sandbox; afterwards the
// harness never runs git inside it again (its .git may have been tampered
// with) and only reads its working tree through a separate, trusted git dir.
type workspace struct {
	Dir       string // absolute path
	Rel       string // relative to data dir, for the sandbox broker
	Conflicts []Conflict
}

func (j *Job) relPath(abs string) string {
	rel, _ := filepath.Rel(j.DataDir, abs)
	return filepath.ToSlash(rel)
}

func (j *Job) objectsRel() string {
	return j.relPath(filepath.Join(j.Mirror.GitDir, "objects"))
}

// newClone creates an empty repository at dir that borrows objects from the
// trusted mirror via alternates and has the fork/upstream refs (and upstream
// tags) for context.
func (j *Job) newClone(ctx context.Context, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: dir}, "init", "--quiet"); err != nil {
		return err
	}
	alt := filepath.Join(dir, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(j.Mirror.GitDir, "objects")+"\n"), 0o644); err != nil {
		return err
	}
	o := gitx.Opts{Dir: dir}
	ours, theirs := j.refNames()
	refs := map[string]string{"refs/heads/" + ours: j.ForkHead, "refs/heads/" + theirs: j.Target}
	switch {
	case j.sub != nil:
		refs["refs/heads/excluded"] = j.sub.SHA
	case j.Mode == ModeRemove:
		refs["refs/heads/excluded"] = j.modeCommit.SHA
	case j.Mode == ModeRestore:
		refs["refs/heads/restored"] = j.modeCommit.SHA
	}
	for ref, sha := range refs {
		if _, err := j.G.Run(ctx, o, "update-ref", ref, sha); err != nil {
			return err
		}
	}
	// Upstream tags help builds that run `git describe`.
	if _, err := j.G.Run(ctx, o, "fetch", "--quiet", "--no-write-fetch-head", "--end-of-options", j.Mirror.GitDir, "+refs/vibeci/upstream-tags/*:refs/tags/*"); err != nil {
		j.logger().Debug("copying tags into workspace", "err", err)
	}
	return nil
}

// refNames are the workspace branch names of the two sides.
func (j *Job) refNames() (ours, theirs string) {
	if j.sub != nil {
		return "upstream", "revert"
	}
	return "fork", "upstream"
}

// prepareMerge creates the agent workspace with the merge in progress.
func (j *Job) prepareMerge(ctx context.Context, name string) (*workspace, error) {
	dir := j.path(name)
	if err := j.newClone(ctx, dir); err != nil {
		return nil, err
	}
	o := gitx.Opts{Dir: dir}
	ours, theirs := j.refNames()
	if _, err := j.G.Run(ctx, o, "checkout", "--quiet", "-b", "vibeci-merge", ours); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Merge %s %s", theirs, short(j.Target))
	_, err := j.G.Run(ctx, gitx.Opts{Dir: dir, AllowExit: []int{1}}, "merge", "--no-ff", "--no-commit", "-m", msg, theirs)
	if err != nil && gitx.ExitCode(err) != 1 {
		return nil, fmt.Errorf("merge in workspace: %w", err)
	}
	if err := j.applyKeepOursWorkspace(ctx, dir, ours); err != nil {
		return nil, err
	}
	conflicts, err := statusConflicts(ctx, j.G, dir)
	if err != nil {
		return nil, err
	}
	return &workspace{Dir: dir, Rel: j.relPath(dir), Conflicts: conflicts}, nil
}

// prepareCheckout creates a workspace with commit checked out (for
// clean-room verification and semantic-fix mode).
func (j *Job) prepareCheckout(ctx context.Context, name, commit string) (*workspace, error) {
	dir := j.path(name)
	if err := j.newClone(ctx, dir); err != nil {
		return nil, err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: dir}, "checkout", "--quiet", "--detach", commit); err != nil {
		return nil, err
	}
	return &workspace{Dir: dir, Rel: j.relPath(dir)}, nil
}

func statusConflicts(ctx context.Context, g *gitx.Git, dir string) ([]Conflict, error) {
	out, err := g.Run(ctx, gitx.Opts{Dir: dir}, "status", "--porcelain=v2", "-z", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	var cs []Conflict
	for _, rec := range bytes.Split(out, []byte{0}) {
		s := string(rec)
		if !strings.HasPrefix(s, "u ") {
			continue
		}
		// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
		f := strings.SplitN(s, " ", 11)
		if len(f) == 11 {
			cs = append(cs, Conflict{Path: f[10], Code: f[1]})
		}
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].Path < cs[b].Path })
	return cs, nil
}

// keepOursPaths returns the paths of the given trees matching keep_ours.
func (j *Job) keepOursPaths(trees ...map[string]gitx.TreeEntry) []string {
	if len(j.Repo.KeepOurs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range trees {
		for p := range t {
			if !seen[p] && config.MatchAny(j.Repo.KeepOurs, p) {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (j *Job) applyKeepOursWorkspace(ctx context.Context, dir, ours string) error {
	if len(j.keepOurs) == 0 {
		return nil
	}
	var restore, remove []string
	for _, p := range j.keepOurs {
		if _, ok := j.oursTree[p]; ok {
			restore = append(restore, p)
		} else {
			remove = append(remove, p)
		}
	}
	o := gitx.Opts{Dir: dir, LiteralPathspecs: true}
	if len(restore) > 0 {
		o.Stdin = []byte(strings.Join(restore, "\x00") + "\x00")
		if _, err := j.G.Run(ctx, o, "checkout", ours, "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return fmt.Errorf("keep_ours restore: %w", err)
		}
	}
	if len(remove) > 0 {
		o.Stdin = []byte(strings.Join(remove, "\x00") + "\x00")
		if _, err := j.G.Run(ctx, o, "rm", "--quiet", "--force", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return fmt.Errorf("keep_ours remove: %w", err)
		}
	}
	return nil
}

// applyKeepOursTree rewrites a tree in the trusted mirror so keep_ours paths
// carry the fork's version (or are absent if the fork does not have them).
func (j *Job) applyKeepOursTree(ctx context.Context, tree string) (string, error) {
	if len(j.keepOurs) == 0 {
		return tree, nil
	}
	idx := j.path("keepours.index")
	defer os.Remove(idx)
	o := gitx.Opts{GitDir: j.Mirror.GitDir, Index: idx}
	if _, err := j.G.Run(ctx, o, "read-tree", tree); err != nil {
		return "", err
	}
	var info strings.Builder
	zero := strings.Repeat("0", len(j.ForkHead))
	for _, p := range j.keepOurs {
		if e, ok := j.oursTree[p]; ok {
			fmt.Fprintf(&info, "%s %s\t%s\x00", e.Mode, e.OID, p)
		} else {
			fmt.Fprintf(&info, "0 %s\t%s\x00", zero, p)
		}
	}
	o.Stdin = []byte(info.String())
	if _, err := j.G.Run(ctx, o, "update-index", "-z", "--index-info"); err != nil {
		return "", fmt.Errorf("keep_ours: %w", err)
	}
	out, err := j.G.Run(ctx, gitx.Opts{GitDir: j.Mirror.GitDir, Index: idx}, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
