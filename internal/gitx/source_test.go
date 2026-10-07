package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sourceUpstream is an upstream that serves partial and by-id fetches, with
// a lightweight tag v1, an annotated tag v2 and a submodule entry.
func sourceUpstream(t *testing.T) *TestRepo {
	up := NewTestRepo(t)
	up.Git("config", "uploadpack.allowFilter", "true")
	up.Git("config", "uploadpack.allowAnySHA1InWant", "true")
	up.Commit("one", map[string]string{"a.txt": "one\n", "dir/b.txt": "bee\n", "we*rd.txt": "glob\n"})
	up.Git("tag", "v1")
	up.Git("update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,sub")
	up.Write(map[string]string{"a.txt": "two\n"})
	up.Git("add", "a.txt") // not -A: that would drop the submodule entry
	up.Git("commit", "-q", "-m", "two")
	up.Git("tag", "-a", "-m", "release 2", "v2")
	return up
}

func TestSourcePartialFetch(t *testing.T) {
	up := sourceUpstream(t)
	url := "file://" + up.Dir
	g := TestGit(t)
	dir := filepath.Join(t.TempDir(), "src.git")
	r, err := g.InitSource(Ctx(), dir, url, true)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := r.LsRemoteTags(Ctx(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags["v1"] != up.Git("rev-parse", "v1^{commit}") || tags["v2"] != up.Git("rev-parse", "v2^{commit}") || len(tags) != 2 {
		t.Fatalf("tags = %v", tags)
	}
	if one, _ := r.LsRemoteTags(Ctx(), url, nil, "v2"); len(one) != 1 || one["v2"] != tags["v2"] {
		t.Errorf("single tag = %v", one)
	}
	if err := r.FetchShallow(Ctx(), url, nil, true, "+refs/tags/v2:refs/vibeci/tags/v2"); err != nil {
		t.Fatal(err)
	}
	commit, err := r.Resolve(Ctx(), "refs/vibeci/tags/v2")
	if err != nil || commit != tags["v2"] {
		t.Fatalf("resolve = %s %v", commit, err)
	}
	ents, err := r.LsTreePaths(Ctx(), commit, []string{"a.txt", "dir/b.txt", "missing.txt", "sub", "sub/inner.txt", "we*rd.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 4 || ents["sub"].Mode != "160000" || ents["a.txt"].Type != "blob" || ents["we*rd.txt"].OID == "" {
		t.Fatalf("entries = %+v", ents)
	}
	oids := []string{ents["a.txt"].OID, ents["dir/b.txt"].OID}
	missing, err := r.MissingObjects(Ctx(), oids)
	if err != nil || len(missing) != 2 {
		t.Fatalf("a partial fetch must not fetch blobs: missing = %v, %v", missing, err)
	}
	if _, err := r.ReadBlobs(Ctx(), oids); err == nil {
		t.Fatal("reading a missing blob must fail, not fetch it")
	}
	if err := r.FetchObjects(Ctx(), url, nil, oids, time.Minute); err != nil {
		t.Fatal(err)
	}
	if missing, _ := r.MissingObjects(Ctx(), oids); len(missing) != 0 {
		t.Fatalf("still missing %v", missing)
	}
	blobs, err := r.ReadBlobs(Ctx(), oids)
	if err != nil || string(blobs[oids[0]]) != "two\n" || string(blobs[oids[1]]) != "bee\n" {
		t.Fatalf("blobs = %q %v", blobs, err)
	}
	all, err := r.LsTreeRecursive(Ctx(), commit, "dir")
	if err != nil || len(all) != 1 || all[0].Path != "dir/b.txt" {
		t.Errorf("recursive = %+v %v", all, err)
	}

	// A commit by id (the way sub-repositories are pinned).
	v1 := tags["v1"]
	if err := r.FetchShallow(Ctx(), url, nil, true, "+"+v1+":refs/vibeci/commits/"+v1); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Resolve(Ctx(), "refs/vibeci/commits/"+v1); got != v1 {
		t.Errorf("fetched commit = %s", got)
	}

	// Switching to a full repository recreates it.
	full, err := g.InitSource(Ctx(), dir, url, false)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := full.ConfigGet(Ctx(), "extensions.partialclone"); v != "" {
		t.Errorf("still partial: %q", v)
	}
	if _, err := full.Resolve(Ctx(), "refs/vibeci/tags/v2"); err == nil {
		t.Error("the partial repository should have been replaced")
	}
	if err := full.FetchShallow(Ctx(), url, nil, false, "+refs/tags/v1:refs/vibeci/tags/v1"); err != nil {
		t.Fatal(err)
	}
	c1, _ := full.Resolve(Ctx(), "refs/vibeci/tags/v1")
	e1, _ := full.LsTreePaths(Ctx(), c1, []string{"a.txt"})
	if b, err := full.ReadBlobs(Ctx(), []string{e1["a.txt"].OID}); err != nil || string(b[e1["a.txt"].OID]) != "one\n" {
		t.Errorf("full fetch blob = %q %v", b, err)
	}
	// Reopening with a new URL keeps the content.
	again, err := g.InitSource(Ctx(), dir, url+"/", false)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := again.ConfigGet(Ctx(), "remote.origin.url"); u != url+"/" {
		t.Errorf("url = %q", u)
	}
	if _, err := again.Resolve(Ctx(), "refs/vibeci/tags/v1"); err != nil {
		t.Error("reopening must keep fetched commits")
	}
}

func TestUpdateTree(t *testing.T) {
	src := NewTestRepo(t)
	head := src.Commit("base", map[string]string{"keep.txt": "k\n", "drop.txt": "d\n", "p/x.patch": "old\n"})
	r := TestGit(t).Open(filepath.Join(src.Dir, ".git"))
	oid, err := r.HashBlob(Ctx(), []byte("new\n"))
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	tree, err := r.UpdateTree(Ctx(), head, []IndexEntry{{Path: "p/x.patch", Mode: "100644", OID: oid}, {Path: "drop.txt"}, {Path: "p/new.patch", Mode: "100644", OID: oid}}, scratch)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := r.LsTree(Ctx(), tree)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for p := range ents {
		names = append(names, p)
	}
	if len(ents) != 3 || ents["p/x.patch"].OID != oid || ents["p/new.patch"].OID != oid || ents["keep.txt"].OID == "" {
		t.Fatalf("tree entries: %v", names)
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Errorf("index files left behind: %d", len(left))
	}
	if strings.Contains(tree, "\n") {
		t.Error("tree id")
	}
}
