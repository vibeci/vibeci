// Package sandbox provides isolated execution environments for the merge
// agent's shell and for build/test verification.
//
// The harness never talks to Docker itself. It asks a broker (vibeci
// sandboxd) for a sandbox by profile name; the broker owns the Docker socket
// and applies a fixed policy: no capabilities, no privilege escalation,
// read-only root filesystem, non-root user, no network unless the profile
// names one, resource limits, and only the requested job directory mounted.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// CreateRequest asks for a sandbox. Paths are relative to the data root and
// validated by the broker.
type CreateRequest struct {
	Profile string `json:"profile"`
	// Workspace is mounted read-write at /workspace (e.g. "jobs/<id>/work").
	Workspace string `json:"workspace"`
	// Objects is a mirror object store mounted read-only at its own absolute
	// path so git alternates resolve (e.g. "mirrors/<repo>.git/objects").
	Objects string `json:"objects,omitempty"`
	// Cache is mounted read-write at /cache (e.g. "jobs/<id>/cache-agent").
	Cache string `json:"cache,omitempty"`
	// Label is a job id for bookkeeping.
	Label string `json:"label,omitempty"`
}

// Info describes a created sandbox.
type Info struct {
	ID      string `json:"id"`
	Profile string `json:"profile"`
	Image   string `json:"image"`
	Network string `json:"network"`
}

// ExecRequest runs a shell command inside a sandbox.
type ExecRequest struct {
	// Command is run with /bin/sh -c.
	Command string `json:"command"`
	// Workdir is relative to /workspace.
	Workdir    string            `json:"workdir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec"`
	// MaxOutput caps stdout and stderr each (head and tail are kept).
	MaxOutput int `json:"max_output,omitempty"`
}

// ExecResult is the outcome of a command.
type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
}

// Combined renders stdout and stderr for humans and models.
func (r *ExecResult) Combined() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit code: %d", r.ExitCode)
	if r.TimedOut {
		sb.WriteString(" (timed out)")
	}
	fmt.Fprintf(&sb, ", %.1fs\n", float64(r.DurationMs)/1000)
	if r.Stdout != "" {
		sb.WriteString("--- stdout ---\n")
		sb.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			sb.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		sb.WriteString("--- stderr ---\n")
		sb.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Sandbox is a running environment.
type Sandbox interface {
	Info() Info
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Close(ctx context.Context) error
}

// Provider creates sandboxes.
type Provider interface {
	Create(ctx context.Context, req CreateRequest) (Sandbox, error)
	Health(ctx context.Context) error
}

var (
	workspaceRe = regexp.MustCompile(`^jobs/[A-Za-z0-9_-]{1,100}/[A-Za-z0-9_-]{1,60}$`)
	objectsRe   = regexp.MustCompile(`^mirrors/[A-Za-z0-9_-]{1,64}\.git/objects$`)
	profileRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	envKeyRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	labelRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,100}$`)
)

// ErrInvalid marks requests rejected by policy.
var ErrInvalid = errors.New("invalid sandbox request")

// Validate checks a create request against the path policy.
func (r *CreateRequest) Validate() error {
	if !profileRe.MatchString(r.Profile) {
		return fmt.Errorf("%w: bad profile name", ErrInvalid)
	}
	if !workspaceRe.MatchString(r.Workspace) {
		return fmt.Errorf("%w: workspace %q must look like jobs/<id>/<name>", ErrInvalid, r.Workspace)
	}
	if r.Objects != "" && !objectsRe.MatchString(r.Objects) {
		return fmt.Errorf("%w: objects %q must look like mirrors/<repo>.git/objects", ErrInvalid, r.Objects)
	}
	if r.Cache != "" && !workspaceRe.MatchString(r.Cache) {
		return fmt.Errorf("%w: cache %q must look like jobs/<id>/<name>", ErrInvalid, r.Cache)
	}
	if r.Cache != "" && r.Cache == r.Workspace {
		return fmt.Errorf("%w: cache and workspace must differ", ErrInvalid)
	}
	if !labelRe.MatchString(r.Label) {
		return fmt.Errorf("%w: bad label", ErrInvalid)
	}
	return nil
}

// Validate checks an exec request and clamps its timeout.
func (r *ExecRequest) Validate(maxTimeout time.Duration) error {
	if strings.TrimSpace(r.Command) == "" {
		return fmt.Errorf("%w: empty command", ErrInvalid)
	}
	if len(r.Command) > 256<<10 {
		return fmt.Errorf("%w: command too long", ErrInvalid)
	}
	if strings.ContainsRune(r.Command, 0) {
		return fmt.Errorf("%w: NUL in command", ErrInvalid)
	}
	if r.Workdir != "" {
		clean := path.Clean("/" + r.Workdir)
		if strings.Contains(r.Workdir, "..") || strings.ContainsRune(r.Workdir, 0) {
			return fmt.Errorf("%w: workdir must stay inside /workspace", ErrInvalid)
		}
		r.Workdir = strings.TrimPrefix(clean, "/")
	}
	if len(r.Env) > 64 {
		return fmt.Errorf("%w: too many env vars", ErrInvalid)
	}
	for k, v := range r.Env {
		if !envKeyRe.MatchString(k) || len(v) > 8192 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: bad env var %q", ErrInvalid, k)
		}
	}
	maxSec := int(maxTimeout / time.Second)
	if r.TimeoutSec <= 0 {
		r.TimeoutSec = 600
	}
	if maxSec > 0 && r.TimeoutSec > maxSec {
		r.TimeoutSec = maxSec
	}
	if r.MaxOutput <= 0 || r.MaxOutput > 8<<20 {
		r.MaxOutput = 1 << 20
	}
	return nil
}

// capBuffer keeps the first and last limit/2 bytes written to it.
type capBuffer struct {
	limit     int
	head      []byte
	tail      []byte // ring
	tailStart int
	total     int64
}

func newCapBuffer(limit int) *capBuffer {
	if limit < 64 {
		limit = 64
	}
	return &capBuffer{limit: limit}
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	c.total += int64(n)
	half := c.limit / 2
	if len(c.head) < half {
		k := min(half-len(c.head), len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	tailCap := c.limit - half
	for _, b := range p {
		if len(c.tail) < tailCap {
			c.tail = append(c.tail, b)
		} else {
			c.tail[c.tailStart] = b
			c.tailStart = (c.tailStart + 1) % tailCap
		}
	}
	return n, nil
}

func (c *capBuffer) Truncated() bool { return c.total > int64(len(c.head)+len(c.tail)) }

func (c *capBuffer) String() string {
	tail := append(append([]byte{}, c.tail[c.tailStart:]...), c.tail[:c.tailStart]...)
	if !c.Truncated() {
		return string(c.head) + string(tail)
	}
	omitted := c.total - int64(len(c.head)+len(tail))
	return string(c.head) + fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted) + string(tail)
}
