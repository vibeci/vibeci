// Package review is the malicious-commit gate. Each new upstream commit is
// triaged by a model from its diff plus deterministic signals; flagged
// commits get a second-stage, read-only investigation; only confirmed
// malicious commits stop the merge.
package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vibeci/vibeci/internal/agent"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
	"github.com/vibeci/vibeci/internal/state"
)

// Verdict values.
const (
	Clean      = "clean"
	Suspicious = "suspicious"
	Malicious  = "malicious"
)

// Finding is one concrete problem reported by a model.
type Finding struct {
	File        string `json:"file"`
	Category    string `json:"category"`
	Evidence    string `json:"evidence"`
	Explanation string `json:"explanation"`
}

// Verdict is the review outcome for one commit.
type Verdict struct {
	Commit     string    `json:"commit"`
	Verdict    string    `json:"verdict"`
	Confidence float64   `json:"confidence"`
	Summary    string    `json:"summary"`
	Findings   []Finding `json:"findings,omitempty"`
	Signals    []Signal  `json:"signals,omitempty"`
	// Stage: trivial, allowlist, triage, investigate.
	Stage   string    `json:"stage"`
	Model   string    `json:"model,omitempty"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// StageInconclusive marks a commit whose investigation could not reach a
// verdict. It always blocks: a human has to look, or allow the commit.
const StageInconclusive = "inconclusive"

// inconclusiveRetry is how long an inconclusive verdict is cached before
// the commit is investigated again.
const inconclusiveRetry = 24 * time.Hour

// Blocks reports whether the verdict stops a merge under the given policy.
func (v *Verdict) Blocks(blockOn string, minConfidence float64) bool {
	if v.Stage == StageInconclusive {
		return true
	}
	switch v.Verdict {
	case Malicious:
		return v.Confidence >= minConfidence
	case Suspicious:
		return blockOn == Suspicious && v.Confidence >= minConfidence
	}
	return false
}

// Options configures a Reviewer.
type Options struct {
	RepoName      string
	UpstreamURL   string
	BatchChars    int
	MaxInvestTurn int
	AllowCommits  []string
	TranscriptDir string
}

// Reviewer reviews commits in a trusted mirror.
type Reviewer struct {
	Repo        *gitx.Repo
	Triage      llm.Client
	Investigate llm.Client
	Store       *state.Store
	Opts        Options
	Logger      *slog.Logger
	// Usage accumulates token usage across calls.
	Usage llm.Usage
}

func (rv *Reviewer) logger() *slog.Logger {
	if rv.Logger == nil {
		return slog.Default()
	}
	return rv.Logger
}

// CacheDir is the directory, relative to the state directory, of the
// verdicts cached for repo: one <commit>.json per reviewed commit.
func CacheDir(repo string) string { return "reviews/" + repo }

func (rv *Reviewer) cachePath(sha string) string {
	return CacheDir(rv.Opts.RepoName) + "/" + sha + ".json"
}

// Cached returns a cached verdict for sha (current version only).
func (rv *Reviewer) Cached(sha string) *Verdict {
	if rv.Store == nil {
		return nil
	}
	var v Verdict
	found, err := rv.Store.ReadJSON(rv.cachePath(sha), &v)
	if err != nil || !found || v.Version != Version {
		return nil
	}
	if v.Stage == StageInconclusive && time.Since(v.Time) > inconclusiveRetry {
		return nil
	}
	return &v
}

func (rv *Reviewer) save(v *Verdict) {
	if rv.Store == nil {
		return
	}
	if err := rv.Store.WriteJSON(rv.cachePath(v.Commit), v); err != nil {
		rv.logger().Warn("saving verdict", "commit", v.Commit, "err", err)
	}
}

// ReviewCommits returns a verdict for every sha. Verdicts are cached, so a
// failed run resumes where it stopped.
func (rv *Reviewer) ReviewCommits(ctx context.Context, shas []string) (map[string]*Verdict, error) {
	out := map[string]*Verdict{}
	var todo []string
	for _, sha := range shas {
		if slices.ContainsFunc(rv.Opts.AllowCommits, func(a string) bool { return strings.HasPrefix(sha, a) }) {
			out[sha] = &Verdict{Commit: sha, Verdict: Clean, Confidence: 1, Stage: "allowlist", Summary: "explicitly allowed (review.allow_commits or vibeci allow)", Version: Version, Time: time.Now().UTC()}
			continue
		}
		if v := rv.Cached(sha); v != nil {
			out[sha] = v
			continue
		}
		todo = append(todo, sha)
	}
	if len(todo) == 0 {
		return out, nil
	}
	commits, err := rv.Repo.ReadCommits(ctx, todo)
	if err != nil {
		return nil, err
	}
	col, err := newCollector(ctx, rv.Repo, rv.Opts.BatchChars*9/10)
	if err != nil {
		return nil, err
	}
	var units []*Unit
	for _, c := range commits {
		u, err := col.build(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("preparing %s: %w", c.SHA, err)
		}
		if u.Trivial != "" {
			v := &Verdict{Commit: c.SHA, Verdict: Clean, Confidence: 1, Stage: "trivial", Summary: u.Trivial, Version: Version, Time: time.Now().UTC()}
			rv.save(v)
			out[c.SHA] = v
			continue
		}
		units = append(units, u)
	}
	rv.logger().Info("reviewing commits", "repo", rv.Opts.RepoName, "commits", len(units), "cached_or_trivial", len(shas)-len(units))

	for i, batch := range batches(units, rv.Opts.BatchChars) {
		verdicts, err := rv.triageSplit(ctx, batch, fmt.Sprintf("%03d", i))
		if err != nil {
			return nil, err
		}
		for _, u := range batch {
			v := verdicts[u.Commit.SHA]
			v.Signals = u.Signals
			if v.Verdict != Clean || u.StrongSignals() {
				iv, err := rv.investigate(ctx, u, v)
				if err != nil {
					if !agent.Inconclusive(err) || ctx.Err() != nil {
						return nil, err
					}
					// Fail closed for this commit only: earlier commits can
					// still be merged, and a human is alerted.
					rv.logger().Warn("investigation inconclusive; blocking the commit", "repo", rv.Opts.RepoName, "commit", u.Commit.SHA[:12], "err", err)
					iv = &Verdict{Commit: u.Commit.SHA, Verdict: Suspicious, Confidence: 1, Stage: StageInconclusive, Findings: v.Findings,
						Summary: clip(fmt.Sprintf("The investigation could not reach a verdict (%v). Triage said %s (confidence %.2f): %s", err, v.Verdict, v.Confidence, v.Summary), 1500),
						Model:   rv.Investigate.Name(), Version: Version, Time: time.Now().UTC()}
				}
				v = iv
				v.Signals = u.Signals
			}
			rv.save(v)
			out[u.Commit.SHA] = v
		}
	}
	return out, nil
}

// triageSplit triages a batch, bisecting it when the model cannot finish
// (so one hostile or confusing commit cannot stall a whole batch). A single
// commit that still cannot be triaged is sent to investigation.
func (rv *Reviewer) triageSplit(ctx context.Context, batch []*Unit, name string) (map[string]*Verdict, error) {
	verdicts, err := rv.triage(ctx, batch, name)
	if err == nil || !agent.Inconclusive(err) || ctx.Err() != nil {
		return verdicts, err
	}
	if len(batch) == 1 {
		u := batch[0]
		rv.logger().Warn("triage inconclusive; investigating", "repo", rv.Opts.RepoName, "commit", u.Commit.SHA[:12], "err", err)
		return map[string]*Verdict{u.Commit.SHA: {Commit: u.Commit.SHA, Verdict: Suspicious, Confidence: 0.5, Stage: "triage", Model: rv.Triage.Name(),
			Summary: fmt.Sprintf("triage could not reach a verdict: %v", err), Version: Version, Time: time.Now().UTC()}}, nil
	}
	rv.logger().Warn("triage inconclusive; splitting the batch", "repo", rv.Opts.RepoName, "commits", len(batch), "err", err)
	mid := len(batch) / 2
	out, err := rv.triageSplit(ctx, batch[:mid], name+"a")
	if err != nil {
		return nil, err
	}
	rest, err := rv.triageSplit(ctx, batch[mid:], name+"b")
	if err != nil {
		return nil, err
	}
	for k, v := range rest {
		out[k] = v
	}
	return out, nil
}

// batches packs units into batches bounded by total characters.
func batches(units []*Unit, maxChars int) [][]*Unit {
	var out [][]*Unit
	var cur []*Unit
	size := 0
	for _, u := range units {
		n := len(u.Rendered)
		if len(cur) > 0 && (size+n > maxChars || len(cur) >= 25) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, u)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func nonce() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fence wraps untrusted content between markers carrying a random token.
func fence(token, content string) string {
	content = strings.ReplaceAll(content, token, "[token]")
	return "<<<UNTRUSTED-" + token + "\n" + content + "\nUNTRUSTED-" + token + ">>>"
}

var reviewSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reviews": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "commit": {"type": "string", "description": "commit id as shown (prefix of at least 7 characters is fine)"},
          "verdict": {"type": "string", "enum": ["clean", "suspicious", "malicious"]},
          "confidence": {"type": "number", "description": "0..1, confidence in the verdict"},
          "summary": {"type": "string", "description": "one sentence"},
          "findings": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "file": {"type": "string"},
                "category": {"type": "string", "enum": ["exfiltration", "remote_code_execution", "backdoor", "obfuscation", "destructive", "ci_tampering", "dependency_tampering", "trojan_source", "reviewer_manipulation", "other"]},
                "evidence": {"type": "string", "description": "short verbatim excerpt of the offending code"},
                "explanation": {"type": "string"}
              },
              "required": ["file", "category", "evidence", "explanation"]
            }
          }
        },
        "required": ["commit", "verdict", "confidence", "summary"]
      }
    }
  },
  "required": ["reviews"]
}`)

type reviewEntry struct {
	Commit     string    `json:"commit"`
	Verdict    string    `json:"verdict"`
	Confidence float64   `json:"confidence"`
	Summary    string    `json:"summary"`
	Findings   []Finding `json:"findings"`
}

// matchCommit resolves a possibly abbreviated id against the batch.
func matchCommit(id string, shas []string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) < 7 {
		return ""
	}
	match := ""
	for _, s := range shas {
		if strings.HasPrefix(s, id) {
			if match != "" {
				return ""
			}
			match = s
		}
	}
	return match
}

func validVerdict(v string) bool { return v == Clean || v == Suspicious || v == Malicious }

func clamp01(f float64) float64 { return max(0, min(1, f)) }

func (rv *Reviewer) transcript(name string) (io.WriteCloser, error) {
	if rv.Opts.TranscriptDir == "" {
		return nopCloser{io.Discard}, nil
	}
	if err := os.MkdirAll(rv.Opts.TranscriptDir, 0o755); err != nil {
		return nil, err
	}
	return os.Create(filepath.Join(rv.Opts.TranscriptDir, name+".jsonl"))
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (rv *Reviewer) triage(ctx context.Context, batch []*Unit, name string) (map[string]*Verdict, error) {
	var shas []string
	for _, u := range batch {
		shas = append(shas, u.Commit.SHA)
	}
	token := nonce()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Upstream repository: %s\nDownstream fork: %s\nCommits in this batch: %d\n\n", rv.Opts.UpstreamURL, rv.Opts.RepoName, len(batch))
	var content strings.Builder
	for i, u := range batch {
		fmt.Fprintf(&content, "=== COMMIT %d/%d %s ===\n%s\n=== END COMMIT %s ===\n\n", i+1, len(batch), u.Commit.SHA, u.Rendered, u.Commit.SHA)
	}
	sb.WriteString(fence(token, content.String()))
	fmt.Fprintf(&sb, "\n\nCall submit_review with exactly one entry for each of these commits:\n%s\n", strings.Join(shas, "\n"))

	result := map[string]*Verdict{}
	submit := &agent.Tool{
		Name:        "submit_review",
		Description: "Submit the verdict for every commit in the batch.",
		Schema:      reviewSchema,
		Terminal:    true,
		Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
			var v struct {
				Reviews []reviewEntry `json:"reviews"`
			}
			if err := json.Unmarshal(in, &v); err != nil {
				return agent.Errorf("invalid input: %v", err), nil
			}
			got := map[string]*Verdict{}
			var problems []string
			for _, e := range v.Reviews {
				sha := matchCommit(e.Commit, shas)
				if sha == "" {
					problems = append(problems, fmt.Sprintf("unknown or ambiguous commit id %q", e.Commit))
					continue
				}
				if !validVerdict(e.Verdict) {
					problems = append(problems, fmt.Sprintf("%s: invalid verdict %q", e.Commit, e.Verdict))
					continue
				}
				got[sha] = &Verdict{Commit: sha, Verdict: e.Verdict, Confidence: clamp01(e.Confidence), Summary: clip(e.Summary, 500), Findings: e.Findings, Stage: "triage", Model: rv.Triage.Name(), Version: Version, Time: time.Now().UTC()}
			}
			var missing []string
			for _, s := range shas {
				if got[s] == nil {
					missing = append(missing, s)
				}
			}
			if len(missing) > 0 {
				problems = append(problems, "missing verdicts for: "+strings.Join(missing, ", "))
			}
			if len(problems) > 0 {
				return agent.Errorf("not accepted: %s. Call submit_review again with all commits.", strings.Join(problems, "; ")), nil
			}
			for k, v := range got {
				result[k] = v
			}
			return agent.OK("recorded"), nil
		},
	}
	tw, err := rv.transcript("triage-" + name)
	if err != nil {
		return nil, err
	}
	defer tw.Close()
	out, err := agent.Run(ctx, agent.Config{
		Name:       "triage",
		Client:     rv.Triage,
		System:     triageSystem,
		Tools:      []*agent.Tool{submit},
		MaxTurns:   4,
		ToolChoice: "tool:submit_review",
		Transcript: tw,
		Logger:     rv.logger(),
	}, []llm.Block{llm.Text(sb.String())})
	if out != nil {
		rv.Usage.Add(out.Usage)
	}
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}
	for _, v := range result {
		if v.Verdict != Clean {
			rv.logger().Warn("triage flagged commit", "repo", rv.Opts.RepoName, "commit", v.Commit, "verdict", v.Verdict, "confidence", v.Confidence, "summary", v.Summary)
		}
	}
	return result, nil
}

// Audit reviews text written by the merge agent. It returns a verdict for
// the pseudo-commit "resolution".
func Audit(ctx context.Context, client llm.Client, report string, transcript io.Writer, logger *slog.Logger) (*Verdict, llm.Usage, error) {
	token := nonce()
	msg := "Lines written by the merge agent, with their location and surrounding context:\n\n" + fence(token, report) + "\n\nCall submit_review with one entry for commit \"resolution\"."
	var result *Verdict
	submit := &agent.Tool{
		Name: "submit_review", Description: "Submit the audit verdict.", Schema: reviewSchema, Terminal: true,
		Run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
			var v struct {
				Reviews []reviewEntry `json:"reviews"`
			}
			if err := json.Unmarshal(in, &v); err != nil || len(v.Reviews) != 1 || !validVerdict(v.Reviews[0].Verdict) {
				return agent.Errorf("submit exactly one review entry with commit \"resolution\" and a valid verdict"), nil
			}
			e := v.Reviews[0]
			result = &Verdict{Commit: "resolution", Verdict: e.Verdict, Confidence: clamp01(e.Confidence), Summary: clip(e.Summary, 500), Findings: e.Findings, Stage: "audit", Model: client.Name(), Version: Version, Time: time.Now().UTC()}
			return agent.OK("recorded"), nil
		},
	}
	out, err := agent.Run(ctx, agent.Config{
		Name: "audit", Client: client, System: auditSystem, Tools: []*agent.Tool{submit},
		MaxTurns: 4, ToolChoice: "tool:submit_review", Transcript: transcript, Logger: logger,
	}, []llm.Block{llm.Text(msg)})
	var usage llm.Usage
	if out != nil {
		usage = out.Usage
	}
	if err != nil {
		return nil, usage, fmt.Errorf("audit: %w", err)
	}
	return result, usage, nil
}
