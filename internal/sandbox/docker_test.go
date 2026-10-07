package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vibeci/vibeci/internal/config"
)

// Docker integration tests run when VIBECI_DOCKER_HOST is set, e.g.
//
//	VIBECI_DOCKER_HOST=unix://$HOME/.colima/default/docker.sock go test ./internal/sandbox -run Docker -v
//
// VIBECI_TEST_DATA must be a directory the docker VM can bind-mount (on
// Colima/Docker Desktop: somewhere under $HOME). VIBECI_TEST_IMAGE defaults
// to alpine:3.22.
func dockerTestSetup(t *testing.T) (*DockerProvider, string) {
	host := os.Getenv("VIBECI_DOCKER_HOST")
	if host == "" {
		t.Skip("VIBECI_DOCKER_HOST not set")
	}
	base := os.Getenv("VIBECI_TEST_DATA")
	if base == "" {
		t.Skip("VIBECI_TEST_DATA not set")
	}
	root, err := os.MkdirTemp(base, "sbx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	image := os.Getenv("VIBECI_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.22"
	}
	cfg := &config.Sandboxd{
		DockerHost:   host,
		DataRoot:     root,
		DataHostPath: root,
		Profiles: map[string]*config.Profile{
			"default": {Image: image, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), Memory: "512m", Pids: 256, TmpSize: "64m"},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := NewDockerProvider(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return p, root
}

func TestDockerSandboxPolicy(t *testing.T) {
	p, root := dockerTestSetup(t)
	ctx := context.Background()
	for _, d := range []string{"jobs/j1/work", "jobs/j1/cache", "mirrors/demo.git/objects"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "mirrors/demo.git/objects/marker"), []byte("obj"), 0o644)
	os.WriteFile(filepath.Join(root, "secret-outside"), []byte("nope"), 0o600)

	s, err := p.Create(ctx, CreateRequest{Profile: "default", Workspace: "jobs/j1/work", Objects: "mirrors/demo.git/objects", Cache: "jobs/j1/cache", Label: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	run := func(cmd string, timeout int) *ExecResult {
		t.Helper()
		res, err := s.Exec(ctx, ExecRequest{Command: cmd, TimeoutSec: timeout})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return res
	}
	if r := run("id -u", 30); strings.TrimSpace(r.Stdout) != fmt.Sprint(os.Getuid()) {
		t.Errorf("uid: %+v", r)
	}
	if r := run("grep CapEff /proc/self/status", 30); !strings.Contains(r.Stdout, "0000000000000000") {
		t.Errorf("capabilities not dropped: %s", r.Stdout)
	}
	if r := run("grep NoNewPrivs /proc/self/status", 30); !strings.Contains(r.Stdout, "1") {
		t.Errorf("no_new_privs not set: %s", r.Stdout)
	}
	if r := run("touch /etc/pwned", 30); r.ExitCode == 0 {
		t.Error("root filesystem is writable")
	}
	if r := run("ls /sys/class/net", 30); strings.TrimSpace(r.Stdout) != "lo" {
		t.Errorf("network interfaces present: %q", r.Stdout)
	}
	if r := run("echo hello > /workspace/out.txt && echo c > /cache/c && echo t > /tmp/t && cat /workspace/out.txt", 30); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "hello" {
		t.Errorf("workspace not writable: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "jobs/j1/work/out.txt")); strings.TrimSpace(string(b)) != "hello" {
		t.Errorf("workspace write not visible to host: %q", b)
	}
	objPath := filepath.Join(root, "mirrors/demo.git/objects")
	if r := run("cat "+objPath+"/marker && touch "+objPath+"/x", 30); !strings.Contains(r.Stdout, "obj") || r.ExitCode == 0 {
		t.Errorf("objects mount must be readable and read-only: %+v", r)
	}
	if r := run("ls "+root+"/secret-outside", 30); r.ExitCode == 0 {
		t.Error("data outside the requested mounts is visible")
	}
	start := time.Now()
	if r := run("sleep 60", 2); !r.TimedOut || time.Since(start) > 30*time.Second {
		t.Errorf("timeout not enforced: %+v after %s", r, time.Since(start))
	}
	if r := run("head -c 3000000 /dev/zero | tr '\\0' a", 30); !r.Truncated || len(r.Stdout) > 1100000 {
		t.Errorf("output not capped: truncated=%v len=%d", r.Truncated, len(r.Stdout))
	}
	if _, err := p.Create(ctx, CreateRequest{Profile: "missing", Workspace: "jobs/j1/work"}); err == nil {
		t.Error("unknown profile accepted")
	}
}

func TestDockerBrokerRoundTrip(t *testing.T) {
	p, root := dockerTestSetup(t)
	os.MkdirAll(filepath.Join(root, "jobs/j2/work"), 0o755)
	sockDir, err := os.MkdirTemp("/tmp", "vci")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Broker{P: p, Logger: slog.Default()}
	go b.Serve(ctx, sock)
	c := NewBrokerClient(sock)
	var herr error
	for i := 0; i < 50; i++ {
		if herr = c.Health(ctx); herr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if herr != nil {
		t.Fatal(herr)
	}
	s, err := c.Create(ctx, CreateRequest{Profile: "default", Workspace: "jobs/j2/work", Label: "broker"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Exec(ctx, ExecRequest{Command: "echo via-broker; exit 7"})
	if err != nil || res.ExitCode != 7 || strings.TrimSpace(res.Stdout) != "via-broker" {
		t.Fatalf("exec: %+v %v", res, err)
	}
	if _, err := c.Create(ctx, CreateRequest{Profile: "default", Workspace: "jobs/../../etc"}); err == nil {
		t.Error("traversal accepted by broker client")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.List()) != 0 {
		t.Errorf("sandbox not removed: %v", p.List())
	}
}

// TestDockerCleanupStaleIsScoped: a deployment starting up removes only
// its own leftover sandboxes, never another deployment's live ones.
func TestDockerCleanupStaleIsScoped(t *testing.T) {
	p, root := dockerTestSetup(t)
	ctx := context.Background()
	os.MkdirAll(filepath.Join(root, "jobs/j3/work"), 0o755)
	s, err := p.Create(ctx, CreateRequest{Profile: "default", Workspace: "jobs/j3/work", Label: "scoped"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	other, _ := dockerTestSetup(t) // another data location: another deployment
	if err := other.CleanupStale(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Exec(ctx, ExecRequest{Command: "true"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("another deployment removed this one's live sandbox: %+v %v", r, err)
	}
	restarted, err := NewDockerProvider(p.cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CleanupStale(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, ExecRequest{Command: "true"}); err == nil {
		t.Error("a leftover sandbox survived the restart of its own deployment")
	}
}
