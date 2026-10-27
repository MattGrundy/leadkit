package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/forge"
)

// ghFake is a gh stand-in keyed by "METHOD path". gh's error shape: JSON on stdout,
// "gh: msg (HTTP n)" on stderr, non-nil exit error.
type ghFake struct {
	answers map[string][3]string // body, status, msg
	calls   int
}

func (f *ghFake) Run(argv []string, _ []byte) ([]byte, []byte, error) {
	f.calls++
	method := "GET"
	for i, a := range argv {
		if a == "--method" {
			method = argv[i+1]
		}
	}
	a, ok := f.answers[method+" "+argv[len(argv)-1]]
	if !ok {
		return nil, nil, fmt.Errorf("no answer for %s %s", method, argv[len(argv)-1])
	}
	if a[1] != "" {
		return []byte(fmt.Sprintf(`{"message":%q}`, a[2])), []byte(fmt.Sprintf("gh: %s (HTTP %s)", a[2], a[1])), errors.New("exit status 1")
	}
	return []byte(a[0]), nil, nil
}

func withForge(t *testing.T, r forge.Runner) {
	t.Helper()
	old := forgeRunner
	forgeRunner = r
	t.Cleanup(func() { forgeRunner = old })
}

const (
	dProt   = "GET repos/acme/widgets/branches/main/protection"
	dRules  = "GET repos/acme/widgets/rules/branches/main"
	dRepo   = "GET repos/acme/widgets"
	dStrict = `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":true},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`
)

func forgeCheck(t *testing.T, answers map[string][3]string, origin string) Check {
	t.Helper()
	answers[dRepo] = [3]string{`{"default_branch":"main"}`, "", ""}
	withForge(t, &ghFake{answers: answers})
	return forgeBranchProtectionCheck(&config.Profile{}, fakeGit{ok: true, remote: origin})
}

const ghOrigin = "git@github.com:acme/widgets.git"

func TestForgeBranchProtection_VerifiedIsTheOnlyOK(t *testing.T) {
	c := forgeCheck(t, map[string][3]string{dProt: {dStrict, "", ""}}, ghOrigin)
	if !c.OK || c.Severity != SeverityInfo || !strings.Contains(c.Detail, "VERIFIED") {
		t.Fatalf("want OK/info VERIFIED, got %+v", c)
	}
}

func TestForgeBranchProtection_NotProtectedWarnsPlainly(t *testing.T) {
	c := forgeCheck(t, map[string][3]string{
		dProt:  {"", "404", "Branch not protected"},
		dRules: {`[]`, "", ""},
	}, ghOrigin)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("want a warn, got %+v", c)
	}
	for _, want := range []string{"NOT PROTECTED", "client-side guards", "leadkit forge protect"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail should contain %q: %s", want, c.Detail)
		}
	}
}

func TestForgeBranchProtection_PartialIsNotVerified(t *testing.T) {
	weak := `{"required_pull_request_reviews":{},"enforce_admins":{"enabled":false},"allow_force_pushes":{"enabled":false},"allow_deletions":{"enabled":false}}`
	c := forgeCheck(t, map[string][3]string{dProt: {weak, "", ""}}, ghOrigin)
	if c.OK || !strings.Contains(c.Detail, "NOT PROTECTED") {
		t.Fatalf("a rule admins can bypass must warn as NOT PROTECTED: %+v", c)
	}
	if !strings.Contains(c.Detail, "admins can bypass") {
		t.Errorf("detail should say why: %s", c.Detail)
	}
}

// The forge plan cannot enforce it (this repo's own situation): say so, and do not send the
// reader to a command that cannot help.
func TestForgeBranchProtection_PlanCannotEnforce(t *testing.T) {
	c := forgeCheck(t, map[string][3]string{dProt: {"", "403", "Upgrade to GitHub Pro or make this repository public to enable this feature."}}, ghOrigin)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("want a warn, got %+v", c)
	}
	if !strings.Contains(c.Detail, "plan cannot enforce") || strings.Contains(c.Detail, "Enable forge protection: `leadkit forge protect`") {
		t.Errorf("detail = %s", c.Detail)
	}
}

// The core requirement: "cannot tell" must never read as "verified".
func TestForgeBranchProtection_CannotTellIsNeverVerified(t *testing.T) {
	cases := map[string]map[string][3]string{
		"unauthenticated": {dProt: {"", "401", "Bad credentials"}},
		"no access":       {dProt: {"", "404", "Not Found"}},
		"ruleset-governed": {
			dProt:  {"", "404", "Branch not protected"},
			dRules: {`[{"type":"pull_request"}]`, "", ""},
		},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			c := forgeCheck(t, answers, ghOrigin)
			if c.OK || c.Severity != SeverityWarn {
				t.Fatalf("want a warn, got %+v", c)
			}
			if !strings.Contains(c.Detail, "CANNOT VERIFY") || !strings.Contains(c.Detail, "unprotected") {
				t.Errorf("detail must say it could not verify and to treat as unprotected: %s", c.Detail)
			}
			if strings.Contains(c.Detail, "VERIFIED —") {
				t.Errorf("must not claim verified: %s", c.Detail)
			}
		})
	}
}

func TestForgeBranchProtection_UnreachableForgeCannotVerify(t *testing.T) {
	withForge(t, &ghFake{answers: map[string][3]string{}}) // every call errors with no HTTP status
	c := forgeBranchProtectionCheck(&config.Profile{}, fakeGit{ok: true, remote: ghOrigin})
	if c.OK || !strings.Contains(c.Detail, "CANNOT VERIFY") {
		t.Fatalf("want CANNOT VERIFY, got %+v", c)
	}
}

func TestForgeBranchProtection_UnknownForgeCannotVerify(t *testing.T) {
	f := &ghFake{answers: map[string][3]string{}}
	withForge(t, f)
	c := forgeBranchProtectionCheck(&config.Profile{}, fakeGit{ok: true, remote: "https://git.corp.example/acme/widgets.git"})
	if c.OK || !strings.Contains(c.Detail, "CANNOT VERIFY") {
		t.Fatalf("an undeterminable forge must not pass: %+v", c)
	}
	if f.calls != 0 {
		t.Errorf("no forge was determined, so nothing should have been called (%d calls)", f.calls)
	}
}

func TestForgeBranchProtection_UsesAuthorityProtectedBranches(t *testing.T) {
	answers := map[string][3]string{
		dProt: {dStrict, "", ""},
		"GET repos/acme/widgets/branches/develop/protection": {"", "404", "Branch not protected"},
		"GET repos/acme/widgets/rules/branches/develop":      {`[]`, "", ""},
	}
	withForge(t, &ghFake{answers: answers})
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"acme/widgets": {ProtectedBranches: []string{"main", "develop"}},
	}}
	c := forgeBranchProtectionCheck(prof, fakeGit{ok: true, remote: ghOrigin})
	if c.OK {
		t.Fatalf("one of two branches is unprotected; the check must warn: %+v", c)
	}
	if !strings.Contains(c.Detail, "main: VERIFIED") || !strings.Contains(c.Detail, "develop: NOT PROTECTED") {
		t.Errorf("each branch should be reported on its own: %s", c.Detail)
	}
}

func TestForgeBranchProtection_NothingToCheck(t *testing.T) {
	f := &ghFake{answers: map[string][3]string{}}
	withForge(t, f)
	for name, c := range map[string]Check{
		"no git seam": forgeBranchProtectionCheck(&config.Profile{}, nil),
		"no origin":   forgeBranchProtectionCheck(&config.Profile{}, fakeGit{ok: true}),
	} {
		if !c.OK || c.Severity != SeverityInfo || !strings.Contains(c.Detail, "not checked") {
			t.Errorf("%s: want OK/info that says it did not check, got %+v", name, c)
		}
	}
	if f.calls != 0 {
		t.Errorf("no forge calls expected, got %d", f.calls)
	}
}
