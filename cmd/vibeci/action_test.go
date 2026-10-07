package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/internal/config"
)

func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		"":                                {},
		"  -dry-run  ":                    {"-dry-run"},
		`-reason "a false positive" abc1`: {"-reason", "a false positive", "abc1"},
		`-reason 'it''s fine'`:            {"-reason", "its fine"},
		`a\ b "q\"x\\" 'raw\'`:            {"a b", `q"x\`, `raw\`},
		"x\ny\tz":                         {"x", "y", "z"},
		`""`:                              {""},
	} {
		got, err := splitArgs(in)
		if err != nil || (len(got) != 0 || len(want) != 0) && !reflect.DeepEqual(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := splitArgs(`-reason "open`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestWorkflowCommandEscaping(t *testing.T) {
	var out bytes.Buffer
	a := &ghAction{stdout: &out}
	a.command("error", map[string]string{"title": "VibeCI: a,b"}, "50% done\nnext line")
	if got := out.String(); got != "::error title=VibeCI%3A a%2Cb::50%25 done%0Anext line\n" {
		t.Errorf("got %q", got)
	}
	if mdCell("a | b\n<script>") != `a \| b &lt;script&gt;` {
		t.Errorf("mdCell: %q", mdCell("a | b\n<script>"))
	}
}

// actionRepo creates a bare repository at <dir>/srv/owner/demo.git holding
// files on main, as GitHub would serve this repository.
func actionRepo(t *testing.T, dir string, files map[string]string) (bare, sha string) {
	t.Helper()
	work := filepath.Join(dir, "work")
	bare = filepath.Join(dir, "srv", "owner", "demo.git")
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(work, 0o755)
	os.MkdirAll(bare, 0o755)
	run(bare, "init", "-q", "--bare")
	run(bare, "config", "uploadpack.allowFilter", "true")
	run(bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	run(work, "init", "-q")
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(work, p)), 0o755)
		os.WriteFile(filepath.Join(work, p), []byte(c), 0o644)
	}
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "init")
	run(work, "push", "-q", bare, "HEAD:main")
	return bare, run(work, "rev-parse", "HEAD")
}

const actionConfig = `{
  // the fork is this repository
  "repos": [{"upstream": {"url": "https://example.invalid/up.git", "branch": "main"}, "review": {"enabled": false}}],
  "roles": {"audit": "m", "resolve": ["m"]},
}`

const actionModels = `{
  "providers": {"gw": {"type": "anthropic", "base_url": "https://llm.gateway.example.invalid/tenant-7/anthropic/v1", "api_key": "sk-inline-secret-1234",
    "headers": {"X-Tenant": "tenant-secret"}}},
  "models": {"m": {"provider": "gw", "model": "model-secret-id"}},
}`

func newTestAction(t *testing.T, env map[string]string) (*ghAction, *bytes.Buffer) {
	var out bytes.Buffer
	return &ghAction{getenv: func(k string) string { return env[k] }, stdout: &out, stderr: os.Stderr}, &out
}

// TestActionCompose: the config comes from the repository at GITHUB_SHA
// when the workspace lacks it; this repository becomes the fork; the
// models input is masked.
func TestActionCompose(t *testing.T) {
	dir := t.TempDir()
	bare, sha := actionRepo(t, dir, map[string]string{".github/vibeci.jsonc": actionConfig})
	env := map[string]string{
		"RUNNER_TEMP": filepath.Join(dir, "temp"), "GITHUB_WORKSPACE": filepath.Join(dir, "empty-workspace"),
		"GITHUB_SERVER_URL": "file://" + filepath.Join(dir, "srv"), "GITHUB_REPOSITORY": "owner/demo", "GITHUB_SHA": sha,
		"VIBECI_TOKEN": "tok",
	}
	a, out := newTestAction(t, env)
	a.work = filepath.Join(env["RUNNER_TEMP"], "vibeci")
	os.MkdirAll(a.work, 0o700)
	overlay, err := decodeModels(a, actionModels)
	if err != nil {
		t.Fatal(err)
	}
	path, cfg, err := a.compose(context.Background(), overlay)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Repos[0]
	if r.Name != "demo" || r.Fork.URL != "file://"+bare || r.Fork.Branch != "main" || r.Fork.Auth == nil || r.Fork.Auth.Token.Env != "VIBECI_TOKEN" {
		t.Errorf("fork defaults: %+v %+v", r, r.Fork)
	}
	if cfg.StateRef != "refs/vibeci/state" || cfg.Sandbox.Mode != "docker" || cfg.Sandbox.Docker.Profiles["go"].Env["GOPROXY"] != "off" || cfg.DataDir != filepath.Join(a.work, "data") {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Providers["gw"].APIKey.Value != "sk-inline-secret-1234" {
		t.Error("models input not merged")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("composed config: %v %v", fi, err)
	}
	masks := out.String()
	for _, want := range []string{"sk-inline-secret-1234", "tenant-secret", "model-secret-id", "llm.gateway.example.invalid", "https://llm.gateway.example.invalid/tenant-7/anthropic/v1"} {
		if !strings.Contains(masks, "::add-mask::"+want+"\n") {
			t.Errorf("%q is not masked:\n%s", want, masks)
		}
	}
	if got := a.redact("POST https://llm.gateway.example.invalid/tenant-7/anthropic/v1/messages (model-secret-id)"); strings.Contains(got, "gateway") || strings.Contains(got, "secret-id") {
		t.Errorf("redact: %s", got)
	}

	// Two repos without fork.url cannot both be this repository.
	two := strings.Replace(actionConfig, `"repos": [{`, `"repos": [{"name": "x", "upstream": {"url": "https://example.invalid/x.git", "branch": "main"}}, {`, 1)
	cfgPath := filepath.Join(dir, "two.jsonc")
	os.WriteFile(cfgPath, []byte(two), 0o644)
	env["VIBECI_ACTION_CONFIG"] = cfgPath
	if _, _, err := a.compose(context.Background(), overlay); err == nil || !strings.Contains(err.Error(), "only one repo can default to this repository") {
		t.Errorf("two repos: %v", err)
	}
	env["VIBECI_ACTION_CONFIG"] = "../outside.jsonc"
	if _, _, err := a.compose(context.Background(), overlay); err == nil || !strings.Contains(err.Error(), "leaves the repository") {
		t.Errorf("path outside: %v", err)
	}
}

func decodeModels(a *ghAction, s string) (map[string]any, error) {
	a.getenv = wrapEnv(a.getenv, "VIBECI_ACTION_MODELS", s)
	m, err := config.Decode("models", []byte(s))
	if err == nil {
		a.maskModels(m)
	}
	return m, err
}

func wrapEnv(f func(string) string, k, v string) func(string) string {
	return func(key string) string {
		if key == k {
			return v
		}
		return f(key)
	}
}

// TestActionReport runs a stand-in for vibeci and checks the outputs,
// summary, annotations and exit status.
func TestActionReport(t *testing.T) {
	dir := t.TempDir()
	outcome := `[{"repo":"demo","status":"blocked","message":"the next upstream commit 1a2b3c4d5e is blocked (via https://llm.gateway.example.invalid/x)","commit":"","blocked":["1a2b3c4d5e6f"],"usage":{"calls":2,"input_tokens":1500,"output_tokens":20},"duration":"3s"}]`
	fake := filepath.Join(dir, "vibeci")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"args: $*\" >&2\ncat <<'EOF'\n"+outcome+"\nEOF\nexit 3\n"), 0o755)
	env := map[string]string{
		"RUNNER_TEMP": filepath.Join(dir, "temp"), "GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary.md"), "GITHUB_OUTPUT": filepath.Join(dir, "output"),
		"VIBECI_ACTION_CONFIG": filepath.Join(dir, "c.jsonc"), "VIBECI_ACTION_MODELS": actionModels, "VIBECI_ACTION_ARGS": "-force",
	}
	cfg := strings.Replace(actionConfig, `"upstream"`, `"fork": {"url": "https://example.invalid/fork.git", "branch": "main"}, "name": "demo", "upstream"`, 1)
	os.WriteFile(env["VIBECI_ACTION_CONFIG"], []byte(cfg), 0o644)
	a, out := newTestAction(t, env)
	a.exe = fake
	err := a.run(context.Background())
	var ec exitCode
	if !errors.As(err, &ec) || ec != 3 {
		t.Fatalf("exit: %v", err)
	}
	summary, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	output, _ := os.ReadFile(env["GITHUB_OUTPUT"])
	if !strings.Contains(string(summary), "| demo | blocked |") || !strings.Contains(string(summary), "blocked: 1a2b3c4d5e") || strings.Contains(string(summary), "gateway.example") {
		t.Errorf("summary:\n%s", summary)
	}
	if !strings.Contains(string(output), "status=blocked\n") || !strings.Contains(string(output), "json<<vibeci_") || strings.Contains(string(output), "gateway.example") {
		t.Errorf("outputs:\n%s", output)
	}
	var outs []map[string]any
	body := string(output)[strings.Index(string(output), "\n[")+1:]
	body = body[:strings.LastIndex(body, "]")+1]
	if json.Unmarshal([]byte(body), &outs) != nil || len(outs) != 1 {
		t.Errorf("json output: %q", body)
	}
	if !strings.Contains(out.String(), "::warning title=VibeCI%3A demo blocked::the next upstream commit") {
		t.Errorf("annotations:\n%s", out.String())
	}

	// A command the action does not run.
	env["VIBECI_ACTION_COMMAND"] = "daemon"
	if err := a.run(context.Background()); !errors.As(err, &ec) || ec != 2 {
		t.Errorf("daemon: %v", err)
	}
}
