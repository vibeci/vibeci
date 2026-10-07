package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// LocalProvider runs commands directly on the host in the workspace
// directory. It provides NO isolation and exists only for tests and local
// development on trusted repositories ("unsafe-local" mode).
type LocalProvider struct {
	DataRoot string
	// MaxTimeout clamps command timeouts (default 1h).
	MaxTimeout time.Duration
}

// passthroughEnv lists host variables unsafe-local sandboxes inherit.
var passthroughEnv = []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY", "GOFLAGS", "GOTOOLCHAIN", "GONOSUMDB", "GOPRIVATE"}

var localSeq struct {
	sync.Mutex
	n int
}

// Health implements Provider.
func (l *LocalProvider) Health(context.Context) error { return nil }

// Create implements Provider.
func (l *LocalProvider) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	ws := filepath.Join(l.DataRoot, filepath.FromSlash(req.Workspace))
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return nil, errors.New("workspace does not exist")
	}
	home, err := os.MkdirTemp("", "vibeci-local-home-")
	if err != nil {
		return nil, err
	}
	s := &localSandbox{dir: ws, home: home, max: l.MaxTimeout}
	if req.Cache != "" {
		// Like Docker bind mounts: the caller must create the directory.
		s.cache = filepath.Join(l.DataRoot, filepath.FromSlash(req.Cache))
		if st, err := os.Stat(s.cache); err != nil || !st.IsDir() {
			os.RemoveAll(home)
			return nil, errors.New("cache directory does not exist")
		}
	}
	localSeq.Lock()
	localSeq.n++
	s.info = Info{ID: "local-" + strconv.Itoa(localSeq.n), Profile: req.Profile, Image: "host", Network: "host"}
	localSeq.Unlock()
	return s, nil
}

type localSandbox struct {
	info  Info
	dir   string
	home  string
	cache string
	max   time.Duration
	mu    sync.Mutex
}

func (s *localSandbox) Info() Info { return s.info }

func (s *localSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	max := s.max
	if max <= 0 {
		max = time.Hour
	}
	if err := req.Validate(max); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", req.Command)
	cmd.Dir = filepath.Join(s.dir, filepath.FromSlash(req.Workdir))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.home,
		"TMPDIR=" + os.TempDir(),
		"LANG=C.UTF-8",
		"VIBECI_SANDBOX=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=vibeci-agent", "GIT_AUTHOR_EMAIL=agent@vibeci.invalid",
		"GIT_COMMITTER_NAME=vibeci-agent", "GIT_COMMITTER_EMAIL=agent@vibeci.invalid",
	}
	// Toolchain cache locations of the host (unsafe-local is a development
	// mode; reusing the host's caches keeps builds fast).
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if s.cache != "" {
		cmd.Env = append(cmd.Env, "VIBECI_CACHE="+s.cache)
	}
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, stderr := newCapBuffer(req.MaxOutput), newCapBuffer(req.MaxOutput)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
		timedOut = true
	}
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.Truncated() || stderr.Truncated(), TimedOut: timedOut, DurationMs: time.Since(start).Milliseconds()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 137
		}
	default:
		return nil, err
	}
	return res, nil
}

func (s *localSandbox) Close(context.Context) error {
	return os.RemoveAll(s.home)
}
