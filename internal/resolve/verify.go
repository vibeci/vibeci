package resolve

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/sandbox"
)

// CheckResult is the outcome of one configured command.
type CheckResult struct {
	Name     string        `json:"name"`
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	TimedOut bool          `json:"timed_out,omitempty"`
	Duration time.Duration `json:"duration"`
	Output   string        `json:"-"`
}

// OK reports success.
func (c CheckResult) OK() bool { return c.ExitCode == 0 && !c.TimedOut }

// VerifyReport is the outcome of a check suite.
type VerifyReport struct {
	Stage   string // "prefetch" or "verify"
	Results []CheckResult
}

// Passed reports whether every command succeeded.
func (v *VerifyReport) Passed() bool {
	for _, r := range v.Results {
		if !r.OK() {
			return false
		}
	}
	return true
}

// Summary is a one-line description.
func (v *VerifyReport) Summary() string {
	var parts []string
	for _, r := range v.Results {
		status := "passed"
		if r.TimedOut {
			status = "timed out"
		} else if r.ExitCode != 0 {
			status = fmt.Sprintf("failed (exit %d)", r.ExitCode)
		}
		parts = append(parts, fmt.Sprintf("%s %s", r.Name, status))
	}
	if len(parts) == 0 {
		return "no checks configured"
	}
	return strings.Join(parts, ", ")
}

// Failure renders failing output for the agent (tail-heavy).
func (v *VerifyReport) Failure(limit int) string {
	var sb strings.Builder
	for _, r := range v.Results {
		if r.OK() {
			fmt.Fprintf(&sb, "[%s] `%s`: passed in %s\n", r.Name, r.Command, r.Duration.Round(time.Second))
			continue
		}
		fmt.Fprintf(&sb, "[%s] `%s`: FAILED (exit %d, timed out: %v)\n", r.Name, r.Command, r.ExitCode, r.TimedOut)
		out := r.Output
		if len(out) > limit {
			out = "[... output truncated ...]\n" + out[len(out)-limit:]
		}
		sb.WriteString(out)
		sb.WriteString("\n")
	}
	return sb.String()
}

// runCommands executes cmds in sb sequentially, stopping at the first
// failure.
func runCommands(ctx context.Context, sb sandbox.Sandbox, stage string, cmds []config.Command, env map[string]string) (*VerifyReport, error) {
	rep := &VerifyReport{Stage: stage}
	for _, c := range cmds {
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: c.Run, Env: env, TimeoutSec: int(c.Timeout.Seconds()), MaxOutput: 2 << 20})
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", stage, c.Name, err)
		}
		cr := CheckResult{Name: c.Name, Command: c.Run, ExitCode: res.ExitCode, TimedOut: res.TimedOut, Duration: time.Duration(res.DurationMs) * time.Millisecond, Output: res.Combined()}
		rep.Results = append(rep.Results, cr)
		if !cr.OK() {
			break
		}
	}
	return rep, nil
}

func (j *Job) newSandbox(ctx context.Context, profile string, ws *workspace, cache string) (sandbox.Sandbox, error) {
	// Docker refuses bind sources and volume subpaths that do not exist,
	// and the broker cannot create them (it never sees the data directory).
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	return j.Sandbox.Create(ctx, sandbox.CreateRequest{
		Profile:   profile,
		Workspace: ws.Rel,
		Objects:   j.objectsRel(),
		Cache:     j.relPath(cache),
		Label:     j.ID,
	})
}

// prefetch runs the configured dependency-fetch commands on ws in a
// sandbox using the prefetch profile (usually the one with network).
func (j *Job) prefetch(ctx context.Context, ws *workspace, cache string) (*VerifyReport, error) {
	if len(j.Repo.Prefetch) == 0 {
		return &VerifyReport{Stage: "prefetch"}, nil
	}
	sb, err := j.newSandbox(ctx, j.Repo.Sandbox.PrefetchProfile, ws, cache)
	if err != nil {
		return nil, err
	}
	defer closeSandbox(ctx, sb)
	return runCommands(ctx, sb, "prefetch", j.Repo.Prefetch, j.env)
}

// cleanVerify checks commit in a brand-new workspace and sandbox that the
// agent never touched: what passes here is what gets pushed.
func (j *Job) cleanVerify(ctx context.Context, commit string, round int) (*VerifyReport, error) {
	if len(j.Repo.Verify) == 0 || j.sub != nil {
		// Sub-jobs produce an intermediate upstream state; the final merge
		// into the fork is what gets verified.
		return &VerifyReport{Stage: "verify"}, nil
	}
	name := fmt.Sprintf("verify-%d", round)
	ws, err := j.prepareCheckout(ctx, name, commit)
	if err != nil {
		return nil, err
	}
	cache := j.path("cache-" + name)
	if rep, err := j.prefetch(ctx, ws, cache); err != nil {
		return nil, err
	} else if !rep.Passed() {
		return rep, nil
	}
	sb, err := j.newSandbox(ctx, j.Repo.Sandbox.VerifyProfile, ws, cache)
	if err != nil {
		return nil, err
	}
	defer closeSandbox(ctx, sb)
	j.logger().Info("clean-room verification", "repo", j.Repo.Name, "commit", short(commit), "round", round)
	return runCommands(ctx, sb, "verify", j.Repo.Verify, j.env)
}
