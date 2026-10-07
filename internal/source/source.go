// Package source keeps local copies of the upstream source trees that
// patch-mode forks apply their patches to. A source is the main upstream
// repository plus optional sub-repositories that appear at paths inside it
// (for Chromium: DEPS dependencies such as v8, which Chromium's tree records
// as submodule entries). Copies are shallow and by default partial: only
// the commits and trees of the versions in use are fetched, and blobs only
// for the files that are read.
package source

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vibeci/vibeci/internal/gitx"
)

// Spec describes one source repository.
type Spec struct {
	// Path is where the repository's tree appears in the source tree; ""
	// for the main repository.
	Path string
	URL  string
	Auth *gitx.Auth
	// Full fetches trees with all their blobs instead of a blob-less
	// partial clone (for servers that are slow to serve single blobs).
	Full bool
	// RevisionFile and RevisionRegex locate a sub-repository's commit in
	// the tree that contains it: the first group of the first match in
	// that file (relative to the containing repository). By default the
	// commit is the submodule entry (gitlink) at Path.
	RevisionFile  string
	RevisionRegex *regexp.Regexp
}

// Store holds the source repositories of one fork.
type Store struct {
	G      *gitx.Git
	Dir    string // e.g. <data_dir>/sources/<repo>
	Logger *slog.Logger
	// FetchTimeout bounds one batch of blob fetches (default 10m).
	FetchTimeout time.Duration

	main    Spec
	subs    []Spec // sorted by path depth
	mu      sync.Mutex
	mirrors map[string]*Mirror
}

// NewStore returns a store for the main repository and sub-repositories.
func NewStore(g *gitx.Git, dir string, main Spec, subs []Spec, logger *slog.Logger) (*Store, error) {
	if main.Path != "" {
		return nil, errors.New("internal error: the main source has no path")
	}
	seen := map[string]bool{}
	for _, s := range subs {
		if s.Path == "" || path.Clean(s.Path) != s.Path || strings.HasPrefix(s.Path, "/") || strings.HasPrefix(s.Path, "..") {
			return nil, fmt.Errorf("source path %q must be a clean relative path", s.Path)
		}
		if seen[s.Path] {
			return nil, fmt.Errorf("source path %q is configured twice", s.Path)
		}
		seen[s.Path] = true
	}
	subs = append([]Spec(nil), subs...)
	sort.SliceStable(subs, func(a, b int) bool { return strings.Count(subs[a].Path, "/") < strings.Count(subs[b].Path, "/") })
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{G: g, Dir: dir, Logger: logger, main: main, subs: subs, mirrors: map[string]*Mirror{}}, nil
}

// Mirror is the local copy of one source repository.
type Mirror struct {
	Spec Spec
	Repo *gitx.Repo
	s    *Store
}

func mirrorName(p string) string {
	if p == "" {
		return "upstream"
	}
	sum := sha1.Sum([]byte(p))
	var b strings.Builder
	for _, r := range p {
		if r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	return name + "-" + hex.EncodeToString(sum[:4])
}

// mirror opens (creating if needed) the repository of spec.
func (s *Store) mirror(ctx context.Context, spec Spec) (*Mirror, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.mirrors[spec.Path]; m != nil {
		return m, nil
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	r, err := s.G.InitSource(ctx, filepath.Join(s.Dir, mirrorName(spec.Path)+".git"), spec.URL, !spec.Full)
	if err != nil {
		return nil, err
	}
	m := &Mirror{Spec: spec, Repo: r, s: s}
	s.mirrors[spec.Path] = m
	return m, nil
}

// Main returns the main repository's mirror.
func (s *Store) Main(ctx context.Context) (*Mirror, error) { return s.mirror(ctx, s.main) }

// Tags lists the remote's tags (name -> commit), or only the named ones.
func (m *Mirror) Tags(ctx context.Context, names ...string) (map[string]string, error) {
	return m.Repo.LsRemoteTags(ctx, m.Spec.URL, m.Spec.Auth, names...)
}

func tagRef(name string) (string, error) {
	ref := "refs/vibeci/tags/" + name
	if !gitx.ValidRef("refs/tags/"+name) || strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("invalid tag name %q", name)
	}
	return ref, nil
}

// FetchTag fetches the commit of tag name (shallow) and returns its id.
func (m *Mirror) FetchTag(ctx context.Context, name string) (string, error) {
	ref, err := tagRef(name)
	if err != nil {
		return "", err
	}
	if err := m.Repo.FetchShallow(ctx, m.Spec.URL, m.Spec.Auth, !m.Spec.Full, "+refs/tags/"+name+":"+ref); err != nil {
		return "", err
	}
	return m.Repo.Resolve(ctx, ref)
}

// HasTag returns the commit a previously fetched tag pointed at ("" if
// it was never fetched).
func (m *Mirror) HasTag(ctx context.Context, name string) (string, error) {
	ref, err := tagRef(name)
	if err != nil {
		return "", err
	}
	return m.Repo.ResolveObject(ctx, ref+"^{commit}")
}

// FetchCommit makes commit available (shallow), fetching it by id unless
// it was fetched before.
func (m *Mirror) FetchCommit(ctx context.Context, commit string) error {
	if !gitx.IsHex(commit) || (len(commit) != 40 && len(commit) != 64) {
		return fmt.Errorf("invalid commit id %q", commit)
	}
	ref := "refs/vibeci/commits/" + commit
	if have, err := m.Repo.ResolveObject(ctx, ref+"^{commit}"); err != nil {
		return err
	} else if have == commit {
		return nil
	}
	if err := m.Repo.FetchShallow(ctx, m.Spec.URL, m.Spec.Auth, !m.Spec.Full, "+"+commit+":"+ref); err != nil {
		return fmt.Errorf("%w (the server must allow fetching commits by id; GitHub does)", err)
	}
	return nil
}

// fetchBlobs makes blobs present, in batches.
func (m *Mirror) fetchBlobs(ctx context.Context, oids []string) error {
	if len(oids) == 0 {
		return nil
	}
	missing, err := m.Repo.MissingObjects(ctx, oids)
	if err != nil || len(missing) == 0 {
		return err
	}
	if m.Spec.Full {
		return fmt.Errorf("internal error: %d object(s) missing from a full source copy of %s", len(missing), m.Spec.URL)
	}
	timeout := m.s.FetchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	start := time.Now()
	err = m.Repo.FetchObjects(ctx, m.Spec.URL, m.Spec.Auth, missing, timeout)
	if errors.Is(err, gitx.ErrFetchTimeout) {
		return fmt.Errorf("%w. Some servers (chromium.googlesource.com among them) serve single files of huge repositories very slowly: use a mirror such as https://github.com/chromium/chromium for the source's url, or set its fetch to \"full\"", err)
	}
	if err == nil {
		m.s.Logger.Debug("fetched source blobs", "url", m.Spec.URL, "count", len(missing), "took", time.Since(start).Round(time.Millisecond))
	}
	return err
}

// SubmoduleError reports a path inside a sub-repository that has no
// configured source.
type SubmoduleError struct {
	Path      string // the path that was read
	Submodule string // the submodule's path in the tree
	Commit    string // the submodule's commit
}

func (e *SubmoduleError) Error() string {
	return fmt.Sprintf("%s is inside %s, a separate repository (submodule at commit %s). Add a source for it: {\"path\": %q, \"url\": \"<a mirror of that repository>\"} in the repo's patches.sources", e.Path, e.Submodule, short(e.Commit), e.Submodule)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// part is one repository of a snapshot.
type part struct {
	path   string // "" or the sub-repository's path
	mirror *Mirror
	commit string
}

// entry is a cached file lookup.
type entry struct {
	exists bool
	mode   string
	oid    string
	data   []byte
	loaded bool
	err    error
}

// Snapshot is the source tree at one version. It implements patch.Source
// (see Bind).
type Snapshot struct {
	Label  string
	parts  []*part // longest path first
	mu     sync.Mutex
	files  map[string]*entry
	fetchN int
}

// Commit returns the commit of the repository at path ("" for the main
// one) in the snapshot.
func (s *Snapshot) Commit(p string) string {
	for _, pt := range s.parts {
		if pt.path == p {
			return pt.commit
		}
	}
	return ""
}

// Parts returns "path@commit" for every repository of the snapshot.
func (s *Snapshot) Parts() []string {
	var out []string
	for i := len(s.parts) - 1; i >= 0; i-- {
		p := s.parts[i]
		name := p.path
		if name == "" {
			name = "(main)"
		}
		out = append(out, name+"@"+short(p.commit))
	}
	return out
}

// Snapshot builds the source tree with the main repository at commit (which
// must have been fetched) and every sub-repository at the commit its parent
// pins, fetching sub-repository commits as needed.
func (s *Store) Snapshot(ctx context.Context, label, commit string) (*Snapshot, error) {
	mainM, err := s.Main(ctx)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Label: label, files: map[string]*entry{}}
	snap.parts = []*part{{path: "", mirror: mainM, commit: commit}}
	for _, spec := range s.subs {
		parent, rel := snap.route(spec.Path)
		var rev string
		if spec.RevisionFile != "" {
			fp := spec.RevisionFile
			if parent.path != "" {
				fp = path.Join(parent.path, fp)
			}
			data, exists, err := snap.read(ctx, fp)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, fmt.Errorf("source %s: revision file %s does not exist in %s", spec.Path, fp, label)
			}
			m := spec.RevisionRegex.FindSubmatch(data)
			if m == nil || len(m) < 2 {
				return nil, fmt.Errorf("source %s: revision_regex matches nothing in %s at %s", spec.Path, fp, label)
			}
			rev = string(m[1])
		} else {
			ents, err := parent.mirror.Repo.LsTreePaths(ctx, parent.commit, []string{rel})
			if err != nil {
				return nil, err
			}
			e, ok := ents[rel]
			if !ok || e.Mode != "160000" {
				return nil, fmt.Errorf("source %s: there is no submodule entry at %s in %s; set its revision file and regex to say where its commit is pinned", spec.Path, spec.Path, label)
			}
			rev = e.OID
		}
		rev = strings.ToLower(strings.TrimSpace(rev))
		m, err := s.mirror(ctx, spec)
		if err != nil {
			return nil, err
		}
		if err := m.FetchCommit(ctx, rev); err != nil {
			return nil, fmt.Errorf("source %s at %s: %w", spec.Path, short(rev), err)
		}
		snap.parts = append(snap.parts, &part{path: spec.Path, mirror: m, commit: rev})
		sort.SliceStable(snap.parts, func(a, b int) bool { return len(snap.parts[a].path) > len(snap.parts[b].path) })
	}
	return snap, nil
}

// route returns the repository containing p and p relative to it.
func (s *Snapshot) route(p string) (*part, string) {
	for _, pt := range s.parts {
		if pt.path == "" {
			return pt, p
		}
		if rel, ok := strings.CutPrefix(p, pt.path+"/"); ok {
			return pt, rel
		}
	}
	return s.parts[len(s.parts)-1], p
}

// Prefetch loads the given files in a few batched requests.
func (s *Snapshot) Prefetch(ctx context.Context, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byPart := map[*part][]string{}
	for _, p := range paths {
		if _, ok := s.files[p]; ok {
			continue
		}
		pt, rel := s.route(p)
		byPart[pt] = append(byPart[pt], rel)
	}
	for pt, rels := range byPart {
		if err := s.lookup(ctx, pt, rels); err != nil {
			return err
		}
		var oids []string
		for _, rel := range rels {
			if e := s.files[join(pt.path, rel)]; e.exists && !e.loaded && e.err == nil {
				oids = append(oids, e.oid)
			}
		}
		if err := pt.mirror.fetchBlobs(ctx, oids); err != nil {
			return err
		}
		blobs, err := pt.mirror.Repo.ReadBlobs(ctx, oids)
		if err != nil {
			return err
		}
		s.fetchN += len(oids)
		for _, rel := range rels {
			if e := s.files[join(pt.path, rel)]; e.exists && !e.loaded && e.err == nil {
				e.data, e.loaded = blobs[e.oid], true
			}
		}
	}
	return nil
}

func join(dir, rel string) string {
	if dir == "" {
		return rel
	}
	return dir + "/" + rel
}

// lookup records the tree entries of rels in part pt, and for paths that
// do not exist whether they are inside a submodule without a source.
func (s *Snapshot) lookup(ctx context.Context, pt *part, rels []string) error {
	ents, err := pt.mirror.Repo.LsTreePaths(ctx, pt.commit, rels)
	if err != nil {
		return err
	}
	var absent []string
	dirs := map[string]bool{}
	for _, rel := range rels {
		full := join(pt.path, rel)
		e, ok := ents[rel]
		switch {
		case !ok:
			s.files[full] = &entry{}
			absent = append(absent, rel)
			for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
				dirs[d] = true
			}
		case e.Type == "tree":
			s.files[full] = &entry{err: fmt.Errorf("%s is a directory in the upstream tree", full)}
		case e.Mode == "160000":
			s.files[full] = &entry{err: &SubmoduleError{Path: full, Submodule: full, Commit: e.OID}}
		default:
			s.files[full] = &entry{exists: true, mode: e.Mode, oid: e.OID}
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	var ds []string
	for d := range dirs {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	dents, err := pt.mirror.Repo.LsTreePaths(ctx, pt.commit, ds)
	if err != nil {
		return err
	}
	for _, rel := range absent {
		for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
			if e, ok := dents[d]; ok && e.Mode == "160000" {
				s.files[join(pt.path, rel)] = &entry{err: &SubmoduleError{Path: join(pt.path, rel), Submodule: join(pt.path, d), Commit: e.OID}}
				break
			}
		}
	}
	return nil
}

// read returns a file of the snapshot (caller holds no lock).
func (s *Snapshot) read(ctx context.Context, p string) ([]byte, bool, error) {
	if err := s.Prefetch(ctx, []string{p}); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.files[p]
	if e.err != nil {
		return nil, false, e.err
	}
	return e.data, e.exists, nil
}

// Entry returns a file's mode and blob id ("" mode if it does not exist).
func (s *Snapshot) Entry(ctx context.Context, p string) (mode, oid string, err error) {
	if err := s.Prefetch(ctx, []string{p}); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.files[p]
	return e.mode, e.oid, e.err
}

// Fetched returns how many blobs the snapshot has loaded.
func (s *Snapshot) Fetched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchN
}

// Bind returns the snapshot as a patch.Source whose reads use ctx.
func (s *Snapshot) Bind(ctx context.Context) *Bound { return &Bound{s: s, ctx: ctx} }

// Bound is a Snapshot bound to a context.
type Bound struct {
	s   *Snapshot
	ctx context.Context
}

// ReadFile implements patch.Source.
func (b *Bound) ReadFile(p string) ([]byte, bool, error) { return b.s.read(b.ctx, p) }

// List returns the files under dir ("" for everything) with their modes,
// sorted, without fetching blobs. Sub-repositories under dir are included.
func (s *Snapshot) List(ctx context.Context, dir string) ([]gitx.TreeEntry, error) {
	dir = strings.Trim(dir, "/")
	var out []gitx.TreeEntry
	for _, pt := range s.parts {
		var rel string
		switch {
		case pt.path == "":
			rel = dir
		case dir == "" || pt.path == dir || strings.HasPrefix(pt.path, dir+"/"):
			rel = "" // the whole sub-repository is under dir
		case strings.HasPrefix(dir, pt.path+"/"):
			rel = strings.TrimPrefix(dir, pt.path+"/")
		default:
			continue
		}
		ents, err := pt.mirror.Repo.LsTreeRecursive(ctx, pt.commit, rel)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.Mode == "160000" {
				continue // a sub-repository: listed from its own part, if configured
			}
			e.Path = join(pt.path, e.Path)
			if owner, _ := s.route(e.Path); owner != pt {
				continue
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, nil
}
