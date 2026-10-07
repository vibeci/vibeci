package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDeployExamplesParse(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "deploy")
	c, err := Load(filepath.Join(root, "config.example.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repos) != 2 || c.Repos[0].Sandbox.PrefetchProfile != "go-egress" {
		t.Fatalf("config example: %+v", c.Repos)
	}
	if p := c.Repos[1]; !p.PatchMode() || len(p.Patches.Sets) != 1 || len(p.Patches.Sources) != 3 || p.Upstream.Fetch != "partial" || p.Review.IsEnabled() {
		t.Errorf("patch-mode example: %+v", p)
	}
	s, err := LoadSandboxd(filepath.Join(root, "sandboxd.example.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Repos {
		for _, p := range []string{r.Sandbox.Profile, r.Sandbox.VerifyProfile, r.Sandbox.PrefetchProfile} {
			if s.Profiles[p] == nil {
				t.Errorf("profile %q used by the config example is missing from the sandboxd example", p)
			}
		}
	}
}
