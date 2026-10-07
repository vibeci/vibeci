package patch

import (
	"fmt"
	"strconv"
	"strings"
)

// Series is a quilt series file: one patch per line, optionally followed
// by -pN, with # comments and blank lines.
type Series struct {
	lines   []string
	eol     bool
	Entries []SeriesEntry
}

// SeriesEntry is one patch of a series.
type SeriesEntry struct {
	// Name is the patch path relative to the series file's directory.
	Name string
	// Strip is the -pN option, or -1 if the line has none.
	Strip int
	line  int
}

// ParseSeries parses a quilt series file.
func ParseSeries(data []byte) (*Series, error) {
	lines, eol := splitLines(string(data))
	s := &Series{lines: lines, eol: eol}
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if j := strings.Index(l, " #"); j >= 0 {
			l = strings.TrimSpace(l[:j])
		}
		fields := strings.Fields(l)
		e := SeriesEntry{Name: fields[0], Strip: -1, line: i}
		for k := 1; k < len(fields); k++ {
			opt := fields[k]
			switch {
			case strings.HasPrefix(opt, "-p"):
				v := strings.TrimPrefix(opt, "-p")
				if v == "" && k+1 < len(fields) {
					k++
					v = fields[k]
				}
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 || n > 9 {
					return nil, fmt.Errorf("series line %d: bad strip option %q", i+1, opt)
				}
				e.Strip = n
			case opt == "-R":
				return nil, fmt.Errorf("series line %d: reversed patches (-R) are not supported", i+1)
			default:
				return nil, fmt.Errorf("series line %d: unknown option %q", i+1, opt)
			}
		}
		if _, err := CleanPath(e.Name); err != nil {
			return nil, fmt.Errorf("series line %d: %v", i+1, err)
		}
		s.Entries = append(s.Entries, e)
	}
	return s, nil
}

// Remove deletes the lines of the named patches.
func (s *Series) Remove(names ...string) {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	skip := map[int]bool{}
	var kept []SeriesEntry
	for _, e := range s.Entries {
		if drop[e.Name] {
			skip[e.line] = true
			continue
		}
		kept = append(kept, e)
	}
	var lines []string
	index := map[int]int{}
	for i, l := range s.lines {
		if skip[i] {
			continue
		}
		index[i] = len(lines)
		lines = append(lines, l)
	}
	for i := range kept {
		kept[i].line = index[kept[i].line]
	}
	s.lines, s.Entries = lines, kept
}

// Format renders the series, unchanged lines byte for byte.
func (s *Series) Format() []byte {
	out := joinLines(s.lines)
	if !s.eol {
		out = strings.TrimSuffix(out, "\n")
	}
	return []byte(out)
}
