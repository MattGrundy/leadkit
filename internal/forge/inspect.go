package forge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// State is a closed classification of one branch's server-side protection. Only
// StateVerified means protected to the standard; every other value, StateUnknown above all,
// must be reported as not protected to that standard rather than rounded up. (StatePartial is
// protected, but short of it.)
type State string

const (
	// StateVerified: the forge affirmatively reported every property leadkit requires.
	StateVerified State = "verified"
	// StatePartial: protected, but weaker than required (Gaps says how).
	StatePartial State = "partial"
	// StateUnprotected: the forge says no rule covers the branch.
	StateUnprotected State = "unprotected"
	// StateUnenforceable: the forge's plan cannot enforce protection on this repo (e.g. a
	// private repo on GitHub Free), so server-side protection is not merely absent — it
	// is unavailable until the plan or visibility changes.
	StateUnenforceable State = "unenforceable"
	// StateUnknown: leadkit could not tell. NEVER to be presented as verified.
	StateUnknown State = "unknown"
)

// Status is what Inspect found for one branch. Detail explains State in words, and for
// StateUnknown says why leadkit could not tell. Gaps is only populated for StatePartial.
type Status struct {
	Branch string
	State  State
	Detail string
	Gaps   []string // for StatePartial: each property that falls short

	ghRules *ghRules // GitHub only: the rule exactly as read, so a write can carry it forward
	// GitLab only: the rule named exactly Branch, if any (for the write plan), and the
	// reasons a write to it cannot make the branch meet the standard (covering rules that
	// are weak and that a write to the exact rule would not touch).
	gitlabExact     *glBranch
	gitlabUnfixable []string
}

// Verified reports whether the branch is protected server-side to leadkit's standard.
func (s Status) Verified() bool { return s.State == StateVerified }

// The standard, stated once. A branch is Verified only if the forge reports ALL of:
//   - changes must arrive through a pull/merge request (no direct push),
//   - force pushes are rejected,
//   - the branch cannot be deleted,
//   - and, on GitHub, admins are held to the same rules — the agent's token is usually
//     its owner's, so a rule admins can bypass does not stop it.
//
// GitLab has no enforce-admins switch; "no direct push" there means the push access
// level is "No one", which binds maintainers and owners too.

// Inspect reads branch's protection from the forge and classifies it. It performs reads
// only.
func Inspect(r Runner, t Target, branch string) Status {
	if t.Kind == KindGitLab {
		return inspectGitLab(r, t, branch)
	}
	return inspectGitHub(r, t, branch)
}

// ---- GitHub ----------------------------------------------------------------

type ghFlag struct {
	Enabled bool `json:"enabled"`
}

type ghProtection struct {
	RequiredPullRequestReviews json.RawMessage `json:"required_pull_request_reviews"`
	EnforceAdmins              *ghFlag         `json:"enforce_admins"`
	AllowForcePushes           *ghFlag         `json:"allow_force_pushes"`
	AllowDeletions             *ghFlag         `json:"allow_deletions"`
	RequiredStatusChecks       *struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	} `json:"required_status_checks"`
}

func inspectGitHub(r Runner, t Target, branch string) Status {
	st := Status{Branch: branch}
	base := "repos/" + t.Repo + "/branches/" + url.PathEscape(branch)
	resp := call(r, t, "GET", base+"/protection", nil)

	switch {
	case resp.ok():
		var p ghProtection
		if err := json.Unmarshal(resp.Body, &p); err != nil {
			return unknown(st, "the forge returned a protection document leadkit could not parse: "+err.Error())
		}
		rules, err := parseGHRules(resp.Body)
		if err != nil {
			return unknown(st, "the forge returned a protection document leadkit could not model: "+err.Error())
		}
		st.ghRules = &rules
		return evalGitHub(st, p, rules)

	case resp.Status == 404 && strings.Contains(resp.Msg, "Branch not protected"):
		// No classic protection rule. A repository ruleset can still govern the branch
		// and does not show up above, so ask for the rules that apply to it.
		rules := call(r, t, "GET", "repos/"+t.Repo+"/rules/branches/"+url.PathEscape(branch), nil)
		if !rules.ok() {
			return unknown(st, "no classic protection rule, and the applicable rulesets could not be read ("+describe(rules)+")")
		}
		var list []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rules.Body, &list); err != nil {
			return unknown(st, "no classic protection rule, and the ruleset response could not be parsed: "+err.Error())
		}
		if len(list) > 0 {
			var types []string
			for _, x := range list {
				types = append(types, x.Type)
			}
			sort.Strings(types)
			return unknown(st, "governed by repository ruleset rule(s) ["+strings.Join(types, ", ")+"], which leadkit does not inspect (bypass actors in particular) — verify them by hand")
		}
		st.State = StateUnprotected
		st.Detail = "no protection rule and no ruleset covers this branch"
		return st

	case resp.Status == 403 && strings.Contains(resp.Msg, "Upgrade to GitHub Pro"):
		st.State = StateUnenforceable
		st.Detail = "the forge cannot enforce branch protection on this repository's plan (" + resp.Msg + ")"
		return st

	case resp.Status == 404 && strings.Contains(resp.Msg, "Branch not found"):
		return unknown(st, "the branch does not exist on the forge")
	}
	return unknown(st, describe(resp))
}

func evalGitHub(st Status, p ghProtection, rules ghRules) Status {
	var gaps []string
	if len(p.RequiredPullRequestReviews) == 0 || string(p.RequiredPullRequestReviews) == "null" {
		gaps = append(gaps, "direct pushes are allowed (no pull request required)")
	}
	// An actor on the bypass list skips the pull request requirement, which is the very
	// hole Layer 0 exists to close (an agent's token on that list sails past the rule).
	if rules.PR != nil && !rules.PR.Bypass.empty() {
		gaps = append(gaps, "the pull request requirement can be bypassed by "+strings.Join(rules.PR.Bypass.names(), ", "))
	}
	if p.AllowForcePushes == nil || p.AllowForcePushes.Enabled {
		gaps = append(gaps, "force pushes are allowed or not reported")
	}
	if p.AllowDeletions == nil || p.AllowDeletions.Enabled {
		gaps = append(gaps, "branch deletion is allowed or not reported")
	}
	if p.EnforceAdmins == nil || !p.EnforceAdmins.Enabled {
		gaps = append(gaps, "admins can bypass the rule (enforce_admins is off or not reported)")
	}
	if len(gaps) > 0 {
		st.State = StatePartial
		st.Gaps = gaps
		st.Detail = "protected, but not to the required standard: " + strings.Join(gaps, "; ")
		return st
	}
	st.State = StateVerified
	st.Detail = "pull request required, force-push and deletion blocked, admins included" + checksNote(p)
	return st
}

func checksNote(p ghProtection) string {
	if p.RequiredStatusChecks == nil {
		return "; no required status checks"
	}
	seen := map[string]bool{}
	var names []string
	for _, c := range p.RequiredStatusChecks.Contexts {
		if !seen[c] {
			seen[c] = true
			names = append(names, c)
		}
	}
	for _, c := range p.RequiredStatusChecks.Checks {
		if c.Context != "" && !seen[c.Context] {
			seen[c.Context] = true
			names = append(names, c.Context)
		}
	}
	if len(names) == 0 {
		return "; no required status checks"
	}
	sort.Strings(names)
	return "; required checks: " + strings.Join(names, ", ")
}

// ---- GitLab ----------------------------------------------------------------

type glLevel struct {
	ID          int  `json:"id"`
	AccessLevel int  `json:"access_level"`
	UserID      *int `json:"user_id"`
	GroupID     *int `json:"group_id"`
	DeployKeyID *int `json:"deploy_key_id"`
}

type glBranch struct {
	Name             string    `json:"name"`
	PushAccessLevels []glLevel `json:"push_access_levels"`
	AllowForcePush   *bool     `json:"allow_force_push"`

	// Not part of the standard; read so the post-write read-back can assert a push-access
	// write left them alone.
	MergeAccessLevels     []glLevel `json:"merge_access_levels"`
	UnprotectAccessLevels []glLevel `json:"unprotect_access_levels"`
	CodeOwnerApproval     bool      `json:"code_owner_approval_required"`
}

// glNoOne is GitLab's "No one" access level.
const glNoOne = 0

func inspectGitLab(r Runner, t Target, branch string) Status {
	st := Status{Branch: branch}
	proj := "projects/" + t.repoPath()
	if resp := call(r, t, "GET", proj, nil); !resp.ok() {
		return unknown(st, "cannot read the project ("+describe(resp)+")")
	}
	// List rather than GET-by-name: a wildcard rule (release/*, *) covers a branch
	// without sharing its name, and a by-name 404 would wrongly read as "unprotected".
	const page = 100
	resp := call(r, t, "GET", fmt.Sprintf("%s/protected_branches?per_page=%d", proj, page), nil)
	if !resp.ok() {
		return unknown(st, "cannot list protected branches ("+describe(resp)+")")
	}
	var all []glBranch
	if err := json.Unmarshal(resp.Body, &all); err != nil {
		return unknown(st, "the protected-branch list could not be parsed: "+err.Error())
	}
	if len(all) >= page {
		return unknown(st, fmt.Sprintf("%d or more protected-branch rules; leadkit does not page through them", page))
	}

	var covering []glBranch
	for i := range all {
		if all[i].Name == branch {
			b := all[i]
			st.gitlabExact = &b
		}
		if globCovers(all[i].Name, branch) {
			covering = append(covering, all[i])
		}
	}
	if len(covering) == 0 {
		st.State = StateUnprotected
		st.Detail = "no protected-branch rule covers this branch"
		return st
	}

	// GitLab grants the union of every covering rule, so one weak rule weakens the lot.
	var gaps []string
	for _, b := range covering {
		for _, g := range evalGitLabRule(b) {
			gaps = append(gaps, fmt.Sprintf("rule %q: %s", b.Name, g))
		}
	}
	if len(gaps) > 0 {
		st.State = StatePartial
		st.Gaps = gaps
		st.Detail = "protected, but not to the required standard: " + strings.Join(gaps, "; ")
		// GitLab grants the union of covering rules. A write only ever edits the exact
		// rule, so a weak rule of another name keeps the branch open whatever is written.
		for _, b := range covering {
			if b.Name == branch {
				continue
			}
			for _, g := range evalGitLabRule(b) {
				st.gitlabUnfixable = append(st.gitlabUnfixable, fmt.Sprintf("rule %q: %s", b.Name, g))
			}
		}
		return st
	}
	st.State = StateVerified
	st.Detail = "push access is \"No one\" (changes arrive by merge request) and force-push is blocked"
	return st
}

func evalGitLabRule(b glBranch) []string {
	var gaps []string
	if len(b.PushAccessLevels) == 0 {
		gaps = append(gaps, "no push access level reported")
	}
	for _, l := range b.PushAccessLevels {
		if l.AccessLevel != glNoOne || l.UserID != nil || l.GroupID != nil || l.DeployKeyID != nil {
			gaps = append(gaps, "direct push is allowed to "+describeLevel(l))
		}
	}
	if b.AllowForcePush == nil || *b.AllowForcePush {
		gaps = append(gaps, "force push is allowed or not reported")
	}
	return gaps
}

func describeLevel(l glLevel) string {
	switch {
	case l.UserID != nil:
		return fmt.Sprintf("user %d", *l.UserID)
	case l.GroupID != nil:
		return fmt.Sprintf("group %d", *l.GroupID)
	case l.DeployKeyID != nil:
		return fmt.Sprintf("deploy key %d", *l.DeployKeyID)
	}
	switch l.AccessLevel {
	case 30:
		return "Developers"
	case 40:
		return "Maintainers"
	case 60:
		return "Admins"
	}
	return fmt.Sprintf("access level %d", l.AccessLevel)
}

// globCovers reports whether a GitLab protected-branch name (exact or `*` wildcard)
// covers branch. GitLab's `*` matches any run of characters, slashes included.
func globCovers(pattern, branch string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == branch
	}
	parts := strings.Split(pattern, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(branch)
}

func unknown(st Status, why string) Status {
	st.State = StateUnknown
	st.Detail = "cannot tell: " + why
	return st
}
