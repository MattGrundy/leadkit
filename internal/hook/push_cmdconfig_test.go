package hook

import (
	"reflect"
	"strings"
	"testing"
)

// Ticket 163, review round 1. The widening config can ride on the push's own command line, where
// the hook can see it: `-c`, `--config-env`, and GIT_CONFIG_* in front of the command. And `:` is
// git's documented spelling of a matching push.

func TestGitPrefixConfig(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"git push origin", nil},
		{"git -c push.default=matching push origin", []string{"push.default=matching"}},
		{"git -c user.name=x -c push.default=simple -c push.default=matching push", []string{"user.name=x", "push.default=simple", "push.default=matching"}},
		{"git --config-env=push.default=PD push", []string{"push.default=?"}},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.default GIT_CONFIG_VALUE_0=matching git push", []string{"push.default=matching"}},
		{"GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=a.b GIT_CONFIG_VALUE_0=1 GIT_CONFIG_KEY_1=remote.o.push GIT_CONFIG_VALUE_1=x:y git push", []string{"a.b=1", "remote.o.push=x:y"}},
		{"GIT_CONFIG_PARAMETERS=x git push", []string{"GIT_CONFIG_PARAMETERS=?"}},
		{"GIT_CONFIG_COUNT=abc git push", []string{"GIT_CONFIG_COUNT=?"}},
		{"git -C /r -c push.default=matching push", []string{"push.default=matching"}},
		{"git push -c push.default=matching", nil}, // after `push` it is a push option, not git config
	}
	for _, c := range cases {
		p := parsePushCommands(c.cmd)
		if p.State != pushFound || len(p.Invocations) != 1 {
			t.Errorf("%q: want one push, got %+v", c.cmd, p)
			continue
		}
		if got := p.Invocations[0].gitConfig; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: gitConfig = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// A gated session HEAD and an unrecorded second branch: judging by HEAD would let each of these by.
func TestPushGate_CommandLineConfigThatWidensABarePush_IsRefused(t *testing.T) {
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{
		"git -c push.default=matching push",
		"git -c push.default=matching push origin",
		"git -c push.default=MATCHING push",
		"git -c remote.origin.push=refs/heads/*:refs/heads/* push origin",
		"git -c remote.origin.push=refs/heads/*:refs/heads/* push",
		"git --config-env=push.default=PD push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.default GIT_CONFIG_VALUE_0=matching git push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=remote.origin.push GIT_CONFIG_VALUE_0=a:b git push origin",
		"GIT_CONFIG_PARAMETERS='push.default=matching' git push",
		"git push origin :",
		"git push origin +:",
		"git push --force origin :",
	} {
		cfg, _ := refPushEnv(t, shaGated)
		res := PushGate(bashInput(cmd), cfg)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "could not determine") {
			t.Errorf("%s: want the commits-not-determined refusal, got:\n%s", cmd, res.Stdout)
		}
	}
}

// CONTROLS: the command line can also say nothing about widening, or narrow it, or name the ref.
func TestPushGate_CommandLineConfigThatDoesNotWiden_IsStillJudgedByHead(t *testing.T) {
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{
		"git -c user.name=x push",
		"git -c push.default=simple push",
		"git -c push.default=current push origin",
		"git -c remote.backup.push=a:b push origin", // another remote's refspec
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.editor GIT_CONFIG_VALUE_0=vi git push",
		"git --config-env=http.extraheader=H push",
		"git -c push.default=matching push origin feat/gated", // a named ref is not widened
	} {
		cfg, _ := refPushEnv(t, shaGated)
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd+" (gated HEAD)")
		cfg, _ = refPushEnv(t, shaUngated)
		if strings.HasSuffix(cmd, "feat/gated") {
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd+" (named gated ref, ungated HEAD)")
			continue
		}
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd+" (ungated HEAD)")
	}
	// The last push.default wins, as in git, and the command line overrides the files.
	stubGitConfig(t, map[string][]string{"push.default": {"matching"}})
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git -c push.default=simple push"), cfg), "the command line narrows a matching file config")
	assertDeny(t, PushGate(bashInput("git push"), cfg), "the file config alone still widens")
}

func TestPushGuard_CommandLineConfigThatWidensABarePush_IsFailClosed(t *testing.T) {
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/ok", true
		}
		return "", false
	})
	stubGitConfig(t, map[string][]string{})
	wl := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	for _, cmd := range []string{
		"git -c push.default=matching push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.default GIT_CONFIG_VALUE_0=matching git push",
		"git push origin :",
		"git push origin +:",
	} {
		assertDeny(t, PushGuard(pushInput(cmd), wl), cmd)
	}
	assertAllow(t, PushGuard(pushInput("git -c user.name=x push"), wl), "unrelated -c on a whitelisted branch")
}

// Ticket 163, round 3. A config KEY built by an expansion can be any setting, including one that
// widens a bare push; and git folds case only in the section and variable name, never in the
// subsection, so an override of a differently-cased remote does not narrow anything.
func TestPushGate_AComputedConfigKeyIsRefusedAsPossiblyWidening(t *testing.T) {
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{
		"git -c $K=matching push",
		"git -c push.$D=matching push",
		"git -c \"$K=matching\" push origin",
		"git -c remote.$R.mirror=true push",
		"git -c `echo push.default`=matching push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=$K GIT_CONFIG_VALUE_0=matching git push",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=push.$D GIT_CONFIG_VALUE_0=matching git push",
	} {
		cfg, _ := refPushEnv(t, shaGated)
		res := PushGate(bashInput(cmd), cfg)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "key the hook cannot read") {
			t.Errorf("%s: the refusal must say the key is unreadable, got:\n%s", cmd, res.Stdout)
		}
	}
	// Controls: a plain unrelated key, and a computed VALUE of an unrelated key, still pass a gated HEAD.
	for _, cmd := range []string{"git -c user.name=x push", "git -c user.name=$NAME push", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.editor GIT_CONFIG_VALUE_0=$E git push"} {
		cfg, _ := refPushEnv(t, shaGated)
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
	}
}

func TestPushGuard_AComputedConfigKeyIsFailClosed(t *testing.T) {
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/ok", true
		}
		return "", false
	})
	stubGitConfig(t, map[string][]string{})
	wl := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	for _, cmd := range []string{"git -c $K=matching push", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=$K GIT_CONFIG_VALUE_0=matching git push"} {
		assertDeny(t, PushGuard(pushInput(cmd), wl), cmd)
	}
	assertAllow(t, PushGuard(pushInput("git -c user.name=x push"), wl), "a plain key")
}

func TestCanonKey(t *testing.T) {
	for k, want := range map[string]string{
		"push.default":             "push.default",
		"Push.Default":             "push.default",
		"remote.origin.mirror":     "remote.origin.mirror",
		"REMOTE.origin.MIRROR":     "remote.origin.mirror",
		"remote.Origin.mirror":     "remote.Origin.mirror", // the subsection is exact
		"branch.Feat.x.PushRemote": "branch.Feat.x.pushremote",
		"nodots":                   "nodots",
		"A":                        "a",
	} {
		if got := canonKey(k); got != want {
			t.Errorf("canonKey(%q) = %q, want %q", k, got, want)
		}
	}
	if canonKey("remote.Origin.mirror") == canonKey("remote.origin.mirror") {
		t.Error("remote.Origin and remote.origin are different remotes to git")
	}
}

func TestPushGate_AnOverrideOfADifferentlyCasedRemoteDoesNotNarrowAMirror(t *testing.T) {
	stubGitConfig(t, map[string][]string{"remote.origin.mirror": {"true"}})
	cfg, _ := refPushEnv(t, shaGated)
	// Firing control: the file config alone mirrors, so a push to origin is refused.
	assertDeny(t, PushGate(bashInput("git push origin"), cfg), "mirror from the file")
	// The same-case override genuinely narrows it, as git does.
	assertAllow(t, PushGate(bashInput("git -c remote.origin.mirror=false push origin"), cfg), "same-case override narrows")
	// A differently-cased REMOTE NAME is a different remote: git still mirrors origin.
	assertDeny(t, PushGate(bashInput("git -c remote.Origin.mirror=false push origin"), cfg), "the override names another remote")
	// ...while the case of the section and variable name does fold.
	assertAllow(t, PushGate(bashInput("git -c REMOTE.origin.MIRROR=false push origin"), cfg), "section and variable fold")
}

func TestPushGate_AnOverrideOfADifferentlyCasedBranchDoesNotRedirectTheRemoteLookup(t *testing.T) {
	// branch.feat.pushRemote sends a bare push to a remote that has a widening refspec.
	stubGitConfig(t, map[string][]string{
		"branch.feat.pushRemote": {"fork"},
		"remote.fork.push":       {"refs/heads/*:refs/heads/*"},
	})
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat", true
		}
		return "", false
	})
	cfg, _ := refPushEnv(t, shaGated)
	assertDeny(t, PushGate(bashInput("git push"), cfg), "bare push goes to fork, which has a widening refspec")
	assertDeny(t, PushGate(bashInput("git -c branch.FEAT.pushRemote=origin push"), cfg), "a differently-cased branch name is another branch to git")
	assertAllow(t, PushGate(bashInput("git -c branch.feat.pushRemote=origin push"), cfg), "same-case override redirects to a remote with no refspec")
}

func TestPushGate_ConfigKeyCaseFoldsInTheSectionAndVariableOnly(t *testing.T) {
	stubGitConfig(t, map[string][]string{})
	cfg, _ := refPushEnv(t, shaGated)
	// The section and variable name fold: these ARE push.default / remote.origin.push.
	assertDeny(t, PushGate(bashInput("git -c Push.Default=matching push"), cfg), "section and variable fold (push.default)")
	assertDeny(t, PushGate(bashInput("git -c REMOTE.origin.PUSH=a:b push origin"), cfg), "section and variable fold (remote push)")
	// The subsection does not: remote.Origin is another remote, so its refspec does not affect origin.
	assertAllow(t, PushGate(bashInput("git -c remote.Origin.push=a:b push origin"), cfg), "a differently-cased remote's refspec is another remote's")
	// A folded-case narrowing of a matching file config.
	stubGitConfig(t, map[string][]string{"push.default": {"matching"}})
	assertAllow(t, PushGate(bashInput("git -c PUSH.DEFAULT=simple push"), cfg), "a folded-case override narrows")
}
