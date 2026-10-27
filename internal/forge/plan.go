package forge

import (
	"fmt"
	"sort"
	"strings"
)

// ChangeKind classifies one line of a plan or read-back diff.
type ChangeKind int

const (
	// Removes: a protection that is present now would be gone or weaker. Never applied.
	Removes ChangeKind = iota
	// Changes: a property moves to a different, stricter-or-neutral value.
	Changes
	// Adds: a protection that is absent now would be put in force.
	Adds
)

// Change is one line of a plan or read-back diff. Kind decides how String renders it and
// where it sorts (removals first); Text is a sentence an operator can read without the JSON.
type Change struct {
	Kind ChangeKind
	Text string
}

func (c Change) String() string {
	switch c.Kind {
	case Removes:
		return "- REMOVES: " + c.Text
	case Adds:
		return "+ ADDS: " + c.Text
	}
	return "~ CHANGES: " + c.Text
}

// sortChanges puts removals first, so a long diff cannot bury the one that matters.
func sortChanges(ch []Change) []Change {
	sort.SliceStable(ch, func(i, j int) bool { return ch[i].Kind < ch[j].Kind })
	return ch
}

// Plan is what a protection write would do, in terms an operator can decide on. It is
// built before anything is written and shown on the dry-run too: the dry-run is where the
// decision is made. The JSON body is for the audit trail; this is the warning.
type Plan struct {
	Current string   // the branch's protection today, in words an operator can set against Changes
	Changes []Change // removals first
	// Why is how this forge applies the write. GitHub's PUT replaces the whole rule, so unsent
	// settings are dropped; GitLab's edit touches only the fields leadkit sends.
	Why string
}

// Removals returns the changes that would weaken the branch. A plan with any Removals is
// refused by Decide even under --confirm: leadkit never weakens a protection, so removing
// one is a manual edit on the forge.
func (p Plan) Removals() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Kind == Removes {
			out = append(out, c)
		}
	}
	return out
}

// Summary is the one loud line: how many protections would be lost, or that none would.
func (p Plan) Summary() string {
	if n := len(p.Removals()); n > 0 {
		return fmt.Sprintf("WARNING: this would REMOVE %s", plural(n, "existing protection"))
	}
	return "nothing the branch is protected by today is removed"
}

const ghWhy = "the forge's PUT replaces the whole protection rule, so anything not sent is dropped; " +
	"leadkit reads the current rule and sends it back with only the changes above"

const glWhy = "leadkit sends only the push-access and force-push fields of the rule; it does not rewrite the rest"

// planFor builds the plan for a composed write. For GitHub it diffs the bytes about to be
// sent against the rule as read, so a body that drops something is caught whatever the
// reason, rather than trusting the merge that produced it.
func planFor(t Target, cur Status, c Call) (Plan, error) {
	if t.Kind == KindGitLab {
		return planGitLab(cur), nil
	}
	now := ghRules{}
	if cur.ghRules != nil {
		now = *cur.ghRules
	}
	sent, err := parseGHRules(c.Body)
	if err != nil {
		return Plan{}, fmt.Errorf("leadkit built a write body it cannot read back (%v); refusing", err)
	}
	return Plan{Current: now.describe(), Changes: diffGH(now, sent), Why: ghWhy}, nil
}

func planGitLab(cur Status) Plan {
	p := Plan{Why: glWhy}
	ex := cur.gitlabExact
	if ex == nil {
		p.Current = "no protected-branch rule for this exact branch"
		p.Changes = []Change{{Adds, fmt.Sprintf("a protected-branch rule for %s: nobody may push directly (merge requests only), Maintainers may merge, force-push blocked", cur.Branch)}}
		return p
	}
	var push []string
	for _, l := range ex.PushAccessLevels {
		push = append(push, describeLevel(l))
	}
	if len(push) == 0 {
		push = []string{"nobody reported"}
	}
	force := "force-push " + flag(ex.AllowForcePush != nil && !*ex.AllowForcePush, "blocked", "allowed or not reported")
	p.Current = "direct push allowed to: " + strings.Join(push, ", ") + "; " + force
	for _, l := range ex.PushAccessLevels {
		if l.AccessLevel != glNoOne || l.UserID != nil || l.GroupID != nil || l.DeployKeyID != nil {
			p.Changes = append(p.Changes, Change{Changes, "direct push to " + describeLevel(l) + " -> nobody"})
		}
	}
	if ex.AllowForcePush == nil || *ex.AllowForcePush {
		p.Changes = append(p.Changes, Change{Adds, "force-push blocked"})
	}
	return p
}

// ReadBackProblems is the adversarial half of the post-write read-back. Verifying that the
// properties leadkit set took effect is not enough, because a replace write can set them
// and clobber everything else. So it also asserts nothing the branch had before the write
// is gone or weaker after it. An empty result means the write did what the plan said.
func ReadBackProblems(before, after Status) []string {
	var out []string
	if !after.Verified() {
		out = append(out, fmt.Sprintf("the forge does not report %s verified: [%s] %s", after.Branch, after.State, after.Detail))
	}
	var lost []Change
	switch {
	case before.ghRules != nil && after.ghRules != nil:
		lost = diffGH(*before.ghRules, *after.ghRules)
	case before.ghRules != nil:
		// It had a rule; now the forge reports none we could read.
		lost = diffGH(*before.ghRules, ghRules{})
	case before.gitlabExact != nil && after.gitlabExact != nil:
		lost = diffGitLab(*before.gitlabExact, *after.gitlabExact)
	}
	for _, c := range lost {
		if c.Kind == Removes {
			out = append(out, "after the write the branch lost a protection it had before: "+c.Text)
		}
	}
	return out
}

// diffGitLab compares the parts of a protected-branch rule the PATCH is not meant to
// touch (merge and unprotect access, code-owner approval).
func diffGitLab(a, b glBranch) []Change {
	var ch []Change
	if a.CodeOwnerApproval && !b.CodeOwnerApproval {
		ch = append(ch, Change{Removes, "code-owner approval requirement"})
	}
	for _, e := range []struct {
		label string
		a, b  []glLevel
	}{{"merge access", a.MergeAccessLevels, b.MergeAccessLevels}, {"unprotect access", a.UnprotectAccessLevels, b.UnprotectAccessLevels}} {
		as, bs := levelSet(e.a), levelSet(e.b)
		if n := missing(bs, as); len(n) > 0 {
			ch = append(ch, Change{Removes, e.label + " now also granted to " + strings.Join(n, ", ")})
		}
		if n := missing(as, bs); len(n) > 0 {
			ch = append(ch, Change{Changes, e.label + " no longer granted to " + strings.Join(n, ", ")})
		}
	}
	return sortChanges(ch)
}

func levelSet(ls []glLevel) map[string]bool {
	m := map[string]bool{}
	for _, l := range ls {
		m[describeLevel(l)] = true
	}
	return m
}
