package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPathValidation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x", "a/../../x", "..", ".", "a/./b", "/etc/passwd", "a//b", "a/b/", "", "x/.tmp-1", "a\\b", "a b"} {
		if _, err := s.Path(bad); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	for _, good := range []string{"repos/demo.json", "reviews/demo/0123abcd.json", "locks/my-repo_1.lock", "a/b.c/d"} {
		if _, err := s.Path(good); err != nil {
			t.Errorf("path %q rejected: %v", good, err)
		}
	}
}

func TestRoundTripAndLock(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	st, err := s.Load("demo")
	if err != nil || st.Repo != "demo" || st.Blocked == nil || st.Warned == nil {
		t.Fatalf("empty load: %+v %v", st, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	st.LastMerged = "abc"
	st.Failure = &Failure{Kind: "failed", Count: 2, NextRetry: now}
	st.Blocked["deadbeef"] = &Blocked{Commit: "deadbeef", Verdict: "malicious", Confidence: 0.9, FirstSeen: now}
	st.Usage.InputTokens = 42
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastMerged != "abc" || got.Failure.Count != 2 || !got.Failure.NextRetry.Equal(now) || got.Blocked["deadbeef"].Verdict != "malicious" || got.Usage.InputTokens != 42 {
		t.Fatalf("round trip: %+v", got)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(filepath.Join(dir, "repos"))
	if len(ents) != 1 {
		t.Errorf("repos dir: %v", ents)
	}

	unlock, err := s.Lock("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("demo"); err == nil {
		t.Error("second lock succeeded")
	}
	unlock()
	unlock2, err := s.Lock("demo")
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	unlock2()
}

func TestMerge3(t *testing.T) {
	cases := []struct {
		name               string
		base, ours, theirs string // "-" is absent
		want               string // "-" is absent
	}{
		{"both sides", `{"a":1,"b":1}`, `{"a":2,"b":1}`, `{"a":1,"b":3}`, `{"a":2,"b":3}`},
		{"deletion ours", `{"a":1,"b":2}`, `{"b":2}`, `{"a":1,"b":2}`, `{"b":2}`},
		{"deletion theirs", `{"a":1}`, `{"a":1}`, `{}`, `{}`},
		{"conflict: theirs wins", `{"a":{"x":1}}`, `{"a":{"x":2}}`, `{"a":{"x":3}}`, `{"a":{"x":3}}`},
		{"conflict: deletion wins", `{"a":1}`, `{"a":2}`, `{}`, `{}`},
		{"nested members merge", `{"m":{"p":1}}`, `{"m":{"p":1,"q":2}}`, `{"m":{"p":1,"r":3}}`, `{"m":{"p":1,"q":2,"r":3}}`},
		{"no base", `-`, `{"a":1,"c":1}`, `{"b":2,"c":2}`, `{"a":1,"b":2,"c":2}`},
		{"only ours", `-`, `{"a":1}`, `-`, `{"a":1}`},
		{"only theirs", `-`, `-`, `{"a":1}`, `{"a":1}`},
		{"arrays replace", `{"l":[1]}`, `{"l":[1,2]}`, `{"l":[1]}`, `{"l":[1,2]}`},
		{"numbers keep their text", `{"n":1}`, `{"n":1.50}`, `{"n":1}`, `{"n":1.50}`},
		{"all absent", `-`, `-`, `-`, `-`},
	}
	doc := func(s string) []byte {
		if s == "-" {
			return nil
		}
		return []byte(s)
	}
	for _, c := range cases {
		got, err := Merge3(doc(c.base), doc(c.ours), doc(c.theirs))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := doc(c.want); string(got) != string(want) || (got == nil) != (want == nil) {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if _, err := Merge3(nil, []byte(`{`), nil); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestStoreFiles(t *testing.T) {
	s, _ := Open(t.TempDir())
	if b, err := s.ReadFile("reviews/x/a.json"); b != nil || err != nil {
		t.Fatalf("missing file: %q %v", b, err)
	}
	if names, err := s.List("reviews/x"); names != nil || err != nil {
		t.Fatalf("missing dir: %v %v", names, err)
	}
	if err := s.WriteFile("reviews/x/a.json", []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.List("reviews/x"); len(names) != 1 || names[0] != "a.json" {
		t.Errorf("list: %v", names)
	}
	if err := s.Remove("reviews/x/a.json"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("reviews/x/a.json"); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
	if names, _ := s.List("reviews/x"); len(names) != 0 {
		t.Errorf("after remove: %v", names)
	}
}
