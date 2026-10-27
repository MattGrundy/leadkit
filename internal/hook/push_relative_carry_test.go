package hook

import (
	"strings"
	"testing"
)

// Ticket 215. A directory change in force makes a LATER relative operand (a second `cd`, a `pushd`, or
// `git -C`) move from the carried directory, not from the session's. Resolving it against the session
// judged the command against the wrong repository: `cd V && cd . && P` read `.` as the session's own
// directory, so a session whose repo the gate does not govern pushed into a governed one.

func firstRepoPath(t *testing.T, cmd string) string {
	t.Helper()
	p := parsePushCommands(cmd)
	if p.State != pushFound || len(p.Invocations) == 0 {
		t.Fatalf("%q: want pushFound with an invocation, got %+v", cmd, p)
	}
	return p.Invocations[0].repoPath
}

func TestParsePush_ARelativeOperandMovesFromTheDirectoryInForce(t *testing.T) {
	for cmd, want := range map[string]string{
		// cd after cd
		"cd /t && cd . && git push origin feat/x":              "/t",
		"cd /t/sub && cd .. && git push origin feat/x":         "/t",
		"cd /t && cd sub && cd .. && git push origin feat/x":   "/t",
		"cd /t && cd sub && git push origin feat/x":            "/t/sub",
		"cd -L /t && cd -P -- sub && git push origin feat/x":   "/t/sub",
		"cd /t/sub && pushd .. && git push origin feat/x":      "/t",
		"cd /t/sub && builtin cd .. && git push origin feat/x": "/t",
		"cd /t && cd '.' && git push origin feat/x":            "/t",
		"cd /t && cd '' && git push origin feat/x":             "/t", // an empty operand changes nothing
		"cd /t && ( cd sub && git push origin feat/x )":        "/t/sub",
		// -C after -C
		"git -C /t -C . push origin feat/x":      "/t",
		"git -C /t/sub -C .. push origin feat/x": "/t",
		"git -C /t -C '' push origin feat/x":     "/t", // -C '' leaves the directory as it was
		"git -C /t -C sub push origin feat/x":    "/t/sub",
		// -C after cd
		"cd /t && git -C . push origin feat/x":   "/t",
		"cd /t && git -C sub push origin feat/x": "/t/sub",
		// an absolute operand replaces the carried one, as before
		"cd /t && cd /u && git push origin feat/x":           "/u",
		"cd $(mktemp -d) && cd /u && git push origin feat/x": "/u", // an absolute operand needs no carry
		"git -C /t -C /u push origin feat/x":                 "/u",
		"cd /t && cd ~/w && git push origin feat/x":          "~/w", // ~ is not relative to the carry
		"cd /t && git -C /u push origin feat/x":              "/u",
		// CONTROLS: with nothing in force a relative operand is the session's, as before
		"cd sub && git push origin feat/x":           "sub",
		"git -C ../x push origin feat/x":             "../x",
		"git -C . push origin feat/x":                ".",
		"cd /t && git push origin feat/x":            "/t",
		"git -C /t push origin feat/x":               "/t",
		"cd ~/w && cd sub && git push origin feat/x": "~/w/sub",
	} {
		if got := firstRepoPath(t, cmd); got != want {
			t.Errorf("%q: repo path = %q, want %q", cmd, got, want)
		}
	}
}

// A relative operand against a directory that is not named (or only a candidate) is an unknown target.
func TestParsePush_ARelativeOperandAgainstAnUnnamedDirectoryIsUnknown(t *testing.T) {
	for _, cmd := range []string{
		"cd $(mktemp -d) && cd . && git push origin feat/x",
		"cd $(mktemp -d) && git -C . push origin feat/x",
		"cd /t ; cd . && git push origin feat/x",            // the first cd may have failed
		"cd /t ; git -C . push origin feat/x",               // so may this one's
		"cd - && cd . && git push origin feat/x",            // $OLDPWD
		"R=/t; cd $R && cd sub && git push origin feat/x",   // a variable the line assigns
		"cd /t && cd $(echo sub) && git push origin feat/x", // a computed operand (already unknown)
		"cd /t || cd . && git push origin feat/x",           // joined by ||
	} {
		p := parsePushCommands(cmd)
		if p.State != pushUnsure || p.Unknown == "" {
			t.Errorf("%q: want pushUnsure with an Unknown reason, got %+v", cmd, p)
		}
	}
}

// THE defect, through the push gate, from a session whose repo the gate does not govern and from one
// it does, in both modes. The target is the repo the shell is really in, so the recorded branch passes
// and the unrecorded one is refused; before, the relative operand resolved against the session.
func TestPushGate_ARelativeOperandIsJudgedByTheDirectoryItMovesFrom(t *testing.T) {
	for _, sessionGated := range []bool{true, false} {
		for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
			cfg, _, target := cdTargetEnv(t, sessionGated)
			cfg.Mode = func(string) GuardMode { return mode }
			for _, pat := range []string{
				"cd " + target + " && cd . && git push origin %s",
				"cd " + target + "/sub && cd .. && git push origin %s",
				"cd " + target + " && cd sub && cd .. && git push origin %s",
				"cd " + target + "/sub && pushd .. && git push origin %s",
				"cd " + target + "/sub && builtin cd .. && git push origin %s",
				"git -C " + target + " -C . push origin %s",
				"git -C " + target + "/sub -C .. push origin %s",
				"git -C " + target + " -C '' push origin %s",
				"cd " + target + " && git -C . push origin %s",
			} {
				bad := strings.ReplaceAll(pat, "%s", "feat/ungated")
				assertDeny(t, PushGate(bashInput(bad), cfg), bad)
				// Firing control: the same shape publishing a recorded commit passes, so the refusal
				// above is the target's and not a refusal of every relative operand.
				ok := strings.ReplaceAll(pat, "%s", "feat/gated")
				assertAllow(t, PushGate(bashInput(ok), cfg), ok)
			}
			// An unnamed carry is refused whatever the session's scope, and the bypass lifts it.
			unk := "cd $(mktemp -d) && cd . && git push origin feat/gated"
			assertDeny(t, PushGate(bashInput(unk), cfg), unk)
			t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
			assertAllow(t, PushGate(bashInput(unk), cfg), "bypassed: "+unk)
			t.Setenv("REVIEW_GATE_BYPASS", "")
		}
	}
}
