package config

import (
	"errors"
	"path"
	"strings"
)

// ValidateGlob checks a path glob. Supported syntax: "*", "?", "[...]"
// within one path segment and "**" for any number of segments. A pattern
// without a slash matches the basename at any depth (like .gitignore).
func ValidateGlob(p string) error {
	if p == "" {
		return errors.New("empty pattern")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("patterns are relative to the repository root; drop the leading slash")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return errors.New(`"**" must be a whole path segment`)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// MatchGlob reports whether repository-relative path p matches pattern.
func MatchGlob(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	pattern = strings.TrimSuffix(pattern, "/")
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

// MatchAny reports whether p matches any of the patterns.
func MatchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if MatchGlob(pat, p) {
			return true
		}
	}
	return false
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true // trailing ** matches everything below
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegs(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
