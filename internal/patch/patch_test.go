package patch

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const quiltPatch = `Description: disable the thing
 Longer text.
Author: someone

--- a/src/foo.c
+++ b/src/foo.c
@@ -2,7 +2,7 @@ int main(void)
 line2
 line3
 line4
-line5
+LINE5
 line6
 line7
 line8
@@ -20,6 +20,7 @@ static void helper(int a, int b, int c, int d)
 line20
 line21
 line22
+added after 22
 line23
 line24
 line25
`

const gitPatch = `From 4dfa8ed0814040317cb82d8545502186daa0a204 Mon Sep 17 00:00:00 2001
From: A U Thor <a@example.com>
Subject: [PATCH] change things

Body.
---
 src/foo.c | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

diff --git a/src/foo.c b/src/foo.c
index 1111111..2222222 100644
--- a/src/foo.c
+++ b/src/foo.c
@@ -1,3 +1,3 @@
 line1
-line2
+LINE2
 line3
diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
\ No newline at end of file
-- 
2.40.0
`

func lines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	return b.String()
}

func TestParseFormatRoundTrip(t *testing.T) {
	for name, src := range map[string]string{"quilt": quiltPatch, "git": gitPatch, "no final newline": strings.TrimSuffix(quiltPatch, "\n"), "empty": "", "text only": "just a description\n"} {
		p, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := string(p.Format()); got != src {
			t.Errorf("%s: round trip differs:\n%q\nwant\n%q", name, got, src)
		}
	}
}

func TestParseStructure(t *testing.T) {
	p, err := Parse([]byte(gitPatch))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("files: %d", len(p.Files))
	}
	if !strings.Contains(p.Preamble, "Subject: [PATCH] change things") || !strings.Contains(p.Preamble, "1 file changed") {
		t.Errorf("preamble: %q", p.Preamble)
	}
	if p.Trailer != "-- \n2.40.0\n" {
		t.Errorf("trailer: %q", p.Trailer)
	}
	f := p.Files[1]
	if !f.NewFile || !f.Git || f.OldName != DevNull || f.NewName != "b/new.txt" {
		t.Errorf("new file section: %+v", f)
	}
	if h := f.Hunks[0]; !h.Lines[1].NoNewline || h.OldStart != 0 || h.NewCount != 2 {
		t.Errorf("hunk: %+v", h)
	}
	op, np, err := p.Files[0].Paths(1, "")
	if err != nil || op != "src/foo.c" || np != "src/foo.c" {
		t.Errorf("paths: %q %q %v", op, np, err)
	}
	if _, np, _ := p.Files[1].Paths(1, "third_party/x"); np != "third_party/x/new.txt" {
		t.Errorf("rooted path: %q", np)
	}
	q, err := Parse([]byte(quiltPatch))
	if err != nil {
		t.Fatal(err)
	}
	if q.Files[0].Hunks[1].Section != " static void helper(int a, int b, int c, int d)" {
		t.Errorf("section: %q", q.Files[0].Hunks[1].Section)
	}
	if s := q.Style(); s.SectionWidth != 80 || s.Git {
		t.Errorf("style: %+v", s)
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"short hunk":   "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n a\n-b\n",
		"bad line":     "--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n*b\n",
		"too many old": "--- a/x\n+++ b/x\n@@ -1,1 +1,2 @@\n a\n-b\n+c\n",
		"unsafe path":  "--- a/../../etc/passwd\n+++ b/../../etc/passwd\n@@ -1 +1 @@\n-a\n+b\n",
		"git dir":      "--- a/.git/config\n+++ b/.git/config\n@@ -1 +1 @@\n-a\n+b\n",
		"names differ": "--- a/x\n+++ b/y\n@@ -1 +1 @@\n-a\n+b\n",
	} {
		p, err := Parse([]byte(src))
		if err == nil {
			_, err = Apply(p, NewTree(MapSource{"x": "a\nb\n"}), Options{Strip: 1})
		}
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestNameParsing(t *testing.T) {
	cases := map[string]string{
		"a/foo.c\t2024-01-01 10:00:00.000000000 +0100": "a/foo.c",
		"a/foo.c 2024-01-01 10:00:00 +0100":            "a/foo.c",
		"a/foo bar.c":                                  "a/foo bar.c",
		`"a/t\303\244st\tx.c"`:                         "a/t\u00e4st\tx.c",
		"/dev/null":                                    "/dev/null",
	}
	for in, want := range cases {
		if got := parseName(in); got != want {
			t.Errorf("parseName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := StripPath("/etc/passwd", 0); err == nil {
		t.Error("absolute path accepted")
	}
	if p, err := StripPath("/etc/passwd", 1); err != nil || p != "etc/passwd" {
		t.Errorf("-p1 of an absolute name: %q %v", p, err) // like GNU patch: relative to the tree
	}
	a, b := parseGitNames("a/dir with space/f b/dir with space/f")
	if a != "a/dir with space/f" || b != "b/dir with space/f" {
		t.Errorf("git names: %q %q", a, b)
	}
}

func apply(t *testing.T, src map[string]string, patch string, o Options) (*Tree, *Result) {
	t.Helper()
	p, err := Parse([]byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	if o.Strip == 0 {
		o.Strip = 1
	}
	tree := NewTree(MapSource(src))
	res, err := Apply(p, tree, o)
	if err != nil {
		t.Fatal(err)
	}
	return tree, res
}

func content(t *testing.T, tree *Tree, p string) string {
	t.Helper()
	s, err := tree.Get(p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Exists {
		return "<missing>"
	}
	return string(s.Data)
}

func TestApplyExactAndShifted(t *testing.T) {
	orig := lines(1, 30)
	tree, res := apply(t, map[string]string{"src/foo.c": orig}, quiltPatch, Options{})
	if res.Status() != Exact {
		t.Fatalf("status %v", res.Status())
	}
	want := strings.Replace(orig, "line5\n", "LINE5\n", 1)
	want = strings.Replace(want, "line22\n", "line22\nadded after 22\n", 1)
	if got := content(t, tree, "src/foo.c"); got != want {
		t.Errorf("result:\n%s", got)
	}

	// Two lines inserted at the top: both hunks shift by 2.
	tree, res = apply(t, map[string]string{"src/foo.c": "new1\nnew2\n" + orig}, quiltPatch, Options{})
	if res.Status() != Shifted {
		t.Fatalf("status %v", res.Status())
	}
	if h := res.Files[0].Hunks; h[0].Offset != 2 || h[1].Offset != 2 || h[1].Line != 22 {
		t.Errorf("hunks: %+v", h)
	}
	if got := content(t, tree, "src/foo.c"); got != "new1\nnew2\n"+want {
		t.Errorf("shifted result:\n%s", got)
	}
	if res.Files[0].Before.Data == nil || string(res.Files[0].After.Data) != "new1\nnew2\n"+want {
		t.Error("before/after not recorded")
	}
}

func TestApplyFuzzAndFailure(t *testing.T) {
	// The first context line of hunk 1 changed upstream.
	src := strings.Replace(lines(1, 30), "line2\n", "LINE2-upstream\n", 1)
	_, res := apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{})
	if res.Status() != Failed || res.FailedHunks() != 1 {
		t.Fatalf("without fuzz: %v, %d failed", res.Status(), res.FailedHunks())
	}
	near := res.Files[0].Hunks[0].Near
	if near == nil || near.Line != 2 || near.Matched != 6 || near.Total != 7 {
		t.Errorf("near: %+v", near)
	}
	if res.Files[0].Hunks[1].Status != Exact {
		t.Errorf("second hunk should still apply: %+v", res.Files[0].Hunks[1])
	}
	tree, res := apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{Fuzz: 1})
	if res.Status() != Fuzzed || res.Files[0].Hunks[0].Fuzz != 1 {
		t.Fatalf("with fuzz: %+v", res.Files[0].Hunks[0])
	}
	if got := content(t, tree, "src/foo.c"); !strings.Contains(got, "LINE2-upstream\nline3\nline4\nLINE5\n") {
		t.Errorf("fuzzed result:\n%s", got)
	}
	// A fuzzed match must be unique: the same lines twice in the file fail.
	dup := "--- a/f\n+++ b/f\n@@ -1,5 +1,5 @@\n ctx-a\n x\n-y\n+Y\n z\n ctx-b\n"
	_, res = apply(t, map[string]string{"f": "other\nx\ny\nz\nother\n\nx\ny\nz\nmore\n"}, dup, Options{Fuzz: 1})
	if res.Status() != Failed {
		t.Errorf("ambiguous fuzzed match: %+v", res.Files[0].Hunks[0])
	}
	_, res = apply(t, map[string]string{"f": "other\nx\ny\nz\nother\n"}, dup, Options{Fuzz: 1})
	if res.Status() != Fuzzed {
		t.Errorf("unique fuzzed match: %+v", res.Files[0].Hunks[0])
	}
	// The removed line itself is gone: no fuzz helps.
	src = strings.Replace(lines(1, 30), "line5\n", "", 1)
	_, res = apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{Fuzz: 2})
	if res.Files[0].Hunks[0].Status != Failed || res.Files[0].Hunks[0].Near.RemovedMissing != 1 {
		t.Errorf("missing removed line: %+v", res.Files[0].Hunks[0])
	}
}

func TestApplyAlreadyApplied(t *testing.T) {
	src := strings.Replace(lines(1, 30), "line5\n", "LINE5\n", 1)
	tree, res := apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{DetectApplied: true})
	h := res.Files[0].Hunks
	if h[0].Status != AlreadyApplied || h[1].Status != Exact || res.Upstreamed() {
		t.Fatalf("hunks: %+v", h)
	}
	if got := content(t, tree, "src/foo.c"); strings.Count(got, "LINE5") != 1 || !strings.Contains(got, "added after 22") {
		t.Errorf("result:\n%s", got)
	}
	// Fully applied.
	src = strings.Replace(src, "line22\n", "line22\nadded after 22\n", 1)
	_, res = apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{DetectApplied: true})
	if !res.Upstreamed() || res.Status() != AlreadyApplied {
		t.Errorf("fully applied: %+v", res.Files[0].Hunks)
	}
	// Without detection the same tree fails.
	_, res = apply(t, map[string]string{"src/foo.c": src}, quiltPatch, Options{})
	if res.Status() != Failed {
		t.Errorf("without detection: %v", res.Status())
	}
	// A deletion-only hunk counts as applied only with enough context and
	// when the removed line is gone from the whole file.
	del := "--- a/f\n+++ b/f\n@@ -1,7 +1,6 @@\n a\n b\n c\n-remove me\n d\n e\n f\n"
	_, res = apply(t, map[string]string{"f": "a\nb\nc\nd\ne\nf\n"}, del, Options{DetectApplied: true})
	if !res.Upstreamed() {
		t.Errorf("deletion already applied: %+v", res.Files[0].Hunks)
	}
	_, res = apply(t, map[string]string{"f": "a\nb\nc\nd\ne\nf\nremove me\n"}, del, Options{DetectApplied: true})
	if res.Status() != Failed {
		t.Errorf("removed line still elsewhere: %v", res.Status())
	}
}

func TestApplyNewDeleteRename(t *testing.T) {
	tree, res := apply(t, map[string]string{"src/foo.c": "line1\nline2\nline3\n"}, gitPatch, Options{})
	if res.Status() != Exact {
		t.Fatalf("status: %v %+v", res.Status(), res.Files[1])
	}
	if got := content(t, tree, "new.txt"); got != "hello\nworld" {
		t.Errorf("new file: %q", got)
	}
	_, res = apply(t, map[string]string{"src/foo.c": "line1\nLINE2\nline3\n", "new.txt": "hello\nworld"}, gitPatch, Options{DetectApplied: true})
	if !res.Upstreamed() {
		t.Errorf("both applied: %v", res.Status())
	}
	_, res = apply(t, map[string]string{"src/foo.c": "line1\nline2\nline3\n", "new.txt": "other"}, gitPatch, Options{DetectApplied: true})
	if res.Files[1].Status != Failed || !strings.Contains(res.Files[1].Problem, "already exists") {
		t.Errorf("existing file: %+v", res.Files[1])
	}

	del := "diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-x\n-y\n"
	tree, res = apply(t, map[string]string{"gone.txt": "x\ny\n"}, del, Options{})
	if res.Status() != Exact || content(t, tree, "gone.txt") != "<missing>" {
		t.Errorf("delete: %v", res.Status())
	}
	_, res = apply(t, map[string]string{"gone.txt": "x\nz\n"}, del, Options{})
	if res.Status() != Failed {
		t.Errorf("delete changed file: %v", res.Status())
	}

	ren := "diff --git a/old.txt b/new.txt\nsimilarity index 80%\nrename from old.txt\nrename to new.txt\n--- a/old.txt\n+++ b/new.txt\n@@ -1,2 +1,2 @@\n keep\n-old\n+new\n"
	tree, res = apply(t, map[string]string{"old.txt": "keep\nold\n"}, ren, Options{})
	if res.Status() != Exact || content(t, tree, "old.txt") != "<missing>" || content(t, tree, "new.txt") != "keep\nnew\n" {
		t.Errorf("rename: %v", res.Status())
	}
}

func TestApplyNoNewlineAtEOF(t *testing.T) {
	// Adds a newline at the end.
	p := "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"
	tree, _ := apply(t, map[string]string{"f": "a\nb"}, p, Options{})
	if got := content(t, tree, "f"); got != "a\nb\n" {
		t.Errorf("got %q", got)
	}
	// Removes it.
	p = "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n+b\n\\ No newline at end of file\n"
	tree, _ = apply(t, map[string]string{"f": "a\nb\n"}, p, Options{})
	if got := content(t, tree, "f"); got != "a\nb" {
		t.Errorf("got %q", got)
	}
	// Context at the end of a file without a newline is copied as is.
	p = "--- a/f\n+++ b/f\n@@ -1,2 +1,3 @@\n+top\n a\n b\n\\ No newline at end of file\n"
	tree, _ = apply(t, map[string]string{"f": "a\nb"}, p, Options{})
	if got := content(t, tree, "f"); got != "top\na\nb" {
		t.Errorf("got %q", got)
	}
}

func TestApplyIgnoreWhitespace(t *testing.T) {
	p := "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n if (x) {\n-  old();\n+  new();\n }\n"
	src := "if (x)  {\n\told();\n}\n"
	_, res := apply(t, map[string]string{"f": src}, p, Options{})
	if res.Status() != Failed {
		t.Fatal("strict match should fail")
	}
	tree, res := apply(t, map[string]string{"f": src}, p, Options{IgnoreWhitespace: true})
	if res.Status() != Exact {
		t.Fatalf("loose: %v", res.Status())
	}
	// Context keeps the file's whitespace; added lines come from the patch.
	if got := content(t, tree, "f"); got != "if (x)  {\n  new();\n}\n" {
		t.Errorf("got %q", got)
	}
}

func TestDiffRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"alpha", "beta", "gamma", "delta", "", "}", "{", "return x;", "int y = 0;"}
	gen := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = words[rng.Intn(len(words))]
		}
		return out
	}
	join := func(ls []string, eol bool) []byte {
		if len(ls) == 0 {
			return nil
		}
		s := strings.Join(ls, "\n")
		if eol {
			s += "\n"
		}
		return []byte(s)
	}
	for iter := 0; iter < 500; iter++ {
		a := gen(rng.Intn(60))
		b := append([]string(nil), a...)
		for e := rng.Intn(6); e >= 0; e-- {
			switch op := rng.Intn(3); {
			case op == 0 && len(b) > 0:
				i := rng.Intn(len(b))
				b = append(b[:i], b[i+1:]...)
			case op == 1:
				i := rng.Intn(len(b) + 1)
				b = append(b[:i], append(gen(1+rng.Intn(3)), b[i:]...)...)
			case len(b) > 0:
				b[rng.Intn(len(b))] = fmt.Sprintf("changed %d", rng.Int())
			}
		}
		aeol, beol := rng.Intn(5) != 0, rng.Intn(5) != 0
		before, after := join(a, aeol), join(b, beol)
		hs := Diff(before, after, Context, Style{SectionWidth: 40})
		f := &File{Header: []string{"--- a/f", "+++ b/f"}, OldName: "a/f", NewName: "b/f", Hunks: hs}
		p := &Patch{Files: []*File{f}}
		if bytes.Equal(before, after) {
			if len(hs) != 0 {
				t.Fatalf("iter %d: hunks for equal content", iter)
			}
			continue
		}
		text := p.Format()
		q, err := Parse(text)
		if err != nil {
			t.Fatalf("iter %d: generated patch does not parse: %v\n%s", iter, err, text)
		}
		tree := NewTree(MapSource{"f": string(before)})
		res, err := Apply(q, tree, Options{Strip: 1, Exact: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status() != Exact {
			t.Fatalf("iter %d: generated patch does not apply exactly (%v):\nbefore %q\nafter %q\n%s", iter, res.Status(), before, after, text)
		}
		if got := content(t, tree, "f"); got != string(after) {
			t.Fatalf("iter %d: got %q want %q\n%s", iter, got, after, text)
		}
	}
}

func TestDiffMinimalAndSections(t *testing.T) {
	a := []byte("int f(void)\n{\n  a();\n  b();\n  c();\n}\n\nint g(void)\n{\n  x();\n  y();\n  z();\n  w();\n}\n")
	b := []byte(strings.Replace(string(a), "  y();\n", "  Y();\n", 1))
	hs := Diff(a, b, Context, Style{SectionWidth: 40})
	if len(hs) != 1 {
		t.Fatalf("hunks: %d", len(hs))
	}
	h := hs[0]
	// Like diff -p and git, the section is the last function line before
	// the hunk's first line, which here is g's own declaration.
	if h.OldStart != 8 || h.OldCount != 7 || h.NewStart != 8 || h.Section != " int f(void)" {
		t.Errorf("hunk %s", h.Header())
	}
	// Large edit distance falls back to a replacement and still applies.
	var x, y strings.Builder
	for i := 0; i < 6000; i++ {
		fmt.Fprintf(&x, "a%d\n", i)
		fmt.Fprintf(&y, "b%d\n", i)
	}
	hs = Diff([]byte(x.String()), []byte(y.String()), Context, Style{})
	tree := NewTree(MapSource{"f": x.String()})
	res, _ := Apply(&Patch{Files: []*File{{OldName: "a/f", NewName: "b/f", Hunks: hs}}}, tree, Options{Strip: 1, Exact: true})
	if res.Status() != Exact || content(t, tree, "f") != y.String() {
		t.Error("large replacement does not apply")
	}
}

func TestRefresh(t *testing.T) {
	p, _ := Parse([]byte(quiltPatch))
	orig := lines(1, 30)

	// Shifted: only the line numbers change; bodies and sections stay.
	src := "new1\nnew2\n" + orig
	tree := NewTree(MapSource{"src/foo.c": src})
	res, _ := Apply(p, tree, Options{Strip: 1, DetectApplied: true})
	np, err := Refresh(p, res, false)
	if err != nil {
		t.Fatal(err)
	}
	got := string(np.Format())
	if !strings.Contains(got, "@@ -4,7 +4,7 @@ int main(void)\n") || !strings.Contains(got, "@@ -22,6 +22,7 @@ static void helper") {
		t.Errorf("shifted refresh:\n%s", got)
	}
	if strings.Replace(strings.Replace(got, "-4,7 +4,7", "-2,7 +2,7", 1), "-22,6 +22,7", "-20,6 +20,7", 1) != quiltPatch {
		t.Errorf("only the line numbers may change:\n%s", got)
	}
	verify := func(np *Patch, src, want string) {
		t.Helper()
		tree := NewTree(MapSource{"src/foo.c": src})
		r, err := Apply(np, tree, Options{Strip: 1, Exact: true})
		if err != nil || r.Status() != Exact {
			t.Fatalf("refreshed patch does not apply exactly: %v %v\n%s", err, r.Status(), np.Format())
		}
		if c := content(t, tree, "src/foo.c"); c != want {
			t.Fatalf("refreshed result differs:\n%s", c)
		}
	}
	verify(np, src, string(res.Files[0].After.Data))
	if kept, _ := Refresh(p, res, true); string(kept.Format()) != quiltPatch {
		t.Error("keepOffsets must keep the patch unchanged")
	}

	// Fuzzed: regenerated with the new context.
	src = strings.Replace(orig, "line2\n", "LINE2-upstream\n", 1)
	tree = NewTree(MapSource{"src/foo.c": src})
	res, _ = Apply(p, tree, Options{Strip: 1, Fuzz: 2, DetectApplied: true})
	np, err = Refresh(p, res, false)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(np.Format()); !strings.Contains(out, " LINE2-upstream\n") || !strings.HasPrefix(out, "Description: disable the thing\n") {
		t.Errorf("fuzz refresh:\n%s", out)
	}
	verify(np, src, string(res.Files[0].After.Data))

	// Partly applied upstream: the applied hunk disappears.
	src = strings.Replace(orig, "line5\n", "LINE5\n", 1)
	tree = NewTree(MapSource{"src/foo.c": src})
	res, _ = Apply(p, tree, Options{Strip: 1, DetectApplied: true})
	np, _ = Refresh(p, res, false)
	if out := string(np.Format()); strings.Contains(out, "-line5") || !strings.Contains(out, "+added after 22") {
		t.Errorf("partial refresh:\n%s", out)
	}
	verify(np, src, string(res.Files[0].After.Data))

	// Fully applied: nothing left.
	src = strings.Replace(src, "line22\n", "line22\nadded after 22\n", 1)
	tree = NewTree(MapSource{"src/foo.c": src})
	res, _ = Apply(p, tree, Options{Strip: 1, DetectApplied: true})
	if np, err := Refresh(p, res, false); err != nil || np != nil {
		t.Errorf("fully applied refresh: %v %v", np, err)
	}

	// Failed patches cannot be refreshed.
	tree = NewTree(MapSource{"src/foo.c": "unrelated\n"})
	res, _ = Apply(p, tree, Options{Strip: 1})
	if _, err := Refresh(p, res, false); err == nil {
		t.Error("refreshing a failed patch must fail")
	}
}

func TestIndexLineAndNewSection(t *testing.T) {
	p, _ := Parse([]byte(gitPatch))
	f := p.Files[0]
	f.SetIndex([]byte("x\n"), true, []byte("y\n"), true)
	if f.Header[1] != "index "+BlobID([]byte("x\n"))[:7]+".."+BlobID([]byte("y\n"))[:7]+" 100644" {
		t.Errorf("index: %q", f.Header[1])
	}
	// git hash-object of "hello\n".
	if BlobID([]byte("hello\n")) != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Error("BlobID")
	}
	n := p.Naming(1, "")
	if n.OldPrefix != "a/" || n.NewPrefix != "b/" || n.IndexLen != 7 {
		t.Errorf("naming: %+v", n)
	}
	sec, err := NewSection("dir/added.c", FileState{}, FileState{Data: []byte("int x;\n"), Exists: true}, n, p.Style())
	if err != nil {
		t.Fatal(err)
	}
	q := &Patch{Files: []*File{sec}}
	out := string(q.Format())
	if !strings.HasPrefix(out, "diff --git a/dir/added.c b/dir/added.c\nnew file mode 100644\nindex 0000000..") || !strings.Contains(out, "--- /dev/null\n+++ b/dir/added.c\n@@ -0,0 +1 @@\n+int x;\n") {
		t.Errorf("new section:\n%s", out)
	}
	if _, err := NewSection("elsewhere/x", FileState{}, FileState{Exists: true}, Naming{Root: "v8"}, Style{}); err == nil {
		t.Error("a section outside the root must fail")
	}
}

func TestSeries(t *testing.T) {
	src := "# comment\nfirst.patch\n\nsub/second.patch -p0\nthird.patch # note\n"
	s, err := ParseSeries([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entries) != 3 || s.Entries[1].Name != "sub/second.patch" || s.Entries[1].Strip != 0 || s.Entries[2].Name != "third.patch" || s.Entries[0].Strip != -1 {
		t.Fatalf("entries: %+v", s.Entries)
	}
	if string(s.Format()) != src {
		t.Error("series round trip")
	}
	s.Remove("sub/second.patch")
	if string(s.Format()) != "# comment\nfirst.patch\n\nthird.patch # note\n" || len(s.Entries) != 2 {
		t.Errorf("after remove: %q", s.Format())
	}
	for _, bad := range []string{"x.patch -R\n", "../x.patch\n", "x.patch -pz\n"} {
		if _, err := ParseSeries([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// TestPatchToolAgrees checks generated patches with the system's patch(1)
// when it is installed.
func TestPatchToolAgrees(t *testing.T) {
	bin, err := exec.LookPath("patch")
	if err != nil {
		t.Skip("patch(1) not installed")
	}
	dir := t.TempDir()
	before := lines(1, 40) + "tail without newline"
	after := strings.Replace(strings.Replace(before, "line10\n", "LINE10\nextra\n", 1), "line33\n", "", 1)
	after = strings.TrimSuffix(after, "tail without newline") + "tail with newline\n"
	f := &File{Header: []string{"--- a/f.txt", "+++ b/f.txt"}, OldName: "a/f.txt", NewName: "b/f.txt", Hunks: Diff([]byte(before), []byte(after), Context, Style{SectionWidth: 40})}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-p1", "-s", "--no-backup-if-mismatch", "-i", "-")
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader((&Patch{Files: []*File{f}}).Format())
	if out, err := cmd.CombinedOutput(); err != nil {
		// BSD patch has no --no-backup-if-mismatch; retry without it.
		cmd = exec.Command(bin, "-p1", "-s", "-i", "-")
		cmd.Dir = dir
		os.WriteFile(filepath.Join(dir, "f.txt"), []byte(before), 0o644)
		cmd.Stdin = bytes.NewReader((&Patch{Files: []*File{f}}).Format())
		if out2, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("patch: %v\n%s\n%s", err, out, out2)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(got) != after {
		t.Errorf("patch(1) result differs:\n%q\nwant\n%q", got, after)
	}
}

// TestCorpus round-trips and self-applies every patch under
// $VIBECI_PATCH_CORPUS (e.g. a checkout of ungoogled-chromium or brave-core).
func TestCorpus(t *testing.T) {
	dir := os.Getenv("VIBECI_PATCH_CORPUS")
	if dir == "" {
		t.Skip("set VIBECI_PATCH_CORPUS to a directory of patches")
	}
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".patch") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		n++
		pt, err := Parse(data)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if !bytes.Equal(pt.Format(), data) {
			t.Errorf("%s: round trip differs", p)
		}
		if len(pt.Files) == 0 {
			t.Errorf("%s: no file sections", p)
		}
		return nil
	})
	t.Logf("%d patches", n)
}
