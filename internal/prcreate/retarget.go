package prcreate

import (
	"fmt"
	"os/exec"
	"strings"
)

// BaseProbe is the I/O seam behind a retarget decision: what the forge says the PR's base is, and
// whether one branch is an ancestor of another in the local clone. The real implementation shells
// out; tests inject a fake so no network or forge is touched.
type BaseProbe interface {
	CurrentBase(forgeName, repo, prNumber string) (string, error)
	// Refresh updates the origin/<branch> remote-tracking refs from the forge, so ancestry is
	// judged on what the forge has now and not on a clone that has not fetched since a force-push.
	Refresh(repo string, branches ...string) error
	IsAncestor(repo, ancestorRef, descendantRef string) (bool, error)
}

// ResolveBaseCheck gathers the evidence DecideEdit needs for `--base newBase`. Any failure
// (unsupported forge, lookup error, a branch missing locally) yields Resolved=false with the reason,
// which DecideEdit turns into a refusal: it never guesses.
//
// Ancestry is judged on the REMOTE-TRACKING refs (origin/<branch>), the state the forge sees, not
// whatever local branch of that name happens to exist. Both are refreshed from the forge first,
// because a stale origin/* after a force-push of either branch can still say "ancestor"; if the
// refresh fails the retarget is refused rather than judged on old refs.
func ResolveBaseCheck(p BaseProbe, forgeName, repo, prNumber, newBase string) BaseCheck {
	if forgeName != "github" {
		return BaseCheck{Reason: fmt.Sprintf("base retargeting is only verifiable on github, not %q", forgeName)}
	}
	cur, err := p.CurrentBase(forgeName, repo, prNumber)
	if err != nil || strings.TrimSpace(cur) == "" {
		return BaseCheck{Reason: fmt.Sprintf("could not read PR #%s's current base: %v", prNumber, orNilMsg(err))}
	}
	cur = strings.TrimSpace(cur)
	if cur == strings.TrimSpace(newBase) {
		return BaseCheck{Resolved: true, CurrentBase: cur, Ancestor: true}
	}
	if err := p.Refresh(repo, cur, strings.TrimSpace(newBase)); err != nil {
		return BaseCheck{CurrentBase: cur, Reason: fmt.Sprintf("could not refresh origin/%s and origin/%s: %v", cur, strings.TrimSpace(newBase), err)}
	}
	anc, err := p.IsAncestor(repo, "origin/"+cur, "origin/"+strings.TrimSpace(newBase))
	if err != nil {
		return BaseCheck{CurrentBase: cur, Reason: err.Error()}
	}
	return BaseCheck{Resolved: true, CurrentBase: cur, Ancestor: anc}
}

func orNilMsg(err error) string {
	if err == nil {
		return "empty answer"
	}
	return err.Error()
}

// ExecProbe is the real BaseProbe: gh for the base, git for ancestry.
type ExecProbe struct{}

// CurrentBase asks gh for the PR's base branch.
func (ExecProbe) CurrentBase(forgeName, repo, prNumber string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prNumber, "--json", "baseRefName", "-q", ".baseRefName")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Refresh fetches each branch from origin into refs/remotes/origin/<branch>. It touches only those
// remote-tracking refs: no working tree, no local branch.
func (ExecProbe) Refresh(repo string, branches ...string) error {
	args := []string{"fetch", "--quiet", "origin"}
	for _, b := range branches {
		if out, err := gitOut(repo, "check-ref-format", "refs/heads/"+b); err != nil {
			return fmt.Errorf("%q is not a valid branch name (%s)", b, strings.TrimSpace(out))
		}
		args = append(args, "+refs/heads/"+b+":refs/remotes/origin/"+b)
	}
	if out, err := gitOut(repo, args...); err != nil {
		return fmt.Errorf("git fetch origin: %s", strings.TrimSpace(out))
	}
	return nil
}

func gitOut(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// IsAncestor reports whether ancestorRef is an ancestor of descendantRef in repo. A ref that does
// not exist is an error (so the caller refuses), never "false".
func (ExecProbe) IsAncestor(repo, ancestorRef, descendantRef string) (bool, error) {
	for _, ref := range []string{ancestorRef, descendantRef} {
		cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		cmd.Dir = repo
		if err := cmd.Run(); err != nil {
			return false, fmt.Errorf("%s is not a ref in this clone; run `git fetch origin` first", ref)
		}
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestorRef, descendantRef)
	cmd.Dir = repo
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}
