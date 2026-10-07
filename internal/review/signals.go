package review

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Signal is a cheap deterministic hint. Signals direct the model's
// attention; they never block a merge on their own. Strong signals force a
// second-stage investigation even when triage says "clean", because they are
// rare in legitimate commits and cheap to double-check.
type Signal struct {
	Kind   string `json:"kind"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail"`
	Strong bool   `json:"strong,omitempty"`
}

func (s Signal) String() string {
	strong := ""
	if s.Strong {
		strong = " (strong)"
	}
	if s.File != "" {
		return fmt.Sprintf("[%s%s] %s: %s", s.Kind, strong, s.File, s.Detail)
	}
	return fmt.Sprintf("[%s%s] %s", s.Kind, strong, s.Detail)
}

// fileDiff is one file section of a unified diff.
type fileDiff struct {
	Path  string
	Text  string
	Added []string
}

// splitPatch splits a unified diff into per-file sections.
func splitPatch(patch string) []*fileDiff {
	var out []*fileDiff
	var cur *fileDiff
	var sb strings.Builder
	inHunk := false
	flush := func() {
		if cur != nil {
			cur.Text = sb.String()
			out = append(out, cur)
		}
		sb.Reset()
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined ") {
			flush()
			cur = &fileDiff{Path: pathFromDiffHeader(strings.TrimRight(line, "\n"))}
			inHunk = false
		}
		if cur == nil {
			cur = &fileDiff{}
		}
		sb.WriteString(line)
		body := strings.TrimRight(line, "\n")
		switch {
		case !inHunk && strings.HasPrefix(body, "+++ "):
			if p := strings.TrimPrefix(body, "+++ "); p != "/dev/null" {
				cur.Path = strings.TrimPrefix(unquote(p), "b/")
			}
		case strings.HasPrefix(body, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(body, "+"):
			cur.Added = append(cur.Added, body[1:])
		}
	}
	flush()
	return out
}

func pathFromDiffHeader(h string) string {
	// diff --git a/x b/x  (best effort; +++ line refines it)
	if i := strings.LastIndex(h, " b/"); i >= 0 {
		return unquote(strings.TrimSpace(h[i+1:]))[2:]
	}
	return ""
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

var (
	bidiRe      = regexp.MustCompile("[\u202A-\u202E\u2066-\u2069]")
	invisibleRe = regexp.MustCompile("[\u200B-\u200D\u2060\u2062-\u2064\u3164\u115F\u1160\uFFA0]")
	// Phrases aimed at manipulating an automated reviewer (strong), versus
	// generic AI-addressing text that LLM-integrating projects contain
	// legitimately in their own prompts (weak).
	aiStrongRe  = regexp.MustCompile(`(?i)(ignore\s+(all\s+|any\s+)?(previous|prior|above|earlier)\s+(instructions|rules|guidelines)|disregard\s+(all\s+|the\s+)?(previous|above|prior)\s+(instructions|rules)|(dear|attention|note\s+to(\s+the)?)\s+(ai|llm|automated|code)\s+(reviewer|review|scanner|auditor)|\b(ai|llm|automated|security)\s+(code\s+)?(reviewer|scanner|auditor)s?\b.{0,60}\b(approve|mark\s+(this|it)\s+(as\s+)?(safe|clean)|ignore\s+this|skip\s+this|must\s+pass)|this\s+(commit|change|code|file)\s+(is|has\s+been)\s+(pre-?approved|verified\s+safe|security[- ]reviewed\s+and\s+approved))`)
	aiWeakRe    = regexp.MustCompile(`(?i)(you\s+are\s+an?\s+(ai|llm|large\s+language\s+model)\b|as\s+an\s+ai\s+(language\s+)?model)`)
	downloadRe  = regexp.MustCompile(`(?i)((curl|wget|fetch|iwr|invoke-webrequest)\b[^\n|]{0,300}\|\s*(sudo\s+)?(ba|z|da|k)?sh\b|(curl|wget)\b[^\n]{0,300}(&&|;)\s*(chmod\s+\+x|sh\s|bash\s|\./)|\biex\s*\(|downloadstring\s*\()`)
	evalDecRe   = regexp.MustCompile(`(?i)(eval|exec|Function|compile|system|popen|spawn)\s*\(\s*[^)]{0,80}(atob|b64decode|base64|fromCharCode|Buffer\.from|unhexlify|decode\(|rot13|gzip\.decompress|zlib\.decompress)`)
	secretsRe   = regexp.MustCompile(`(?i)(toJSON\(\s*secrets\s*\)|JSON\.stringify\(\s*process\.env\s*\)|dict\(\s*os\.environ\s*\)|os\.environ\.copy\(\)|/proc/self/environ|\.ssh/id_(rsa|ed25519|ecdsa)|\.aws/credentials|\.docker/config\.json|\.npmrc|\.pypirc|\.git-credentials|\.netrc|wallet\.dat|Local State|Login Data|keychain)`)
	urlRe       = regexp.MustCompile(`(?i)\b(https?|wss?|ftp)://[^\s"'<>\x60)\]}]+`)
	b64Re       = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)
	hexBlobRe   = regexp.MustCompile(`(?i)(\\x[0-9a-f]{2}){60,}|\b[0-9a-f]{300,}\b`)
	installKey  = regexp.MustCompile(`"(preinstall|install|postinstall|prepare|prepublish|preuninstall|postuninstall)"\s*:`)
	setupHookRe = regexp.MustCompile(`(cmdclass\s*=|class\s+\w+\((install|develop|egg_info|build_py)\)|setuptools\.command\.install)`)
	registryRe  = regexp.MustCompile(`(?i)(--(extra-)?index-url|registry\s*=|"resolved"\s*:|resolved\s+"|source\s*=\s*"|tarball:|git\+(https?|ssh)://|\breplace\s+\S+\s+=>)`)
)

var ciPatterns = []string{
	".github/workflows/*", ".github/actions/**", ".gitlab-ci.yml", ".gitlab/**", ".circleci/**", ".travis.yml",
	"azure-pipelines.yml", ".azure-pipelines/**", "Jenkinsfile", ".buildkite/**", ".drone.yml", ".woodpecker.yml",
	".woodpecker/**", ".forgejo/workflows/*", ".gitea/workflows/*", "bitbucket-pipelines.yml", "appveyor.yml",
	"cloudbuild.yaml", ".cirrus.yml",
}

var buildBasenames = map[string]bool{
	"makefile": true, "gnumakefile": true, "cmakelists.txt": true, "configure": true, "configure.ac": true,
	"configure.in": true, "meson.build": true, "build.rs": true, "build.gradle": true, "build.gradle.kts": true,
	"settings.gradle": true, "pom.xml": true, "setup.py": true, "setup.cfg": true, "pyproject.toml": true,
	"package.json": true, "dockerfile": true, "containerfile": true, "build": true, "build.bazel": true,
	"workspace": true, "module.bazel": true, "rakefile": true, "go.mod": true, "cargo.toml": true,
	"justfile": true, "taskfile.yml": true, "build.zig": true, "flake.nix": true, "default.nix": true,
	".npmrc": true, ".yarnrc": true, ".yarnrc.yml": true, "pip.conf": true, ".pypirc": true, "bunfig.toml": true,
	".gitattributes": true, ".gitmodules": true, ".pre-commit-config.yaml": true,
}

var lockBasenames = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"bun.lock": true, "bun.lockb": true, "cargo.lock": true, "go.sum": true, "poetry.lock": true,
	"uv.lock": true, "pipfile.lock": true, "composer.lock": true, "gemfile.lock": true, "flake.lock": true,
	"pdm.lock": true, "mix.lock": true, "pubspec.lock": true, "packages.lock.json": true, "gradle.lockfile": true,
}

var defaultRegistries = []string{
	"registry.npmjs.org", "registry.yarnpkg.com", "pypi.org", "files.pythonhosted.org", "crates.io",
	"static.crates.io", "index.crates.io", "github.com", "codeload.github.com", "proxy.golang.org",
	"rubygems.org", "repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "registry.bun.sh",
}

func isLockfile(p string) bool { return lockBasenames[strings.ToLower(path.Base(p))] }

func isBuildFile(p string) bool {
	b := strings.ToLower(path.Base(p))
	if buildBasenames[b] || strings.HasPrefix(b, "dockerfile") {
		return true
	}
	switch path.Ext(b) {
	case ".mk", ".cmake", ".m4", ".bzl", ".gemspec", ".gradle", ".pth":
		return true
	}
	return b == "sitecustomize.py" || b == "usercustomize.py" || strings.HasPrefix(p, ".husky/") || strings.HasPrefix(p, ".githooks/")
}

func isCI(p string) bool {
	for _, pat := range ciPatterns {
		if matchSimple(pat, p) {
			return true
		}
	}
	return false
}

func isDocFile(p string) bool {
	low := strings.ToLower(p)
	switch path.Ext(low) {
	case ".md", ".rst", ".txt", ".adoc", ".html", ".htm", ".svg", ".xml", ".json", ".po", ".pot", ".1", ".man":
		return true
	}
	return strings.HasPrefix(low, "docs/") || strings.HasPrefix(low, "doc/") || strings.Contains(low, "/docs/") ||
		strings.Contains(low, "changelog") || strings.Contains(low, "license") || strings.Contains(low, "readme")
}

// matchSimple supports "*" within a segment and a trailing "/**".
func matchSimple(pat, p string) bool {
	if strings.HasSuffix(pat, "/**") {
		return strings.HasPrefix(p, strings.TrimSuffix(pat, "**"))
	}
	ok, _ := path.Match(pat, p)
	return ok
}

// binaryKind classifies a blob from its leading bytes.
func binaryKind(head []byte) (kind string, executable bool) {
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "ELF executable/library", true
	case bytes.HasPrefix(head, []byte("MZ")):
		return "Windows PE executable", true
	case bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xce}), bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xcf}),
		bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(head, []byte{0xca, 0xfe, 0xba, 0xbe}):
		return "Mach-O executable / Java class", true
	case bytes.HasPrefix(head, []byte("\x00asm")):
		return "WebAssembly module", true
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "zip/jar archive", false
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return "gzip data", false
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		return "xz data", false
	case bytes.HasPrefix(head, []byte("BZh")):
		return "bzip2 data", false
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return "zstd data", false
	case bytes.HasPrefix(head, []byte("\x89PNG")), bytes.HasPrefix(head, []byte("GIF8")), bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		return "image", false
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "PDF", false
	}
	return "binary data", false
}

// scanAdded inspects the added lines of one file.
func scanAdded(p string, added []string) []Signal {
	var sigs []Signal
	seen := map[string]bool{}
	add := func(s Signal) {
		key := s.Kind + "|" + s.File
		if seen[key] {
			return
		}
		seen[key] = true
		sigs = append(sigs, s)
	}
	doc := isDocFile(p)
	lock := isLockfile(p)
	hosts := map[string]bool{}
	longLines := 0
	for _, l := range added {
		if bidiRe.MatchString(l) {
			add(Signal{Kind: "bidi_control", File: p, Detail: "added line contains Unicode bidirectional override/isolate characters (Trojan Source)", Strong: true})
		}
		if invisibleRe.MatchString(l) {
			add(Signal{Kind: "invisible_unicode", File: p, Detail: "added line contains zero-width or invisible filler characters"})
		}
		if m := aiStrongRe.FindString(l); m != "" {
			add(Signal{Kind: "reviewer_manipulation", File: p, Detail: fmt.Sprintf("text that appears aimed at an automated/AI reviewer: %q", clip(m, 100)), Strong: true})
		} else if m := aiWeakRe.FindString(l); m != "" {
			add(Signal{Kind: "ai_directed_text", File: p, Detail: fmt.Sprintf("text addressing an AI model (common in LLM-integrating projects): %q", clip(m, 80))})
		}
		if !doc {
			if m := downloadRe.FindString(l); m != "" {
				add(Signal{Kind: "download_exec", File: p, Detail: fmt.Sprintf("downloads and executes remote content: %q", clip(m, 120))})
			}
			if m := evalDecRe.FindString(l); m != "" {
				add(Signal{Kind: "decode_exec", File: p, Detail: fmt.Sprintf("executes decoded/obfuscated data: %q", clip(m, 120)), Strong: true})
			}
			if m := secretsRe.FindString(l); m != "" {
				strong := strings.Contains(strings.ToLower(m), "tojson(") && strings.Contains(strings.ToLower(m), "secrets")
				add(Signal{Kind: "credential_access", File: p, Detail: fmt.Sprintf("references credentials or environment dumps: %q", clip(m, 80)), Strong: strong})
			}
			if !lock && (b64Re.MatchString(l) || hexBlobRe.MatchString(l)) {
				add(Signal{Kind: "encoded_blob", File: p, Detail: "long base64/hex encoded string added"})
			}
			if utf8.RuneCountInString(l) > 1000 && !lock {
				longLines++
			}
			for _, u := range urlRe.FindAllString(l, -1) {
				if pu, err := url.Parse(strings.TrimRight(u, ".,;:")); err == nil && pu.Hostname() != "" {
					hosts[strings.ToLower(pu.Hostname())] = true
				}
			}
		}
		base := strings.ToLower(path.Base(p))
		if base == "package.json" && installKey.MatchString(l) {
			add(Signal{Kind: "install_hook", File: p, Detail: "npm lifecycle script (runs on install) added or changed: " + clip(strings.TrimSpace(l), 120)})
		}
		if base == "setup.py" && setupHookRe.MatchString(l) {
			add(Signal{Kind: "install_hook", File: p, Detail: "custom setuptools install command"})
		}
		if (lock || isBuildFile(p) || strings.HasPrefix(base, "requirements")) && registryRe.MatchString(l) {
			for _, u := range urlRe.FindAllString(l, -1) {
				if pu, err := url.Parse(u); err == nil && !knownRegistry(pu.Hostname()) {
					add(Signal{Kind: "dependency_source", File: p, Detail: "dependency resolved from a non-default host: " + pu.Hostname()})
				}
			}
		}
	}
	if longLines > 0 {
		add(Signal{Kind: "very_long_lines", File: p, Detail: fmt.Sprintf("%d added line(s) longer than 1000 characters (minified/generated/obfuscated?)", longLines)})
	}
	if len(hosts) > 0 && !doc {
		list := make([]string, 0, len(hosts))
		for h := range hosts {
			if !boringHost(h) {
				list = append(list, h)
			}
		}
		sort.Strings(list)
		if len(list) > 8 {
			list = append(list[:8], fmt.Sprintf("(+%d more)", len(list)-8))
		}
		if len(list) > 0 {
			add(Signal{Kind: "new_urls", File: p, Detail: "hosts referenced in added code: " + strings.Join(list, ", ")})
		}
	}
	base := path.Base(p)
	if strings.HasSuffix(base, ".pth") || base == "sitecustomize.py" || base == "usercustomize.py" {
		add(Signal{Kind: "startup_hook", File: p, Detail: "Python file executed automatically at interpreter startup", Strong: true})
	}
	return sigs
}

func knownRegistry(h string) bool {
	h = strings.ToLower(h)
	for _, r := range defaultRegistries {
		if h == r || strings.HasSuffix(h, "."+r) {
			return true
		}
	}
	return false
}

func boringHost(h string) bool {
	for _, b := range []string{"localhost", "127.0.0.1", "example.com", "example.org", "example.net", "schemas.xmlsoap.org", "www.w3.org", "json-schema.org", "www.apache.org", "opensource.org", "www.gnu.org"} {
		if h == b || strings.HasSuffix(h, ".example.com") {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
