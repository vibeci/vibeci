package e2e

import (
	"bytes"
	"context"
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

// actionRun is one `vibeci action` step.
type actionRun struct {
	result
	outputs map[string]string
	summary string
}

// action runs `vibeci action` as action.yml does, on a fresh runner (an
// empty RUNNER_TEMP), with the given inputs (VIBECI_ACTION_* names without
// the prefix, e.g. "COMMAND=status").
func (h *harness) action(env map[string]string, inputs ...string) actionRun {
	h.t.Helper()
	temp := filepath.Join(h.dir, "runner-"+randSuffix())
	if err := os.MkdirAll(temp, 0o755); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, vibeciBinary(h.t), "action")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GITHUB_ACTIONS=true", "RUNNER_TEMP="+temp,
		"GITHUB_OUTPUT="+filepath.Join(temp, "output"), "GITHUB_STEP_SUMMARY="+filepath.Join(temp, "summary.md"))
	if gc, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
		cmd.Env = append(cmd.Env, "GOCACHE="+strings.TrimSpace(string(gc)))
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for _, in := range inputs {
		cmd.Env = append(cmd.Env, "VIBECI_ACTION_"+in)
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
		h.t.Fatalf("vibeci action: %v", err)
	}
	if f, ferr := os.OpenFile(h.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
		fmt.Fprintf(f, "\n### vibeci action %v (exit %d, %s)\n%s%s", inputs, code, time.Since(start).Round(time.Millisecond), out.String(), errb.String())
		f.Close()
	}
	h.t.Logf("vibeci action %v: exit %d in %s", inputs, code, time.Since(start).Round(100*time.Millisecond))
	r := actionRun{result: result{stdout: out.String(), stderr: errb.String(), code: code}, outputs: map[string]string{}}
	raw, _ := os.ReadFile(filepath.Join(temp, "output"))
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		k, v, ok := strings.Cut(lines[i], "=")
		if name, delim, heredoc := strings.Cut(lines[i], "<<"); heredoc && (!ok || len(name) < len(k)) {
			var body []string
			for i++; i < len(lines) && lines[i] != delim; i++ {
				body = append(body, lines[i])
			}
			r.outputs[name] = strings.Join(body, "\n")
			continue
		}
		if ok {
			r.outputs[k] = v
		}
	}
	sum, _ := os.ReadFile(filepath.Join(temp, "summary.md"))
	r.summary = string(sum)
	return r
}

func (h *harness) mustAction(want int, env map[string]string, inputs ...string) actionRun {
	h.t.Helper()
	r := h.action(env, inputs...)
	if r.code != want {
		h.t.Fatalf("vibeci action %v: exit %d, want %d\nstdout:\n%s\nstderr (tail):\n%s", inputs, r.code, want, r.stdout, tail(r.stderr, 6000))
	}
	return r
}

// TestAction runs the GitHub Action's command the way a scheduled workflow
// does: every run on a fresh runner, the config in the checked-out
// repository, the models from the models input, this repository as the
// fork, and the state kept in the fork (state_ref).
func TestAction(t *testing.T) {
	h := newHarness(t, "action")
	c := scenario.NewCalc(t, h.dir)
	h.fake.Script = c.Script()

	// The fork as GitHub serves it: <server>/<owner>/<repo>.git.
	srv := filepath.Join(h.dir, "srv")
	if err := os.MkdirAll(filepath.Join(srv, "owner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.ForkBare, filepath.Join(srv, "owner", "calc.git")); err != nil {
		t.Fatal(err)
	}
	repo := calcRepo(c)
	delete(repo, "fork")
	delete(repo, "name")
	cfg := map[string]any{
		"identity": map[string]any{"name": "VibeCI", "email": "vibeci@e2e.invalid"},
		"alerts":   []any{map[string]any{"type": "generic", "url": h.srv.URL + "/alerts", "events": []string{"*"}}},
		"repos":    []any{repo},
	}
	if sandboxMode() == "docker" {
		// The action's default profiles; action.sh builds the images and
		// the network.
		cfg["sandbox"] = map[string]any{"docker": map[string]any{"docker_host": dockerHost(t)}}
		ensureImage(t, "vibeci-sandbox:base", "deploy/sandbox/base.Dockerfile", "deploy/sandbox")
		if exec.Command("docker", "network", "inspect", "vibeci-egress").Run() != nil {
			if out, err := exec.Command("docker", "network", "create", "-o", "com.docker.network.bridge.enable_icc=false", "vibeci-egress").CombinedOutput(); err != nil {
				t.Fatalf("docker network create: %v %s", err, out)
			}
		}
	} else {
		cfg["sandbox"] = map[string]any{"mode": "unsafe-local"}
	}
	ws := filepath.Join(h.dir, "workspace")
	writeJSONFile(t, filepath.Join(ws, ".github", "vibeci.jsonc"), cfg)
	models, _ := json.Marshal(h.llmConfig(h.srv.URL))
	env := map[string]string{
		"GITHUB_WORKSPACE": ws, "GITHUB_SERVER_URL": "file://" + srv, "GITHUB_REPOSITORY": "owner/calc",
		"GITHUB_SHA": c.ForkHead(t), "VIBECI_TOKEN": "e2e-unused-token", "VIBECI_ACTION_MODELS": string(models),
	}
	stateRef := func() string {
		out, _ := exec.Command("git", "-C", c.ForkBare, "rev-parse", "--verify", "--quiet", "refs/vibeci/state").Output()
		return strings.TrimSpace(string(out))
	}

	r := h.mustAction(0, env)
	if r.outputs["status"] != "synced" || r.outputs["commit"] != c.ForkHead(t) || !strings.Contains(r.summary, "| calc | synced |") {
		t.Fatalf("first run: outputs %v\nsummary:\n%s", r.outputs, r.summary)
	}
	var outs []outcome
	if err := json.Unmarshal([]byte(r.outputs["json"]), &outs); err != nil || len(outs) != 1 || len(outs[0].Excluded) != 1 || outs[0].Excluded[0] != c.Malicious {
		t.Fatalf("json output: %v %s", err, r.outputs["json"])
	}
	var shown []string // what the log shows besides the masks themselves
	for _, line := range strings.Split(r.stdout+r.stderr+r.summary, "\n") {
		if !strings.HasPrefix(line, "::add-mask::") {
			shown = append(shown, line)
		}
	}
	if !strings.Contains(r.stdout, "::add-mask::e2e-fake-key-do-not-print\n") && !liveLLM() || strings.Contains(strings.Join(shown, "\n"), "e2e-fake-key-do-not-print") {
		t.Errorf("the inline API key is not masked:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "::notice title=VibeCI%3A calc synced::") {
		t.Errorf("annotations:\n%s", r.stdout)
	}
	if stateRef() == "" {
		t.Fatal("no state ref in the fork")
	}
	checkCalcResult(t, c, h.dir)

	// The next runner starts empty and loads the state from the fork.
	calls := len(h.fake.Calls())
	if r := h.mustAction(0, env); r.outputs["status"] != "up-to-date" {
		t.Fatalf("second run: %v", r.outputs)
	}
	if !liveLLM() && len(h.fake.Calls()) != calls {
		t.Error("an up-to-date run called the model")
	}
	excludedBy := func() string {
		r := h.mustAction(0, env, "COMMAND=status")
		var st struct {
			Repos map[string]struct {
				Excluded map[string]struct {
					Source  string `json:"source"`
					Removed bool   `json:"removed"`
				} `json:"excluded"`
			} `json:"repos"`
		}
		if err := json.Unmarshal([]byte(r.outputs["json"]), &st); err != nil {
			t.Fatalf("status json: %v %s", err, r.outputs["json"])
		}
		x, ok := st.Repos["calc"].Excluded[c.Malicious]
		if !ok || !x.Removed {
			return ""
		}
		return x.Source
	}
	if by := excludedBy(); by != "review" {
		t.Fatalf("exclusion after a fresh start: %q", by)
	}

	// The state ref is lost: the exclusion is recovered from the fork's
	// history by the next sync, and the ref is recreated.
	scenario.Git(t, c.ForkBare, "update-ref", "-d", "refs/vibeci/state")
	if r := h.mustAction(0, env); r.outputs["status"] != "up-to-date" || stateRef() == "" {
		t.Fatalf("run without state: %v", r.outputs)
	}
	if by := excludedBy(); by != "history" {
		t.Fatalf("exclusion not recovered from history: %q", by)
	}

	// Operator commands through the action: allow on one runner, the
	// restore on the next.
	r = h.mustAction(0, env, "COMMAND=allow", `ARGS=-reason "e2e: restore it" `+c.Malicious[:12])
	if !strings.Contains(r.outputs["json"], `"restore-scheduled"`) {
		t.Fatalf("allow: %v", r.outputs)
	}
	if r := h.mustAction(0, env); r.outputs["status"] != "synced" || !c.Has(t, "metrics.go") {
		t.Fatalf("restore: %v", r.outputs)
	}

	// Usage errors.
	if r := h.action(env, "COMMAND=daemon"); r.code != 2 || !strings.Contains(r.stdout, "::error title=VibeCI::command \"daemon\"") {
		t.Errorf("daemon: exit %d %s", r.code, r.stdout)
	}
}
