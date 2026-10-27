package prcreate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/gate"
)

// A gate record is per sha, which is right for "has this diff been reviewed" and silent about
// lineage: a tip with a complete record can sit on any number of commits nobody reviewed, and a
// check of the tip alone waves the whole PR through. This file is the check that looks DOWN.
//
// The structure it reads is the stack. Work here lands as branches built on branches, and each
// branch's tip is what gets reviewed (against the branch beneath it, not against main). So a PR is
// only as reviewed as every published branch tip it is built on: every branch whose tip is an
// ancestor of the head, and is not already on the base, must itself be fully gated. A PR from a
// branch that forks straight off the base has no such tip and is unaffected.
//
// COVERAGE BY A RECORDED BASE. A branch beneath the head needs no record of its own when every required
// layer of a gated tip above it is ready with a base (gate.Layer.Base) that puts the branch's commits inside the reviewed range.
// A layer with no base covers only its own sha.
//
// THE LIMIT. The check finds the branches by ref name. Commits that reached the head without a branch
// tip beneath it (the branch was deleted or never fetched, the commits were cherry-picked, rebased or
// squashed on) are invisible, and the pass says so rather than claiming the PR was built directly
// on its base.

// StackParent is one branch tip beneath a PR head that is not yet on the base.
type StackParent struct {
	Refs    []string // every ref name at this sha (e.g. feat/x and origin/feat/x)
	SHA     string
	Commits int // commits this tip carries beyond the base
}

// Lineage is what the probe found beneath a head.
type Lineage struct {
	Total   int // commits the PR carries beyond the base
	Parents []StackParent
}

// LineageProbe is the git seam: the real one shells out, tests inject a fake.
type LineageProbe interface {
	// StackParents lists the branch tips that are ancestors of head and are not on base, and the
	// number of commits head carries beyond base. Any failure to determine either is an error:
	// an unknown lineage is never read as a clean one.
	StackParents(repo, head, base string) (Lineage, error)

	// Covers reports whether a review that read reviewBase...tip covers every commit ancestor carries beyond prBase, counting gated tips beneath it.
	Covers(repo, prBase, reviewBase, ancestor, tip string, gated []string) (bool, error)
}

// RecordReader is the part of the gate this check needs: a record read, with no scope or bypass
// semantics, because a lineage check must not be satisfied by "this repo is out of scope".
type RecordReader interface {
	ReadRecord(sha string) (gate.Record, error)
}

// CheckLineage decides whether the PR's whole stack is gated. ok=false carries a reason naming the
// ungated branches, how many commits each carries, and how many commits the PR carries in all.
func CheckLineage(p LineageProbe, g RecordReader, repo, head, base string, required []string) (ok bool, reason string) {
	if strings.TrimSpace(head) == "" {
		return false, "the head sha could not be resolved, so the commits beneath it cannot be checked"
	}
	l, err := p.StackParents(repo, head, base)
	if err != nil {
		return false, fmt.Sprintf("cannot determine what lies beneath %s on %s: %v. Fail-closed", shortSHA(head), base, err)
	}
	if len(required) == 0 {
		required = gate.RequiredLayers
	}
	var ungated, covered []string
	unreviewed := 0
	for _, sp := range l.Parents {
		if fullyGated(g, sp.SHA, required) {
			continue
		}
		by, err := coveredBy(p, g, repo, base, required, sp, head, l.Parents)
		if err != nil {
			return false, fmt.Sprintf("cannot determine whether a recorded review base covers %s@%s: %v. Fail-closed", strings.Join(sp.Refs, " = "), shortSHA(sp.SHA), err)
		}
		if by != "" {
			covered = append(covered, fmt.Sprintf("%s@%s by %s", strings.Join(sp.Refs, " = "), shortSHA(sp.SHA), shortSHA(by)))
			continue
		}
		unreviewed += sp.Commits
		ungated = append(ungated, fmt.Sprintf("%s@%s (%d commit(s), no complete gate record)", strings.Join(sp.Refs, " = "), shortSHA(sp.SHA), sp.Commits))
	}
	if len(ungated) > 0 {
		return false, fmt.Sprintf("this PR carries %d commit(s) beyond %s and is built on %d ungated branch(es): %s. "+
			"A gated tip does not make the commits beneath it reviewed; gate those branches first",
			l.Total, base, len(ungated), strings.Join(ungated, "; "))
	}
	if len(l.Parents) == 0 {
		if l.Total <= 1 {
			return true, fmt.Sprintf("the head is the only commit beyond %s", base)
		}
		return true, fmt.Sprintf("no branch tip beneath the head was found, so the %d commit(s) beyond %s are covered only by the head's own record. "+
			"A record names a sha, not what it reviewed: commits that reached this line without a branch tip (a deleted or unfetched branch, "+
			"a cherry-pick, a squash) look the same as reviewed ones", l.Total, base)
	}
	if len(covered) > 0 {
		return true, fmt.Sprintf("%d commit(s) beyond %s, built on %d branch tip(s), each fully gated or inside the range a ready, fully gated tip recorded as reviewed (%s); commits between tips are not separately checked",
			l.Total, base, len(l.Parents), strings.Join(covered, "; "))
	}
	return true, fmt.Sprintf("%d commit(s) beyond %s, built on %d branch tip(s), each fully gated; commits between tips are not separately checked", l.Total, base, len(l.Parents))
}

// coveredBy returns the sha of a tip (the head, or another branch beneath it) whose record covers sp
// through a recorded base, or "" if none does. A tip covers only if EVERY required layer is ready AND
// carries a base that covers sp: a layer with no base vouches for its own sha alone.
func coveredBy(p LineageProbe, g RecordReader, repo, prBase string, required []string, sp StackParent, head string, parents []StackParent) (string, error) {
	tips := []string{head}
	var gated []string
	for _, o := range parents {
		if o.SHA == sp.SHA {
			continue
		}
		tips = append(tips, o.SHA)
		if fullyGated(g, o.SHA, required) {
			gated = append(gated, o.SHA)
		}
	}
	for _, tip := range tips {
		rec, err := g.ReadRecord(tip)
		if err != nil {
			continue
		}
		all := true
		for _, layer := range required {
			l, ok := rec.Layers[layer]
			if !ok || l.Verdict != "ready" || l.Base == "" {
				all = false
				break
			}
			cov, err := p.Covers(repo, prBase, l.Base, sp.SHA, tip, gated)
			if err != nil {
				return "", err
			}
			if !cov {
				all = false
				break
			}
		}
		if all {
			return tip, nil
		}
	}
	return "", nil
}

func fullyGated(g RecordReader, sha string, required []string) bool {
	rec, err := g.ReadRecord(sha)
	if err != nil {
		return false
	}
	return len(gate.Missing(rec, required)) == 0
}

// ExecLineageProbe is the real LineageProbe. Ancestry is judged on the base's origin remote-tracking
// ref when there is one (the state the forge sees), else a local branch of that name; the branches
// beneath the head are looked for under refs/heads and every remote.
type ExecLineageProbe struct{}

// StackParents implements LineageProbe with git.
func (ExecLineageProbe) StackParents(repo, head, base string) (Lineage, error) {
	baseRef, err := resolveBaseRef(repo, base)
	if err != nil {
		return Lineage{}, err
	}
	headSHA, err := git(repo, "rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return Lineage{}, fmt.Errorf("cannot resolve head %q: %v", head, err)
	}
	headSHA = strings.TrimSpace(headSHA)
	total, err := commitCount(repo, baseRef, headSHA)
	if err != nil {
		return Lineage{}, err
	}

	// One pass: git itself keeps only refs that are ancestors of the head (--merged) and not already
	// on the base (--no-merged). The survivors are exactly the stack-parent candidates; filtering per
	// ref with merge-base spawns is O(all refs) subprocess walks, which hangs on a monorepo.
	out, err := git(repo, "for-each-ref", "--merged="+headSHA, "--no-merged="+baseRef,
		"--format=%(objectname) %(refname)", "refs/heads", "refs/remotes")
	if err != nil {
		return Lineage{}, fmt.Errorf("list refs: %v", err)
	}
	bySHA := map[string]*StackParent{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || sha == "" || sha == headSHA || strings.HasSuffix(ref, "/HEAD") {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/")
		if sp, seen := bySHA[sha]; seen {
			sp.Refs = append(sp.Refs, name)
			continue
		}
		n, err := commitCount(repo, baseRef, sha)
		if err != nil {
			return Lineage{}, err
		}
		bySHA[sha] = &StackParent{Refs: []string{name}, SHA: sha, Commits: n}
	}
	l := Lineage{Total: total}
	for _, sp := range bySHA {
		sort.Strings(sp.Refs)
		l.Parents = append(l.Parents, *sp)
	}
	sort.Slice(l.Parents, func(i, j int) bool {
		if l.Parents[i].Commits != l.Parents[j].Commits {
			return l.Parents[i].Commits > l.Parents[j].Commits
		}
		return l.Parents[i].SHA < l.Parents[j].SHA
	})
	return l, nil
}

func resolveBaseRef(repo, base string) (string, error) {
	for _, c := range []string{"refs/remotes/origin/" + base, "refs/heads/" + base} {
		if _, err := git(repo, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("base %q is not a branch in this clone (fetch it first)", base)
}

// Covers implements LineageProbe with git: ancestor is covered iff it lies at or below tip and every commit
// beyond prBase is inside reviewBase...tip or inside the history of a gated tip beneath ancestor.
func (ExecLineageProbe) Covers(repo, prBase, reviewBase, ancestor, tip string, gated []string) (bool, error) {
	baseRef, err := resolveBaseRef(repo, prBase)
	if err != nil {
		return false, err
	}
	for _, c := range []string{reviewBase, ancestor, tip} {
		if _, err := git(repo, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err != nil {
			return false, fmt.Errorf("cannot resolve %s", shortSHA(c))
		}
	}
	below, err := isAncestor(repo, ancestor, tip)
	if err != nil || !below {
		return false, err
	}
	// reviewBase...tip is the three-dot range: it starts at the merge base. No merge base is no range.
	mb, found, err := mergeBase(repo, reviewBase, tip)
	if err != nil || !found {
		return false, err
	}
	exclude := []string{"^" + baseRef}
	for _, gsha := range gated {
		if gsha == ancestor {
			continue
		}
		beneath, err := isAncestor(repo, gsha, ancestor)
		if err != nil {
			return false, err
		}
		if beneath {
			exclude = append(exclude, "^"+gsha)
		}
	}
	count := func(extra ...string) (int, error) {
		args := append(append([]string{"rev-list", "--count", ancestor}, exclude...), extra...)
		out, err := git(repo, args...)
		if err != nil {
			return 0, fmt.Errorf("count %s beyond %s: %v", shortSHA(ancestor), prBase, err)
		}
		return strconv.Atoi(strings.TrimSpace(out))
	}
	remaining, err := count()
	if err != nil {
		return false, err
	}
	readByTheReview, err := count("^" + mb)
	if err != nil {
		return false, err
	}
	return readByTheReview == remaining, nil
}

// mergeBase is git merge-base: exit 0 is a commit, 1 is none in common (an answer), and anything else is
// an ERROR, for the same reason as isAncestor.
func mergeBase(repo, a, b string) (sha string, found bool, err error) {
	out, code, err := gitRun(repo, "merge-base", a, b)
	if err != nil {
		return "", false, fmt.Errorf("git merge-base %s %s: %v", shortSHA(a), shortSHA(b), err)
	}
	switch code {
	case 0:
		return strings.TrimSpace(out), true, nil
	case 1:
		return "", false, nil
	}
	return "", false, fmt.Errorf("git merge-base %s %s: exit status %d", shortSHA(a), shortSHA(b), code)
}

// gitTimeout bounds every git call the probe makes, so a pathological repo fails CLOSED with an
// error instead of hanging. A variable so tests can shrink it.
var gitTimeout = 120 * time.Second

// gitRun runs git under the deadline. err is non-nil only when git did not run to an exit (spawn
// failure, timeout, signal); a normal non-zero exit is reported through code with err nil.
func gitRun(repo string, args ...string) (out string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	b, rerr := cmd.Output()
	if rerr == nil {
		return string(b), 0, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", -1, fmt.Errorf("git %s timed out after %s (repo too large? fetch narrower?)", args[0], gitTimeout)
	}
	var ee *exec.ExitError
	if errors.As(rerr, &ee) && ee.ExitCode() >= 0 {
		return string(b), ee.ExitCode(), nil
	}
	return "", -1, rerr
}

func git(repo string, args ...string) (string, error) {
	out, code, err := gitRun(repo, args...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("git %s: exit status %d", args[0], code)
	}
	return out, nil
}

func commitCount(repo, base, tip string) (int, error) {
	out, err := git(repo, "rev-list", "--count", base+".."+tip)
	if err != nil {
		return 0, fmt.Errorf("count %s..%s: %v", base, shortSHA(tip), err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("count %s..%s: %v", base, shortSHA(tip), err)
	}
	return n, nil
}

// isAncestor is git merge-base --is-ancestor: exit 0 is yes, 1 is no, and anything else (a bad
// ref, a corrupt repo) is an ERROR, never "no": reading a failure as "not an ancestor" would drop a
// ref from the stack and let an ungated line through.
func isAncestor(repo, a, b string) (bool, error) {
	_, code, err := gitRun(repo, "merge-base", "--is-ancestor", a, b)
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %v", shortSHA(a), shortSHA(b), err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: exit status %d", shortSHA(a), shortSHA(b), code)
}
