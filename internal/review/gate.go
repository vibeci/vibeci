package review

import (
	"context"

	"github.com/vibeci/vibeci/internal/gitx"
)

// Step is one commit on upstream's first-parent chain together with the new
// commits it brings in (itself, plus a merged side branch if it is a merge).
type Step struct {
	Commit     string
	Introduces []string
}

// Plan lists the upstream steps not yet in the fork, oldest first.
type Plan struct {
	Steps []Step
	// Limited is set when the chain was cut short by maxCommits; the fork
	// catches up over several cycles.
	Limited bool
	// Total is the number of new upstream commits in the full range.
	Total int
}

// Commits returns every commit introduced by the plan's steps.
func (p *Plan) Commits() []string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.Introduces...)
	}
	return out
}

// Target is the newest step (the full plan's merge target).
func (p *Plan) Target() string {
	if len(p.Steps) == 0 {
		return ""
	}
	return p.Steps[len(p.Steps)-1].Commit
}

// BuildPlan walks upstream's first-parent chain from forkHead (exclusive) to
// tip, attributing every new commit to the first-parent step that brings it
// in. At most maxCommits commits are planned (at least one step).
func BuildPlan(ctx context.Context, repo *gitx.Repo, forkHead, tip string, maxCommits int) (*Plan, error) {
	chain, err := repo.RevList(ctx, "--first-parent", "--reverse", tip, "^"+forkHead)
	if err != nil {
		return nil, err
	}
	all, err := repo.RevList(ctx, "--count", tip, "^"+forkHead)
	if err != nil {
		return nil, err
	}
	plan := &Plan{}
	if len(all) == 1 {
		var n int
		for _, c := range all[0] {
			n = n*10 + int(c-'0')
		}
		plan.Total = n
	}
	count := 0
	prev := ""
	for _, c := range chain {
		args := []string{"--topo-order", "--reverse", c, "^" + forkHead}
		if prev != "" {
			args = append(args, "^"+prev)
		}
		intro, err := repo.RevList(ctx, args...)
		if err != nil {
			return nil, err
		}
		if len(plan.Steps) > 0 && maxCommits > 0 && count+len(intro) > maxCommits {
			plan.Limited = true
			break
		}
		plan.Steps = append(plan.Steps, Step{Commit: c, Introduces: intro})
		count += len(intro)
		prev = c
	}
	return plan, nil
}

// SafeTarget returns the newest step such that no commit introduced up to
// and including it is blocked, plus the first blocked commit found (if any).
// An empty target means not even the first step is safe.
func (p *Plan) SafeTarget(blocked func(sha string) bool) (target, firstBlocked string) {
	for _, s := range p.Steps {
		for _, c := range s.Introduces {
			if blocked(c) {
				return target, c
			}
		}
		target = s.Commit
	}
	return target, ""
}
