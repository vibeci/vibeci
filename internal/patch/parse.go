// Package patch parses, applies, generates and rewrites unified diffs the
// way GNU patch and quilt treat them. VibeCI uses it to maintain forks that
// are a series of patch files against an upstream source tree (Chromium
// derivatives such as ungoogled-chromium or Brave, distribution packages):
// when upstream moves, every patch is re-applied, and patches that no
// longer apply exactly are rewritten.
//
// Parsing keeps every byte that is not a hunk (descriptions, mail headers,
// diffstats, git extended headers), so a rewritten patch differs from the
// original only in the hunks that changed.
package patch

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Patch is a parsed patch file.
type Patch struct {
	// Preamble is the text before the first file section (description,
	// mail headers, diffstat), kept verbatim.
	Preamble string
	Files    []*File
	// Trailer is the text after the last hunk, e.g. a format-patch
	// signature, kept verbatim.
	Trailer string

	noFinalNewline bool
}

// File is the part of a patch that changes one file.
type File struct {
	// Header holds the raw lines before the first hunk, without newlines:
	// text between sections, "diff --git", extended headers, "---", "+++".
	Header []string
	// OldName and NewName are the names on the "---" and "+++" lines (or
	// on the "diff --git" line), "/dev/null" for a created or deleted
	// file, "" if the section has neither.
	OldName, NewName string
	// Git is set for sections with a "diff --git" line.
	Git     bool
	NewFile bool
	Deleted bool
	// RenameFrom/RenameTo and CopyFrom/CopyTo are git rename and copy
	// paths (relative to the diff root, without the a/ b/ prefixes).
	RenameFrom, RenameTo string
	CopyFrom, CopyTo     string
	// Binary is set for "GIT binary patch" and "Binary files ... differ"
	// sections. VibeCI cannot apply them; they are kept verbatim.
	Binary bool
	Hunks  []*Hunk
}

// Hunk is one "@@" block.
type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	// Section is the text after the closing "@@", including its leading
	// space (usually a function name); "" if none.
	Section string
	Lines   []Line

	// The hunk as it appeared in the patch, so unchanged hunks are
	// reproduced byte for byte. Empty for generated hunks.
	rawHeader, rawBody         string
	origOldStart, origNewStart int
}

// Line is one line of a hunk.
type Line struct {
	// Op is ' ' (context), '-' (removed) or '+' (added).
	Op   byte
	Text string // without the newline
	// NoNewline marks the last line of a file that has no trailing newline
	// ("\ No newline at end of file" follows it in the patch).
	NoNewline bool
}

// DevNull is the name of the missing side of a created or deleted file.
const DevNull = "/dev/null"

var hunkRe = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(.*)$`)

// splitLines splits text into lines without their newlines. eol reports
// whether the last line ended with a newline (true for empty text).
func splitLines(s string) (lines []string, eol bool) {
	if s == "" {
		return nil, true
	}
	lines = strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1], true
	}
	return lines, false
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// Parse parses a patch file: unified diffs as written by diff -u, quilt,
// git diff and git format-patch.
func Parse(data []byte) (*Patch, error) {
	lines, eol := splitLines(string(data))
	p := &Patch{noFinalNewline: !eol}
	start := findFileStart(lines, 0)
	if start < 0 {
		p.Preamble = joinLines(lines)
		return p, nil
	}
	p.Preamble = joinLines(lines[:start])
	var junk []string
	for i := start; i < len(lines); {
		f, next, err := parseFile(lines, i, junk)
		if err != nil {
			return nil, err
		}
		p.Files = append(p.Files, f)
		ns := findFileStart(lines, next)
		if ns < 0 {
			p.Trailer = joinLines(lines[next:])
			break
		}
		junk = lines[next:ns]
		i = ns
	}
	return p, nil
}

// headerish reports lines that may precede a "---" line as part of the
// same file section (diff -r command lines, CVS/SVN headers, git headers).
func headerish(l string) bool {
	for _, p := range []string{"diff ", "Index: ", "index ", "RCS file: ", "retrieving revision ", "===", "Only in "} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// findFileStart returns the index of the first line of the next file
// section at or after from, or -1.
func findFileStart(lines []string, from int) int {
	for k := from; k < len(lines); k++ {
		l := lines[k]
		if strings.HasPrefix(l, "diff --git ") {
			return k
		}
		if strings.HasPrefix(l, "--- ") && k+2 < len(lines) && strings.HasPrefix(lines[k+1], "+++ ") && strings.HasPrefix(lines[k+2], "@@ -") {
			j := k
			for j > from && headerish(lines[j-1]) {
				j--
			}
			return j
		}
	}
	return -1
}

// gitExtended lists the extended header lines of a "diff --git" section.
var gitExtended = []string{"old mode ", "new mode ", "deleted file mode ", "new file mode ", "copy from ", "copy to ",
	"rename from ", "rename to ", "rename old ", "rename new ", "similarity index ", "dissimilarity index ", "index "}

func parseFile(lines []string, start int, junk []string) (*File, int, error) {
	f := &File{Header: append([]string(nil), junk...)}
	k := start
	if strings.HasPrefix(lines[k], "diff --git ") {
		f.Git = true
		f.OldName, f.NewName = parseGitNames(strings.TrimPrefix(lines[k], "diff --git "))
		f.Header = append(f.Header, lines[k])
		k++
	header:
		for k < len(lines) {
			l := lines[k]
			switch {
			case strings.HasPrefix(l, "--- ") && k+1 < len(lines) && strings.HasPrefix(lines[k+1], "+++ "):
				break header
			case strings.HasPrefix(l, "diff --git "):
				return f, k, nil
			case strings.HasPrefix(l, "GIT binary patch"):
				// The binary data runs to the next file section.
				f.Binary = true
				for k < len(lines) && !strings.HasPrefix(lines[k], "diff --git ") {
					f.Header = append(f.Header, lines[k])
					k++
				}
				return f, k, nil
			case strings.HasPrefix(l, "Binary files "):
				f.Binary = true
			case strings.HasPrefix(l, "new file mode "):
				f.NewFile = true
			case strings.HasPrefix(l, "deleted file mode "):
				f.Deleted = true
			case cutAny(l, "rename from ", "rename old ") != "":
				f.RenameFrom = unquoteName(cutAny(l, "rename from ", "rename old "))
			case cutAny(l, "rename to ", "rename new ") != "":
				f.RenameTo = unquoteName(cutAny(l, "rename to ", "rename new "))
			case strings.HasPrefix(l, "copy from "):
				f.CopyFrom = unquoteName(strings.TrimPrefix(l, "copy from "))
			case strings.HasPrefix(l, "copy to "):
				f.CopyTo = unquoteName(strings.TrimPrefix(l, "copy to "))
			case hasAnyPrefix(l, gitExtended):
			default:
				return f, k, nil // a section without hunks (mode change, empty file)
			}
			f.Header = append(f.Header, l)
			k++
		}
		if k >= len(lines) {
			return f, k, nil
		}
	}
	// diff -r command lines and CVS/SVN headers before "---".
	for k < len(lines) && !(strings.HasPrefix(lines[k], "--- ") && k+1 < len(lines) && strings.HasPrefix(lines[k+1], "+++ ")) && headerish(lines[k]) {
		f.Header = append(f.Header, lines[k])
		k++
	}
	if k >= len(lines) || !strings.HasPrefix(lines[k], "--- ") || k+1 >= len(lines) || !strings.HasPrefix(lines[k+1], "+++ ") {
		return nil, 0, fmt.Errorf("line %d: expected a \"---\" and \"+++\" file header", k+1)
	}
	f.Header = append(f.Header, lines[k], lines[k+1])
	f.OldName = parseName(lines[k][4:])
	f.NewName = parseName(lines[k+1][4:])
	f.NewFile = f.NewFile || f.OldName == DevNull
	f.Deleted = f.Deleted || f.NewName == DevNull
	k += 2
	for k < len(lines) && strings.HasPrefix(lines[k], "@@ ") {
		h, next, err := parseHunk(lines, k)
		if err != nil {
			return nil, 0, err
		}
		f.Hunks = append(f.Hunks, h)
		k = next
	}
	if len(f.Hunks) == 0 && !f.Git {
		return nil, 0, fmt.Errorf("line %d: file header without hunks", k)
	}
	return f, k, nil
}

// cutAny returns s without the first matching prefix, or "".
func cutAny(s string, prefixes ...string) string {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest
		}
	}
	return ""
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func parseHunk(lines []string, k int) (*Hunk, int, error) {
	m := hunkRe.FindStringSubmatch(lines[k])
	if m == nil {
		return nil, 0, fmt.Errorf("line %d: malformed hunk header %q", k+1, clip(lines[k], 80))
	}
	num := func(s string, def int) int {
		if s == "" {
			return def
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	h := &Hunk{OldStart: num(m[1], 0), OldCount: num(m[2], 1), NewStart: num(m[3], 0), NewCount: num(m[4], 1), Section: m[5]}
	h.origOldStart, h.origNewStart = h.OldStart, h.NewStart
	h.rawHeader = lines[k] + "\n"
	first := k + 1
	k++
	oldLeft, newLeft := h.OldCount, h.NewCount
	for oldLeft > 0 || newLeft > 0 {
		if k >= len(lines) {
			return nil, 0, fmt.Errorf("line %d: the patch ends inside the hunk that starts at line %d", k, first-1)
		}
		l := lines[k]
		op, text := byte(' '), ""
		if l != "" { // GNU patch accepts an empty line as an empty context line
			op, text = l[0], l[1:]
		}
		switch op {
		case ' ':
			oldLeft--
			newLeft--
		case '-':
			oldLeft--
		case '+':
			newLeft--
		case '\\':
			if len(h.Lines) == 0 {
				return nil, 0, fmt.Errorf("line %d: %q before any hunk line", k+1, clip(l, 60))
			}
			h.Lines[len(h.Lines)-1].NoNewline = true
			k++
			continue
		default:
			return nil, 0, fmt.Errorf("line %d: unexpected line %q in the hunk that starts at line %d (its header promises %d old and %d new lines)", k+1, clip(l, 60), first, h.OldCount, h.NewCount)
		}
		if oldLeft < 0 || newLeft < 0 {
			return nil, 0, fmt.Errorf("line %d: the hunk that starts at line %d has more lines than its header says (%d old, %d new)", k+1, first, h.OldCount, h.NewCount)
		}
		h.Lines = append(h.Lines, Line{Op: op, Text: text})
		k++
	}
	if k < len(lines) && strings.HasPrefix(lines[k], "\\") && len(h.Lines) > 0 {
		h.Lines[len(h.Lines)-1].NoNewline = true
		k++
	}
	h.rawBody = joinLines(lines[first:k])
	return h, k, nil
}

var stampRe = regexp.MustCompile(`^(.*?)\s+\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?( ?[+-]\d{2}:?\d{2})?\s*$`)

// parseName extracts the file name from the rest of a "---" or "+++"
// line: an optional C-quoted name, then an optional tab and timestamp.
func parseName(s string) string {
	if strings.HasPrefix(s, `"`) {
		if name, _, ok := unquoteC(s); ok {
			return name
		}
	}
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		return s[:i]
	}
	if m := stampRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return strings.TrimRight(s, " \r")
}

// parseGitNames splits the rest of a "diff --git" line into the two names.
func parseGitNames(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		a, rest, ok := unquoteC(s)
		if ok {
			rest = strings.TrimPrefix(rest, " ")
			if strings.HasPrefix(rest, `"`) {
				b, _, _ := unquoteC(rest)
				return a, b
			}
			return a, rest
		}
	}
	// Unquoted: "a/x b/x". With spaces in names, prefer the split that
	// gives equal names after the prefixes.
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			continue
		}
		a, b := s[:i], s[i+1:]
		if strings.HasSuffix(b, `"`) {
			continue
		}
		if stripOne(a) == stripOne(b) {
			return a, b
		}
	}
	if i := strings.LastIndex(s, " b/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, s
}

func stripOne(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func unquoteName(s string) string {
	if strings.HasPrefix(s, `"`) {
		if name, _, ok := unquoteC(s); ok {
			return name
		}
	}
	return s
}

// unquoteC decodes a C-style quoted string as git writes it and returns
// the rest of s after the closing quote.
func unquoteC(s string) (string, string, bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", s, false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), s[i+1:], true
		case c == '\\' && i+1 < len(s):
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '0', '1', '2', '3':
				if i+2 < len(s) {
					if n, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
						b.WriteByte(byte(n))
						i += 2
						continue
					}
				}
				return "", s, false
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", s, false
}

// ErrUnsafePath is returned for patch paths that escape the tree.
var ErrUnsafePath = errors.New("unsafe path")

// StripPath removes n leading path components from a patch name, like
// patch -pN.
func StripPath(name string, n int) (string, error) {
	p := name
	for i := 0; i < n; i++ {
		j := strings.IndexByte(p, '/')
		if j < 0 {
			return "", fmt.Errorf("%q has fewer than %d leading directories to strip (-p%d)", name, n, n)
		}
		p = strings.TrimLeft(p[j+1:], "/")
	}
	return CleanPath(p)
}

// CleanPath validates a tree-relative path: no absolute paths, no "..",
// no .git components.
func CleanPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\r\n") {
		return "", fmt.Errorf("%w %q", ErrUnsafePath, p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w %q", ErrUnsafePath, p)
	}
	for _, seg := range strings.Split(c, "/") {
		if strings.EqualFold(seg, ".git") {
			return "", fmt.Errorf("%w %q", ErrUnsafePath, p)
		}
	}
	return c, nil
}

// Paths returns the tree paths the section reads (old) and writes (new),
// relative to root, for strip leading components. A created file has no
// old path, a deleted file no new path; otherwise they differ only for
// renames and copies.
func (f *File) Paths(strip int, root string) (oldPath, newPath string, err error) {
	join := func(p string) string {
		if root == "" {
			return p
		}
		return path.Join(root, p)
	}
	// Rename and copy headers carry paths without the a/ and b/ prefixes.
	plain := func(name string) (string, error) { return StripPath(name, max(0, strip-1)) }
	switch {
	case f.RenameFrom != "" || f.RenameTo != "":
		if oldPath, err = plain(f.RenameFrom); err == nil {
			newPath, err = plain(f.RenameTo)
		}
		return join(oldPath), join(newPath), err
	case f.CopyFrom != "" || f.CopyTo != "":
		if oldPath, err = plain(f.CopyFrom); err == nil {
			newPath, err = plain(f.CopyTo)
		}
		return join(oldPath), join(newPath), err
	}
	if f.OldName != DevNull && f.OldName != "" && !f.NewFile {
		if oldPath, err = StripPath(f.OldName, strip); err != nil {
			return "", "", err
		}
		oldPath = join(oldPath)
	}
	if f.NewName != DevNull && f.NewName != "" && !f.Deleted {
		if newPath, err = StripPath(f.NewName, strip); err != nil {
			return "", "", err
		}
		newPath = join(newPath)
	}
	switch {
	case oldPath == "" && newPath == "":
		return "", "", errors.New("file section without a file name")
	case f.NewFile:
		return "", newPath, nil
	case f.Deleted:
		return oldPath, "", nil
	case oldPath == "":
		oldPath = newPath
	case newPath == "":
		newPath = oldPath
	case oldPath != newPath:
		return "", "", fmt.Errorf("the file header names differ (%s, %s); only git rename headers can rename files", f.OldName, f.NewName)
	}
	return oldPath, newPath, nil
}

// Old returns the old (pre-patch) lines of the hunk.
func (h *Hunk) Old() []string {
	var out []string
	for _, l := range h.Lines {
		if l.Op != '+' {
			out = append(out, l.Text)
		}
	}
	return out
}

// New returns the new (post-patch) lines of the hunk.
func (h *Hunk) New() []string {
	var out []string
	for _, l := range h.Lines {
		if l.Op != '-' {
			out = append(out, l.Text)
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
