// Package scenario builds the deterministic git histories used by VibeCI's
// end-to-end tests: an upstream repository, a fork (bare, as a hosting
// service would hold it) and the fork maintainer's working clone.
package scenario

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a working repository with a scripted identity and clock.
type Repo struct {
	t     testing.TB
	Dir   string
	name  string
	email string
	day   *int
}

func gitEnv(name, email, date string) []string {
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + os.TempDir(),
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}
	if date != "" {
		env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	return env
}

// Git runs git in dir and returns trimmed output; failures are fatal.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv("Scenario", "scenario@example.invalid", "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Git runs git as the repo's author.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	*r.day++
	date := fmt.Sprintf("2026-%02d-%02dT10:00:00Z", 1+(*r.day-1)/28, 1+(*r.day-1)%28)
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = r.Dir
	cmd.Env = gitEnv(r.name, r.email, date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), r.Dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// As returns the same repository with another identity.
func (r *Repo) As(name, email string) *Repo {
	c := *r
	c.name, c.email = name, email
	return &c
}

// Write writes files (path -> content); content "" deletes the file.
func (r *Repo) Write(files map[string]string) {
	r.t.Helper()
	for p, c := range files {
		full := filepath.Join(r.Dir, filepath.FromSlash(p))
		if c == "" {
			os.Remove(full)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

// Commit writes files, stages everything and commits; returns the sha.
func (r *Repo) Commit(msg string, files map[string]string) string {
	r.t.Helper()
	r.Write(files)
	r.Git("add", "-A")
	r.Git("commit", "-q", "--allow-empty", "-m", msg)
	return r.Git("rev-parse", "HEAD")
}

// Push pushes the current branch to origin's main.
func (r *Repo) Push() {
	r.t.Helper()
	r.Git("push", "-q", "origin", "HEAD:main")
}

// Layout is a built scenario.
type Layout struct {
	// Upstream is the upstream working repository; its path is the
	// upstream remote URL.
	Upstream *Repo
	// ForkBare is the fork remote (a bare repository).
	ForkBare string
	// Fork is the fork maintainer's clone.
	Fork *Repo
}

// newLayout initializes upstream, then forks it after setup.
func newLayout(t testing.TB, dir string) *Layout {
	t.Helper()
	day := 0
	up := &Repo{t: t, Dir: filepath.Join(dir, "upstream"), name: "Upstream Dev", email: "dev@upstream.invalid", day: &day}
	if err := os.MkdirAll(up.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	up.Git("init", "-q", "-b", "main")
	return &Layout{Upstream: up, ForkBare: filepath.Join(dir, "fork.git")}
}

// fork creates the fork from upstream's current main.
func (l *Layout) fork(t testing.TB) {
	t.Helper()
	Git(t, filepath.Dir(l.ForkBare), "clone", "-q", "--bare", l.Upstream.Dir, l.ForkBare)
	work := filepath.Join(filepath.Dir(l.ForkBare), "fork-work")
	Git(t, filepath.Dir(l.ForkBare), "clone", "-q", l.ForkBare, work)
	l.Fork = &Repo{t: t, Dir: work, name: "Fork Maintainer", email: "me@fork.invalid", day: l.Upstream.day}
}

// ForkHead returns the fork's main.
func (l *Layout) ForkHead(t testing.TB) string {
	return Git(t, l.ForkBare, "rev-parse", "refs/heads/main")
}

// Show returns a file of the fork's main ("" if absent).
func (l *Layout) Show(t testing.TB, path string) string {
	t.Helper()
	cmd := exec.Command("git", "show", "refs/heads/main:"+path)
	cmd.Dir = l.ForkBare
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Has reports whether the fork's main has path.
func (l *Layout) Has(t testing.TB, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", "refs/heads/main:"+path)
	cmd.Dir = l.ForkBare
	return cmd.Run() == nil
}

// Contains reports whether commit is in the history of the fork's main.
func (l *Layout) Contains(t testing.TB, commit string) bool {
	t.Helper()
	cmd := exec.Command("git", "merge-base", "--is-ancestor", commit, "refs/heads/main")
	cmd.Dir = l.ForkBare
	return cmd.Run() == nil
}

// Checkout exports the fork's main into dir (for building it).
func (l *Layout) Checkout(t testing.TB, dir string) {
	t.Helper()
	Git(t, filepath.Dir(dir), "clone", "-q", l.ForkBare, dir)
}

// Bare makes bare copies of upstream and fork under dir (for serving them
// with git daemon) and returns their paths. Upstream serves partial and
// by-id fetches, which patch-mode forks use.
func (l *Layout) Bare(t testing.TB, dir string) (upstream, fork string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	upstream, fork = filepath.Join(dir, "upstream.git"), filepath.Join(dir, "fork.git")
	Git(t, dir, "clone", "-q", "--bare", l.Upstream.Dir, upstream)
	Git(t, upstream, "config", "uploadpack.allowFilter", "true")
	Git(t, upstream, "config", "uploadpack.allowAnySHA1InWant", "true")
	Git(t, dir, "clone", "-q", "--bare", l.ForkBare, fork)
	return upstream, fork
}
