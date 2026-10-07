package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ColorSource is the default upstream of the Color scenario.
const ColorSource = "https://github.com/fatih/color.git"

// ColorBase is the upstream release the fork was taken from.
const ColorBase = "v1.13.0"

// ColorDescription is the fork description given to VibeCI.
const ColorDescription = "Our fork adds FORCE_COLOR / CLICOLOR_FORCE support (force.go, the NoColor initialisation in color.go, force_test.go) so CI logs keep colors; NO_COLOR must still win. It also runs its own CI workflow."

// ColorWorkflow is the fork's own CI workflow, which syncs must keep.
const ColorWorkflow = `name: fork-ci
on: [push]
jobs:
  test:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`

// Color is a real-world fork: github.com/fatih/color taken at v1.13.0 with
// the kind of patches forks carry — a feature in a new file plus a hook in
// upstream code (FORCE_COLOR support), a README note, and its own CI
// workflow — tracking upstream's release tags. Upstream has since changed
// the code the hook and the new file depend on.
type Color struct {
	*Layout
	// Source is the upstream remote.
	Source string
	// Latest is upstream's newest v* tag, and Target its commit.
	Latest, Target string
}

// NewColor builds the scenario under dir from source (default ColorSource;
// a local clone works too).
func NewColor(t testing.TB, dir, source string) *Color {
	t.Helper()
	if source == "" {
		source = ColorSource
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "upstream-mirror.git")
	Git(t, dir, "clone", "-q", "--bare", source, mirror)
	latest := strings.SplitN(Git(t, mirror, "tag", "-l", "v*", "--sort=-v:refname"), "\n", 2)[0]
	c := &Color{Source: source, Latest: latest, Target: Git(t, mirror, "rev-parse", latest+"^{commit}")}

	l := &Layout{ForkBare: filepath.Join(dir, "fork.git")}
	Git(t, dir, "init", "-q", "--bare", "-b", "main", l.ForkBare)
	Git(t, mirror, "push", "-q", l.ForkBare, ColorBase+"^{commit}:refs/heads/main")
	work := filepath.Join(dir, "fork-work")
	Git(t, dir, "clone", "-q", l.ForkBare, work)
	day := 0
	f := &Repo{t: t, Dir: work, name: "Fork Maintainer", email: "me@fork.invalid", day: &day}
	l.Fork = f

	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(work, p))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	color := read("color.go")
	const oldInit = `	NoColor = noColorExists() || os.Getenv("TERM") == "dumb" ||
		(!isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd()))`
	const newInit = `	//
	// FORCE_COLOR or CLICOLOR_FORCE (set to anything but "0") turn colors on
	// even when stdout is not a terminal, e.g. in CI logs, unless NO_COLOR is
	// also set.
	NoColor = !forceColor() && (noColorExists() || os.Getenv("TERM") == "dumb" ||
		(!isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd())))`
	if !strings.Contains(color, oldInit) {
		t.Fatalf("%s color.go: NoColor initialisation not found", ColorBase)
	}
	f.Commit("Support FORCE_COLOR and CLICOLOR_FORCE", map[string]string{
		"color.go":      strings.Replace(color, oldInit, newInit, 1),
		"force.go":      colorForce,
		"force_test.go": colorForceTest,
	})
	readme := read("README.md")
	i := strings.Index(readme, "\n## ")
	if i < 0 {
		t.Fatal("README.md has no section heading")
	}
	f.Commit("README: document the fork's FORCE_COLOR support", map[string]string{
		"README.md": readme[:i] + "\n\n" + ColorForkNote + readme[i:],
	})
	f.Commit("CI: run on our self-hosted runner", map[string]string{".github/workflows/go.yml": ColorWorkflow})
	f.Push()
	c.Layout = l
	return c
}

// ColorForkNote is the fork's README addition.
const ColorForkNote = "> **Fork note:** this fork also honours `FORCE_COLOR` and `CLICOLOR_FORCE`\n> to keep colors in CI logs. `NO_COLOR` still takes precedence.\n"

// ColorInitTest checks the package-level NoColor initialisation against
// the environment the test binary was started with (FORCE_COLOR set,
// stdout not a terminal): colors are on unless E2E_WANT_NOCOLOR=1.
const ColorInitTest = `package color

import (
	"os"
	"testing"
)

func TestE2EColorInit(t *testing.T) {
	want := os.Getenv("E2E_WANT_NOCOLOR") == "1"
	if NoColor != want {
		t.Fatalf("NoColor = %v, want %v (FORCE_COLOR=%q NO_COLOR=%q)", NoColor, want, os.Getenv("FORCE_COLOR"), os.Getenv("NO_COLOR"))
	}
}
`

const colorForce = `package color

import "os"

// forceColor reports whether colors are forced on via FORCE_COLOR or
// CLICOLOR_FORCE (e.g. in CI logs). NO_COLOR always wins.
func forceColor() bool {
	if noColorExists() {
		return false
	}
	for _, k := range []string{"FORCE_COLOR", "CLICOLOR_FORCE"} {
		if v := os.Getenv(k); v != "" && v != "0" {
			return true
		}
	}
	return false
}
`

const colorForceTest = `package color

import (
	"os"
	"testing"
)

func TestForceColor(t *testing.T) {
	os.Unsetenv("NO_COLOR")
	t.Setenv("FORCE_COLOR", "1")
	if !forceColor() {
		t.Fatal("FORCE_COLOR=1 should force colors")
	}
	t.Setenv("FORCE_COLOR", "0")
	if forceColor() {
		t.Fatal("FORCE_COLOR=0 should not force colors")
	}
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "1")
	if forceColor() {
		t.Fatal("NO_COLOR must win over CLICOLOR_FORCE")
	}
}
`
