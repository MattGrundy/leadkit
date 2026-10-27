package hook

import (
	"strings"
	"testing"
)

// Ticket 163. A push that publishes refs the command does not name was judged by the current
// branch when the option was abbreviated, or when git config widened a bare push.

func TestPushRefSetOption(t *testing.T) {
	cases := map[string]string{
		"--all": "all", "--al": "all", "--a": "all",
		"--mirror": "mirror", "--mir": "mirror", "--m": "mirror",
		"--tags": "tags", "--tag": "tags", "--t": "tags", "--tags=x": "tags",
		"--branches": "branches", "--branch": "branches", "--b": "branches",
		"--force": "", "--force-with-lease=x": "", "--dry-run": "", "--delete": "", "--set-upstream": "", "--no-verify": "",
		"-f": "", "-u": "", "--": "", "origin": "", "feat/x": "",
	}
	for arg, want := range cases {
		if got := pushRefSetOption(arg); got != want {
			t.Errorf("pushRefSetOption(%q) = %q, want %q", arg, got, want)
		}
	}
}

// Every abbreviation git accepts is judged like its full spelling, by the gate and by the whitelist.
func TestPushGate_AbbreviatedRefSetOptions_AreRefusedLikeTheFullSpelling(t *testing.T) {
	for _, opt := range []string{"--all", "--al", "--mirror", "--mir", "--tags", "--tag", "--t", "--branches", "--branch"} {
		cmd := "git push " + opt + " origin"
		cfg, _ := refPushEnv(t, shaGated) // the session HEAD is gated: a HEAD judgement would let it by
		res := PushGate(bashInput(cmd), cfg)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "could not determine") {
			t.Errorf("%s: want the commits-not-determined refusal, got:\n%s", cmd, res.Stdout)
		}
	}
}

func TestPushGuard_RefSetOptionsAreDeniedWhateverTheSpelling(t *testing.T) {
	noRemoteStub(t)
	for _, opt := range []string{"--tags", "--tag", "--all", "--al", "--mirror", "--mir", "--branches", "--branch"} {
		cmd := "git push " + opt + " origin feat/ok"
		assertDeny(t, PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), cmd)
	}
	// CONTROL: the same push without the option is whitelisted.
	assertAllow(t, PushGuard(pushInput("git push origin feat/ok"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), "plain whitelisted push")
}

// A bare push is the current branch only if git is not configured to publish more.
func stubGitConfig(t *testing.T, values map[string][]string) {
	t.Helper()
	orig, origBool := gitConfigValues, gitBool
	gitConfigValues = func(repo, key string) []string { return values[key] }
	// The unit-test stand-in for git's boolean reader, for the plain spellings the unit tests use;
	// every spelling git accepts is pinned against real git in push_bool_test.go.
	gitBool = func(repo string, cmd []string, key string) (value, set bool) {
		for i := len(cmd) - 1; i >= 0; i-- {
			if k, v, _ := strings.Cut(cmd[i], "="); canonKey(k) == canonKey(key) {
				return strings.EqualFold(v, "true") || strings.EqualFold(v, "yes"), true
			}
		}
		if vs := values[key]; len(vs) > 0 {
			v := vs[len(vs)-1]
			return strings.EqualFold(v, "true") || strings.EqualFold(v, "yes"), true
		}
		return false, false
	}
	t.Cleanup(func() { gitConfigValues, gitBool = orig, origBool })
}

func TestPushGate_BarePushThatGitConfigWidens_IsRefusedNotJudgedByHead(t *testing.T) {
	cases := map[string]map[string][]string{
		"push.default=matching":            {"push.default": {"matching"}},
		"push.default=MATCHING (any case)": {"push.default": {"MATCHING"}},
		"remote.origin.push":               {"remote.origin.push": {"refs/heads/*:refs/heads/*"}},
	}
	for name, cfgValues := range cases {
		for _, cmd := range []string{"git push", "git push origin"} {
			t.Run(name+" / "+cmd, func(t *testing.T) {
				stubGitConfig(t, cfgValues)
				cfg, _ := refPushEnv(t, shaGated) // gated HEAD: judging HEAD would allow it
				res := PushGate(bashInput(cmd), cfg)
				assertDeny(t, res, cmd)
				if !strings.Contains(res.Stdout, "could not determine") {
					t.Errorf("want the commits-not-determined refusal, got:\n%s", res.Stdout)
				}
			})
		}
	}
}

func TestPushGate_BarePushWithOrdinaryConfig_IsStillJudgedByTheCurrentBranch(t *testing.T) {
	// push.default=simple/current/upstream publish one ref; another remote's push refspec is not this push's.
	for name, cfgValues := range map[string]map[string][]string{
		"unset":                  {},
		"push.default=simple":    {"push.default": {"simple"}},
		"push.default=upstream":  {"push.default": {"upstream"}},
		"other remote's refspec": {"remote.backup.push": {"refs/heads/*:refs/heads/*"}},
		"matching then simple":   {"push.default": {"matching", "simple"}}, // the last value wins in git
	} {
		t.Run(name, func(t *testing.T) {
			stubGitConfig(t, cfgValues)
			cfg, _ := refPushEnv(t, shaGated)
			assertAllow(t, PushGate(bashInput("git push"), cfg), "bare push on a gated HEAD")
			cfg, _ = refPushEnv(t, shaUngated)
			assertDeny(t, PushGate(bashInput("git push"), cfg), "bare push on an ungated HEAD")
		})
	}
}

// A refspec that names the branch is not widened by config: the config only matters when none is named.
func TestPushGate_NamedRefIsNotWidenedByGitConfig(t *testing.T) {
	stubGitConfig(t, map[string][]string{"push.default": {"matching"}, "remote.origin.push": {"x:y"}})
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push origin feat/gated"), cfg), "named gated ref")
}

// The remote a bare push uses comes from the branch when no remote is named.
func TestUnnamedRefSetConfig_UsesTheBranchsRemote(t *testing.T) {
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/x", true
		}
		return "", false
	})
	stubGitConfig(t, map[string][]string{"branch.feat/x.remote": {"upstream"}, "remote.upstream.push": {"a:b"}})
	if unnamedRefSetConfig("/r", "", nil) == "" {
		t.Error("a bare push on a branch whose remote has a push refspec must be flagged")
	}
	if unnamedRefSetConfig("/r", "origin", nil) != "" {
		t.Error("an explicitly named remote is the one consulted, and origin has no push refspec here")
	}
}

// The push-guard (whitelist) must not judge such a bare push by the current branch either.
func TestPushGuard_BarePushThatGitConfigWidens_IsFailClosed(t *testing.T) {
	withGitStub(t, func(repo string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/ok", true
		}
		return "", false
	})
	stubGitConfig(t, map[string][]string{"push.default": {"matching"}})
	assertDeny(t, PushGuard(pushInput("git push"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), "bare push under push.default=matching")
	stubGitConfig(t, map[string][]string{})
	assertAllow(t, PushGuard(pushInput("git push"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}), "bare push on a whitelisted branch")
}
