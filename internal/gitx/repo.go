package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repo is a repository addressed by its git dir (the harness's trusted
// mirrors are bare repositories).
type Repo struct {
	G      *Git
	GitDir string
}

// Open returns a Repo for gitDir.
func (g *Git) Open(gitDir string) *Repo { return &Repo{G: g, GitDir: gitDir} }

func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.G.Run(ctx, Opts{GitDir: r.GitDir}, args...)
}

func (r *Repo) runIn(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return r.G.Run(ctx, Opts{GitDir: r.GitDir, Stdin: stdin}, args...)
}

// InitBare creates a bare repository at dir if it does not exist.
func (g *Git) InitBare(ctx context.Context, dir string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return g.Open(dir), nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := g.Run(ctx, Opts{}, "init", "--quiet", "--bare", dir); err != nil {
		return nil, err
	}
	return g.Open(dir), nil
}

// Resolve resolves rev to a commit id. rev must come from the harness or be
// validated by the caller.
func (r *Repo) Resolve(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveObject resolves rev to any object id (or "" if missing).
func (r *Repo) ResolveObject(ctx context.Context, rev string) (string, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "rev-parse", "--verify", "--quiet", "--end-of-options", rev)
	if ExitCode(err) == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// MergeBase returns the best common ancestor ("" if unrelated).
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "merge-base", a, b)
	if ExitCode(err) == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// RevList runs rev-list and returns the ids.
func (r *Repo) RevList(ctx context.Context, args ...string) ([]string, error) {
	out, err := r.run(ctx, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// UpdateRef points ref at sha.
func (r *Repo) UpdateRef(ctx context.Context, ref, sha string) error {
	if !ValidRef(ref) {
		return fmt.Errorf("invalid ref %q", ref)
	}
	_, err := r.run(ctx, "update-ref", ref, sha)
	return err
}

// DeleteRef removes ref if present.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	if !ValidRef(ref) {
		return fmt.Errorf("invalid ref %q", ref)
	}
	_, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "update-ref", "-d", ref)
	if ExitCode(err) == 1 {
		return nil
	}
	return err
}

// EmptyTree returns the id of the empty tree in this repository's hash.
func (r *Repo) EmptyTree(ctx context.Context) (string, error) {
	out, err := r.runIn(ctx, []byte{}, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Commit is a parsed commit object.
type Commit struct {
	SHA        string
	Tree       string
	Parents    []string
	Author     string
	AuthorTime time.Time
	Committer  string
	CommitTime time.Time
	Subject    string
	Body       string
	Signed     bool
}

// ReadCommits parses commit objects via cat-file --batch (robust against
// hostile commit messages, unlike delimiter-based --format parsing).
func (r *Repo) ReadCommits(ctx context.Context, shas []string) ([]*Commit, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	var in bytes.Buffer
	for _, s := range shas {
		if !IsHex(s) {
			return nil, fmt.Errorf("invalid commit id %q", s)
		}
		in.WriteString(s + "\n")
	}
	out, err := r.runIn(ctx, in.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	objs, err := parseBatch(out)
	if err != nil {
		return nil, err
	}
	commits := make([]*Commit, 0, len(objs))
	for i, o := range objs {
		if o.typ != "commit" {
			return nil, fmt.Errorf("object %s is a %s, not a commit", shas[i], o.typ)
		}
		c := parseCommit(o.data)
		c.SHA = o.sha
		commits = append(commits, c)
	}
	return commits, nil
}

type batchObj struct {
	sha, typ string
	data     []byte
}

func parseBatch(out []byte) ([]batchObj, error) {
	var objs []batchObj
	for len(out) > 0 {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return nil, errors.New("cat-file: truncated header")
		}
		hdr := strings.Fields(string(out[:nl]))
		out = out[nl+1:]
		if len(hdr) == 2 && hdr[1] == "missing" {
			return nil, fmt.Errorf("object %s missing", hdr[0])
		}
		if len(hdr) != 3 {
			return nil, fmt.Errorf("cat-file: bad header %q", hdr)
		}
		size, err := strconv.Atoi(hdr[2])
		if err != nil || size < 0 || size+1 > len(out) {
			return nil, errors.New("cat-file: bad size")
		}
		objs = append(objs, batchObj{sha: hdr[0], typ: hdr[1], data: out[:size]})
		out = out[size+1:] // trailing LF
	}
	return objs, nil
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	hdr, msg, _ := bytes.Cut(data, []byte("\n\n"))
	var last string
	for _, line := range strings.Split(string(hdr), "\n") {
		if strings.HasPrefix(line, " ") {
			continue // continuation (e.g. gpgsig)
		}
		key, val, _ := strings.Cut(line, " ")
		last = key
		switch key {
		case "tree":
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author, c.AuthorTime = parseIdent(val)
		case "committer":
			c.Committer, c.CommitTime = parseIdent(val)
		case "gpgsig", "gpgsig-sha256":
			c.Signed = true
		}
	}
	_ = last
	m := string(msg)
	subject, body, _ := strings.Cut(m, "\n")
	c.Subject = strings.TrimSpace(subject)
	c.Body = strings.TrimSpace(body)
	return c
}

// parseIdent splits "Name <email> 1700000000 +0100".
func parseIdent(s string) (string, time.Time) {
	gt := strings.LastIndexByte(s, '>')
	if gt < 0 {
		return s, time.Time{}
	}
	who := s[:gt+1]
	rest := strings.Fields(s[gt+1:])
	var t time.Time
	if len(rest) >= 1 {
		if secs, err := strconv.ParseInt(rest[0], 10, 64); err == nil {
			t = time.Unix(secs, 0).UTC()
		}
	}
	return who, t
}

// TreeEntry is one entry of a recursive tree listing.
type TreeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
	Size int64 // LsTreeSizes only
}

// LsTree lists a tree recursively.
func (r *Repo) LsTree(ctx context.Context, treeish string) (map[string]TreeEntry, error) {
	out, err := r.run(ctx, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", treeish)
	if err != nil {
		return nil, err
	}
	m := map[string]TreeEntry{}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := bytes.Cut(rec, []byte("\t"))
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			continue
		}
		m[string(p)] = TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: string(p)}
	}
	return m, nil
}

// ListDir lists one directory of a tree (non-recursive).
func (r *Repo) ListDir(ctx context.Context, rev, dir string) ([]TreeEntry, error) {
	spec := rev + ":" + strings.Trim(dir, "/")
	if strings.Trim(dir, "/") == "" {
		spec = rev + "^{tree}"
	}
	out, err := r.run(ctx, "ls-tree", "-z", "-l", "--end-of-options", spec)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		meta, p, ok := bytes.Cut(rec, []byte("\t"))
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) < 3 {
			continue
		}
		e := TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: path.Join(strings.Trim(dir, "/"), string(p))}
		if len(f) == 4 && f[3] != "-" {
			e.Type += " " + f[3] + "B"
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// CatBlob reads a blob by object id or "rev:path". found is false when the
// path does not exist.
func (r *Repo) CatBlob(ctx context.Context, spec string, maxBytes int) (data []byte, found bool, err error) {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	typ, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1, 128}}, "cat-file", "-t", "--end-of-options", spec)
	if err != nil {
		if c := ExitCode(err); c == 1 || c == 128 {
			return nil, false, nil
		}
		return nil, false, err
	}
	if strings.TrimSpace(string(typ)) != "blob" {
		return nil, false, fmt.Errorf("%s is a %s, not a file", spec, strings.TrimSpace(string(typ)))
	}
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, MaxOut: maxBytes}, "cat-file", "blob", "--end-of-options", spec)
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// BlobHead returns up to n leading bytes of a blob.
func (r *Repo) BlobHead(ctx context.Context, oid string, n int) ([]byte, error) {
	return r.G.Run(ctx, Opts{GitDir: r.GitDir, MaxOut: n, Truncate: true}, "cat-file", "blob", "--end-of-options", oid)
}

// BlobSize returns the size of an object.
func (r *Repo) BlobSize(ctx context.Context, oid string) (int64, error) {
	out, err := r.run(ctx, "cat-file", "-s", "--end-of-options", oid)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// FileChange is one entry of a raw tree diff.
type FileChange struct {
	Status  byte // A, M, D, R, C, T
	OldMode string
	NewMode string
	OldOID  string
	NewOID  string
	OldPath string
	Path    string
	Added   int
	Deleted int
	Binary  bool
}

// DiffTree compares two tree-ish (a may be the empty tree) with rename
// detection and per-file line counts.
func (r *Repo) DiffTree(ctx context.Context, a, b string) ([]*FileChange, error) {
	raw, err := r.run(ctx, "diff-tree", "-r", "-z", "-M", "--raw", "--no-commit-id", "--end-of-options", a, b)
	if err != nil {
		return nil, err
	}
	changes, err := parseRawDiff(raw)
	if err != nil {
		return nil, err
	}
	num, err := r.run(ctx, "diff-tree", "-r", "-z", "-M", "--numstat", "--no-commit-id", "--end-of-options", a, b)
	if err != nil {
		return nil, err
	}
	stats := parseNumstat(num)
	for _, c := range changes {
		if s, ok := stats[c.Path]; ok {
			c.Added, c.Deleted, c.Binary = s.added, s.deleted, s.binary
		}
	}
	return changes, nil
}

func parseRawDiff(out []byte) ([]*FileChange, error) {
	var changes []*FileChange
	fields := bytes.Split(out, []byte{0})
	for i := 0; i < len(fields); i++ {
		rec := string(fields[i])
		if rec == "" {
			continue
		}
		if !strings.HasPrefix(rec, ":") {
			return nil, fmt.Errorf("diff-tree: unexpected record %q", truncate(rec, 80))
		}
		f := strings.Fields(rec[1:])
		if len(f) != 5 {
			return nil, fmt.Errorf("diff-tree: bad record %q", rec)
		}
		c := &FileChange{OldMode: f[0], NewMode: f[1], OldOID: f[2], NewOID: f[3], Status: f[4][0]}
		if i+1 >= len(fields) {
			return nil, errors.New("diff-tree: missing path")
		}
		i++
		c.Path = string(fields[i])
		if c.Status == 'R' || c.Status == 'C' {
			if i+1 >= len(fields) {
				return nil, errors.New("diff-tree: missing rename target")
			}
			i++
			c.OldPath, c.Path = c.Path, string(fields[i])
		}
		changes = append(changes, c)
	}
	return changes, nil
}

type numstat struct {
	added, deleted int
	binary         bool
}

func parseNumstat(out []byte) map[string]numstat {
	m := map[string]numstat{}
	fields := bytes.Split(out, []byte{0})
	for i := 0; i < len(fields); i++ {
		rec := string(fields[i])
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		var s numstat
		if parts[0] == "-" && parts[1] == "-" {
			s.binary = true
		} else {
			s.added, _ = strconv.Atoi(parts[0])
			s.deleted, _ = strconv.Atoi(parts[1])
		}
		p := parts[2]
		if p == "" && i+2 < len(fields) { // rename: "\0old\0new"
			p = string(fields[i+2])
			i += 2
		}
		m[p] = s
	}
	return m
}

// Patch returns a unified diff between a and b, optionally limited to paths.
func (r *Repo) Patch(ctx context.Context, a, b string, maxBytes int, paths ...string) ([]byte, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M", "--unified=3", "--end-of-options", a, b, "--"}
	args = append(args, paths...)
	return r.G.Run(ctx, Opts{GitDir: r.GitDir, MaxOut: maxBytes, LiteralPathspecs: true}, args...)
}

// RemergeDiff shows what a merge commit changed relative to an automatic
// re-merge of its parents: this is exactly where "evil merge" content hides.
func (r *Repo) RemergeDiff(ctx context.Context, sha string, maxBytes int) ([]byte, error) {
	return r.G.Run(ctx, Opts{GitDir: r.GitDir, MaxOut: maxBytes}, "show", "--remerge-diff", "--no-color", "--no-ext-diff", "--no-textconv", "--format=", "--end-of-options", sha)
}

// MergeTreeResult is the outcome of an in-object-store merge.
type MergeTreeResult struct {
	Tree      string
	Clean     bool
	Conflicts []ConflictStage
	Messages  []string
}

// ConflictStage is one index stage of a conflicted path.
type ConflictStage struct {
	Mode  string
	OID   string
	Stage int
	Path  string
}

// ConflictedPaths returns the distinct conflicted paths, sorted.
func (m *MergeTreeResult) ConflictedPaths() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range m.Conflicts {
		if !seen[c.Path] {
			seen[c.Path] = true
			out = append(out, c.Path)
		}
	}
	sort.Strings(out)
	return out
}

// MergeTree merges theirs into ours without a worktree (git merge-tree
// --write-tree). The resulting tree contains conflict markers for content
// conflicts.
func (r *Repo) MergeTree(ctx context.Context, ours, theirs string) (*MergeTreeResult, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, AllowExit: []int{1}}, "merge-tree", "--write-tree", "-z", "--end-of-options", ours, theirs)
	clean := err == nil
	if err != nil && ExitCode(err) != 1 {
		return nil, err
	}
	res := &MergeTreeResult{Clean: clean}
	// <tree>\0[<mode> <oid> <stage>\t<path>\0...]\0<messages>
	tree, rest, ok := bytes.Cut(out, []byte{0})
	if !ok {
		return nil, errors.New("merge-tree: unexpected output")
	}
	res.Tree = string(tree)
	for len(rest) > 0 {
		rec, after, _ := bytes.Cut(rest, []byte{0})
		rest = after
		if len(rec) == 0 {
			break // end of conflicted file info
		}
		meta, p, ok := bytes.Cut(rec, []byte("\t"))
		if !ok {
			return nil, fmt.Errorf("merge-tree: bad conflict record %q", rec)
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 {
			return nil, fmt.Errorf("merge-tree: bad conflict record %q", rec)
		}
		stage, _ := strconv.Atoi(f[2])
		res.Conflicts = append(res.Conflicts, ConflictStage{Mode: f[0], OID: f[1], Stage: stage, Path: string(p)})
	}
	// Informational messages: <n>\0<path>...\0<type>\0<message>\0
	fields := bytes.Split(rest, []byte{0})
	for i := 0; i < len(fields); {
		n, err := strconv.Atoi(string(fields[i]))
		if err != nil {
			break
		}
		j := i + 1 + n
		if j+1 >= len(fields) {
			break
		}
		res.Messages = append(res.Messages, strings.TrimSpace(string(fields[j+1])))
		i = j + 2
	}
	return res, nil
}

// CommitTree creates a commit object.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := r.runIn(ctx, []byte(message), append(args, "-F", "-")...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ForEachRef lists refs under prefix, sorted by sortKey (e.g. "-v:refname").
func (r *Repo) ForEachRef(ctx context.Context, prefix, sortKey string) ([]string, error) {
	args := []string{"for-each-ref", "--format=%(refname)"}
	if sortKey != "" {
		args = append(args, "--sort="+sortKey)
	}
	out, err := r.run(ctx, append(args, prefix)...)
	if err != nil {
		return nil, err
	}
	var refs []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			refs = append(refs, l)
		}
	}
	return refs, nil
}

// LatestTag returns the highest version tag under refPrefix (e.g.
// "refs/vibeci/upstream-tags/") whose short name matches glob. Pre-release
// tags (-rc, -alpha, -beta, ...) are skipped unless the glob mentions "-".
func (r *Repo) LatestTag(ctx context.Context, refPrefix, glob string) (name, sha string, err error) {
	refs, err := r.ForEachRef(ctx, refPrefix, "-v:refname")
	if err != nil {
		return "", "", err
	}
	for _, ref := range refs {
		short := strings.TrimPrefix(ref, refPrefix)
		if ok, _ := path.Match(glob, short); !ok {
			continue
		}
		if !strings.Contains(glob, "-") && isPrerelease(short) {
			continue
		}
		sha, err := r.Resolve(ctx, ref)
		if err != nil {
			continue // tag pointing at a non-commit
		}
		return short, sha, nil
	}
	return "", "", fmt.Errorf("no tag matching %q", glob)
}

func isPrerelease(tag string) bool {
	low := strings.ToLower(tag)
	for _, m := range []string{"-rc", "-alpha", "-beta", "-pre", "-dev", "-snapshot", "-nightly", ".rc", "rc."} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
