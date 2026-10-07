// Command vibeci keeps forks merged with their upstreams: it reviews new
// upstream commits for malicious changes, merges them (resolving conflicts
// with an LLM agent in a sandbox), verifies the result and pushes it.
// Patch-mode forks (a series of patches plus a pinned upstream version) are
// moved to new upstream versions, with the patches that no longer apply
// rewritten the same way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/vibeci/vibeci/internal/alert"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/pipeline"
	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/state"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 12 {
					v += "+" + s.Value[:12]
				}
			}
		}
	}
	return "vibeci " + v
}

const usageText = `usage: vibeci <command> [flags]

Commands:
  daemon     sync all repos every interval (SIGUSR1: sync now, SIGHUP: reload config)
  run        run one sync cycle and exit (-repo, -dry-run, -force, -json)
  review     review pending upstream commits without merging (-repo, -n, -json)
  patches    show how a patch-mode fork's patches apply to an upstream version (-repo, -version, -json)
  status     show the state of every repo (-repo, -json)
  allow      let upstream commits through the review gate; re-applies excluded ones
  exclude    keep upstream commits out of the fork; removes already merged ones
  release    clear an upstream-rewrite hold and failure backoff (-repo, -json)
  check      validate config, credentials, models, remotes and the sandbox (-json)
  config     print the effective configuration as JSON (defaults applied, secrets redacted)
  health     exit 0 if the daemon completed a cycle recently (container healthcheck)
  sandboxd   run the sandbox broker (the only component with Docker access)
  action     run one command as a GitHub Actions step (action.yml; see vibeci action -h)
  version    print the version

Every command takes -config FILE and -h for its flags. -config may be
repeated: the files are merged in order, later ones overriding earlier ones
key by key (arrays are replaced). Default: $VIBECI_CONFIG, a list separated
like PATH, else /etc/vibeci/config.jsonc.

Exit codes: 0 ok; 1 failure or error (for run: a merge
failed or an infrastructure error happened); 2 usage error; 3 (run, review)
attention needed: an upstream commit blocks merging (review.on_blocked is
hold, or the commit cannot be excluded) or upstream rewrote merged history.
`

// exitCode lets commands choose the process exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon(args)
	case "run":
		err = cmdRun(args)
	case "review":
		err = cmdReview(args)
	case "patches":
		err = cmdPatches(args)
	case "status":
		err = cmdStatus(args)
	case "release":
		err = cmdRelease(args)
	case "allow":
		err = cmdAllow(args)
	case "exclude":
		err = cmdExclude(args)
	case "config":
		err = cmdConfig(args)
	case "check":
		err = cmdCheck(args)
	case "health":
		err = cmdHealth(args)
	case "sandboxd":
		err = cmdSandboxd(args)
	case "action":
		err = cmdAction(args)
	case "version", "-version", "--version":
		fmt.Println(versionString())
	case "help", "-h", "-help", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "vibeci: unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "vibeci:", err)
		os.Exit(1)
	}
}

// configFlag is the repeatable -config flag. Several files are merged in
// order, later ones overriding earlier ones key by key (config.Merge). The
// first -config replaces the default list.
type configFlag struct {
	paths []string
	set   bool
}

func (c *configFlag) String() string {
	if c == nil {
		return ""
	}
	return strings.Join(c.paths, string(filepath.ListSeparator))
}

func (c *configFlag) Set(v string) error {
	if v == "" {
		return errors.New("empty path")
	}
	if !c.set {
		c.paths, c.set = nil, true
	}
	c.paths = append(c.paths, v)
	return nil
}

// configVar registers -config on fs. Default: $VIBECI_CONFIG, a list
// separated like PATH, else /etc/vibeci/config.jsonc.
func configVar(fs *flag.FlagSet) *configFlag {
	c := &configFlag{}
	for _, p := range filepath.SplitList(os.Getenv("VIBECI_CONFIG")) {
		if p != "" {
			c.paths = append(c.paths, p)
		}
	}
	if len(c.paths) == 0 {
		c.paths = []string{"/etc/vibeci/config.jsonc"}
	}
	fs.Var(c, "config", "config file; repeat to merge several, later files overriding earlier ones key by key")
	return c
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("VIBECI_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func loadConfig(c *configFlag) (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(c.paths...)
	if err != nil {
		return nil, nil, err
	}
	return cfg, newLogger(cfg.LogLevel), nil
}

func findRepo(cfg *config.Config, name string) (*config.Repo, error) {
	for _, r := range cfg.Repos {
		if r.Name == name {
			return r, nil
		}
	}
	var names []string
	for _, r := range cfg.Repos {
		names = append(names, r.Name)
	}
	return nil, fmt.Errorf("no repo named %q (configured: %s)", name, strings.Join(names, ", "))
}

// localRunner builds a runner for commands that need neither models nor
// sandboxes.
func localRunner(cfg *config.Config, logger *slog.Logger) (*pipeline.Runner, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	g, err := gitx.New(filepath.Join(cfg.DataDir, "home"), gitx.Identity{Name: cfg.Identity.Name, Email: cfg.Identity.Email}, logger)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(filepath.Join(cfg.DataDir, "state"))
	if err != nil {
		return nil, err
	}
	return &pipeline.Runner{Cfg: cfg, G: g, Store: store, Logger: logger}, nil
}

// setup builds the pipeline runner. daemon mode also owns cleanup of
// embedded docker sandboxes.
func setup(ctx context.Context, cfg *config.Config, logger *slog.Logger, daemon bool) (*pipeline.Runner, func(), error) {
	r, err := localRunner(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	if r.Alerts, err = alert.New(cfg.Alerts, logger); err != nil {
		return nil, nil, err
	}
	if r.Models, err = buildModels(cfg, logger); err != nil {
		return nil, nil, err
	}
	sb, cleanup, err := sandboxProvider(ctx, cfg, logger, daemon)
	if err != nil {
		return nil, nil, err
	}
	r.Sandbox = sb
	return r, cleanup, nil
}

func sandboxProvider(ctx context.Context, cfg *config.Config, logger *slog.Logger, daemon bool) (sandbox.Provider, func(), error) {
	switch cfg.Sandbox.Mode {
	case "broker":
		return sandbox.NewBrokerClient(cfg.Sandbox.Socket), func() {}, nil
	case "docker":
		p, err := sandbox.NewDockerProvider(cfg.Sandbox.Docker, logger)
		if err != nil {
			return nil, nil, err
		}
		if !daemon {
			return p, func() {}, nil
		}
		if err := p.CleanupStale(ctx); err != nil {
			logger.Warn("stale sandbox cleanup failed", "err", err)
		}
		rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		go reapLoop(rctx, p)
		return p, cancel, nil
	case "unsafe-local":
		logger.Warn("sandbox.mode is unsafe-local: agent and build commands run directly on this host WITHOUT isolation")
		return &sandbox.LocalProvider{DataRoot: cfg.DataDir}, func() {}, nil
	}
	return nil, nil, fmt.Errorf("unknown sandbox mode %q", cfg.Sandbox.Mode)
}

// usedModels lists the model names referenced by any role.
func usedModels(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r config.Roles) {
		for _, n := range append([]string{r.Triage, r.Investigate, r.Audit}, r.Resolve...) {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(cfg.Roles)
	for _, r := range cfg.Repos {
		add(cfg.RolesFor(r))
	}
	return out
}

func buildModels(cfg *config.Config, logger *slog.Logger) (map[string]llm.Client, error) {
	specs := map[string]llm.ProviderSpec{}
	out := map[string]llm.Client{}
	for _, name := range usedModels(cfg) {
		m := cfg.Models[name]
		spec, ok := specs[m.Provider]
		if !ok {
			var err error
			if spec, err = providerSpec(cfg.Providers[m.Provider]); err != nil {
				return nil, fmt.Errorf("provider %q: %w", m.Provider, err)
			}
			specs[m.Provider] = spec
		}
		c, err := llm.New(spec, llm.ModelOptions{
			Name: name, Model: m.Model, MaxTokens: m.MaxTokens, ContextWindow: m.ContextWindow,
			Thinking: m.Thinking, ThinkingBudget: m.ThinkingBudget, Effort: m.Effort,
			Temperature: m.Temperature, PromptCache: m.PromptCache == nil || *m.PromptCache,
		}, logger)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", name, err)
		}
		out[name] = c
	}
	return out, nil
}

func providerSpec(p *config.Provider) (llm.ProviderSpec, error) {
	spec := llm.ProviderSpec{
		Type: p.Type, BaseURL: p.BaseURL, Auth: p.Auth, Headers: p.Headers,
		TimeoutSeconds: int(p.Timeout.Seconds()), MaxRetries: *p.MaxRetries,
		Stream: p.Stream == nil || *p.Stream, MaxTokensField: p.MaxTokensField,
	}
	var err error
	if p.APIKey.IsSet() {
		spec.APIKey, err = p.APIKey.Resolve()
	}
	return spec, err
}
