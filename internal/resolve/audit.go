package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/vibeci/vibeci/internal/gitx"
)

// importTree stages the workspace's working tree into a temporary index of
// the trusted mirror and writes a tree object. Only paths that exist in the
// merge (or that the agent declared) are considered, so build artifacts and
// stray files never end up in the commit. The workspace's own .git is never
// consulted.
func (j *Job) importTree(ctx context.Context, ws *workspace, declared []string, n int) (tree string, excluded []string, err error) {
	idx := j.path(fmt.Sprintf("import-%d.index", n))
	os.Remove(idx)
	defer os.Remove(idx)
	o := gitx.Opts{GitDir: j.Mirror.GitDir, WorkTree: ws.Dir, Index: idx, Dir: ws.Dir, LiteralPathspecs: true}
	if _, err := j.G.Run(ctx, o, "read-tree", j.baseline); err != nil {
		return "", nil, err
	}
	root, err := os.OpenRoot(ws.Dir)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()

	candidates := map[string]bool{}
	for _, t := range []map[string]struct{}{keys(j.baselineTree), keys(j.oursTree), keys(j.theirsTree)} {
		for p := range t {
			candidates[p] = true
		}
	}
	declaredSet := map[string]bool{}
	for _, p := range declared {
		clean, err := cleanRel(p)
		if err != nil {
			return "", nil, fmt.Errorf("created_files: %q: %v", p, err)
		}
		candidates[clean] = true
		declaredSet[clean] = true
	}
	var specs []string
	for p := range candidates {
		_, inBase := j.baselineTree[p]
		if inBase {
			specs = append(specs, p)
			continue
		}
		if _, err := root.Lstat(p); err == nil {
			specs = append(specs, p)
		}
	}
	sort.Strings(specs)
	if len(specs) > 0 {
		o.Stdin = []byte(strings.Join(specs, "\x00") + "\x00")
		if _, err := j.G.Run(ctx, o, "add", "--all", "--force", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", nil, fmt.Errorf("staging the working tree failed: %v", err)
		}
		o.Stdin = nil
	}
	out, err := j.G.Run(ctx, o, "ls-files", "--others", "--exclude-standard", "-z")
	if err == nil {
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" && !declaredSet[p] && !strings.HasPrefix(p, ".git/") {
				excluded = append(excluded, p)
			}
		}
	}
	wt, err := j.G.Run(ctx, gitx.Opts{GitDir: j.Mirror.GitDir, Index: idx}, "write-tree")
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(wt)), excluded, nil
}

func keys(m map[string]gitx.TreeEntry) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// cleanRel validates a repository-relative path from the model.
func cleanRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/workspace/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || strings.ContainsAny(p, "\x00\r\n") || filepath.IsAbs(p) {
		return "", errors.New("must be a relative path inside the repository")
	}
	c := filepath.ToSlash(filepath.Clean(p))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", errors.New("must stay inside the repository")
	}
	for _, seg := range strings.Split(c, "/") {
		if strings.EqualFold(seg, ".git") {
			return "", errors.New(".git is managed by the harness")
		}
	}
	return c, nil
}

// novelLine is a line in the result that exists in neither side nor base.
type novelLine struct {
	Line int
	Text string
}

type fileAudit struct {
	Path     string
	Kind     string // conflict description, "changed by agent", "new file"
	Novel    []novelLine
	Dropped  []string
	Markers  bool
	Deleted  bool
	Binary   bool
	resultLn []string
}

// auditReport is the deterministic part of the resolution audit.
type auditReport struct {
	Tree     string
	Files    []*fileAudit
	Novel    int
	Markers  []string
	Gitlinks []string
	Excluded []string
}

func (r *auditReport) droppedTotal() (files []string, lines int) {
	for _, f := range r.Files {
		if len(f.Dropped) > 0 {
			files = append(files, fmt.Sprintf("%s (%d lines)", f.Path, len(f.Dropped)))
			lines += len(f.Dropped)
		}
	}
	return
}

func hasMarkers(s string) bool {
	start, end := false, false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "<<<<<<< ") || l == "<<<<<<<" {
			start = true
		}
		if strings.HasPrefix(l, ">>>>>>> ") || l == ">>>>>>>" {
			end = true
		}
	}
	return start && end
}

func norm(l string) string { return strings.TrimSpace(l) }

func informative(l string) bool {
	for _, r := range l {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func lineSet(contents ...string) map[string]bool {
	set := map[string]bool{}
	for _, c := range contents {
		for _, l := range strings.Split(c, "\n") {
			set[norm(l)] = true
		}
	}
	return set
}

func (j *Job) blob(ctx context.Context, rev, p string) (string, bool) {
	data, found, err := j.Mirror.CatBlob(ctx, rev+":"+p, 8<<20)
	if err != nil || !found {
		return "", false
	}
	return string(data), true
}

func isBinary(s string) bool {
	return strings.IndexByte(s[:min(len(s), 8000)], 0) >= 0
}

// audit compares the imported tree with both sides of the merge.
func (j *Job) audit(ctx context.Context, tree string, conflicts []Conflict) (*auditReport, error) {
	rep := &auditReport{Tree: tree}
	resultTree, err := j.Mirror.LsTree(ctx, tree)
	if err != nil {
		return nil, err
	}
	for p, e := range resultTree {
		if e.Mode == "160000" {
			if o, ok := j.oursTree[p]; !ok || o.OID != e.OID {
				if t, ok := j.theirsTree[p]; !ok || t.OID != e.OID {
					rep.Gitlinks = append(rep.Gitlinks, p)
				}
			}
		}
	}
	conflicted := map[string]Conflict{}
	for _, c := range conflicts {
		conflicted[c.Path] = c
	}
	check := map[string]bool{}
	for p := range conflicted {
		check[p] = true
	}
	for p, e := range resultTree {
		if b, ok := j.baselineTree[p]; !ok || b.OID != e.OID || b.Mode != e.Mode {
			check[p] = true
		}
	}
	for p := range j.baselineTree {
		if _, ok := resultTree[p]; !ok {
			check[p] = true // deleted by the agent
		}
	}
	paths := make([]string, 0, len(check))
	for p := range check {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		fa := &fileAudit{Path: p, Kind: "changed by the agent"}
		c, isConflict := conflicted[p]
		if isConflict {
			fa.Kind = "conflict: " + c.Describe()
		}
		e, inResult := resultTree[p]
		if !inResult {
			fa.Deleted = true
			rep.Files = append(rep.Files, fa)
			continue
		}
		if e.Mode == "160000" || e.Mode == "120000" {
			rep.Files = append(rep.Files, fa)
			continue
		}
		result, _ := j.blob(ctx, tree, p)
		ours, inOurs := j.blob(ctx, j.ForkHead, p)
		theirs, inTheirs := j.blob(ctx, j.Target, p)
		base := ""
		if j.MergeBase != "" {
			base, _ = j.blob(ctx, j.MergeBase, p)
		}
		if !inOurs && !inTheirs {
			fa.Kind = "new file created by the agent"
		}
		if isBinary(result) {
			fa.Binary = true
			rep.Files = append(rep.Files, fa)
			continue
		}
		if (result == ours && inOurs) || (result == theirs && inTheirs) {
			// Identical to one side: nothing authored, no markers introduced.
			if isConflict {
				fa.Dropped = droppedLines(ours, base, result)
				rep.Files = append(rep.Files, fa)
			}
			continue
		}
		if hasMarkers(result) && !hasMarkers(ours) && !hasMarkers(theirs) {
			fa.Markers = true
			rep.Markers = append(rep.Markers, p)
		}
		known := lineSet(ours, theirs, base)
		if !isConflict {
			if b, ok := j.blob(ctx, j.baseline, p); ok {
				for l := range lineSet(b) {
					known[l] = true
				}
			}
		}
		lines := strings.Split(result, "\n")
		fa.resultLn = lines
		for i, l := range lines {
			n := norm(l)
			if n == "" || known[n] || !informative(n) {
				continue
			}
			if strings.HasPrefix(n, "<<<<<<<") || strings.HasPrefix(n, ">>>>>>>") || strings.HasPrefix(n, "|||||||") || n == "=======" {
				continue
			}
			fa.Novel = append(fa.Novel, novelLine{Line: i + 1, Text: l})
		}
		rep.Novel += len(fa.Novel)
		if isConflict {
			fa.Dropped = droppedLines(ours, base, result)
		}
		rep.Files = append(rep.Files, fa)
	}
	return rep, nil
}

// droppedLines returns fork-added lines (in ours, not in base) that are
// missing from the result.
func droppedLines(ours, base, result string) []string {
	if ours == "" {
		return nil
	}
	baseSet := lineSet(base)
	resSet := lineSet(result)
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(ours, "\n") {
		n := norm(l)
		if len(n) < 4 || !informative(n) || baseSet[n] || resSet[n] || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// novelReport renders the agent-authored lines with context for the LLM
// audit.
func (r *auditReport) novelReport(limit int) string {
	var sb strings.Builder
	for _, f := range r.Files {
		if len(f.Novel) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "=== %s (%s): %d new line(s) ===\n", f.Path, f.Kind, len(f.Novel))
		novel := map[int]bool{}
		for _, n := range f.Novel {
			novel[n.Line] = true
		}
		last := 0
		for _, n := range f.Novel {
			from, to := max(1, n.Line-2), min(len(f.resultLn), n.Line+2)
			if from <= last {
				from = last + 1
			} else if last > 0 {
				sb.WriteString("   ...\n")
			}
			for i := from; i <= to; i++ {
				mark := " "
				if novel[i] {
					mark = ">"
				}
				fmt.Fprintf(&sb, "%s%6d  %s\n", mark, i, f.resultLn[i-1])
			}
			last = to
			if sb.Len() > limit {
				sb.WriteString("\n[... report truncated ...]\n")
				return sb.String()
			}
		}
		sb.WriteString("\n")
	}
	for _, f := range r.Files {
		if f.Deleted {
			fmt.Fprintf(&sb, "Deleted by the agent: %s (%s)\n", f.Path, f.Kind)
		}
		if f.Binary {
			fmt.Fprintf(&sb, "Binary file in result changed: %s (%s)\n", f.Path, f.Kind)
		}
	}
	return sb.String()
}

// markerFiles scans the initially conflicted files in the workspace for
// leftover markers (cheap pre-check before importing).
func markerFiles(dir string, conflicts []Conflict) []string {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []string
	for _, c := range conflicts {
		data, err := root.ReadFile(c.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				out = append(out, c.Path+" (unreadable: "+err.Error()+")")
			}
			continue
		}
		if bytes.Contains(data, []byte("<<<<<<< ")) && hasMarkers(string(data)) {
			out = append(out, c.Path)
		}
	}
	return out
}
