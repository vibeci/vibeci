package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepo is a scratch non-bare repository for tests.
type TestRepo struct {
	T   testing.TB
	Dir string
}

// NewTestRepo creates an empty repository with branch main.
func NewTestRepo(t testing.TB) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	r := &TestRepo{T: t, Dir: dir}
	r.Git("init", "-q", "-b", "main")
	return r
}

// Git runs a git command in the test repo and returns trimmed stdout.
func (r *TestRepo) Git(args ...string) string {
	r.T.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false"}, args...)...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write writes files (path -> content); empty content with a "\x00DELETE"
// marker deletes the file.
func (r *TestRepo) Write(files map[string]string) {
	r.T.Helper()
	for p, c := range files {
		full := filepath.Join(r.Dir, p)
		if c == "\x00DELETE" {
			os.Remove(full)
			continue
		}
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			r.T.Fatal(err)
		}
	}
}

// Commit writes files, stages everything and commits; returns the sha.
func (r *TestRepo) Commit(msg string, files map[string]string) string {
	r.T.Helper()
	r.Write(files)
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", msg)
	return r.Git("rev-parse", "HEAD")
}

// TestGit returns a hardened runner for tests.
func TestGit(t testing.TB) *Git {
	t.Helper()
	g, err := New(t.TempDir(), Identity{Name: "VibeCI Test", Email: "vibeci@test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Ctx returns a background context (tests).
func Ctx() context.Context { return context.Background() }
