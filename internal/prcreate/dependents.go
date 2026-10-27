package prcreate

import (
	"fmt"
	"sort"
	"strings"
)

// A squash rewrites EVERY commit in base..head into one new commit. A branch built on any of those
// commits still carries them as unmerged after the squash, git then sees both sides adding the same
// lines, and the branch conflicts with the base. Squashing a branch that others are stacked on
// therefore breaks every one of them at once.
//
// THE ANCHOR IS THE OLDEST COMMIT IN THE RANGE, NOT THE HEAD. A branch stacked on an older commit does
// not contain the head, so counting the branches that contain the head misses it. A range that merges other lines has several oldest commits
// (one per line); every one is an anchor, because a branch containing any commit of the range contains
// at least one of them.
//
// THE LIMIT. Only refs this clone knows are seen: fetch first, or a branch published after the last
// fetch is invisible. Which is why the caller reads a probe failure as "unknown", never as "none".

// Dependents is what a squash of a head would orphan.
type Dependents struct {
	Anchor   string   // the oldest commit in base..head, as found (empty when the range is empty)
	Commits  int      // commits in base..head, which a squash rewrites into one
	Branches []string // branch names (local and remote collapsed) carrying a commit of the range, other than the PR's own
}

// DependentProbe is the git seam: the real one shells out, tests inject a fake.
type DependentProbe interface {
	// Dependents finds the branches that carry a commit of base..head and are not the PR's own branch.
	// headRef is the PR's head branch name; refs at the head sha are the PR's own too. Any failure to
	// determine the answer is an error: an unknown set is never read as an empty one.
	Dependents(repo, head, headRef, base string) (Dependents, error)
}

// ExecDependentProbe is the real DependentProbe. The range is measured against the base's origin
// remote-tracking ref when there is one (what the forge sees), else a local branch of that name.
type ExecDependentProbe struct{}

// Dependents implements DependentProbe with git.
func (ExecDependentProbe) Dependents(repo, head, headRef, base string) (Dependents, error) {
	baseRef, err := resolveBaseRef(repo, base)
	if err != nil {
		return Dependents{}, err
	}
	headSHA, err := git(repo, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return Dependents{}, fmt.Errorf("cannot resolve head %q: %v", head, err)
	}
	headSHA = strings.TrimSpace(headSHA)

	out, err := git(repo, "rev-list", "--parents", "--topo-order", "--reverse", baseRef+".."+headSHA)
	if err != nil {
		return Dependents{}, fmt.Errorf("list %s..%s: %v", base, shortSHA(headSHA), err)
	}
	roots, total := rangeRoots(out)
	d := Dependents{Commits: total}
	if total == 0 {
		return d, nil
	}
	d.Anchor = roots[0]

	names := map[string]bool{}
	for _, root := range roots {
		refs, err := git(repo, "for-each-ref", "--contains", root, "--format=%(objectname) %(refname)", "refs/heads", "refs/remotes")
		if err != nil {
			return Dependents{}, fmt.Errorf("list refs containing %s: %v", shortSHA(root), err)
		}
		for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
			sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || sha == "" || sha == headSHA || strings.HasSuffix(ref, "/HEAD") {
				continue
			}
			name := branchName(ref)
			if name == headRef {
				continue
			}
			names[name] = true
		}
	}
	for n := range names {
		d.Branches = append(d.Branches, n)
	}
	sort.Strings(d.Branches)
	return d, nil
}

// rangeRoots reads `git rev-list --parents --topo-order --reverse` output: the commits of the range
// that have no parent inside it (oldest first), and how many commits the range holds.
func rangeRoots(revList string) (roots []string, total int) {
	inRange := map[string]bool{}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(revList), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		rows = append(rows, f)
		inRange[f[0]] = true
	}
	for _, f := range rows {
		root := true
		for _, p := range f[1:] {
			if inRange[p] {
				root = false
				break
			}
		}
		if root {
			roots = append(roots, f[0])
		}
	}
	return roots, len(rows)
}

// branchName strips refs/heads/ or refs/remotes/<remote>/ so a branch and its remote-tracking copies
// are one name.
func branchName(ref string) string {
	if n, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return n
	}
	if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		if _, n, ok := strings.Cut(rest, "/"); ok {
			return n
		}
	}
	return ref
}

// ResolveMergeMethod picks the method a merge will use and says why, as a pure function so the
// decision and the dry-run plan cannot disagree. A squash or rebase rewrites the commits and so
// orphans every branch stacked on them; a merge commit keeps them. So:
//   - no method asked for, nothing stacked: squash (the default).
//   - no method asked for, branches stacked: merge, and the reason says it was defaulted.
//   - squash or rebase asked for, branches stacked: REFUSED. A human can still choose --method merge.
//   - the stack could not be determined: REFUSED for squash and rebase, since "unknown" is not "none".
//     A merge commit orphans nothing, so it needs no answer.
//
// refusal is non-empty when the method cannot be used; note is non-empty when the method was chosen
// for the caller and the plan should say so.
func ResolveMergeMethod(in DecideMergeInput) (method, note, refusal string) {
	asked := strings.ToLower(strings.TrimSpace(in.Req.Method))
	method = normalizeMergeMethod(in.Req.Method)
	if method == "merge" {
		return method, "", "" // merge orphans nothing
	}
	if !in.DependentsOK {
		return method, "", fmt.Sprintf("cannot determine whether branches are stacked on PR #%s: %s. Fail-closed: a %s would orphan them if there are any. "+
			"Pass --method merge, which orphans nothing", in.Req.PRNumber, reasonOr(in.DependentsReason, "the stack was not checked"), method)
	}
	if len(in.Dependents) == 0 {
		return method, "", ""
	}
	stacked := fmt.Sprintf("%d branch(es) carry commits this PR would merge (%s)", len(in.Dependents), sample(in.Dependents, 5))
	if asked != "" {
		return method, "", fmt.Sprintf("PR #%s: a %s rewrites its commits into one new commit, and %s; each would be left conflicting with the base. "+
			"Use --method merge, which keeps them, or, if those branches are not wanted, delete them on the remote (and locally) and then run `git fetch --prune`", in.Req.PRNumber, method, stacked)
	}
	return "merge", fmt.Sprintf("defaulted to merge, not squash: %s, and a squash would leave each conflicting", stacked), ""
}

func sample(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(", and %d more", len(names)-n)
}
