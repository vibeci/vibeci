package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Source repositories are shallow, optionally partial (blob-less) bare
// clones of upstream projects that patch-mode forks apply their patches to.
// They hold one commit per fetched version; blobs of a partial repository
// are fetched in batches with FetchObjects, never lazily: every command that
// only reads sets GIT_NO_LAZY_FETCH, so a missing object is an error instead
// of a slow (or, on some servers, hanging) one-object fetch.

// noLazy disables on-demand fetching of missing objects.
var noLazy = []string{"GIT_NO_LAZY_FETCH=1"}

// InitSource creates or opens the bare repository dir whose remote "origin"
// is url. A partial repository fetches commits and trees only. An existing
// repository of the other kind is recreated (source repositories are
// caches); a changed url is just updated.
func (g *Git) InitSource(ctx context.Context, dir, url string, partial bool) (*Repo, error) {
	r := g.Open(dir)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		cur, err := r.ConfigGet(ctx, "extensions.partialclone")
		if err != nil {
			return nil, err
		}
		if (cur != "") != partial {
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if _, err := g.Run(ctx, Opts{}, "init", "--quiet", "--bare", dir); err != nil {
			return nil, err
		}
	}
	settings := [][2]string{{"remote.origin.url", url}}
	if partial {
		settings = append(settings,
			[2]string{"core.repositoryformatversion", "1"},
			[2]string{"extensions.partialclone", "origin"},
			[2]string{"remote.origin.promisor", "true"},
			[2]string{"remote.origin.partialclonefilter", "blob:none"})
	}
	for _, kv := range settings {
		if cur, err := r.ConfigGet(ctx, kv[0]); err != nil {
			return nil, err
		} else if cur == kv[1] {
			continue
		}
		if _, err := r.run(ctx, "config", kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// ConfigGet returns a repository config value ("" if unset).
func (r *Repo) ConfigGet(ctx context.Context, key string) (string, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "config", "--get", key)
	if ExitCode(err) == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchShallow fetches refspecs from the remote "origin" (whose URL is url;
// auth applies to it) at depth 1. In a partial repository no blobs are
// fetched.
func (r *Repo) FetchShallow(ctx context.Context, url string, auth *Auth, partial bool, refspecs ...string) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--depth=1"}
	if partial {
		args = append(args, "--filter=blob:none")
	}
	args = append(append(args, "--end-of-options", "origin"), refspecs...)
	if _, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, args...); err != nil {
		return fmt.Errorf("fetch %s: %w", redactURL(url), err)
	}
	return nil
}

// MissingObjects returns the ids among oids that are not in the local
// object store, without fetching anything.
func (r *Repo) MissingObjects(ctx context.Context, oids []string) ([]string, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	var in bytes.Buffer
	for _, o := range oids {
		if !IsHex(o) {
			return nil, fmt.Errorf("invalid object id %q", o)
		}
		in.WriteString(o + "\n")
	}
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Stdin: in.Bytes(), Env: noLazy}, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if oid, ok := strings.CutSuffix(line, " missing"); ok {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}

// fetchBatch is how many objects one FetchObjects request asks for.
const fetchBatch = 2000

// ErrFetchTimeout means a batch of objects did not arrive in time. Some
// servers (chromium.googlesource.com among them) are very slow to serve
// individual blobs of huge repositories.
var ErrFetchTimeout = errors.New("fetching objects timed out")

// FetchObjects fetches objects by id from the remote "origin" of a partial
// repository, in batches, each bounded by timeout (0: no bound).
func (r *Repo) FetchObjects(ctx context.Context, url string, auth *Auth, oids []string, timeout time.Duration) error {
	for len(oids) > 0 {
		n := min(len(oids), fetchBatch)
		batch := oids[:n]
		oids = oids[n:]
		var in bytes.Buffer
		for _, o := range batch {
			if !IsHex(o) {
				return fmt.Errorf("invalid object id %q", o)
			}
			in.WriteString(o + "\n")
		}
		fctx, cancel := ctx, context.CancelFunc(func() {})
		if timeout > 0 {
			fctx, cancel = context.WithTimeout(ctx, timeout)
		}
		_, err := r.G.Run(fctx, Opts{GitDir: r.GitDir, Stdin: in.Bytes(), Env: auth.env(url)},
			"-c", "fetch.negotiationAlgorithm=noop", "fetch", "--quiet", "--no-tags", "--no-recurse-submodules",
			"--no-write-fetch-head", "--filter=blob:none", "--stdin", "--end-of-options", "origin")
		timedOut := fctx.Err() != nil && ctx.Err() == nil
		cancel()
		if timedOut {
			return fmt.Errorf("%w: %d object(s) from %s did not arrive within %s", ErrFetchTimeout, len(batch), redactURL(url), timeout)
		}
		if err != nil {
			return fmt.Errorf("fetch objects from %s: %w", redactURL(url), err)
		}
	}
	return nil
}

// LsTreePaths returns the entries of the given paths in treeish (paths that
// do not exist are absent from the result). It does not descend into
// submodules: a path inside one is absent too.
func (r *Repo) LsTreePaths(ctx context.Context, treeish string, paths []string) (map[string]TreeEntry, error) {
	out := map[string]TreeEntry{}
	const chunk = 500 // keep the command line well below ARG_MAX
	for start := 0; start < len(paths); start += chunk {
		part := paths[start:min(start+chunk, len(paths))]
		args := append([]string{"ls-tree", "-z", "--full-tree", "--end-of-options", treeish, "--"}, part...)
		res, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: noLazy, LiteralPathspecs: true}, args...)
		if err != nil {
			return nil, err
		}
		for _, e := range parseLsTree(res) {
			out[e.Path] = e
		}
	}
	return out, nil
}

// LsTreeRecursive lists every entry under dir ("" for the whole tree) of
// treeish without fetching blobs (trees are always local).
func (r *Repo) LsTreeRecursive(ctx context.Context, treeish, dir string) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", "--end-of-options", treeish}
	if dir = strings.Trim(dir, "/"); dir != "" {
		args = append(args, "--", dir)
	}
	res, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: noLazy, LiteralPathspecs: true}, args...)
	if err != nil {
		return nil, err
	}
	return parseLsTree(res), nil
}

func parseLsTree(out []byte) []TreeEntry {
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		meta, p, ok := bytes.Cut(rec, []byte("\t"))
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			continue
		}
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: string(p)})
	}
	return entries
}

// ReadBlobs returns the contents of the given blobs, which must be present.
func (r *Repo) ReadBlobs(ctx context.Context, oids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(oids) == 0 {
		return out, nil
	}
	var in bytes.Buffer
	seen := map[string]bool{}
	for _, o := range oids {
		if !IsHex(o) {
			return nil, fmt.Errorf("invalid object id %q", o)
		}
		if !seen[o] {
			seen[o] = true
			in.WriteString(o + "\n")
		}
	}
	res, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Stdin: in.Bytes(), Env: noLazy}, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	objs, err := parseBatch(res)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if o.typ != "blob" {
			return nil, fmt.Errorf("object %s is a %s, not a blob", o.sha, o.typ)
		}
		out[o.sha] = o.data
	}
	return out, nil
}

// LsRemoteTags lists the tags of url: tag name -> commit id. With names,
// only those tags are listed (else all). Annotated tags are peeled.
func (r *Repo) LsRemoteTags(ctx context.Context, url string, auth *Auth, names ...string) (map[string]string, error) {
	args := []string{"ls-remote", "--tags", "--end-of-options", url}
	for _, n := range names {
		args = append(args, "refs/tags/"+n, "refs/tags/"+n+"^{}")
	}
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, args...)
	if err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", redactURL(url), err)
	}
	tags := map[string]string{}
	peeled := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || !IsHex(sha) {
			continue
		}
		name, ok := strings.CutPrefix(ref, "refs/tags/")
		if !ok {
			continue
		}
		if base, ok := strings.CutSuffix(name, "^{}"); ok {
			tags[base], peeled[base] = sha, true
			continue
		}
		if !peeled[name] {
			tags[name] = sha
		}
	}
	return tags, nil
}

// LsTreeSizes lists every blob of treeish with its size (TreeEntry.Size).
func (r *Repo) LsTreeSizes(ctx context.Context, treeish string) ([]TreeEntry, error) {
	res, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: noLazy}, "ls-tree", "-r", "-l", "-z", "--full-tree", "--end-of-options", treeish)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(res, []byte{0}) {
		meta, p, ok := bytes.Cut(rec, []byte("\t"))
		f := strings.Fields(string(meta))
		if !ok || len(f) != 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64) // "-" for submodules
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: string(p), Size: size})
	}
	return entries, nil
}

// HashFiles writes the given files as blob objects (no filters) and
// returns their ids in order.
func (r *Repo) HashFiles(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\x00") || !filepath.IsAbs(p) {
			return nil, fmt.Errorf("invalid path %q", p)
		}
		in.WriteString(p + "\n")
	}
	out, err := r.runIn(ctx, []byte(in.String()), "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	oids := strings.Fields(string(out))
	if len(oids) != len(paths) {
		return nil, fmt.Errorf("hash-object: %d ids for %d files", len(oids), len(paths))
	}
	return oids, nil
}

// HashBlob writes data as a blob object and returns its id.
func (r *Repo) HashBlob(ctx context.Context, data []byte) (string, error) {
	out, err := r.runIn(ctx, data, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IndexEntry is one path of a tree being built: Mode "" removes the path.
type IndexEntry struct {
	Path string
	Mode string
	OID  string
}

// UpdateTree returns the tree of base (a tree-ish) with entries changed.
func (r *Repo) UpdateTree(ctx context.Context, base string, entries []IndexEntry, scratch string) (string, error) {
	idx := filepath.Join(scratch, "update-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".index")
	defer os.Remove(idx)
	o := Opts{GitDir: r.GitDir, Index: idx}
	if _, err := r.G.Run(ctx, o, "read-tree", base); err != nil {
		return "", err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Path < entries[b].Path })
	var info strings.Builder
	zero := strings.Repeat("0", 40)
	if IsHex(base) && len(base) == 64 {
		zero = strings.Repeat("0", 64) // SHA-256 repository
	}
	for _, e := range entries {
		if strings.ContainsAny(e.Path, "\x00\n") {
			return "", fmt.Errorf("invalid path %q", e.Path)
		}
		if e.Mode == "" {
			fmt.Fprintf(&info, "0 %s\t%s\x00", zero, e.Path)
			continue
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", e.Mode, e.OID, e.Path)
	}
	o.Stdin = []byte(info.String())
	if _, err := r.G.Run(ctx, o, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Index: idx}, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
