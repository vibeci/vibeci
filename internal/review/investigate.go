package review

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/agent"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
)

// readTools are read-only views of the trusted mirror. Every revision
// argument must be a hex commit id that exists; paths are always passed
// after "--" or as rev:path, so model input can never become a git option.
type readTools struct {
	repo   *gitx.Repo
	commit string
	parent string
	col    *collector
}

func (t *readTools) rev(ctx context.Context, s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "commit", "head":
		return t.commit, nil
	case "parent", "before":
		if t.parent == "" {
			return "", fmt.Errorf("the commit has no parent")
		}
		return t.parent, nil
	}
	if !gitx.IsHex(s) {
		return "", fmt.Errorf("revision must be a hex commit id, \"commit\" or \"parent\"")
	}
	return t.repo.Resolve(ctx, s)
}

func cleanPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if strings.ContainsAny(p, "\x00\n\r") || strings.HasPrefix(p, "-") {
		return "", fmt.Errorf("invalid path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid path")
		}
	}
	return p, nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func (t *readTools) tools() []*agent.Tool {
	return []*agent.Tool{
		{
			Name:        "show_commit",
			Description: "Show the diff of a commit (merge commits show their remerge-diff). Optionally limit to one path. Long output is paginated: pass the returned next_offset to continue.",
			Schema:      schema(`{"type":"object","properties":{"commit":{"type":"string","description":"hex id, or \"commit\" for the commit under investigation"},"path":{"type":"string"},"offset":{"type":"integer"}}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Commit string `json:"commit"`
					Path   string `json:"path"`
					Offset int    `json:"offset"`
				}
				if err := agent.Decode(in, &a); err != nil {
					return agent.Errorf("%v", err), nil
				}
				sha, err := t.rev(ctx, a.Commit)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				cs, err := t.repo.ReadCommits(ctx, []string{sha})
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				var text string
				if len(cs[0].Parents) > 1 && a.Path == "" {
					out, err := t.repo.RemergeDiff(ctx, sha, 32<<20)
					if err != nil {
						return agent.Errorf("%v", err), nil
					}
					text = header(cs[0], "merge") + "\nRemerge diff:\n" + string(out)
				} else {
					base := t.col.emptyTree
					if len(cs[0].Parents) > 0 {
						base = cs[0].Parents[0]
					}
					var paths []string
					if a.Path != "" {
						p, err := cleanPath(a.Path)
						if err != nil {
							return agent.Errorf("%v", err), nil
						}
						paths = []string{p}
					}
					out, err := t.repo.Patch(ctx, base, sha, 32<<20, paths...)
					if err != nil {
						return agent.Errorf("%v", err), nil
					}
					text = header(cs[0], "") + "\n" + string(out)
				}
				return agent.OK(page(text, a.Offset, 30000)), nil
			},
		},
		{
			Name:        "read_file",
			Description: "Read a file at a revision, with line numbers. Use start_line/end_line for large files (max 600 lines per call).",
			Schema:      schema(`{"type":"object","properties":{"rev":{"type":"string","description":"hex commit id, \"commit\" (after the change) or \"parent\" (before it)"},"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Rev       string `json:"rev"`
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
				}
				if err := agent.Decode(in, &a); err != nil {
					return agent.Errorf("%v", err), nil
				}
				sha, err := t.rev(ctx, a.Rev)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				p, err := cleanPath(a.Path)
				if err != nil || p == "" {
					return agent.Errorf("invalid path"), nil
				}
				data, found, err := t.repo.CatBlob(ctx, sha+":"+p, 32<<20)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				if !found {
					return agent.Errorf("%s does not exist at %s", p, sha[:12]), nil
				}
				return agent.OK(numbered(data, a.StartLine, a.EndLine, 600)), nil
			},
		},
		{
			Name:        "list_dir",
			Description: "List a directory at a revision (non-recursive).",
			Schema:      schema(`{"type":"object","properties":{"rev":{"type":"string"},"path":{"type":"string"}}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Rev  string `json:"rev"`
					Path string `json:"path"`
				}
				if err := agent.Decode(in, &a); err != nil {
					return agent.Errorf("%v", err), nil
				}
				sha, err := t.rev(ctx, a.Rev)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				p, err := cleanPath(a.Path)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				entries, err := t.repo.ListDir(ctx, sha, p)
				if err != nil {
					return agent.Errorf("cannot list %q: %v", p, err), nil
				}
				var sb strings.Builder
				for i, e := range entries {
					if i == 500 {
						fmt.Fprintf(&sb, "[... %d more entries ...]\n", len(entries)-500)
						break
					}
					fmt.Fprintf(&sb, "%s %s\n", e.Type, e.Path)
				}
				return agent.OK(sb.String()), nil
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents at a revision with an extended regular expression. Returns path:line:text (max 200 matches).",
			Schema:      schema(`{"type":"object","properties":{"rev":{"type":"string"},"pattern":{"type":"string"},"path":{"type":"string","description":"optional directory or file to limit the search"},"ignore_case":{"type":"boolean"}},"required":["pattern"]}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Rev        string `json:"rev"`
					Pattern    string `json:"pattern"`
					Path       string `json:"path"`
					IgnoreCase bool   `json:"ignore_case"`
				}
				if err := agent.Decode(in, &a); err != nil {
					return agent.Errorf("%v", err), nil
				}
				sha, err := t.rev(ctx, a.Rev)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				p, err := cleanPath(a.Path)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				if a.Pattern == "" || len(a.Pattern) > 500 || strings.ContainsAny(a.Pattern, "\x00\n") {
					return agent.Errorf("invalid pattern"), nil
				}
				args := []string{"grep", "-n", "-I", "-E", "--no-color", "--max-count=50"}
				if a.IgnoreCase {
					args = append(args, "-i")
				}
				args = append(args, "-e", a.Pattern, sha, "--")
				if p != "" {
					args = append(args, p)
				}
				gctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				out, err := t.repo.G.Run(gctx, gitx.Opts{GitDir: t.repo.GitDir, MaxOut: 4 << 20, Truncate: true, AllowExit: []int{1}, LiteralPathspecs: true}, args...)
				if gitx.ExitCode(err) == 1 {
					return agent.OK("no matches"), nil
				}
				if err != nil {
					return agent.Errorf("grep failed: %v", err), nil
				}
				lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
				var sb strings.Builder
				for i, l := range lines {
					if i == 200 {
						fmt.Fprintf(&sb, "[... %d more matches ...]\n", len(lines)-200)
						break
					}
					l = strings.TrimPrefix(l, sha+":")
					sb.WriteString(clip(l, 400) + "\n")
				}
				return agent.OK(sb.String()), nil
			},
		},
		{
			Name:        "log",
			Description: "Show recent history (one line per commit) up to a revision, optionally for one path.",
			Schema:      schema(`{"type":"object","properties":{"rev":{"type":"string"},"path":{"type":"string"},"max_count":{"type":"integer"}}}`),
			Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Rev      string `json:"rev"`
					Path     string `json:"path"`
					MaxCount int    `json:"max_count"`
				}
				if err := agent.Decode(in, &a); err != nil {
					return agent.Errorf("%v", err), nil
				}
				sha, err := t.rev(ctx, a.Rev)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				p, err := cleanPath(a.Path)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				n := a.MaxCount
				if n <= 0 || n > 100 {
					n = 30
				}
				args := []string{"log", "--no-color", "--date=short", "--format=%h %ad %an: %s", "-n", strconv.Itoa(n), sha, "--"}
				if p != "" {
					args = append(args, p)
				}
				out, err := t.repo.G.Run(ctx, gitx.Opts{GitDir: t.repo.GitDir, MaxOut: 1 << 20, Truncate: true, LiteralPathspecs: true}, args...)
				if err != nil {
					return agent.Errorf("%v", err), nil
				}
				return agent.OK(string(out)), nil
			},
		},
	}
}

func page(text string, offset, size int) string {
	if offset < 0 || offset > len(text) {
		offset = 0
	}
	end := min(len(text), offset+size)
	out := text[offset:end]
	if end < len(text) {
		out += fmt.Sprintf("\n[... output continues; call again with offset=%d (total %d) ...]", end, len(text))
	}
	return out
}

func numbered(data []byte, start, end, maxLines int) string {
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		kind, _ := binaryKind(data)
		return fmt.Sprintf("binary file, %d bytes (%s). First 256 bytes:\n%s", len(data), kind, hex.Dump(data[:min(256, len(data))]))
	}
	lines := strings.Split(string(data), "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if end-start+1 > maxLines {
		end = start + maxLines - 1
	}
	var sb strings.Builder
	for i := start; i <= end && i <= len(lines); i++ {
		fmt.Fprintf(&sb, "%6d  %s\n", i, clip(lines[i-1], 2000))
	}
	if end < len(lines) {
		fmt.Fprintf(&sb, "[... file has %d lines; continue with start_line=%d ...]\n", len(lines), end+1)
	}
	return sb.String()
}

var verdictSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["clean", "suspicious", "malicious"]},
    "confidence": {"type": "number", "description": "0..1"},
    "summary": {"type": "string", "description": "two or three sentences: what the commit does and why it is or is not an attack"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "file": {"type": "string"},
          "category": {"type": "string", "enum": ["exfiltration", "remote_code_execution", "backdoor", "obfuscation", "destructive", "ci_tampering", "dependency_tampering", "trojan_source", "reviewer_manipulation", "other"]},
          "evidence": {"type": "string"},
          "explanation": {"type": "string"}
        },
        "required": ["file", "category", "evidence", "explanation"]
      }
    }
  },
  "required": ["verdict", "confidence", "summary"]
}`)

func (rv *Reviewer) investigate(ctx context.Context, u *Unit, triage *Verdict) (*Verdict, error) {
	col, err := newCollector(ctx, rv.Repo, rv.Opts.BatchChars*9/10)
	if err != nil {
		return nil, err
	}
	rt := &readTools{repo: rv.Repo, commit: u.Commit.SHA, col: col}
	if len(u.Commit.Parents) > 0 {
		rt.parent = u.Commit.Parents[0]
	}
	var result *Verdict
	submit := &agent.Tool{
		Name: "submit_verdict", Description: "Submit your final verdict for the commit.", Schema: verdictSchema, Terminal: true,
		Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
			var e struct {
				Verdict    string    `json:"verdict"`
				Confidence float64   `json:"confidence"`
				Summary    string    `json:"summary"`
				Findings   []Finding `json:"findings"`
			}
			if err := json.Unmarshal(in, &e); err != nil || !validVerdict(e.Verdict) {
				return agent.Errorf("verdict must be clean, suspicious or malicious"), nil
			}
			if e.Verdict != Clean && len(e.Findings) == 0 {
				return agent.Errorf("a %s verdict needs at least one finding with verbatim evidence", e.Verdict), nil
			}
			result = &Verdict{Commit: u.Commit.SHA, Verdict: e.Verdict, Confidence: clamp01(e.Confidence), Summary: clip(e.Summary, 1500), Findings: e.Findings, Stage: "investigate", Model: rv.Investigate.Name(), Version: Version, Time: time.Now().UTC()}
			return agent.OK("recorded"), nil
		},
	}
	token := nonce()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Upstream repository: %s\nDownstream fork: %s\nCommit under investigation: %s\n\n", rv.Opts.UpstreamURL, rv.Opts.RepoName, u.Commit.SHA)
	if triage.Verdict != Clean {
		fmt.Fprintf(&sb, "Triage verdict: %s (confidence %.2f): %s\n", triage.Verdict, triage.Confidence, triage.Summary)
		for _, f := range triage.Findings {
			fmt.Fprintf(&sb, "- [%s] %s: %s\n  evidence: %s\n", f.Category, f.File, f.Explanation, clip(f.Evidence, 500))
		}
	} else {
		sb.WriteString("Triage found nothing, but a strong heuristic signal fired, so the commit is double-checked. Confirm whether the signal is benign.\n")
	}
	sb.WriteString("\nCommit as shown to triage:\n")
	sb.WriteString(fence(token, u.Rendered))
	sb.WriteString("\n\nInvestigate with the tools, then call submit_verdict.")

	tw, err := rv.transcript("investigate-" + u.Commit.SHA[:12])
	if err != nil {
		return nil, err
	}
	defer tw.Close()
	turns := rv.Opts.MaxInvestTurn
	if turns <= 0 {
		turns = 30
	}
	tools := append(rt.tools(), submit)
	out, err := agent.Run(ctx, agent.Config{
		Name: "investigate", Client: rv.Investigate, System: investigateSystem, Tools: tools,
		MaxTurns: turns, Timeout: 30 * time.Minute, Transcript: tw, Logger: rv.logger(),
	}, []llm.Block{llm.Text(sb.String())})
	if out != nil {
		rv.Usage.Add(out.Usage)
	}
	if err != nil {
		return nil, fmt.Errorf("investigating %s: %w", u.Commit.SHA[:12], err)
	}
	rv.logger().Info("investigation finished", "repo", rv.Opts.RepoName, "commit", u.Commit.SHA[:12], "verdict", result.Verdict, "confidence", result.Confidence, "turns", out.Turns)
	return result, nil
}
