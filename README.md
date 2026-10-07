# VibeCI

VibeCI keeps a fork merged with its upstream with no human in the loop.
Every cycle it fetches upstream, has LLMs review each new upstream commit
for blatant malice, merges the rest into the fork (an LLM agent in a
sandbox resolves conflicts and repairs the build), verifies the result in a
clean sandbox and pushes a merge commit. Commits the review blocks are kept
out of the fork automatically. Anything that needs attention is reported to
webhooks.

It also maintains patch-based forks — a series of patch files plus a pinned
upstream version, such as ungoogled-chromium or Brave: it moves the pin to
new upstream releases and has an agent rewrite the patches that no longer
apply. See [Patch-mode forks](#patch-mode-forks).

It is one Go binary, `vibeci` (standard library only), deployed as two
containers with Docker Compose or run on a schedule as a
[GitHub Action](#run-as-a-github-action).

This file is for whoever deploys and operates VibeCI — usually an agent.
[AGENTS.md](AGENTS.md) is for working on VibeCI's code.

## At a glance

| | |
|---|---|
| Commands | `vibeci help`; every command takes `-h` and `-config` |
| Config | JSONC. `-config` (repeatable: files are merged in order), else `$VIBECI_CONFIG` (a `:`-separated list), else `/etc/vibeci/config.jsonc`. Annotated example: [`deploy/config.example.jsonc`](deploy/config.example.jsonc). Effective values: `vibeci config` |
| Deploy | [Docker Compose](#deploy-with-docker-compose) (a daemon) or [GitHub Action](#run-as-a-github-action) (scheduled workflow runs) |
| Releases | binaries for linux and darwin, amd64 and arm64, with `SHA256SUMS` and build provenance (`gh attestation verify FILE --repo vibeci/vibeci`); image `ghcr.io/vibeci/vibeci:<version>`; `go install github.com/vibeci/vibeci/cmd/vibeci@<version>` |
| Machine-readable output | `-json` on `run`, `review`, `status`, `allow`, `exclude`, `release`, `check`, `patches` (stdout; logs go to stderr) |
| Exit codes | `0` ok · `1` failure or error · `2` usage error · `3` attention needed (`run`, `review`) |
| Requirements | git ≥ 2.38, an LLM provider, Docker Engine ≥ 26 for sandboxes |
| State | `data_dir` (default `/data`); see [Files and state](#files-and-state) |

## What one sync does

For a fork that merges upstream (the default; patch-mode forks:
[below](#patch-mode-forks)):

1. Locks the repo (a second process gets status `locked`). With
   `state_ref`, merges the state saved in the fork into the local state;
   if that fails the status is `error` and nothing else happens.
2. Fetches the fork branch and upstream — a branch, or the newest tag
   matching `upstream.tags` — into a private mirror.
3. Stops with status `held` (alert `rewritten`) if upstream rewrote history
   the fork already merged, unless `on_upstream_rewrite` is `continue`.
4. Reviews every upstream commit the fork does not have: deterministic
   signals, then a batched LLM triage of the diffs, then an investigator
   agent with read-only git tools for anything flagged. Verdicts (`clean`,
   `suspicious`, `malicious`, with a confidence) are cached per commit. A
   commit without a verdict is never merged.
5. Blocks commits whose verdict reaches `review.block_on` (default
   `malicious`) with at least `review.min_confidence` (default 0.7); see
   [Blocked commits](#blocked-commits). A `suspicious` verdict below the
   bar only raises a `suspicious` alert.
6. Merges: a fast-forward when the fork has no changes of its own, else an
   in-object-store `git merge-tree`. On conflicts, or when the clean merge
   fails the checks, the merge agent works in a sandbox (shell, file tools,
   `run_checks`, `submit`), escalating through the `resolve` models. Its
   result is imported through the harness's own git, every line it wrote
   that is in neither parent is reviewed by the `audit` model, and
   `keep_ours` paths keep the fork's version.
7. Verifies in a fresh sandbox: `prefetch` commands (may have network, e.g.
   module downloads), then the offline `verify` commands.
8. Pushes a merge commit — never a rebase, never a force push of the fork
   branch. If the fork moved meanwhile the status is `raced` and the next
   cycle retries.
9. With `state_ref`, saves the state to the fork; if that fails the
   outcome has `state_error` (exit 1) and the next run elsewhere starts
   from the state saved before.

Every commit VibeCI makes has a `VibeCI-Job: <job id>` trailer; exclusions
add `VibeCI-Excluded: <sha>`, restores `VibeCI-Restored: <sha>`. If the
state loses an exclusion (a new data directory, a deleted state ref), the
next sync recovers it from these trailers on the fork branch (`source:
"history"`), so excluded content stays quarantined.

## Blocked commits

`review.on_blocked` decides what a blocked commit does to the sync:

- **`exclude`** (default): the fork still gets every other upstream commit.
  The merge reverts the blocked commit's changes on the upstream side, so
  the commit counts as merged and is not offered again, and its distinctive
  content (added lines that existed nowhere before it, new binary files) is
  quarantined: no later merge — clean or agent-written — may bring it back.
  An excluded merge commit is reverted relative to its first parent, which
  excludes everything it merged. The alert is `blocked`, severity
  `critical`, titled `… blocked as malicious (97%) and excluded`; the
  outcome lists the commit under `excluded`, `vibeci status` under
  excluded upstream commits. Nothing needs to be done.
- **`hold`**: merging stops right before the blocked commit (status
  `blocked`, exit code 3) until it is allowed or excluded.

Root commits (no parent) cannot be excluded, so they always hold.

Operator decisions take effect on the next sync (daemon: the next cycle, or
`SIGUSR1` now):

| Command | Use it when | Effect |
|---|---|---|
| `vibeci allow [-repo R] [-reason T] SHA...` | the block is a false positive | a pending or held commit is let through; an excluded commit is re-applied by a `Restore upstream commit …` merge |
| `vibeci exclude [-repo R] [-reason T] SHA...` | you want an upstream commit out although the review passed it | a pending commit is left out of future merges; one the fork already has is removed by a `Remove upstream commit …` merge; its content is quarantined from then on |
| `review.allow_commits` in config | a permanent allow | like `allow` |

Commit ids may be abbreviated (at least 7 hex characters, unambiguous).
`exclude` needs a synced mirror (with `state_ref` it fetches one) and a
commit that is part of upstream's fetched history (a VibeCI merge commit is
not); before the first sync, `allow` accepts only full 40- or 64-character
ids.

Exclusion removes code from the fork's tree; it cannot undo effects outside
git, e.g. of a build that ran before a commit was excluded.

## Patch-mode forks

A patch-mode fork does not contain upstream's history. It holds patch files
and a file that pins the upstream version they apply to: ungoogled-chromium,
Brave's brave-core, distribution packaging. Upstream must tag its releases.
`repos[].patches` switches a repo to patch mode. One sync:

1. Locks the repo, fetches the fork branch and reads the pinned version
   from `patches.version_file` (the first group of `patches.version_regex`,
   default the whole file).
2. Picks the version to follow: the one `upstream.version_url` returns (the
   first group of `upstream.version_regex`, default the whole response), or
   the newest among the tags matching `upstream.tags` (compared like
   `sort -V`; release candidates only if the glob contains `-`).
   `upstream.tag_format` maps versions to tags (default `{version}`, e.g.
   `v{version}`). If it is not newer than the pin, the status is
   `up-to-date`.
3. Fetches the new version by tag (the pinned one too, when the agent or a
   binary check needs it), shallow and by default partial: commits and
   trees, files only as they are read (`upstream.fetch: "full"` for
   servers that are slow at single files).
   Patches that touch a repository embedded in upstream's tree (Chromium's
   DEPS dependencies such as `v8`) need a `patches.sources` entry for it;
   its commit is the submodule entry at that path, or the first group of
   `revision_regex` in `revision_file`. `vibeci patches` names missing
   sources.
4. Applies the patches in order to the new version:

   | Status | The patch | Result |
   |---|---|---|
   | `exact` | applies at its recorded lines | kept byte for byte |
   | `shifted` | applies at other lines | line numbers updated (`keep_offsets`: kept as they are) |
   | `refreshed` | applies only with `fuzz`, or upstream contains part of it | regenerated from the result |
   | `dropped` | upstream contains all of it | removed with its series entry (`drop_upstreamed`, default true; false makes it a failure) |
   | `agent` | does not apply | rewritten by the patch agent |

   Text outside hunks (descriptions, mail headers, diffstats) is kept
   verbatim.
5. The patch agent works in a sandbox (`sandbox.profile`) on a git
   repository of the patch's files, with refs `old` (pinned version),
   `old-patched` (with the patch) and `new`, plus `.vibeci/patch.diff`,
   `.vibeci/rejects.diff` and tools that read and search the whole new
   upstream tree. It ports what the patch does and submits the files,
   drops the patch if upstream now does the same, or gives up; the
   `resolve` models escalate as for merges. The harness rebuilds the patch
   from the submitted files. The `audit` model reviews every line the agent
   wrote that is in neither the original patch nor upstream, and every
   upstream line it deletes that the original did not (at most
   `resolve.max_novel_lines`); a verdict that reaches `review.block_on`
   fails the update.
6. Re-applies the whole new series strictly and commits on top of the fork
   branch (one parent): changed patches, series files without dropped
   patches, the new pin and `patches.update_files` (`{version}` in their
   content becomes the new version). Message: `Update upstream to <new>
   (from <old>)`, counts, the agent's summaries, trailers `VibeCI-Job` and
   `VibeCI-Upstream: <version>`.
7. With `verify` configured: runs `prefetch`, then `verify` in a fresh
   sandbox whose working directory holds `fork/` (the fork at the new
   commit), `upstream/` and `patched/` (the files the patches touch,
   without and with the patches; `patches.verify_tree: "full"` copies the
   whole tree, for small projects only), with `VIBECI_UPSTREAM_VERSION` and
   `VIBECI_PREVIOUS_VERSION` set.
8. Pushes as `push` says, then deletes the upstream copies under
   `sources/`.

A sync moves straight to the target version, however many releases lie in
between. Other differences from merge mode:

- Upstream's releases are trusted: no upstream commit is reviewed,
  `review.enabled` must stay unset or false, and `review`, `allow` and
  `exclude` refuse patch-mode repos. `review.block_on` and
  `review.min_confidence` still apply to the audit.
- `keep_ours` does not apply; the fork's files are its own.
- If the tag of a version VibeCI moved the fork to later points at another
  commit, the repo is held (alert `rewritten`) until `vibeci release`.
- Patches are unified diffs as GNU patch and quilt read them: quilt
  series (with `-pN` per line), `git diff`, `git format-patch`; renames,
  copies, mode changes, created and deleted files. A binary section is kept
  while its file is unchanged upstream; otherwise the update fails, because
  VibeCI cannot rewrite binary patches. `patched/` lacks binary changes.

`vibeci patches -repo R [-version V]` shows how the patches apply to the
version a sync would move to (or `V`) without the agent: a cheap first look
at a new release.

ungoogled-chromium, following Chrome's stable channel:

```jsonc
{
  "name": "ungoogled-chromium",
  "description": "Chromium without Google integration: the patches remove Google services and background requests and add privacy options.",
  "fork": {"url": "https://github.com/me/ungoogled-chromium.git", "branch": "master",
           "auth": {"token": "file:/run/secrets/git_token"}},
  "upstream": {
    "url": "https://github.com/chromium/chromium.git", // serves single files of a partial clone quickly
    "version_url": "https://versionhistory.googleapis.com/v1/chrome/platforms/linux/channels/stable/versions?pageSize=1",
    "version_regex": "\"version\": \"([0-9.]+)\""
  },
  "patches": {
    "series": "patches/series",
    "version_file": "chromium_version.txt",
    "update_files": {"revision.txt": "1\n"},
    "ignore_whitespace": true, // the project applies its patches with patch --ignore-whitespace
    "sources": [
      {"path": "v8", "url": "https://github.com/v8/v8.git"},
      {"path": "third_party/devtools-frontend/src", "url": "https://github.com/ChromeDevTools/devtools-frontend.git"},
      {"path": "third_party/search_engines_data/resources", "fetch": "full",
       "url": "https://chromium.googlesource.com/external/search_engines_data.git"}
    ]
  }
}
```

Checking the whole ungoogled-chromium series against a new Chromium release
downloads tens of megabytes, not Chromium's tens of gigabytes. brave-core
pins the version in `package.json` and keeps the patches for an embedded
repository relative to it, one directory each:

```jsonc
"patches": {
  "version_file": "package.json",
  "version_regex": "\"tag\": \"([0-9.]+)\"",
  "sets": [
    {"glob": "patches/*.patch"},
    {"glob": "patches/v8/*.patch", "root": "v8"},
    {"glob": "patches/third_party/ffmpeg/*.patch", "root": "third_party/ffmpeg"}
    // … one set per directory under patches/, and a source for each root
  ],
  "sources": [{"path": "v8", "url": "https://github.com/v8/v8.git"} /* , … */]
}
```

## Deploy with Docker Compose

Two containers run from one image. `vibeci` holds the LLM key and git
credentials, has network access and no Docker access; it asks `sandboxd`
for sandboxes by profile name. `sandboxd` is the only container with the
Docker socket; it has no network and no credentials and applies a fixed
policy to every sandbox: no capabilities, read-only root filesystem,
non-root user, resource limits, network only if the profile names one, and
only the job's own directory mounted from the data volume.

```sh
cd deploy
cp config.example.jsonc config.jsonc       # providers, models, repos
cp sandboxd.example.jsonc sandboxd.jsonc   # sandbox profiles: images, limits, networks
mkdir -p secrets && chmod 700 secrets
printf %s "$LLM_API_KEY" > secrets/llm_api_key
printf %s "$GIT_TOKEN"   > secrets/git_token
chmod 0644 secrets/*                       # read by uid 10001 in the container
# only for profiles with network egress (e.g. dependency prefetch):
docker network create -o com.docker.network.bridge.enable_icc=false vibeci-egress
docker compose --profile images build      # sandbox images
docker compose up -d --build
docker compose exec vibeci vibeci check    # validates config, credentials, models, remotes, sandboxes
```

- `VIBECI_IMAGE` (default `vibeci:latest`) and `VIBECI_DATA_VOLUME`
  (default `vibeci-data`; must equal `data_volume` in `sandboxd.jsonc`)
  can be set in the environment of `docker compose`. To run a released
  image instead of a local build: `export
  VIBECI_IMAGE=ghcr.io/vibeci/vibeci:<version>`, `docker compose pull
  sandboxd vibeci`, then `docker compose up -d` without `--build` (the
  sandbox images are always built locally).
- The daemon syncs at startup and then every `interval`. `SIGUSR1` starts a
  cycle now, `SIGHUP` reloads the config:
  `docker compose kill -s SIGUSR1 vibeci`.
- Run other commands inside the container:
  `docker compose exec vibeci vibeci status`.
- The container healthcheck is `vibeci health`.
- Sandbox images: [`deploy/sandbox/base.Dockerfile`](deploy/sandbox/base.Dockerfile)
  (git and shell tools) and [`go.Dockerfile`](deploy/sandbox/go.Dockerfile).
  For another toolchain build a derived image, add a profile to
  `sandboxd.jsonc` and name it in the repo's `sandbox` settings.

Without Compose, `vibeci daemon` also runs directly on a host with
`"sandbox": {"mode": "docker", "docker": {...}}` (the harness then talks to
the Docker engine itself, so it is no longer isolated from it) or
`"mode": "unsafe-local"` (no isolation at all; for tests only).

## Run as a GitHub Action

[`action.yml`](action.yml) runs one VibeCI command per workflow run, in the
fork's own repository, on a GitHub-hosted Linux runner. It builds `vibeci`
and the sandbox images from the action's source (a few minutes), composes
the config, runs the command with Docker sandboxes and reports in the job
summary. A runner starts empty, so the state lives in the fork, under
`state_ref` ([Files and state](#files-and-state)).

1. Commit `.github/vibeci.jsonc` to the fork: a config whose repo has no
   `fork` (the action makes this repository the fork, at its default
   branch, named after it) and no providers:

   ```jsonc
   {
     "repos": [{
       "upstream": {"url": "https://github.com/someone/project.git", "branch": "main"},
       "description": "What the fork changes and why.",
       "keep_ours": [".github/**"],
       "sandbox": {"profile": "go", "prefetch_profile": "go-egress"},
       "prefetch": [{"name": "modules", "run": "go mod download"}],
       "verify": [{"name": "build", "run": "go build ./..."}, {"name": "test", "run": "go test ./..."}]
     }],
     "alerts": [{"type": "ntfy", "url": "env:NTFY_URL"}]
   }
   ```

2. Add repository secrets: `VIBECI_TOKEN`, a token that may push to the
   fork and change its workflows ([Tokens for GitHub](#tokens-for-github)),
   and `VIBECI_MODELS`, the providers, models and roles as JSONC:

   ```jsonc
   {"providers": {"anthropic": {"type": "anthropic", "api_key": "sk-ant-..."}},
    "models": {"strong": {"provider": "anthropic", "model": "claude-opus-4-1", "max_tokens": 32000, "thinking": "adaptive", "effort": "high"}},
    "roles": {"triage": "strong", "investigate": "strong", "audit": "strong", "resolve": ["strong"]}}
   ```

3. Add `.github/workflows/vibeci.yml`, with the action pinned by commit:

   ```yaml
   name: VibeCI
   on:
     schedule:
       - cron: "23 */6 * * *"
     workflow_dispatch:
       inputs:
         command:
           description: run, review, status, allow, exclude, release, check, patches or config
           default: run
         args:
           description: arguments, e.g. -dry-run or -reason "false positive" 1a2b3c4d5e
           default: ""
   permissions:
     contents: read
   concurrency:
     group: vibeci
   jobs:
     vibeci:
       runs-on: ubuntu-latest
       timeout-minutes: 300
       steps:
         - uses: vibeci/vibeci@<commit> # v0.1.0
           env:
             NTFY_URL: ${{ secrets.NTFY_URL }}   # for env: references in the config
           with:
             models: ${{ secrets.VIBECI_MODELS }}
             token: ${{ secrets.VIBECI_TOKEN }}
             command: ${{ inputs.command || 'run' }}
             args: ${{ inputs.args }}
   ```

4. Run the workflow (Actions → VibeCI → Run workflow) with command `check`,
   then `run` with args `-dry-run`.

`concurrency` keeps runs from overlapping; runs that overlap anyway (another
workflow, a daemon on the same fork) merge their state rather than lose it.
Operator commands (`allow`, `exclude`, `release`) are workflow runs too; the
next scheduled run applies them.

| Input | Default | Meaning |
|---|---|---|
| `config` | `.github/vibeci.jsonc` | config files, one per line, merged in order; relative to the repository root, read from the workflow's commit when not checked out (no `actions/checkout` needed) |
| `models` | | JSONC merged after the config files; its base URLs, hosts, header values, inline API keys and model ids are masked in the log and the summary |
| `token` | `github.token` | the fork's token (`fork.auth.token` of the repo without `fork.url`; also reads the config files) |
| `command`, `args` | `run`, none | the command and its arguments, split like a shell does; `-json` is added (not for `config`) |
| `mask` | | more strings to mask, one per line |
| `images` | `base go` | sandbox images to build |

Outputs: `status` (`run`: the outcome status needing the most attention;
other commands: `ok`, `attention` or `error`), `commit` (pushed, or
proposed by a dry run), `json` (the command's `-json` output) and `records`
(the job records directory on the runner). The step's exit status is the
command's; failures and blocks are also annotations.

The action starts from these settings, which the config files override:
`data_dir` under `$RUNNER_TEMP`, `state_ref` `refs/vibeci/state`, and
`sandbox` `docker` with profiles `default` (image `vibeci-sandbox:base`),
`go` (`vibeci-sandbox:go`, offline: `GOPROXY=off`, caches under `/cache`)
and `default-egress`, `go-egress` (the same with network, for `prefetch`).
Each sandbox gets 4 GB memory and 2 CPUs unless the profile says otherwise.

Limits:

- Linux runners with Docker only (GitHub-hosted `ubuntu-*`, or self-hosted
  ones whose Docker engine VibeCI may use); not inside a job `container:`.
- Every run fetches the fork and upstream anew; nothing is cached between
  runs. The runner's disk and the job time limit (6 h on GitHub-hosted
  runners) bound the project size; deploy with
  [Docker Compose](#deploy-with-docker-compose) beyond that.
- Job records (agent transcripts) stay on the runner. To keep them, upload
  `${{ steps.<id>.outputs.records }}` with `actions/upload-artifact`; they
  hold upstream code and model output, and a public repository's artifacts
  are public.
- A public repository's logs and summaries are public. Provider errors can
  still name the provider; add such strings to `mask`.
- GitHub runs no workflows in a repository made with the Fork button until
  they are enabled in its Actions tab, and disables the scheduled workflows
  of a public repository after 60 days without activity.

## Configuration

Read [`deploy/config.example.jsonc`](deploy/config.example.jsonc) (harness)
and [`deploy/sandboxd.example.jsonc`](deploy/sandboxd.example.jsonc)
(broker); `vibeci config` prints the effective configuration with defaults
applied. Secrets are `env:NAME`, `file:/path` or a literal; `vibeci config`
shows literals as `"[redacted]"`.

Several `-config` files are merged in order: objects key by key, anything
else (arrays included) replaced by the later file; an unknown key is an
error naming its file. A file of providers and keys can thus stay apart
from the rest. Write secrets in their string form in such overlays: the
object form merges key by key.

| Section | Contents |
|---|---|
| top level | `data_dir` (`/data`), `interval` (`30m`), `repo_timeout` (`4h`), `keep_jobs` (`168h`), `max_parallel` (`1`), `log_level` (`info`; `VIBECI_LOG_FORMAT=json` for JSON logs), `identity` (merge commit author), `state_ref` (a ref such as `refs/vibeci/state`: also keep each repo's state in its fork; see [Files and state](#files-and-state)) |
| `providers` | `type`: `anthropic` (Messages API), `openai-responses` or `openai-chat` (Chat Completions); `base_url` (any compatible gateway or server), `auth` (`anthropic`: `x-api-key` or `bearer`), `api_key`, `headers`, `timeout`, `max_retries`, `stream`, `max_tokens_field` |
| `models` | `provider`, `model`, `max_tokens`, `thinking`, `effort`, … |
| `roles` | `triage`, `investigate`, `audit`: a model name each; `resolve`: a list, the merge agent's escalation ladder. Per repo: `repos[].roles` |
| `sandbox` | `mode`: `broker` (`socket`), `docker` (`docker`: same keys as `sandboxd.jsonc`) or `unsafe-local` |
| `alerts` | webhooks: `type` (`ntfy`, `discord`, `slack`, `generic`), `url`, `headers`, `events` |
| `repos[]` | see below |

Per repo:

| Key | Meaning |
|---|---|
| `name` | identifier used by `-repo`, state and alerts |
| `fork` | `url`, `branch`, `auth` (`username`, `token`, `ssh_key`, `known_hosts`). The token needs write access; on GitHub also the right to change workflows ([below](#tokens-for-github)) |
| `upstream` | `url` plus `branch`, or `tags` (a glob such as `v*`: follow the newest matching release). Patch mode: `tags` or `version_url` (+ `version_regex`), `tag_format` (`{version}`), `fetch` (`partial` or `full`) |
| `patches` | makes the repo a [patch-mode fork](#patch-mode-forks): `series` (quilt series file) or `glob`, with `strip` (1) and `root`, or `sets` of those; `version_file`, `version_regex`, `update_files`, `sources` (`path`, `url`, `auth`, `fetch`, `revision_file`, `revision_regex`), `fuzz` (0, max 3), `ignore_whitespace`, `drop_upstreamed` (true), `keep_offsets`, `verify_tree` (`touched` or `full`) |
| `description` | what the fork changes and why — the merge and patch agents preserve it |
| `keep_ours` | globs where the fork's version always wins; include `.github/workflows/**` (not in patch mode) |
| `sandbox` | `profile` (agent), `verify_profile`, `prefetch_profile` |
| `prefetch`, `verify` | commands `{name, run, timeout}`; verify runs offline |
| `review` | `block_on` (`malicious` or `suspicious`), `min_confidence`, `on_blocked` (`exclude` or `hold`), `allow_commits`, `max_commits_per_run` (250), `enabled` |
| `resolve` | `max_rounds` (4), `max_turns` (150), `timeout`, `max_novel_lines` (3000), `command_timeout`; also for the patch agent |
| `push` | `mode`: `direct`, or `branch` with `branch` (merges go to a branch you merge yourself; VibeCI keeps your fixups there); `dry_run` |
| `on_upstream_rewrite` | `hold` (default) or `continue` |

Every role is security- or correctness-critical: configure the most capable
models available.

### Tokens for GitHub

A merge pushes upstream's commits along with the merge commit. When one of
them changes `.github/workflows`, GitHub refuses the push (`refusing to allow
… to create or update workflow …`; status `error`) unless the token may
change workflows — even when `keep_ours` keeps the fork's workflows. Use
one of:

- a fine-grained personal access token for the fork with Contents and
  Workflows read and write;
- a GitHub App installation token with the same permissions;
- a classic personal access token with the `repo` and `workflow` scopes.

The `GITHUB_TOKEN` of a workflow run cannot be given that permission, and
its pushes start no workflows, so the fork's CI would not run on VibeCI's
merges. If the fork branch is protected, the token's user or app must be
allowed to push to it, or use `push.mode` `branch`.

## CLI reference

| Command | Flags | Output and exit status |
|---|---|---|
| `daemon` | | sync every `interval`; signals as above |
| `run` | `-repo`, `-dry-run`, `-force` (ignore backoff and cached dry runs), `-json` | one outcome per repo; exit 1 if any `failed`/`error` or has a `state_error`, else 3 if any needs attention |
| `review` | `-repo`, `-n`, `-json` | review report without merging, pushing or alerting; exit 3 if a commit blocks merging |
| `status` | `-repo`, `-json` | stored state of each repo and the daemon heartbeat |
| `allow`, `exclude` | `-repo`, `-reason`, `-json`, commit ids | `{"repo", "changes": [{"commit", "effect", "detail"}]}` |
| `release` | `-repo`, `-json` | clears an upstream-rewrite hold and the failure backoff: `{"repo", "released": true}` |
| `check` | `-json`, `-no-llm`, `-no-sandbox` | `{"ok", "checks": [{"name", "ok", "detail", "error"}]}`; exit 1 if any check fails. With `state_ref`, `<repo> state` shows the ref's commit; patch mode adds `<repo> upstream` (the version a sync would move to) and `<repo> source <path>` checks |
| `patches` | `-repo`, `-version`, `-json` | how a patch-mode fork's patches apply to upstream version `-version` (default: the one a sync would move to), without the agent; exit 0 whatever their status |
| `config` | | effective configuration as JSON |
| `action` | inputs in `VIBECI_ACTION_*` (`vibeci action -h`) | runs one command as a GitHub Actions step ([Run as a GitHub Action](#run-as-a-github-action)); exit status the command's |
| `health` | | exit 0 if the daemon finished a cycle within 2 × `interval` + `repo_timeout` |
| `sandboxd` | `-config` (`$VIBECI_SANDBOXD_CONFIG` or `/etc/vibeci/sandboxd.jsonc`) | the sandbox broker |
| `version` | | `vibeci <version>` |

`-repo` may be omitted when only one repo is configured (`run` and
`status` default to all repos). `-config` may be repeated
([Configuration](#configuration)).

**`run -json`** prints an array of outcomes:

```json
[{"repo": "myproject", "status": "synced",
  "message": "pushed 2d8064a1b9 to main: merge of upstream main; 2 conflicted file(s) resolved by strong in 7 turns; excluded 1 upstream commit(s): c3cb66befe; checks passed (build passed, test passed)",
  "job_id": "myproject-20261005-033732-57bb14", "fork_head": "…", "upstream": "…", "target": "…", "commit": "…",
  "excluded": ["c3cb66befe…"], "merges": 1,
  "usage": {"input_tokens": 56378, "output_tokens": 2535, "cache_read_tokens": 30210, "calls": 8}, "duration": "1m1s"}]
```

Empty fields are omitted: `blocked` (commits that stopped the merge),
`excluded`, `restored`, `pending` (upstream commits left for the next
cycle), `merges`, `state_error` (the sync's result could not be saved to
`state_ref`; exit 1). Patch-mode outcomes add `pinned` (the version the fork
pinned before), `version` (the version upstream offers), and after an
update `patches`: `{"total", "exact", "shifted", "refreshed", "agent",
"dropped"}`; `upstream` and `target` are the tag's commit.

| `status` | Meaning | Attention (exit 3) |
|---|---|---|
| `up-to-date` | the fork contains upstream (patch mode: pins upstream's version or a newer one) | |
| `synced` | merged (patch mode: moved to the new version) and pushed (`commit`); `pending` > 0: more upstream commits remain for the next cycle | |
| `dry-run` | merge computed and verified, not pushed | |
| `blocked` | the next upstream commit is blocked and was not excluded; nothing merged | yes |
| `held` | upstream rewrote merged history; nothing merged until `vibeci release` | yes |
| `failed` | the merge could not be completed | exit 1 |
| `error` | infrastructure problem (network, LLM API, sandbox, git) | exit 1 |
| `backoff` | skipped: waiting after earlier failures | |
| `raced` | the fork moved during the sync; retried next cycle | |
| `locked` | another vibeci process is syncing this repo | |
| `disabled` | `enabled: false` | |

**`review -json`**: `{"repo", "base", "upstream", "label", "total",
"limited", "safe_target", "first_blocked", "excluded": [...], "verdicts":
[{"commit", "verdict", "confidence", "summary", "findings", "signals",
"stage", "model", "version", "time"}], "usage"}`. `safe_target` is the
newest upstream commit a sync would merge; `excluded` the commits it would
leave out; `first_blocked` the commit that stops it.

**`patches -json`**: `{"repo", "fork_head", "tag", "upstream", "pinned",
"version", "counts": {"<status>": n}, "patches": [{"name", "path",
"status", "hunks", "failed": [{"file", "hunk", "line", "near", "matched",
"total", "problem"}], "problem"}]}`. `status` is `exact`, `shifted`,
`refreshed`, `dropped` or `failed`; `upstream` is the tag's commit. A failed
hunk was expected at `line`; `near` is the closest match (`matched` of its
`total` old lines). `problem` explains failures that are not about lines,
e.g. a file that no longer exists.

**`status -json`**: `{"repos": {"<name>": {...}}, "heartbeat": {"time",
"interval", "repos": {"<name>": "<status>"}} or null}`. Per repo, among
others: `last_result`, `last_message`, `last_success`, `last_merged`,
`last_job`, `failure`, `blocked`, `rewrite_hold`, `usage`, `allowed`, and
`excluded`: `{"<sha>": {"commit", "source": "review"|"manual"|"history",
"verdict", "confidence", "summary", "since", "removed", "restore"}}` —
`history`: recovered from the trailers of VibeCI's merges on the fork
branch; `removed` is true once the fork no longer contains the commit's
changes, `restore` asks the next sync to re-apply it. Patch mode adds `pinned_version` (the fork's
pin as of the last sync) and `upstream_version` (the version upstream
offered).

**`allow`/`exclude` effects**: `allowed`, `restore-scheduled`,
`exclusion-cancelled` (allowed before it was removed), `excluded`,
`restore-cancelled`, `unchanged`.

Errors go to stderr as `vibeci: <message>` with exit 1, for example
`no repo named "x" (configured: …)`, `"zzz" is not a commit id (7 to 64 hex
characters)`, `"abc1234" is ambiguous`, `the repo has not been synced yet
(no mirror); …`, `repo X is locked by another vibeci process` (a sync is
running; retry shortly), `review does not apply to X: it is a patch-mode
fork …` (likewise `allow`, `exclude`), `repo X is not a patch-mode fork
(repos[].patches is not set)` (`patches`) and `loading the state from
refs/vibeci/state in the fork: …` (`state_ref`; nothing was changed).

## Alerts

Default events: `blocked`, `suspicious`, `failed`, `rewritten`, `error`,
`recovered`; add `synced` (or `"*"`) to hear about every merge. A
`generic` webhook receives the event as JSON:

```json
{"kind": "blocked", "severity": "critical", "repo": "myproject",
 "title": "myproject: upstream commit c3cb66befe blocked as malicious (99%) and excluded",
 "message": "…; if this is a false positive: vibeci allow -repo myproject c3cb66befe…",
 "commits": ["c3cb66befe…"], "job_id": "…", "time": "…", "details": {}}
```

`failed` is sent once per failing (fork head, upstream) pair, `error` after
three consecutive infrastructure errors, `recovered` when an alerted
problem clears. In patch mode, `synced` is titled `<repo>: updated to
upstream <version>` with `details` `version`, `from_version`, `tag`,
`upstream_commit`, `patches` (counts), `rewritten`, `dropped`,
`agent_models` and `checks`; `failed` is titled `… could not update to
upstream <version>`; an audit rejection is a `blocked` alert titled `…
patch agent output rejected by the security audit`.

## Operating playbook

- **Routine**: `vibeci status -json` and `vibeci health`.
- **`blocked … and excluded`**: nothing to do; the fork keeps syncing
  without the commit. If the verdict is wrong (read the summary, findings
  and the job transcript), `vibeci allow -repo R SHA`.
- **`blocked`** in hold mode, or a root commit: decide with `vibeci allow`
  or `vibeci exclude`; until then the fork stays before that commit.
- **`suspicious`**: merged; worth a look.
- **`failed`**: the agent gave up, the checks fail, or the audit rejected
  the result. VibeCI backs off (doubling up to 24 h) and retries at once
  when the fork or upstream changes, unless three different attempts failed
  in a row. Read the job transcript, then fix the fork by hand, improve the
  repo's `description`/`verify`/`resolve` settings, or wait for upstream.
  `vibeci release -repo R` clears the backoff; `vibeci run -repo R -force`
  retries now.
- **`error`**: run `vibeci check`; VibeCI retries with backoff (up to 4 h).
- **`rewritten`** / status `held`: upstream force-pushed history the fork
  had merged. Check that the new history is legitimate, then
  `vibeci release -repo R`.
- **`failed` in patch mode** (`could not update to upstream V`): `vibeci
  patches -repo R -version V` shows which patches do not apply and where.
  Fix them in the fork by hand (an updated pin in the fork's own commit
  works too: the next sync starts from it) or improve `description`; the
  usual backoff applies. A patch with a binary change to a file upstream
  changed always needs a human.
- **`raced`**, **`locked`**: transient.
- **Preview** a sync: `vibeci run -repo R -dry-run -json`; review only:
  `vibeci review -repo R -json`.

## Files and state

Under `data_dir`:

| Path | Contents |
|---|---|
| `mirrors/<repo>.git` | bare mirror of fork and upstream (patch mode: the fork); VibeCI's own refs under `refs/vibeci/`, with `state_ref` also `state-fetched` (the fork's copy of the state as last fetched) and `state-synced` (the copy last merged or pushed: the base of the next merge) |
| `state/repos/<repo>.json` | per-repo state (what `status -json` shows) |
| `state/reviews/<repo>/<sha>.json` | cached review verdicts |
| `state/locks/`, `state/heartbeat.json` | repo locks, daemon heartbeat |
| `jobs/<job id>/` | merge job records (agent transcripts, logs); workspaces are deleted when the job ends, records after `keep_jobs` |
| `sources/<repo>/` | patch mode: partial clones of upstream and `patches.sources`; deleted after each pushed update |
| `home/` | git's `HOME` |

State is plain JSON; stop the daemon before editing it by hand.

With `state_ref` (the GitHub Action sets `refs/vibeci/state`), each repo's
state is also kept in its fork, as a commit without parents under that ref
holding `repo.json` (what `status -json` shows for the repo) and
`reviews/<xx>/<sha>.json` (the cached verdicts of upstream commits the fork
does not contain yet). Commands merge the fork's copy into the local state
before they read it and push the result back, with a lease, after they
change it; concurrent writers are merged, the fork's copy winning
conflicting fields. A missing ref is created from the local state by the
next save. To start over, delete it (`git push origin
:refs/vibeci/state`): the next sync recovers exclusions from history and
reviews the pending upstream commits again. Who may change the ref:
[Security model](#security-model).

## Security model

- Upstream content and the merge agent's output are untrusted. The review
  never executes anything; untrusted text reaches models inside
  random-nonce fences; every command from the agent, the prefetch and the
  checks runs in a sandbox that holds no credentials.
- Git runs hardened: no hooks, credential helpers, fsmonitor, external
  diff drivers or `ext::` transport. Tokens travel as per-URL headers in
  environment-scoped config and are never written to disk; SSH keys live
  in a temporary directory.
- Nothing the agent produces is pushed before it was imported through the
  harness's own git, audited, checked against quarantined content and
  verified in a clean sandbox.
- `keep_ours` protects paths such as CI workflows, a common route to a
  fork's secrets.
- The GitHub Action runs the harness in `docker` mode on the runner, whose
  user may use Docker: sandboxes keep their policy, but the harness is not
  separated from the engine as `sandboxd` separates it in the Compose
  deployment. Use runners that serve one job each (GitHub-hosted ones do).
- With `state_ref`, the state ref is trusted like `data_dir`: allowed and
  excluded commits and cached verdicts come from it. Anyone who may push to
  the fork may change it, and branch protection does not cover a ref
  outside `refs/heads/`. If the fork branch is protected against some of
  its collaborators, protect the state as well: name a branch
  (`"state_ref": "refs/heads/vibeci-state"`) and let a ruleset allow only
  VibeCI's token to update it.
- Patch mode trusts upstream's release tags and reviews no upstream
  commits; what it adds to the fork — patches the agent rewrote — is
  audited like the merge agent's output.
- The review targets blatant attacks (remote code execution, credential
  theft, backdoors, obfuscated payloads, instructions aimed at automated
  reviewers) and is tuned not to block ordinary commits. A subtle
  vulnerability that reads like normal code can get through.

## Development

See [AGENTS.md](AGENTS.md). `make help` lists the tasks; `make ci` runs
everything CI runs.
