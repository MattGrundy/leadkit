package doctor

import (
	"strings"
	"testing"
)

func TestScopeOriginCheck(t *testing.T) {
	prof := cleanProfile(t)
	if c := scopeOriginCheck(prof, "/p/leadkit.yaml", fakeGit{ok: true, remote: "git@github.com:acme/w.git"}); !c.OK {
		t.Errorf("a readable origin is fine: %+v", c)
	}
	c := scopeOriginCheck(prof, "/p/leadkit.yaml", fakeGit{ok: true, remote: ""})
	if c.OK || c.Severity != SeverityWarn || !strings.Contains(c.Detail, "cannot confirm scope") || !strings.Contains(c.Detail, "git remote rename") {
		t.Errorf("a profiled repo with no readable origin must warn and say how to fix it: %+v", c)
	}
	// Not a finding about a repo no profile governs, or when there is no git to ask.
	if c := scopeOriginCheck(prof, "", fakeGit{ok: true, remote: ""}); !c.OK {
		t.Errorf("no profile: %+v", c)
	}
	if c := scopeOriginCheck(prof, "/p/leadkit.yaml", nil); !c.OK {
		t.Errorf("no git seam: %+v", c)
	}
}

// scopeOriginCheck on its own proves the check is right; this proves doctor RUNS it. Deleting the append in
// Run leaves TestScopeOriginCheck green and the report silent, which is the failure this guards.
func TestRun_IncludesTheScopeOriginCheck(t *testing.T) {
	find := func(git Git) *Check {
		report := Run(cleanProfile(t), "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", git, "", "", "", "", "", "", "", "")
		for i := range report.Checks {
			if report.Checks[i].Name == "scope-origin" {
				return &report.Checks[i]
			}
		}
		return nil
	}
	c := find(fakeGit{ok: true, remote: ""})
	if c == nil {
		t.Fatal("doctor.Run does not include scope-origin")
	}
	if c.OK || c.Severity != SeverityWarn || !strings.Contains(c.Detail, "git remote rename") {
		t.Errorf("a profiled repo with no readable origin must warn through Run, got %+v", *c)
	}
	if c := find(fakeGit{ok: true, remote: "git@github.com:acme/w.git"}); c == nil || !c.OK {
		t.Errorf("a readable origin must pass through Run, got %+v", c)
	}
}
