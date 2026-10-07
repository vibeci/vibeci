// Package gitx runs git with a hardened environment: no system/global
// config, no hooks, no pagers, no credential helpers, no fsmonitor, no
// external diff/textconv drivers and no ext:: transport. Credentials are
// passed per invocation via environment-scoped config, never written to disk.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Identity is used for commits created by the harness.
type Identity struct {
	Name  string
	Email string
}

// Git is a hardened git runner.
type Git struct {
	Bin      string
	Home     string // HOME for git and ssh; holds nothing sensitive
	Identity Identity
	Logger   *slog.Logger
}

// New returns a runner. home is created if missing.
func New(home string, id Identity, logger *slog.Logger) (*Git, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not found: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Git{Bin: bin, Home: home, Identity: id, Logger: logger}, nil
}

// hardening flags prepended to every invocation.
var hardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.pager=cat",
	"-c", "core.quotePath=false",
	"-c", "core.autocrlf=false",
	"-c", "core.symlinks=true",
	"-c", "color.ui=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "advice.detachedHead=false",
	"-c", "submodule.recurse=false",
	"-c", "fetch.recurseSubmodules=false",
	"-c", "protocol.ext.allow=never",
	"-c", "credential.helper=",
	"-c", "commit.gpgSign=false",
	"-c", "tag.gpgSign=false",
	"-c", "rerere.enabled=false",
	"-c", "merge.conflictStyle=zdiff3",
	"-c", "merge.renames=true",
	"-c", "diff.renames=true",
	"-c", "init.defaultBranch=main",
	"-c", "versionsort.suffix=-",
	"-c", "pack.threads=2",
}

// Opts controls one invocation.
type Opts struct {
	Dir      string // working directory
	GitDir   string // GIT_DIR
	WorkTree string // GIT_WORK_TREE
	Index    string // GIT_INDEX_FILE
	Env      []string
	Stdin    []byte
	// MaxOut caps captured stdout (default 512 MiB); excess is an error
	// unless Truncate is set.
	MaxOut   int
	Truncate bool
	// LiteralPathspecs disables glob/magic interpretation of paths.
	LiteralPathspecs bool
	// AllowExit lists non-zero exit codes that are not errors.
	AllowExit []int
}

// ExitError is a failed git invocation.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(redactArgs(e.Args), " "), e.Code, strings.TrimSpace(truncate(e.Stderr, 2000)))
}

// ExitCode extracts the exit code from an *ExitError (or -1).
func ExitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

func (g *Git) env(o Opts) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.Home,
		"XDG_CONFIG_HOME=" + g.Home + "/.config",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=" + g.Identity.Name,
		"GIT_AUTHOR_EMAIL=" + g.Identity.Email,
		"GIT_COMMITTER_NAME=" + g.Identity.Name,
		"GIT_COMMITTER_EMAIL=" + g.Identity.Email,
		"SSH_ASKPASS=/bin/false",
		"GIT_ASKPASS=/bin/false",
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	if o.GitDir != "" {
		env = append(env, "GIT_DIR="+o.GitDir)
	}
	if o.WorkTree != "" {
		env = append(env, "GIT_WORK_TREE="+o.WorkTree)
	}
	if o.Index != "" {
		env = append(env, "GIT_INDEX_FILE="+o.Index)
	}
	if o.LiteralPathspecs {
		env = append(env, "GIT_LITERAL_PATHSPECS=1")
	}
	return append(env, o.Env...)
}

// Run executes git and returns stdout.
func (g *Git) Run(ctx context.Context, o Opts, args ...string) ([]byte, error) {
	full := append(append([]string{}, hardening...), args...)
	cmd := exec.CommandContext(ctx, g.Bin, full...)
	cmd.Env = g.env(o)
	cmd.Dir = o.Dir
	if cmd.Dir == "" {
		cmd.Dir = g.Home
	}
	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}
	maxOut := o.MaxOut
	if maxOut <= 0 {
		maxOut = 512 << 20
	}
	stdout := &limitedBuffer{limit: maxOut}
	var stderr limitedBuffer
	stderr.limit = 1 << 20
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	g.Logger.Debug("git", "args", strings.Join(redactArgs(args), " "), "dur", time.Since(start).Round(time.Millisecond))
	if stdout.overflow {
		if o.Truncate {
			return stdout.Bytes(), nil
		}
		return nil, fmt.Errorf("git %s: output exceeds %d bytes", firstArg(args), maxOut)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			for _, ok := range o.AllowExit {
				if code == ok {
					return stdout.Bytes(), &ExitError{Args: args, Code: code, Stderr: stderr.String()}
				}
			}
			return nil, &ExitError{Args: args, Code: code, Stderr: stderr.String()}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", firstArg(args), ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %w", firstArg(args), err)
	}
	return stdout.Bytes(), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

var secretArg = regexp.MustCompile(`(?i)(authorization:\s*\S+\s+)\S+`)

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = secretArg.ReplaceAllString(a, "${1}***")
	}
	return out
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		room := b.limit - b.Len()
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.overflow = true
		return len(p), nil // keep draining so the child does not block
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var (
	hexRe = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
	// refRe matches the refs the harness itself creates.
	refRe = regexp.MustCompile(`^refs/[A-Za-z0-9._/+-]+$`)
)

// IsHex reports whether s is a (possibly abbreviated) lowercase object id.
func IsHex(s string) bool { return hexRe.MatchString(s) }

// ValidRef reports whether s looks like a full ref name without tricks.
func ValidRef(s string) bool {
	return refRe.MatchString(s) && !strings.Contains(s, "..") && !strings.HasSuffix(s, ".lock") && !strings.Contains(s, "@{")
}
