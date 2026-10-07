package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/jsonc"
)

var (
	// NameRe constrains repo, model, provider and profile names. Repo names
	// become path components and docker labels, so keep them boring.
	NameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	networkRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	branchRe  = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,200}$`)
	// stateRefRe: a full ref name of the characters the harness's git
	// wrapper accepts.
	stateRefRe = regexp.MustCompile(`^refs/[A-Za-z0-9._/+-]{1,200}$`)
)

// Event kinds that can be routed to alerts.
var EventKinds = []string{"blocked", "suspicious", "failed", "rewritten", "error", "synced", "recovered"}

// Load reads, merges, defaults and validates the harness config: one file,
// or several merged in order (see Merge).
func Load(paths ...string) (*Config, error) {
	raw, err := ReadMerged(paths...)
	if err != nil {
		return nil, err
	}
	return Parse(raw, strings.Join(paths, " + "))
}

// Parse decodes a merged config document (ReadMerged), applies defaults and
// validates it; name prefixes errors.
func Parse(raw []byte, name string) (*Config, error) {
	var c Config
	if err := jsonc.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &c, nil
}

// ReadMerged reads config files and merges them in order into one JSON
// document (see Merge). Each file is checked on its own first, so syntax
// errors and unknown keys are reported with that file's line and column.
// A single file is returned as it is.
func ReadMerged(paths ...string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, errors.New("no config file given")
	}
	var merged any
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		v, err := Decode(p, b)
		if err != nil {
			return nil, err
		}
		if len(paths) == 1 {
			return b, nil
		}
		merged = Merge(merged, v)
	}
	return json.Marshal(merged)
}

// Decode checks one config document (JSONC; name prefixes errors) and
// returns it as generic JSON for Merge. Syntax errors and unknown keys are
// reported here, with the document's own line and column.
func Decode(name string, b []byte) (map[string]any, error) {
	var probe Config
	if err := jsonc.Unmarshal(b, &probe); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	v, err := decodeGeneric(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not a JSON object", name)
	}
	return m, nil
}

// decodeGeneric decodes JSONC into maps, slices and json.Numbers.
func decodeGeneric(b []byte) (any, error) {
	std, err := jsonc.Standardize(b)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(std))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Merge returns over merged onto base: objects are merged key by key,
// recursively; any other value of over (arrays included) replaces base's.
// Neither argument is modified.
func Merge(base, over any) any {
	bm, ok1 := base.(map[string]any)
	om, ok2 := over.(map[string]any)
	if !ok1 || !ok2 {
		return over
	}
	out := make(map[string]any, len(bm)+len(om))
	for k, v := range bm {
		out[k] = v
	}
	for k, v := range om {
		out[k] = Merge(bm[k], v)
	}
	return out
}

// LoadSandboxd reads, defaults and validates the broker config.
func LoadSandboxd(path string) (*Sandboxd, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Sandboxd
	if err := jsonc.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.ApplyDefaults()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// ApplyDefaults fills unset fields.
func (c *Config) ApplyDefaults() {
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	c.DataDir = filepath.Clean(c.DataDir)
	if c.Interval.Duration == 0 {
		c.Interval.Duration = 30 * time.Minute
	}
	if c.RepoTimeout.Duration == 0 {
		c.RepoTimeout.Duration = 4 * time.Hour
	}
	if c.KeepJobs.Duration == 0 {
		c.KeepJobs.Duration = 7 * 24 * time.Hour
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.MaxParallel == 0 {
		c.MaxParallel = 1
	}
	if c.Identity.Name == "" {
		c.Identity.Name = "VibeCI"
	}
	if c.Identity.Email == "" {
		c.Identity.Email = "vibeci@localhost"
	}
	for _, p := range c.Providers {
		if p == nil {
			continue
		}
		if p.Timeout.Duration == 0 {
			p.Timeout.Duration = 20 * time.Minute
		}
		if p.MaxRetries == nil {
			n := 6
			p.MaxRetries = &n
		}
		switch p.Type {
		case "anthropic":
			if p.BaseURL == "" {
				p.BaseURL = "https://api.anthropic.com/v1"
			}
			if p.Auth == "" {
				p.Auth = "x-api-key"
			}
		case "openai-responses", "openai-chat":
			if p.BaseURL == "" {
				p.BaseURL = "https://api.openai.com/v1"
			}
			if p.MaxTokensField == "" {
				if strings.Contains(p.BaseURL, "api.openai.com") {
					p.MaxTokensField = "max_completion_tokens"
				} else {
					p.MaxTokensField = "max_tokens"
				}
			}
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	for _, m := range c.Models {
		if m == nil {
			continue
		}
		if m.MaxTokens == 0 {
			m.MaxTokens = 16000
		}
		if m.ContextWindow == 0 {
			m.ContextWindow = 200000
		}
	}
	if c.Sandbox.Mode == "" {
		c.Sandbox.Mode = "broker"
	}
	if c.Sandbox.Socket == "" {
		c.Sandbox.Socket = "/run/vibeci/sandboxd.sock"
	}
	if c.Sandbox.Docker != nil {
		if c.Sandbox.Docker.DataRoot == "" {
			c.Sandbox.Docker.DataRoot = c.DataDir
		}
		if c.Sandbox.Docker.DataVolume == "" && c.Sandbox.Docker.DataHostPath == "" {
			// Harness runs directly on the docker host: data dir is a host path.
			c.Sandbox.Docker.DataHostPath = c.DataDir
		}
		// Embedded mode bind-mounts directories owned by the harness user, so
		// sandboxes default to the same (non-root) uid:gid.
		if uid, gid := os.Getuid(), os.Getgid(); uid > 0 {
			for _, p := range c.Sandbox.Docker.Profiles {
				if p != nil && p.User == "" {
					p.User = fmt.Sprintf("%d:%d", uid, gid)
				}
			}
		}
		c.Sandbox.Docker.ApplyDefaults()
	}
	for _, a := range c.Alerts {
		if a != nil && len(a.Events) == 0 {
			a.Events = []string{"blocked", "suspicious", "failed", "rewritten", "error", "recovered"}
		}
	}
	for _, r := range c.Repos {
		if r == nil {
			continue
		}
		if r.Sandbox.Profile == "" {
			r.Sandbox.Profile = "default"
		}
		if r.Sandbox.VerifyProfile == "" {
			r.Sandbox.VerifyProfile = r.Sandbox.Profile
		}
		if r.Sandbox.PrefetchProfile == "" {
			r.Sandbox.PrefetchProfile = r.Sandbox.Profile
		}
		if r.Review.BlockOn == "" {
			r.Review.BlockOn = "malicious"
		}
		if r.Review.OnBlocked == "" {
			r.Review.OnBlocked = "exclude"
		}
		if r.Review.MinConfidence == 0 {
			r.Review.MinConfidence = 0.7
		}
		if r.Review.MaxCommitsPerRun == 0 {
			r.Review.MaxCommitsPerRun = 250
		}
		if r.Review.BatchChars == 0 {
			r.Review.BatchChars = 150000
		}
		if r.Review.InvestigateTurns == 0 {
			r.Review.InvestigateTurns = 30
		}
		for i, s := range r.Review.AllowCommits {
			r.Review.AllowCommits[i] = strings.ToLower(strings.TrimSpace(s))
		}
		if r.Resolve.MaxRounds == 0 {
			r.Resolve.MaxRounds = 4
		}
		if r.Resolve.MaxTurns == 0 {
			r.Resolve.MaxTurns = 150
		}
		if r.Resolve.Timeout.Duration == 0 {
			r.Resolve.Timeout.Duration = 90 * time.Minute
		}
		if r.Resolve.MaxNovelLines == 0 {
			r.Resolve.MaxNovelLines = 3000
		}
		if r.Resolve.CommandTimeout.Duration == 0 {
			r.Resolve.CommandTimeout.Duration = 20 * time.Minute
		}
		if r.Push.Mode == "" {
			r.Push.Mode = "direct"
		}
		if r.Push.Mode == "branch" && r.Push.Branch == "" {
			r.Push.Branch = "vibeci/sync"
		}
		if r.OnUpstreamRewrite == "" {
			r.OnUpstreamRewrite = "hold"
		}
		for i := range r.Verify {
			if r.Verify[i].Timeout.Duration == 0 {
				r.Verify[i].Timeout.Duration = 30 * time.Minute
			}
			if r.Verify[i].Name == "" {
				r.Verify[i].Name = fmt.Sprintf("verify-%d", i+1)
			}
		}
		for i := range r.Prefetch {
			if r.Prefetch[i].Timeout.Duration == 0 {
				r.Prefetch[i].Timeout.Duration = 20 * time.Minute
			}
			if r.Prefetch[i].Name == "" {
				r.Prefetch[i].Name = fmt.Sprintf("prefetch-%d", i+1)
			}
		}
		if r.Fork.Auth != nil && r.Fork.Auth.Username == "" {
			r.Fork.Auth.Username = "x-access-token"
		}
		if r.Upstream.Auth != nil && r.Upstream.Auth.Username == "" {
			r.Upstream.Auth.Username = "x-access-token"
		}
		if r.Patches != nil {
			r.Patches.applyDefaults()
			if r.Upstream.TagFormat == "" {
				r.Upstream.TagFormat = "{version}"
			}
			if r.Upstream.Fetch == "" {
				r.Upstream.Fetch = "partial"
			}
			if r.Review.Enabled == nil {
				off := false // a patch-mode fork merges no upstream commits
				r.Review.Enabled = &off
			}
		}
	}
}

func (p *Patches) applyDefaults() {
	if p.Series != "" || p.Glob != "" {
		p.Sets = append([]*PatchSet{{Series: p.Series, Glob: p.Glob, Strip: p.Strip, Root: p.Root}}, p.Sets...)
		p.Series, p.Glob, p.Strip, p.Root = "", "", nil, ""
	}
	for _, s := range p.Sets {
		if s == nil {
			continue
		}
		if s.Strip == nil {
			one := 1
			s.Strip = &one
		}
		s.Root = strings.Trim(s.Root, "/")
	}
	if p.DropUpstreamed == nil {
		on := true
		p.DropUpstreamed = &on
	}
	if p.VerifyTree == "" {
		p.VerifyTree = "touched"
	}
	for _, s := range p.Sources {
		if s == nil {
			continue
		}
		s.Path = strings.Trim(s.Path, "/")
		if s.Fetch == "" {
			s.Fetch = "partial"
		}
		if s.Auth != nil && s.Auth.Username == "" {
			s.Auth.Username = "x-access-token"
		}
	}
}

// RolesFor returns the effective roles for a repo.
func (c *Config) RolesFor(r *Repo) Roles {
	out := c.Roles
	if r.Roles != nil {
		if r.Roles.Triage != "" {
			out.Triage = r.Roles.Triage
		}
		if r.Roles.Investigate != "" {
			out.Investigate = r.Roles.Investigate
		}
		if r.Roles.Audit != "" {
			out.Audit = r.Roles.Audit
		}
		if len(r.Roles.Resolve) > 0 {
			out.Resolve = r.Roles.Resolve
		}
	}
	return out
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !filepath.IsAbs(c.DataDir) {
		add("data_dir must be an absolute path")
	}
	if c.Interval.Duration < time.Minute {
		add("interval must be at least 1m")
	}
	if c.MaxParallel < 1 || c.MaxParallel > 32 {
		add("max_parallel must be within 1..32")
	}
	if c.StateRef != "" {
		if !stateRefRe.MatchString(c.StateRef) || strings.Contains(c.StateRef, "..") || strings.HasSuffix(c.StateRef, ".lock") ||
			strings.HasSuffix(c.StateRef, "/") || strings.Contains(c.StateRef, "//") || strings.HasPrefix(c.StateRef, "refs/tags/") {
			add("state_ref must be a ref name such as refs/vibeci/state (not a tag)")
		}
		for _, r := range c.Repos {
			if r != nil && (c.StateRef == "refs/heads/"+r.Fork.Branch || (r.Push.Mode == "branch" && c.StateRef == "refs/heads/"+r.Push.Branch)) {
				add("state_ref %s is a branch repo %q merges into", c.StateRef, r.Name)
			}
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("log_level must be debug, info, warn or error")
	}

	if len(c.Providers) == 0 {
		add("at least one provider is required")
	}
	for name, p := range c.Providers {
		if p == nil {
			add("provider %q is null", name)
			continue
		}
		if !NameRe.MatchString(name) {
			add("provider name %q is invalid", name)
		}
		switch p.Type {
		case "anthropic":
			if p.Auth != "" && p.Auth != "x-api-key" && p.Auth != "bearer" {
				add("provider %q: auth must be x-api-key or bearer", name)
			}
			if !p.APIKey.IsSet() {
				add("provider %q: api_key is required", name)
			}
		case "openai-responses", "openai-chat":
			// api_key optional: local servers (llama.cpp, vLLM) often need none.
			if p.MaxTokensField != "" && p.MaxTokensField != "max_tokens" && p.MaxTokensField != "max_completion_tokens" {
				add("provider %q: max_tokens_field must be max_tokens or max_completion_tokens", name)
			}
		default:
			add("provider %q: type must be anthropic, openai-responses or openai-chat", name)
		}
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				add("provider %q: base_url must be an http(s) URL", name)
			}
		}
	}

	for name, m := range c.Models {
		if m == nil {
			add("model %q is null", name)
			continue
		}
		if !NameRe.MatchString(name) {
			add("model name %q is invalid", name)
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			add("model %q: unknown provider %q", name, m.Provider)
		}
		if m.Model == "" {
			add("model %q: model id is required", name)
		}
		switch m.Thinking {
		case "", "off", "adaptive", "enabled":
		default:
			add("model %q: thinking must be off, adaptive or enabled", name)
		}
		switch m.Effort {
		case "", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			add("model %q: effort must be minimal, low, medium, high, xhigh or max", name)
		}
		if m.Thinking == "enabled" && m.ThinkingBudget >= m.MaxTokens {
			add("model %q: thinking_budget must be below max_tokens", name)
		}
	}

	// triage and investigate are only needed by repos with the review gate
	// (patch-mode forks have none).
	checkRoles := func(where string, r Roles, review bool) {
		for _, x := range []struct {
			role, name string
			need       bool
		}{{"triage", r.Triage, review}, {"investigate", r.Investigate, review}, {"audit", r.Audit, true}} {
			if x.name == "" {
				if x.need {
					add("%s: roles.%s is required", where, x.role)
				}
			} else if _, ok := c.Models[x.name]; !ok {
				add("%s: roles.%s references unknown model %q", where, x.role, x.name)
			}
		}
		if len(r.Resolve) == 0 {
			add("%s: roles.resolve needs at least one model", where)
		}
		for _, name := range r.Resolve {
			if _, ok := c.Models[name]; !ok {
				add("%s: roles.resolve references unknown model %q", where, name)
			}
		}
	}
	review := len(c.Repos) == 0
	for _, r := range c.Repos {
		if r != nil && r.Roles == nil && r.Review.IsEnabled() {
			review = true
		}
	}
	checkRoles("config", c.Roles, review)

	switch c.Sandbox.Mode {
	case "broker":
		if !filepath.IsAbs(c.Sandbox.Socket) {
			add("sandbox.socket must be an absolute path")
		}
	case "docker":
		if c.Sandbox.Docker == nil {
			add("sandbox.docker is required when sandbox.mode is docker")
		} else if err := c.Sandbox.Docker.Validate(); err != nil {
			add("sandbox.docker: %v", err)
		}
	case "unsafe-local":
	default:
		add("sandbox.mode must be broker, docker or unsafe-local")
	}

	for i, a := range c.Alerts {
		if a == nil {
			add("alerts[%d] is null", i)
			continue
		}
		switch a.Type {
		case "generic", "ntfy", "discord", "slack":
		default:
			add("alerts[%d]: type must be generic, ntfy, discord or slack", i)
		}
		if !a.URL.IsSet() {
			add("alerts[%d]: url is required", i)
		}
		for _, e := range a.Events {
			if e != "*" && !slices.Contains(EventKinds, e) {
				add("alerts[%d]: unknown event %q (valid: %s, *)", i, e, strings.Join(EventKinds, ", "))
			}
		}
	}

	if len(c.Repos) == 0 {
		add("at least one repo is required")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if r == nil {
			add("repos[%d] is null", i)
			continue
		}
		where := fmt.Sprintf("repo %q", r.Name)
		if !NameRe.MatchString(r.Name) {
			add("repos[%d]: name %q must match %s", i, r.Name, NameRe)
		}
		if seen[r.Name] {
			add("%s: duplicate name", where)
		}
		seen[r.Name] = true
		if err := validateRemoteURL(r.Fork.URL); err != nil {
			add("%s: fork.url: %v", where, err)
		}
		if err := validateRemoteURL(r.Upstream.URL); err != nil {
			add("%s: upstream.url: %v", where, err)
		}
		if !branchRe.MatchString(r.Fork.Branch) || strings.HasPrefix(r.Fork.Branch, "-") {
			add("%s: fork.branch is required and must be a plain branch name", where)
		}
		if r.Fork.Tags != "" {
			add("%s: fork.tags is not supported", where)
		}
		if f := r.Fork; f.VersionURL != "" || f.VersionRegex != "" || f.TagFormat != "" || f.Fetch != "" {
			add("%s: version_url, version_regex, tag_format and fetch belong to upstream, not fork", where)
		}
		auths := []*GitAuth{r.Fork.Auth, r.Upstream.Auth}
		if r.PatchMode() {
			c.validatePatches(where, r, add)
			for _, s := range r.Patches.Sources {
				if s != nil {
					auths = append(auths, s.Auth)
				}
			}
		} else {
			switch {
			case r.Upstream.Branch != "" && r.Upstream.Tags != "":
				add("%s: set either upstream.branch or upstream.tags, not both", where)
			case r.Upstream.Branch == "" && r.Upstream.Tags == "":
				add("%s: upstream.branch or upstream.tags is required", where)
			case r.Upstream.Branch != "" && (!branchRe.MatchString(r.Upstream.Branch) || strings.HasPrefix(r.Upstream.Branch, "-")):
				add("%s: upstream.branch must be a plain branch name", where)
			case r.Upstream.Tags != "" && (strings.ContainsAny(r.Upstream.Tags, " \t\n") || strings.HasPrefix(r.Upstream.Tags, "-")):
				add("%s: upstream.tags must be a glob like v*", where)
			}
			if u := r.Upstream; u.VersionURL != "" || u.VersionRegex != "" || u.TagFormat != "" || u.Fetch != "" {
				add("%s: upstream.version_url, version_regex, tag_format and fetch are only used by patch-mode forks (repos[].patches)", where)
			}
		}
		for _, au := range auths {
			if au == nil {
				continue
			}
			if au.Token.IsSet() && au.SSHKey.IsSet() {
				add("%s: auth must use either token or ssh_key", where)
			}
			if !au.Token.IsSet() && !au.SSHKey.IsSet() {
				add("%s: auth needs token or ssh_key", where)
			}
		}
		if r.Fork.URL == r.Upstream.URL && r.Fork.URL != "" {
			add("%s: fork.url and upstream.url are identical", where)
		}
		for _, p := range r.KeepOurs {
			if err := ValidateGlob(p); err != nil {
				add("%s: keep_ours %q: %v", where, p, err)
			}
		}
		for _, p := range []string{r.Sandbox.Profile, r.Sandbox.VerifyProfile, r.Sandbox.PrefetchProfile} {
			if !NameRe.MatchString(p) {
				add("%s: sandbox profile name %q is invalid", where, p)
			}
			if c.Sandbox.Mode == "docker" && c.Sandbox.Docker != nil {
				if _, ok := c.Sandbox.Docker.Profiles[p]; !ok {
					add("%s: sandbox profile %q not defined in sandbox.docker.profiles", where, p)
				}
			}
		}
		for _, list := range [][]Command{r.Prefetch, r.Verify} {
			for _, cmd := range list {
				if strings.TrimSpace(cmd.Run) == "" {
					add("%s: command %q has empty run", where, cmd.Name)
				}
			}
		}
		switch r.Review.BlockOn {
		case "malicious", "suspicious":
		default:
			add("%s: review.block_on must be malicious or suspicious", where)
		}
		switch r.Review.OnBlocked {
		case "exclude", "hold":
		default:
			add("%s: review.on_blocked must be exclude or hold", where)
		}
		if r.Review.MinConfidence < 0 || r.Review.MinConfidence > 1 {
			add("%s: review.min_confidence must be within 0..1", where)
		}
		for _, s := range r.Review.AllowCommits {
			if !shaRe.MatchString(s) {
				add("%s: review.allow_commits entry %q is not a commit sha", where, s)
			}
		}
		switch r.Push.Mode {
		case "direct":
		case "branch":
			if !branchRe.MatchString(r.Push.Branch) || r.Push.Branch == r.Fork.Branch {
				add("%s: push.branch must be a branch name different from fork.branch", where)
			}
		default:
			add("%s: push.mode must be direct or branch", where)
		}
		switch r.OnUpstreamRewrite {
		case "hold", "continue":
		default:
			add("%s: on_upstream_rewrite must be hold or continue", where)
		}
		if r.Roles != nil {
			checkRoles(where, c.RolesFor(r), r.Review.IsEnabled())
		}
	}
	return errors.Join(errs...)
}

// validatePatches checks the settings of a patch-mode repo.
func (c *Config) validatePatches(where string, r *Repo, add func(string, ...any)) {
	p, up := r.Patches, r.Upstream
	switch {
	case up.Branch != "":
		add("%s: patch mode follows upstream versions, not a branch: replace upstream.branch with upstream.tags (a glob of release tags) or upstream.version_url", where)
	case up.Tags != "" && up.VersionURL != "":
		add("%s: set either upstream.tags or upstream.version_url, not both", where)
	case up.Tags == "" && up.VersionURL == "":
		add("%s: upstream.tags (a glob of release tags) or upstream.version_url is required in patch mode", where)
	case up.Tags != "" && (strings.ContainsAny(up.Tags, " \t\n") || strings.HasPrefix(up.Tags, "-")):
		add("%s: upstream.tags must be a glob like v*", where)
	}
	if up.VersionURL != "" {
		if u, err := url.Parse(up.VersionURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("%s: upstream.version_url must be an http(s) URL", where)
		}
	}
	checkRegex := func(what, re string) {
		if re == "" {
			return
		}
		if x, err := regexp.Compile(re); err != nil {
			add("%s: %s: %v", where, what, err)
		} else if x.NumSubexp() < 1 {
			add("%s: %s needs a group that captures the value, e.g. \"version\": \"([0-9.]+)\"", where, what)
		}
	}
	checkRegex("upstream.version_regex", up.VersionRegex)
	if strings.Count(up.TagFormat, "{version}") != 1 {
		add("%s: upstream.tag_format must contain {version} once", where)
	} else if pre, suf, _ := strings.Cut(up.TagFormat, "{version}"); strings.ContainsAny(pre+suf, " \t\n*?[\\:~^") {
		add("%s: upstream.tag_format contains characters that cannot appear in tag names", where)
	}
	checkFetch := func(what, f string) {
		if f != "partial" && f != "full" {
			add("%s: %s must be partial or full", where, what)
		}
	}
	checkFetch("upstream.fetch", up.Fetch)
	checkPath := func(what, p string) {
		if err := cleanRelPath(p); err != nil {
			add("%s: %s %q: %v", where, what, p, err)
		}
	}
	if p.Strip != nil || p.Root != "" {
		add("%s: patches.strip and patches.root go with patches.series or patches.glob", where)
	}
	if len(p.Sets) == 0 {
		add("%s: patches needs series or glob (or sets)", where)
	}
	for i, s := range p.Sets {
		w := fmt.Sprintf("patches.sets[%d]", i)
		if s == nil {
			add("%s: %s is null", where, w)
			continue
		}
		switch {
		case (s.Series == "") == (s.Glob == ""):
			add("%s: %s: set exactly one of series or glob", where, w)
		case s.Series != "":
			checkPath(w+".series", s.Series)
		default:
			if err := ValidateGlob(s.Glob); err != nil {
				add("%s: %s.glob %q: %v", where, w, s.Glob, err)
			}
		}
		if s.Strip != nil && (*s.Strip < 0 || *s.Strip > 9) {
			add("%s: %s.strip must be within 0..9", where, w)
		}
		if s.Root != "" {
			checkPath(w+".root", s.Root)
		}
	}
	if p.VersionFile == "" {
		add("%s: patches.version_file (the file in the fork that pins the upstream version) is required", where)
	} else {
		checkPath("patches.version_file", p.VersionFile)
	}
	checkRegex("patches.version_regex", p.VersionRegex)
	for f, content := range p.UpdateFiles {
		checkPath("patches.update_files", f)
		if f == p.VersionFile {
			add("%s: patches.update_files must not contain the version file", where)
		}
		if len(content) > 64<<10 {
			add("%s: patches.update_files %q is larger than 64 KiB", where, f)
		}
	}
	seen := map[string]bool{}
	for i, s := range p.Sources {
		w := fmt.Sprintf("patches.sources[%d]", i)
		if s == nil {
			add("%s: %s is null", where, w)
			continue
		}
		checkPath(w+".path", s.Path)
		if seen[s.Path] {
			add("%s: %s: path %q is configured twice", where, w, s.Path)
		}
		seen[s.Path] = true
		if err := validateRemoteURL(s.URL); err != nil {
			add("%s: %s.url: %v", where, w, err)
		}
		checkFetch(w+".fetch", s.Fetch)
		if (s.RevisionFile == "") != (s.RevisionRegex == "") {
			add("%s: %s: set both revision_file and revision_regex, or neither (then the commit is the submodule entry at the path)", where, w)
		}
		if s.RevisionFile != "" {
			checkPath(w+".revision_file", s.RevisionFile)
		}
		checkRegex(w+".revision_regex", s.RevisionRegex)
	}
	if p.Fuzz < 0 || p.Fuzz > 3 {
		add("%s: patches.fuzz must be within 0..3", where)
	}
	switch p.VerifyTree {
	case "touched", "full":
	default:
		add("%s: patches.verify_tree must be touched or full", where)
	}
	if len(r.KeepOurs) > 0 {
		add("%s: keep_ours does not apply to patch mode (the fork's files are its own; upstream is not merged into it)", where)
	}
	if r.Review.IsEnabled() {
		add("%s: review.enabled cannot be true in patch mode: the fork merges no upstream commits (it pins versions), and the patch agent's output is audited", where)
	}
	if len(r.Review.AllowCommits) > 0 {
		add("%s: review.allow_commits does not apply to patch mode", where)
	}
	if len(r.Prefetch) > 0 && len(r.Verify) == 0 {
		add("%s: prefetch commands run before verify commands; patch mode runs none without verify", where)
	}
}

// cleanRelPath checks a path relative to a repository root.
func cleanRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\r\n\\") {
		return errors.New("must be a relative path")
	}
	c := path.Clean(p)
	if c != p || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return errors.New("must be a clean relative path inside the repository")
	}
	for _, seg := range strings.Split(c, "/") {
		if strings.EqualFold(seg, ".git") {
			return errors.New("must not be inside .git")
		}
	}
	return nil
}

func validateRemoteURL(s string) error {
	if s == "" {
		return errors.New("required")
	}
	if strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n") {
		return errors.New("invalid")
	}
	// ext:: and fd:: transports execute commands; never allow them.
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "ext::") || strings.HasPrefix(low, "fd::") {
		return errors.New("transport not allowed")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return err
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "file":
		default:
			return fmt.Errorf("scheme %q not allowed", u.Scheme)
		}
		if u.User != nil {
			if _, hasPw := u.User.Password(); hasPw {
				return errors.New("do not embed credentials in the URL; use auth.token")
			}
		}
		return nil
	}
	// scp-like syntax: user@host:path
	if strings.Contains(s, ":") && !strings.HasPrefix(s, "/") {
		return nil
	}
	if filepath.IsAbs(s) {
		return nil // local path (tests)
	}
	return errors.New("must be a URL, scp-style ssh address, or absolute path")
}

// ApplyDefaults fills unset broker fields.
func (s *Sandboxd) ApplyDefaults() {
	if s.Listen == "" {
		s.Listen = "/run/vibeci/sandboxd.sock"
	}
	if s.DockerHost == "" {
		s.DockerHost = "unix:///var/run/docker.sock"
	}
	if s.DataRoot == "" {
		s.DataRoot = "/data"
	}
	s.DataRoot = filepath.Clean(s.DataRoot)
	if s.MaxSandboxes == 0 {
		s.MaxSandboxes = 8
	}
	if s.MaxLifetime.Duration == 0 {
		s.MaxLifetime.Duration = 6 * time.Hour
	}
	if s.MaxExecTimeout.Duration == 0 {
		s.MaxExecTimeout.Duration = 2 * time.Hour
	}
	for _, p := range s.Profiles {
		if p == nil {
			continue
		}
		if p.Pull == "" {
			p.Pull = "never"
		}
		if p.Network == "" {
			p.Network = "none"
		}
		if p.Memory == "" {
			p.Memory = "4g"
		}
		if p.CPUs == 0 {
			p.CPUs = 2
		}
		if p.Pids == 0 {
			p.Pids = 2048
		}
		if p.TmpSize == "" {
			p.TmpSize = "4g"
		}
		if p.User == "" {
			p.User = "10001:10001"
		}
	}
}

var userRe = regexp.MustCompile(`^[0-9]{1,10}(:[0-9]{1,10})?$`)

// Validate checks the broker config.
func (s *Sandboxd) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !filepath.IsAbs(s.Listen) {
		add("listen must be an absolute socket path")
	}
	if !filepath.IsAbs(s.DataRoot) {
		add("data_root must be absolute")
	}
	if (s.DataVolume == "") == (s.DataHostPath == "") {
		add("exactly one of data_volume or data_host_path is required")
	}
	if s.DataVolume != "" && !networkRe.MatchString(s.DataVolume) {
		add("data_volume %q is not a valid volume name", s.DataVolume)
	}
	if s.DataHostPath != "" && !filepath.IsAbs(s.DataHostPath) {
		add("data_host_path must be absolute")
	}
	if !strings.HasPrefix(s.DockerHost, "unix://") && !strings.HasPrefix(s.DockerHost, "tcp://") {
		add("docker_host must be unix:// or tcp://")
	}
	if len(s.Profiles) == 0 {
		add("at least one profile is required")
	}
	for name, p := range s.Profiles {
		if p == nil {
			add("profile %q is null", name)
			continue
		}
		if !NameRe.MatchString(name) {
			add("profile name %q is invalid", name)
		}
		if p.Image == "" || strings.ContainsAny(p.Image, " \t\n") {
			add("profile %q: image is required", name)
		}
		if p.Pull != "never" && p.Pull != "missing" {
			add("profile %q: pull must be never or missing", name)
		}
		if p.Network != "none" && !networkRe.MatchString(p.Network) {
			add("profile %q: invalid network %q", name, p.Network)
		}
		if p.Network == "host" {
			add("profile %q: host networking is not allowed", name)
		}
		if _, err := ParseBytes(p.Memory); err != nil {
			add("profile %q: memory: %v", name, err)
		}
		if _, err := ParseBytes(p.TmpSize); err != nil {
			add("profile %q: tmp_size: %v", name, err)
		}
		if p.CPUs < 0 || p.CPUs > 256 {
			add("profile %q: cpus out of range", name)
		}
		if !userRe.MatchString(p.User) || p.User == "0" || strings.HasPrefix(p.User, "0:") {
			add("profile %q: user must be a numeric non-root uid[:gid]", name)
		}
		for k := range p.Env {
			if !envKeyRe.MatchString(k) {
				add("profile %q: invalid env name %q", name, k)
			}
		}
	}
	return errors.Join(errs...)
}
