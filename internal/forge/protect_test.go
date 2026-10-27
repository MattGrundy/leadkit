package forge

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

func decodeBody(t *testing.T, c *Call) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, c.Body)
	}
	return m
}

func governed(target Target, cur Status) DecideInput {
	return DecideInput{OriginURL: "origin", AuthKey: "acme/widgets", HasAuthority: true, Target: target, Current: cur}
}

func TestDecide_RefusesWithoutAuthority(t *testing.T) {
	in := governed(ghTarget, Status{Branch: "main", State: StateUnprotected})
	in.HasAuthority = false
	d := Decide(in)
	if d.Allowed || d.Call != nil {
		t.Fatalf("an ungoverned repo must be refused with no call, got %+v", d)
	}
	if !strings.Contains(d.Reason, "authority") {
		t.Errorf("reason should name the missing authority entry: %q", d.Reason)
	}
}

func TestDecide_RefusesWhenForgeUnresolved(t *testing.T) {
	in := governed(Target{}, Status{Branch: "main", State: StateUnprotected})
	in.TargetErr = errors.New("cannot determine forge")
	if d := Decide(in); d.Allowed || d.Call != nil {
		t.Fatalf("want refusal, got %+v", d)
	}
}

func TestDecide_NeverWritesAgainstStateItCouldNotRead(t *testing.T) {
	for _, s := range []State{StateUnknown, StateUnenforceable} {
		d := Decide(governed(ghTarget, Status{Branch: "main", State: s, Detail: "why"}))
		if d.Allowed || d.Call != nil {
			t.Errorf("state %s: want refusal with no call, got %+v", s, d)
		}
	}
}

func TestDecide_VerifiedIsANoop(t *testing.T) {
	d := Decide(governed(ghTarget, Status{Branch: "main", State: StateVerified, Detail: "ok"}))
	if !d.Allowed || !d.Noop || d.Call != nil {
		t.Fatalf("want an allowed no-op, got %+v", d)
	}
}

func TestDecide_GitHubWriteIsStrict(t *testing.T) {
	d := Decide(governed(ghTarget, Status{Branch: "release/1.0", State: StateUnprotected}))
	if !d.Allowed || d.Call == nil {
		t.Fatalf("want a write, got %+v", d)
	}
	if d.Call.Method != "PUT" || d.Call.Path != "repos/acme/widgets/branches/release%2F1.0/protection" {
		t.Errorf("wrong request: %s %s", d.Call.Method, d.Call.Path)
	}
	b := decodeBody(t, d.Call)
	if b["enforce_admins"] != true || b["allow_force_pushes"] != false || b["allow_deletions"] != false {
		t.Errorf("body must bind admins and block force-push/deletion: %s", d.Call.Body)
	}
	if b["required_pull_request_reviews"] == nil {
		t.Errorf("body must require a pull request: %s", d.Call.Body)
	}
	// GitHub rejects a PUT that omits these keys, even when null.
	for _, k := range []string{"required_status_checks", "restrictions"} {
		if v, present := b[k]; !present || v != nil {
			t.Errorf("%s must be present and null without --require-check, got %v (present=%v)", k, v, present)
		}
	}
	want := []string{"gh", "api", "--hostname", "github.com", "--method", "PUT", "--input", "-",
		"repos/acme/widgets/branches/release%2F1.0/protection"}
	if !reflect.DeepEqual(d.Call.Argv, want) {
		t.Errorf("argv = %v, want %v", d.Call.Argv, want)
	}
}

func TestDecide_GitHubRequireCheck(t *testing.T) {
	in := governed(ghTarget, Status{Branch: "main", State: StateUnprotected})
	in.Opts = Options{RequireChecks: []string{"ci/build", "ci/test"}}
	b := decodeBody(t, Decide(in).Call)
	rsc, _ := b["required_status_checks"].(map[string]any)
	if rsc == nil || rsc["strict"] != true || !reflect.DeepEqual(rsc["contexts"], []any{"ci/build", "ci/test"}) {
		t.Errorf("required_status_checks = %v", b["required_status_checks"])
	}
}

func TestDecide_GitLabCreateWhenNoExactRule(t *testing.T) {
	d := Decide(governed(glTarget, Status{Branch: "main", State: StateUnprotected}))
	if !d.Allowed || d.Call == nil {
		t.Fatalf("want a write, got %+v", d)
	}
	if d.Call.Method != "POST" || d.Call.Path != "projects/acme%2Fgrp%2Fwidgets/protected_branches" {
		t.Errorf("wrong request: %s %s", d.Call.Method, d.Call.Path)
	}
	b := decodeBody(t, d.Call)
	if b["name"] != "main" || b["push_access_level"] != float64(0) || b["allow_force_push"] != false {
		t.Errorf("body = %s", d.Call.Body)
	}
	if got := d.Call.Argv[0]; got != "glab" {
		t.Errorf("argv[0] = %q", got)
	}
	if !strings.Contains(strings.Join(d.Call.Argv, " "), "--header Content-Type: application/json") {
		t.Errorf("a JSON body needs an explicit content type: %v", d.Call.Argv)
	}
}

func TestDecide_GitLabUpdateReplacesExistingPushLevels(t *testing.T) {
	cur := Inspect(newFake(glList(`[{"name":"main","push_access_levels":[{"id":224037991,"access_level":40}],"allow_force_push":false}]`)), glTarget, "main")
	if cur.State != StatePartial {
		t.Fatalf("setup: %s", cur.State)
	}
	d := Decide(governed(glTarget, cur))
	if d.Call == nil || d.Call.Method != "PATCH" || d.Call.Path != "projects/acme%2Fgrp%2Fwidgets/protected_branches/main" {
		t.Fatalf("want a PATCH of the existing rule, got %+v", d.Call)
	}
	b := decodeBody(t, d.Call)
	push, _ := b["allowed_to_push"].([]any)
	if len(push) != 2 {
		t.Fatalf("allowed_to_push = %v", b["allowed_to_push"])
	}
	if first := push[0].(map[string]any); first["id"] != float64(224037991) || first["_destroy"] != true {
		t.Errorf("the existing level must be destroyed by id, got %v", first)
	}
	if second := push[1].(map[string]any); second["access_level"] != float64(0) {
		t.Errorf("the replacement must be 'No one', got %v", second)
	}
	if b["allow_force_push"] != false {
		t.Errorf("force push must be disabled: %s", d.Call.Body)
	}
}

func TestDecide_GitLabRefusesRequireCheck(t *testing.T) {
	in := governed(glTarget, Status{Branch: "main", State: StateUnprotected})
	in.Opts = Options{RequireChecks: []string{"x"}}
	if d := Decide(in); d.Allowed || d.Call != nil {
		t.Fatalf("GitLab cannot honour --require-check; it must refuse, not ignore it: %+v", d)
	}
}

func TestApply(t *testing.T) {
	c := Call{Argv: []string{"gh", "api", "--method", "PUT", "p"}, Body: []byte(`{}`)}
	f := newFake(map[string]reply{"PUT p": {Body: `{}`}})
	if err := Apply(f, c); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.calls[0].Stdin != `{}` {
		t.Errorf("body must go on stdin, got %q", f.calls[0].Stdin)
	}
	f = newFake(map[string]reply{"PUT p": {Status: 422, Msg: "Validation Failed"}})
	if err := Apply(f, c); err == nil || !strings.Contains(err.Error(), "Validation Failed") {
		t.Errorf("want the forge's message in the error, got %v", err)
	}
}

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		origin string
		spec   config.AuthoritySpec
		want   Target
		bad    bool
	}{
		{"git@github.com:acme/widgets.git", config.AuthoritySpec{}, Target{KindGitHub, "github.com", "acme/widgets"}, false},
		{"https://gitlab.com/acme/grp/widgets.git", config.AuthoritySpec{}, Target{KindGitLab, "gitlab.com", "acme/grp/widgets"}, false},
		{"https://git.corp.example/acme/widgets.git", config.AuthoritySpec{Forge: "gitlab"}, Target{KindGitLab, "git.corp.example", "acme/widgets"}, false},
		{"https://git.corp.example/acme/widgets.git", config.AuthoritySpec{}, Target{}, true}, // never guess
		{"", config.AuthoritySpec{}, Target{}, true},
	}
	for _, c := range cases {
		got, err := ResolveTarget(c.origin, c.spec)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want an error, got %+v", c.origin, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %+v, %v; want %+v", c.origin, got, err, c.want)
		}
	}
}

func TestBranches(t *testing.T) {
	if got, err := Branches(nil, ghTarget, config.AuthoritySpec{ProtectedBranches: []string{"main", " develop ", ""}}); err != nil || !reflect.DeepEqual(got, []string{"main", "develop"}) {
		t.Errorf("explicit list: %v, %v", got, err)
	}
	if _, err := Branches(nil, ghTarget, config.AuthoritySpec{ProtectedBranches: []string{"release/*"}}); err == nil {
		t.Error("a glob must be refused: a pattern leadkit cannot verify must not read as covered")
	}
	f := newFake(map[string]reply{ghRepoPath: {Body: `{"default_branch":"trunk"}`}})
	if got, err := Branches(f, ghTarget, config.AuthoritySpec{}); err != nil || !reflect.DeepEqual(got, []string{"trunk"}) {
		t.Errorf("default branch: %v, %v", got, err)
	}
	f = newFake(map[string]reply{ghRepoPath: {Status: 404, Msg: "Not Found"}})
	if _, err := Branches(f, ghTarget, config.AuthoritySpec{}); err == nil {
		t.Error("an unreadable repo must be an error, not a guessed 'main'")
	}
	f = newFake(map[string]reply{glProjPath: {Body: `{"default_branch":"develop"}`}})
	if got, err := Branches(f, glTarget, config.AuthoritySpec{}); err != nil || !reflect.DeepEqual(got, []string{"develop"}) {
		t.Errorf("gitlab default branch: %v, %v", got, err)
	}
}
