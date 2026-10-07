package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vibeci/vibeci/internal/config"
)

// DockerProvider creates sandboxes on a Docker engine under a fixed policy.
// It is used by the broker (and directly in "docker" mode).
type DockerProvider struct {
	cfg    *config.Sandboxd
	api    *dockerAPI
	logger *slog.Logger

	mu   sync.Mutex
	live map[string]*dockerSandbox
}

// NewDockerProvider connects to the engine described by cfg.
func NewDockerProvider(cfg *config.Sandboxd, logger *slog.Logger) (*DockerProvider, error) {
	api, err := newDockerAPI(cfg.DockerHost)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DockerProvider{cfg: cfg, api: api, logger: logger, live: map[string]*dockerSandbox{}}, nil
}

// Health pings the engine.
func (p *DockerProvider) Health(ctx context.Context) error {
	_, err := p.api.version(ctx)
	return err
}

// Version reports the engine version.
func (p *DockerProvider) Version(ctx context.Context) (string, error) { return p.api.version(ctx) }

// Instance identifies this deployment's sandboxes on a shared engine. Two
// deployments never share a data location, so it is derived from that.
func (p *DockerProvider) Instance() string {
	if p.cfg.DataVolume != "" {
		return "volume:" + p.cfg.DataVolume
	}
	return "path:" + p.cfg.DataHostPath
}

// CleanupStale removes sandbox containers that earlier runs of this
// deployment left behind. Sandboxes of other deployments on the same
// engine are left alone.
func (p *DockerProvider) CleanupStale(ctx context.Context) error {
	list, err := p.api.listManaged(ctx, p.Instance())
	if err != nil {
		return err
	}
	p.mu.Lock()
	known := map[string]bool{}
	for _, s := range p.live {
		known[s.containerID] = true
	}
	p.mu.Unlock()
	for _, c := range list {
		if known[c.ID] {
			continue
		}
		p.logger.Info("removing stale sandbox", "container", short(c.ID), "job", c.Labels["vibeci.job"])
		if err := p.api.removeContainer(ctx, c.ID); err != nil {
			p.logger.Warn("remove stale sandbox", "err", err)
		}
	}
	return nil
}

// Reap removes sandboxes older than the configured lifetime. Run it
// periodically.
func (p *DockerProvider) Reap(ctx context.Context) {
	p.mu.Lock()
	var old []*dockerSandbox
	for _, s := range p.live {
		if time.Since(s.created) > p.cfg.MaxLifetime.Duration {
			old = append(old, s)
		}
	}
	p.mu.Unlock()
	for _, s := range old {
		p.logger.Warn("reaping sandbox past max lifetime", "id", s.info.ID, "age", time.Since(s.created).Round(time.Second))
		s.Close(ctx)
	}
}

// Get returns a live sandbox by id.
func (p *DockerProvider) Get(id string) (*dockerSandbox, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.live[id]
	return s, ok
}

// List returns live sandbox infos.
func (p *DockerProvider) List() []Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Info
	for _, s := range p.live {
		out = append(out, s.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *DockerProvider) mount(rel, target string, readOnly bool) mount {
	if p.cfg.DataVolume != "" {
		return mount{Type: "volume", Source: p.cfg.DataVolume, Target: target, ReadOnly: readOnly,
			VolumeOptions: &volumeOptions{NoCopy: true, Subpath: rel}}
	}
	return mount{Type: "bind", Source: path.Join(p.cfg.DataHostPath, rel), Target: target, ReadOnly: readOnly}
}

// Create implements Provider.
func (p *DockerProvider) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	prof, ok := p.cfg.Profiles[req.Profile]
	if !ok {
		return nil, fmt.Errorf("%w: unknown profile %q", ErrInvalid, req.Profile)
	}
	p.mu.Lock()
	n := len(p.live)
	p.mu.Unlock()
	if n >= p.cfg.MaxSandboxes {
		return nil, fmt.Errorf("sandbox limit (%d) reached", p.cfg.MaxSandboxes)
	}

	exists, err := p.api.imageExists(ctx, prof.Image)
	if err != nil {
		return nil, err
	}
	if !exists {
		if prof.Pull != "missing" {
			return nil, fmt.Errorf("sandbox image %q not present (build it, or set pull: \"missing\" in the profile)", prof.Image)
		}
		p.logger.Info("pulling sandbox image", "image", prof.Image)
		if err := p.api.pull(ctx, prof.Image); err != nil {
			return nil, err
		}
	}
	if prof.Network != "none" {
		ok, err := p.api.networkExists(ctx, prof.Network)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("docker network %q for profile %q does not exist", prof.Network, req.Profile)
		}
	}

	mem, _ := config.ParseBytes(prof.Memory)
	tmp, _ := config.ParseBytes(prof.TmpSize)
	id := randomID()
	env := []string{
		"HOME=/tmp/home",
		"TMPDIR=/tmp",
		"XDG_CACHE_HOME=/tmp/cache",
		"VIBECI_SANDBOX=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=*",
		"GIT_AUTHOR_NAME=vibeci-agent", "GIT_AUTHOR_EMAIL=agent@vibeci.invalid",
		"GIT_COMMITTER_NAME=vibeci-agent", "GIT_COMMITTER_EMAIL=agent@vibeci.invalid",
	}
	keys := make([]string, 0, len(prof.Env))
	for k := range prof.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+prof.Env[k])
	}
	mounts := []mount{p.mount(req.Workspace, "/workspace", false)}
	if req.Objects != "" {
		// Same absolute path as in the harness so .git/objects/info/alternates resolves.
		mounts = append(mounts, p.mount(req.Objects, path.Join(p.cfg.DataRoot, req.Objects), true))
	}
	if req.Cache != "" {
		mounts = append(mounts, p.mount(req.Cache, "/cache", false))
		env = append(env, "VIBECI_CACHE=/cache")
	}
	spec := &containerSpec{
		Image:      prof.Image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"mkdir -p /tmp/home /tmp/cache; trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"},
		User:       prof.User,
		WorkingDir: "/workspace",
		Env:        env,
		Labels: map[string]string{
			"vibeci.managed":  "true",
			"vibeci.instance": p.Instance(),
			"vibeci.sandbox":  id,
			"vibeci.job":      req.Label,
			"vibeci.profile":  req.Profile,
		},
		HostConfig: hostConfig{
			NetworkMode:    prof.Network,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			Privileged:     false,
			Init:           true,
			IpcMode:        "private",
			CgroupnsMode:   "private",
			Memory:         mem,
			MemorySwap:     mem,
			NanoCpus:       int64(prof.CPUs * 1e9),
			PidsLimit:      prof.Pids,
			OomScoreAdj:    500,
			Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,exec,mode=1777,size=" + strconv.FormatInt(tmp, 10)},
			Mounts:         mounts,
			LogConfig:      logConfig{Type: "none"},
		},
	}
	name := "vibeci-" + sanitizeName(req.Label) + "-" + id[:8]
	cid, err := p.api.createContainer(ctx, name, spec)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	if err := p.api.startContainer(ctx, cid); err != nil {
		p.api.removeContainer(context.WithoutCancel(ctx), cid)
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	s := &dockerSandbox{
		p: p, containerID: cid, created: time.Now(),
		info: Info{ID: id, Profile: req.Profile, Image: prof.Image, Network: prof.Network},
	}
	// Probe for a timeout utility (busybox and coreutils both have one).
	if r, err := p.api.exec(ctx, cid, []string{"/bin/sh", "-c", "command -v timeout >/dev/null 2>&1"}, nil, "/workspace", 1024); err == nil && r.ExitCode == 0 {
		s.hasTimeout = true
	}
	p.mu.Lock()
	p.live[id] = s
	p.mu.Unlock()
	p.logger.Info("sandbox created", "id", id, "profile", req.Profile, "image", prof.Image, "network", prof.Network, "job", req.Label)
	return s, nil
}

type dockerSandbox struct {
	p           *DockerProvider
	containerID string
	info        Info
	created     time.Time
	hasTimeout  bool
	mu          sync.Mutex
	closed      bool
}

func (s *dockerSandbox) Info() Info { return s.info }

// Exec implements Sandbox. Commands run one at a time per sandbox.
func (s *dockerSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if err := req.Validate(s.p.cfg.MaxExecTimeout.Duration); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("sandbox closed")
	}
	cmd := []string{"/bin/sh", "-c", req.Command}
	if s.hasTimeout {
		cmd = append([]string{"timeout", "-s", "TERM", "-k", "10", strconv.Itoa(req.TimeoutSec)}, cmd...)
	}
	var env []string
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	workdir := "/workspace"
	if req.Workdir != "" {
		workdir = path.Join("/workspace", req.Workdir)
	}
	// Outer deadline in case the in-container timeout is missing or ignored.
	ectx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second+30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := s.p.api.exec(ectx, s.containerID, cmd, env, workdir, req.MaxOutput)
	if err != nil {
		if ctx.Err() == nil && ectx.Err() != nil {
			// Hard timeout: restart the container to kill everything in it.
			// The workspace mount survives; /tmp does not.
			s.p.logger.Warn("sandbox command exceeded hard deadline; restarting container", "id", s.info.ID)
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
			defer rcancel()
			if rerr := s.p.api.restartContainer(rctx, s.containerID); rerr != nil {
				return nil, fmt.Errorf("sandbox restart after timeout: %w", rerr)
			}
			return &ExecResult{ExitCode: 137, TimedOut: true, Stderr: "command killed: timeout (sandbox restarted, /tmp was cleared)", DurationMs: time.Since(start).Milliseconds()}, nil
		}
		return nil, err
	}
	if s.hasTimeout && (res.ExitCode == 124 || res.ExitCode == 137 || res.ExitCode == 143) && time.Since(start) >= time.Duration(req.TimeoutSec)*time.Second-time.Second {
		res.TimedOut = true
	}
	return res, nil
}

// Close removes the container.
func (s *dockerSandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.p.mu.Lock()
	delete(s.p.live, s.info.ID)
	s.p.mu.Unlock()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	err := s.p.api.removeContainer(rctx, s.containerID)
	s.p.logger.Info("sandbox removed", "id", s.info.ID, "err", err)
	return err
}

func randomID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func sanitizeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		out = "job"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
