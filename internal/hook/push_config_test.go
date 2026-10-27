package hook

import (
	"strings"
	"testing"
)

// The configuration that decides what a push publishes: which remote a bare push goes to (git's
// order, not just branch.<b>.remote), a remote that mirrors, and values the hook cannot read.

// onBranch makes the hook see the current branch as feat, so branch.<b>.* keys are consulted.
func onBranch(t *testing.T) {
	t.Helper()
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat", true
		}
		return "", false
	})
}

func denyUndetermined(t *testing.T, cmd string) {
	t.Helper()
	cfg, _ := refPushEnv(t, shaGated) // a gated session HEAD: judging by HEAD would let it by
	res := PushGate(bashInput(cmd), cfg)
	assertDeny(t, res, cmd)
	if !strings.Contains(res.Stdout, "could not determine") {
		t.Errorf("%s: want the commits-not-determined refusal, got:\n%s", cmd, res.Stdout)
	}
}

func TestPushGate_AMirrorRemoteWidensEveryPushToIt(t *testing.T) {
	onBranch(t)
	stubGitConfig(t, map[string][]string{"remote.origin.mirror": {"true"}, "remote.fork.mirror": {"yes"}})
	for _, cmd := range []string{
		"git push", "git push origin", "git push fork",
		"git push origin feat/gated", // a mirror remote ignores the named ref
		"git push fork feat/gated",
	} {
		denyUndetermined(t, cmd)
	}
	// CONTROLS: mirror=false, and a remote that is not the mirror.
	stubGitConfig(t, map[string][]string{"remote.origin.mirror": {"false"}, "remote.fork.mirror": {"true"}})
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push"), cfg), "origin does not mirror")
	assertAllow(t, PushGate(bashInput("git push origin feat/gated"), cfg), "origin does not mirror, named ref")
	denyUndetermined(t, "git push fork")
}

func TestPushGate_MirrorOnTheCommandLine(t *testing.T) {
	onBranch(t)
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{
		"git -c remote.origin.mirror=true push origin",
		"git -c remote.origin.mirror=true push",
		"git -c remote.origin.mirror=true push origin feat/gated",
		"git --config-env=remote.origin.mirror=M push origin",
		"git -c remote.origin.mirror=$M push origin",
		"git -c remote.$R.mirror=true push origin", // a remote name the hook cannot read
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.mirror GIT_CONFIG_VALUE_0=true git push origin",
	} {
		denyUndetermined(t, cmd)
	}
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git -c remote.origin.mirror=false push origin feat/gated"), cfg), "mirror=false on the command line")
}

// Git's bare-push remote is branch.<b>.pushRemote, else remote.pushDefault, else branch.<b>.remote,
// else origin. The refspec that widens is the one on THAT remote.
func TestPushGate_TheRemoteABarePushGoesToIsTheOneGitUses(t *testing.T) {
	onBranch(t)
	widenFork := "remote.fork.push"
	for name, values := range map[string]map[string][]string{
		"remote.pushDefault":              {"remote.pushDefault": {"fork"}, widenFork: {"refs/heads/*:refs/heads/*"}},
		"branch.feat.pushRemote":          {"branch.feat.pushRemote": {"fork"}, widenFork: {"refs/heads/*:refs/heads/*"}},
		"branch.feat.remote":              {"branch.feat.remote": {"fork"}, widenFork: {"refs/heads/*:refs/heads/*"}},
		"pushRemote beats remote":         {"branch.feat.pushRemote": {"fork"}, "branch.feat.remote": {"other"}, widenFork: {"x:y"}},
		"pushRemote beats pushDefault":    {"branch.feat.pushRemote": {"fork"}, "remote.pushDefault": {"other"}, widenFork: {"x:y"}},
		"pushDefault beats branch.remote": {"remote.pushDefault": {"fork"}, "branch.feat.remote": {"other"}, widenFork: {"x:y"}},
	} {
		t.Run(name, func(t *testing.T) {
			stubGitConfig(t, values)
			denyUndetermined(t, "git push")
		})
	}
	// CONTROLS: the widening refspec is on a remote the push does NOT go to.
	for name, values := range map[string]map[string][]string{
		"refspec on origin, push goes to fork": {"remote.pushDefault": {"fork"}, "remote.origin.push": {"x:y"}},
		"refspec on fork, pushRemote is other": {"branch.feat.pushRemote": {"other"}, "remote.pushDefault": {"fork"}, widenFork: {"x:y"}},
		"refspec on fork, branch.remote other": {"branch.feat.remote": {"other"}, widenFork: {"x:y"}},
	} {
		t.Run("control "+name, func(t *testing.T) {
			stubGitConfig(t, values)
			cfg, _ := refPushEnv(t, shaGated)
			assertAllow(t, PushGate(bashInput("git push"), cfg), name)
		})
	}
}

func TestPushGate_TheSelectorsOnTheCommandLineCountToo(t *testing.T) {
	onBranch(t)
	stubGitConfig(t, map[string][]string{"remote.fork.push": {"x:y"}})
	for _, cmd := range []string{
		"git -c remote.pushDefault=fork push",
		"git -c branch.feat.pushRemote=fork push",
		"git -c branch.feat.remote=fork push",
	} {
		denyUndetermined(t, cmd)
	}
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git -c remote.pushDefault=other push"), cfg), "the selector points away from the widened remote")
}

// A value the hook cannot read (built by an expansion) is the widening value, as --config-env is.
func TestPushGate_AComputedConfigValueIsTheWideningValue(t *testing.T) {
	onBranch(t)
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{
		"git -c push.default=$M push",
		`git -c "push.default=$M" push`,
		"git -c push.default=$(echo matching) push",
		"git -c push.default=`echo matching` push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.default GIT_CONFIG_VALUE_0=$M git push",
		"git -c remote.origin.push=$X push",
	} {
		denyUndetermined(t, cmd)
	}
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git -c push.default=simple push"), cfg), "a readable, narrow value")
}

func TestPushGuard_MirrorAndSelectorsAreFailClosed(t *testing.T) {
	onBranch(t)
	wl := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	stubGitConfig(t, map[string][]string{"remote.origin.mirror": {"true"}})
	assertDeny(t, PushGuard(pushInput("git push origin feat/ok"), wl), "a mirror remote publishes every ref, so the whitelisted name is not the whole push")
	stubGitConfig(t, map[string][]string{"remote.pushDefault": {"fork"}, "remote.fork.push": {"x:y"}})
	assertDeny(t, PushGuard(pushInput("git push"), wl), "a bare push goes to the pushDefault remote, whose refspec widens it")
	stubGitConfig(t, map[string][]string{})
	assertAllow(t, PushGuard(pushInput("git push origin feat/ok"), wl), "control")
}
