package forge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func richStatus(t *testing.T, doc string) Status {
	t.Helper()
	st := ghInspect(t, map[string]reply{ghProtPath: {Body: doc}})
	if st.State != StatePartial || st.ghRules == nil {
		t.Fatalf("setup: want a partial status carrying its rules, got %s: %s", st.State, st.Detail)
	}
	return st
}

func changeTexts(ch []Change, k ChangeKind) []string {
	var out []string
	for _, c := range ch {
		if c.Kind == k {
			out = append(out, c.Text)
		}
	}
	return out
}

func mustContainAll(t *testing.T, haystack []string, needles ...string) {
	t.Helper()
	joined := strings.Join(haystack, "\n")
	for _, n := range needles {
		if !strings.Contains(joined, n) {
			t.Errorf("want a line containing %q in:\n%s", n, joined)
		}
	}
}

// A Partial branch with stricter-than-default settings must keep all of them in the write.
func TestDecide_GitHubWriteCarriesExistingProtectionForward(t *testing.T) {
	d := Decide(governed(ghTarget, richStatus(t, ghRichJSON)))
	if !d.Allowed || d.Call == nil {
		t.Fatalf("want a write, got %+v", d)
	}
	b := decodeBody(t, d.Call)
	rv, _ := b["required_pull_request_reviews"].(map[string]any)
	if rv["required_approving_review_count"] != float64(2) || rv["require_code_owner_reviews"] != true ||
		rv["dismiss_stale_reviews"] != true || rv["require_last_push_approval"] != true {
		t.Errorf("review settings were lowered: %v", rv)
	}
	if dr, _ := rv["dismissal_restrictions"].(map[string]any); !reflect.DeepEqual(dr["users"], []any{"alice"}) || !reflect.DeepEqual(dr["teams"], []any{"leads"}) {
		t.Errorf("dismissal restrictions lost: %v", rv["dismissal_restrictions"])
	}
	rsc, _ := b["required_status_checks"].(map[string]any)
	if !reflect.DeepEqual(rsc["contexts"], []any{"ci/build", "ci/lint", "ci/test"}) || rsc["strict"] != true {
		t.Errorf("status checks lost: %v", b["required_status_checks"])
	}
	if checks, _ := rsc["checks"].([]any); len(checks) != 3 || checks[0].(map[string]any)["app_id"] != float64(15368) {
		t.Errorf("the check pinned to an app lost its pin: %v", rsc["checks"])
	}
	rs, _ := b["restrictions"].(map[string]any)
	if !reflect.DeepEqual(rs["users"], []any{"bob"}) || !reflect.DeepEqual(rs["teams"], []any{"core", "release"}) {
		t.Errorf("push restrictions lost: %v", b["restrictions"])
	}
	if b["required_linear_history"] != true || b["required_conversation_resolution"] != true {
		t.Errorf("other settings lost: %s", d.Call.Body)
	}
	if b["enforce_admins"] != true {
		t.Errorf("the one property that was missing must be set: %s", d.Call.Body)
	}
	if d.Plan == nil || len(d.Plan.Removals()) != 0 {
		t.Fatalf("a correct merge removes nothing, plan: %+v", d.Plan)
	}
	if got := changeTexts(d.Plan.Changes, Changes); !reflect.DeepEqual(got, []string{"admins exempt -> admins bound"}) {
		t.Errorf("the plan should say exactly what changes, got %v", got)
	}
}

// Every protection a fixed-body write would drop from the rich branch must be NAMED in
// the diff.
func TestDiffGH_NamesEveryRemovalTheOldFixedBodyCaused(t *testing.T) {
	cur := richStatus(t, ghRichJSON).ghRules
	old := mustJSON(map[string]any{
		"required_status_checks":        nil,
		"enforce_admins":                true,
		"required_pull_request_reviews": map[string]any{"required_approving_review_count": 0},
		"restrictions":                  nil,
		"allow_force_pushes":            false,
		"allow_deletions":               false,
	})
	sent, err := parseGHRules(old)
	if err != nil {
		t.Fatal(err)
	}
	rm := changeTexts(diffGH(*cur, sent), Removes)
	mustContainAll(t, rm,
		"required approvals 2 -> 0",
		"code-owner review",
		"dismissal of stale approvals",
		"approval of the last push",
		"3 required status checks (ci/build, ci/lint, ci/test)",
		"push restrictions (1 user, 2 teams)",
		"review-dismissal restrictions",
		"linear history requirement",
		"conversation resolution requirement",
	)
	if ch := diffGH(*cur, sent); ch[0].Kind != Removes {
		t.Errorf("removals must come first, got %v", ch[0])
	}
}

func TestPlanDescribesCurrentProtectionInWords(t *testing.T) {
	st := richStatus(t, ghRichJSON)
	d := Decide(governed(ghTarget, st))
	for _, want := range []string{"2 approvals required", "code-owner review required", "3 status checks required",
		"pushes restricted to 1 user, 2 teams", "admins exempt", "force-push blocked"} {
		if !strings.Contains(d.Plan.Current, want) {
			t.Errorf("current protection should say %q: %s", want, d.Plan.Current)
		}
	}
	if strings.ContainsAny(d.Plan.Current, "{}\"") {
		t.Errorf("the description must be words, not JSON: %s", d.Plan.Current)
	}
}

// A property the model cannot carry through a PUT must refuse the write, not be dropped.
func TestDecide_RefusesAWriteThatWouldDropAnUnmodeledProtection(t *testing.T) {
	for name, extra := range map[string]string{
		"required_signatures":     `"required_signatures": {"url":"x","enabled": true}`,
		"a key from the future":   `"required_signatures": {"enabled": false}, "required_future_gate": {"enabled": true}`,
		"a non-object new switch": `"new_switch": true`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(ghRichJSON, `"required_signatures": {"url":"x","enabled": false}`, extra, 1)
			d := Decide(governed(ghTarget, richStatus(t, doc)))
			if d.Allowed || d.Call != nil {
				t.Fatalf("must refuse with no call, got %+v", d)
			}
			if d.Plan == nil || len(d.Plan.Removals()) != 1 {
				t.Fatalf("the plan must name the removal, got %+v", d.Plan)
			}
			if !strings.Contains(d.Reason, "REMOVE 1 existing protection") || !strings.Contains(d.Reason, "never weakens") {
				t.Errorf("the refusal must say what and why: %s", d.Reason)
			}
		})
	}
}

func TestInspectGitHub_DisabledUnknownSwitchIsNotARemoval(t *testing.T) {
	d := Decide(governed(ghTarget, richStatus(t, ghRichJSON))) // required_signatures {enabled:false}
	if !d.Allowed {
		t.Fatalf("a switch that is off carries nothing to lose: %+v", d)
	}
}

func TestInspectGitHub_BypassAllowancesAreAGapNotVerified(t *testing.T) {
	doc := `{"required_pull_request_reviews":{"bypass_pull_request_allowances":{"users":[{"login":"agent-bot"}],"teams":[],"apps":[{"slug":"ci-app"}]}},
	"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`
	st := ghInspect(t, map[string]reply{ghProtPath: {Body: doc}})
	if st.State != StatePartial {
		t.Fatalf("want partial, got %s: %s", st.State, st.Detail)
	}
	mustContainAll(t, st.Gaps, "bypassed by user:agent-bot, app:ci-app")

	// Closing the hole is part of the write, and the plan says so as a change, not a removal.
	d := Decide(governed(ghTarget, st))
	b := decodeBody(t, d.Call)
	if rv, _ := b["required_pull_request_reviews"].(map[string]any); rv["bypass_pull_request_allowances"] != nil {
		t.Errorf("the write must not carry the bypass list forward: %s", d.Call.Body)
	}
	mustContainAll(t, changeTexts(d.Plan.Changes, Changes), "pull request bypass for app:ci-app, user:agent-bot closed")
	if len(d.Plan.Removals()) != 0 {
		t.Errorf("closing a bypass is tightening, not a removal: %v", d.Plan.Removals())
	}
}

func TestDecide_RequireCheckIsAddedToExistingChecksNeverSubstituted(t *testing.T) {
	in := governed(ghTarget, richStatus(t, ghRichJSON))
	in.Opts = Options{RequireChecks: []string{"ci/build", "ci/e2e"}}
	d := Decide(in)
	rsc, _ := decodeBody(t, d.Call)["required_status_checks"].(map[string]any)
	if !reflect.DeepEqual(rsc["contexts"], []any{"ci/build", "ci/e2e", "ci/lint", "ci/test"}) {
		t.Errorf("want the union, got %v", rsc["contexts"])
	}
	if checks, _ := rsc["checks"].([]any); len(checks) != 4 || checks[0].(map[string]any)["app_id"] != float64(15368) {
		t.Errorf("existing pin must survive: %v", rsc["checks"])
	}
	mustContainAll(t, changeTexts(d.Plan.Changes, Adds), "1 required status check (ci/e2e)")
	if len(d.Plan.Removals()) != 0 {
		t.Errorf("removals: %v", d.Plan.Removals())
	}
}

func TestDecide_PartialWithoutItsRulesRefuses(t *testing.T) {
	d := Decide(governed(ghTarget, Status{Branch: "main", State: StatePartial}))
	if d.Allowed || d.Call != nil {
		t.Fatalf("a replace write against a rule that was never read must be refused: %+v", d)
	}
}

func TestDiffGH_DirectionIsJudgedNotJustKeysChanged(t *testing.T) {
	two := &actors{Users: []string{"a", "b"}}
	one := &actors{Users: []string{"a"}}
	cases := []struct {
		name         string
		a, b         ghRules
		wantRemovals int
	}{
		{"narrowing the push list is not a removal", ghRules{Restrict: two}, ghRules{Restrict: one}, 0},
		{"widening the push list is", ghRules{Restrict: one}, ghRules{Restrict: two}, 1},
		{"dropping the push list is", ghRules{Restrict: one}, ghRules{}, 1},
		{"force-push re-enabled is", ghRules{}, ghRules{AllowForce: true}, 1},
		{"approvals raised is not", ghRules{PR: &ghPR{Approvals: 1}}, ghRules{PR: &ghPR{Approvals: 2}}, 0},
		{"strict dropped is", ghRules{Checks: &ghChecks{Strict: true, Checks: []ghCheck{{Context: "x"}}}}, ghRules{Checks: &ghChecks{Checks: []ghCheck{{Context: "x"}}}}, 1},
		{"a new bypass is", ghRules{PR: &ghPR{}}, ghRules{PR: &ghPR{Bypass: actors{Apps: []string{"x"}}}}, 1},
		{"identical is nothing", ghRules{PR: &ghPR{Approvals: 1}, EnforceAdmins: true}, ghRules{PR: &ghPR{Approvals: 1}, EnforceAdmins: true}, 0},
	}
	for _, c := range cases {
		if got := len(changeTexts(diffGH(c.a, c.b), Removes)); got != c.wantRemovals {
			t.Errorf("%s: %d removals, want %d: %v", c.name, got, c.wantRemovals, diffGH(c.a, c.b))
		}
	}
}

// The adversarial read-back: verified is not enough if the write clobbered the rest.
func TestReadBackProblems(t *testing.T) {
	before := richStatus(t, ghRichJSON)
	good := strings.Replace(ghRichJSON, `"enforce_admins": {"url":"x","enabled": false}`, `"enforce_admins": {"enabled": true}`, 1)
	if p := ReadBackProblems(before, Inspect(newFake(map[string]reply{ghProtPath: {Body: good}}), ghTarget, "main")); len(p) != 0 {
		t.Errorf("a write that kept everything must read back clean: %v", p)
	}
	// Verified by leadkit's standard, yet it lost the approvals, code-owner review, checks
	// and restrictions: the read-back must still flag the loss.
	clobbered := `{"required_pull_request_reviews":{"required_approving_review_count":0},"enforce_admins":{"enabled":true},
	"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"restrictions":null,"required_status_checks":null}`
	after := Inspect(newFake(map[string]reply{ghProtPath: {Body: clobbered}}), ghTarget, "main")
	if !after.Verified() {
		t.Fatalf("setup: the clobbered branch must satisfy the old check, got %s", after.State)
	}
	p := ReadBackProblems(before, after)
	mustContainAll(t, p, "required approvals 2 -> 0", "code-owner review", "3 required status checks", "push restrictions")
	// And a forge that ignored the write.
	if p := ReadBackProblems(before, before); len(p) == 0 || !strings.Contains(p[0], "does not report main verified") {
		t.Errorf("an unchanged partial branch must not read back clean: %v", p)
	}
}

func TestParseGHRules_RoundTripsThroughTheWireBody(t *testing.T) {
	r, err := parseGHRules([]byte(ghRichJSON))
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseGHRules(r.encode())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, back) {
		t.Errorf("encode/parse is lossy:\n%+v\n%+v", r, back)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.encode(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"required_status_checks", "enforce_admins", "required_pull_request_reviews", "restrictions"} {
		if _, ok := m[k]; !ok {
			t.Errorf("PUT requires %s even when null", k)
		}
	}
}

// ---- GitLab ----

func TestDecide_GitLabRefusesWhenAWeakWildcardWouldKeepTheBranchOpen(t *testing.T) {
	body := `[{"name":"ma*","push_access_levels":[{"id":5,"access_level":40}],"allow_force_push":false}]`
	cur := Inspect(newFake(glList(body)), glTarget, "main")
	d := Decide(governed(glTarget, cur))
	if d.Allowed || d.Call != nil {
		t.Fatalf("writing an exact rule cannot close a weak wildcard; must refuse before writing: %+v", d)
	}
	if !strings.Contains(d.Reason, `rule "ma*"`) || !strings.Contains(d.Reason, "union") {
		t.Errorf("reason should name the rule and the union: %s", d.Reason)
	}
}

func TestDecide_GitLabPlanNamesWhatChanges(t *testing.T) {
	cur := Inspect(newFake(glList(`[{"name":"main","push_access_levels":[{"id":9,"access_level":40}],"allow_force_push":true}]`)), glTarget, "main")
	d := Decide(governed(glTarget, cur))
	mustContainAll(t, []string{d.Plan.Current}, "direct push allowed to: Maintainers", "force-push allowed")
	mustContainAll(t, changeTexts(d.Plan.Changes, Changes), "direct push to Maintainers -> nobody")
	mustContainAll(t, changeTexts(d.Plan.Changes, Adds), "force-push blocked")
	un := Decide(governed(glTarget, Status{Branch: "main", State: StateUnprotected}))
	mustContainAll(t, changeTexts(un.Plan.Changes, Adds), "a protected-branch rule for main")
}

func TestReadBackProblems_GitLabNotesWhatThePatchWasNotMeantToTouch(t *testing.T) {
	rule := func(extra string) Status {
		return Inspect(newFake(glList(`[{"name":"main","push_access_levels":[{"id":9,"access_level":0}],"allow_force_push":false,`+extra+`}]`)), glTarget, "main")
	}
	before := rule(`"code_owner_approval_required":true,"merge_access_levels":[{"access_level":40}]`)
	if p := ReadBackProblems(before, rule(`"code_owner_approval_required":true,"merge_access_levels":[{"access_level":40}]`)); len(p) != 0 {
		t.Errorf("unchanged must be clean: %v", p)
	}
	p := ReadBackProblems(before, rule(`"code_owner_approval_required":false,"merge_access_levels":[{"access_level":40},{"access_level":30}]`))
	mustContainAll(t, p, "code-owner approval requirement", "merge access now also granted to Developers")
}

// missing builds its answer from a map, whose iteration order is random. Many names, checked repeatedly,
// so removing the sort fails every run.
func TestMissing_IsSortedWhateverTheMapOrder(t *testing.T) {
	a, b := map[string]bool{}, map[string]bool{"present-a": true, "present-b": true}
	var want []string
	for i := 11; i >= 0; i-- {
		name := fmt.Sprintf("check-%02d", i)
		a[name] = true
		want = append([]string{name}, want...)
	}
	a["present-a"], a["present-b"] = true, true
	for run := 0; run < 64; run++ {
		got := missing(a, b)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: missing = %v, want sorted %v", run, got, want)
		}
	}
}
