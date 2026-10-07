package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/pipeline"
)

const actionUsage = `usage: vibeci action

Runs one VibeCI command as a GitHub Actions step; action.yml calls it. The
inputs arrive in the environment:

  VIBECI_ACTION_CONFIG   config files, one per line, merged in order
                         (default .github/vibeci.jsonc). Relative paths are
                         relative to $GITHUB_WORKSPACE; files missing there
                         are read from this repository at $GITHUB_SHA.
  VIBECI_ACTION_MODELS   JSONC merged last, usually a secret (providers,
                         models and roles, with API keys)
  VIBECI_ACTION_COMMAND  run (default), review, status, allow, exclude,
                         release, check, patches or config
  VIBECI_ACTION_ARGS     further arguments, split like a shell does
  VIBECI_ACTION_MASK     strings to mask in the log and the job summary,
                         one per line
  VIBECI_TOKEN           the fork's token (a repo without fork.auth uses it)

The config starts from defaults for a runner that starts empty: data_dir
under $RUNNER_TEMP, state_ref refs/vibeci/state, docker sandboxes with the
images the action builds (profiles default, default-egress, go, go-egress).
A repo without fork.url is this repository at its default branch.

Results: the step outputs status, commit, json and records, the job
summary and annotations; the exit status is the command's.
`

// actionCommands are the commands the action runs, and whether they take
// -json.
var actionCommands = map[string]bool{
	"run": true, "review": true, "status": true, "allow": true, "exclude": true,
	"release": true, "check": true, "patches": true, "config": false,
}

func cmdAction(args []string) error {
	fs := flag.NewFlagSet("action", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), actionUsage) }
	fs.Parse(args)
	if fs.NArg() > 0 {
		fs.Usage()
		return exitCode(2)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := &ghAction{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr, exe: exe}
	return a.run(ctx)
}

// ghAction is one run of `vibeci action`.
type ghAction struct {
	getenv         func(string) string
	stdout, stderr io.Writer
	exe            string // the vibeci binary that runs the command

	work    string // $RUNNER_TEMP/vibeci
	masks   []string
	source  *gitx.Repo // this repository, for config files and its default branch
	fetched bool       // source holds $GITHUB_SHA
}

func (a *ghAction) run(ctx context.Context) error {
	cmd := strings.TrimSpace(a.getenv("VIBECI_ACTION_COMMAND"))
	if cmd == "" {
		cmd = "run"
	}
	takesJSON, ok := actionCommands[cmd]
	if !ok {
		return a.fail(fmt.Errorf("command %q is not one the action runs (run, review, status, allow, exclude, release, check, patches, config)", cmd), 2)
	}
	args, err := splitArgs(a.getenv("VIBECI_ACTION_ARGS"))
	if err != nil {
		return a.fail(fmt.Errorf("args: %w", err), 2)
	}
	temp := a.getenv("RUNNER_TEMP")
	if temp == "" {
		return a.fail(errors.New("RUNNER_TEMP is not set: vibeci action runs as a step of a GitHub Actions job (see action.yml)"), 2)
	}
	a.work = filepath.Join(temp, "vibeci")
	if err := os.MkdirAll(a.work, 0o700); err != nil {
		return a.fail(err, 1)
	}
	models := a.getenv("VIBECI_ACTION_MODELS")
	for _, line := range strings.Split(a.getenv("VIBECI_ACTION_MASK"), "\n") {
		a.mask(strings.TrimSpace(line))
	}
	var overlay map[string]any
	if strings.TrimSpace(models) != "" {
		if overlay, err = config.Decode("the models input", []byte(models)); err != nil {
			return a.fail(err, 1)
		}
		a.maskModels(overlay)
	}

	path, cfg, err := a.compose(ctx, overlay)
	if err != nil {
		return a.fail(err, 1)
	}
	full := []string{cmd, "-config", path}
	if takesJSON {
		full = append(full, "-json")
	}
	full = append(full, args...)
	fmt.Fprintf(a.stderr, "vibeci action: vibeci %s\n", strings.Join(full, " "))
	stdout, errTail, code, err := a.exec(ctx, full)
	if err != nil {
		return a.fail(err, 1)
	}
	res := a.report(cmd, cfg, stdout, errTail, code)
	if werr := a.outputs(res, stdout, cfg); werr != nil {
		fmt.Fprintf(a.stderr, "vibeci action: writing the step outputs: %v\n", werr)
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// fail reports an error of the action itself.
func (a *ghAction) fail(err error, code int) error {
	msg := a.redact(err.Error())
	a.command("error", map[string]string{"title": "VibeCI"}, msg)
	fmt.Fprintln(a.stderr, "vibeci action:", msg)
	a.summary("### VibeCI\n\n" + mdText(msg) + "\n")
	return exitCode(code)
}

// --- config ---

// actionDefaults is the config a runner starts from.
func actionDefaults(work string) map[string]any {
	goEnv := map[string]any{"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild", "GOPROXY": "off", "GOFLAGS": "-mod=readonly"}
	goEgressEnv := map[string]any{"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild"}
	return map[string]any{
		"data_dir":  filepath.Join(work, "data"),
		"state_ref": "refs/vibeci/state",
		"sandbox": map[string]any{"mode": "docker", "docker": map[string]any{"profiles": map[string]any{
			"default":        map[string]any{"image": "vibeci-sandbox:base"},
			"default-egress": map[string]any{"image": "vibeci-sandbox:base", "network": "vibeci-egress"},
			"go":             map[string]any{"image": "vibeci-sandbox:go", "env": goEnv},
			"go-egress":      map[string]any{"image": "vibeci-sandbox:go", "network": "vibeci-egress", "env": goEgressEnv},
		}}},
	}
}

// compose merges the defaults, the config files and the models overlay,
// fills in this repository as the fork, and writes the result for the
// command (JSON, 0600: it can hold API keys).
func (a *ghAction) compose(ctx context.Context, overlay map[string]any) (string, *config.Config, error) {
	var merged any = actionDefaults(a.work)
	list := a.getenv("VIBECI_ACTION_CONFIG")
	if strings.TrimSpace(list) == "" {
		list = ".github/vibeci.jsonc"
	}
	for _, p := range strings.Split(list, "\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		b, name, err := a.readConfig(ctx, p)
		if err != nil {
			return "", nil, err
		}
		doc, err := config.Decode(name, b)
		if err != nil {
			return "", nil, err
		}
		merged = config.Merge(merged, doc)
	}
	if overlay != nil {
		merged = config.Merge(merged, overlay)
	}
	if err := a.forkDefaults(ctx, merged.(map[string]any)); err != nil {
		return "", nil, err
	}
	raw, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", nil, err
	}
	cfg, err := config.Parse(raw, "the composed config")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(a.work, "config.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", nil, err
	}
	return path, cfg, os.Chmod(path, 0o600)
}

// readConfig reads a config file from the workspace or, if it is not
// checked out, from this repository at $GITHUB_SHA.
func (a *ghAction) readConfig(ctx context.Context, p string) ([]byte, string, error) {
	if filepath.IsAbs(p) {
		b, err := os.ReadFile(p)
		return b, p, err
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, "", fmt.Errorf("config path %q leaves the repository", p)
	}
	if ws := a.getenv("GITHUB_WORKSPACE"); ws != "" {
		b, err := os.ReadFile(filepath.Join(ws, clean))
		if err == nil {
			return b, clean, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	sha := a.getenv("GITHUB_SHA")
	if sha == "" || !gitx.IsHex(sha) {
		return nil, "", fmt.Errorf("config file %s: not in the workspace, and GITHUB_SHA is not set", clean)
	}
	src, auth, url, err := a.sourceRepo(ctx)
	if err != nil {
		return nil, "", err
	}
	if !a.fetched {
		if err := src.FetchShallow(ctx, url, auth, true, sha); err != nil {
			return nil, "", err
		}
		a.fetched = true
	}
	ents, err := src.LsTreePaths(ctx, sha, []string{clean})
	if err != nil {
		return nil, "", err
	}
	e, ok := ents[clean]
	if !ok || e.Type != "blob" {
		return nil, "", fmt.Errorf("config file %s: not in the workspace, nor in %s at %s", clean, a.getenv("GITHUB_REPOSITORY"), short(sha))
	}
	if err := src.FetchObjects(ctx, url, auth, []string{e.OID}, 2*time.Minute); err != nil {
		return nil, "", err
	}
	blobs, err := src.ReadBlobs(ctx, []string{e.OID})
	if err != nil {
		return nil, "", err
	}
	return blobs[e.OID], clean + "@" + short(sha), nil
}

// thisRepo returns the URL and credentials of the repository the workflow
// runs in.
func (a *ghAction) thisRepo() (string, *gitx.Auth, error) {
	repo, server := a.getenv("GITHUB_REPOSITORY"), a.getenv("GITHUB_SERVER_URL")
	if repo == "" || server == "" {
		return "", nil, errors.New("GITHUB_REPOSITORY or GITHUB_SERVER_URL is not set")
	}
	var auth *gitx.Auth
	if tok := a.getenv("VIBECI_TOKEN"); tok != "" {
		auth = &gitx.Auth{Token: tok}
	}
	return strings.TrimRight(server, "/") + "/" + repo + ".git", auth, nil
}

// sourceRepo returns a partial (blob-less) repository whose origin is
// this repository.
func (a *ghAction) sourceRepo(ctx context.Context) (*gitx.Repo, *gitx.Auth, string, error) {
	url, auth, err := a.thisRepo()
	if err != nil {
		return nil, nil, "", err
	}
	if a.source != nil {
		return a.source, auth, url, nil
	}
	g, err := gitx.New(filepath.Join(a.work, "home"), gitx.Identity{Name: "VibeCI", Email: "vibeci@localhost"}, nil)
	if err != nil {
		return nil, nil, "", err
	}
	src, err := g.InitSource(ctx, filepath.Join(a.work, "source.git"), url, true)
	if err != nil {
		return nil, nil, "", err
	}
	a.source = src
	return src, auth, url, nil
}

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// forkDefaults makes this repository the fork of the one repo without
// fork.url: at its default branch, pushed with VIBECI_TOKEN.
func (a *ghAction) forkDefaults(ctx context.Context, merged map[string]any) error {
	repos, _ := merged["repos"].([]any)
	var target map[string]any
	for i, r := range repos {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		fork, _ := m["fork"].(map[string]any)
		if u, _ := fork["url"].(string); u != "" {
			continue
		}
		if target != nil {
			return fmt.Errorf("repos[%d] has no fork.url either: only one repo can default to this repository", i)
		}
		if fork == nil {
			fork = map[string]any{}
			m["fork"] = fork
		}
		target = m
	}
	if target == nil {
		return nil
	}
	url, auth, err := a.thisRepo()
	if err != nil {
		return fmt.Errorf("a repo has no fork.url and this repository is unknown: %w", err)
	}
	fork := target["fork"].(map[string]any)
	fork["url"] = url
	if fork["auth"] == nil && auth != nil {
		fork["auth"] = map[string]any{"token": "env:VIBECI_TOKEN"}
	}
	if name, _ := target["name"].(string); name == "" {
		_, short, _ := strings.Cut(a.getenv("GITHUB_REPOSITORY"), "/")
		target["name"] = strings.Trim(nameUnsafe.ReplaceAllString(short, "-"), "-_")
	}
	if b, _ := fork["branch"].(string); b == "" {
		src, _, _, err := a.sourceRepo(ctx)
		if err != nil {
			return err
		}
		head, err := src.RemoteHead(ctx, url, auth)
		if err != nil {
			return err
		}
		if head == "" {
			return fmt.Errorf("%s does not name a default branch; set fork.branch", url)
		}
		fork["branch"] = head
	}
	return nil
}

// --- running the command ---

// exec runs the vibeci command and returns its stdout, the tail of its
// stderr (which also goes to the log) and its exit status.
func (a *ghAction) exec(ctx context.Context, args []string) ([]byte, string, int, error) {
	c := exec.CommandContext(ctx, a.exe, args...)
	for _, kv := range os.Environ() {
		// The models input is in the config file now; the command's own
		// default config must not interfere.
		if !strings.HasPrefix(kv, "VIBECI_ACTION_MODELS=") && !strings.HasPrefix(kv, "VIBECI_CONFIG=") {
			c.Env = append(c.Env, kv)
		}
	}
	var out bytes.Buffer
	tail := &tailBuffer{max: 8 << 10}
	c.Stdout = &out
	c.Stderr = io.MultiWriter(a.stderr, tail)
	c.WaitDelay = 10 * time.Second
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), tail.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return nil, "", 0, err
	}
	return out.Bytes(), tail.String(), 0, nil
}

type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// --- results ---

// actionResult is what the step reports.
type actionResult struct {
	status string
	commit string
}

// statusRank orders run statuses by how much attention they need.
var statusRank = map[string]int{
	pipeline.StatusDisabled: 0, pipeline.StatusUpToDate: 1, pipeline.StatusSynced: 2, pipeline.StatusDryRun: 3,
	pipeline.StatusLocked: 4, pipeline.StatusRaced: 5, pipeline.StatusBackoff: 6, pipeline.StatusHeld: 7,
	pipeline.StatusBlocked: 8, pipeline.StatusFailed: 9, pipeline.StatusError: 10,
}

// report writes the job summary and annotations.
func (a *ghAction) report(cmd string, cfg *config.Config, stdout []byte, errTail string, code int) actionResult {
	res := actionResult{status: "ok"}
	switch code {
	case 0:
	case 3:
		res.status = "attention"
	default:
		res.status = "error"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "### VibeCI %s\n\n", cmd)
	var outs []*pipeline.Outcome
	if cmd == "run" && json.Unmarshal(stdout, &outs) == nil && len(outs) > 0 {
		res.status = ""
		sb.WriteString("| Repo | Status | Result |\n|---|---|---|\n")
		for _, o := range outs {
			if res.status == "" || statusRank[o.Status] > statusRank[res.status] {
				res.status = o.Status
			}
			if res.commit == "" {
				res.commit = o.Commit
			}
			fmt.Fprintf(&sb, "| %s | %s | %s |\n", mdCell(o.Repo), mdCell(o.Status), mdCell(a.redact(o.Message)))
		}
		sb.WriteString("\n")
		for _, o := range outs {
			fmt.Fprintf(&sb, "- **%s**: %s\n", mdText(o.Repo), mdText(a.redact(outcomeDetails(o))))
			a.annotate(o)
		}
		if cfg != nil {
			fmt.Fprintf(&sb, "\nJob records (transcripts, kept on the runner only): `%s`\n", filepath.Join(cfg.DataDir, "jobs"))
		}
	} else {
		body := strings.TrimSpace(a.redact(string(stdout)))
		if len(body) > 60000 {
			body = body[:60000] + "\n... (truncated; see the json output)"
		}
		if body != "" {
			fence := "```"
			for strings.Contains(body, fence) {
				fence += "`"
			}
			fmt.Fprintf(&sb, "%sjson\n%s\n%s\n", fence, body, fence)
		}
	}
	if code != 0 {
		msg := lastError(errTail)
		if msg == "" {
			msg = fmt.Sprintf("vibeci %s exited with status %d", cmd, code)
		}
		msg = a.redact(msg)
		if cmd != "run" || len(outs) == 0 {
			kind := "error"
			if code == 3 {
				kind = "warning"
			}
			a.command(kind, map[string]string{"title": "VibeCI " + cmd}, msg)
		}
		fmt.Fprintf(&sb, "\n**Exit status %d**: %s\n", code, mdText(msg))
	}
	a.summary(sb.String())
	return res
}

func outcomeDetails(o *pipeline.Outcome) string {
	var parts []string
	if o.Commit != "" {
		parts = append(parts, "commit "+o.Commit)
	}
	if o.Merges > 0 {
		parts = append(parts, fmt.Sprintf("%d merge(s)", o.Merges))
	}
	if o.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d upstream commit(s) pending", o.Pending))
	}
	if len(o.Blocked) > 0 {
		parts = append(parts, "blocked: "+strings.Join(shorts(o.Blocked), ", "))
	}
	if len(o.Excluded) > 0 {
		parts = append(parts, "excluded: "+strings.Join(shorts(o.Excluded), ", "))
	}
	if len(o.Restored) > 0 {
		parts = append(parts, "restored: "+strings.Join(shorts(o.Restored), ", "))
	}
	if o.JobID != "" {
		parts = append(parts, "job "+o.JobID)
	}
	if o.Usage.Calls > 0 {
		parts = append(parts, fmt.Sprintf("%d LLM calls, %s in / %s out tokens", o.Usage.Calls, kilo(o.Usage.InputTokens), kilo(o.Usage.OutputTokens)))
	}
	if o.StateError != "" {
		parts = append(parts, "state not saved to the fork: "+o.StateError)
	}
	return strings.Join(append(parts, "took "+o.Duration), "; ")
}

// annotate adds an annotation for an outcome that needs a look.
func (a *ghAction) annotate(o *pipeline.Outcome) {
	props := map[string]string{"title": "VibeCI: " + o.Repo + " " + o.Status}
	msg := a.redact(o.Message)
	switch o.Status {
	case pipeline.StatusFailed, pipeline.StatusError:
		a.command("error", props, msg)
	case pipeline.StatusBlocked, pipeline.StatusHeld, pipeline.StatusBackoff, pipeline.StatusRaced, pipeline.StatusLocked:
		a.command("warning", props, msg)
	case pipeline.StatusSynced:
		a.command("notice", props, msg)
	}
	if o.StateError != "" {
		a.command("error", map[string]string{"title": "VibeCI: " + o.Repo + " state not saved"}, a.redact(o.StateError))
	}
}

// lastError returns the last "vibeci: ..." line of a command's stderr.
func lastError(tail string) string {
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if msg, ok := strings.CutPrefix(lines[i], "vibeci: "); ok {
			return msg
		}
	}
	return ""
}

// outputs writes the step outputs.
func (a *ghAction) outputs(res actionResult, stdout []byte, cfg *config.Config) error {
	p := a.getenv("GITHUB_OUTPUT")
	if p == "" {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var sb strings.Builder
	fmt.Fprintf(&sb, "status=%s\ncommit=%s\n", res.status, res.commit)
	if cfg != nil {
		fmt.Fprintf(&sb, "records=%s\n", filepath.Join(cfg.DataDir, "jobs"))
	}
	body := strings.TrimSpace(a.redact(string(stdout)))
	delim := "vibeci_" + randHex(12)
	for strings.Contains(body, delim) {
		delim = "vibeci_" + randHex(12)
	}
	fmt.Fprintf(&sb, "json<<%s\n%s\n%s\n", delim, body, delim)
	_, err = f.WriteString(sb.String())
	return err
}

func (a *ghAction) summary(md string) {
	p := a.getenv("GITHUB_STEP_SUMMARY")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(a.stderr, "vibeci action: writing the job summary: %v\n", err)
		return
	}
	defer f.Close()
	f.WriteString(a.redact(md) + "\n")
}

// --- masking and workflow commands ---

// mask hides s in the log (and in what the action writes itself).
func (a *ghAction) mask(s string) {
	if len(s) < 4 || strings.ContainsAny(s, "\r\n") {
		return
	}
	for _, m := range a.masks {
		if m == s {
			return
		}
	}
	a.masks = append(a.masks, s)
	sort.Slice(a.masks, func(i, j int) bool { return len(a.masks[i]) > len(a.masks[j]) })
	fmt.Fprintf(a.stdout, "::add-mask::%s\n", escapeData(s))
}

// maskModels masks what the models input says about the providers: base
// URLs and their hosts, header values, inline API keys, model ids.
func (a *ghAction) maskModels(overlay map[string]any) {
	providers, _ := overlay["providers"].(map[string]any)
	for _, p := range providers {
		pm, _ := p.(map[string]any)
		if u, _ := pm["base_url"].(string); u != "" {
			a.mask(strings.TrimRight(u, "/"))
			if pu, err := url.Parse(u); err == nil && pu.Host != "" {
				a.mask(pu.Host)
				a.mask(pu.Hostname())
				a.mask(strings.TrimRight(pu.Path, "/"))
			}
		}
		headers, _ := pm["headers"].(map[string]any)
		for _, v := range headers {
			if s, ok := v.(string); ok {
				a.mask(s)
			}
		}
		switch k := pm["api_key"].(type) {
		case string:
			if !strings.HasPrefix(k, "env:") && !strings.HasPrefix(k, "file:") {
				a.mask(k)
			}
		case map[string]any:
			if s, ok := k["value"].(string); ok {
				a.mask(s)
			}
		}
	}
	models, _ := overlay["models"].(map[string]any)
	for _, m := range models {
		mm, _ := m.(map[string]any)
		if id, _ := mm["model"].(string); id != "" {
			a.mask(id)
		}
	}
}

func (a *ghAction) redact(s string) string {
	for _, m := range a.masks {
		s = strings.ReplaceAll(s, m, "***")
	}
	return s
}

// command prints a workflow command (::error, ::warning, ::notice).
func (a *ghAction) command(name string, props map[string]string, msg string) {
	var keys []string
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ps []string
	for _, k := range keys {
		ps = append(ps, k+"="+escapeProperty(a.redact(props[k])))
	}
	sep := ""
	if len(ps) > 0 {
		sep = " "
	}
	fmt.Fprintf(a.stdout, "::%s%s%s::%s\n", name, sep, strings.Join(ps, ","), escapeData(a.redact(msg)))
}

func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func escapeProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

// mdCell makes s safe inside a markdown table cell.
func mdCell(s string) string {
	return strings.ReplaceAll(mdText(strings.ReplaceAll(s, "\n", " ")), "|", `\|`)
}

// mdText keeps s from being read as HTML.
func mdText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// splitArgs splits s into words like a POSIX shell, without expansions:
// whitespace separates words; 'single' and "double" quotes group (\" and
// \\ escape inside double quotes); a backslash escapes the next character.
func splitArgs(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	word := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, word = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			word = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if word {
				out = append(out, cur.String())
				cur.Reset()
				word = false
			}
		default:
			cur.WriteByte(c)
			word = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if word {
		out = append(out, cur.String())
	}
	return out, nil
}
