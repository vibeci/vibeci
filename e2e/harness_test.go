// Package e2e runs the vibeci binary end to end against real git
// repositories: a scripted fake model by default, real models on request,
// with the unsafe-local sandbox, the embedded docker sandbox, or the full
// docker compose deployment.
//
// The tests are skipped unless VIBECI_E2E=1. Knobs:
//
//	VIBECI_E2E=1                  enable the end-to-end tests
//	VIBECI_E2E_SANDBOX=docker     run sandboxes in docker (default: unsafe-local)
//	VIBECI_E2E_LLM_CONFIG=file    use real models: a JSONC file with "providers",
//	                              "models" and "roles" (see e2e/llm.example.jsonc);
//	                              relative paths are relative to the repository root
//	VIBECI_E2E_COMPOSE=1          also run the docker compose deployment test
//	VIBECI_E2E_OSS=1              also run the fatih/color test (network; real models)
//	VIBECI_E2E_DIR=dir            parent of the work dirs (default .tmp-test/e2e;
//	                              docker must be able to bind-mount it)
//	VIBECI_E2E_KEEP=1             keep work dirs after passing tests
//	DOCKER_HOST                   docker engine (default: current docker context)
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vibeci/vibeci/e2e/fakellm"
	"github.com/vibeci/vibeci/internal/jsonc"
)

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("VIBECI_E2E") != "1" {
		t.Skip("end-to-end tests are disabled; set VIBECI_E2E=1")
	}
}

func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// vibeciBinary builds cmd/vibeci once per test process.
func vibeciBinary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vibeci-e2e-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "vibeci")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/vibeci")
		cmd.Dir = repoRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func randSuffix() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// workDir creates a fresh directory that docker can bind-mount.
func workDir(t *testing.T, name string) string {
	t.Helper()
	parent := os.Getenv("VIBECI_E2E_DIR")
	if parent == "" {
		parent = filepath.Join(repoRoot(t), ".tmp-test", "e2e")
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(parent, name+"-")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ = filepath.EvalSymlinks(dir)
	t.Cleanup(func() {
		if t.Failed() || os.Getenv("VIBECI_E2E_KEEP") == "1" {
			t.Logf("work dir kept: %s", dir)
			return
		}
		removeAll(dir)
	})
	return dir
}

// removeAll also removes read-only trees (Go's module cache).
func removeAll(p string) {
	if os.RemoveAll(p) == nil {
		return
	}
	filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o755)
		}
		return nil
	})
	os.RemoveAll(p)
}

func sandboxMode() string {
	if m := os.Getenv("VIBECI_E2E_SANDBOX"); m != "" {
		return m
	}
	return "local"
}

func liveLLM() bool { return os.Getenv("VIBECI_E2E_LLM_CONFIG") != "" }

// dockerHost returns the engine address for vibeci's own docker client.
func dockerHost(t testing.TB) string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	return "unix:///var/run/docker.sock"
}

func requireDocker(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Fatalf("docker is required: %v\n%s", err, out)
	}
}

var imageMu sync.Mutex

// ensureImage builds an image from the repository unless it exists (set
// VIBECI_E2E_REBUILD=1 to always rebuild).
func ensureImage(t *testing.T, tag, dockerfile, contextDir string) {
	t.Helper()
	imageMu.Lock()
	defer imageMu.Unlock()
	if os.Getenv("VIBECI_E2E_REBUILD") != "1" {
		if exec.Command("docker", "image", "inspect", tag).Run() == nil {
			return
		}
	}
	t.Logf("building %s", tag)
	cmd := exec.Command("docker", "build", "-q", "-f", dockerfile, "-t", tag, contextDir)
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build %s: %v\n%s", tag, err, out)
	}
}

// harness runs vibeci against one scenario.
type harness struct {
	t    *testing.T
	dir  string
	cfg  string
	fake *fakellm.Server // model (unless live) and alert sink
	srv  *httptest.Server
	log  string
}

func newHarness(t *testing.T, name string) *harness {
	t.Helper()
	requireE2E(t)
	h := &harness{t: t, dir: workDir(t, name), fake: &fakellm.Server{}}
	h.srv = httptest.NewServer(h.fake)
	t.Cleanup(h.srv.Close)
	h.cfg = filepath.Join(h.dir, "config.json")
	h.log = filepath.Join(h.dir, "vibeci.log")
	if sandboxMode() == "docker" {
		requireDocker(t)
		ensureImage(t, "vibeci-sandbox:go", "deploy/sandbox/go.Dockerfile", "deploy/sandbox")
	}
	return h
}

// llmConfig returns providers, models and roles: the fake model, or the
// live configuration from VIBECI_E2E_LLM_CONFIG.
func (h *harness) llmConfig(fakeURL string) map[string]any {
	if f := os.Getenv("VIBECI_E2E_LLM_CONFIG"); f != "" {
		if !filepath.IsAbs(f) {
			f = filepath.Join(repoRoot(h.t), f) // relative to the repository root
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			h.t.Fatal(err)
		}
		clean, err := jsonc.Standardize(raw)
		if err != nil {
			h.t.Fatalf("%s: %v", f, err)
		}
		var frag map[string]any
		if err := json.Unmarshal(clean, &frag); err != nil {
			h.t.Fatalf("%s: %v", f, err)
		}
		out := map[string]any{}
		for _, k := range []string{"providers", "models", "roles"} {
			if frag[k] == nil {
				h.t.Fatalf("%s: %q is required", f, k)
			}
			out[k] = frag[k]
		}
		return out
	}
	return map[string]any{
		"providers": map[string]any{"fake": map[string]any{"type": "anthropic", "base_url": fakeURL + "/v1", "api_key": "e2e-fake-key-do-not-print"}},
		"models":    map[string]any{"fake": map[string]any{"provider": "fake", "model": "fake-model", "max_tokens": 8192}},
		"roles":     map[string]any{"triage": "fake", "investigate": "fake", "audit": "fake", "resolve": []string{"fake"}},
	}
}

// sandboxConfig returns the sandbox section for the selected mode.
func (h *harness) sandboxConfig() map[string]any {
	if sandboxMode() != "docker" {
		return map[string]any{"mode": "unsafe-local"}
	}
	goEnv := map[string]any{"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild", "GOPROXY": "off", "GOFLAGS": "-mod=readonly"}
	return map[string]any{"mode": "docker", "docker": map[string]any{
		"docker_host": dockerHost(h.t),
		"profiles": map[string]any{
			"go":        map[string]any{"image": "vibeci-sandbox:go", "memory": "4g", "cpus": 2, "env": goEnv},
			"go-egress": map[string]any{"image": "vibeci-sandbox:go", "network": "bridge", "memory": "4g", "cpus": 2, "env": map[string]any{"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild"}},
		},
	}}
}

// writeConfig writes the harness config for repo.
func (h *harness) writeConfig(repo map[string]any, mutate ...func(map[string]any)) {
	h.t.Helper()
	cfg := map[string]any{
		"data_dir":  filepath.Join(h.dir, "data"),
		"interval":  "30m",
		"log_level": "info",
		"identity":  map[string]any{"name": "VibeCI", "email": "vibeci@e2e.invalid"},
		"sandbox":   h.sandboxConfig(),
		"alerts":    []any{map[string]any{"type": "generic", "url": h.srv.URL + "/alerts", "events": []string{"*"}}},
		"repos":     []any{repo},
	}
	for k, v := range h.llmConfig(h.srv.URL) {
		cfg[k] = v
	}
	for _, m := range mutate {
		m(cfg)
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(h.cfg, b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// result of one vibeci invocation.
type result struct {
	stdout string
	stderr string
	code   int
}

func (r result) json(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
		t.Fatalf("invalid JSON output (%v):\n%s", err, r.stdout)
	}
}

// vibeci runs the binary with the harness config.
func (h *harness) vibeci(args ...string) result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, vibeciBinary(h.t), args...)
	cmd.Env = append(os.Environ(), "VIBECI_CONFIG="+h.cfg, "GOTOOLCHAIN=local")
	if gc, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
		cmd.Env = append(cmd.Env, "GOCACHE="+strings.TrimSpace(string(gc)))
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("vibeci %s: %v", strings.Join(args, " "), err)
	}
	if f, ferr := os.OpenFile(h.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
		fmt.Fprintf(f, "\n### vibeci %s (exit %d, %s)\n%s", strings.Join(args, " "), code, time.Since(start).Round(time.Millisecond), errb.String())
		f.Close()
	}
	h.t.Logf("vibeci %s: exit %d in %s", strings.Join(args, " "), code, time.Since(start).Round(100*time.Millisecond))
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// must runs vibeci and fails unless it exits with want.
func (h *harness) must(want int, args ...string) result {
	h.t.Helper()
	r := h.vibeci(args...)
	if r.code != want {
		h.t.Fatalf("vibeci %s: exit %d, want %d\nstdout:\n%s\nstderr (tail):\n%s", strings.Join(args, " "), r.code, want, r.stdout, tail(r.stderr, 6000))
	}
	return r
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}

// alert is the generic webhook payload.
type alert struct {
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Repo     string         `json:"repo"`
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Commits  []string       `json:"commits"`
	Details  map[string]any `json:"details"`
}

func parseAlerts(t testing.TB, raws []json.RawMessage) []alert {
	t.Helper()
	var out []alert
	for _, r := range raws {
		var a alert
		if err := json.Unmarshal(r, &a); err != nil {
			t.Fatalf("bad alert %s: %v", r, err)
		}
		out = append(out, a)
	}
	return out
}

func (h *harness) alerts() []alert { return parseAlerts(h.t, h.fake.Alerts()) }

func countAlerts(as []alert, kind, titleContains string) int {
	n := 0
	for _, a := range as {
		if a.Kind == kind && strings.Contains(a.Title, titleContains) {
			n++
		}
	}
	return n
}

// outcome mirrors pipeline.Outcome's JSON.
type outcome struct {
	Repo     string   `json:"repo"`
	Status   string   `json:"status"`
	Message  string   `json:"message"`
	JobID    string   `json:"job_id"`
	Target   string   `json:"target"`
	Commit   string   `json:"commit"`
	Blocked  []string `json:"blocked"`
	Excluded []string `json:"excluded"`
	Restored []string `json:"restored"`
	Pending  int      `json:"pending"`
	Merges   int      `json:"merges"`
	// Patch mode.
	Version string `json:"version"`
	Pinned  string `json:"pinned"`
	Patches *struct {
		Total, Exact, Shifted, Refreshed, Agent, Dropped int
	} `json:"patches"`
	Usage struct {
		Calls int `json:"calls"`
	} `json:"usage"`
}

func (h *harness) run(want int, args ...string) outcome {
	h.t.Helper()
	r := h.must(want, append([]string{"run", "-json"}, args...)...)
	var outs []outcome
	r.json(h.t, &outs)
	if len(outs) != 1 {
		h.t.Fatalf("expected one outcome, got %s", r.stdout)
	}
	h.t.Logf("outcome: %s: %s", outs[0].Status, outs[0].Message)
	return outs[0]
}

// goRun runs a go command in dir.
func goRun(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
