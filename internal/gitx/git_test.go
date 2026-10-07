package gitx

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCommitsHostileMessage(t *testing.T) {
	r := NewTestRepo(t)
	base := r.Commit("base", map[string]string{"a.txt": "a\n"})
	evil := r.Commit("subject\x1fwith separator\n\nbody line\nauthor fake <x@y> 1 +0000", map[string]string{"a.txt": "b\n"})
	g := TestGit(t)
	repo := g.Open(filepath.Join(r.Dir, ".git"))
	cs, err := repo.ReadCommits(Ctx(), []string{base, evil})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[1].Parents[0] != base || cs[1].Author != "Test <test@example.com>" {
		t.Fatalf("parsed: %+v", cs[1])
	}
	if !strings.Contains(cs[1].Body, "author fake") || cs[1].Subject != "subject\x1fwith separator" {
		t.Errorf("message parse: %q / %q", cs[1].Subject, cs[1].Body)
	}
	if _, err := repo.ReadCommits(Ctx(), []string{"--output=/tmp/x"}); err == nil {
		t.Error("option-like commit id must be rejected")
	}
}

func TestDiffTreeAndPatch(t *testing.T) {
	r := NewTestRepo(t)
	a := r.Commit("a", map[string]string{"old.txt": strings.Repeat("same line\n", 20), "bin.dat": "x"})
	r.Git("mv", "old.txt", "new.txt")
	b := r.Commit("b", map[string]string{"bin.dat": "\x00\x01\x02binary", "add.txt": "hello\n"})
	repo := TestGit(t).Open(filepath.Join(r.Dir, ".git"))
	changes, err := repo.DiffTree(Ctx(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*FileChange{}
	for _, c := range changes {
		got[c.Path] = c
	}
	if c := got["new.txt"]; c == nil || c.Status != 'R' || c.OldPath != "old.txt" {
		t.Errorf("rename: %+v", c)
	}
	if c := got["bin.dat"]; c == nil || !c.Binary {
		t.Errorf("binary: %+v", c)
	}
	if c := got["add.txt"]; c == nil || c.Status != 'A' || c.Added != 1 {
		t.Errorf("add: %+v", c)
	}
	p, err := repo.Patch(Ctx(), a, b, 1<<20, "add.txt")
	if err != nil || !strings.Contains(string(p), "+hello") {
		t.Errorf("patch: %s %v", p, err)
	}
}

func TestMergeTreeConflicts(t *testing.T) {
	r := NewTestRepo(t)
	r.Commit("base", map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "keep\n"})
	r.Git("checkout", "-q", "-b", "fork")
	r.Commit("fork", map[string]string{"a.txt": "1\nFORK\n3\n", "b.txt": "\x00DELETE", "f.txt": "f\n"})
	r.Git("checkout", "-q", "main")
	r.Commit("up", map[string]string{"a.txt": "1\nUP\n3\n", "b.txt": "changed\n", "u.txt": "u\n"})
	repo := TestGit(t).Open(filepath.Join(r.Dir, ".git"))
	fork, _ := repo.Resolve(Ctx(), "fork")
	up, _ := repo.Resolve(Ctx(), "main")
	res, err := repo.MergeTree(Ctx(), fork, up)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean || strings.Join(res.ConflictedPaths(), ",") != "a.txt,b.txt" {
		t.Fatalf("conflicts: %+v", res)
	}
	data, found, err := repo.CatBlob(Ctx(), res.Tree+":a.txt", 0)
	if err != nil || !found || !strings.Contains(string(data), "||||||| ") {
		t.Errorf("expected zdiff3 markers, got %q %v", data, err)
	}
	if len(res.Messages) == 0 {
		t.Error("expected informational messages")
	}
	clean, err := repo.MergeTree(Ctx(), fork, fork)
	if err != nil || !clean.Clean {
		t.Errorf("self merge should be clean: %+v %v", clean, err)
	}
	if _, found, _ := repo.CatBlob(Ctx(), res.Tree+":missing.txt", 0); found {
		t.Error("missing path reported as found")
	}
}

func TestFetchPushAndTags(t *testing.T) {
	up := NewTestRepo(t)
	up.Commit("one", map[string]string{"a": "1"})
	up.Git("tag", "v1.0.0")
	up.Commit("two", map[string]string{"a": "2"})
	up.Git("tag", "v1.10.0")
	up.Git("tag", "v1.9.0", "HEAD~1")
	up.Commit("three", map[string]string{"a": "3"})
	up.Git("tag", "v2.0.0-rc1")

	g := TestGit(t)
	mirror, err := g.InitBare(Ctx(), filepath.Join(t.TempDir(), "m.git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mirror.Fetch(Ctx(), up.Dir, nil, "+refs/heads/main:refs/vibeci/up/main", "+refs/tags/*:refs/vibeci/up-tags/*"); err != nil {
		t.Fatal(err)
	}
	name, sha, err := mirror.LatestTag(Ctx(), "refs/vibeci/up-tags/", "v*")
	if err != nil || name != "v1.10.0" || sha != up.Git("rev-parse", "v1.10.0^{commit}") {
		t.Fatalf("latest tag = %s %s %v", name, sha, err)
	}
	if name, _, _ := mirror.LatestTag(Ctx(), "refs/vibeci/up-tags/", "v*-*"); name != "v2.0.0-rc1" {
		t.Errorf("explicit prerelease glob = %s", name)
	}

	// Push to a bare "fork" and verify non-fast-forward is rejected.
	fork, _ := g.InitBare(Ctx(), filepath.Join(t.TempDir(), "fork.git"))
	head, _ := mirror.Resolve(Ctx(), "refs/vibeci/up/main")
	if err := mirror.Push(Ctx(), fork.GitDir, nil, head, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	old, _ := mirror.Resolve(Ctx(), "refs/vibeci/up/main~2")
	if err := mirror.Push(Ctx(), fork.GitDir, nil, old, "refs/heads/main"); err == nil {
		t.Error("non-fast-forward push must fail")
	}
	if got, _ := mirror.LsRemote(Ctx(), fork.GitDir, nil, "refs/heads/main"); got != head {
		t.Errorf("ls-remote = %s", got)
	}
}

func TestAuthEnvScoping(t *testing.T) {
	a := &Auth{Token: "s3cret"}
	env := strings.Join(a.env("https://github.com/me/fork.git"), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.https://github.com/me/fork.git.extraHeader") || strings.Contains(env, "s3cret") {
		t.Errorf("env: %s", env)
	}
	err := &ExitError{Args: []string{"-c", "http.extraHeader=Authorization: Basic abc123"}, Code: 1}
	if strings.Contains(err.Error(), "abc123") {
		t.Errorf("secret leaked in error: %s", err)
	}
	if redactURL("https://user:pw@host/x") != "https://***@host/x" {
		t.Error("url redaction")
	}
}
