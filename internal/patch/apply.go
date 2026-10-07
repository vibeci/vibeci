package patch

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Source provides the original contents of a tree.
type Source interface {
	// ReadFile returns the content of a tree path; exists is false if the
	// file does not exist.
	ReadFile(path string) (data []byte, exists bool, err error)
}

// MapSource is a Source backed by a map (tests, small trees).
type MapSource map[string]string

// ReadFile implements Source.
func (m MapSource) ReadFile(p string) ([]byte, bool, error) {
	s, ok := m[p]
	return []byte(s), ok, nil
}

// FileState is the content of one file at some point.
type FileState struct {
	Data   []byte
	Exists bool
}

// Equal reports whether two states are the same.
func (s FileState) Equal(o FileState) bool {
	return s.Exists == o.Exists && (!s.Exists || bytes.Equal(s.Data, o.Data))
}

// Tree is the state a patch series is applied to: files are loaded lazily
// from Source and changed in memory.
type Tree struct {
	Source Source
	files  map[string]FileState
}

// NewTree returns a tree over src.
func NewTree(src Source) *Tree { return &Tree{Source: src, files: map[string]FileState{}} }

// Get returns the current state of a file.
func (t *Tree) Get(p string) (FileState, error) {
	if s, ok := t.files[p]; ok {
		return s, nil
	}
	data, exists, err := t.Source.ReadFile(p)
	if err != nil {
		return FileState{}, err
	}
	s := FileState{Data: data, Exists: exists}
	t.files[p] = s
	return s, nil
}

// Set replaces the state of a file.
func (t *Tree) Set(p string, s FileState) {
	if !s.Exists {
		s.Data = nil
	}
	t.files[p] = s
}

// Paths returns every path the tree has loaded or changed, sorted.
func (t *Tree) Paths() []string {
	out := make([]string, 0, len(t.files))
	for p := range t.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Clone returns an independent copy of the tree (sharing the source).
func (t *Tree) Clone() *Tree {
	c := &Tree{Source: t.Source, files: make(map[string]FileState, len(t.files))}
	for k, v := range t.files {
		c.files[k] = v
	}
	return c
}

// text is file content split into lines.
type text struct {
	lines []string
	eol   bool // the last line ends with a newline
}

func splitText(data []byte) text {
	lines, eol := splitLines(string(data))
	return text{lines: lines, eol: eol}
}

func (t text) bytes() []byte {
	if len(t.lines) == 0 {
		return []byte{}
	}
	s := strings.Join(t.lines, "\n")
	if t.eol {
		s += "\n"
	}
	return []byte(s)
}

// Options control how a patch is applied.
type Options struct {
	// Strip removes leading path components, like patch -pN.
	Strip int
	// Root is the tree directory the patch paths are relative to.
	Root string
	// Fuzz is how many context lines may be ignored at each end of a hunk
	// (patch -F). Fuzz can place a hunk in the wrong spot.
	Fuzz int
	// IgnoreWhitespace compares lines ignoring differences in spaces and
	// tabs (patch -l).
	IgnoreWhitespace bool
	// Exact requires every hunk to apply at its recorded line without
	// fuzz (checking generated patches).
	Exact bool
	// DetectApplied reports hunks whose change is already present as
	// AlreadyApplied instead of Failed.
	DetectApplied bool
	// KeepBinary reports whether a binary section for a tree path can be
	// kept as it is. VibeCI cannot apply binary changes, so such sections
	// count as Exact without changing the tree; the caller decides when
	// that is safe (the file is unchanged since the patch was written).
	// Without it, binary sections fail.
	KeepBinary func(path string) bool
}

// Status is the outcome of applying a hunk or a patch.
type Status int

// Statuses, from best to worst.
const (
	Exact          Status = iota // applied at the recorded line
	Shifted                      // applied at another line
	Fuzzed                       // applied ignoring some context lines
	AlreadyApplied               // the change is already present
	Failed                       // could not be applied
)

func (s Status) String() string {
	switch s {
	case Exact:
		return "exact"
	case Shifted:
		return "shifted"
	case Fuzzed:
		return "fuzzed"
	case AlreadyApplied:
		return "applied"
	}
	return "failed"
}

// HunkResult is the outcome for one hunk.
type HunkResult struct {
	Status Status
	// Line is the 1-based line of the file before the hunk where the
	// hunk's old lines start (where they were expected, if it failed).
	Line   int
	Offset int
	Fuzz   int
	// Near is the closest approximate location of a failed hunk.
	Near *Near
}

// Near describes where a failed hunk would fit best.
type Near struct {
	Line    int // 1-based line of the file where the best window starts
	Matched int // old lines of the hunk that match there
	Total   int
	// RemovedMissing counts lines the hunk removes that appear nowhere in
	// the file.
	RemovedMissing int
}

// FileResult is the outcome for one file section.
type FileResult struct {
	File *File
	// OldPath and Path are the tree paths read and written.
	OldPath, Path string
	Hunks         []HunkResult
	// Before and After are the file states around the section (After is
	// with every hunk that could be placed applied). For renames Before
	// is the old path's state.
	Before, After FileState
	// Status is the worst hunk status (or the section's own outcome for
	// creations, deletions and binary sections).
	Status Status
	// Problem explains a failed section that has no failed hunk.
	Problem string
}

// Result is the outcome of applying a patch.
type Result struct {
	Files []*FileResult
}

// Status returns the worst outcome of any section. AlreadyApplied means
// some (see Upstreamed) of the changes are already present.
func (r *Result) Status() Status {
	worst := Exact
	for _, f := range r.Files {
		worst = max(worst, f.Status)
	}
	return worst
}

// Upstreamed reports whether every change of the patch is already
// present.
func (r *Result) Upstreamed() bool {
	for _, f := range r.Files {
		if f.Status != AlreadyApplied {
			return false
		}
		for _, h := range f.Hunks {
			if h.Status != AlreadyApplied {
				return false
			}
		}
	}
	return len(r.Files) > 0
}

// Count returns the number of hunks with each status.
func (r *Result) Count() map[Status]int {
	out := map[Status]int{}
	for _, f := range r.Files {
		if len(f.Hunks) == 0 {
			out[f.Status]++
		}
		for _, h := range f.Hunks {
			out[h.Status]++
		}
	}
	return out
}

// FailedHunks counts failed hunks (a failed section without hunks counts
// as one).
func (r *Result) FailedHunks() int {
	n := 0
	for _, f := range r.Files {
		if f.Problem != "" {
			n++
		}
		for _, h := range f.Hunks {
			if h.Status == Failed {
				n++
			}
		}
	}
	return n
}

// Touched returns the tree paths the patch reads or writes, in order.
func (r *Result) Touched() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range r.Files {
		for _, p := range []string{f.OldPath, f.Path} {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// TouchedPaths returns the tree paths a patch reads or writes, without
// applying it.
func TouchedPaths(p *Patch, strip int, root string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range p.Files {
		op, np, err := f.Paths(strip, root)
		if err != nil {
			return nil, err
		}
		for _, x := range []string{op, np} {
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out, nil
}

// Apply applies p to t. Like patch, it applies every hunk it can place and
// reports the rest as failed; the tree is changed either way. The error is
// only for unusable patches (bad paths) and source read failures.
func Apply(p *Patch, t *Tree, o Options) (*Result, error) {
	res := &Result{}
	for _, f := range p.Files {
		fr, err := applyFile(f, t, o)
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, fr)
	}
	return res, nil
}

func applyFile(f *File, t *Tree, o Options) (*FileResult, error) {
	oldPath, newPath, err := f.Paths(o.Strip, o.Root)
	if err != nil {
		return nil, err
	}
	fr := &FileResult{File: f, OldPath: oldPath, Path: newPath}
	readPath := oldPath
	if readPath == "" {
		readPath = newPath
	}
	before, err := t.Get(readPath)
	if err != nil {
		return nil, err
	}
	fr.Before = before
	// File-level outcomes give every hunk the section's status.
	all := func(s Status) {
		fr.Status = s
		for _, h := range f.Hunks {
			fr.Hunks = append(fr.Hunks, HunkResult{Status: s, Line: max(h.OldStart, 1)})
		}
	}
	fail := func(why string) (*FileResult, error) {
		all(Failed)
		fr.Problem, fr.After = why, before
		return fr, nil
	}
	applied := func() (*FileResult, error) {
		all(AlreadyApplied)
		fr.After = before
		if newPath != "" && newPath != readPath {
			fr.After, _ = t.Get(newPath)
		}
		return fr, nil
	}
	if f.Binary {
		if o.KeepBinary != nil && o.KeepBinary(readPath) {
			fr.Status, fr.After = Exact, before
			return fr, nil
		}
		return fail("binary change: VibeCI cannot apply or rewrite binary patches")
	}

	switch {
	case f.RenameFrom != "" || f.CopyFrom != "":
		dest, err := t.Get(newPath)
		if err != nil {
			return nil, err
		}
		switch {
		case !before.Exists && dest.Exists && f.RenameFrom != "" && o.DetectApplied:
			// Already renamed: apply the content changes to the new name.
			before = dest
			fr.Before = dest
		case !before.Exists:
			return fail(fmt.Sprintf("%s does not exist", oldPath))
		case dest.Exists:
			return fail(fmt.Sprintf("%s already exists", newPath))
		}
		if f.RenameFrom != "" {
			t.Set(oldPath, FileState{})
		}
		t.Set(newPath, before)
	case f.NewFile:
		content := text{eol: true}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Op == '+' {
					content.lines = append(content.lines, l.Text)
					content.eol = !l.NoNewline
				}
			}
		}
		data := content.bytes()
		switch {
		case !before.Exists || len(before.Data) == 0:
			t.Set(newPath, FileState{Data: data, Exists: true})
			all(Exact)
			fr.After = FileState{Data: data, Exists: true}
			return fr, nil
		case o.DetectApplied && sameText(before.Data, data, o.IgnoreWhitespace):
			return applied()
		}
		return fail(fmt.Sprintf("%s already exists (the patch creates it)", newPath))
	case f.Deleted:
		if !before.Exists {
			if o.DetectApplied {
				return applied()
			}
			return fail(fmt.Sprintf("%s does not exist (the patch deletes it)", oldPath))
		}
		var old text
		old.eol = true
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Op != '+' {
					old.lines = append(old.lines, l.Text)
					old.eol = !l.NoNewline
				}
			}
		}
		if !sameText(before.Data, old.bytes(), o.IgnoreWhitespace) {
			return fail(fmt.Sprintf("%s differs from the content the patch deletes", oldPath))
		}
		t.Set(oldPath, FileState{})
		all(Exact)
		fr.After = FileState{}
		return fr, nil
	}

	if len(f.Hunks) == 0 { // mode change or pure rename
		fr.After, _ = t.Get(newPath)
		fr.Status = Exact
		return fr, nil
	}
	cur, err := t.Get(newPath)
	if err != nil {
		return nil, err
	}
	if !cur.Exists {
		return fail(fmt.Sprintf("%s does not exist", newPath))
	}
	out, results := applyHunks(splitText(cur.Data), f.Hunks, o)
	after := FileState{Data: out.bytes(), Exists: true}
	t.Set(newPath, after)
	fr.After, fr.Hunks = after, results
	fr.Status = Exact
	for _, r := range results {
		fr.Status = max(fr.Status, r.Status)
	}
	return fr, nil
}

func sameText(a, b []byte, loose bool) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if !loose {
		return false
	}
	ta, tb := splitText(a), splitText(b)
	if len(ta.lines) != len(tb.lines) {
		return false
	}
	for i := range ta.lines {
		if !lineEqual(ta.lines[i], tb.lines[i], true) {
			return false
		}
	}
	return true
}

func lineEqual(a, b string, loose bool) bool {
	if a == b {
		return true
	}
	if !loose {
		return false
	}
	return normWS(a) == normWS(b)
}

// normWS collapses runs of spaces and tabs and trims them at the ends.
func normWS(s string) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	return b.String()
}

// hunkShape describes a hunk's old and new images.
type hunkShape struct {
	old, new    []string
	lead, trail int // context lines at the start and end
	expected    int // 0-based index of the old image in the recorded file
}

func shape(h *Hunk) hunkShape {
	var s hunkShape
	for i, l := range h.Lines {
		if l.Op != '+' {
			s.old = append(s.old, l.Text)
		}
		if l.Op != '-' {
			s.new = append(s.new, l.Text)
		}
		if l.Op == ' ' && s.lead == i {
			s.lead++
		}
	}
	for i := len(h.Lines) - 1; i >= 0 && h.Lines[i].Op == ' '; i-- {
		s.trail++
	}
	if s.lead == len(h.Lines) { // context only
		s.trail = 0
	}
	s.expected = h.OldStart - 1
	if h.OldCount == 0 {
		s.expected = h.OldStart
	}
	return s
}

// applyHunks applies hunks in order to in. Like GNU patch, a hunk is
// searched for nearest to where the previous hunks put it, never before
// the end of the previous hunk.
func applyHunks(in text, hunks []*Hunk, o Options) (text, []HunkResult) {
	var out []string
	lastEOL := true
	copyIn := func(from, to int) {
		for i := from; i < to; i++ {
			out = append(out, in.lines[i])
			lastEOL = i < len(in.lines)-1 || in.eol
		}
	}
	pos, offset := 0, 0
	results := make([]HunkResult, len(hunks))
	for hi, h := range hunks {
		s := shape(h)
		exp := s.expected + offset
		where, fuzz, ok := locate(in.lines, pos, exp, s, o)
		if ok {
			lead, trail := min(fuzz, s.lead), min(fuzz, s.trail)
			start := where - lead // where the full old image would start
			status := Exact
			switch {
			case fuzz > 0:
				status = Fuzzed
			case start != s.expected:
				status = Shifted
			}
			results[hi] = HunkResult{Status: status, Line: start + 1, Offset: start - s.expected, Fuzz: fuzz}
			copyIn(pos, where)
			// Walk the hunk lines that take part in the match: all but the
			// leading and trailing context lines ignored by fuzz. Context
			// is copied from the file, so whitespace the match ignored is
			// kept.
			i := where
			for k, l := range h.Lines {
				if k < lead || k >= len(h.Lines)-trail {
					continue
				}
				switch l.Op {
				case ' ':
					out = append(out, in.lines[i])
					lastEOL = i < len(in.lines)-1 || in.eol
					i++
				case '-':
					i++
				case '+':
					out = append(out, l.Text)
					lastEOL = !l.NoNewline
				}
			}
			pos = i
			offset = start - s.expected
			continue
		}
		if o.DetectApplied && !o.Exact {
			if p, ok := locateApplied(in.lines, pos, exp, s, o); ok {
				results[hi] = HunkResult{Status: AlreadyApplied, Line: p + 1, Offset: p - s.expected}
				copyIn(pos, p+len(s.new))
				pos = p + len(s.new)
				offset = p - s.expected + len(s.new) - len(s.old)
				continue
			}
		}
		results[hi] = HunkResult{Status: Failed, Line: exp + 1, Offset: offset, Near: nearest(in.lines, exp, s, o)}
	}
	copyIn(pos, len(in.lines))
	return text{lines: out, eol: lastEOL || len(out) == 0}, results
}

func match(lines []string, at int, pat []string, loose bool) bool {
	if at < 0 || at+len(pat) > len(lines) {
		return false
	}
	for i, p := range pat {
		if !lineEqual(lines[at+i], p, loose) {
			return false
		}
	}
	return true
}

// search finds pat nearest to exp within [minPos, len(lines)-len(pat)],
// trying later positions first at equal distance (like GNU patch).
func search(lines []string, minPos, exp int, pat []string, loose, exact bool) (int, bool) {
	maxPos := len(lines) - len(pat)
	if exact {
		if exp >= minPos && match(lines, exp, pat, loose) {
			return exp, true
		}
		return 0, false
	}
	if maxPos < minPos {
		return 0, false
	}
	exp = max(minPos, min(exp, maxPos))
	for d := 0; ; d++ {
		after, before := exp+d, exp-d
		if after > maxPos && before < minPos {
			return 0, false
		}
		if after <= maxPos && match(lines, after, pat, loose) {
			return after, true
		}
		if d > 0 && before >= minPos && match(lines, before, pat, loose) {
			return before, true
		}
	}
}

// locate finds where a hunk's old image applies; it returns the position
// of the part that has to match (without fuzzed context) and the fuzz.
func locate(lines []string, minPos, exp int, s hunkShape, o Options) (int, int, bool) {
	if len(s.old) == 0 {
		// Pure insertion without context: there is nothing to verify.
		p := max(minPos, min(exp, len(lines)))
		if o.Exact && p != exp {
			return 0, 0, false
		}
		return p, 0, true
	}
	maxFuzz := o.Fuzz
	if o.Exact {
		maxFuzz = 0
	}
	for fuzz := 0; fuzz <= maxFuzz; fuzz++ {
		lead, trail := min(fuzz, s.lead), min(fuzz, s.trail)
		if fuzz > 0 && lead == min(fuzz-1, s.lead) && trail == min(fuzz-1, s.trail) {
			break // no more context to ignore
		}
		pat := s.old[lead : len(s.old)-trail]
		if len(pat) == 0 {
			break
		}
		p, ok := search(lines, minPos, exp+lead, pat, o.IgnoreWhitespace, o.Exact)
		if !ok {
			continue
		}
		// Unlike GNU patch, a fuzzed match must be the only place the
		// remaining lines fit: with less context the nearest of several
		// matches is too often the wrong one.
		if fuzz > 0 && !unique(lines, minPos, p, pat, o.IgnoreWhitespace) {
			return 0, 0, false
		}
		return p, fuzz, true
	}
	return 0, 0, false
}

// unique reports whether pat matches nowhere in lines[minPos:] but at p.
func unique(lines []string, minPos, p int, pat []string, loose bool) bool {
	for q := minPos; q+len(pat) <= len(lines); q++ {
		if q != p && match(lines, q, pat, loose) {
			return false
		}
	}
	return true
}

// locateApplied looks for a hunk's new image where its old image does not
// fit: the change is already in the file. Only distinctive evidence
// counts: the hunk adds a line with letters or digits, or it only removes
// lines, has at least three context lines, and none of the removed lines
// appear anywhere in the file.
func locateApplied(lines []string, minPos, exp int, s hunkShape, o Options) (int, bool) {
	added, removed := informativeDiff(s.old, s.new)
	if len(added) == 0 {
		if len(removed) == 0 || len(s.new) < 3 {
			return 0, false
		}
		for _, r := range removed {
			for _, l := range lines {
				if lineEqual(l, r, o.IgnoreWhitespace) {
					return 0, false
				}
			}
		}
	}
	if len(s.new) == 0 {
		return 0, false
	}
	return search(lines, minPos, exp, s.new, o.IgnoreWhitespace, false)
}

// informativeDiff returns the lines (with letters or digits) only in new
// (added) and only in old (removed).
func informativeDiff(old, new []string) (added, removed []string) {
	count := map[string]int{}
	for _, l := range old {
		count[l]++
	}
	for _, l := range new {
		if count[l] > 0 {
			count[l]--
			continue
		}
		if informative(l) {
			added = append(added, l)
		}
	}
	for l, n := range count {
		if n > 0 && informative(l) {
			removed = append(removed, l)
		}
	}
	sort.Strings(removed)
	return added, removed
}

func informative(l string) bool {
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80 {
			return true
		}
	}
	return false
}

// nearest finds the window of the file that matches most of a failed
// hunk's old lines.
func nearest(lines []string, exp int, s hunkShape, o Options) *Near {
	n := &Near{Total: len(s.old)}
	_, removed := informativeDiff(s.old, s.new)
	for _, r := range removed {
		found := false
		for _, l := range lines {
			if lineEqual(l, r, o.IgnoreWhitespace) {
				found = true
				break
			}
		}
		if !found {
			n.RemovedMissing++
		}
	}
	if len(s.old) == 0 || len(lines) == 0 {
		return n
	}
	best, bestDist := -1, 0
	for p := 0; p+1 <= len(lines); p++ {
		score := 0
		for i, pl := range s.old {
			if p+i < len(lines) && lineEqual(lines[p+i], pl, o.IgnoreWhitespace) {
				score++
			}
		}
		dist := p - exp
		if dist < 0 {
			dist = -dist
		}
		if score > best || (score == best && dist < bestDist) {
			best, bestDist = score, dist
			n.Line, n.Matched = p+1, score
		}
	}
	return n
}
