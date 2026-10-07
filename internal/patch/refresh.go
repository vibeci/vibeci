package patch

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Context is the number of context lines in generated hunks.
const Context = 3

// ErrFailed is returned when refreshing a patch that did not apply.
var ErrFailed = errors.New("the patch did not apply")

// Refresh rewrites p so it applies exactly to the tree it was applied to,
// using the result of Apply (with DetectApplied). Sections that applied at
// their recorded lines are kept byte for byte; sections that only moved
// get new line numbers (unless keepOffsets); everything else is
// regenerated from the file states before and after the section, which
// also drops hunks that were already applied. It returns nil when nothing
// is left (the whole patch is already applied).
func Refresh(p *Patch, res *Result, keepOffsets bool) (*Patch, error) {
	if len(res.Files) != len(p.Files) {
		return nil, errors.New("internal error: result does not match the patch")
	}
	style := p.Style()
	out := p.Clone()
	out.Files = out.Files[:0]
	for i, fr := range res.Files {
		f := p.Files[i]
		switch fr.Status {
		case Failed:
			return nil, fmt.Errorf("%w: %s", ErrFailed, fr.Path)
		case Exact:
			out.Files = append(out.Files, f)
			continue
		case Shifted:
			if keepOffsets {
				out.Files = append(out.Files, f)
				continue
			}
			nf := *f
			nf.Header = append([]string(nil), f.Header...)
			nf.Hunks = shiftHunks(f.Hunks, fr.Hunks)
			nf.SetIndex(fr.Before.Data, fr.Before.Exists, fr.After.Data, fr.After.Exists)
			out.Files = append(out.Files, &nf)
			continue
		}
		// Fuzzed or (partly) already applied: regenerate.
		nf, keep := RewriteSection(f, fr.Before, fr.After, style)
		if keep {
			out.Files = append(out.Files, nf)
		}
	}
	if len(out.Files) == 0 {
		return nil, nil
	}
	return out, nil
}

// shiftHunks moves hunks to the lines where they applied.
func shiftHunks(hs []*Hunk, rs []HunkResult) []*Hunk {
	out := make([]*Hunk, len(hs))
	delta := 0
	for i, h := range hs {
		start := rs[i].Line - 1 // 0-based index of the old image
		oldStart, newStart := start, start+delta
		if h.OldCount > 0 {
			oldStart++
		}
		if h.NewCount > 0 {
			newStart++
		}
		out[i] = h.Shift(oldStart, newStart)
		delta += h.NewCount - h.OldCount
	}
	return out
}

// RewriteSection returns a copy of f whose hunks turn before into after,
// keeping its header lines (with an updated index line). keep is false
// when the section no longer changes anything.
func RewriteSection(f *File, before, after FileState, s Style) (nf *File, keep bool) {
	nf = &File{}
	*nf = *f
	nf.Header = append([]string(nil), f.Header...)
	renames := f.RenameFrom != "" || f.CopyFrom != ""
	if before.Equal(after) && !renames {
		return nf, false
	}
	nf.Hunks = Diff(before.Data, after.Data, Context, s)
	nf.NewFile = !before.Exists && after.Exists
	nf.Deleted = before.Exists && !after.Exists
	if len(nf.Hunks) == 0 && !renames && !nf.NewFile && !nf.Deleted {
		return nf, false
	}
	nf.SetIndex(before.Data, before.Exists, after.Data, after.Exists)
	return nf, true
}

// Naming describes how a patch names files, to add sections for files the
// original patch did not touch.
type Naming struct {
	OldPrefix, NewPrefix string // e.g. "a/" and "b/"
	Root                 string // tree directory the names are relative to
	IndexLen             int    // length of the ids on git index lines, 0 if none
}

// Naming infers the file naming of p for the given strip level and root.
func (p *Patch) Naming(strip int, root string) Naming {
	n := Naming{Root: root}
	for _, f := range p.Files {
		if n.OldPrefix == "" && f.OldName != "" && f.OldName != DevNull {
			n.OldPrefix = prefixOf(f.OldName, strip)
		}
		if n.NewPrefix == "" && f.NewName != "" && f.NewName != DevNull {
			n.NewPrefix = prefixOf(f.NewName, strip)
		}
		for _, l := range f.Header {
			if rest, ok := strings.CutPrefix(l, "index "); ok && n.IndexLen == 0 {
				ids, _, _ := strings.Cut(rest, " ")
				if a, _, ok := strings.Cut(ids, ".."); ok {
					n.IndexLen = len(a)
				}
			}
		}
	}
	def := strings.Repeat("a/", strip)
	if n.OldPrefix == "" {
		n.OldPrefix = def
	}
	if n.NewPrefix == "" {
		n.NewPrefix = strings.Repeat("b/", strip)
	}
	return n
}

func prefixOf(name string, strip int) string {
	p := name
	for i := 0; i < strip; i++ {
		j := strings.IndexByte(p, '/')
		if j < 0 {
			return ""
		}
		p = p[j+1:]
	}
	return name[:len(name)-len(p)]
}

// NewSection builds a section for treePath, which the original patch did
// not change, turning before into after.
func NewSection(treePath string, before, after FileState, n Naming, s Style) (*File, error) {
	rel := treePath
	if n.Root != "" {
		r, ok := strings.CutPrefix(treePath, strings.TrimSuffix(n.Root, "/")+"/")
		if !ok {
			return nil, fmt.Errorf("%s is outside %s, the directory this patch's paths are relative to", treePath, n.Root)
		}
		rel = r
	}
	rel = path.Clean(rel)
	f := &File{OldName: n.OldPrefix + rel, NewName: n.NewPrefix + rel, Git: s.Git}
	if s.Git {
		f.Header = append(f.Header, "diff --git "+quoteName(n.OldPrefix+rel)+" "+quoteName(n.NewPrefix+rel))
	}
	switch {
	case !before.Exists && !after.Exists:
		return nil, fmt.Errorf("%s does not exist before or after", treePath)
	case !before.Exists:
		f.NewFile, f.OldName = true, DevNull
		if s.Git {
			f.Header = append(f.Header, "new file mode 100644")
		}
	case !after.Exists:
		f.Deleted, f.NewName = true, DevNull
		if s.Git {
			f.Header = append(f.Header, "deleted file mode 100644")
		}
	}
	if n.IndexLen > 0 {
		mode := " 100644"
		if f.NewFile || f.Deleted {
			mode = ""
		}
		f.Header = append(f.Header, "index "+zeroID[:n.IndexLen]+".."+zeroID[:n.IndexLen]+mode)
	}
	f.Header = append(f.Header, "--- "+quoteName(f.OldName), "+++ "+quoteName(f.NewName))
	f.Hunks = Diff(before.Data, after.Data, Context, s)
	f.SetIndex(before.Data, before.Exists, after.Data, after.Exists)
	return f, nil
}

// quoteName C-quotes names with characters git would quote.
func quoteName(s string) string {
	if s == DevNull {
		return s
	}
	need := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c == 0x7f {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
