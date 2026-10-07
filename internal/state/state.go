// Package state persists per-repository progress, failure backoff, blocked
// commits and the review cache as JSON files under the data directory.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// RepoState is the persistent state of one fork.
type RepoState struct {
	Repo string `json:"repo"`
	// LastUpstreamTip and LastForkHead are what the last cycle observed.
	LastUpstreamTip string `json:"last_upstream_tip,omitempty"`
	LastForkHead    string `json:"last_fork_head,omitempty"`
	// LastMerged is the upstream commit most recently merged and pushed. If
	// upstream history stops containing it, upstream rewrote history the
	// fork already depends on.
	LastMerged string `json:"last_merged,omitempty"`
	// LastMergedTag is the upstream tag name of LastMerged (tag mode).
	LastMergedTag string `json:"last_merged_tag,omitempty"`
	LastPushed    string `json:"last_pushed,omitempty"`
	// PinnedVersion and UpstreamVersion (patch mode) are the upstream
	// version the fork pins and the newest upstream version, as last seen.
	PinnedVersion   string    `json:"pinned_version,omitempty"`
	UpstreamVersion string    `json:"upstream_version,omitempty"`
	LastResult      string    `json:"last_result,omitempty"`
	LastMessage     string    `json:"last_message,omitempty"`
	LastJob         string    `json:"last_job,omitempty"`
	LastAttempt     time.Time `json:"last_attempt,omitzero"`
	LastSuccess     time.Time `json:"last_success,omitzero"`
	LastGC          time.Time `json:"last_gc,omitzero"`

	Failure     *Failure             `json:"failure,omitempty"`
	Blocked     map[string]*Blocked  `json:"blocked,omitempty"`
	Warned      map[string]time.Time `json:"warned,omitempty"`
	RewriteHold *RewriteHold         `json:"rewrite_hold,omitempty"`
	// Proposal is the last dry-run result, so an unchanged dry run is not
	// recomputed every cycle.
	Proposal *Proposal `json:"proposal,omitempty"`
	// Excluded upstream commits are kept out of the fork: their changes are
	// reverted and may not reappear in later merges.
	Excluded map[string]*Excluded `json:"excluded,omitempty"`
	// Allowed commits were cleared with `vibeci allow` (in addition to the
	// config's review.allow_commits).
	Allowed map[string]*Allowed `json:"allowed,omitempty"`
	Usage   Usage               `json:"usage"`
}

// Excluded is an upstream commit whose changes are kept out of the fork.
type Excluded struct {
	Commit     string    `json:"commit"`
	Source     string    `json:"source"` // "review" or "manual"
	Verdict    string    `json:"verdict,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Since      time.Time `json:"since"`
	// Removed is set once the fork no longer contains the commit's changes.
	Removed bool `json:"removed"`
	// Restore asks the next sync to re-apply the commit (it was allowed
	// after it had been excluded).
	Restore bool `json:"restore,omitempty"`
}

// Allowed is a commit cleared with `vibeci allow`.
type Allowed struct {
	Commit string    `json:"commit"`
	Reason string    `json:"reason,omitempty"`
	Time   time.Time `json:"time"`
}

// Proposal is a merge that was computed and verified but not pushed.
type Proposal struct {
	Base     string    `json:"base"`
	Upstream string    `json:"upstream"`
	Target   string    `json:"target"`
	Commit   string    `json:"commit"`
	JobID    string    `json:"job_id"`
	Time     time.Time `json:"time"`
}

// Usage accumulates LLM token usage for a repo.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	Calls            int64 `json:"calls"`
}

// Failure tracks repeated failures for one (fork head, target) pair so the
// daemon backs off instead of burning tokens on the same impossible merge.
type Failure struct {
	// Kind: "failed" (the merge itself could not be completed) or "error"
	// (infrastructure: network, LLM API, sandbox).
	Kind     string `json:"kind"`
	ForkHead string `json:"fork_head"`
	Target   string `json:"target"`
	// Count is the number of consecutive failures for this (fork head,
	// upstream tip) pair; Streak counts consecutive failures of any pair.
	Count     int       `json:"count"`
	Streak    int       `json:"streak"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
	NextRetry time.Time `json:"next_retry"`
	Error     string    `json:"error"`
	Alerted   bool      `json:"alerted"`
}

// Blocked is an upstream commit the review gate refused to merge.
type Blocked struct {
	Commit     string    `json:"commit"`
	Verdict    string    `json:"verdict"`
	Confidence float64   `json:"confidence"`
	Summary    string    `json:"summary"`
	FirstSeen  time.Time `json:"first_seen"`
	Alerted    bool      `json:"alerted"`
}

// RewriteHold records a detected upstream force-push.
type RewriteHold struct {
	OldTip  string    `json:"old_tip"`
	NewTip  string    `json:"new_tip"`
	Since   time.Time `json:"since"`
	Alerted bool      `json:"alerted"`
}

// Store reads and writes state files.
type Store struct {
	Dir string
}

var relRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// Open creates the state directory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// Path returns the file path of rel, a slash-separated path under the
// state directory.
func (s *Store) Path(rel string) (string, error) {
	if !relRe.MatchString(rel) {
		return "", fmt.Errorf("invalid state path %q", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "." || part == ".." || strings.HasPrefix(part, ".tmp-") {
			return "", fmt.Errorf("invalid state path %q", rel)
		}
	}
	return filepath.Join(s.Dir, filepath.FromSlash(rel)), nil
}

// ReadFile returns the contents of rel (nil, nil if it does not exist).
func (s *Store) ReadFile(rel string) ([]byte, error) {
	p, err := s.Path(rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// ReadJSON loads rel into v; found is false if the file does not exist.
func (s *Store) ReadJSON(rel string, v any) (found bool, err error) {
	b, err := s.ReadFile(rel)
	if err != nil || b == nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		p, _ := s.Path(rel)
		return false, fmt.Errorf("%s: %w", p, err)
	}
	return true, nil
}

// WriteJSON atomically writes v to rel.
func (s *Store) WriteJSON(rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.WriteFile(rel, append(b, '\n'))
}

// WriteFile atomically writes data to rel.
func (s *Store) WriteFile(rel string, data []byte) error {
	p, err := s.Path(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), p)
}

// Remove deletes rel if it exists.
func (s *Store) Remove(rel string) error {
	p, err := s.Path(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns the names of the files in directory rel (none if it does
// not exist), skipping temporary files.
func (s *Store) List(rel string) ([]string, error) {
	p, err := s.Path(rel)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".tmp-") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Load returns the state of repo (empty state if none).
func (s *Store) Load(repo string) (*RepoState, error) {
	st := &RepoState{Repo: repo}
	if _, err := s.ReadJSON("repos/"+repo+".json", st); err != nil {
		return nil, err
	}
	if st.Blocked == nil {
		st.Blocked = map[string]*Blocked{}
	}
	if st.Warned == nil {
		st.Warned = map[string]time.Time{}
	}
	if st.Excluded == nil {
		st.Excluded = map[string]*Excluded{}
	}
	if st.Allowed == nil {
		st.Allowed = map[string]*Allowed{}
	}
	return st, nil
}

// Save persists the state of a repo.
func (s *Store) Save(st *RepoState) error {
	return s.WriteJSON("repos/"+st.Repo+".json", st)
}

// Lock takes an exclusive, non-blocking lock for repo so a cron "run" and
// the daemon never process the same fork concurrently.
func (s *Store) Lock(repo string) (unlock func(), err error) {
	p, err := s.Path("locks/" + repo + ".lock")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("repo %s is locked by another vibeci process", repo)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
