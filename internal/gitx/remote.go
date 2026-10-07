package gitx

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Auth holds resolved credentials for one remote URL.
type Auth struct {
	Username   string
	Token      string
	SSHKeyFile string // private key file with 0600 permissions
	KnownHosts string
}

// env returns environment entries that apply the credentials to url only.
// Tokens travel as an http.<url>.extraHeader set through GIT_CONFIG_*
// variables: they are not written to any config file and do not appear on
// the command line.
func (a *Auth) env(url string) []string {
	if a == nil {
		return nil
	}
	var env []string
	if a.Token != "" {
		user := a.Username
		if user == "" {
			user = "x-access-token"
		}
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + a.Token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+url+".extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	if a.SSHKeyFile != "" {
		kh := a.KnownHosts
		env = append(env, "GIT_SSH_VARIANT=ssh", "GIT_SSH_COMMAND="+strings.Join([]string{
			"ssh", "-F", "/dev/null",
			"-i", shellQuote(a.SSHKeyFile),
			"-o", "IdentitiesOnly=yes",
			"-o", "IdentityAgent=none",
			"-o", "BatchMode=yes",
			"-o", "UserKnownHostsFile=" + shellQuote(kh),
			"-o", "StrictHostKeyChecking=accept-new",
		}, " "))
	}
	return env
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteKeyFile stores an SSH private key with 0600 permissions (ssh refuses
// keys readable by others, and secret mounts are often 0644).
func WriteKeyFile(dir, name, key string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
		return "", err
	}
	return p, os.Chmod(p, 0o600)
}

// Fetch fetches refspecs from url.
func (r *Repo) Fetch(ctx context.Context, url string, auth *Auth, refspecs ...string) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--prune", "--no-recurse-submodules", "--no-write-fetch-head", "--end-of-options", url}
	_, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, append(args, refspecs...)...)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", redactURL(url), err)
	}
	return nil
}

// LsRemote returns the commit id a remote ref points at ("" if absent).
func (r *Repo) LsRemote(ctx context.Context, url string, auth *Auth, ref string) (string, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, "ls-remote", "--end-of-options", url, ref)
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w", redactURL(url), err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == ref {
			return f[0], nil
		}
	}
	return "", nil
}

// RemoteHead returns the branch the remote's HEAD points at (its default
// branch), or "" if the remote does not say.
func (r *Repo) RemoteHead(ctx context.Context, url string, auth *Auth) (string, error) {
	out, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, "ls-remote", "--symref", "--end-of-options", url, "HEAD")
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w", redactURL(url), err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "ref: ")
		if !ok {
			continue
		}
		if target, name, ok := strings.Cut(rest, "\t"); ok && name == "HEAD" {
			if b, ok := strings.CutPrefix(target, "refs/heads/"); ok {
				return b, nil
			}
		}
	}
	return "", nil
}

// Push pushes sha to ref on url without force: the push fails if the remote
// branch moved since it was fetched, so concurrent human pushes are never
// clobbered.
func (r *Repo) Push(ctx context.Context, url string, auth *Auth, sha, ref string) error {
	if !IsHex(sha) || !ValidRef(ref) {
		return fmt.Errorf("invalid push %s:%s", sha, ref)
	}
	_, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, "push", "--quiet", "--porcelain", "--no-verify", "--end-of-options", url, sha+":"+ref)
	if err != nil {
		return fmt.Errorf("push %s: %w", redactURL(url), err)
	}
	return nil
}

// PushLease replaces ref with sha only if the remote ref currently equals
// expect ("" means the ref must not exist). It is used solely for VibeCI's
// own refs (the proposal branch, the state ref); fork branches only ever
// receive fast-forward pushes.
func (r *Repo) PushLease(ctx context.Context, url string, auth *Auth, sha, ref, expect string) error {
	if !IsHex(sha) || !ValidRef(ref) || (expect != "" && !IsHex(expect)) {
		return fmt.Errorf("invalid push %s:%s", sha, ref)
	}
	_, err := r.G.Run(ctx, Opts{GitDir: r.GitDir, Env: auth.env(url)}, "push", "--quiet", "--porcelain", "--no-verify",
		"--force-with-lease="+ref+":"+expect, "--end-of-options", url, sha+":"+ref)
	if err != nil {
		return fmt.Errorf("push %s: %w", redactURL(url), err)
	}
	return nil
}

// redactURL strips userinfo from URLs for logs.
func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 && at < strings.IndexAny(rest+"/", "/") {
			return u[:i+3] + "***@" + rest[at+1:]
		}
	}
	return u
}
