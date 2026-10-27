package hook

import (
	"strings"
	"testing"
)

// An environment variable can point git at a repository the command does not name, exactly as
// --git-dir can. The push is then published from a repo other than the one the working directory
// and -C say, so judging it by them judges the wrong repo: a gated session's record for its own
// branch would pass a push to anywhere. The variable makes the target unknown.

var gitTargetEnvLines = func(target string) []string {
	return []string{
		"GIT_DIR=" + target + "/.git git push origin feat/gated",
		"GIT_WORK_TREE=" + target + " git push origin feat/gated",
		"GIT_COMMON_DIR=" + target + "/.git git push origin feat/gated",
		"GIT_NAMESPACE=x git push origin feat/gated",
		"GIT_DIR= git push origin feat/gated", // an empty value is still the variable
		"FOO=1 GIT_DIR=" + target + "/.git git push origin feat/gated",
		"export GIT_DIR=" + target + "/.git; git push origin feat/gated",
		"export GIT_DIR=" + target + "/.git && git push origin feat/gated",
		"GIT_DIR=" + target + "/.git; git push origin feat/gated",
		"declare -x GIT_WORK_TREE=" + target + "; git push origin feat/gated",
		"env GIT_DIR=" + target + "/.git git push origin feat/gated",
		"git -C " + target + " push origin feat/gated; GIT_DIR=" + target + " true",
		"read GIT_DIR <<< " + target + "; git push origin feat/gated",
		"(export GIT_DIR=" + target + "/.git; git push origin feat/gated)",
	}
}

func TestParsePush_AnEnvVariableThatRedirectsGitMakesTheTargetUnknown(t *testing.T) {
	for _, cmd := range gitTargetEnvLines("/t") {
		p := parsePushCommands(cmd)
		if p.State != pushUnsure || p.Unknown == "" {
			t.Errorf("%q: want pushUnsure with an Unknown reason, got %+v", cmd, p)
			continue
		}
		if !strings.Contains(p.Unknown, "GIT_") || !strings.Contains(p.Why, "GIT_") {
			t.Errorf("%q: both reasons must name the variable, got Unknown=%q Why=%q", cmd, p.Unknown, p.Why)
		}
	}
	// CONTROLS: a variable that is not one of them, a name that only starts like one, and an
	// ordinary push are all found and judged by what they name.
	for _, cmd := range []string{
		"git push origin feat/gated",
		"FOO=1 git push origin feat/gated",
		"REVIEW_GATE_BYPASS=why git push origin feat/gated",
		"GIT_DIRECTORY=/t git push origin feat/gated",
		"MY_GIT_DIR=/t git push origin feat/gated",
		"GIT_SSH_COMMAND=ssh git push origin feat/gated",
		"git -C /t push origin feat/gated",
	} {
		if p := parsePushCommands(cmd); p.State != pushFound {
			t.Errorf("%q: want pushFound, got %+v", cmd, p)
		}
	}
	// The variable only matters when there is a push for it to redirect.
	if p := parsePushCommands("GIT_DIR=/t git status"); p.State != pushNone {
		t.Errorf("a variable on a command that is not a push: want pushNone, got %+v", p)
	}
}

// THE defect, through the push gate: the session is judged by its own record, so a branch the
// session has gated passed no matter which repository the variable pointed git at.
func TestPushGate_AnEnvVariableThatRedirectsGit_IsRefusedFromAnySession(t *testing.T) {
	for _, sessionGated := range []bool{true, false} {
		for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
			cfg, _, target := cdTargetEnv(t, sessionGated)
			cfg.Mode = func(string) GuardMode { return mode }
			// Firing control: the recorded branch is allowed when nothing redirects git.
			assertAllow(t, PushGate(bashInput("git push origin feat/gated"), cfg), "control: a recorded branch")
			for _, cmd := range gitTargetEnvLines(target) {
				assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
			}
			t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
			for _, cmd := range gitTargetEnvLines(target) {
				assertAllow(t, PushGate(bashInput(cmd), cfg), "bypassed: "+cmd)
			}
			t.Setenv("REVIEW_GATE_BYPASS", "")
		}
	}
}

func TestPushGuard_AnEnvVariableThatRedirectsGit_IsRefused(t *testing.T) {
	cfg := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	noRemoteStub(t)
	if res := PushGuard(pushInput("git push origin feat/x"), cfg); res.Stdout != "" {
		t.Fatalf("control: a whitelisted branch must be allowed, got %+v", res)
	}
	for _, cmd := range gitTargetEnvLines("/t") {
		cmd = strings.ReplaceAll(cmd, "feat/gated", "feat/x")
		if res := PushGuard(pushInput(cmd), cfg); res.Stdout == "" {
			t.Errorf("%q: want a deny, got %+v", cmd, res)
		}
	}
}
