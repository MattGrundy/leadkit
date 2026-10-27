package hook

import (
	"reflect"
	"testing"
)

// Round 3 review. A push option whose value is the NEXT word must not make that word the remote: the
// remote-keyed settings (mirror, remote.<r>.push) would be looked up on a remote git does not use, and a
// push that mirrors or widens would be judged by HEAD alone.

func TestParsePushArgs_AnOptionsSeparateValueIsNotTheRemote(t *testing.T) {
	cases := []struct {
		args       []string
		wantRemote string
		wantRefs   []string
	}{
		{[]string{"-o", "ci.skip"}, "", nil},
		{[]string{"--push-option", "ci.skip"}, "", nil},
		{[]string{"--push-opt", "x"}, "", nil}, // a unique abbreviation
		{[]string{"--push-option=x"}, "", nil},
		{[]string{"--receive-pack", "x"}, "", nil},
		{[]string{"--rece", "x"}, "", nil},
		{[]string{"--exec", "x"}, "", nil},
		{[]string{"--recurse-submodules", "on-demand"}, "", nil},
		{[]string{"--recurse-submodules", "check", "origin"}, "origin", nil},
		{[]string{"--recurse-s", "check"}, "", nil}, // a unique abbreviation
		{[]string{"--recurse-submodules=check", "origin"}, "origin", nil},
		{[]string{"--no-recurse-submodules", "origin"}, "origin", nil}, // negated: takes no value
		{[]string{"-uo", "x"}, "", nil},                                // a cluster ending in o takes the next word
		{[]string{"-ox"}, "", nil},                                     // attached
		{[]string{"-ox", "origin"}, "origin", nil},                     // attached: the value is not the next word
		// the value is attached whatever the letters, including a trailing o
		{[]string{"-ohello", "upstream"}, "upstream", nil},
		{[]string{"-ofoo", "upstream"}, "upstream", nil},
		{[]string{"-uohello", "upstream"}, "upstream", nil},
		{[]string{"-oxo", "upstream", "main"}, "upstream", []string{"main"}},
		{[]string{"-ono", "origin", "main"}, "origin", []string{"main"}},
		{[]string{"-fuo", "x", "upstream"}, "upstream", nil}, // a cluster whose first o is last
		{[]string{"-o", "hello", "upstream"}, "upstream", nil},
		{[]string{"-uo", "x", "upstream"}, "upstream", nil},
		{[]string{"-o", "x", "origin"}, "origin", nil},
		{[]string{"-o", "x", "origin", "feat/a"}, "origin", []string{"feat/a"}},
		{[]string{"origin", "-o", "x", "feat/a"}, "origin", []string{"feat/a"}},
		{[]string{"-o"}, "", nil}, // no value to take
		// not value options: the next word is the remote
		{[]string{"-u", "origin"}, "origin", nil},
		{[]string{"--force", "origin"}, "origin", nil},
		{[]string{"--force-with-lease", "origin"}, "origin", nil},
		{[]string{"--set-upstream", "origin", "feat/a"}, "origin", []string{"feat/a"}},
		{[]string{"--re", "origin"}, "origin", nil}, // ambiguous (repo, receive-pack, recurse-submodules): git refuses it
		{[]string{"--no-verify", "origin"}, "origin", nil},
	}
	for _, c := range cases {
		remote, refs, _ := parsePushArgs(c.args)
		if remote != c.wantRemote || len(refs) != len(c.wantRefs) || (len(refs) > 0 && !reflect.DeepEqual(refs, c.wantRefs)) {
			t.Errorf("parsePushArgs(%q) = remote %q refs %q, want %q %q", c.args, remote, refs, c.wantRemote, c.wantRefs)
		}
	}
}

// `--repo` is the one value option whose value IS the remote: it is equivalent to the <repository>
// argument, and a positional repository wins when both are given. Each row uses a NON-origin remote,
// because a fallback to origin would make an origin test unable to tell the bug from the fix.
func TestParsePushArgs_RepoOptionIsTheRemote(t *testing.T) {
	cases := []struct {
		args       []string
		wantRemote string
		wantRefs   []string
	}{
		{[]string{"--repo", "upstream"}, "upstream", nil},
		{[]string{"--repo=upstream"}, "upstream", nil},
		{[]string{"--rep", "upstream"}, "upstream", nil}, // a unique abbreviation
		{[]string{"--rep=upstream"}, "upstream", nil},
		{[]string{"--repo=upstream", "-f"}, "upstream", nil},
		{[]string{"-u", "--repo", "upstream"}, "upstream", nil},
		{[]string{"--repo", "upstream", "origin"}, "origin", nil}, // the positional repository wins
		{[]string{"--repo"}, "", nil},                             // no value to take
		{[]string{"--re", "origin"}, "origin", nil},               // ambiguous: not --repo
	}
	for _, c := range cases {
		remote, refs, _ := parsePushArgs(c.args)
		if remote != c.wantRemote || len(refs) != len(c.wantRefs) {
			t.Errorf("parsePushArgs(%q) = remote %q refs %q, want %q %q", c.args, remote, refs, c.wantRemote, c.wantRefs)
		}
	}
}

// The value-taking spellings, each with a config that widens the push, against a gated HEAD: judging by
// HEAD would let them by.
func TestPushGate_AValueTakingOptionDoesNotHideAMirrorOrWideningRefspec(t *testing.T) {
	for name, values := range map[string]map[string][]string{
		"mirror":                {"remote.origin.mirror": {"true"}},
		"remote.origin.push":    {"remote.origin.push": {"refs/heads/*:refs/heads/*"}},
		"push.default=matching": {"push.default": {"matching"}},
	} {
		for _, opt := range []string{"-o x", "--push-option x", "--push-opt x", "--receive-pack x", "--rece x", "--exec x", "-uo x", "--recurse-submodules on-demand", "--recurse-s check", "--no-recurse-submodules"} {
			cmd := "git push " + opt
			t.Run(name+" / "+cmd, func(t *testing.T) {
				stubGitConfig(t, values)
				cfg, _ := refPushEnv(t, shaGated)
				assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
			})
		}
	}
	// Controls: with nothing widening, the same commands are judged by the (gated) HEAD and pass, so the
	// refusals above come from the config and not from the option.
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{"git push -o x", "git push --push-option x", "git push --receive-pack x", "git push --exec x", "git push -uo x"} {
		cfg, _ := refPushEnv(t, shaGated)
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd+" with no widening config")
	}
	// ...and on an ungated HEAD they are still refused: the value did not become the target either.
	for _, cmd := range []string{"git push -o x", "git push --push-option x"} {
		cfg, _ := refPushEnv(t, shaUngated)
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd+" on an ungated HEAD")
	}
}

// The whitelist has the same blind spot: a bare push that mirrors is refused whatever option precedes it.
func TestPushGuard_AValueTakingOptionDoesNotHideAMirror(t *testing.T) {
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/ok", true
		}
		return "", false
	})
	stubGitConfig(t, map[string][]string{"remote.origin.mirror": {"true"}})
	for _, cmd := range []string{"git push -o x", "git push --push-option x", "git push --receive-pack x", "git push --exec x"} {
		assertDeny(t, PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), cmd)
	}
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{"git push -o x", "git push --push-option x"} {
		assertAllow(t, PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), cmd+" with no mirror")
	}
}

// `--repo upstream` publishes to upstream, so upstream's mirror setting governs the push. Reading the
// value as discardable leaves the remote empty and the lookup falls back to origin, which has no mirror:
// the push is then judged by a gated HEAD. The remote is NOT origin, so a fallback cannot pass this.
func TestPushGateAndGuard_RepoOptionNamesTheRemoteWhoseMirrorGoverns(t *testing.T) {
	values := map[string][]string{"remote.upstream.mirror": {"true"}}
	for _, cmd := range []string{"git push upstream", "git push -o hello upstream", "git push -ohello upstream", "git push -ofoo upstream", "git push -uohello upstream", "git push -oxo upstream main", "git push --repo upstream", "git push --repo=upstream", "git push --rep upstream", "git push -f --repo=upstream"} {
		t.Run("gate / "+cmd, func(t *testing.T) {
			stubGitConfig(t, values)
			cfg, _ := refPushEnv(t, shaGated)
			assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
		})
		t.Run("guard / "+cmd, func(t *testing.T) {
			withGitStub(t, func(repo string, args ...string) (string, bool) {
				if len(args) >= 1 && args[0] == "rev-parse" {
					return "feat/ok", true
				}
				return "", false
			})
			stubGitConfig(t, values)
			assertDeny(t, PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), cmd)
		})
	}
	// Control: the same spelling with origin named is not mirrored, so it is judged by the gated HEAD.
	stubGitConfig(t, values)
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push --repo origin"), cfg), "--repo origin, no mirror on origin")
	// A positional repository wins over --repo, as it does in git: the mirrored upstream is not the target.
	assertAllow(t, PushGate(bashInput("git push --repo upstream origin"), cfg), "positional origin wins over --repo upstream")
}

// `--recurse-submodules <mode>` takes its mode as the next word. Read as the remote, `check` would be the
// remote and `origin` a refspec, which refuses an ordinary documented spelling.
func TestPushGate_RecurseSubmodulesModeIsNotTheRemote(t *testing.T) {
	stubGitConfig(t, map[string][]string{})
	for _, cmd := range []string{"git push --recurse-submodules check origin feat/gated", "git push --recurse-s on-demand origin feat/gated", "git push --recurse-submodules=no origin feat/gated"} {
		cfg, _ := refPushEnv(t, shaGated)
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
	}
}
