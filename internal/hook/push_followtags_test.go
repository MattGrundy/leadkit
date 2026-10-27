package hook

import (
	"strings"
	"testing"
)

// Ticket 229. `git push --tags` was refused and `git push --follow-tags` allowed, so the guard
// enforced how tags are spelled, not whether they may be pushed; and a semver tag starts a release.
// --follow-tags is a ref-set option like --tags, and so is the config that turns it on.

func TestPushRefSetOption_FollowTagsAndItsAbbreviations(t *testing.T) {
	for arg, want := range map[string]string{
		"--follow-tags": "follow-tags", "--follow-tag": "follow-tags", "--follow": "follow-tags", "--fol": "follow-tags",
		"--follow-tags=x": "follow-tags",
		// Not ref-set options: the neighbours it must not swallow.
		"--force": "", "--force-with-lease": "", "--force-if-includes": "", "--no-follow-tags": "", "--no-verify": "",
		"--set-upstream": "", "--dry-run": "",
	} {
		if got := pushRefSetOption(arg); got != want {
			t.Errorf("pushRefSetOption(%q) = %q, want %q", arg, got, want)
		}
	}
}

func TestPushGuard_FollowTagsIsDeniedWhateverTheSpellingAndSaysWhatToDoInstead(t *testing.T) {
	noRemoteStub(t)
	for _, opt := range []string{"--follow-tags", "--follow-tag", "--fol"} {
		cmd := "git push " + opt + " origin feat/ok"
		res := PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
		assertDeny(t, res, cmd)
		for _, want := range []string{"--follow-tags", "a tag push can start a release", "deliberately as its own command"} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("%s: the refusal must say %q, got:\n%s", cmd, want, res.Stdout)
			}
		}
	}
	// Every ref-set option now says what to do instead.
	for opt, want := range map[string]string{"--all": "Name the branch", "--mirror": "Name the branch", "--branches": "Name the branch", "--tags": "deliberately as its own command"} {
		res := PushGuard(pushInput("git push "+opt+" origin feat/ok"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("%s: the refusal must say %q, got:\n%s", opt, want, res.Stdout)
		}
	}
	// CONTROL: the same push without the option is whitelisted, and so is its negation.
	for _, cmd := range []string{"git push origin feat/ok", "git push --no-follow-tags origin feat/ok"} {
		assertAllow(t, PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), cmd)
	}
}

func TestPushGate_FollowTagsPublishesRefsTheCommandDoesNotName(t *testing.T) {
	for _, opt := range []string{"--follow-tags", "--follow-tag", "--fol"} {
		cmd := "git push " + opt + " origin"
		cfg, _ := refPushEnv(t, shaGated)
		res := PushGate(bashInput(cmd), cfg)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "could not determine") {
			t.Errorf("%s: want the commits-not-determined refusal, got:\n%s", cmd, res.Stdout)
		}
	}
}

// `push.followTags=true` is the same option as configuration, for a push that names a branch too.
func TestPushGuardAndGate_PushFollowTagsConfigIsRefusedAndTheNegationLiftsIt(t *testing.T) {
	noRemoteStub(t)
	stubGitConfig(t, map[string][]string{"push.followTags": {"true"}})
	guard := func(cmd string) Result {
		return PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
	}
	for _, cmd := range []string{"git push origin feat/ok", "git push", "git -c push.followTags=true push origin feat/ok"} {
		res := guard(cmd)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "push.followTags") {
			t.Errorf("%s: want the config named, got:\n%s", cmd, res.Stdout)
		}
	}
	cfg, _ := refPushEnv(t, shaGated)
	assertDeny(t, PushGate(bashInput("git push origin feat/gated"), cfg), "gate, push.followTags=true in the files")

	// The push turns it off for itself, in git's own terms.
	assertAllow(t, guard("git push --no-follow-tags origin feat/ok"), "--no-follow-tags")
	cfg, _ = refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push --no-follow-tags origin feat/gated"), cfg), "gate, --no-follow-tags")
	// The command line beats the files, last value wins.
	assertAllow(t, guard("git -c push.followTags=false push origin feat/ok"), "-c push.followTags=false over a true file value")

	stubGitConfig(t, map[string][]string{"push.followTags": {"false"}})
	assertAllow(t, guard("git push origin feat/ok"), "push.followTags=false")
	stubGitConfig(t, map[string][]string{})
	assertAllow(t, guard("git push origin feat/ok"), "push.followTags unset")
}
