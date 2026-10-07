package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vibeci/vibeci/internal/alert"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/pipeline"
	"github.com/vibeci/vibeci/internal/sandbox"
	"github.com/vibeci/vibeci/internal/source"
	"github.com/vibeci/vibeci/internal/state"
)

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := configVar(fs)
	fs.Parse(args)
	cfg, logger, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	now := make(chan os.Signal, 1)
	signal.Notify(now, syscall.SIGUSR1)

	runner, cleanup, err := setup(ctx, cfg, logger, true)
	if err != nil {
		return err
	}
	defer func() { cleanup() }()
	logger.Info("vibeci daemon starting", "version", versionString(), "repos", len(cfg.Repos), "interval", cfg.Interval.Duration, "sandbox", cfg.Sandbox.Mode)
	if err := runner.Sandbox.Health(ctx); err != nil {
		logger.Error("sandbox provider is not healthy; merges needing a sandbox will fail until it is", "err", err)
	}
	runner.WriteHeartbeat(nil)
	for {
		start := time.Now()
		outs := runner.Cycle(ctx, "", pipeline.Options{})
		runner.GCJobs(cfg.KeepJobs.Duration)
		summary := map[string]int{}
		for _, o := range outs {
			summary[o.Status]++
		}
		logger.Info("cycle finished", "took", time.Since(start).Round(time.Second), "statuses", summary, "next", time.Now().Add(cfg.Interval.Duration).Format(time.RFC3339))
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return nil
		case <-time.After(cfg.Interval.Duration):
		case <-now:
			logger.Info("SIGUSR1: starting a cycle now")
		case <-hup:
			ncfg, err := config.Load(cfgPath.paths...)
			if err != nil {
				logger.Error("config reload failed; keeping the previous config", "err", err)
				continue
			}
			nrunner, ncleanup, err := setup(ctx, ncfg, newLogger(ncfg.LogLevel), true)
			if err != nil {
				logger.Error("config reload failed; keeping the previous config", "err", err)
				continue
			}
			cleanup()
			cfg, runner, cleanup, logger = ncfg, nrunner, ncleanup, nrunner.Logger
			logger.Info("config reloaded", "repos", len(cfg.Repos))
		}
	}
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := configVar(fs)
	repo := fs.String("repo", "", "sync only this repo")
	dry := fs.Bool("dry-run", false, "compute and verify merges but do not push")
	force := fs.Bool("force", false, "ignore failure backoff and cached dry-run results")
	asJSON := fs.Bool("json", false, "print outcomes as JSON")
	fs.Parse(args)
	cfg, logger, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if *repo != "" {
		if _, err := findRepo(cfg, *repo); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runner, cleanup, err := setup(ctx, cfg, logger, false)
	if err != nil {
		return err
	}
	defer cleanup()
	outs := runner.Cycle(ctx, *repo, pipeline.Options{DryRun: *dry, Force: *force})
	runner.GCJobs(cfg.KeepJobs.Duration)
	if *asJSON {
		printJSON(outs)
	} else {
		for _, o := range outs {
			printOutcome(o)
		}
	}
	code := 0
	for _, o := range outs {
		switch {
		case o.Status == pipeline.StatusFailed || o.Status == pipeline.StatusError || o.StateError != "":
			code = 1
		case o.NeedsAttention() && code == 0:
			code = 3
		}
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

func printOutcome(o *pipeline.Outcome) {
	fmt.Printf("%-20s %-11s %s\n", o.Repo, o.Status, o.Message)
	var extra []string
	if o.JobID != "" {
		extra = append(extra, "job "+o.JobID)
	}
	if len(o.Blocked) > 0 {
		extra = append(extra, fmt.Sprintf("blocked: %s", strings.Join(shorts(o.Blocked), ", ")))
	}
	if len(o.Excluded) > 0 {
		extra = append(extra, fmt.Sprintf("excluded: %s", strings.Join(shorts(o.Excluded), ", ")))
	}
	if len(o.Restored) > 0 {
		extra = append(extra, fmt.Sprintf("restored: %s", strings.Join(shorts(o.Restored), ", ")))
	}
	if o.Usage.Calls > 0 || o.Usage.InputTokens > 0 {
		extra = append(extra, fmt.Sprintf("%d LLM calls, %s in / %s out tokens (cache read %s)", o.Usage.Calls, kilo(o.Usage.InputTokens), kilo(o.Usage.OutputTokens), kilo(o.Usage.CacheReadTokens)))
	}
	extra = append(extra, o.Duration)
	fmt.Printf("%-20s %-11s %s\n", "", "", strings.Join(extra, "; "))
	if o.StateError != "" {
		fmt.Printf("%-20s %-11s %s\n", "", "", "STATE NOT SAVED TO THE FORK: "+o.StateError)
	}
}

func shorts(shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = short(s)
	}
	return out
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func kilo(n int) string {
	if n >= 10000 {
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	cfgPath := configVar(fs)
	name := fs.String("repo", "", "repo to review (default: the only configured repo)")
	n := fs.Int("n", 0, "review at most this many commits (default review.max_commits_per_run)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Parse(args)
	cfg, logger, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	repo, err := pickRepo(cfg, *name)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runner, cleanup, err := setup(ctx, cfg, logger, false)
	if err != nil {
		return err
	}
	defer cleanup()
	rep, err := runner.Review(ctx, repo, *n)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(rep)
	} else {
		fmt.Printf("%s: %d upstream commit(s) pending on %s (%s)", rep.Repo, rep.Total, rep.Label, short(rep.Upstream))
		if rep.Limited {
			fmt.Printf(", reviewed the first %d", len(rep.Verdicts))
		}
		fmt.Println()
		for _, v := range rep.Verdicts {
			if v == nil {
				continue
			}
			fmt.Printf("  %s  %-10s %.2f  %-11s %s\n", short(v.Commit), v.Verdict, v.Confidence, v.Stage, clip(v.Summary, 160))
			for _, f := range v.Findings {
				fmt.Printf("      - %s [%s]: %s\n", f.File, f.Category, clip(f.Evidence, 160))
			}
		}
		fmt.Printf("safe merge target: %s", orDash(short(rep.Target)))
		if len(rep.Excluded) > 0 {
			fmt.Printf("; excluded from the merge: %s", strings.Join(shorts(rep.Excluded), ", "))
		}
		if rep.Blocked != "" {
			fmt.Printf("; first blocked commit: %s", short(rep.Blocked))
		}
		fmt.Printf("\nLLM usage: %d calls, %s in / %s out tokens\n", rep.Usage.Calls, kilo(rep.Usage.InputTokens), kilo(rep.Usage.OutputTokens))
	}
	if rep.Blocked != "" {
		return exitCode(3)
	}
	return nil
}

func cmdPatches(args []string) error {
	fs := flag.NewFlagSet("patches", flag.ExitOnError)
	cfgPath := configVar(fs)
	name := fs.String("repo", "", "patch-mode repo (default: the only configured repo)")
	ver := fs.String("version", "", "upstream version to apply the patches to (default: the version a sync would move to)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: vibeci patches [-repo NAME] [-version V] [-json]\n\nApply a patch-mode fork's patches to an upstream version and report each\npatch's status (exact, shifted, refreshed, dropped, failed) without running\nthe agent, committing or pushing.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	cfg, logger, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	repo, err := pickRepo(cfg, *name)
	if err != nil {
		return err
	}
	r, err := localRunner(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rep, err := r.Patches(ctx, repo, *ver)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(rep)
		return nil
	}
	fmt.Printf("%s: pinned to upstream %s; patches applied to %s (tag %s, %s)\n", rep.Repo, rep.From, rep.To, rep.Tag, short(rep.Upstream))
	for _, p := range rep.Patches {
		fmt.Printf("  %-9s %s\n", p.Status, p.Path)
		if p.Problem != "" {
			fmt.Printf("      %s\n", p.Problem)
		}
		for _, h := range p.Failed {
			switch {
			case h.Problem != "":
				fmt.Printf("      %s: %s\n", h.File, h.Problem)
			case h.Total > 0:
				fmt.Printf("      %s hunk %d: expected at line %d; closest match at line %d (%d of %d old lines)\n", h.File, h.Hunk, h.Line, h.Near, h.Matched, h.Total)
			default:
				fmt.Printf("      %s hunk %d: expected at line %d\n", h.File, h.Hunk, h.Line)
			}
		}
	}
	c := rep.Counts
	fmt.Printf("%d patch(es): %d exact, %d shifted, %d refreshed, %d dropped, %d failed\n", len(rep.Patches), c["exact"], c["shifted"], c["refreshed"], c["dropped"], c["failed"])
	if c["failed"] > 0 {
		fmt.Println("A sync has the patch agent rewrite the failed patches (see `vibeci run -repo " + repo.Name + " -dry-run`).")
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := configVar(fs)
	only := fs.String("repo", "", "show only this repo")
	asJSON := fs.Bool("json", false, "print raw state as JSON")
	fs.Parse(args)
	cfg, err := config.Load(cfgPath.paths...)
	if err != nil {
		return err
	}
	if *only != "" {
		r, err := findRepo(cfg, *only)
		if err != nil {
			return err
		}
		cfg.Repos = []*config.Repo{r}
	}
	store, err := state.Open(filepath.Join(cfg.DataDir, "state"))
	if err != nil {
		return err
	}
	var busy []string // repos whose state could not be refreshed from the fork
	if cfg.StateRef != "" {
		runner, err := localRunner(cfg, newLogger(cfg.LogLevel))
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		for _, r := range cfg.Repos {
			if ok, err := runner.PullState(ctx, r); err != nil {
				return err
			} else if !ok {
				busy = append(busy, r.Name)
			}
		}
	}
	states := map[string]*state.RepoState{}
	for _, r := range cfg.Repos {
		st, err := store.Load(r.Name)
		if err != nil {
			return err
		}
		states[r.Name] = st
	}
	var hb pipeline.Heartbeat
	hbFound, _ := store.ReadJSON("heartbeat.json", &hb)
	if *asJSON {
		var hbOut any
		if hbFound {
			hbOut = hb
		}
		printJSON(map[string]any{"repos": states, "heartbeat": hbOut})
		return nil
	}
	if hbFound {
		fmt.Printf("last cycle: %s (%s ago)\n\n", hb.Time.Format(time.RFC3339), time.Since(hb.Time).Round(time.Second))
	}
	if len(busy) > 0 {
		fmt.Printf("being synced by another process here; showing the local state of: %s\n\n", strings.Join(busy, ", "))
	}
	ts := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	for _, r := range cfg.Repos {
		st := states[r.Name]
		enabled := ""
		if !r.IsEnabled() {
			enabled = " (disabled)"
		}
		fmt.Printf("%s%s: %s\n", r.Name, enabled, orDash(st.LastResult))
		if st.LastMessage != "" {
			fmt.Printf("  %s\n", clip(st.LastMessage, 300))
		}
		fmt.Printf("  last attempt %s, last push %s", ts(st.LastAttempt), ts(st.LastSuccess))
		if st.LastJob != "" {
			fmt.Printf(", last job %s", st.LastJob)
		}
		if r.PatchMode() {
			fmt.Printf("\n  fork %s  pinned upstream %s  newest upstream %s (%s)", orDash(short(st.LastForkHead)), orDash(st.PinnedVersion), orDash(st.UpstreamVersion), orDash(short(st.LastUpstreamTip)))
		} else {
			fmt.Printf("\n  fork %s  upstream %s  last merged %s", orDash(short(st.LastForkHead)), orDash(short(st.LastUpstreamTip)), orDash(short(st.LastMerged)))
			if st.LastMergedTag != "" {
				fmt.Printf(" (%s)", st.LastMergedTag)
			}
		}
		fmt.Println()
		if h := st.RewriteHold; h != nil {
			fmt.Printf("  ON HOLD since %s: upstream rewrote history (merged %s, now %s); run `vibeci release -repo %s` once handled\n", ts(h.Since), short(h.OldTip), short(h.NewTip), r.Name)
		}
		if f := st.Failure; f != nil {
			fmt.Printf("  %s %d time(s) (streak %d) since %s; next retry %s\n    %s\n", f.Kind, f.Count, f.Streak, ts(f.FirstAt), ts(f.NextRetry), clip(f.Error, 400))
		}
		if len(st.Blocked) > 0 {
			var shas []string
			for sha := range st.Blocked {
				shas = append(shas, sha)
			}
			sort.Strings(shas)
			fmt.Println("  blocked upstream commits:")
			for _, sha := range shas {
				b := st.Blocked[sha]
				fmt.Printf("    %s %s %.2f (since %s): %s\n", short(sha), b.Verdict, b.Confidence, ts(b.FirstSeen), clip(b.Summary, 200))
			}
		}
		if len(st.Excluded) > 0 {
			var shas []string
			for sha := range st.Excluded {
				shas = append(shas, sha)
			}
			sort.Strings(shas)
			fmt.Println("  excluded upstream commits (kept out of the fork):")
			for _, sha := range shas {
				x := st.Excluded[sha]
				what := "removed"
				switch {
				case x.Restore:
					what = "restore pending"
				case !x.Removed:
					what = "removal pending"
				}
				detail := x.Summary
				if x.Verdict != "" {
					detail = fmt.Sprintf("%s %.2f: %s", x.Verdict, x.Confidence, x.Summary)
				}
				fmt.Printf("    %s %s, %s (since %s): %s\n", short(sha), x.Source, what, ts(x.Since), clip(detail, 200))
			}
		}
		if len(st.Allowed) > 0 {
			var shas []string
			for sha := range st.Allowed {
				shas = append(shas, sha)
			}
			sort.Strings(shas)
			fmt.Println("  allowed commits (vibeci allow):")
			for _, sha := range shas {
				a := st.Allowed[sha]
				fmt.Printf("    %s (%s) %s\n", short(sha), ts(a.Time), clip(a.Reason, 200))
			}
		}
		if p := st.Proposal; p != nil {
			fmt.Printf("  dry-run proposal %s (target %s, job %s)\n", short(p.Commit), short(p.Target), p.JobID)
		}
		u := st.Usage
		fmt.Printf("  LLM usage total: %d calls, %s in / %s out tokens, cache read %s / write %s\n\n", u.Calls, kilo(int(u.InputTokens)), kilo(int(u.OutputTokens)), kilo(int(u.CacheReadTokens)), kilo(int(u.CacheWriteTokens)))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdRelease(args []string) error {
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	cfgPath := configVar(fs)
	name := fs.String("repo", "", "repo to release (default: the only configured repo)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)
	cfg, err := config.Load(cfgPath.paths...)
	if err != nil {
		return err
	}
	repo, err := pickRepo(cfg, *name)
	if err != nil {
		return err
	}
	r, err := localRunner(cfg, newLogger(cfg.LogLevel))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := r.Release(ctx, repo); err != nil {
		return err
	}
	if *asJSON {
		printJSON(map[string]any{"repo": repo.Name, "released": true})
		return nil
	}
	fmt.Printf("%s: hold and backoff cleared; the next sync accepts upstream's current history\n", repo.Name)
	return nil
}

func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	cfgPath := configVar(fs)
	fs.Parse(args)
	cfg, err := config.Load(cfgPath.paths...)
	if err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(cfg.DataDir, "state"))
	if err != nil {
		return err
	}
	var hb pipeline.Heartbeat
	found, err := store.ReadJSON("heartbeat.json", &hb)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no heartbeat yet")
	}
	maxAge := 2*cfg.Interval.Duration + cfg.RepoTimeout.Duration
	if age := time.Since(hb.Time); age > maxAge {
		return fmt.Errorf("last heartbeat %s ago (limit %s)", age.Round(time.Second), maxAge)
	}
	fmt.Println("ok")
	return nil
}

func cmdSandboxd(args []string) error {
	fs := flag.NewFlagSet("sandboxd", flag.ExitOnError)
	def := os.Getenv("VIBECI_SANDBOXD_CONFIG")
	if def == "" {
		def = "/etc/vibeci/sandboxd.jsonc"
	}
	cfgPath := fs.String("config", def, "broker config file")
	fs.Parse(args)
	cfg, err := config.LoadSandboxd(*cfgPath)
	if err != nil {
		return err
	}
	logger := newLogger(os.Getenv("VIBECI_LOG_LEVEL"))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	p, err := sandbox.NewDockerProvider(cfg, logger)
	if err != nil {
		return err
	}
	for i := 0; ; i++ {
		err := p.Health(ctx)
		if err == nil {
			break
		}
		if i >= 24 || ctx.Err() != nil {
			return fmt.Errorf("docker engine unreachable at %s: %w", cfg.DockerHost, err)
		}
		logger.Warn("waiting for the docker engine", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	v, _ := p.Version(ctx)
	var profiles []string
	for name := range cfg.Profiles {
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	logger.Info("vibeci sandboxd starting", "version", versionString(), "docker", v, "profiles", profiles, "max_sandboxes", cfg.MaxSandboxes)
	return (&sandbox.Broker{P: p, Logger: logger}).Serve(ctx, cfg.Listen)
}

func reapLoop(ctx context.Context, p *sandbox.DockerProvider) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Reap(ctx)
		}
	}
}

var gitVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := configVar(fs)
	noLLM := fs.Bool("no-llm", false, "skip the model round trips")
	noSandbox := fs.Bool("no-sandbox", false, "skip starting test sandboxes")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	fs.Parse(args)
	type checkResult struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	var results []checkResult
	failed := false
	report := func(what string, err error, detail string) {
		r := checkResult{Name: what, OK: err == nil, Detail: detail}
		if err != nil {
			failed = true
			r.Error = err.Error()
			if !*asJSON {
				fmt.Printf("FAIL  %-34s %v\n", what, err)
			}
		} else if !*asJSON {
			fmt.Printf("ok    %-34s %s\n", what, detail)
		}
		results = append(results, r)
	}
	finish := func() error {
		if *asJSON {
			printJSON(map[string]any{"ok": !failed, "checks": results})
		}
		if failed {
			return exitCode(1)
		}
		if !*asJSON {
			fmt.Println("\nall checks passed")
		}
		return nil
	}
	cfg, err := config.Load(cfgPath.paths...)
	report("config", err, strings.Join(cfgPath.paths, " + "))
	if err != nil {
		return finish()
	}
	logger := newLogger("warn")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	out, err := exec.Command("git", "version").Output()
	if err == nil {
		if m := gitVersionRe.FindStringSubmatch(string(out)); m == nil {
			err = fmt.Errorf("unexpected git version %q", out)
		} else {
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			if major < 2 || (major == 2 && minor < 38) {
				err = fmt.Errorf("git >= 2.38 is required (merge-tree --write-tree), found %s", strings.TrimSpace(string(out)))
			}
		}
	}
	report("git", err, strings.TrimSpace(string(out)))

	err = os.MkdirAll(cfg.DataDir, 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(cfg.DataDir, ".check-*"); err == nil {
			f.Close()
			os.Remove(f.Name())
		}
	}
	report("data dir writable", err, cfg.DataDir)

	_, err = alert.New(cfg.Alerts, logger)
	report("alert webhooks", err, fmt.Sprintf("%d configured", len(cfg.Alerts)))

	models, err := buildModels(cfg, logger)
	report("model credentials", err, fmt.Sprintf("%d model(s) in use", len(models)))
	if err == nil && !*noLLM {
		var names []string
		for n := range models {
			names = append(names, n)
		}
		sort.Strings(names)
		// Concurrent and bounded: a diagnostic should report an unreachable
		// model in minutes, not wait out the whole retry budget.
		type ping struct {
			detail string
			err    error
		}
		pings := make([]ping, len(names))
		var wg sync.WaitGroup
		for i, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				start := time.Now()
				resp, err := models[n].Complete(pctx, &llm.Request{
					System:   "You are a connectivity check.",
					Messages: []llm.Message{{Role: llm.User, Content: []llm.Block{llm.Text("Reply with the single word OK.")}}},
				})
				if err == nil {
					pings[i].detail = fmt.Sprintf("replied %q in %s (%s)", clip(resp.TextContent(), 30), time.Since(start).Round(100*time.Millisecond), cfg.Models[n].Model)
				}
				pings[i].err = err
			}()
		}
		wg.Wait()
		for i, n := range names {
			report("model "+n, pings[i].err, pings[i].detail)
		}
	}

	sb, cleanup, err := sandboxProvider(ctx, cfg, logger, false)
	if err == nil {
		defer cleanup()
		err = sb.Health(ctx)
	}
	report("sandbox provider", err, cfg.Sandbox.Mode)
	if err == nil && !*noSandbox {
		profiles := map[string]bool{}
		for _, r := range cfg.Repos {
			profiles[r.Sandbox.Profile] = true
			profiles[r.Sandbox.VerifyProfile] = true
			profiles[r.Sandbox.PrefetchProfile] = true
		}
		var names []string
		for p := range profiles {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			detail, err := checkProfile(ctx, cfg, sb, p)
			report("sandbox profile "+p, err, detail)
		}
	}

	g, err := gitx.New(filepath.Join(cfg.DataDir, "home"), gitx.Identity{Name: cfg.Identity.Name, Email: cfg.Identity.Email}, logger)
	if err == nil {
		for _, r := range cfg.Repos {
			mirror, err := g.InitBare(ctx, filepath.Join(cfg.DataDir, "mirrors", r.Name+".git"))
			if err != nil {
				report("mirror "+r.Name, err, "")
				continue
			}
			sides := []struct {
				which string
				rem   config.Remote
				ref   string
			}{{"fork", r.Fork, "refs/heads/" + r.Fork.Branch}, {"upstream", r.Upstream, "refs/heads/" + r.Upstream.Branch}}
			if r.PatchMode() {
				sides = sides[:1]
				checkPatchSources(ctx, cfg, g, r, mirror, report)
			}
			for _, side := range sides {
				if side.rem.Tags != "" {
					side.ref = "HEAD"
				}
				auth, err := pipeline.ResolveAuth(cfg.DataDir, r.Name, side.which, side.rem.Auth)
				var sha string
				if err == nil {
					sha, err = mirror.LsRemote(ctx, side.rem.URL, auth, side.ref)
				}
				if err == nil && sha == "" {
					err = fmt.Errorf("%s not found at %s", side.ref, side.rem.URL)
				}
				report(fmt.Sprintf("%s %s", r.Name, side.which), err, fmt.Sprintf("%s @ %s", side.ref, short(sha)))
			}
			if cfg.StateRef != "" {
				runner := &pipeline.Runner{Cfg: cfg, G: g, Logger: logger}
				sha, err := runner.StateRefTip(ctx, r)
				detail := cfg.StateRef + " @ " + short(sha)
				if err == nil && sha == "" {
					detail = cfg.StateRef + " does not exist yet; the first sync creates it"
				}
				report(r.Name+" state", err, detail)
			}
		}
	}
	return finish()
}

// checkPatchSources checks a patch-mode repo's upstream version source
// and the sub-repositories its patches need.
func checkPatchSources(ctx context.Context, cfg *config.Config, g *gitx.Git, r *config.Repo, mirror *gitx.Repo, report func(string, error, string)) {
	store, err := pipeline.SourceStore(cfg, g, r, nil)
	var up *source.Mirror
	if err == nil {
		up, err = store.Main(ctx)
	}
	var target *pipeline.Target
	if err == nil {
		target, err = pipeline.TargetVersion(ctx, r, up)
	}
	detail := ""
	if err == nil {
		detail = fmt.Sprintf("version %s: tag %s @ %s", target.Version, target.Tag, short(target.Commit))
	}
	report(r.Name+" upstream", err, detail)
	for i, ps := range r.Patches.Sources {
		auth, err := pipeline.ResolveAuth(cfg.DataDir, r.Name, fmt.Sprintf("source-%d", i+1), ps.Auth)
		var sha string
		if err == nil {
			sha, err = mirror.LsRemote(ctx, ps.URL, auth, "HEAD")
		}
		if err == nil && sha == "" {
			err = fmt.Errorf("HEAD not found at %s", ps.URL)
		}
		report(fmt.Sprintf("%s source %s", r.Name, ps.Path), err, ps.URL)
	}
}

// checkProfile starts a throwaway sandbox and confirms the tools the merge
// agent relies on exist in the image.
func checkProfile(ctx context.Context, cfg *config.Config, sb sandbox.Provider, profile string) (string, error) {
	rnd := make([]byte, 4)
	rand.Read(rnd)
	id := "check-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(rnd)
	dir := filepath.Join(cfg.DataDir, "jobs", id, "ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	defer pipeline.RemoveAll(filepath.Join(cfg.DataDir, "jobs", id))
	s, err := sb.Create(ctx, sandbox.CreateRequest{Profile: profile, Workspace: "jobs/" + id + "/ws", Label: id})
	if err != nil {
		return "", err
	}
	defer s.Close(context.WithoutCancel(ctx))
	res, err := s.Exec(ctx, sandbox.ExecRequest{Command: "git --version && touch .vibeci-probe && rm .vibeci-probe && id -u", TimeoutSec: 60, MaxOutput: 4096})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("probe failed (the image needs git and a writable /workspace): %s", clip(res.Combined(), 300))
	}
	info := s.Info()
	return fmt.Sprintf("image %s, network %s, %s", info.Image, info.Network, clip(strings.TrimSpace(res.Stdout), 80)), nil
}

// parseInterleaved parses flags that may follow positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdAllow(args []string) error   { return cmdExclusion("allow", args) }
func cmdExclude(args []string) error { return cmdExclusion("exclude", args) }

// cmdExclusion implements allow and exclude.
func cmdExclusion(which string, args []string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	cfgPath := configVar(fs)
	name := fs.String("repo", "", "repo (default: the only configured repo)")
	reason := fs.String("reason", "", "why; recorded in state, alerts and commit messages")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		if which == "allow" {
			fmt.Fprint(fs.Output(), "usage: vibeci allow [-repo NAME] [-reason TEXT] [-json] COMMIT...\n\nLet upstream commits through the review gate. Commits that were excluded\nand already removed from the fork are re-applied by the next sync.\n\n")
		} else {
			fmt.Fprint(fs.Output(), "usage: vibeci exclude [-repo NAME] [-reason TEXT] [-json] COMMIT...\n\nKeep upstream commits out of the fork. Pending commits are left out of\nfuture merges; commits the fork already merged are removed by the next\nsync. Their content is quarantined in all later merges.\n\n")
		}
		fs.PrintDefaults()
	}
	revs := parseInterleaved(fs, args)
	if len(revs) == 0 {
		fs.Usage()
		return exitCode(2)
	}
	cfg, logger, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	repo, err := pickRepo(cfg, *name)
	if err != nil {
		return err
	}
	r, err := localRunner(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var changes []pipeline.ChangeResult
	if which == "allow" {
		changes, err = r.Allow(ctx, repo, revs, *reason)
	} else {
		changes, err = r.Exclude(ctx, repo, revs, *reason)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(map[string]any{"repo": repo.Name, "changes": changes})
		return nil
	}
	for _, c := range changes {
		fmt.Printf("%s %s: %s\n", short(c.Commit), c.Effect, c.Detail)
	}
	fmt.Printf("Takes effect on the next sync of %s (daemon: next cycle, or send SIGUSR1; or run `vibeci run -repo %s`).\n", repo.Name, repo.Name)
	return nil
}

// cmdConfig prints the effective configuration (defaults applied, secrets
// redacted).
func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	cfgPath := configVar(fs)
	fs.Parse(args)
	cfg, err := config.Load(cfgPath.paths...)
	if err != nil {
		return err
	}
	for _, p := range cfg.Providers {
		for k := range p.Headers {
			p.Headers[k] = "[redacted]"
		}
	}
	printJSON(cfg)
	return nil
}

// pickRepo returns the named repo, or the only configured one.
func pickRepo(cfg *config.Config, name string) (*config.Repo, error) {
	if name == "" && len(cfg.Repos) == 1 {
		return cfg.Repos[0], nil
	}
	if name == "" {
		var names []string
		for _, r := range cfg.Repos {
			names = append(names, r.Name)
		}
		return nil, fmt.Errorf("-repo is required (configured: %s)", strings.Join(names, ", "))
	}
	return findRepo(cfg, name)
}
