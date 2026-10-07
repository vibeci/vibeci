package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vibeci/vibeci/e2e/scenario"
)

// compose drives one docker compose project made of the production
// deploy/docker-compose.yml plus the test additions in e2e/compose.
type compose struct {
	t       *testing.T
	project string
	dir     string // project directory: relative paths in the files resolve here
	env     []string
}

// run runs `docker compose ...` and returns stdout, stderr and the exit code.
func (c *compose) run(args ...string) (string, string, int) {
	c.t.Helper()
	root := repoRoot(c.t)
	full := append([]string{"compose", "-p", c.project, "--project-directory", c.dir,
		"-f", filepath.Join(root, "deploy", "docker-compose.yml"),
		"-f", filepath.Join(root, "e2e", "compose", "docker-compose.e2e.yml")}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), c.env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errb.String(), ee.ExitCode()
	}
	if err != nil {
		c.t.Fatalf("docker compose %s: %v", strings.Join(args, " "), err)
	}
	return out.String(), errb.String(), 0
}

func (c *compose) must(args ...string) string {
	c.t.Helper()
	out, errs, code := c.run(args...)
	if code != 0 {
		c.t.Fatalf("docker compose %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

// vibeci runs the CLI in the running vibeci container.
func (c *compose) vibeci(cmd string, args ...string) result {
	c.t.Helper()
	full := append([]string{"exec", "-T", "vibeci", "vibeci", cmd, "-config", "/etc/vibeci/config.jsonc"}, args...)
	out, errs, code := c.run(full...)
	return result{stdout: out, stderr: errs, code: code}
}

// alive fails the test at once if a service stopped or keeps restarting.
func (c *compose) alive() {
	c.t.Helper()
	out, _, code := c.run("ps", "-a", "--format", "{{.Service}} {{.State}}")
	if code != 0 {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] != "running" {
			c.t.Fatalf("service %s is %s", f[0], f[1])
		}
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // umask
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b)+"\n", 0o644)
}

// waitFor polls cond every two seconds until it holds.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Second)
	}
}

// readAlerts reads the alerts the fake model's container has received
// (complete lines only: the last one may still be being written).
func readAlerts(t *testing.T, path string) []alert {
	t.Helper()
	b, _ := os.ReadFile(path)
	lines := strings.Split(string(b), "\n")
	var raws []json.RawMessage
	for _, line := range lines[:len(lines)-1] {
		if line != "" {
			raws = append(raws, json.RawMessage(line))
		}
	}
	return parseAlerts(t, raws)
}

// TestCompose deploys the production compose stack (harness and sandbox
// broker, hardened as shipped) against a git server and the fake model, and
// checks the daemon's first cycle (a merge-mode fork and a patch-mode
// fork), the CLI inside the container, and an operator round trip (allow,
// then SIGUSR1 to sync now).
func TestCompose(t *testing.T) {
	requireE2E(t)
	if os.Getenv("VIBECI_E2E_COMPOSE") != "1" {
		t.Skip("set VIBECI_E2E_COMPOSE=1 to run the docker compose test")
	}
	requireDocker(t)
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Fatalf("docker compose is required: %v\n%s", err, out)
	}
	// Images of code under test are always rebuilt (layer caching keeps
	// that cheap); the others only if missing.
	rebuild := os.Getenv("VIBECI_E2E_REBUILD")
	os.Setenv("VIBECI_E2E_REBUILD", "1")
	ensureImage(t, "vibeci:e2e", "deploy/Dockerfile", ".")
	ensureImage(t, "vibeci-e2e-fakellm", "e2e/compose/fakellm.Dockerfile", ".")
	os.Setenv("VIBECI_E2E_REBUILD", rebuild)
	ensureImage(t, "vibeci-e2e-gitd", "e2e/compose/gitd.Dockerfile", "e2e/compose")
	ensureImage(t, "vibeci-sandbox:go", "deploy/sandbox/go.Dockerfile", "deploy/sandbox")

	dir := workDir(t, "compose")
	calc := scenario.NewCalc(t, filepath.Join(dir, "scenario"))
	gitDir := filepath.Join(dir, "git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The containers use the repositories served by gitd; the assertions
	// read the served fork directly.
	_, servedFork := calc.Bare(t, gitDir)
	served := *calc.Layout
	served.ForkBare = servedFork
	c := *calc
	c.Layout = &served
	before := c.ForkHead(t)
	greet := scenario.NewGreet(t, filepath.Join(dir, "scenario-greet"))
	_, servedGreetFork := greet.Bare(t, filepath.Join(gitDir, "greet"))
	servedGreet := *greet.Layout
	servedGreet.ForkBare = servedGreetFork
	g := *greet
	g.Layout = &servedGreet
	greetBefore := g.ForkHead(t)

	suffix := randSuffix()
	volume := "vibeci-e2e-" + suffix + "-data"
	cp := &compose{t: t, project: "vibeci-e2e-" + suffix, dir: dir, env: []string{
		"VIBECI_IMAGE=vibeci:e2e", "VIBECI_DATA_VOLUME=" + volume,
		fmt.Sprintf("VIBECI_E2E_UID=%d", os.Getuid()), fmt.Sprintf("VIBECI_E2E_GID=%d", os.Getgid()),
	}}

	script := calc.Script()
	script.Patches = greet.Script().Patches
	writeJSONFile(t, filepath.Join(dir, "fakellm", "script.json"), script)
	writeFile(t, filepath.Join(dir, "secrets", "llm_api_key"), "e2e-fake-key", 0o644)
	writeFile(t, filepath.Join(dir, "secrets", "git_token"), "unused", 0o644)
	writeJSONFile(t, filepath.Join(dir, "sandboxd.jsonc"), map[string]any{
		"listen": "/run/vibeci/sandboxd.sock", "docker_host": "unix:///var/run/docker.sock",
		"data_root": "/data", "data_volume": volume, "max_sandboxes": 4,
		"profiles": map[string]any{"go": map[string]any{"image": "vibeci-sandbox:go", "memory": "4g", "cpus": 2,
			"env": map[string]any{"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild", "GOPROXY": "off", "GOFLAGS": "-mod=readonly"}}},
	})
	repo := calcRepo(&c)
	repo["fork"] = map[string]any{"url": "git://gitd/fork.git", "branch": "main"}
	repo["upstream"] = map[string]any{"url": "git://gitd/upstream.git", "branch": "main"}
	greetCfg := greetRepo(&g)
	greetCfg["fork"] = map[string]any{"url": "git://gitd/greet/fork.git", "branch": "main"}
	greetCfg["upstream"] = map[string]any{"url": "git://gitd/greet/upstream.git", "tags": "v*", "tag_format": "v{version}"}
	writeJSONFile(t, filepath.Join(dir, "config.jsonc"), map[string]any{
		"data_dir": "/data", "interval": "30m", "log_level": "info",
		"identity":  map[string]any{"name": "VibeCI", "email": "vibeci@e2e.invalid"},
		"providers": map[string]any{"fake": map[string]any{"type": "anthropic", "base_url": "http://fakellm:8080/v1", "api_key": "file:/run/secrets/llm_api_key"}},
		"models":    map[string]any{"fake": map[string]any{"provider": "fake", "model": "fake-model", "max_tokens": 8192}},
		"roles":     map[string]any{"triage": "fake", "investigate": "fake", "audit": "fake", "resolve": []string{"fake"}},
		"sandbox":   map[string]any{"mode": "broker", "socket": "/run/vibeci/sandboxd.sock"},
		"alerts":    []any{map[string]any{"type": "generic", "url": "http://fakellm:8080/alerts", "events": []string{"*"}}},
		"repos":     []any{repo, greetCfg},
	})

	t.Cleanup(func() {
		if t.Failed() {
			out, _, _ := cp.run("logs", "--no-color", "--tail", "300")
			t.Logf("compose logs:\n%s", out)
		}
		cp.run("down", "-v", "--remove-orphans", "--timeout", "20")
		// Sandboxes are created by sandboxd, not compose; remove any a
		// failed run left behind, then the data volume they hold.
		if ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=vibeci.instance=volume:"+volume).Output(); err == nil && len(bytes.TrimSpace(ids)) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, strings.Fields(string(ids))...)...).Run()
		}
		exec.Command("docker", "volume", "rm", "-f", volume).Run()
	})
	cp.must("up", "-d", "--no-build", "--pull", "never")
	alertsFile := filepath.Join(dir, "fakellm", "alerts.jsonl")

	// The daemon starts a cycle at startup.
	waitFor(t, "the first merge", 6*time.Minute, func() bool {
		cp.alive()
		return c.ForkHead(t) != before && countAlerts(readAlerts(t, alertsFile), "synced", "") > 0
	})
	checkCalcResult(t, &c, dir)
	if as := readAlerts(t, alertsFile); countAlerts(as, "blocked", "excluded") != 1 {
		t.Errorf("alerts: %+v", as)
	}
	waitFor(t, "the patch-mode update", 4*time.Minute, func() bool {
		cp.alive()
		return g.ForkHead(t) != greetBefore && countAlerts(readAlerts(t, alertsFile), "synced", "updated to upstream 1.1.0") > 0
	})
	if g.Show(t, "upstream-version.txt") != "1.1.0\n" || !strings.Contains(g.Show(t, "patches/0002-friendlier-greeting.patch"), "nice to meet you") || g.Has(t, "patches/0003-trim-names.patch") {
		t.Errorf("patch-mode fork after the update: %q\n%s", g.Show(t, "upstream-version.txt"), g.Show(t, "patches/series"))
	}

	var st struct {
		Repos map[string]struct {
			LastResult string `json:"last_result"`
			Excluded   map[string]struct {
				Removed bool `json:"removed"`
			} `json:"excluded"`
		} `json:"repos"`
	}
	waitFor(t, "status to report the sync", 2*time.Minute, func() bool {
		r := cp.vibeci("status", "-json")
		return r.code == 0 && json.Unmarshal([]byte(r.stdout), &st) == nil && st.Repos["calc"].LastResult == "synced"
	})
	if x, ok := st.Repos["calc"].Excluded[calc.Malicious]; !ok || !x.Removed {
		t.Fatalf("status: %+v", st)
	}
	var prep struct {
		Version string         `json:"version"`
		Counts  map[string]int `json:"counts"`
	}
	var pr result
	waitFor(t, "vibeci patches", 2*time.Minute, func() bool {
		pr = cp.vibeci("patches", "-repo", "greet", "-json")
		return pr.code == 0 || !strings.Contains(pr.stderr, "locked")
	})
	if pr.code != 0 || json.Unmarshal([]byte(pr.stdout), &prep) != nil || prep.Version != "1.1.0" || prep.Counts["exact"] != 2 {
		t.Errorf("patches in the container: exit %d: %s%s", pr.code, pr.stdout, pr.stderr)
	}
	if r := cp.vibeci("health"); r.code != 0 {
		t.Errorf("health: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	var chk struct {
		OK bool `json:"ok"`
	}
	if r := cp.vibeci("check", "-json"); r.code != 0 || json.Unmarshal([]byte(r.stdout), &chk) != nil || !chk.OK {
		t.Errorf("check: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}

	// Operator round trip inside the container: allow (retried while the
	// daemon holds the repo lock), then SIGUSR1 to sync now.
	var allow result
	waitFor(t, "allow", 2*time.Minute, func() bool {
		allow = cp.vibeci("allow", "-repo", "calc", "-json", "-reason", "e2e", calc.Malicious)
		return allow.code == 0 || !strings.Contains(allow.stderr, "locked")
	})
	if allow.code != 0 || !strings.Contains(allow.stdout, "restore-scheduled") {
		t.Fatalf("allow: exit %d: %s%s", allow.code, allow.stdout, allow.stderr)
	}
	cp.must("kill", "-s", "SIGUSR1", "vibeci")
	waitFor(t, "the restore", 4*time.Minute, func() bool {
		cp.alive()
		return c.Has(t, "metrics.go") && countAlerts(readAlerts(t, alertsFile), "synced", "restored upstream commit") > 0
	})
}
