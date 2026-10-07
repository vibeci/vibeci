// Package config defines and validates the VibeCI configuration files:
// the harness config (config.jsonc) and the sandbox broker config
// (sandboxd.jsonc).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that unmarshals from "30m"-style strings or a
// number of seconds.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = v
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\" or a number of seconds")
	}
	d.Duration = time.Duration(n * float64(time.Second))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Or returns d, or def if d is zero.
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration <= 0 {
		return def
	}
	return d.Duration
}

// Secret is a credential loaded at runtime. In JSON it is either a string
// ("env:NAME", "file:/path", or a literal value) or an object
// {"env": "NAME"} / {"file": "/path"} / {"value": "..."}.
type Secret struct {
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Value string `json:"value,omitempty"`
}

func (s *Secret) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		switch {
		case strings.HasPrefix(str, "env:"):
			s.Env = strings.TrimPrefix(str, "env:")
		case strings.HasPrefix(str, "file:"):
			s.File = strings.TrimPrefix(str, "file:")
		default:
			s.Value = str
		}
		return nil
	}
	type plain Secret
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("secret must be a string or {env|file|value}: %w", err)
	}
	*s = Secret(p)
	return nil
}

// MarshalJSON shows env/file references but never inline values.
func (s Secret) MarshalJSON() ([]byte, error) {
	switch {
	case s.Env != "":
		return json.Marshal("env:" + s.Env)
	case s.File != "":
		return json.Marshal("file:" + s.File)
	case s.Value != "":
		return json.Marshal("[redacted]")
	}
	return []byte("null"), nil
}

// IsSet reports whether any source is configured.
func (s Secret) IsSet() bool { return s.Env != "" || s.File != "" || s.Value != "" }

// Describe returns a non-sensitive description of the secret source.
func (s Secret) Describe() string {
	switch {
	case s.Env != "":
		return "env:" + s.Env
	case s.File != "":
		return "file:" + s.File
	case s.Value != "":
		return "inline value"
	}
	return "unset"
}

// Resolve loads the secret. File contents are trimmed of surrounding
// whitespace (trailing newlines are common in secret files).
func (s Secret) Resolve() (string, error) {
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			return "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
		return v, nil
	case s.File != "":
		b, err := os.ReadFile(s.File)
		if err != nil {
			return "", fmt.Errorf("reading secret file: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("secret file %s is empty", s.File)
		}
		return v, nil
	case s.Value != "":
		return s.Value, nil
	}
	return "", errors.New("secret not configured")
}

// ResolveOr resolves the secret if set, otherwise reads the fallback
// environment variable (which may be empty).
func (s Secret) ResolveOr(fallbackEnv string) (string, error) {
	if s.IsSet() {
		return s.Resolve()
	}
	return os.Getenv(fallbackEnv), nil
}

// ParseBytes parses sizes like "512m", "4g", "1073741824".
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	s = strings.TrimSuffix(s, "b")
	if s == "" {
		return 0, errors.New("invalid size")
	}
	switch s[len(s)-1] {
	case 'k':
		mult = 1 << 10
	case 'm':
		mult = 1 << 20
	case 'g':
		mult = 1 << 30
	case 't':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

// Config is the harness configuration (config.jsonc).
type Config struct {
	// DataDir holds mirrors, job workspaces, state, review cache and logs.
	DataDir string `json:"data_dir"`
	// Interval between sync cycles in daemon mode.
	Interval Duration `json:"interval"`
	// RepoTimeout bounds one sync of one repository.
	RepoTimeout Duration `json:"repo_timeout"`
	// LogLevel: debug, info, warn, error.
	LogLevel string `json:"log_level"`
	// KeepJobs is how long finished job workspaces are retained (failed jobs
	// are useful for post-mortems).
	KeepJobs Duration `json:"keep_jobs"`
	// MaxParallel is how many repos are synced concurrently (default 1).
	MaxParallel int `json:"max_parallel"`
	// StateRef keeps each repo's state (state/repos/<repo>.json and its
	// cached review verdicts) in the fork repository under this ref, e.g.
	// "refs/vibeci/state", as well as under data_dir. Every command loads it
	// from there first and pushes it back after changing it, so VibeCI can
	// run on hosts that start empty, such as CI runners.
	StateRef string `json:"state_ref,omitempty"`

	Identity  Identity             `json:"identity"`
	Providers map[string]*Provider `json:"providers"`
	Models    map[string]*Model    `json:"models"`
	Roles     Roles                `json:"roles"`
	Sandbox   SandboxClient        `json:"sandbox"`
	Alerts    []*Alert             `json:"alerts"`
	Repos     []*Repo              `json:"repos"`
}

// Identity is the author/committer used for merge commits.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Provider is an LLM API endpoint: a vendor's API or a compatible gateway or
// server (BaseURL).
type Provider struct {
	// Type: anthropic | openai-responses | openai-chat
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	// Auth: anthropic: "x-api-key" (default) or "bearer"; openai-*: always
	// bearer.
	Auth   string `json:"auth"`
	APIKey Secret `json:"api_key,omitzero"`

	Headers    map[string]string `json:"headers"`
	Timeout    Duration          `json:"timeout"`
	MaxRetries *int              `json:"max_retries"`
	Stream     *bool             `json:"stream"`
	// MaxTokensField (openai-chat): "max_tokens" or "max_completion_tokens".
	MaxTokensField string `json:"max_tokens_field"`
}

// Model is a named model configuration referenced by roles.
type Model struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	MaxTokens     int    `json:"max_tokens"`
	ContextWindow int    `json:"context_window"`
	// Thinking: "" (off) | "adaptive" | "enabled".
	Thinking       string `json:"thinking"`
	ThinkingBudget int    `json:"thinking_budget"`
	// Effort: low | medium | high | xhigh | max (provider-specific support).
	Effort      string   `json:"effort"`
	Temperature *float64 `json:"temperature"`
	PromptCache *bool    `json:"prompt_cache"`
}

// Roles maps pipeline roles to model names.
type Roles struct {
	// Triage reviews batches of commit diffs in one shot.
	Triage string `json:"triage"`
	// Investigate digs into flagged commits with read-only repository tools.
	Investigate string `json:"investigate"`
	// Audit reviews lines written by the merge agent.
	Audit string `json:"audit"`
	// Resolve is an escalation ladder for the merge agent: each model gets
	// a fresh attempt if the previous one fails.
	Resolve []string `json:"resolve"`
}

// SandboxClient selects how the harness obtains sandboxes.
type SandboxClient struct {
	// Mode: "broker" (default; talk to vibeci sandboxd over a unix socket),
	// "docker" (embed the broker policy in-process; needs docker socket
	// access, e.g. on a CI runner), or "unsafe-local" (run commands directly
	// on the host; tests and development only).
	Mode   string `json:"mode"`
	Socket string `json:"socket"`
	// Docker holds the embedded broker policy when Mode is "docker".
	Docker *Sandboxd `json:"docker"`
}

// Alert is a webhook destination.
type Alert struct {
	Name string `json:"name"`
	// Type: generic | ntfy | discord | slack
	Type    string            `json:"type"`
	URL     Secret            `json:"url"`
	Headers map[string]Secret `json:"headers"`
	// Events to deliver (default: everything except "synced").
	Events []string `json:"events"`
}

// Repo is one fork maintained by VibeCI.
type Repo struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled"`
	Fork     Remote `json:"fork"`
	Upstream Remote `json:"upstream"`
	// Description of what the fork changes and why; given to the merge agent
	// so it preserves intent.
	Description string `json:"description"`
	// KeepOurs: glob patterns of paths where the fork's version always wins
	// and upstream changes are discarded (e.g. ".github/workflows/**").
	KeepOurs []string `json:"keep_ours"`

	Sandbox  RepoSandbox `json:"sandbox"`
	Prefetch []Command   `json:"prefetch"`
	Verify   []Command   `json:"verify"`
	Review   Review      `json:"review"`
	Resolve  Resolve     `json:"resolve"`
	Push     Push        `json:"push"`
	Roles    *Roles      `json:"roles"`

	// OnUpstreamRewrite: "hold" (default) stops syncing and alerts when the
	// upstream branch is force-pushed; "continue" merges anyway.
	OnUpstreamRewrite string `json:"on_upstream_rewrite"`

	// Patches makes the fork a patch repository (patch mode): instead of
	// containing upstream's history, the fork pins an upstream version in
	// a file and carries patch files that are applied to upstream's tree
	// (Chromium derivatives such as ungoogled-chromium or Brave). VibeCI
	// moves the pin to new upstream versions and refreshes the patches.
	Patches *Patches `json:"patches"`
}

// IsEnabled reports whether the repo is enabled (default true).
func (r *Repo) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// PatchMode reports whether the fork is a patch repository.
func (r *Repo) PatchMode() bool { return r.Patches != nil }

// Remote is a git remote.
type Remote struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	// Tags (upstream only): track the highest version tag matching this glob
	// (e.g. "v*") instead of a branch.
	Tags string   `json:"tags"`
	Auth *GitAuth `json:"auth"`

	// Patch mode, upstream only. VersionURL is fetched (http or https) to
	// learn the version to follow, instead of using the newest tag;
	// VersionRegex's first group extracts it from the response (default:
	// the whole response, trimmed).
	VersionURL   string `json:"version_url,omitempty"`
	VersionRegex string `json:"version_regex,omitempty"`
	// TagFormat maps versions to upstream tags: "{version}" is replaced by
	// the version (default "{version}").
	TagFormat string `json:"tag_format,omitempty"`
	// Fetch: "partial" (default: commits and trees only; files are fetched
	// when read) or "full" (for servers that serve single files slowly).
	Fetch string `json:"fetch,omitempty"`
}

// Patches configures a patch-mode fork.
type Patches struct {
	// Series (a quilt series file; patch names are relative to its
	// directory) or Glob (patch files, applied in path order) lists the
	// patches, applied with Strip (patch -pN, default 1) to the directory
	// Root of the upstream tree (default: its top). Together they form the
	// first entry of Sets.
	Series string `json:"series,omitempty"`
	Glob   string `json:"glob,omitempty"`
	Strip  *int   `json:"strip,omitempty"`
	Root   string `json:"root,omitempty"`
	// Sets are groups of patches applied in order, each with its own
	// series or glob, strip and root (Brave keeps patches for v8 relative
	// to v8/).
	Sets []*PatchSet `json:"sets"`
	// VersionFile holds the pinned upstream version. VersionRegex's first
	// group locates the version in it (default: the whole file, trimmed).
	VersionFile  string `json:"version_file"`
	VersionRegex string `json:"version_regex,omitempty"`
	// UpdateFiles are written on every version update; "{version}" in the
	// content is replaced by the new version (ungoogled-chromium resets
	// revision.txt to 1).
	UpdateFiles map[string]string `json:"update_files,omitempty"`
	// Sources are repositories whose trees appear at paths inside the
	// upstream tree (Chromium's DEPS dependencies, e.g. v8). Patches that
	// touch files inside one need it.
	Sources []*PatchSource `json:"sources,omitempty"`
	// Fuzz lets a hunk apply with up to this many context lines ignored at
	// each end (patch -F; default 0, max 3). Such patches are rewritten.
	Fuzz int `json:"fuzz"`
	// IgnoreWhitespace matches lines ignoring whitespace (patch -l).
	IgnoreWhitespace bool `json:"ignore_whitespace"`
	// DropUpstreamed removes patches whose changes upstream now contains
	// from the series (default true).
	DropUpstreamed *bool `json:"drop_upstreamed"`
	// KeepOffsets leaves patches that only moved unchanged instead of
	// updating their line numbers.
	KeepOffsets bool `json:"keep_offsets"`
	// VerifyTree selects what the verify commands get in upstream/ and
	// patched/: "touched" (default: the files the patches touch) or "full"
	// (the whole tree; small projects only).
	VerifyTree string `json:"verify_tree"`
}

// Drop reports whether upstreamed patches are removed.
func (p *Patches) Drop() bool { return p.DropUpstreamed == nil || *p.DropUpstreamed }

// PatchSet is a group of patches.
type PatchSet struct {
	Series string `json:"series,omitempty"`
	Glob   string `json:"glob,omitempty"`
	Strip  *int   `json:"strip"`
	Root   string `json:"root,omitempty"`
}

// PatchSource is a repository that appears at Path inside the upstream
// tree. Its commit is the submodule entry at Path in the containing tree
// (Chromium records DEPS dependencies that way), or the first group of
// RevisionRegex in RevisionFile (relative to the containing repository).
type PatchSource struct {
	Path          string   `json:"path"`
	URL           string   `json:"url"`
	Auth          *GitAuth `json:"auth"`
	Fetch         string   `json:"fetch,omitempty"`
	RevisionFile  string   `json:"revision_file,omitempty"`
	RevisionRegex string   `json:"revision_regex,omitempty"`
}

// GitAuth holds credentials for a remote. The harness is the only component
// that ever sees them; they never enter a sandbox or an LLM context.
type GitAuth struct {
	// HTTPS token auth. Username defaults to "x-access-token", which works
	// for GitHub and Gitea/Forgejo.
	Username string `json:"username"`
	Token    Secret `json:"token,omitzero"`
	// SSH private key (file or value). KnownHosts defaults to
	// <data_dir>/known_hosts with trust-on-first-use.
	SSHKey     Secret `json:"ssh_key,omitzero"`
	KnownHosts string `json:"known_hosts"`
}

// RepoSandbox selects broker profiles for a repo.
type RepoSandbox struct {
	// Profile for the merge agent's shell (default "default").
	Profile string `json:"profile"`
	// VerifyProfile for clean-room verification (default: Profile).
	VerifyProfile string `json:"verify_profile"`
	// PrefetchProfile for dependency fetching; usually a profile with network
	// egress (default: Profile).
	PrefetchProfile string `json:"prefetch_profile"`
}

// Command is a shell command run inside a sandbox.
type Command struct {
	Name    string   `json:"name"`
	Run     string   `json:"run"`
	Timeout Duration `json:"timeout"`
}

// Review configures the malicious-commit gate.
type Review struct {
	Enabled *bool `json:"enabled"`
	// BlockOn: "malicious" (default) or "suspicious".
	BlockOn string `json:"block_on"`
	// MinConfidence required to block (default 0.7).
	MinConfidence float64 `json:"min_confidence"`
	// MaxCommitsPerRun bounds review cost when far behind (default 250);
	// the fork catches up over several cycles.
	MaxCommitsPerRun int `json:"max_commits_per_run"`
	// BatchChars bounds the diff text sent in one triage call (default 150000).
	BatchChars int `json:"batch_chars"`
	// OnBlocked: "exclude" (default) merges past blocked commits with their
	// changes reverted and kept out of later merges; "hold" stops merging at
	// the first blocked commit until it is allowed.
	OnBlocked string `json:"on_blocked"`
	// AllowCommits are commit SHAs that were reviewed and accepted.
	AllowCommits []string `json:"allow_commits"`
	// InvestigateTurns bounds the investigation agent (default 30).
	InvestigateTurns int `json:"investigate_turns"`
}

// IsEnabled reports whether review is enabled (default true).
func (r Review) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Resolve configures the merge agent.
type Resolve struct {
	// MaxRounds is the number of submit attempts per model (default 4).
	MaxRounds int `json:"max_rounds"`
	// MaxTurns bounds LLM calls per model attempt (default 150).
	MaxTurns int `json:"max_turns"`
	// Timeout per model attempt (default 90m).
	Timeout Duration `json:"timeout"`
	// MaxNovelLines bounds lines the agent may write that exist in neither
	// side of the merge (default 3000).
	MaxNovelLines int `json:"max_novel_lines"`
	// CommandTimeout caps a single agent shell command (default 20m).
	CommandTimeout Duration `json:"command_timeout"`
}

// Push configures how results are published.
type Push struct {
	// Mode: "direct" (default) pushes to the fork branch; "branch" pushes to
	// Branch for a human to merge.
	Mode   string `json:"mode"`
	Branch string `json:"branch"`
	// DryRun computes everything but never pushes.
	DryRun bool `json:"dry_run"`
}

// Sandboxd is the sandbox broker configuration (sandboxd.jsonc). It is the
// security policy for everything that runs untrusted code or an LLM shell,
// so it lives in a separate file owned by the broker, not the harness.
type Sandboxd struct {
	// Listen is the unix socket path (default /run/vibeci/sandboxd.sock).
	Listen string `json:"listen"`
	// DockerHost (default unix:///var/run/docker.sock).
	DockerHost string `json:"docker_host"`
	// DataRoot is the absolute path where the harness sees its data dir.
	// Workspaces requested by the harness are relative to it.
	DataRoot string `json:"data_root"`
	// DataVolume is the docker named volume mounted at DataRoot in the
	// harness container (compose deployments)...
	DataVolume string `json:"data_volume"`
	// ...or DataHostPath is the host directory that is DataRoot (bind mounts).
	DataHostPath string `json:"data_host_path"`

	MaxSandboxes   int      `json:"max_sandboxes"`
	MaxLifetime    Duration `json:"max_lifetime"`
	MaxExecTimeout Duration `json:"max_exec_timeout"`

	Profiles map[string]*Profile `json:"profiles"`
}

// Profile is a sandbox container template.
type Profile struct {
	Image string `json:"image"`
	// Pull: "never" (default) or "missing".
	Pull string `json:"pull"`
	// Network: "none" (default) or the name of an existing docker network.
	Network string            `json:"network"`
	Memory  string            `json:"memory"`
	CPUs    float64           `json:"cpus"`
	Pids    int64             `json:"pids"`
	TmpSize string            `json:"tmp_size"`
	User    string            `json:"user"`
	Env     map[string]string `json:"env"`
}
