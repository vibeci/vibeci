package source

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/patch"
)

func serve(r *gitx.TestRepo) string {
	r.Git("config", "uploadpack.allowFilter", "true")
	r.Git("config", "uploadpack.allowAnySHA1InWant", "true")
	return "file://" + r.Dir
}

// fixture: a main repository with a configured submodule ("sub"), a
// dependency pinned in a DEPS-style file ("third/dep") and a submodule
// without a source ("other").
func fixture(t *testing.T) (store *Store, mainURL string, subCommit string) {
	t.Helper()
	sub := gitx.NewTestRepo(t)
	subURL := serve(sub)
	subCommit = sub.Commit("sub", map[string]string{"x.txt": "sub file\n", "deep/z.txt": "zed\n"})
	dep := gitx.NewTestRepo(t)
	depURL := serve(dep)
	depCommit := dep.Commit("dep", map[string]string{"y.txt": "dep file\n"})

	m := gitx.NewTestRepo(t)
	mainURL = serve(m)
	m.Write(map[string]string{"a.txt": "main\n", "dir/b.txt": "bee\n", "DEPS": "vars = {\n  'dep_revision': '" + depCommit + "',\n}\n"})
	m.Git("add", "-A")
	m.Git("update-index", "--add", "--cacheinfo", "160000,"+subCommit+",sub")
	m.Git("update-index", "--add", "--cacheinfo", "160000,2222222222222222222222222222222222222222,other")
	m.Git("commit", "-q", "-m", "main")
	m.Git("tag", "v1")

	g := gitx.TestGit(t)
	store, err := NewStore(g, filepath.Join(t.TempDir(), "sources"), Spec{URL: mainURL}, []Spec{
		{Path: "third/dep", URL: depURL, RevisionFile: "DEPS", RevisionRegex: regexp.MustCompile(`'dep_revision': '([0-9a-f]+)'`)},
		{Path: "sub", URL: subURL},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, mainURL, subCommit
}

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	store, _, subCommit := fixture(t)
	mainM, err := store.Main(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := mainM.Tags(ctx)
	if err != nil || tags["v1"] == "" {
		t.Fatalf("tags = %v %v", tags, err)
	}
	commit, err := mainM.FetchTag(ctx, "v1")
	if err != nil || commit != tags["v1"] {
		t.Fatalf("fetch tag = %s %v", commit, err)
	}
	if have, _ := mainM.HasTag(ctx, "v1"); have != commit {
		t.Errorf("HasTag = %q", have)
	}
	if have, _ := mainM.HasTag(ctx, "v2"); have != "" {
		t.Errorf("HasTag(v2) = %q", have)
	}
	snap, err := store.Snapshot(ctx, "v1", commit)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Commit("sub") != subCommit {
		t.Errorf("sub commit = %s", snap.Commit("sub"))
	}
	if err := snap.Prefetch(ctx, []string{"a.txt", "sub/x.txt", "third/dep/y.txt", "missing.txt"}); err != nil {
		t.Fatal(err)
	}
	if snap.Fetched() != 4 { // DEPS (read for the revision) and the three files
		t.Errorf("fetched %d blobs", snap.Fetched())
	}
	src := snap.Bind(ctx)
	for p, want := range map[string]string{"a.txt": "main\n", "dir/b.txt": "bee\n", "sub/x.txt": "sub file\n", "sub/deep/z.txt": "zed\n", "third/dep/y.txt": "dep file\n"} {
		data, ok, err := src.ReadFile(p)
		if err != nil || !ok || string(data) != want {
			t.Errorf("%s = %q %v %v", p, data, ok, err)
		}
	}
	if _, ok, err := src.ReadFile("missing.txt"); ok || err != nil {
		t.Errorf("missing file: %v %v", ok, err)
	}
	_, _, err = src.ReadFile("other/inside.txt")
	var se *SubmoduleError
	if !errors.As(err, &se) || se.Submodule != "other" || !strings.Contains(err.Error(), `"path": "other"`) {
		t.Errorf("unconfigured submodule: %v", err)
	}
	if _, _, err := src.ReadFile("dir"); err == nil {
		t.Error("reading a directory must fail")
	}
	ents, err := snap.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range ents {
		paths = append(paths, e.Path)
	}
	if got := strings.Join(paths, " "); got != "DEPS a.txt dir/b.txt sub/deep/z.txt sub/x.txt third/dep/y.txt" {
		t.Errorf("list = %s", got)
	}
	if ents, _ := snap.List(ctx, "sub/deep"); len(ents) != 1 || ents[0].Path != "sub/deep/z.txt" {
		t.Errorf("list sub/deep = %+v", ents)
	}

	// A patch applies across repositories.
	p, err := patch.Parse([]byte("--- a/sub/x.txt\n+++ b/sub/x.txt\n@@ -1 +1 @@\n-sub file\n+patched\n"))
	if err != nil {
		t.Fatal(err)
	}
	tree := patch.NewTree(src)
	res, err := patch.Apply(p, tree, patch.Options{Strip: 1})
	if err != nil || res.Status() != patch.Exact {
		t.Fatalf("apply: %v %v", res, err)
	}
	if st, _ := tree.Get("sub/x.txt"); string(st.Data) != "patched\n" {
		t.Errorf("patched = %q", st.Data)
	}
}

func TestSnapshotErrors(t *testing.T) {
	ctx := context.Background()
	m := gitx.NewTestRepo(t)
	url := serve(m)
	m.Commit("main", map[string]string{"a.txt": "a\n"})
	m.Git("tag", "v1")
	store, err := NewStore(gitx.TestGit(t), t.TempDir(), Spec{URL: url}, []Spec{{Path: "nosub", URL: url}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mainM, _ := store.Main(ctx)
	commit, err := mainM.FetchTag(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(ctx, "v1", commit); err == nil || !strings.Contains(err.Error(), "no submodule entry at nosub") {
		t.Errorf("expected a missing submodule error, got %v", err)
	}
	if _, err := mainM.FetchTag(ctx, "--upload-pack=x"); err == nil {
		t.Error("option-like tag accepted")
	}
	if _, err := NewStore(gitx.TestGit(t), t.TempDir(), Spec{URL: url}, []Spec{{Path: "../x", URL: url}}, nil); err == nil {
		t.Error("escaping source path accepted")
	}
}
