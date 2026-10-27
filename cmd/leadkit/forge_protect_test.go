package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/forge"
)

// forgeFake is a stateful gh stand-in: GET answers come from a map that a successful
// write can change, so a test can model "the write took" and "the forge ignored it".
type forgeFake struct {
	answers map[string]fakeAnswer
	calls   [][]string
	stdins  []string
	onWrite func(f *forgeFake)
}

type fakeAnswer struct {
	body   string
	status int
	msg    string
}

func (f *forgeFake) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, argv)
	f.stdins = append(f.stdins, string(stdin))
	method := "GET"
	for i, a := range argv {
		if a == "--method" {
			method = argv[i+1]
		}
	}
	key := method + " " + argv[len(argv)-1]
	if method != "GET" && f.onWrite != nil {
		defer f.onWrite(f)
	}
	a, ok := f.answers[key]
	if !ok {
		return nil, nil, fmt.Errorf("no answer for %s", key)
	}
	if a.status != 0 {
		return []byte(fmt.Sprintf(`{"message":%q}`, a.msg)), []byte(fmt.Sprintf("gh: %s (HTTP %d)", a.msg, a.status)), errors.New("exit status 1")
	}
	return []byte(a.body), nil, nil
}

func (f *forgeFake) writes() int {
	n := 0
	for _, c := range f.calls {
		for i, a := range c {
			if a == "--method" && c[i+1] != "GET" {
				n++
			}
		}
	}
	return n
}

const (
	pProt  = "GET repos/acme/widgets/branches/main/protection"
	pRules = "GET repos/acme/widgets/rules/branches/main"
	pRepo  = "GET repos/acme/widgets"
	pPut   = "PUT repos/acme/widgets/branches/main/protection"
	strict = `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`
)

func unprotectedAnswers() map[string]fakeAnswer {
	return map[string]fakeAnswer{
		pRepo:  {body: `{"default_branch":"main"}`},
		pProt:  {status: 404, msg: "Branch not protected"},
		pRules: {body: `[]`},
		pPut:   {body: `{}`},
	}
}

func runForgeProtect(t *testing.T, f *forgeFake, authority bool, args ...string) (code int, out, errOut string) {
	t.Helper()
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{}}
	if authority {
		prof.Authority["acme/widgets"] = config.AuthoritySpec{}
	}
	var so, se bytes.Buffer
	code = forgeProtect(args, forgeDeps{
		prof:   func() *config.Profile { return prof },
		origin: func(string) string { return "git@github.com:acme/widgets.git" },
		runner: f,
	}, &so, &se)
	return code, so.String(), se.String()
}

func TestForgeProtect_RefusesWithoutAuthorityAndTouchesNothing(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	code, _, errOut := runForgeProtect(t, f, false, "--apply", "--confirm")
	if code != 1 || !strings.Contains(errOut, "REFUSED") {
		t.Fatalf("want exit 1 + REFUSED, got %d %q", code, errOut)
	}
	if len(f.calls) != 0 {
		t.Errorf("an ungoverned repo must not even be read; calls: %v", f.calls)
	}
}

func TestForgeProtect_DryRunIsTheDefaultAndWritesNothing(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	code, out, _ := runForgeProtect(t, f, true)
	if code != 0 {
		t.Fatalf("dry-run exit = %d\n%s", code, out)
	}
	if f.writes() != 0 {
		t.Errorf("dry-run wrote: %v", f.calls)
	}
	for _, want := range []string{"dry-run", "[unprotected]", "would run: gh api", "PUT"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan should contain %q:\n%s", want, out)
		}
	}
}

func TestForgeProtect_BareApplyRefusesAndWritesNothing(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	code, out, _ := runForgeProtect(t, f, true, "--apply")
	if code != 1 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("want exit 1 + REFUSED, got %d\n%s", code, out)
	}
	if f.writes() != 0 {
		t.Errorf("--apply without --confirm wrote: %v", f.calls)
	}
}

func TestForgeProtect_ConfirmAloneDoesNotWrite(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	if code, _, _ := runForgeProtect(t, f, true, "--confirm"); code != 0 || f.writes() != 0 {
		t.Fatalf("--confirm alone must be a dry-run: code=%d writes=%d", code, f.writes())
	}
}

func TestForgeProtect_ApplyConfirmWritesThenReadsBack(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	f.onWrite = func(f *forgeFake) { f.answers[pProt] = fakeAnswer{body: strict} } // the write took
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if f.writes() != 1 {
		t.Fatalf("want exactly one write, calls: %v", f.calls)
	}
	var body string
	for i, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "--method PUT") {
			body = f.stdins[i]
		}
	}
	if !strings.Contains(body, `"enforce_admins":true`) {
		t.Errorf("PUT body missing from stdin: %q", body)
	}
	if !strings.Contains(out, "applied and verified server-side") {
		t.Errorf("success must be reported only after the read-back:\n%s", out)
	}
}

// A 2xx from the write is not proof. If the forge now reports the branch still unprotected,
// the command must fail, not claim success.
func TestForgeProtect_FailsWhenReadBackDisagrees(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()} // PUT "succeeds"; GET still says unprotected
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if strings.Contains(out, "applied and verified") {
		t.Errorf("must not claim success:\n%s", out)
	}
	if !strings.Contains(out, "does not report main verified") {
		t.Errorf("should say the read-back disagreed:\n%s", out)
	}
}

func TestForgeProtect_WriteFailure(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	f.answers[pPut] = fakeAnswer{status: 422, msg: "Validation Failed"}
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 1 || !strings.Contains(out, "Validation Failed") {
		t.Fatalf("want exit 1 carrying the forge's message, got %d\n%s", code, out)
	}
}

func TestForgeProtect_NeverWritesAgainstUnreadableState(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	f.answers[pProt] = fakeAnswer{status: 401, msg: "Bad credentials"}
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 1 || f.writes() != 0 {
		t.Fatalf("code=%d writes=%d\n%s", code, f.writes(), out)
	}
}

func TestForgeProtect_AlreadyProtectedIsANoopSuccess(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	f.answers[pProt] = fakeAnswer{body: strict}
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 0 || f.writes() != 0 || !strings.Contains(out, "already protected") {
		t.Fatalf("code=%d writes=%d\n%s", code, f.writes(), out)
	}
}

func TestForgeProtect_BranchFlagPatternRefused(t *testing.T) {
	f := &forgeFake{answers: unprotectedAnswers()}
	code, _, errOut := runForgeProtect(t, f, true, "--branch", "release/*")
	if code != 1 || !strings.Contains(errOut, "pattern") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

// richProt is a branch protected well beyond the standard, short of it only in that
// admins are exempt: a write must not downgrade it.
const richProt = `{"required_status_checks":{"strict":true,"contexts":["ci/build","ci/lint","ci/test"],"checks":[{"context":"ci/build","app_id":15368},{"context":"ci/lint","app_id":null},{"context":"ci/test","app_id":null}]},
"enforce_admins":{"enabled":false},
"required_pull_request_reviews":{"dismiss_stale_reviews":true,"require_code_owner_reviews":true,"required_approving_review_count":2},
"restrictions":{"users":[],"teams":[{"slug":"core"},{"slug":"release"}],"apps":[]},
"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false},"required_signatures":{"enabled":false}}`

// putToGet turns a PUT /protection body into the GET document GitHub would serve after it:
// the endpoint REPLACES the rule, so the result is exactly what was sent and nothing else.
func putToGet(t *testing.T, body string) string {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("write body is not JSON: %v", err)
	}
	obj := func(list any, key string) any {
		var out []map[string]string
		for _, x := range list.([]any) {
			out = append(out, map[string]string{key: x.(string)})
		}
		return out
	}
	actors := func(v any) any {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		return map[string]any{"users": obj(m["users"], "login"), "teams": obj(m["teams"], "slug"), "apps": obj(m["apps"], "slug")}
	}
	out := map[string]any{"required_status_checks": in["required_status_checks"], "restrictions": actors(in["restrictions"])}
	for _, k := range []string{"enforce_admins", "allow_force_pushes", "allow_deletions", "required_linear_history",
		"block_creations", "required_conversation_resolution", "lock_branch", "allow_fork_syncing"} {
		out[k] = map[string]any{"enabled": in[k]}
	}
	if rv, ok := in["required_pull_request_reviews"].(map[string]any); ok {
		if d, has := rv["dismissal_restrictions"]; has {
			rv["dismissal_restrictions"] = actors(d)
		}
		out["required_pull_request_reviews"] = rv
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func richAnswers() map[string]fakeAnswer {
	a := unprotectedAnswers()
	a[pProt] = fakeAnswer{body: richProt}
	return a
}

// The plan on the dry-run must tell the operator what they have and what would change,
// in words, before anyone is asked to --confirm anything.
func TestForgeProtect_DryRunNamesCurrentProtectionAndTheChange(t *testing.T) {
	f := &forgeFake{answers: richAnswers()}
	code, out, _ := runForgeProtect(t, f, true)
	if code != 0 || f.writes() != 0 {
		t.Fatalf("code=%d writes=%d\n%s", code, f.writes(), out)
	}
	for _, want := range []string{
		"protected by now: ", "2 approvals required", "code-owner review required", "3 status checks required",
		"pushes restricted to 2 teams", "admins exempt",
		"~ CHANGES: admins exempt -> admins bound",
		"nothing the branch is protected by today is removed",
		"why: the forge's PUT replaces the whole protection rule",
		"audit trail, would run: gh api",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run should contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "REMOVES") {
		t.Errorf("a correct merge removes nothing:\n%s", out)
	}
}

// End to end against a forge that really replaces the rule on PUT: applying to a
// well-protected branch keeps every protection it had.
func TestForgeProtect_ApplyToARichBranchKeepsEverythingItHad(t *testing.T) {
	f := &forgeFake{answers: richAnswers()}
	f.onWrite = func(f *forgeFake) { f.answers[pProt] = fakeAnswer{body: putToGet(t, f.stdins[len(f.stdins)-1])} }
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 0 || f.writes() != 1 || !strings.Contains(out, "applied and verified server-side") {
		t.Fatalf("code=%d writes=%d\n%s", code, f.writes(), out)
	}
	after := f.answers[pProt].body
	for _, want := range []string{`"required_approving_review_count":2`, `"require_code_owner_reviews":true`, `"ci/lint"`, `"slug":"release"`, `"app_id":15368`} {
		if !strings.Contains(after, want) {
			t.Errorf("after the write the branch lost %s:\n%s", want, after)
		}
	}
}

// Anything leadkit cannot carry through the replace must refuse, even under --confirm,
// and the plan must say what would have been lost.
func TestForgeProtect_RefusesAWriteThatWouldRemoveAProtection(t *testing.T) {
	f := &forgeFake{answers: richAnswers()}
	f.answers[pProt] = fakeAnswer{body: strings.Replace(richProt, `"required_signatures":{"enabled":false}`, `"required_signatures":{"enabled":true}`, 1)}
	for _, args := range [][]string{nil, {"--apply", "--confirm"}} {
		code, out, _ := runForgeProtect(t, f, true, args...)
		if code != 1 || f.writes() != 0 {
			t.Fatalf("%v: code=%d writes=%d\n%s", args, code, f.writes(), out)
		}
		for _, want := range []string{"- REMOVES: required_signatures", "WARNING: this would REMOVE 1 existing protection", "leadkit never weakens"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: output should contain %q:\n%s", args, want, out)
			}
		}
	}
}

// A forge that "takes" the write but loses the rest must fail loudly.
func TestForgeProtect_ReadBackFailsWhenTheWriteClobberedTheRest(t *testing.T) {
	f := &forgeFake{answers: richAnswers()}
	f.onWrite = func(f *forgeFake) { f.answers[pProt] = fakeAnswer{body: strict} } // verified, but 0 approvals, no checks
	code, out, _ := runForgeProtect(t, f, true, "--apply", "--confirm")
	if code != 1 || strings.Contains(out, "applied and verified") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	for _, want := range []string{"read-back disagrees", "required approvals 2 -> 0", "3 required status checks", "push restrictions"} {
		if !strings.Contains(out, want) {
			t.Errorf("should contain %q:\n%s", want, out)
		}
	}
}

var _ forge.Runner = (*forgeFake)(nil)
