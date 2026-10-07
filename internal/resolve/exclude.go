package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/vibeci/vibeci/internal/gitx"
)

// Exclusion is an upstream commit whose changes are removed from the merge
// target before merging (the review gate blocked it, or an operator
// excluded it).
type Exclusion struct {
	Commit string
	// Reason is shown to the merge agent and recorded in commit messages.
	Reason string
}

// Quarantined is a commit whose distinctive content (added lines that did
// not exist anywhere before it, and binary files it added) must not appear
// in a merge result.
type Quarantined struct {
	Commit string
	// Except is a commit whose content is exempt: lines and files it
	// already contains are not violations (usually the fork head, so a
	// merge never has to remove what the fork already had). Empty means
	// nothing is exempt.
	Except string
}

// Violation is quarantined content found in a merge result.
type Violation struct {
	Commit string `json:"commit"`
	Path   string `json:"path"`
	Line   string `json:"line,omitempty"` // empty for a binary file
}

// Job modes.
const (
	// ModeMerge merges Target (an upstream commit), minus Exclude.
	ModeMerge = ""
	// ModeRemove undoes Target, a commit ForkHead already contains.
	ModeRemove = "remove"
	// ModeRestore re-applies Target, a commit whose changes ForkHead removed.
	ModeRestore = "restore"
)

// Trailers recorded on VibeCI-made commits.
const (
	TrailerJob      = "VibeCI-Job"
	TrailerExcluded = "VibeCI-Excluded"
	TrailerRestored = "VibeCI-Restored"
)

const (
	minFingerprintLen   = 8
	maxFingerprintLines = 20000
	maxViolations       = 500
)

// fingerprint is the distinctive content of one commit.
type fingerprint struct {
	lines map[string]bool // normalized lines
	blobs map[string]bool // binary blob ids
}

func (j *Job) commitInfo(ctx context.Context, sha string) (*gitx.Commit, error) {
	if c := j.commits[sha]; c != nil {
		return c, nil
	}
	cs, err := j.Mirror.ReadCommits(ctx, []string{sha})
	if err != nil {
		return nil, err
	}
	if j.commits == nil {
		j.commits = map[string]*gitx.Commit{}
	}
	j.commits[sha] = cs[0]
	return cs[0], nil
}

// synthetic creates the commit that, merged into a history containing c,
// undoes c (revert: c's parent's tree on top of c), or that, merged into a
// history containing c's parent, re-applies c (restore: c's tree on top of
// its parent). Merge commits are undone and re-applied relative to their
// first parent.
func (j *Job) synthetic(ctx context.Context, c *gitx.Commit, restore bool) (string, error) {
	if len(c.Parents) == 0 {
		return "", fmt.Errorf("upstream commit %s has no parent; it cannot be excluded or restored", short(c.SHA))
	}
	parent := c.Parents[0]
	if restore {
		msg := fmt.Sprintf("Re-apply upstream commit %s (%s)\n\nSynthetic commit made by VibeCI: the tree of %s on top of its parent, so that\nmerging it re-applies the commit.\n\n%s: %s\n%s: %s\n",
			short(c.SHA), clip(c.Subject, 100), c.SHA, TrailerRestored, c.SHA, TrailerJob, j.ID)
		return j.Mirror.CommitTree(ctx, c.Tree, []string{parent}, msg)
	}
	ptree, err := j.Mirror.ResolveObject(ctx, parent+"^{tree}")
	if err != nil {
		return "", err
	}
	if ptree == "" {
		return "", fmt.Errorf("parent of %s is missing from the mirror", short(c.SHA))
	}
	msg := fmt.Sprintf("Revert upstream commit %s (%s)\n\nSynthetic revert made by VibeCI: the tree of the commit's parent %s on top of\nthe commit, so that merging it removes the commit's changes.\n\n%s: %s\n%s: %s\n",
		short(c.SHA), clip(c.Subject, 100), short(parent), TrailerExcluded, c.SHA, TrailerJob, j.ID)
	return j.Mirror.CommitTree(ctx, ptree, []string{c.SHA}, msg)
}

// cleanTarget removes the excluded commits from the upstream target,
// newest first: each revert is merged in the object store when it applies
// cleanly, otherwise a sub-job lets the merge agent resolve it. The result
// contains the original target (so later merges see the excluded commits
// as merged) but none of their changes.
func (j *Job) cleanTarget(ctx context.Context) (string, error) {
	cur := j.Target
	for i := len(j.Exclude) - 1; i >= 0; i-- {
		x := j.Exclude[i]
		if !gitx.IsHex(x.Commit) {
			return "", fmt.Errorf("invalid excluded commit %q", x.Commit)
		}
		c, err := j.commitInfo(ctx, x.Commit)
		if err != nil {
			return "", err
		}
		if ok, err := j.Mirror.IsAncestor(ctx, c.SHA, cur); err != nil {
			return "", err
		} else if !ok {
			return "", fmt.Errorf("excluded commit %s is not part of the upstream being merged", short(c.SHA))
		}
		if ok, err := j.Mirror.IsAncestor(ctx, c.SHA, j.ForkHead); err != nil {
			return "", err
		} else if ok {
			return "", fmt.Errorf("excluded commit %s is already part of the fork", short(c.SHA))
		}
		rev, err := j.synthetic(ctx, c, false)
		if err != nil {
			return "", err
		}
		mt, err := j.Mirror.MergeTree(ctx, cur, rev)
		if err != nil {
			return "", err
		}
		if mt.Clean {
			if cur, err = j.Mirror.CommitTree(ctx, mt.Tree, []string{cur, rev}, j.exclusionMessage(c, x.Reason, "", "", nil)); err != nil {
				return "", err
			}
			j.logger().Info("excluded upstream commit", "repo", j.Repo.Name, "commit", short(c.SHA), "how", "clean revert")
			j.removed = append(j.removed, c.SHA)
			continue
		}
		j.logger().Info("reverting an excluded commit conflicts with later upstream changes; starting agent", "repo", j.Repo.Name, "commit", short(c.SHA), "conflicts", len(mt.ConflictedPaths()))
		sub := &Job{
			ID: j.ID, Repo: j.Repo, DataDir: j.DataDir, JobDir: j.JobDir,
			G: j.G, Mirror: j.Mirror, Sandbox: j.Sandbox,
			ForkHead: cur, Target: rev, TargetLabel: "revert of " + short(c.SHA),
			Models: j.Models, Auditor: j.Auditor, BlockOn: j.BlockOn, MinConfidence: j.MinConfidence,
			Quarantine: j.Quarantine, Logger: j.Logger,
			prefix:  fmt.Sprintf("x%d-", i+1),
			sub:     c,
			subWhy:  x.Reason,
			parent:  j,
			fps:     j.fps,
			commits: j.commits,
		}
		res, err := sub.Run(ctx)
		j.usage.Add(sub.usage)
		if err != nil {
			return "", fmt.Errorf("removing excluded upstream commit %s: %w", short(c.SHA), err)
		}
		cur = res.Commit
		j.removed = append(j.removed, c.SHA)
		j.subResults = append(j.subResults, res)
	}
	return cur, nil
}

// exclusionMessage is the commit message of a merge that removes c from
// the upstream side.
func (j *Job) exclusionMessage(c *gitx.Commit, reason, summary, model string, conflicts []string) string {
	var sb strings.Builder
	root := j
	if j.parent != nil {
		root = j.parent
	}
	label := root.label()
	fmt.Fprintf(&sb, "Exclude %s (%s) from upstream %s\n\n", short(c.SHA), clip(c.Subject, 80), label)
	fmt.Fprintf(&sb, "Upstream commit %s is excluded from %s: its changes are removed\nbefore upstream is merged.\n", c.SHA, j.Repo.Name)
	if r := strings.TrimSpace(reason); r != "" {
		sb.WriteString("Reason: " + wrap(clip(r, 1500), 72) + "\n")
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(&sb, "Conflicts with later upstream changes resolved by VibeCI (%s): %s\n", model, strings.Join(limitList(conflicts, 20), ", "))
	}
	if s := strings.TrimSpace(summary); s != "" {
		sb.WriteString("\n" + wrap(clip(s, 4000), 72) + "\n")
	}
	fmt.Fprintf(&sb, "\n%s: %s\n%s: %s\n", TrailerExcluded, c.SHA, TrailerJob, j.ID)
	return sb.String()
}

func limitList(list []string, n int) []string {
	list = append([]string(nil), list...)
	sort.Strings(list)
	if len(list) > n {
		return append(list[:n:n], fmt.Sprintf("(+%d more)", len(list)-n))
	}
	return list
}

// fingerprint returns the distinctive content of commit sha (relative to
// its first parent).
func (j *Job) fingerprint(ctx context.Context, sha string) (*fingerprint, error) {
	if fp := j.fps[sha]; fp != nil {
		return fp, nil
	}
	c, err := j.commitInfo(ctx, sha)
	if err != nil {
		return nil, err
	}
	fp := &fingerprint{lines: map[string]bool{}, blobs: map[string]bool{}}
	if len(c.Parents) == 0 {
		return nil, fmt.Errorf("quarantined commit %s has no parent", short(sha))
	}
	parent := c.Parents[0]
	patch, err := j.G.Run(ctx, gitx.Opts{GitDir: j.Mirror.GitDir, MaxOut: 64 << 20, Truncate: true},
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M", "--unified=0", "--end-of-options", parent, sha, "--")
	if err != nil {
		return nil, err
	}
	for _, l := range addedLines(patch) {
		n := norm(l)
		if len(n) < minFingerprintLen || !informative(n) || isMarkerLine(n) {
			continue
		}
		fp.lines[n] = true
		if len(fp.lines) >= maxFingerprintLines {
			break
		}
	}
	// Lines that existed anywhere before the commit are not distinctive.
	present, err := j.grepLines(ctx, parent, fp.lines)
	if err != nil {
		return nil, err
	}
	for l := range present {
		delete(fp.lines, l)
	}
	changes, err := j.Mirror.DiffTree(ctx, parent, sha)
	if err != nil {
		return nil, err
	}
	var parentOIDs map[string]bool
	for _, ch := range changes {
		if !ch.Binary || ch.Status == 'D' || ch.NewMode == "160000" {
			continue
		}
		if parentOIDs == nil {
			tree, err := j.Mirror.LsTree(ctx, parent)
			if err != nil {
				return nil, err
			}
			parentOIDs = map[string]bool{}
			for _, e := range tree {
				parentOIDs[e.OID] = true
			}
		}
		if !parentOIDs[ch.NewOID] {
			fp.blobs[ch.NewOID] = true
		}
	}
	if j.fps == nil {
		j.fps = map[string]*fingerprint{}
	}
	j.fps[sha] = fp
	return fp, nil
}

func isMarkerLine(n string) bool {
	return strings.HasPrefix(n, "<<<<<<<") || strings.HasPrefix(n, ">>>>>>>") || strings.HasPrefix(n, "|||||||") || n == "======="
}

// addedLines returns the added lines of a unified diff, tracking hunk
// lengths so content that looks like diff headers is not misread.
func addedLines(patch []byte) []string {
	var out []string
	oldLeft, newLeft := 0, 0
	for _, line := range strings.Split(string(patch), "\n") {
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(line, "+"):
				out = append(out, line[1:])
				newLeft--
			case strings.HasPrefix(line, "-"):
				oldLeft--
			case strings.HasPrefix(line, " ") || line == "":
				oldLeft--
				newLeft--
			case strings.HasPrefix(line, `\`): // "\ No newline at end of file"
			default:
				oldLeft, newLeft = 0, 0
			}
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			oldLeft, newLeft = parseHunkHeader(line)
		}
	}
	return out
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@" into (b, d).
func parseHunkHeader(h string) (oldN, newN int) {
	f := strings.Fields(h)
	if len(f) < 3 {
		return 0, 0
	}
	count := func(s string) int {
		_, n, ok := strings.Cut(s[1:], ",")
		if !ok {
			return 1
		}
		v, err := strconv.Atoi(n)
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	if !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return 0, 0
	}
	return count(f[1]), count(f[2])
}

func (j *Job) writePatterns(lines map[string]bool) (string, error) {
	pats := make([]string, 0, len(lines))
	for l := range lines {
		if l != "" && !strings.ContainsAny(l, "\n\x00") {
			pats = append(pats, l)
		}
	}
	sort.Strings(pats)
	if err := os.MkdirAll(j.JobDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(j.JobDir, ".patterns-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(pats, "\n") + "\n"); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// grepHit is one line of a tree that matches a pattern exactly (after
// normalization).
type grepHit struct {
	path, line string
}

// grep searches treeish for lines whose normalized form is in lines. git
// grep -F finds substring matches; exact matches are filtered here.
func (j *Job) grep(ctx context.Context, treeish string, lines map[string]bool, withPaths bool) ([]grepHit, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	pf, err := j.writePatterns(lines)
	if err != nil {
		return nil, err
	}
	defer os.Remove(pf)
	args := []string{"grep", "--no-color", "-F", "-I", "--full-name", "-f", pf}
	if withPaths {
		args = append(args, "--null")
	} else {
		args = append(args, "-h")
	}
	args = append(args, treeish, "--")
	out, err := j.G.Run(ctx, gitx.Opts{GitDir: j.Mirror.GitDir, AllowExit: []int{1}, MaxOut: 64 << 20, Truncate: true}, args...)
	if err != nil && gitx.ExitCode(err) != 1 {
		return nil, fmt.Errorf("git grep: %w", err)
	}
	var hits []grepHit
	prefix := []byte(treeish + ":")
	for len(out) > 0 {
		var p string
		if withPaths {
			nul := bytes.IndexByte(out, 0)
			if nul < 0 {
				break
			}
			p = string(bytes.TrimPrefix(out[:nul], prefix))
			out = out[nul+1:]
		}
		nl := bytes.IndexByte(out, '\n')
		var content []byte
		if nl < 0 {
			content, out = out, nil
		} else {
			content, out = out[:nl], out[nl+1:]
		}
		if n := norm(string(content)); lines[n] {
			hits = append(hits, grepHit{path: p, line: n})
		}
	}
	return hits, nil
}

// grepLines returns which of lines occur (normalized, exactly) in treeish.
func (j *Job) grepLines(ctx context.Context, treeish string, lines map[string]bool) (map[string]bool, error) {
	hits, err := j.grep(ctx, treeish, lines, false)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, h := range hits {
		found[h.line] = true
	}
	return found, nil
}

// violations finds quarantined content in treeish.
func (j *Job) violations(ctx context.Context, treeish string) ([]Violation, error) {
	if len(j.Quarantine) == 0 {
		return nil, nil
	}
	byLine := map[string][]Quarantined{}
	byBlob := map[string][]Quarantined{}
	all := map[string]bool{}
	for _, q := range j.Quarantine {
		fp, err := j.fingerprint(ctx, q.Commit)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			j.logger().Warn("cannot fingerprint quarantined commit; skipping it", "repo", j.Repo.Name, "commit", short(q.Commit), "err", err)
			continue
		}
		for l := range fp.lines {
			byLine[l] = append(byLine[l], q)
			all[l] = true
		}
		for b := range fp.blobs {
			byBlob[b] = append(byBlob[b], q)
		}
	}
	hits, err := j.grep(ctx, treeish, all, true)
	if err != nil {
		return nil, err
	}
	var out []Violation
	seen := map[Violation]bool{}
	add := func(v Violation) {
		if !seen[v] && len(out) < maxViolations {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, h := range hits {
		for _, q := range byLine[h.line] {
			if q.Except != "" {
				ex, err := j.exceptLineSet(ctx, q.Except, all)
				if err != nil {
					return nil, err
				}
				if ex[h.line] {
					continue
				}
			}
			add(Violation{Commit: q.Commit, Path: h.path, Line: h.line})
		}
	}
	if len(byBlob) > 0 {
		tree, err := j.Mirror.LsTree(ctx, treeish)
		if err != nil {
			return nil, err
		}
		for p, e := range tree {
			for _, q := range byBlob[e.OID] {
				if q.Except != "" {
					ex, err := j.exceptBlobSet(ctx, q.Except)
					if err != nil {
						return nil, err
					}
					if ex[e.OID] {
						continue
					}
				}
				add(Violation{Commit: q.Commit, Path: p})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Path != out[b].Path {
			return out[a].Path < out[b].Path
		}
		return out[a].Line < out[b].Line
	})
	return out, nil
}

func (j *Job) exceptLineSet(ctx context.Context, rev string, all map[string]bool) (map[string]bool, error) {
	if s, ok := j.exceptLines[rev]; ok {
		return s, nil
	}
	s, err := j.grepLines(ctx, rev, all)
	if err != nil {
		return nil, err
	}
	if j.exceptLines == nil {
		j.exceptLines = map[string]map[string]bool{}
	}
	j.exceptLines[rev] = s
	return s, nil
}

func (j *Job) exceptBlobSet(ctx context.Context, rev string) (map[string]bool, error) {
	if s, ok := j.exceptBlobs[rev]; ok {
		return s, nil
	}
	tree, err := j.Mirror.LsTree(ctx, rev)
	if err != nil {
		return nil, err
	}
	s := map[string]bool{}
	for _, e := range tree {
		s[e.OID] = true
	}
	if j.exceptBlobs == nil {
		j.exceptBlobs = map[string]map[string]bool{}
	}
	j.exceptBlobs[rev] = s
	return s, nil
}

// violationText renders violations for the merge agent.
func violationText(vs []Violation, limit int) string {
	var sb strings.Builder
	for i, v := range vs {
		if i == limit {
			fmt.Fprintf(&sb, "  (+%d more)\n", len(vs)-limit)
			break
		}
		if v.Line == "" {
			fmt.Fprintf(&sb, "  %s: binary file added by excluded commit %s\n", v.Path, short(v.Commit))
		} else {
			fmt.Fprintf(&sb, "  %s: %s   [from excluded commit %s]\n", v.Path, clip(v.Line, 200), short(v.Commit))
		}
	}
	return sb.String()
}
