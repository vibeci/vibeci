package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vibeci/vibeci/e2e/fakellm"
)

// Greet is a patch-mode fork in the style of ungoogled-chromium: the fork
// holds patches/series, the patches and the pinned upstream version
// (upstream-version.txt, plus revision.txt that restarts at 1 for every
// upstream version). Upstream is a small Go module tagged v1.0.0 and
// v1.1.0 (and a v1.2.0-rc1 release candidate that must be ignored).
// Moving the fork from 1.0.0 to 1.1.0:
//
//   - 0001-disable-usage-reports.patch only moves (upstream added lines
//     above it): its line numbers are updated.
//   - 0002-friendlier-greeting.patch conflicts (upstream rewrote the line
//     it changes with fmt.Sprintf): the patch agent rewrites it.
//   - 0003-trim-names.patch is part of upstream 1.1.0: it is dropped.
//
// The fork's checks apply the series to the upstream tree and run the Go
// tests, which expect the fork's greeting and disabled reports.
type Greet struct {
	*Layout
	// V1 and V2 are the upstream commits of v1.0.0 and v1.1.0.
	V1, V2 string
}

// GreetDescription is what the fork changes (for the patch agent).
const GreetDescription = "The fork disables usage reports and uses a friendlier greeting (\"Hello, NAME, nice to meet you!\")."

// NewGreet builds the scenario under dir.
func NewGreet(t testing.TB, dir string) *Greet {
	t.Helper()
	l := newLayout(t, dir)
	up := l.Upstream
	up.Git("config", "uploadpack.allowFilter", "true")
	up.Git("config", "uploadpack.allowAnySHA1InWant", "true")
	g := &Greet{Layout: l}
	g.V1 = up.Commit("greet 1.0.0", map[string]string{
		"go.mod":            "module example.com/greet\n\ngo 1.22\n",
		"greet.go":          greetV1,
		"greet_test.go":     greetTestV1,
		"report.go":         reportV1,
		"cmd/greet/main.go": greetMain,
	})
	up.Git("tag", "v1.0.0")
	g.V2 = up.Commit("greet 1.1.0", map[string]string{"greet.go": greetV2, "report.go": reportV2})
	up.Git("tag", "v1.1.0")
	up.Commit("greet 1.2.0-rc1", map[string]string{"README.md": "release candidate\n"})
	up.Git("tag", "v1.2.0-rc1")

	// The fork is not a clone of upstream: it only holds patches.
	if err := os.MkdirAll(l.ForkBare, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, l.ForkBare, "init", "-q", "--bare", "-b", "main")
	work := filepath.Join(filepath.Dir(l.ForkBare), "fork-work")
	Git(t, filepath.Dir(l.ForkBare), "clone", "-q", l.ForkBare, work)
	l.Fork = &Repo{t: t, Dir: work, name: "Fork Maintainer", email: "me@fork.invalid", day: up.day}
	Git(t, work, "symbolic-ref", "HEAD", "refs/heads/main")

	// Each patch is made against the tree with the earlier ones applied.
	tree := map[string]string{"greet.go": greetV1, "greet_test.go": greetTestV1, "report.go": reportV1}
	patch := func(desc string, quilt bool, edits ...[3]string) string {
		after := map[string]string{}
		for k, v := range tree {
			after[k] = v
		}
		for _, e := range edits {
			if !strings.Contains(after[e[0]], e[1]) {
				t.Fatalf("%s: %q not found", e[0], e[1])
			}
			after[e[0]] = strings.Replace(after[e[0]], e[1], e[2], 1)
		}
		out := desc + "\n" + diff(t, dir, tree, after, quilt)
		tree = after
		return out
	}
	p1 := patch("# Never send usage reports.\n", true,
		[3]string{"report.go", "var Enabled = true", "var Enabled = false"},
		[3]string{"greet_test.go", "\nfunc TestReports(", "\nfunc TestNoReports(t *testing.T) {\n\tif Enabled {\n\t\tt.Fatal(\"usage reports must be disabled\")\n\t}\n}\n\nfunc TestReports("})
	p2 := patch("From: Fork Maintainer <me@fork.invalid>\nSubject: [PATCH] Friendlier greeting\n\n---\n", false,
		[3]string{"greet.go", `return "Hello, " + name + "!"`, `return "Hello, " + name + ", nice to meet you!"`},
		[3]string{"greet_test.go", `"Hello, world!"`, `"Hello, world, nice to meet you!"`})
	p3 := patch("# Ignore surrounding spaces in names.\n", true,
		[3]string{"greet.go", "func Hello(name string) string {\n", "func Hello(name string) string {\n\tname = strings.TrimSpace(name)\n"})
	l.Fork.Commit("Patches for greet 1.0.0", map[string]string{
		"README.md":            "# greet, the private edition\n\nPatches on top of upstream greet.\n",
		"upstream-version.txt": "1.0.0\n",
		"revision.txt":         "3\n",
		"patches/series":       "# applied in order with patch -p1\n0001-disable-usage-reports.patch\n0002-friendlier-greeting.patch\n0003-trim-names.patch\n",
		"patches/0001-disable-usage-reports.patch": p1,
		"patches/0002-friendlier-greeting.patch":   p2,
		"patches/0003-trim-names.patch":            p3,
	})
	l.Fork.Push()
	return g
}

// diff returns the patch turning before into after, in git format or (quilt)
// with only the ---/+++ headers.
func diff(t testing.TB, dir string, before, after map[string]string, quilt bool) string {
	t.Helper()
	scratch, err := os.MkdirTemp(dir, "diff-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(scratch)
	day := 0
	r := &Repo{t: t, Dir: scratch, name: "x", email: "x@x.invalid", day: &day}
	r.Git("init", "-q")
	r.Commit("before", before)
	r.Write(after)
	out := r.Git("diff", "--no-color", "--no-renames")
	if quilt {
		var keep []string
		for _, l := range strings.Split(out, "\n") {
			if !strings.HasPrefix(l, "diff --git ") && !strings.HasPrefix(l, "index ") {
				keep = append(keep, l)
			}
		}
		out = strings.Join(keep, "\n")
	}
	return out + "\n"
}

// Script returns the fake model's script: how the patch agent rewrites the
// conflicting patch.
func (g *Greet) Script() fakellm.Script {
	return fakellm.Script{Patches: map[string]fakellm.PatchUpdate{
		"0002-friendlier-greeting.patch": {Files: map[string]string{"greet.go": strings.Replace(greetV2, `"Hello, %s!"`, `"Hello, %s, nice to meet you!"`, 1)}},
	}}
}

// Verify lists the fork's checks: they run with the fork in fork/, the
// upstream files in upstream/ and upstream with the patches in patched/.
func (g *Greet) Verify() []map[string]string {
	return []map[string]string{
		{"name": "test", "run": "cd patched && go vet ./... && go test ./...", "timeout": "10m"},
	}
}

// Apply applies the fork's series (at the fork's main) to a checkout of
// upstream rev in dir.
func (g *Greet) Apply(t testing.TB, dir, rev string) {
	t.Helper()
	Git(t, filepath.Dir(dir), "clone", "-q", g.Upstream.Dir, dir)
	Git(t, dir, "checkout", "-q", "--detach", rev)
	for _, name := range strings.Split(g.Show(t, "patches/series"), "\n") {
		if name = strings.TrimSpace(name); name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		p := filepath.Join(dir, ".patch")
		if err := os.WriteFile(p, []byte(g.Show(t, "patches/"+name)), 0o644); err != nil {
			t.Fatal(err)
		}
		Git(t, dir, "apply", "--whitespace=nowarn", ".patch")
		os.Remove(p)
	}
}

const greetV1 = `// Package greet says hello.
package greet

import "strings"

// Hello greets name (the world by default).
func Hello(name string) string {
	if name == "" {
		name = "world"
	}
	return "Hello, " + name + "!"
}

// Shout greets name loudly.
func Shout(name string) string {
	return strings.ToUpper(Hello(name))
}
`

// greetV2 adopts the fork's trimming, builds the greeting with Sprintf and
// adds Wave.
const greetV2 = `// Package greet says hello.
package greet

import (
	"fmt"
	"strings"
)

// Hello greets name (the world by default).
func Hello(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("Hello, %s!", name)
}

// Shout greets name loudly.
func Shout(name string) string {
	return strings.ToUpper(Hello(name))
}

// Wave greets without words.
func Wave() string { return "o/" }
`

const greetTestV1 = `package greet

import "testing"

func TestHello(t *testing.T) {
	if got := Hello(""); got != "Hello, world!" {
		t.Fatalf("Hello() = %q", got)
	}
	if got := Shout(" bob "); got[:6] != "HELLO," {
		t.Fatalf("Shout() = %q", got)
	}
}

func TestReports(t *testing.T) {
	if Enabled && ReportURL == "" {
		t.Fatal("reports are enabled without a URL")
	}
}
`

const reportV1 = `package greet

// ReportURL receives anonymous usage reports.
const ReportURL = "https://usage.greet.invalid/report"

// Enabled turns usage reports on.
var Enabled = true
`

const reportV2 = `package greet

// Usage reports help the authors decide what to work on. They contain
// the number of greetings and nothing else.

// ReportURL receives anonymous usage reports.
const ReportURL = "https://usage.greet.invalid/report"

// Enabled turns usage reports on.
var Enabled = true
`

const greetMain = `// Command greet greets its argument.
package main

import (
	"fmt"
	"os"

	"example.com/greet"
)

func main() {
	name := ""
	if len(os.Args) > 1 {
		name = os.Args[1]
	}
	fmt.Println(greet.Hello(name))
}
`
