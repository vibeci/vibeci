package vers

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	ordered := []string{"1.0-rc1", "1.0", "1.0.1", "1.2", "1.9", "1.10", "1.10.0.1", "2.0~beta", "2.0", "153.0.8010.52", "154.0.8037.57", "154.0.8037.97", "154.0.8038.0"}
	for i := range ordered {
		for j := range ordered {
			want := sign(i - j)
			if got := Compare(ordered[i], ordered[j]); got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	shuffled := []string{"1.10", "1.9", "1.0", "1.2", "1.0-rc1"}
	sort.Slice(shuffled, func(a, b int) bool { return Compare(shuffled[a], shuffled[b]) < 0 })
	if got := strings.Join(shuffled, " "); got != "1.0-rc1 1.0 1.2 1.9 1.10" {
		t.Errorf("sorted = %s", got)
	}
	if Compare("1.01", "1.1") != 0 {
		t.Error("leading zeros are numeric")
	}
}

func TestReadReplace(t *testing.T) {
	if v, err := Read([]byte("154.0.8037.97\n"), nil); err != nil || v != "154.0.8037.97" {
		t.Errorf("plain = %q %v", v, err)
	}
	out, err := Replace([]byte("154.0.8037.97\n"), nil, "154.0.8037.120")
	if err != nil || string(out) != "154.0.8037.120\n" {
		t.Errorf("replace plain = %q %v", out, err)
	}
	pkg := []byte("{\n  \"config\": {\n    \"projects\": {\n      \"chrome\": {\n        \"tag\": \"155.0.8059.30\",\n        \"repository\": {}\n      }\n    }\n  },\n  \"version\": \"1.2.3\"\n}\n")
	re := regexp.MustCompile(`"chrome":\s*\{\s*"tag":\s*"([^"]+)"`)
	if v, err := Read(pkg, re); err != nil || v != "155.0.8059.30" {
		t.Errorf("regex = %q %v", v, err)
	}
	out, err = Replace(pkg, re, "155.0.8059.41")
	if err != nil || !strings.Contains(string(out), `"tag": "155.0.8059.41",`) || !strings.Contains(string(out), `"version": "1.2.3"`) || len(out) != len(pkg) {
		t.Errorf("replace regex = %s %v", out, err)
	}
	if _, err := Read([]byte("not a version at all\n"), nil); err == nil {
		t.Error("a sentence is not a version")
	}
	if _, err := Read(pkg, regexp.MustCompile(`"nope": "([^"]+)"`)); err == nil {
		t.Error("no match must fail")
	}
}

func TestValidAndFormat(t *testing.T) {
	for _, v := range []string{"154.0.8037.97", "v1.2.3", "1.0-rc1", "2024.01.02+build"} {
		if !Valid(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"", "-1", "1..2", "1.2.lock", "a b", "1/2", "../x", "1.", "x;y", strings.Repeat("1", 200)} {
		if Valid(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
	f := Format("v{version}")
	if f.Tag("1.2") != "v1.2" {
		t.Error("tag")
	}
	if v, ok := f.Version("v1.2"); !ok || v != "1.2" {
		t.Errorf("version = %q %v", v, ok)
	}
	if _, ok := f.Version("1.2"); ok {
		t.Error("tag without prefix accepted")
	}
	if _, ok := f.Version("v"); ok {
		t.Error("empty version accepted")
	}
	if v, ok := Format("").Version("154.0.8037.97"); !ok || v != "154.0.8037.97" {
		t.Errorf("default format = %q %v", v, ok)
	}
	if v, ok := Format("release-{version}-final").Version("release-3.1-final"); !ok || v != "3.1" {
		t.Errorf("suffix format = %q %v", v, ok)
	}
	if got := Latest([]string{"1.9", "1.10", "2.0-rc1", "bad version"}, false); got != "1.10" {
		t.Errorf("latest = %q", got)
	}
	if got := Latest([]string{"1.9", "2.0-rc1"}, true); got != "2.0-rc1" {
		t.Errorf("latest with prereleases = %q", got)
	}
}
