package resolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/agent"
	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/patch"
	"github.com/vibeci/vibeci/internal/review"
	"github.com/vibeci/vibeci/internal/sandbox"
)

// patchSession is one failed patch the agent works on.
type patchSession struct {
	pf      *patchFile
	before  *patch.Tree // at To with the earlier patches applied
	partial *patch.Tree // before plus the hunks that applied
	res     *patch.Result
	tracked []string // the paths the patch touches

	// Context at From (nil when the old version is unavailable).
	oldTree         *patch.Tree
	oldPre, oldPost map[string]patch.FileState
	oldApplied      bool

	ws            *workspace
	opened        map[string]bool
	rounds        int
	warnedDropped bool
	result        *agentPatchResult
	tw            io.Writer
}

type agentPatchResult struct {
	data    []byte                     // the rewritten patch; nil drops it
	states  map[string]patch.FileState // final content of every path involved
	summary string
	model   string
	turns   int
	novel   int
	verdict *review.Verdict
}

// agentPatch has the agent rewrite one patch, escalating through the
// resolve models.
func (j *PatchJob) agentPatch(ctx context.Context, pf *patchFile, before, partial *patch.Tree, res *patch.Result) (*agentPatchResult, error) {
	what := "could not update patch " + pf.path + " for upstream " + j.To
	if len(j.Models) == 0 {
		return nil, &FailedError{What: what, Attempts: []string{"no resolve models configured"}}
	}
	sess := &patchSession{pf: pf, before: before, partial: partial, res: res, tracked: res.Touched()}
	j.oldContext(ctx, sess)
	var attempts []string
	for i, m := range j.Models {
		r, err := j.runPatchAgent(ctx, sess, i, m)
		if err == nil {
			return r, nil
		}
		var ie *infraError
		if errors.Is(err, ErrAuditRejected) || errors.As(err, &ie) || ctx.Err() != nil {
			return nil, err
		}
		j.logger().Warn("patch agent failed", "repo", j.Repo.Name, "patch", pf.path, "model", m.Name(), "err", err)
		attempts = append(attempts, fmt.Sprintf("%s: %v", m.Name(), err))
	}
	return nil, &FailedError{What: what, Attempts: attempts}
}

// oldContext computes the patch's files at From, before and after it
// (best effort: the agent can work without them).
func (j *PatchJob) oldContext(ctx context.Context, sess *patchSession) {
	old, err := j.old(ctx)
	if err != nil {
		return
	}
	k := sess.pf.index
	if j.oldTree == nil {
		j.oldTree, j.oldAt = patch.NewTree(old.Bind(ctx)), 0
	}
	var paths []string
	for i := j.oldAt; i <= k; i++ {
		ps, _ := patch.TouchedPaths(j.patches[i].parsed, j.patches[i].strip, j.patches[i].root)
		paths = append(paths, ps...)
	}
	if err := old.Prefetch(ctx, paths); err != nil {
		j.logger().Warn("loading the old version's files", "repo", j.Repo.Name, "err", err)
		return
	}
	loose := func(pf *patchFile) patch.Options {
		return patch.Options{Strip: pf.strip, Root: pf.root, Fuzz: 2, IgnoreWhitespace: true, KeepBinary: func(string) bool { return true }}
	}
	for j.oldAt < k {
		if _, err := patch.Apply(j.patches[j.oldAt].parsed, j.oldTree, loose(j.patches[j.oldAt])); err != nil {
			j.logger().Warn("re-applying the series at the old version", "repo", j.Repo.Name, "patch", j.patches[j.oldAt].path, "err", err)
			j.oldTree = nil
			j.oldErr = err
			return
		}
		j.oldAt++
	}
	pre := j.oldTree.Clone()
	post := pre.Clone()
	r, err := patch.Apply(sess.pf.parsed, post, loose(sess.pf))
	if err != nil {
		return
	}
	sess.oldTree, sess.oldApplied = pre, r.FailedHunks() == 0
	sess.oldPre, sess.oldPost = map[string]patch.FileState{}, map[string]patch.FileState{}
	for _, p := range sess.tracked {
		sess.oldPre[p], _ = pre.Get(p)
		sess.oldPost[p], _ = post.Get(p)
	}
}

func (j *PatchJob) runPatchAgent(ctx context.Context, sess *patchSession, attempt int, model llm.Client) (*agentPatchResult, error) {
	ws, err := j.patchWorkspace(ctx, sess, attempt)
	if err != nil {
		return nil, &infraError{err}
	}
	sess.ws, sess.opened, sess.rounds, sess.warnedDropped, sess.result = ws, map[string]bool{}, 0, false, nil
	sb, err := j.base.newSandbox(ctx, j.Repo.Sandbox.Profile, ws, j.base.path("cache-agent"))
	if err != nil {
		return nil, &infraError{err}
	}
	defer closeSandbox(ctx, sb)
	tw := j.base.transcript(fmt.Sprintf("patch-%d-%d-%s", sess.pf.index+1, attempt, sanitize(model.Name())))
	defer tw.Close()
	sess.tw = tw
	out, err := agent.Run(ctx, agent.Config{
		Name:          "patch",
		Client:        model,
		System:        patchSystem,
		Tools:         j.patchTools(sess, sb),
		MaxTurns:      j.Repo.Resolve.MaxTurns,
		Timeout:       j.Repo.Resolve.Timeout.Duration,
		MaxToolOutput: 30000,
		Transcript:    tw,
		Logger:        j.logger(),
	}, []llm.Block{llm.Text(j.patchBrief(sess, sb))})
	if out != nil {
		j.usage.Add(out.Usage)
	}
	if err != nil {
		return nil, err
	}
	if out.Terminal == "give_up" {
		var a struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(out.Input, &a)
		return nil, fmt.Errorf("agent gave up: %s", clip(a.Reason, 1000))
	}
	if sess.result == nil {
		return nil, errors.New("agent finished without an accepted submission")
	}
	sess.result.model, sess.result.turns = model.Name(), out.Turns
	return sess.result, nil
}

// patchWorkspace creates the agent's sparse repository: refs old,
// old-patched and new hold the patch's files; the working tree is new with
// the hunks that applied.
func (j *PatchJob) patchWorkspace(ctx context.Context, sess *patchSession, attempt int) (*workspace, error) {
	dir := j.base.path(fmt.Sprintf("patch%d-%d", sess.pf.index+1, attempt))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: dir}, "init", "--quiet"); err != nil {
		return nil, err
	}
	snapshot := func(get func(string) (patch.FileState, error)) (map[string][]byte, error) {
		files := map[string][]byte{}
		for _, p := range sess.tracked {
			st, err := get(p)
			if err != nil {
				return nil, err
			}
			if st.Exists {
				files[p] = st.Data
			}
		}
		return files, nil
	}
	newFiles, err := snapshot(sess.before.Get)
	if err != nil {
		return nil, err
	}
	var commits []sparseCommit
	parent := ""
	if sess.oldPre != nil {
		pick := func(m map[string]patch.FileState) func(string) (patch.FileState, error) {
			return func(p string) (patch.FileState, error) { return m[p], nil }
		}
		oldFiles, _ := snapshot(pick(sess.oldPre))
		postFiles, _ := snapshot(pick(sess.oldPost))
		commits = append(commits,
			sparseCommit{ref: "refs/heads/old", msg: fmt.Sprintf("upstream %s with the earlier patches", j.From), files: oldFiles},
			sparseCommit{ref: "refs/heads/old-patched", msg: fmt.Sprintf("%s applied to upstream %s", sess.pf.name, j.From), files: postFiles, from: "refs/heads/old"})
		parent = "refs/heads/old"
	}
	commits = append(commits, sparseCommit{ref: "refs/heads/new", msg: fmt.Sprintf("upstream %s with the earlier patches", j.To), files: newFiles, from: parent})
	ident := fmt.Sprintf("%s <%s>", j.G.Identity.Name, j.G.Identity.Email)
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: dir, Stdin: fastImportStream(commits, ident, time.Now().Unix())}, "fast-import", "--quiet", "--done"); err != nil {
		return nil, err
	}
	if _, err := j.G.Run(ctx, gitx.Opts{Dir: dir}, "checkout", "--quiet", "--force", "-B", "work", "new", "--"); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, p := range sess.tracked {
		st, err := sess.partial.Get(p)
		if err != nil {
			return nil, err
		}
		if !st.Exists {
			if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if err := writeRootFile(root, p, st.Data); err != nil {
			return nil, err
		}
	}
	if err := writeRootFile(root, ".vibeci/patch.diff", sess.pf.data); err != nil {
		return nil, err
	}
	if err := writeRootFile(root, ".vibeci/rejects.diff", []byte(rejectsText(sess.res))); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(".git/info/exclude", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	f.WriteString("/.vibeci/\n")
	f.Close()
	return &workspace{Dir: dir, Rel: j.base.relPath(dir)}, nil
}

func writeRootFile(root *os.Root, p string, data []byte) error {
	if d := path.Dir(p); d != "." {
		if err := root.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return root.WriteFile(p, data, 0o644)
}

type sparseCommit struct {
	ref   string
	msg   string
	files map[string][]byte
	from  string // the parent's ref ("" for a root commit)
}

// fastImportStream renders commits (full snapshots) for git fast-import.
func fastImportStream(commits []sparseCommit, ident string, when int64) []byte {
	var b bytes.Buffer
	mark := 0
	marks := map[string]int{}
	for _, c := range commits {
		paths := make([]string, 0, len(c.files))
		for p := range c.files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		blobs := map[string]int{}
		for _, p := range paths {
			mark++
			blobs[p] = mark
			fmt.Fprintf(&b, "blob\nmark :%d\ndata %d\n", mark, len(c.files[p]))
			b.Write(c.files[p])
			b.WriteByte('\n')
		}
		mark++
		marks[c.ref] = mark
		fmt.Fprintf(&b, "commit %s\nmark :%d\ncommitter %s %d +0000\ndata %d\n%s\n", c.ref, mark, ident, when, len(c.msg), c.msg)
		if c.from != "" {
			fmt.Fprintf(&b, "from :%d\n", marks[c.from])
		}
		b.WriteString("deleteall\n")
		for _, p := range paths {
			fmt.Fprintf(&b, "M 100644 :%d %s\n", blobs[p], fastImportQuote(p))
		}
		b.WriteByte('\n')
	}
	b.WriteString("done\n")
	return b.Bytes()
}

// fastImportQuote C-quotes a path the way git unquotes it.
func fastImportQuote(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// rejectsText lists the hunks that failed, like a .rej file with hints.
func rejectsText(res *patch.Result) string {
	var b strings.Builder
	for _, fr := range res.Files {
		if fr.Problem != "" {
			fmt.Fprintf(&b, "=== %s: %s ===\n\n", fr.Path, fr.Problem)
			continue
		}
		for i, hr := range fr.Hunks {
			if hr.Status != patch.Failed {
				continue
			}
			fmt.Fprintf(&b, "=== %s: hunk %d of %d failed: %s ===\n", fr.Path, i+1, len(fr.Hunks), nearText(hr))
			b.WriteString(fr.File.Hunks[i].Text())
			b.WriteString("\n")
		}
	}
	return b.String()
}

func nearText(hr patch.HunkResult) string {
	s := fmt.Sprintf("expected at line %d of ref new", hr.Line)
	if n := hr.Near; n != nil && n.Total > 0 {
		s += fmt.Sprintf("; closest match at line %d (%d of %d old lines match)", n.Line, n.Matched, n.Total)
		if n.RemovedMissing > 0 {
			s += fmt.Sprintf("; %d line(s) it removes appear nowhere in the file", n.RemovedMissing)
		}
	}
	return s
}

func (j *PatchJob) patchBrief(sess *patchSession, sb sandbox.Sandbox) string {
	pf := sess.pf
	var b strings.Builder
	fmt.Fprintf(&b, "Fork: %s (%s, branch %s), a series of patches applied to the upstream tree of %s.\n", j.Repo.Name, j.Repo.Fork.URL, j.Repo.Fork.Branch, j.Repo.Upstream.URL)
	if d := strings.TrimSpace(j.Repo.Description); d != "" {
		fmt.Fprintf(&b, "What the fork does, according to its maintainer:\n%s\n", d)
	}
	fmt.Fprintf(&b, "\nTask: update patch %d of %d, %s (file %s in the fork), so that it applies to upstream %s. It was written for upstream %s.\n",
		pf.index+1, len(j.patches), pf.name, pf.path, j.To, j.From)
	if pf.root != "" {
		fmt.Fprintf(&b, "Its paths are relative to %s/ in the upstream tree; it can only change files there. In the workspace, files are at their full upstream paths.\n", pf.root)
	}
	n := hunkCount(pf.parsed)
	fmt.Fprintf(&b, "%d of its %d hunks applied; these did not:\n", n-sess.res.FailedHunks(), n)
	for _, fr := range sess.res.Files {
		if fr.Problem != "" {
			fmt.Fprintf(&b, "  - %s: %s\n", fr.Path, fr.Problem)
		}
		for i, hr := range fr.Hunks {
			if hr.Status == patch.Failed {
				fmt.Fprintf(&b, "  - %s, hunk %d: %s\n", fr.Path, i+1, nearText(hr))
			}
		}
	}
	b.WriteString("\nWorkspace (/workspace): a sparse git repository with only the files this patch touches.\n")
	if sess.oldPre != nil {
		fmt.Fprintf(&b, "  ref old:         these files at upstream %s with the earlier patches of the series applied\n", j.From)
		b.WriteString("  ref old-patched: old with this patch applied (the patch's intent: git diff old old-patched)\n")
		if !sess.oldApplied {
			fmt.Fprintf(&b, "                   (even at %s this patch did not apply completely, so old-patched may lack some of its changes)\n", j.From)
		}
	} else {
		fmt.Fprintf(&b, "  (upstream %s is not available, so there are no refs old and old-patched; .vibeci/patch.diff shows the patch's intent)\n", j.From)
	}
	fmt.Fprintf(&b, "  ref new:         these files at upstream %s with the earlier patches applied (as updated in this run)\n", j.To)
	b.WriteString("  working tree:    new plus the hunks that applied (git diff shows them)\n")
	b.WriteString("  .vibeci/patch.diff: the patch file; .vibeci/rejects.diff: the hunks that failed\n")
	if desc := strings.TrimSpace(pf.parsed.Preamble); desc != "" {
		fmt.Fprintf(&b, "\nThe patch's own description:\n%s\n", indentBlock(clip(desc, 3000)))
	}
	counts := map[string]int{}
	for _, o := range j.outcomes {
		if o.Path != pf.path {
			counts[o.Status]++
		}
	}
	if pf.index > 0 {
		fmt.Fprintf(&b, "\nEarlier patches in this run: %d unchanged, %d with new line numbers, %d refreshed, %d rewritten by the agent, %d dropped.\n",
			counts[PatchExact], counts[PatchShifted], counts[PatchRefreshed], counts[PatchAgent], counts[PatchDropped])
	}
	info := sb.Info()
	fmt.Fprintf(&b, "\nSandbox: image %s, network: %s. Only the files above exist, so nothing can be built or tested here.\n", info.Image, info.Network)
	b.WriteString("\nStart with git diff old old-patched, git diff old new and .vibeci/rejects.diff. Edit the working tree, then submit.")
	return b.String()
}

func (j *PatchJob) patchTools(sess *patchSession, sb sandbox.Sandbox) []*agent.Tool {
	ft := &fileTools{dir: sess.ws.Dir}
	return []*agent.Tool{
		shellTool(sb, j.Repo.Resolve.CommandTimeout.Duration),
		{Name: "read_file", Description: "Read a file in /workspace with line numbers (or list a directory). Use start_line/end_line for large files.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string","description":"relative to /workspace"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
			Run:    ft.read},
		{Name: "write_file", Description: "Create or overwrite a file in /workspace with the given content.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run:    ft.write},
		{Name: "edit_file", Description: "Replace an exact string in a file in /workspace. old_string must match exactly once unless replace_all is set. Prefer this over write_file for large files.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"]}`),
			Run:    ft.edit},
		{Name: "open_file", Description: "Copy a file of the upstream tree (the new version, with the earlier patches applied) into the workspace so you can edit it, when this patch's change now belongs in a file it does not touch yet. List it in submit's files if you change it.",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string","description":"path in the upstream tree"}},"required":["path"]}`),
			Run:    func(ctx context.Context, in json.RawMessage) (agent.Result, error) { return j.openFile(ctx, sess, in) }},
		{Name: "read_upstream", Description: "Read any file of the upstream tree without adding it to the workspace. version: \"new\" (default: the version the patch must apply to, with the earlier patches applied) or \"old\" (the version the patch was written for).",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"},"version":{"type":"string","enum":["new","old"]},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return j.readUpstream(ctx, sess, in)
			}},
		{Name: "find_upstream", Description: "Find files of the upstream tree (new version) by path: a glob such as **/foo_*.cc (a pattern without a slash matches file names at any depth) or a plain substring of the path. At most 200 results.",
			Schema: js(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
			Run:    func(ctx context.Context, in json.RawMessage) (agent.Result, error) { return j.findUpstream(ctx, in) }},
		{Name: "grep_upstream", Description: "Search the contents of the files under a directory of the upstream tree (new version, with the earlier patches applied) for a regular expression (RE2 syntax). The directory may hold at most 3000 files.",
			Schema: js(`{"type":"object","properties":{"pattern":{"type":"string"},"dir":{"type":"string","description":"directory in the upstream tree"}},"required":["pattern","dir"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return j.grepUpstream(ctx, sess, in)
			}},
		{Name: "upstream_diff", Description: "Show how upstream changed a file between the old and the new version (without any patches).",
			Schema: js(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Run:    func(ctx context.Context, in json.RawMessage) (agent.Result, error) { return j.upstreamDiff(ctx, in) }},
		{Name: "submit", Terminal: true,
			Description: "Submit the updated files. The harness turns the working tree into the new version of the patch, checks that it applies exactly and audits the lines you wrote; problems are reported back to you.",
			Schema:      js(`{"type":"object","properties":{"summary":{"type":"string","description":"how you adapted each failed hunk, and anything noteworthy"},"files":{"type":"array","items":{"type":"string"},"description":"files you changed or created that the patch did not touch before; they become part of the patch"},"drop_patch":{"type":"boolean","description":"upstream now does everything this patch did: remove the patch from the series (leave the working tree equal to ref new)"},"confirm_dropped_changes":{"type":"boolean","description":"set after a warning, if leaving out those lines of the original patch is intentional"}},"required":["summary"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return j.patchSubmit(ctx, sess, in)
			}},
		{Name: "give_up", Terminal: true, Description: "Abandon the patch when it cannot be adapted correctly. Explain precisely why.",
			Schema: js(`{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				return agent.OK("acknowledged"), nil
			}},
	}
}

func inRoot(pf *patchFile, p string) error {
	if pf.root != "" && !strings.HasPrefix(p, pf.root+"/") {
		return fmt.Errorf("%s is outside %s/: this patch's paths are relative to %s/, so it can only change files there", p, pf.root, pf.root)
	}
	return nil
}

func (j *PatchJob) openFile(ctx context.Context, sess *patchSession, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	if err := inRoot(sess.pf, p); err != nil {
		return agent.Errorf("%v", err), nil
	}
	if slices.Contains(sess.tracked, p) || sess.opened[p] {
		return agent.Errorf("%s is already in the workspace", p), nil
	}
	st, err := sess.before.Get(p)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	if !st.Exists {
		return agent.Errorf("%s does not exist in the upstream tree. To add a file, create it with write_file and list it in submit's files.", p), nil
	}
	root, err := os.OpenRoot(sess.ws.Dir)
	if err != nil {
		return agent.Result{}, err
	}
	defer root.Close()
	if err := writeRootFile(root, p, st.Data); err != nil {
		return agent.Errorf("%v", trimRootErr(err)), nil
	}
	sess.opened[p] = true
	return agent.OK(fmt.Sprintf("opened %s (%d lines); if you change it, list it in submit's files", p, bytes.Count(st.Data, []byte("\n")))), nil
}

func (j *PatchJob) readUpstream(ctx context.Context, sess *patchSession, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path      string `json:"path"`
		Version   string `json:"version"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	tree, label := sess.before, j.To
	switch a.Version {
	case "", "new":
	case "old":
		if sess.oldTree == nil {
			return agent.Errorf("the old version (%s) is not available", j.From), nil
		}
		tree, label = sess.oldTree, j.From
	default:
		return agent.Errorf("version must be new or old"), nil
	}
	st, err := tree.Get(p)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	if !st.Exists {
		return agent.Errorf("%s does not exist at upstream %s", p, label), nil
	}
	return agent.OK(numberedLines(st.Data, a.StartLine, a.EndLine)), nil
}

func (j *PatchJob) upstreamPaths(ctx context.Context) ([]string, error) {
	if j.allPaths == nil {
		ents, err := j.New.List(ctx, "")
		if err != nil {
			return nil, err
		}
		j.allPaths = make([]string, 0, len(ents))
		for _, e := range ents {
			j.allPaths = append(j.allPaths, e.Path)
		}
	}
	return j.allPaths, nil
}

func (j *PatchJob) findUpstream(ctx context.Context, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	pat := strings.TrimPrefix(strings.TrimSpace(a.Pattern), "/")
	if pat == "" {
		return agent.Errorf("pattern is empty"), nil
	}
	glob := strings.ContainsAny(pat, "*?[")
	if glob {
		if err := config.ValidateGlob(pat); err != nil {
			return agent.Errorf("invalid glob: %v", err), nil
		}
	}
	all, err := j.upstreamPaths(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	var hits []string
	n := 0
	for _, p := range all {
		if (glob && config.MatchGlob(pat, p)) || (!glob && strings.Contains(p, pat)) {
			n++
			if len(hits) < 200 {
				hits = append(hits, p)
			}
		}
	}
	if n == 0 {
		return agent.OK("no files match"), nil
	}
	out := strings.Join(hits, "\n") + "\n"
	if n > len(hits) {
		out += fmt.Sprintf("[... %d more; narrow the pattern ...]\n", n-len(hits))
	}
	return agent.OK(out), nil
}

const grepMaxFiles = 3000

func (j *PatchJob) grepUpstream(ctx context.Context, sess *patchSession, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Dir     string `json:"dir"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return agent.Errorf("invalid regular expression: %v", err), nil
	}
	dir := strings.Trim(strings.TrimSpace(a.Dir), "/")
	if dir == "." {
		dir = ""
	}
	if dir != "" {
		if dir, err = cleanRel(dir); err != nil {
			return agent.Errorf("%v", err), nil
		}
	}
	all, err := j.upstreamPaths(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	var files []string
	for _, p := range all {
		if dir == "" || strings.HasPrefix(p, dir+"/") {
			files = append(files, p)
		}
	}
	switch {
	case len(files) == 0:
		return agent.Errorf("no files under %q", dir), nil
	case len(files) > grepMaxFiles:
		return agent.Errorf("%q holds %d files; search a smaller directory (at most %d files)", dir, len(files), grepMaxFiles), nil
	}
	if err := j.New.Prefetch(ctx, files); err != nil {
		return agent.Result{}, err
	}
	var out strings.Builder
	matches := 0
	for _, f := range files {
		st, err := sess.before.Get(f)
		if err != nil || !st.Exists || isBinary(string(st.Data)) {
			continue
		}
		for i, line := range strings.Split(string(st.Data), "\n") {
			if re.MatchString(line) {
				matches++
				if matches <= 200 {
					fmt.Fprintf(&out, "%s:%d: %s\n", f, i+1, clip(line, 300))
				}
			}
		}
	}
	if matches == 0 {
		return agent.OK(fmt.Sprintf("no matches in %d files", len(files))), nil
	}
	if matches > 200 {
		fmt.Fprintf(&out, "[... %d matches in total; narrow the search ...]\n", matches)
	}
	return agent.OK(out.String()), nil
}

func (j *PatchJob) upstreamDiff(ctx context.Context, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	p, err := cleanRel(a.Path)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	old, err := j.old(ctx)
	if err != nil {
		return agent.Errorf("the old version (%s) is not available: %v", j.From, err), nil
	}
	before, inOld, err := old.Bind(ctx).ReadFile(p)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	after, inNew, err := j.New.Bind(ctx).ReadFile(p)
	if err != nil {
		return agent.Errorf("%v", err), nil
	}
	switch {
	case !inOld && !inNew:
		return agent.Errorf("%s exists in neither %s nor %s", p, j.From, j.To), nil
	case inOld == inNew && bytes.Equal(before, after):
		return agent.OK(fmt.Sprintf("%s is unchanged between %s and %s", p, j.From, j.To)), nil
	case isBinary(string(before)) || isBinary(string(after)):
		return agent.OK(fmt.Sprintf("%s is a binary file and changed between %s and %s", p, j.From, j.To)), nil
	}
	var b strings.Builder
	oldName, newName := p+" (upstream "+j.From+")", p+" (upstream "+j.To+")"
	if !inOld {
		oldName = "/dev/null (not in " + j.From + ")"
	}
	if !inNew {
		newName = "/dev/null (deleted in " + j.To + ")"
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
	for _, h := range patch.Diff(before, after, patch.Context, patch.Style{SectionWidth: 80}) {
		b.WriteString(h.Text())
	}
	return agent.OK(b.String()), nil
}

func (j *PatchJob) patchSubmit(ctx context.Context, sess *patchSession, in json.RawMessage) (agent.Result, error) {
	var a struct {
		Summary        string   `json:"summary"`
		Files          []string `json:"files"`
		DropPatch      bool     `json:"drop_patch"`
		ConfirmDropped bool     `json:"confirm_dropped_changes"`
	}
	if err := agent.Decode(in, &a); err != nil {
		return agent.Errorf("%v", err), nil
	}
	sess.rounds++
	if sess.rounds > j.Repo.Resolve.MaxRounds {
		return agent.Result{}, fmt.Errorf("%d submissions rejected; giving up on this model", sess.rounds-1)
	}
	pf := sess.pf
	keep := map[string]bool{}
	for _, p := range sess.tracked {
		keep[p] = true
	}
	for p := range sess.opened {
		keep[p] = true
	}
	for _, f := range a.Files {
		p, err := cleanRel(f)
		if err != nil {
			return agent.Errorf("files: %q: %v", f, err), nil
		}
		keep[p] = true
	}
	root, err := os.OpenRoot(sess.ws.Dir)
	if err != nil {
		return agent.Result{}, &infraError{err}
	}
	defer root.Close()
	if stray := strayFiles(root, keep); len(stray) > 0 {
		return agent.Errorf("not accepted: these files are not part of the patch: %s. List them in files if they belong to it, or delete them.", strings.Join(limitList(stray, 20), ", ")), nil
	}
	paths := make([]string, 0, len(keep))
	for p := range keep {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	pre, final := map[string]patch.FileState{}, map[string]patch.FileState{}
	var changed []string
	for _, p := range paths {
		st, err := readWorkspaceFile(root, p)
		if err != nil {
			return agent.Errorf("not accepted: %v", err), nil
		}
		b, err := sess.before.Get(p)
		if err != nil {
			return agent.Errorf("not accepted: %s: %v", p, err), nil
		}
		pre[p], final[p] = b, st
		if !b.Equal(st) {
			if err := inRoot(pf, p); err != nil {
				return agent.Errorf("not accepted: %v", err), nil
			}
			changed = append(changed, p)
		}
	}
	switch {
	case len(changed) == 0 && !a.DropPatch:
		return agent.Errorf("not accepted: your working tree is identical to ref new, so the patch would change nothing. If upstream %s already does everything this patch did, call submit with drop_patch=true and explain why in the summary; otherwise make the changes.", j.To), nil
	case len(changed) > 0 && a.DropPatch:
		return agent.Errorf("not accepted: drop_patch is set, but your working tree changes %s. Restore those files (git checkout new -- <path>) to drop the patch, or unset drop_patch.", strings.Join(limitList(changed, 20), ", ")), nil
	}
	if !a.ConfirmDropped && !sess.warnedDropped {
		if miss := missingAdded(pf.parsed, final); len(miss) > 0 {
			sess.warnedDropped = true
			return agent.Errorf("not accepted yet: %d line(s) that the original patch adds are not in the result:\n  %s\nRe-apply them, or if leaving them out is intentional (for example upstream changed so they are no longer needed), call submit again with confirm_dropped_changes=true and explain why in the summary.",
				len(miss), strings.Join(limitList(miss, 15), "\n  ")), nil
		}
	}
	var data []byte
	var np *patch.Patch
	if len(changed) > 0 {
		if np, err = rebuildPatch(pf, pre, final, changed); err != nil {
			return agent.Errorf("not accepted: %v", err), nil
		}
		data = np.Format()
		if err := j.checkRebuilt(pf, sess.before, data, final); err != nil {
			return agent.Result{}, &infraError{err}
		}
	}
	added, removed := novelPatchLines(pf, np, sess, pre)
	if n := len(added) + len(removed); n > j.Repo.Resolve.MaxNovelLines {
		return agent.Errorf("not accepted: you wrote %d lines that are in neither the original patch nor the upstream files, or delete upstream lines the original did not (limit %d). Keep the update minimal.", n, j.Repo.Resolve.MaxNovelLines), nil
	}
	var verdict *review.Verdict
	if len(added)+len(removed) > 0 {
		v, usage, err := review.Audit(ctx, j.Auditor, patchAuditReport(pf, added, removed, pre, final, 80000), sess.tw, j.logger())
		j.usage.Add(usage)
		if err != nil {
			return agent.Result{}, &infraError{err}
		}
		verdict = v
		j.logger().Info("patch audit", "repo", j.Repo.Name, "patch", pf.path, "verdict", v.Verdict, "confidence", v.Confidence, "novel_lines", len(added), "novel_removals", len(removed))
		if v.Blocks(j.BlockOn, j.MinConfidence) {
			return agent.Result{}, fmt.Errorf("%w: %s", ErrAuditRejected, v.Summary)
		}
	}
	sess.result = &agentPatchResult{data: data, states: final, summary: a.Summary, novel: len(added) + len(removed), verdict: verdict}
	if data == nil {
		return agent.OK("accepted: the patch will be dropped from the series"), nil
	}
	return agent.OK(fmt.Sprintf("accepted: %s rewritten (%d file section(s))", pf.path, len(np.Files))), nil
}

func readWorkspaceFile(root *os.Root, p string) (patch.FileState, error) {
	st, err := root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return patch.FileState{}, nil
	}
	if err != nil {
		return patch.FileState{}, trimRootErr(err)
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return patch.FileState{}, fmt.Errorf("%s is a symlink; patches can only carry regular files", p)
	case st.IsDir():
		return patch.FileState{}, fmt.Errorf("%s is a directory", p)
	case st.Size() > 32<<20:
		return patch.FileState{}, fmt.Errorf("%s is larger than 32 MiB", p)
	}
	data, err := root.ReadFile(p)
	if err != nil {
		return patch.FileState{}, trimRootErr(err)
	}
	return patch.FileState{Data: data, Exists: true}, nil
}

// strayFiles lists workspace files that are not part of the result.
func strayFiles(root *os.Root, keep map[string]bool) []string {
	var out []string
	fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == ".git" || p == ".vibeci" {
				return fs.SkipDir
			}
			return nil
		}
		if !keep[p] {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func stateKind(before, after patch.FileState) string {
	switch {
	case !before.Exists && !after.Exists:
		return "none"
	case !before.Exists:
		return "create"
	case !after.Exists:
		return "delete"
	}
	return "modify"
}

func sectionKind(f *patch.File) string {
	switch {
	case f.NewFile:
		return "create"
	case f.Deleted:
		return "delete"
	}
	return "modify"
}

// rebuildPatch writes the patch that turns pre into final, keeping the
// original's description, naming and section headers where they still fit.
func rebuildPatch(pf *patchFile, pre, final map[string]patch.FileState, changed []string) (*patch.Patch, error) {
	style := pf.parsed.Style()
	naming := pf.parsed.Naming(pf.strip, pf.root)
	out := pf.parsed.Clone()
	out.Files = nil
	covered := map[string]bool{}
	for _, f := range pf.parsed.Files {
		oldPath, newPath, err := f.Paths(pf.strip, pf.root)
		if err != nil {
			return nil, err
		}
		if f.RenameFrom != "" || f.CopyFrom != "" {
			if covered[oldPath] || covered[newPath] {
				continue
			}
			covered[oldPath], covered[newPath] = true, true
			if f.RenameFrom != "" && final[oldPath].Exists {
				return nil, fmt.Errorf("the patch renames %s to %s, but %s still exists in your working tree: delete it", oldPath, newPath, oldPath)
			}
			if nf, keep := patch.RewriteSection(f, pre[oldPath], final[newPath], style); keep {
				out.Files = append(out.Files, nf)
			}
			continue
		}
		p := newPath
		if p == "" {
			p = oldPath
		}
		if covered[p] {
			continue // a second section for the same file: the first one now carries all of it
		}
		covered[p] = true
		if f.Binary {
			out.Files = append(out.Files, f) // it applied (the file is unchanged since the patch was written)
			continue
		}
		b, a := pre[p], final[p]
		switch k := stateKind(b, a); {
		case k == "none" || b.Equal(a):
		case k == sectionKind(f):
			if nf, keep := patch.RewriteSection(f, b, a, style); keep {
				out.Files = append(out.Files, nf)
			}
		default:
			nf, err := patch.NewSection(p, b, a, naming, style)
			if err != nil {
				return nil, err
			}
			out.Files = append(out.Files, nf)
		}
	}
	for _, p := range changed {
		if covered[p] {
			continue
		}
		nf, err := patch.NewSection(p, pre[p], final[p], naming, style)
		if err != nil {
			return nil, err
		}
		out.Files = append(out.Files, nf)
	}
	if len(out.Files) == 0 {
		return nil, errors.New("the rewritten patch would be empty")
	}
	return out, nil
}

// checkRebuilt re-applies a rewritten patch strictly to the tree before it
// and compares the result with what the agent wrote.
func (j *PatchJob) checkRebuilt(pf *patchFile, before *patch.Tree, data []byte, final map[string]patch.FileState) error {
	np, err := patch.Parse(data)
	if err != nil {
		return fmt.Errorf("internal error: the rewritten %s does not parse: %w", pf.path, err)
	}
	t := before.Clone()
	ar, err := patch.Apply(np, t, patch.Options{Strip: pf.strip, Root: pf.root, Exact: true, KeepBinary: func(string) bool { return true }})
	if err != nil {
		return fmt.Errorf("internal error: re-applying the rewritten %s: %w", pf.path, err)
	}
	if ar.Status() != patch.Exact {
		return fmt.Errorf("internal error: the rewritten %s does not apply exactly", pf.path)
	}
	for p, st := range final {
		got, err := t.Get(p)
		if err != nil {
			return err
		}
		if !got.Equal(st) {
			return fmt.Errorf("internal error: the rewritten %s does not reproduce %s", pf.path, p)
		}
	}
	return nil
}

// missingAdded returns lines the original patch adds that are in none of
// the final files, not even adapted: a line counts as adapted when some
// final line has most of its words (an added line usually changes along
// with the upstream line it replaces).
func missingAdded(orig *patch.Patch, final map[string]patch.FileState) []string {
	var lines []string
	for _, st := range final {
		if st.Exists {
			lines = append(lines, strings.Split(string(st.Data), "\n")...)
		}
	}
	have := map[string]bool{}
	postings := map[string][]int{}
	for i, l := range lines {
		have[norm(l)] = true
		for w := range words(l) {
			postings[w] = append(postings[w], i)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range orig.Files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				n := norm(l.Text)
				if l.Op != '+' || len(n) < 4 || !informative(n) || have[n] || seen[n] {
					continue
				}
				seen[n] = true
				if !adapted(n, postings) {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// words returns the identifier-like words of a line.
func words(l string) map[string]bool {
	out := map[string]bool{}
	start := -1
	for i := 0; i <= len(l); i++ {
		word := i < len(l) && (l[i] == '_' || l[i] >= 0x80 || (l[i] >= '0' && l[i] <= '9') || (l[i]|0x20 >= 'a' && l[i]|0x20 <= 'z'))
		switch {
		case word && start < 0:
			start = i
		case !word && start >= 0:
			out[l[start:i]] = true
			start = -1
		}
	}
	return out
}

// adapted reports whether some line (by postings) has at least 60% of
// l's words.
func adapted(l string, postings map[string][]int) bool {
	ws := words(l)
	if len(ws) == 0 {
		return false
	}
	counts := map[int]int{}
	for w := range ws {
		for _, i := range postings[w] {
			counts[i]++
			if counts[i]*5 >= len(ws)*3 {
				return true
			}
		}
	}
	return false
}

// auditLine is a line the agent wrote or an upstream line it deletes.
type auditLine struct {
	path string
	line int // in the final file (added) or the file before (removed)
	text string
}

// novelPatchLines returns the lines the rewritten patch adds that are in
// neither the original patch nor the upstream files it touches, and the
// upstream lines it deletes that the original did not delete.
func novelPatchLines(pf *patchFile, np *patch.Patch, sess *patchSession, pre map[string]patch.FileState) (added, removed []auditLine) {
	if np == nil {
		return nil, nil
	}
	var known []string
	origRemoved := map[string]bool{}
	for _, f := range pf.parsed.Files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch l.Op {
				case '+':
					known = append(known, l.Text)
				case '-':
					origRemoved[norm(l.Text)] = true
				}
			}
		}
	}
	for _, m := range []map[string]patch.FileState{pre, sess.oldPre, sess.oldPost} {
		for _, st := range m {
			if st.Exists {
				known = append(known, string(st.Data))
			}
		}
	}
	knownSet := lineSet(known...)
	for _, f := range np.Files {
		oldPath, newPath, _ := f.Paths(pf.strip, pf.root)
		p := newPath
		if p == "" {
			p = oldPath
		}
		for _, h := range f.Hunks {
			o, n := h.OldStart, h.NewStart
			for _, l := range h.Lines {
				t := norm(l.Text)
				switch l.Op {
				case ' ':
					o++
					n++
				case '-':
					if t != "" && informative(t) && !origRemoved[t] {
						removed = append(removed, auditLine{path: oldPath, line: o, text: l.Text})
					}
					o++
				case '+':
					if t != "" && informative(t) && !knownSet[t] {
						added = append(added, auditLine{path: p, line: n, text: l.Text})
					}
					n++
				}
			}
		}
	}
	return added, removed
}

// patchAuditReport renders the audit lines with surrounding context.
func patchAuditReport(pf *patchFile, added, removed []auditLine, pre, final map[string]patch.FileState, limit int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Patch file %s, rewritten by the agent for a new upstream version.\n\n", pf.path)
	section := func(lines []auditLine, title string, states map[string]patch.FileState, mark string) {
		byPath := map[string][]auditLine{}
		var order []string
		for _, l := range lines {
			if byPath[l.path] == nil {
				order = append(order, l.path)
			}
			byPath[l.path] = append(byPath[l.path], l)
		}
		for _, p := range order {
			ls := byPath[p]
			fmt.Fprintf(&sb, "=== %s: %d %s ===\n", p, len(ls), title)
			text := strings.Split(string(states[p].Data), "\n")
			marked := map[int]bool{}
			for _, l := range ls {
				marked[l.line] = true
			}
			last := 0
			for _, l := range ls {
				from, to := max(1, l.line-2), min(len(text), l.line+2)
				if from <= last {
					from = last + 1
				} else if last > 0 {
					sb.WriteString("   ...\n")
				}
				for i := from; i <= to; i++ {
					m := " "
					if marked[i] {
						m = mark
					}
					fmt.Fprintf(&sb, "%s%6d  %s\n", m, i, text[i-1])
				}
				last = max(last, to)
				if sb.Len() > limit {
					sb.WriteString("\n[... report truncated ...]\n")
					return
				}
			}
			sb.WriteString("\n")
		}
	}
	section(added, "line(s) the agent wrote (in neither the original patch nor the upstream files)", final, ">")
	section(removed, "upstream line(s) the rewritten patch deletes that the original patch did not delete", pre, "-")
	return sb.String()
}
