package review

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/gitx"
)

// Unit is one commit prepared for review.
type Unit struct {
	Commit  *gitx.Commit
	Base    string
	Merge   bool
	Files   []*gitx.FileChange
	Signals []Signal
	// Rendered is the text shown to the model.
	Rendered string
	// Trivial is set when no model review is needed, with the reason.
	Trivial string
}

// StrongSignals reports whether any strong signal fired.
func (u *Unit) StrongSignals() bool {
	for _, s := range u.Signals {
		if s.Strong {
			return true
		}
	}
	return false
}

type collector struct {
	repo           *gitx.Repo
	emptyTree      string
	maxCommitChars int
	maxFileChars   int
}

func newCollector(ctx context.Context, repo *gitx.Repo, maxCommitChars int) (*collector, error) {
	et, err := repo.EmptyTree(ctx)
	if err != nil {
		return nil, err
	}
	if maxCommitChars < 20000 {
		maxCommitChars = 20000
	}
	return &collector{repo: repo, emptyTree: et, maxCommitChars: maxCommitChars, maxFileChars: min(40000, maxCommitChars/3)}, nil
}

func (c *collector) build(ctx context.Context, cm *gitx.Commit) (*Unit, error) {
	u := &Unit{Commit: cm, Base: c.emptyTree}
	if len(cm.Parents) > 0 {
		u.Base = cm.Parents[0]
	}
	if len(cm.Parents) > 1 {
		return c.buildMerge(ctx, u)
	}
	files, err := c.repo.DiffTree(ctx, u.Base, cm.SHA)
	if err != nil {
		return nil, err
	}
	u.Files = files
	if len(files) == 0 {
		u.Trivial = "commit changes no files"
		u.Rendered = header(cm, "")
		return u, nil
	}
	patch, perr := c.repo.Patch(ctx, u.Base, cm.SHA, 64<<20)
	sections := map[string]*fileDiff{}
	if perr == nil {
		for _, fd := range splitPatch(string(patch)) {
			sections[fd.Path] = fd
		}
	}

	var list strings.Builder
	var body strings.Builder
	var omitted []string
	budget := c.maxCommitChars
	for _, f := range files {
		sigs, desc := c.fileSignals(ctx, f)
		u.Signals = append(u.Signals, sigs...)
		fmt.Fprintf(&list, "  %s\n", desc)

		fd := sections[f.Path]
		if fd == nil && perr != nil && !f.Binary {
			// Whole-commit diff too large: fetch this file alone.
			if p, err := c.repo.Patch(ctx, u.Base, cm.SHA, 8<<20, pathsOf(f)...); err == nil {
				if fds := splitPatch(string(p)); len(fds) > 0 {
					fd = fds[0]
				}
			}
		}
		if fd == nil {
			if !f.Binary && f.Status != 'D' {
				omitted = append(omitted, f.Path+" (diff unavailable: too large)")
			}
			continue
		}
		u.Signals = append(u.Signals, scanAdded(f.Path, fd.Added)...)
		text := fd.Text
		switch {
		case f.Binary:
			continue // listed above; a binary diff carries no reviewable text
		case f.Status == 'D':
			text = fmt.Sprintf("diff for deleted file %s omitted (-%d lines)\n", f.Path, f.Deleted)
		case isLockfile(f.Path):
			text = summarizeLockfile(f, fd)
		case len(text) > c.maxFileChars:
			text = text[:c.maxFileChars] + fmt.Sprintf("\n[... diff of %s truncated: +%d -%d lines in total; use repository tools to see the rest ...]\n", f.Path, f.Added, f.Deleted)
		}
		if len(text) > budget {
			omitted = append(omitted, fmt.Sprintf("%s (+%d -%d)", f.Path, f.Added, f.Deleted))
			continue
		}
		budget -= len(text)
		body.WriteString(text)
	}
	var sb strings.Builder
	sb.WriteString(header(cm, ""))
	sb.WriteString("Files changed:\n")
	sb.WriteString(list.String())
	writeSignals(&sb, u.Signals)
	if len(omitted) > 0 {
		sb.WriteString("Diffs omitted for size (review budget):\n")
		for _, o := range omitted {
			sb.WriteString("  " + o + "\n")
		}
	}
	sb.WriteString("Diff:\n")
	sb.WriteString(body.String())
	u.Rendered = sb.String()
	return u, nil
}

func (c *collector) buildMerge(ctx context.Context, u *Unit) (*Unit, error) {
	u.Merge = true
	out, err := c.repo.RemergeDiff(ctx, u.Commit.SHA, 16<<20)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		u.Trivial = "merge commit adds nothing beyond git's automatic merge of its parents (the merged commits are reviewed individually)"
		u.Rendered = header(u.Commit, "merge")
		return u, nil
	}
	for _, fd := range splitPatch(text + "\n") {
		u.Signals = append(u.Signals, scanAdded(fd.Path, fd.Added)...)
	}
	if len(text) > c.maxCommitChars {
		text = text[:c.maxCommitChars] + "\n[... remerge diff truncated ...]"
	}
	var sb strings.Builder
	sb.WriteString(header(u.Commit, "merge"))
	sb.WriteString("This is a merge commit. Shown is its remerge-diff: the difference between git's automatic merge of the parents and what was actually committed. Anything added here was introduced by the merge itself (an \"evil merge\" hides code exactly here). The merged commits themselves are reviewed separately.\n")
	writeSignals(&sb, u.Signals)
	sb.WriteString("Remerge diff:\n")
	sb.WriteString(text)
	sb.WriteString("\n")
	u.Rendered = sb.String()
	return u, nil
}

func header(cm *gitx.Commit, kind string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Commit: %s\n", cm.SHA)
	if kind == "merge" {
		fmt.Fprintf(&sb, "Parents: %s\n", strings.Join(cm.Parents, " "))
	}
	fmt.Fprintf(&sb, "Author: %s, %s\n", cm.Author, cm.AuthorTime.Format(time.RFC3339))
	if cm.Committer != cm.Author {
		fmt.Fprintf(&sb, "Committer: %s, %s\n", cm.Committer, cm.CommitTime.Format(time.RFC3339))
	}
	if cm.Signed {
		sb.WriteString("Signature: present (not verified)\n")
	}
	fmt.Fprintf(&sb, "Subject: %s\n", cm.Subject)
	if cm.Body != "" {
		body := cm.Body
		if len(body) > 3000 {
			body = body[:3000] + "\n[... message truncated ...]"
		}
		sb.WriteString("Message:\n" + indent(body) + "\n")
	}
	return sb.String()
}

func writeSignals(sb *strings.Builder, sigs []Signal) {
	if len(sigs) == 0 {
		return
	}
	sb.WriteString("Automated signals (heuristic hints, frequently benign):\n")
	for _, s := range sigs {
		sb.WriteString("  - " + s.String() + "\n")
	}
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func pathsOf(f *gitx.FileChange) []string {
	if f.OldPath != "" {
		return []string{f.OldPath, f.Path}
	}
	return []string{f.Path}
}

// fileSignals computes path/mode-level signals and a one-line description.
func (c *collector) fileSignals(ctx context.Context, f *gitx.FileChange) ([]Signal, string) {
	var sigs []Signal
	desc := fmt.Sprintf("%c %s (+%d -%d)", f.Status, f.Path, f.Added, f.Deleted)
	if f.OldPath != "" {
		desc = fmt.Sprintf("%c %s -> %s (+%d -%d)", f.Status, f.OldPath, f.Path, f.Added, f.Deleted)
	}
	if f.Status == 'D' {
		return nil, desc
	}
	if isCI(f.Path) {
		sigs = append(sigs, Signal{Kind: "ci_config", File: f.Path, Detail: "CI/CD configuration changed (runs with repository secrets)"})
	} else if isBuildFile(f.Path) {
		sigs = append(sigs, Signal{Kind: "build_config", File: f.Path, Detail: "build, packaging or install configuration changed"})
	}
	switch f.NewMode {
	case "120000":
		target, _ := c.repo.BlobHead(ctx, f.NewOID, 512)
		t := string(target)
		s := Signal{Kind: "symlink", File: f.Path, Detail: fmt.Sprintf("symlink to %q", clip(t, 200))}
		if strings.HasPrefix(t, "/") || strings.HasPrefix(path.Clean(path.Join(path.Dir(f.Path), t)), "..") {
			s.Detail += " (points outside the repository)"
		}
		sigs = append(sigs, s)
		return sigs, desc + " [symlink]"
	case "160000":
		sigs = append(sigs, Signal{Kind: "submodule", File: f.Path, Detail: "submodule pointer changed to " + f.NewOID})
		return sigs, desc + " [submodule]"
	}
	if f.OldMode != "100755" && f.NewMode == "100755" {
		desc += " [executable]"
	}
	if f.Binary {
		size, _ := c.repo.BlobSize(ctx, f.NewOID)
		head, _ := c.repo.BlobHead(ctx, f.NewOID, 16)
		kind, exe := binaryKind(head)
		desc = fmt.Sprintf("%c %s (binary, %d bytes, %s)", f.Status, f.Path, size, kind)
		verb := "added"
		if f.Status != 'A' {
			verb = "changed"
		}
		sigs = append(sigs, Signal{Kind: "binary_" + verb, File: f.Path, Detail: fmt.Sprintf("%s, %d bytes", kind, size), Strong: exe})
	}
	if f.Added > 3000 {
		sigs = append(sigs, Signal{Kind: "large_change", File: f.Path, Detail: fmt.Sprintf("%d lines added (generated/vendored?)", f.Added)})
	}
	return sigs, desc
}

// summarizeLockfile renders a lockfile diff as the lines that matter for
// supply-chain review (dependency sources), not thousands of hashes.
func summarizeLockfile(f *gitx.FileChange, fd *fileDiff) string {
	var keep []string
	for _, l := range fd.Added {
		if registryRe.MatchString(l) || urlRe.MatchString(l) {
			keep = append(keep, "+"+strings.TrimSpace(l))
		}
	}
	sort.Strings(keep)
	keep = dedupe(keep)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Lockfile %s changed (+%d -%d lines); full diff omitted. Added dependency-source lines (%d):\n", f.Path, f.Added, f.Deleted, len(keep))
	for i, l := range keep {
		if i == 60 {
			fmt.Fprintf(&sb, "  [... %d more ...]\n", len(keep)-60)
			break
		}
		sb.WriteString("  " + clip(l, 300) + "\n")
	}
	return sb.String()
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
