package forge

import (
	"fmt"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

func ghInspect(t *testing.T, replies map[string]reply) Status {
	t.Helper()
	return Inspect(newFake(replies), ghTarget, "main")
}

func TestInspectGitHub_VerifiedOnlyWhenEveryPropertyIsAffirmative(t *testing.T) {
	st := ghInspect(t, map[string]reply{ghProtPath: {Body: ghStrictJSON}})
	if st.State != StateVerified {
		t.Fatalf("want verified, got %s: %s", st.State, st.Detail)
	}
	if !strings.Contains(st.Detail, "ci/build") {
		t.Errorf("detail should name the required check, got %q", st.Detail)
	}
}

func TestInspectGitHub_EachMissingPropertyIsPartialNeverVerified(t *testing.T) {
	cases := []struct {
		name, body, gap string
	}{
		{"admins can bypass", `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":false},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`, "admins can bypass"},
		{"no PR required", `{"required_pull_request_reviews":null,"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`, "no pull request required"},
		{"force push allowed", `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":true},"allow_deletions":{"enabled":false}}`, "force pushes"},
		{"deletion allowed", `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":true}}`, "deletion"},
		// A key the forge did not report is not evidence of safety.
		{"force-push key absent", `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":true},"allow_deletions":{"enabled":false}}`, "force pushes"},
		{"empty document", `{}`, "no pull request required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := ghInspect(t, map[string]reply{ghProtPath: {Body: c.body}})
			if st.State != StatePartial {
				t.Fatalf("want partial, got %s: %s", st.State, st.Detail)
			}
			if !strings.Contains(st.Detail, c.gap) {
				t.Errorf("detail %q should mention %q", st.Detail, c.gap)
			}
		})
	}
}

func TestInspectGitHub_Unprotected(t *testing.T) {
	st := ghInspect(t, map[string]reply{
		ghProtPath:  {Status: 404, Msg: "Branch not protected"},
		ghRulesPath: {Body: `[]`},
	})
	if st.State != StateUnprotected {
		t.Fatalf("want unprotected, got %s: %s", st.State, st.Detail)
	}
}

// A ruleset can protect a branch without a classic rule, so "no classic rule" alone must
// not read as unprotected, and leadkit does not inspect rulesets, so it must not read as
// verified either.
func TestInspectGitHub_RulesetCoverageIsUnknown(t *testing.T) {
	st := ghInspect(t, map[string]reply{
		ghProtPath:  {Status: 404, Msg: "Branch not protected"},
		ghRulesPath: {Body: `[{"type":"pull_request"},{"type":"non_fast_forward"}]`},
	})
	if st.State != StateUnknown {
		t.Fatalf("want unknown, got %s: %s", st.State, st.Detail)
	}
	if !strings.Contains(st.Detail, "pull_request") {
		t.Errorf("detail should name the ruleset rule types, got %q", st.Detail)
	}
}

func TestInspectGitHub_UnreadableStatesAreUnknown(t *testing.T) {
	cases := map[string]map[string]reply{
		"rules endpoint 403":            {ghProtPath: {Status: 404, Msg: "Branch not protected"}, ghRulesPath: {Status: 403, Msg: "Resource not accessible"}},
		"rules unparseable":             {ghProtPath: {Status: 404, Msg: "Branch not protected"}, ghRulesPath: {Body: `not json`}},
		"branch does not exist":         {ghProtPath: {Status: 404, Msg: "Branch not found"}},
		"no access (404 Not Found)":     {ghProtPath: {Status: 404, Msg: "Not Found"}},
		"unauthenticated":               {ghProtPath: {Status: 401, Msg: "Bad credentials"}},
		"gh not installed":              {ghProtPath: {Err: errNotInstalled}},
		"network error, no HTTP status": {ghProtPath: {Err: fmt.Errorf("exit status 1")}},
		"unparseable document":          {ghProtPath: {Body: `<html>`}},
		"403 that is not a plan limit":  {ghProtPath: {Status: 403, Msg: "Must have admin rights to Repository."}},
	}
	for name, replies := range cases {
		t.Run(name, func(t *testing.T) {
			st := ghInspect(t, replies)
			if st.State != StateUnknown {
				t.Fatalf("want unknown, got %s: %s", st.State, st.Detail)
			}
			if st.Verified() {
				t.Fatal("an unreadable state must never be Verified")
			}
		})
	}
}

func TestInspectGitHub_MissingCLIIsSaidPlainly(t *testing.T) {
	st := ghInspect(t, map[string]reply{ghProtPath: {Err: errNotInstalled}})
	if !strings.Contains(st.Detail, "gh is not installed") {
		t.Errorf("want a plain 'gh is not installed', got %q", st.Detail)
	}
}

// This repo is private on GitHub Free: the forge cannot enforce protection at all. That
// is its own state, not "unprotected" (a write cannot fix it) and not "unknown".
func TestInspectGitHub_PlanCannotEnforce(t *testing.T) {
	st := ghInspect(t, map[string]reply{ghProtPath: {Status: 403, Msg: "Upgrade to GitHub Pro or make this repository public to enable this feature."}})
	if st.State != StateUnenforceable {
		t.Fatalf("want unenforceable, got %s: %s", st.State, st.Detail)
	}
}

func TestInspectGitHub_BranchNameWithSlashIsEscaped(t *testing.T) {
	f := newFake(map[string]reply{"GET repos/acme/widgets/branches/release%2F1.0/protection": {Body: ghStrictJSON}})
	if st := Inspect(f, ghTarget, "release/1.0"); st.State != StateVerified {
		t.Fatalf("want verified, got %s: %s", st.State, st.Detail)
	}
}

// ---- GitLab ----

func glList(body string) map[string]reply {
	return map[string]reply{glProjPath: {Body: `{"id":1}`}, glListPath: {Body: body}}
}

const glStrict = `{"id":1,"name":"main","push_access_levels":[{"id":10,"access_level":0,"user_id":null,"group_id":null,"deploy_key_id":null}],"merge_access_levels":[{"access_level":40}],"allow_force_push":false}`

func TestInspectGitLab(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  State
		gapIn string
	}{
		{"strict exact rule", `[` + glStrict + `]`, StateVerified, ""},
		{"no rules at all", `[]`, StateUnprotected, ""},
		{"maintainers may push", `[{"name":"main","push_access_levels":[{"id":9,"access_level":40}],"allow_force_push":false}]`, StatePartial, "Maintainers"},
		{"developers may push", `[{"name":"main","push_access_levels":[{"id":9,"access_level":30}],"allow_force_push":false}]`, StatePartial, "Developers"},
		{"a named user may push", `[{"name":"main","push_access_levels":[{"id":9,"access_level":40,"user_id":77}],"allow_force_push":false}]`, StatePartial, "user 77"},
		{"force push allowed", `[{"name":"main","push_access_levels":[{"id":9,"access_level":0}],"allow_force_push":true}]`, StatePartial, "force push"},
		{"force push not reported", `[{"name":"main","push_access_levels":[{"id":9,"access_level":0}]}]`, StatePartial, "force push"},
		{"no push levels reported", `[{"name":"main","push_access_levels":[],"allow_force_push":false}]`, StatePartial, "no push access level"},
		{"non-covering wildcard ignored", `[{"name":"release/*","push_access_levels":[{"access_level":40}],"allow_force_push":true}]`, StateUnprotected, ""},
		{"covering wildcard, strict", `[{"name":"*","push_access_levels":[{"access_level":0}],"allow_force_push":false}]`, StateVerified, ""},
		// GitLab grants the union of covering rules: a weak wildcard defeats a strict exact rule.
		{"strict exact + weak covering wildcard", `[` + glStrict + `,{"name":"ma*","push_access_levels":[{"access_level":40}],"allow_force_push":false}]`, StatePartial, `rule "ma*"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := Inspect(newFake(glList(c.body)), glTarget, "main")
			if st.State != c.want {
				t.Fatalf("want %s, got %s: %s", c.want, st.State, st.Detail)
			}
			if c.gapIn != "" && !strings.Contains(st.Detail, c.gapIn) {
				t.Errorf("detail %q should mention %q", st.Detail, c.gapIn)
			}
		})
	}
}

func TestInspectGitLab_UnreadableStatesAreUnknown(t *testing.T) {
	hundred := "[" + strings.TrimSuffix(strings.Repeat(`{"name":"x"},`, 100), ",") + "]"
	cases := map[string]map[string]reply{
		"project unreadable": {glProjPath: {Status: 404, Msg: "404 Project Not Found"}},
		"list forbidden":     {glProjPath: {Body: `{}`}, glListPath: {Status: 403, Msg: "403 Forbidden"}},
		"list unparseable":   {glProjPath: {Body: `{}`}, glListPath: {Body: `nope`}},
		"glab missing":       {glProjPath: {Err: fmt.Errorf("exec: %q: %w", "glab", errNotInstalled)}},
		"too many rules":     glList(hundred),
	}
	for name, replies := range cases {
		t.Run(name, func(t *testing.T) {
			st := Inspect(newFake(replies), glTarget, "main")
			if st.State != StateUnknown {
				t.Fatalf("want unknown, got %s: %s", st.State, st.Detail)
			}
		})
	}
}

func TestGlobCovers(t *testing.T) {
	cases := []struct {
		pattern, branch string
		want            bool
	}{
		{"main", "main", true}, {"main", "mainline", false},
		{"*", "anything/at/all", true}, {"release/*", "release/1.0", true},
		{"release/*", "main", false}, {"*-stable", "1-stable", true},
		{"*-stable", "1-stable-x", false}, {"a.b", "aXb", false}, {"a.*", "a.b", true},
	}
	for _, c := range cases {
		if got := globCovers(c.pattern, c.branch); got != c.want {
			t.Errorf("globCovers(%q,%q) = %v, want %v", c.pattern, c.branch, got, c.want)
		}
	}
}

// Doctor calls Inspect and Branches on a schedule, so they are the read path: a forge answer
// of any shape, including the unprotected one a repair would be tempted by, must not make
// them issue a write. The fake records a call before it looks up a reply, so a stray write is
// seen even though no reply is registered for it.
func TestInspectAndBranches_NeverWrite(t *testing.T) {
	unprotected := map[string]reply{ghProtPath: {Status: 404, Msg: "Branch not protected"}, ghRulesPath: {Body: `[]`}}
	cases := []struct {
		name string
		run  func(f *fakeRunner)
		fake map[string]reply
	}{
		{"github verified", func(f *fakeRunner) { Inspect(f, ghTarget, "main") }, map[string]reply{ghProtPath: {Body: ghStrictJSON}}},
		{"github rich but admins exempt", func(f *fakeRunner) { Inspect(f, ghTarget, "main") }, map[string]reply{ghProtPath: {Body: ghRichJSON}}},
		{"github unprotected", func(f *fakeRunner) { Inspect(f, ghTarget, "main") }, unprotected},
		{"github unreadable", func(f *fakeRunner) { Inspect(f, ghTarget, "main") }, map[string]reply{ghProtPath: {Status: 401, Msg: "Bad credentials"}}},
		{"gitlab no rules", func(f *fakeRunner) { Inspect(f, glTarget, "main") }, glList(`[]`)},
		{"gitlab partial", func(f *fakeRunner) { Inspect(f, glTarget, "main") },
			glList(`[{"name":"main","push_access_levels":[{"id":9,"access_level":40}],"allow_force_push":false}]`)},
		{"github default branch", func(f *fakeRunner) { Branches(f, ghTarget, config.AuthoritySpec{}) }, map[string]reply{ghRepoPath: {Body: `{"default_branch":"main"}`}}},
		{"gitlab default branch", func(f *fakeRunner) { Branches(f, glTarget, config.AuthoritySpec{}) }, map[string]reply{glProjPath: {Body: `{"default_branch":"main"}`}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(c.fake)
			c.run(f)
			if len(f.calls) == 0 {
				t.Fatal("the scenario made no forge call, so a clean write list proves nothing")
			}
			if w := f.writes(); len(w) != 0 {
				t.Errorf("a read path issued %d write(s): %v", len(w), w)
			}
		})
	}
}
