# Security policy

Report vulnerabilities privately:
[Security → Report a vulnerability](https://github.com/vibeci/vibeci/security/advisories/new).
Please do not open a public issue for them.

VibeCI pushes to repositories unattended with write credentials, so these
count as vulnerabilities:

- untrusted content (upstream commits, anything the merge or patch agent
  writes) executed outside a sandbox, or a sandbox escape through VibeCI's
  sandbox policy;
- credentials (LLM keys, git tokens, SSH keys) reaching a sandbox, a
  prompt, a log, a job record or the data directory;
- a commit pushed without a review verdict, an excluded commit's changes
  coming back into a fork, agent output pushed without its audit and
  verification, or a push that rewrites a fork branch's history;
- a way for upstream content to change VibeCI's own state or decisions
  (for example, forged trailers or prompt injection that gets past the
  audit).

The review is not a guarantee: a subtle vulnerability that reads like
ordinary code can get through (see README.md, Security model). Reports that
show a class of blatant attacks passing the review are welcome.

Fixes go into the latest release only.
