# Working on VibeCI

For agents changing VibeCI's code. [README.md](README.md) covers what
VibeCI does, how to deploy it and its CLI contract; read it first.

## Ground rules

- **Standard library only.** `go.mod` has no requirements; keep it that
  way. VibeCI pushes to repositories unattended with write credentials, so
  its own supply chain is part of the threat model.
- **Go version**: the minimum is the `go` line in `go.mod` (CI tests it
  and the current stable release); images build with `golang:1.27-alpine`.
- `make lint` (gofmt + go vet) must be clean; `make ci` must pass.
- **The CLI is an API.** Operating agents parse commands, flags, `-json`
  field names, exit codes and some error strings. `e2e/TestCLIContract`
  pins them. Change them deliberately, together with README.md.
- **State persists across upgrades.** `data_dir/state` is JSON written by
  older versions, and so is the copy in forks under `state_ref`
  (`internal/pipeline/remote.go`; tree entries a version does not know are
  kept): new fields must be optional, and renamed or removed fields need a
  migration.
- Docs, comments and messages are for agents too: precise, no fluff, say
  what to do next.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/vibeci` | CLI: one function per command in `cmds.go`; wiring (`setup`, `localRunner`, `sandboxProvider`, `buildModels`, the repeatable `-config`) in `main.go`; `action.go`: `vibeci action`, the GitHub Action's side (defaults, config files from the workspace or the repository, this repository as the fork, masking, job summary, annotations, step outputs) |
| `internal/config` | config types (`types.go`), defaults and validation (`load.go`), `Secret` (`env:`/`file:`/literal, redacted when marshalled) |
| `internal/jsonc` | JSONC to JSON |
| `internal/gitx` | hardened git CLI wrapper: mirrors, fetch, `merge-tree`, `commit-tree`, push with lease, per-URL credentials; `source.go`: shallow partial fetches, tags, blobs and trees for patch mode |
| `internal/llm` | provider clients (Anthropic Messages, OpenAI Responses, Chat Completions; vendor APIs and compatible gateways), SSE, retries (`transport.go`), `Fake` for tests |
| `internal/agent` | tool-using agent loop shared by the investigator, the merge agent and the patch agent |
| `internal/review` | security review gate: `signals.go` (deterministic), `collect.go` (diff units), triage, `investigate.go`, `gate.go` (plan, safe target); the audit of agent-written lines |
| `internal/resolve` | one merge job: fast-forward / clean merge / agent (`job.go`, `tools.go`, `workspace.go`), audit, clean-room verification, exclusions and quarantine (`exclude.go`); one patch-mode update: `patchjob.go` (apply, refresh, commit, verify; `Analyze` for `vibeci patches`), `patchagent.go` (the patch agent) |
| `internal/patch` | unified diffs as GNU patch and quilt treat them: parse (keeping every non-hunk byte), apply with offsets/fuzz/whitespace, detect upstreamed hunks, regenerate, quilt series |
| `internal/source` | patch-mode upstream copies under `sources/`: `Store`, `Mirror` (partial clone per remote), `Snapshot` (one version's tree, sub-repositories via `patches.sources`) |
| `internal/vers` | version strings: natural order, prereleases, tag formats, reading and replacing the pin |
| `internal/sandbox` | `Provider` interface; `DockerProvider` (the policy, `policy.go`), `Broker` + `BrokerClient` (`sandboxd` over a unix socket), `LocalProvider` (`unsafe-local`) |
| `internal/pipeline` | `Runner`: sync state machine (`pipeline.go`), patch mode (`patches.go`), `allow`/`exclude` and exclusions recovered from trailers (`exclusions.go`), the state in the fork (`remote.go`), backoff, alerts |
| `internal/state` | JSON state store, `flock` repo locks, three-way JSON merge (`merge.go`) |
| `internal/alert` | webhook sinks (ntfy, Discord, Slack, generic) |
| `deploy/` | image, compose file, sandbox images, example configs; `action.sh`, the GitHub Action's script (builds the binary from the image's build stage and the sandbox images, runs `vibeci action`) |
| `action.yml` | the GitHub Action (composite): inputs to `VIBECI_ACTION_*` |
| `.github/workflows` | `ci.yml` (the `make ci` targets, and the action itself), `e2e-live.yml`, `release.yml` (tags `v*`: binaries with provenance, multi-arch image on ghcr.io) |
| `e2e/` | end-to-end tests: harness, scripted fake model, scenarios, compose additions |

## Invariants

Breaking one of these is a security bug, whatever the tests say.

1. Untrusted code — upstream content and anything the merge or patch agent
   writes — runs only in sandboxes. The harness never executes repository
   content:
   `gitx` disables hooks, fsmonitor, credential helpers, external diff and
   textconv drivers and `ext::`.
2. Credentials (LLM keys, git tokens, SSH keys) never reach sandboxes,
   prompts, logs, job records or the data volume.
3. Sandboxes come only from a `sandbox.Provider`: the harness picks a
   profile by name and a workspace under `jobs/`; the provider validates
   the path (`^jobs/[A-Za-z0-9_-]{1,100}/[A-Za-z0-9_-]{1,60}$`) and applies
   the profile's fixed policy. In broker mode (the Compose deployment) only
   `sandboxd` talks to Docker; `docker` mode (a daemon on a host, the GitHub
   Action) gives up that separation, not the policy.
4. Agent output is imported through the harness's own git, its novel lines
   are audited, it is checked against quarantined content and verified in
   a fresh sandbox before anything is pushed. The patch agent's files are
   rebuilt into patches by the harness, their novel and deleted lines
   audited, and the whole series re-applied strictly.
5. The review fails closed: a commit without a verdict is never merged.
   Untrusted text in prompts sits inside random-nonce fences.
6. History only grows: merge commits (patch mode: one commit on top of the
   fork branch), no rebases, no force pushes of the fork branch. Branch
   mode may lease-push only over commits VibeCI made; the state ref
   (`state_ref`) is VibeCI's own and replaced with a lease, its concurrent
   changes merged.
7. Excluded commits stay out: their changes are reverted and their content
   quarantined in every later merge, including fast-forwards and clean
   merges — also after the state is lost: exclusions are recovered from the
   trailers of VibeCI's own merges on the fork branch.

## Tests

| Target | What | Needs | Time |
|---|---|---|---|
| `make test`, `make race` | unit tests: `llm.Fake` models, `LocalProvider`, file-path git remotes, `httptest` webhooks | Go, git | ~40 s |
| `make test-docker` | `DockerProvider` policy against a real engine | Docker | ~5 s |
| `make e2e` | `e2e/`: the built binary against git scenarios and the scripted fake model, `unsafe-local` sandbox; `TestAction` runs `vibeci action` on simulated fresh runners | Go, git | ~20 s |
| `make e2e-docker` | the same with docker sandboxes | Docker | ~45 s |
| `make e2e-compose` | builds the images and deploys `deploy/docker-compose.yml` + `e2e/compose/` (git daemon, fake model) as a compose project with a merge-mode and a patch-mode fork: daemon cycle, CLI in the container, `SIGUSR1` | Docker + compose | ~45 s |
| `make e2e-live` | real models from `E2E_LLM` (default `.secrets/e2e-llm.jsonc`, format of `e2e/llm.example.jsonc`), docker sandboxes, plus `TestColor`: a fatih/color fork at v1.13.0 synced to the newest release | models, network | ~10 min |
| `make ci` | everything except `e2e-live` | Docker + compose | ~3 min |

CI (`.github/workflows/ci.yml`) runs the `make ci` targets, and the action
itself (`uses: ./`, `vibeci check -no-llm` against this repository, config
`e2e/action/smoke.jsonc`); real-model tests run in `e2e-live.yml`, on
demand and weekly, only if the `VIBECI_E2E_LLM_CONFIG` secret is set.
Actions are pinned by commit SHA, base images by digest; Dependabot
proposes updates.

The e2e knobs are documented at the top of `e2e/harness_test.go`:
`VIBECI_E2E=1` enables the package; `VIBECI_E2E_SANDBOX=docker`,
`VIBECI_E2E_LLM_CONFIG`, `VIBECI_E2E_COMPOSE=1`, `VIBECI_E2E_OSS=1`,
`VIBECI_E2E_DIR`, `VIBECI_E2E_KEEP=1` (keep work dirs; failed tests always
keep theirs), `VIBECI_E2E_REBUILD=1`. A failing e2e test logs where its
work dir is; `vibeci.log` there has every invocation's stderr.

### Writing tests

- Unit tests of the pipeline and resolver use scripted `llm.Fake` models
  and real git repositories in temp dirs; follow the helpers in
  `internal/pipeline/pipeline_test.go` and `internal/resolve/*_test.go`
  (patch mode: `internal/pipeline/patches_test.go`,
  `internal/resolve/patchjob_test.go`). Build a test patch series one
  patch at a time, each against the tree with the earlier ones applied.
- An e2e scenario is a deterministic git history in `e2e/scenario`
  (upstream, bare fork, maintainer clone; fixed identities and dates) plus
  a `Script` for the fake model: `MaliciousMarkers` (a reviewed commit
  whose diff contains one is malicious), `Resolution` (files the fake
  merge agent writes) and `Patches` (per patch name, files the fake patch
  agent writes, or `Drop`). See `scenario/calc.go` and `calc_test.go`;
  for patch mode `scenario/greet.go` (one patch each shifted, conflicting
  and upstreamed) and `greet_test.go`.
- `e2e/fakellm` recognizes roles by the tools a request offers
  (`submit_review`, `submit_verdict`, `submit` + `open_file` for the patch
  agent, `submit` + `write_file` for the merge agent), reads the patch
  name from the patch agent's brief ("update patch N of M, NAME (file
  ...") and tells the audit from triage by the audit prompt's "Lines
  written by the merge agent". Renaming tools or rewording those prompts
  means updating fakellm.
- `e2e/action_test.go` runs `vibeci action` with the environment a runner
  provides (`RUNNER_TEMP`, `GITHUB_OUTPUT`, `GITHUB_STEP_SUMMARY`,
  `GITHUB_REPOSITORY`, `GITHUB_SERVER_URL` a directory with the fork bare
  repository at `owner/name.git`) and inputs as `VIBECI_ACTION_*`; a new
  `RUNNER_TEMP` per run, so all state must come from the fork.
- Live tests skip unless `liveLLM()`. Model output varies, so assert
  behaviour — it builds and tests clean, fork features work, malicious
  content is absent, protected files are byte-identical — not exact text.
- Run the live tier after changing prompts, review, resolve or provider
  code.

## Environment gotchas

- Docker rejects bind sources and volume subpaths that do not exist, and
  the broker cannot create them: create directories before
  `Provider.Create` (`resolve` does; `LocalProvider` fails the same way so
  unit tests catch it).
- Colima and Docker Desktop share only `$HOME` with the VM: keep test data
  under the repository's `.tmp-test/` (gitignored), not `/tmp`. Never reuse
  a path, container or volume name that was just deleted — use unique
  names.
- Images are not rebuilt automatically: `VIBECI_E2E_REBUILD=1` (the compose
  test always rebuilds `vibeci:e2e` and the fake model).
- Sandboxes are labelled `vibeci.managed=true` and `vibeci.instance=<data
  location>`; a deployment removes only its own leftovers.
- The `unsafe-local` sandbox has no `/workspace`: sandbox commands must use
  relative paths.
- A host firewall that vets new executables can block freshly built Go
  test binaries while `curl` and `git` still connect (dial timeouts to
  every address). Run the test inside a container then, e.g. `docker run
  -v "$PWD:$PWD" -w "$PWD" golang:1.27-alpine` with git added. For docker
  sandboxes from inside it, add `docker-cli` and mount the engine's socket
  by its path in the VM (`-v /var/run/docker.sock:/var/run/docker.sock`
  with Colima; the host path of Colima's socket fails).
- Partial and by-commit fetches from file-path remotes need
  `uploadpack.allowFilter` and `uploadpack.allowAnySHA1InWant` on the
  remote (`scenario.Bare` sets them). `git ls-remote` prints an annotated
  tag's peeled line only if `refs/tags/X^{}` is asked for too; with
  `GIT_NO_LAZY_FETCH=1`, `git cat-file --batch-check` reports blobs a
  partial clone lacks as `<oid> missing`.
- Some LLM gateways reject unknown request fields with HTTP 400: send only
  documented fields for each provider type.
- Errors can arrive inside an HTTP 200 stream. They are retried unless the
  type is known to be permanent (`permanentAnthropicError`,
  `permanentResponsesError`); gateways send types of their own.
- `encoding/json` HTML-escapes `<`, `>` and `&` in custom `MarshalJSON`
  output even with `SetEscapeHTML(false)`: keep such characters out of
  marker strings (`Secret` uses `[redacted]`).
- `git merge-tree --write-tree` needs git ≥ 2.38.
- GitHub refuses pushes of commits that change `.github/workflows` (a
  merge's upstream commits included) from tokens without the workflows
  permission, which `GITHUB_TOKEN` never has: `pushHint` matches the
  message. A workflow cannot override `GITHUB_*` and `RUNNER_*` variables
  through `GITHUB_ENV`; in `exec.Cmd.Env` the last duplicate wins.
- The module path is `github.com/vibeci/vibeci`; strings such as
  `vibeci/1` (User-Agent) and `vibeci/sync` (default push branch) look
  like import paths: exclude string literals when rewriting imports.

## Before you commit

1. `make lint test`, plus the e2e tier your change touches (`make ci` if
   unsure).
2. User-visible behaviour, config or CLI changed: update README.md, the
   examples in `deploy/`, and `TestCLIContract`.
3. Prompts, review, resolve or providers changed: `make e2e-live`.
4. Never commit secrets: `.secrets/`, `secrets/`, `deploy/config.jsonc` and
   `deploy/sandboxd.jsonc` are gitignored for that reason.
