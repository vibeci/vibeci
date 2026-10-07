package patch

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Format renders the patch. Parsed parts that were not changed are
// reproduced byte for byte.
func (p *Patch) Format() []byte {
	var b strings.Builder
	b.WriteString(p.Preamble)
	for _, f := range p.Files {
		for _, l := range f.Header {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		for _, h := range f.Hunks {
			h.format(&b)
		}
	}
	b.WriteString(p.Trailer)
	out := b.String()
	if p.noFinalNewline {
		out = strings.TrimSuffix(out, "\n")
	}
	return []byte(out)
}

func (h *Hunk) format(b *strings.Builder) {
	if h.rawHeader != "" && h.OldStart == h.origOldStart && h.NewStart == h.origNewStart {
		b.WriteString(h.rawHeader)
	} else {
		b.WriteString(h.Header())
		b.WriteByte('\n')
	}
	if h.rawBody != "" {
		b.WriteString(h.rawBody)
		return
	}
	for _, l := range h.Lines {
		b.WriteByte(l.Op)
		b.WriteString(l.Text)
		b.WriteByte('\n')
		if l.NoNewline {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
}

// Text renders the hunk as it appears in a patch.
func (h *Hunk) Text() string {
	var b strings.Builder
	h.format(&b)
	return b.String()
}

// Header returns the "@@ ... @@" line (without newline).
func (h *Hunk) Header() string {
	return "@@ -" + hunkRange(h.OldStart, h.OldCount) + " +" + hunkRange(h.NewStart, h.NewCount) + " @@" + h.Section
}

func hunkRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(count)
}

// Shift returns a copy of the hunk that starts at different lines but is
// otherwise unchanged (its body is kept byte for byte).
func (h *Hunk) Shift(oldStart, newStart int) *Hunk {
	c := *h
	c.Lines = append([]Line(nil), h.Lines...)
	c.OldStart, c.NewStart = oldStart, newStart
	return &c
}

// Clone returns a deep copy of the patch.
func (p *Patch) Clone() *Patch {
	c := *p
	c.Files = make([]*File, len(p.Files))
	for i, f := range p.Files {
		fc := *f
		fc.Header = append([]string(nil), f.Header...)
		fc.Hunks = make([]*Hunk, len(f.Hunks))
		for j, h := range f.Hunks {
			hc := *h
			hc.Lines = append([]Line(nil), h.Lines...)
			fc.Hunks[j] = &hc
		}
		c.Files[i] = &fc
	}
	return &c
}

// Style describes how a patch writes the text after the "@@" (the function
// context of diff -p and git diff).
type Style struct {
	// SectionWidth is the maximum length of the section text: 40 for GNU
	// diff -p (quilt), 80 for git, 0 for none.
	SectionWidth int
	// Git writes new file sections with "diff --git" headers.
	Git bool
}

// Style infers the conventions a patch was written with.
func (p *Patch) Style() Style {
	s := Style{}
	sections := 0
	for _, f := range p.Files {
		if f.Git {
			s.Git = true
		}
		for _, h := range f.Hunks {
			if t := strings.TrimSpace(h.Section); t != "" {
				sections++
				if len(t) > 40 {
					s.SectionWidth = 80
				}
			}
		}
	}
	switch {
	case sections == 0:
		s.SectionWidth = 0
	case s.SectionWidth == 0 && s.Git:
		s.SectionWidth = 80
	case s.SectionWidth == 0:
		s.SectionWidth = 40
	}
	return s
}

// funcLine reports whether a line starts a "function" for the section
// text: it begins with a letter, '_' or '$' (the default of both diff -p
// and git).
func funcLine(l string) bool {
	if l == "" {
		return false
	}
	c := l[0]
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// sectionFor returns the section text for a hunk whose old lines start at
// index first (0-based) of lines.
func sectionFor(lines []string, first int, s Style) string {
	if s.SectionWidth <= 0 {
		return ""
	}
	for i := min(first, len(lines)) - 1; i >= 0; i-- {
		l := lines[i]
		if !funcLine(l) {
			continue
		}
		if len(l) > s.SectionWidth {
			l = l[:s.SectionWidth]
		}
		l = strings.TrimRight(l, " \t\r\f\v")
		if l == "" {
			return ""
		}
		return " " + l
	}
	return ""
}

// BlobID returns the git object id of a blob with this content (SHA-1).
func BlobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

const zeroID = "0000000000000000000000000000000000000000"

// SetIndex rewrites the section's git "index <old>..<new>" line for new
// file contents (exists false: the side is absent), keeping the original
// abbreviation length and mode. Sections without an index line are left
// alone.
func (f *File) SetIndex(before []byte, beforeExists bool, after []byte, afterExists bool) {
	for i, l := range f.Header {
		rest, ok := strings.CutPrefix(l, "index ")
		if !ok {
			continue
		}
		ids, mode, _ := strings.Cut(rest, " ")
		a, b, ok := strings.Cut(ids, "..")
		if !ok {
			continue
		}
		n := max(len(a), len(b), 7)
		id := func(data []byte, exists bool) string {
			if !exists {
				return zeroID[:n]
			}
			return BlobID(data)[:min(n, 40)]
		}
		line := "index " + id(before, beforeExists) + ".." + id(after, afterExists)
		if mode != "" {
			line += " " + mode
		}
		f.Header[i] = line
		return
	}
}
