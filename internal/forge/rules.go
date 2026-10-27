package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ghRules is leadkit's model of one GitHub branch-protection rule: every property of the
// classic rule that PUT /protection accepts. It exists because that endpoint is a full
// REPLACE: whatever a request does not carry is reset. So a write must start from what is
// there (parseGHRules on the GET), change only what leadkit means to change (mergeGH),
// and send ALL of it back (encode). The same model decodes the GET document and the PUT
// body, which is what lets the plan diff the bytes actually sent against what is there.
type ghRules struct {
	PR       *ghPR // nil: no pull request is required
	Checks   *ghChecks
	Restrict *actors // nil: anyone with write access may push

	EnforceAdmins bool
	AllowForce    bool
	AllowDelete   bool
	LinearHistory bool
	BlockCreation bool
	ConvResolve   bool
	LockBranch    bool
	ForkSync      bool

	// Unmodeled names properties the forge reported as in force that this model cannot
	// carry through a PUT (e.g. required_signatures, which has its own endpoint). A write
	// would drop them, so the diff reports each as a removal and Decide refuses.
	Unmodeled []string
}

type ghPR struct {
	Approvals    int
	CodeOwners   bool
	DismissStale bool
	LastPush     bool
	Dismiss      *actors // nil: anyone may dismiss reviews
	Bypass       actors  // actors exempt from the pull request requirement
}

type ghChecks struct {
	Strict bool
	Checks []ghCheck // sorted by context
}

type ghCheck struct {
	Context string
	AppID   *int // nil: not pinned to an app
}

// actors is a set of users/teams/apps, each sorted. Entries are "user:x", "team:x", "app:x".
type actors struct{ Users, Teams, Apps []string }

func (a actors) names() []string {
	var out []string
	for _, u := range a.Users {
		out = append(out, "user:"+u)
	}
	for _, t := range a.Teams {
		out = append(out, "team:"+t)
	}
	for _, p := range a.Apps {
		out = append(out, "app:"+p)
	}
	return out
}

func (a actors) empty() bool { return len(a.Users)+len(a.Teams)+len(a.Apps) == 0 }

func (a actors) count() string {
	var parts []string
	for _, c := range []struct {
		n    int
		word string
	}{{len(a.Users), "user"}, {len(a.Teams), "team"}, {len(a.Apps), "app"}} {
		if c.n > 0 {
			parts = append(parts, plural(c.n, c.word))
		}
	}
	if len(parts) == 0 {
		return "nobody"
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ---- decoding: one decoder for the GET document and the PUT body ----

// flexBool reads either a bare bool (PUT body) or {"enabled": bool, ...} (GET document).
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	var v bool
	if json.Unmarshal(raw, &v) == nil {
		*b = flexBool(v)
		return nil
	}
	var o struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	*b = flexBool(o.Enabled)
	return nil
}

// flexActor reads a login/slug string (PUT body) or a user/team/app object (GET document).
type flexActor string

func (a *flexActor) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		*a = flexActor(s)
		return nil
	}
	var o struct {
		Login string `json:"login"`
		Slug  string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	switch {
	case o.Login != "":
		*a = flexActor(o.Login)
	case o.Slug != "":
		*a = flexActor(o.Slug)
	default:
		return fmt.Errorf("an actor with neither a login nor a slug: %s", raw)
	}
	return nil
}

type rawActors struct {
	Users []flexActor `json:"users"`
	Teams []flexActor `json:"teams"`
	Apps  []flexActor `json:"apps"`
}

func (r *rawActors) set() *actors {
	if r == nil {
		return nil
	}
	conv := func(in []flexActor) []string {
		out := make([]string, 0, len(in))
		for _, x := range in {
			out = append(out, string(x))
		}
		sort.Strings(out)
		return out
	}
	return &actors{Users: conv(r.Users), Teams: conv(r.Teams), Apps: conv(r.Apps)}
}

type rawGH struct {
	Checks *struct {
		Strict   bool     `json:"strict"`
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
			AppID   *int   `json:"app_id"`
		} `json:"checks"`
	} `json:"required_status_checks"`
	EnforceAdmins flexBool `json:"enforce_admins"`
	Reviews       *struct {
		Approvals    int        `json:"required_approving_review_count"`
		CodeOwners   bool       `json:"require_code_owner_reviews"`
		DismissStale bool       `json:"dismiss_stale_reviews"`
		LastPush     bool       `json:"require_last_push_approval"`
		Dismiss      *rawActors `json:"dismissal_restrictions"`
		Bypass       *rawActors `json:"bypass_pull_request_allowances"`
	} `json:"required_pull_request_reviews"`
	Restrictions  *rawActors `json:"restrictions"`
	LinearHistory flexBool   `json:"required_linear_history"`
	AllowForce    flexBool   `json:"allow_force_pushes"`
	AllowDelete   flexBool   `json:"allow_deletions"`
	BlockCreation flexBool   `json:"block_creations"`
	ConvResolve   flexBool   `json:"required_conversation_resolution"`
	LockBranch    flexBool   `json:"lock_branch"`
	ForkSync      flexBool   `json:"allow_fork_syncing"`
}

// modeledKeys are the top-level properties ghRules carries. Any other key the forge
// reports as in force lands in Unmodeled.
var modeledKeys = map[string]bool{
	"required_status_checks": true, "enforce_admins": true, "required_pull_request_reviews": true,
	"restrictions": true, "required_linear_history": true, "allow_force_pushes": true,
	"allow_deletions": true, "block_creations": true, "required_conversation_resolution": true,
	"lock_branch": true, "allow_fork_syncing": true,
}

func parseGHRules(doc []byte) (ghRules, error) {
	var raw rawGH
	if err := json.Unmarshal(doc, &raw); err != nil {
		return ghRules{}, err
	}
	r := ghRules{
		EnforceAdmins: bool(raw.EnforceAdmins), AllowForce: bool(raw.AllowForce),
		AllowDelete: bool(raw.AllowDelete), LinearHistory: bool(raw.LinearHistory),
		BlockCreation: bool(raw.BlockCreation), ConvResolve: bool(raw.ConvResolve),
		LockBranch: bool(raw.LockBranch), ForkSync: bool(raw.ForkSync),
		Restrict: raw.Restrictions.set(),
	}
	if rv := raw.Reviews; rv != nil {
		r.PR = &ghPR{Approvals: rv.Approvals, CodeOwners: rv.CodeOwners, DismissStale: rv.DismissStale,
			LastPush: rv.LastPush, Dismiss: rv.Dismiss.set()}
		if b := rv.Bypass.set(); b != nil {
			r.PR.Bypass = *b
		}
	}
	if c := raw.Checks; c != nil {
		r.Checks = &ghChecks{Strict: c.Strict}
		seen := map[string]bool{}
		for _, x := range c.Checks {
			if x.Context != "" && !seen[x.Context] {
				seen[x.Context] = true
				r.Checks.Checks = append(r.Checks.Checks, ghCheck{x.Context, pin(x.AppID)})
			}
		}
		for _, x := range c.Contexts {
			if x != "" && !seen[x] {
				seen[x] = true
				r.Checks.Checks = append(r.Checks.Checks, ghCheck{Context: x})
			}
		}
		sort.Slice(r.Checks.Checks, func(i, j int) bool { return r.Checks.Checks[i].Context < r.Checks.Checks[j].Context })
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return ghRules{}, err
	}
	for k, v := range top {
		if modeledKeys[k] || k == "url" || strings.HasSuffix(k, "_url") || inertValue(v) {
			continue
		}
		r.Unmodeled = append(r.Unmodeled, k)
	}
	sort.Strings(r.Unmodeled)
	return r, nil
}

// inertValue reports whether a property the model does not know says nothing is in force:
// null, false, or {"enabled": false} with at most url-ish siblings.
func inertValue(v json.RawMessage) bool {
	s := bytes.TrimSpace(v)
	if string(s) == "null" || string(s) == "false" {
		return true
	}
	var o map[string]json.RawMessage
	if json.Unmarshal(s, &o) != nil {
		return false
	}
	for k, x := range o {
		if k == "url" || strings.HasSuffix(k, "_url") {
			continue
		}
		if k == "enabled" && string(bytes.TrimSpace(x)) == "false" {
			continue
		}
		return false
	}
	return true
}

// pin normalises "not pinned": GitHub spells it null, or -1 when sent.
func pin(id *int) *int {
	if id == nil || *id == -1 {
		return nil
	}
	return id
}

// ---- merging and encoding ----

// mergeGH overlays leadkit's standard on what is already there: a pull request is
// required, force-push and deletion are blocked, admins are bound, and no actor is
// exempt from the pull request requirement. Everything else carries forward untouched,
// and a requested status check is added to (never substituted for) the existing ones.
func mergeGH(cur ghRules, requireChecks []string) ghRules {
	out := cur
	if cur.PR == nil {
		out.PR = &ghPR{}
	} else {
		pr := *cur.PR
		out.PR = &pr
	}
	out.PR.Bypass = actors{}
	out.EnforceAdmins, out.AllowForce, out.AllowDelete = true, false, false

	if len(requireChecks) > 0 {
		c := ghChecks{Strict: true}
		have := map[string]bool{}
		if cur.Checks != nil {
			c.Checks = append(c.Checks, cur.Checks.Checks...)
			for _, x := range c.Checks {
				have[x.Context] = true
			}
		}
		for _, name := range requireChecks {
			if name != "" && !have[name] {
				have[name] = true
				c.Checks = append(c.Checks, ghCheck{Context: name})
			}
		}
		sort.Slice(c.Checks, func(i, j int) bool { return c.Checks[i].Context < c.Checks[j].Context })
		out.Checks = &c
	}
	return out
}

func (a *actors) body() any {
	if a == nil {
		return nil
	}
	arr := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return map[string]any{"users": arr(a.Users), "teams": arr(a.Teams), "apps": arr(a.Apps)}
}

// encode renders r as a PUT /protection body. The keys the endpoint requires are
// always present (null when the rule is absent); every other property is sent explicitly
// so a replace cannot reset it.
func (r ghRules) encode() []byte {
	var checks any
	if r.Checks != nil {
		contexts := []string{}
		var pinned []map[string]any
		anyPin := false
		for _, c := range r.Checks.Checks {
			contexts = append(contexts, c.Context)
			e := map[string]any{"context": c.Context}
			if c.AppID != nil {
				e["app_id"] = *c.AppID
				anyPin = true
			} else {
				e["app_id"] = -1
			}
			pinned = append(pinned, e)
		}
		m := map[string]any{"strict": r.Checks.Strict, "contexts": contexts}
		if anyPin {
			m["checks"] = pinned
		}
		checks = m
	}
	var reviews any
	if r.PR != nil {
		m := map[string]any{
			"required_approving_review_count": r.PR.Approvals,
			"require_code_owner_reviews":      r.PR.CodeOwners,
			"dismiss_stale_reviews":           r.PR.DismissStale,
			"require_last_push_approval":      r.PR.LastPush,
		}
		if r.PR.Dismiss != nil {
			m["dismissal_restrictions"] = r.PR.Dismiss.body()
		}
		if !r.PR.Bypass.empty() {
			m["bypass_pull_request_allowances"] = r.PR.Bypass.body()
		}
		reviews = m
	}
	return mustJSON(map[string]any{
		"required_status_checks":           checks,
		"enforce_admins":                   r.EnforceAdmins,
		"required_pull_request_reviews":    reviews,
		"restrictions":                     r.Restrict.body(),
		"allow_force_pushes":               r.AllowForce,
		"allow_deletions":                  r.AllowDelete,
		"required_linear_history":          r.LinearHistory,
		"block_creations":                  r.BlockCreation,
		"required_conversation_resolution": r.ConvResolve,
		"lock_branch":                      r.LockBranch,
		"allow_fork_syncing":               r.ForkSync,
	})
}

// ---- describing ----

// describe renders the rule in human terms, for the plan. No JSON.
func (r ghRules) describe() string {
	var p []string
	if r.PR == nil {
		p = append(p, "no pull request required (direct pushes allowed)")
	} else {
		p = append(p, "pull request required")
		p = append(p, plural(r.PR.Approvals, "approval")+" required")
		if r.PR.CodeOwners {
			p = append(p, "code-owner review required")
		}
		if r.PR.DismissStale {
			p = append(p, "stale approvals dismissed on new pushes")
		}
		if r.PR.LastPush {
			p = append(p, "approval of the last push required")
		}
		if r.PR.Dismiss != nil {
			p = append(p, "review dismissal restricted to "+r.PR.Dismiss.count())
		}
		if !r.PR.Bypass.empty() {
			p = append(p, "pull request requirement BYPASSABLE by "+strings.Join(r.PR.Bypass.names(), ", "))
		}
	}
	if r.Checks == nil || len(r.Checks.Checks) == 0 {
		p = append(p, "no required status checks")
	} else {
		s := plural(len(r.Checks.Checks), "status check") + " required"
		if r.Checks.Strict {
			s += " (branch must be up to date)"
		}
		p = append(p, s)
	}
	if r.Restrict != nil {
		p = append(p, "pushes restricted to "+r.Restrict.count())
	} else {
		p = append(p, "pushes not restricted to named actors")
	}
	p = append(p, flag(r.EnforceAdmins, "admins bound by the rule", "admins exempt"))
	p = append(p, flag(!r.AllowForce, "force-push blocked", "force-push allowed"))
	p = append(p, flag(!r.AllowDelete, "deletion blocked", "deletion allowed"))
	for _, e := range []struct {
		on   bool
		text string
	}{{r.LinearHistory, "linear history required"}, {r.ConvResolve, "conversation resolution required"},
		{r.LockBranch, "branch locked read-only"}, {r.BlockCreation, "branch creation blocked"}} {
		if e.on {
			p = append(p, e.text)
		}
	}
	for _, k := range r.Unmodeled {
		p = append(p, k+" in force (leadkit cannot carry it through a write)")
	}
	return strings.Join(p, "; ")
}

func flag(on bool, yes, no string) string {
	if on {
		return yes
	}
	return no
}

// ---- diffing ----

// diffGH lists what moving from a to b changes, in the terms the operator decides on.
// REMOVES means a protection a has and b lacks or holds weaker; it is judged by direction,
// not by which keys changed, so a widened push allow-list is a removal and a narrowed one
// is not. The same function judges the plan (current -> the PUT body about to be sent) and
// the read-back (before the write -> after it).
func diffGH(a, b ghRules) []Change {
	var ch []Change
	rm := func(f string, x ...any) { ch = append(ch, Change{Removes, fmt.Sprintf(f, x...)}) }
	add := func(f string, x ...any) { ch = append(ch, Change{Adds, fmt.Sprintf(f, x...)}) }
	chg := func(f string, x ...any) { ch = append(ch, Change{Changes, fmt.Sprintf(f, x...)}) }

	apr, bpr := ghPR{}, ghPR{}
	if a.PR != nil {
		apr = *a.PR
	}
	if b.PR != nil {
		bpr = *b.PR
	}
	switch {
	case a.PR == nil && b.PR != nil:
		add("pull request required")
	case a.PR != nil && b.PR == nil:
		rm("the pull request requirement")
	}
	switch {
	case bpr.Approvals < apr.Approvals:
		rm("required approvals %d -> %d", apr.Approvals, bpr.Approvals)
	case bpr.Approvals > apr.Approvals && a.PR != nil:
		chg("required approvals %d -> %d", apr.Approvals, bpr.Approvals)
	case bpr.Approvals > apr.Approvals:
		add("%s required", plural(bpr.Approvals, "approval"))
	}
	protective := func(was, is bool, what string) {
		switch {
		case was && !is:
			rm("%s", what)
		case !was && is:
			add("%s", what)
		}
	}
	protective(apr.CodeOwners, bpr.CodeOwners, "code-owner review")
	protective(apr.DismissStale, bpr.DismissStale, "dismissal of stale approvals")
	protective(apr.LastPush, bpr.LastPush, "approval of the last push")
	switch {
	case !a.EnforceAdmins && b.EnforceAdmins:
		chg("admins exempt -> admins bound")
	case a.EnforceAdmins && !b.EnforceAdmins:
		rm("admins bound -> admins exempt")
	}
	protective(!a.AllowForce, !b.AllowForce, "force-push blocked")
	protective(!a.AllowDelete, !b.AllowDelete, "deletion blocked")
	protective(a.LinearHistory, b.LinearHistory, "linear history requirement")
	protective(a.ConvResolve, b.ConvResolve, "conversation resolution requirement")
	protective(a.LockBranch, b.LockBranch, "branch lock")
	protective(a.BlockCreation, b.BlockCreation, "branch creation block")
	if a.ForkSync != b.ForkSync {
		chg("fork syncing %v -> %v", a.ForkSync, b.ForkSync)
	}
	// Who may push / dismiss: absent means "anyone", so gaining a list narrows, losing it
	// widens, and a bigger list widens.
	ch = append(ch, diffRestrictive("push restrictions", a.Restrict, b.Restrict)...)
	ch = append(ch, diffRestrictive("review-dismissal restrictions", apr.Dismiss, bpr.Dismiss)...)
	// Who is exempt from the PR requirement: more exempt actors is weaker.
	ab, bb := toSet(apr.Bypass.names()), toSet(bpr.Bypass.names())
	if n := missing(bb, ab); len(n) > 0 {
		rm("pull request bypass newly granted to %s", strings.Join(n, ", "))
	}
	if n := missing(ab, bb); len(n) > 0 {
		chg("pull request bypass for %s closed", strings.Join(n, ", "))
	}

	ch = append(ch, diffChecks(a.Checks, b.Checks)...)

	for _, k := range a.Unmodeled {
		if !contains(b.Unmodeled, k) {
			// b not carrying it is what makes it a removal; if b keeps listing it, it was carried.
			rm("%s (leadkit cannot carry it through this write)", k)
		}
	}
	return sortChanges(ch)
}

func diffChecks(a, b *ghChecks) []Change {
	var ch []Change
	var ac, bc []ghCheck
	var as, bs bool
	if a != nil {
		ac, as = a.Checks, a.Strict
	}
	if b != nil {
		bc, bs = b.Checks, b.Strict
	}
	am, bm := map[string]ghCheck{}, map[string]ghCheck{}
	for _, c := range ac {
		am[c.Context] = c
	}
	for _, c := range bc {
		bm[c.Context] = c
	}
	var lost, gained []string
	for k := range am {
		if _, ok := bm[k]; !ok {
			lost = append(lost, k)
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			gained = append(gained, k)
		}
	}
	sort.Strings(lost)
	sort.Strings(gained)
	if len(lost) > 0 {
		ch = append(ch, Change{Removes, fmt.Sprintf("%s (%s)", plural(len(lost), "required status check"), strings.Join(lost, ", "))})
	}
	if len(gained) > 0 {
		ch = append(ch, Change{Adds, fmt.Sprintf("%s (%s)", plural(len(gained), "required status check"), strings.Join(gained, ", "))})
	}
	var repinned []string
	for k, x := range am {
		if y, ok := bm[k]; ok && !samePin(x.AppID, y.AppID) {
			repinned = append(repinned, k)
		}
	}
	sort.Strings(repinned)
	for _, k := range repinned {
		ch = append(ch, Change{Changes, fmt.Sprintf("status check %s is now required from a different app than before", k)})
	}
	switch {
	case len(ac) > 0 && as && !bs:
		ch = append(ch, Change{Removes, "up-to-date requirement for status checks"})
	case len(bc) > 0 && !as && bs:
		ch = append(ch, Change{Changes, "status checks now require the branch to be up to date"})
	}
	return ch
}

func samePin(a, b *int) bool {
	a, b = pin(a), pin(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// diffRestrictive handles a "who is allowed" list where having no list means everyone.
func diffRestrictive(label string, a, b *actors) []Change {
	var ch []Change
	switch {
	case a == nil && b != nil:
		ch = append(ch, Change{Adds, fmt.Sprintf("%s (%s)", label, b.count())})
	case a != nil && b == nil:
		ch = append(ch, Change{Removes, fmt.Sprintf("%s (%s)", label, a.count())})
	case a != nil && b != nil:
		as, bs := toSet(a.names()), toSet(b.names())
		if n := missing(bs, as); len(n) > 0 {
			ch = append(ch, Change{Removes, fmt.Sprintf("%s now also admit %s", label, strings.Join(n, ", "))})
		}
		if n := missing(as, bs); len(n) > 0 {
			ch = append(ch, Change{Changes, fmt.Sprintf("%s no longer admit %s", label, strings.Join(n, ", "))})
		}
	}
	return ch
}

func toSet(s []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

func missing(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
