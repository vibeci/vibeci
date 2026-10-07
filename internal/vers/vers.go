// Package vers compares and formats upstream version strings for
// patch-mode forks, which pin an upstream version (such as Chromium's
// 154.0.8037.97) in a file and follow upstream's releases.
package vers

import (
	"fmt"
	"regexp"
	"strings"
)

var validRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// Valid reports whether v is usable as a version: it becomes part of git
// ref names, commit messages and file contents.
func Valid(v string) bool {
	return validRe.MatchString(v) && !strings.Contains(v, "..") && !strings.HasSuffix(v, ".lock") && !strings.HasSuffix(v, ".")
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Compare orders versions naturally, like sort -V: runs of digits compare
// as numbers, other characters byte by byte, so 1.9 < 1.10 and
// 154.0.8037.57 < 154.0.8037.97. A suffix that starts with "-" or "~"
// sorts before the end of the string, so 2.0-rc1 < 2.0.
func Compare(a, b string) int {
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			i, j := 0, 0
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(na) != len(nb) {
				return sign(len(na) - len(nb))
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return sign(rank(a[0]) - rank(b[0]))
		}
		a, b = a[1:], b[1:]
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		if b[0] == '-' || b[0] == '~' {
			return 1
		}
		return -1
	default:
		if a[0] == '-' || a[0] == '~' {
			return -1
		}
		return 1
	}
}

// rank orders single characters: "~" first, then "-", then the rest by
// byte value (digits after letters never meet here: digit runs are
// compared as numbers when both sides have one).
func rank(c byte) int {
	switch c {
	case '~':
		return -2
	case '-':
		return -1
	}
	return int(c)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Prerelease reports versions that look like release candidates or
// development snapshots.
func Prerelease(v string) bool {
	low := strings.ToLower(v)
	for _, m := range []string{"-rc", "-alpha", "-beta", "-pre", "-dev", "-snapshot", "-nightly", ".rc", "rc.", "~"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// Placeholder is replaced by the version in tag formats and file contents.
const Placeholder = "{version}"

// Format maps versions to tag names, e.g. "v{version}".
type Format string

// Tag returns the tag of version v.
func (f Format) Tag(v string) string {
	if f == "" {
		return v
	}
	return strings.Replace(string(f), Placeholder, v, 1)
}

// Version extracts the version from a tag name; ok is false if the tag does
// not have the format's shape.
func (f Format) Version(tag string) (v string, ok bool) {
	if f == "" {
		f = Placeholder
	}
	pre, suf, _ := strings.Cut(string(f), Placeholder)
	if len(tag) <= len(pre)+len(suf) || !strings.HasPrefix(tag, pre) || !strings.HasSuffix(tag, suf) {
		return "", false
	}
	v = tag[len(pre) : len(tag)-len(suf)]
	return v, Valid(v)
}

// Read returns the version pinned in a version file: the first group of
// the first match of re, or (re nil) the whole content without
// surrounding whitespace.
func Read(data []byte, re *regexp.Regexp) (string, error) {
	v := strings.TrimSpace(string(data))
	if re != nil {
		m := re.FindSubmatch(data)
		if len(m) < 2 {
			return "", fmt.Errorf("the version regex %q matches nothing", re)
		}
		v = string(m[1])
	}
	if !Valid(v) {
		return "", fmt.Errorf("%q is not a usable version (letters, digits and . _ + - only)", clip(v, 80))
	}
	return v, nil
}

// Replace returns data with the pinned version (see Read) replaced by v;
// everything else is kept byte for byte.
func Replace(data []byte, re *regexp.Regexp, v string) ([]byte, error) {
	if re == nil {
		s := string(data)
		body := strings.TrimSpace(s)
		if body == "" {
			return []byte(v + "\n"), nil
		}
		i := strings.Index(s, body)
		return []byte(s[:i] + v + s[i+len(body):]), nil
	}
	m := re.FindSubmatchIndex(data)
	if len(m) < 4 || m[2] < 0 {
		return nil, fmt.Errorf("the version regex %q matches nothing", re)
	}
	out := append(append(append([]byte{}, data[:m[2]]...), v...), data[m[3]:]...)
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Latest returns the highest of versions ("" if none), skipping
// prereleases unless includePre.
func Latest(versions []string, includePre bool) string {
	best := ""
	for _, v := range versions {
		if !Valid(v) || (!includePre && Prerelease(v)) {
			continue
		}
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	return best
}
