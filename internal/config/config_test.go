package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
  // comments are allowed
  "data_dir": "/data",
  "interval": "15m",
  "providers": {
    "gateway": {
      "type": "anthropic",
      "base_url": "https://llm.example.invalid/anthropic/v1/",
      "auth": "bearer",
      "api_key": "file:/run/secrets/llm_api_key",
    },
  },
  "models": {
    "fast": {"provider": "gateway", "model": "claude-a"},
    "strong": {"provider": "gateway", "model": "claude-b", "thinking": "adaptive", "effort": "high", "max_tokens": 32000},
  },
  "roles": {"triage": "fast", "investigate": "strong", "audit": "fast", "resolve": ["strong"]},
  "alerts": [{"type": "ntfy", "url": "env:NTFY_URL"}],
  "repos": [{
    "name": "demo",
    "fork": {"url": "https://github.com/me/demo.git", "branch": "main", "auth": {"token": "file:/run/secrets/gh"}},
    "upstream": {"url": "https://github.com/up/demo.git", "branch": "main"},
    "keep_ours": [".github/workflows/**"],
    "verify": [{"run": "make test"}],
  }],
}`

func writeTemp(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.jsonc")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSample(t *testing.T) {
	c, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval.Minutes() != 15 {
		t.Errorf("interval = %v", c.Interval)
	}
	p := c.Providers["gateway"]
	if p.BaseURL != "https://llm.example.invalid/anthropic/v1" {
		t.Errorf("base url not trimmed: %q", p.BaseURL)
	}
	if p.APIKey.File != "/run/secrets/llm_api_key" {
		t.Errorf("secret parse: %+v", p.APIKey)
	}
	r := c.Repos[0]
	if r.Fork.Auth.Username != "x-access-token" || r.Sandbox.Profile != "default" || r.Review.BlockOn != "malicious" {
		t.Errorf("defaults not applied: %+v", r)
	}
	if r.Verify[0].Name != "verify-1" || r.Verify[0].Timeout.Minutes() != 30 {
		t.Errorf("verify defaults: %+v", r.Verify[0])
	}
	if got := c.Alerts[0].Events; len(got) == 0 {
		t.Error("alert events default missing")
	}
}

func TestValidateErrors(t *testing.T) {
	bad := strings.Replace(sample, `"resolve": ["strong"]`, `"resolve": ["nope"]`, 1)
	bad = strings.Replace(bad, `"name": "demo"`, `"name": "../evil"`, 1)
	bad = strings.Replace(bad, `"https://github.com/up/demo.git"`, `"ext::sh -c touch% /tmp/pwned"`, 1)
	_, err := Load(writeTemp(t, bad))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"unknown model \"nope\"", "../evil", "upstream.url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

const patchSample = `{
  "providers": {"p": {"type": "anthropic", "api_key": "env:KEY"}},
  "models": {"m": {"provider": "p", "model": "x"}},
  // no triage/investigate: a patch-mode fork has no review gate
  "roles": {"audit": "m", "resolve": ["m"]},
  "repos": [{
    "name": "ungoogled",
    "fork": {"url": "https://github.com/me/ungoogled-chromium.git", "branch": "master"},
    "upstream": {
      "url": "https://github.com/chromium/chromium.git",
      "version_url": "https://versionhistory.googleapis.com/v1/chrome/platforms/linux/channels/stable/versions?pageSize=1",
      "version_regex": "\"version\": \"([0-9.]+)\"",
    },
    "patches": {
      "series": "patches/series",
      "version_file": "chromium_version.txt",
      "update_files": {"revision.txt": "1\n"},
      "ignore_whitespace": true,
      "sets": [{"glob": "patches/v8/*.patch", "root": "v8/"}],
      "sources": [{"path": "v8/", "url": "https://github.com/v8/v8.git"}],
    },
  }],
}`

func TestLoadPatchMode(t *testing.T) {
	c, err := Load(writeTemp(t, patchSample))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Repos[0]
	p := r.Patches
	if !r.PatchMode() || r.Review.IsEnabled() || !p.Drop() || p.VerifyTree != "touched" {
		t.Errorf("patch-mode defaults: %+v", p)
	}
	if len(p.Sets) != 2 || p.Sets[0].Series != "patches/series" || *p.Sets[0].Strip != 1 || p.Sets[1].Root != "v8" || p.Series != "" {
		t.Errorf("sets: %+v %+v", p.Sets[0], p.Sets[1])
	}
	if r.Upstream.TagFormat != "{version}" || r.Upstream.Fetch != "partial" || p.Sources[0].Path != "v8" || p.Sources[0].Fetch != "partial" {
		t.Errorf("upstream/source defaults: %+v %+v", r.Upstream, p.Sources[0])
	}

	bad := strings.Replace(patchSample, `"version_url"`, `"branch": "main", "version_url"`, 1)
	bad = strings.Replace(bad, `"version_file": "chromium_version.txt",`, ``, 1)
	bad = strings.Replace(bad, `"root": "v8/"`, `"root": "../v8"`, 1)
	bad = strings.Replace(bad, `"ignore_whitespace": true,`, `"ignore_whitespace": true, "fuzz": 7,`, 1)
	_, err = Load(writeTemp(t, strings.Replace(bad, `"patches": {`, `"keep_ours": ["x"], "review": {"enabled": true}, "patches": {`, 1)))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"replace upstream.branch", "patches.version_file", "patches.sets[1].root", "patches.fuzz", "keep_ours does not apply", "review.enabled cannot be true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	merge := strings.Replace(sample, `"branch": "main"},
    "keep_ours"`, `"branch": "main", "version_url": "https://x.invalid/"},
    "keep_ours"`, 1)
	if _, err := Load(writeTemp(t, merge)); err == nil || !strings.Contains(err.Error(), "only used by patch-mode forks") {
		t.Errorf("version_url outside patch mode: %v", err)
	}
	noTriage := strings.Replace(sample, `"triage": "fast", `, "", 1)
	if _, err := Load(writeTemp(t, noTriage)); err == nil || !strings.Contains(err.Error(), "roles.triage is required") {
		t.Errorf("merge mode still needs triage: %v", err)
	}
}

// TestLoadMerged: later files override earlier ones key by key; arrays are
// replaced; secrets of the base file survive an overlay that does not set
// them.
func TestLoadMerged(t *testing.T) {
	base := writeTemp(t, sample)
	over := filepath.Join(t.TempDir(), "models.jsonc")
	os.WriteFile(over, []byte(`{
	  // the provider endpoint kept out of the repository
	  "providers": {"gateway": {"base_url": "https://other.example.invalid/v1", "auth": "x-api-key"}},
	  "models": {"strong": {"effort": "max"}},
	  "roles": {"resolve": ["strong", "fast"]},
	}`), 0o600)
	c, err := Load(base, over)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers["gateway"]
	if p.Type != "anthropic" || p.BaseURL != "https://other.example.invalid/v1" || p.Auth != "x-api-key" || p.APIKey.File != "/run/secrets/llm_api_key" {
		t.Errorf("provider merge: %+v", p)
	}
	if m := c.Models["strong"]; m.Effort != "max" || m.Thinking != "adaptive" || m.MaxTokens != 32000 {
		t.Errorf("model merge: %+v", m)
	}
	if strings.Join(c.Roles.Resolve, ",") != "strong,fast" || c.Roles.Triage != "fast" || len(c.Repos) != 1 {
		t.Errorf("roles/repos merge: %+v %d repos", c.Roles, len(c.Repos))
	}

	os.WriteFile(over, []byte("{\n  \"providers\": {\"gateway\": {\"bse_url\": \"x\"}}\n}"), 0o600)
	if _, err := Load(base, over); err == nil || !strings.Contains(err.Error(), over) || !strings.Contains(err.Error(), "bse_url") {
		t.Errorf("unknown key in an overlay must name the file: %v", err)
	}
	os.WriteFile(over, []byte(`{"roles": {"resolve": ["nope"]}}`), 0o600)
	if _, err := Load(base, over); err == nil || !strings.Contains(err.Error(), base+" + "+over) || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("validation errors of a merged config must name both files: %v", err)
	}
}

func TestMerge(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 1, "y": []any{1, 2}}, "b": "keep"}
	over := map[string]any{"a": map[string]any{"y": []any{3}, "z": true}, "c": nil}
	got := Merge(base, over).(map[string]any)
	a := got["a"].(map[string]any)
	if a["x"] != 1 || len(a["y"].([]any)) != 1 || a["z"] != true || got["b"] != "keep" || got["c"] != nil {
		t.Errorf("merge: %v", got)
	}
	if len(base["a"].(map[string]any)) != 2 {
		t.Error("Merge modified its base")
	}
	if Merge(nil, "x") != "x" || Merge(map[string]any{"a": 1}, []any{}) == nil {
		t.Error("non-object values must replace")
	}
}

func TestStateRef(t *testing.T) {
	for ref, ok := range map[string]bool{
		"refs/vibeci/state":       true,
		"refs/heads/vibeci-state": true,
		"vibeci/state":            false,
		"refs/tags/state":         false,
		"refs/heads/main":         false, // the fork branch
		"refs/vibeci/../x":        false,
		"refs/vibeci/state.lock":  false,
	} {
		cfg := strings.Replace(sample, `"interval": "15m",`, `"interval": "15m", "state_ref": "`+ref+`",`, 1)
		_, err := Load(writeTemp(t, cfg))
		if (err == nil) != ok {
			t.Errorf("state_ref %q: err = %v", ref, err)
		}
	}
}

func TestSecretResolve(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte("  abc123\n"), 0o600)
	s := Secret{File: f}
	if v, err := s.Resolve(); err != nil || v != "abc123" {
		t.Fatalf("file secret: %q %v", v, err)
	}
	t.Setenv("VIBECI_TEST_SECRET", "xyz")
	if v, err := (Secret{Env: "VIBECI_TEST_SECRET"}).Resolve(); err != nil || v != "xyz" {
		t.Fatalf("env secret: %q %v", v, err)
	}
	if _, err := (Secret{Env: "VIBECI_DEFINITELY_UNSET"}).Resolve(); err == nil {
		t.Fatal("expected error for unset env")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, p string
		want   bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/sub/x.yml", true},
		{".github/workflows/**", ".github/ci.yml", false},
		{"*.lock", "Cargo.lock", true},
		{"*.lock", "a/b/yarn.lock", true},
		{"docs/**/*.md", "docs/a/b/c.md", true},
		{"docs/**/*.md", "docs/c.md", true},
		{"docs/**/*.md", "src/c.md", false},
		{"src/*.go", "src/a/b.go", false},
		{"Makefile", "Makefile", true},
		{"Makefile", "sub/Makefile", true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pat, c.p); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pat, c.p, got, c.want)
		}
	}
	if ValidateGlob("a/**b") == nil || ValidateGlob("/abs") == nil || ValidateGlob("[") == nil {
		t.Error("expected invalid globs to be rejected")
	}
}

func TestSandboxdValidate(t *testing.T) {
	s := &Sandboxd{
		DataVolume: "vibeci-data",
		Profiles: map[string]*Profile{
			"default": {Image: "vibeci-sandbox:latest"},
			"root":    {Image: "x", User: "0:0"},
			"hostnet": {Image: "x", Network: "host"},
		},
	}
	s.ApplyDefaults()
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "non-root") || !strings.Contains(err.Error(), "host networking") {
		t.Fatalf("expected root and host-network rejections, got %v", err)
	}
	if s.Profiles["default"].Network != "none" || s.Profiles["default"].User != "10001:10001" {
		t.Errorf("profile defaults: %+v", s.Profiles["default"])
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"4g": 4 << 30, "512m": 512 << 20, "1024": 1024, "1.5g": 3 << 29, "2GB": 2 << 30} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
