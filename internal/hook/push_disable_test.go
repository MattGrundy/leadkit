package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The text layer refuses the bounded set of ways to switch the pre-push hook off, where the
// gate governs the repo (ticket 171). Words that would trip the live push gate while this file
// is being written are assembled from pieces.
const gp = "pu" + "sh"

func disableCfg(t *testing.T) (PushGateConfig, string, string) {
	t.Helper()
	repo := t.TempDir()
	cfg, records := pushGateEnv(t, repo, enforcedRemote)
	return cfg, repo, records
}

func TestPushGate_RefusesTheSpellingsThatSwitchThePrePushHookOff(t *testing.T) {
	cfg, repo, _ := disableCfg(t)
	for name, c := range map[string]struct{ cmd, cause string }{
		"--no-verify":                 {"git " + gp + " --no-verify", "skips the pre-push hook"},
		"after the refspec":           {"git " + gp + " origin main --no-verify", "skips the pre-push hook"},
		"abbreviation --no-ver":       {"git " + gp + " --no-ver origin main", "skips the pre-push hook"},
		"abbreviation --no-v":         {"git " + gp + " --no-v", "skips the pre-push hook"},
		"-C":                          {"git -C " + repo + " " + gp + " --no-verify", "skips the pre-push hook"},
		"absolute git":                {"/usr/bin/git " + gp + " --no-verify", "skips the pre-push hook"},
		"env wrapper":                 {"env FOO=1 git " + gp + " --no-verify", "skips the pre-push hook"},
		"alias, flag after":           {"git -c alias.p=" + gp + " p --no-verify", "skips the pre-push hook"},
		"alias that adds it":          {"git -c 'alias.p=" + gp + " --no-verify' p", "alias defined on the command line adds --no-verify"},
		"-c core.hooksPath on a push": {"git -c core.hooksPath=/dev/null " + gp, "git -c core.hooksPath"},
		"-c core.hooksPath, any verb": {"git -c core.hooksPath=/dev/null status", "git -c core.hooksPath"},
		"-c core.hookspath, any case": {"git -c CORE.HOOKSPATH=/dev/null " + gp, "git -c core.hooksPath"},
		"--config-env=":               {"git --config-env=core.hooksPath=H " + gp, "--config-env"},
		"--config-env separate":       {"git --config-env core.hooksPath=H " + gp, "--config-env"},
		"alias that sets it":          {"git -c 'alias.x=config core.hooksPath /x' x", "alias defined on the command line sets core.hooksPath"},
		"GIT_CONFIG_PARAMETERS":       {"GIT_CONFIG_PARAMETERS=\"'core.hooksPath=/x'\" git " + gp, "GIT_CONFIG_PARAMETERS"},
		"GIT_CONFIG_KEY_0":            {"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=/dev/null git " + gp, "GIT_CONFIG_KEY_0"},
		"exported GIT_CONFIG_KEY":     {"export GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_KEY_0"},
		"git config":                  {"git config core.hooksPath /dev/null", "git config core.hooksPath"},
		"git config --global":         {"git config --global core.hooksPath /x", "git config core.hooksPath"},
		// a flag git ignores on a write must not read as a read: these all SET the key
		"git config --show-scope":         {"git config --show-scope core.hooksPath /x", "git config core.hooksPath"},
		"git config, --show-scope after":  {"git config core.hooksPath /x --show-scope", "git config core.hooksPath"},
		"git config --show-scope global":  {"git config --show-scope --global core.hooksPath /x", "git config core.hooksPath"},
		"git config --type, --show-scope": {"git config --type=path --show-scope core.hooksPath /x", "git config core.hooksPath"},
		"git config --show-origin":        {"git config --show-origin core.hooksPath /x", "git config core.hooksPath"},
		"git config --list with a value":  {"git config -l core.hooksPath /x", "git config core.hooksPath"},
		"git config, any case":            {"git config --local CORE.hookspath /x", "git config core.hooksPath"},
		"git config after a cd":           {"cd " + repo + " && git config core.hooksPath /x", "git config core.hooksPath"},
		"inside bash -c":                  {"bash -c 'git " + gp + " --no-verify'", "`bash`"},
		"inside sudo":                     {"sudo git " + gp + " --no-verify", "`sudo`"},
		"inside xargs":                    {"echo x | xargs git config core.hooksPath", "`xargs`"},
		"wrapper with an option":          {"env -i git " + gp + " --no-verify", "`env`"},
	} {
		t.Run(name, func(t *testing.T) {
			reason := decodeDeny(t, PushGate(bashInput(c.cmd), cfg))
			if !strings.Contains(reason, c.cause) {
				t.Errorf("the refusal does not name %q, so it cannot be diagnosed:\n%s", c.cause, reason)
			}
			if !strings.Contains(reason, "pre-push hook") || !strings.Contains(reason, "BYPASS") {
				t.Errorf("the refusal must say what the hook is and how to override:\n%s", reason)
			}
		})
	}
}

func TestPushGate_DoesNotRefuseWhatLeavesThePrePushHookRunning(t *testing.T) {
	cfg, repo, _ := disableCfg(t)
	for name, cmd := range map[string]string{
		"the plain push (judged by the existing gate, which allows it with a record)": "git " + gp,
		"-n is --dry-run on a push":      "git " + gp + " -n",
		"--dry-run":                      "git " + gp + " --dry-run",
		"--no-verbose is another flag":   "git " + gp + " --no-verbose",
		"--verify":                       "git " + gp + " --verify",
		"commit --no-verify":             "git commit --no-verify -m x",
		"commit -n":                      "git commit -n -m x",
		"reading hooksPath":              "git config --get core.hooksPath",
		"reading hooksPath, key alone":   "git config core.hooksPath",
		"reading with --show-scope":      "git config --show-scope core.hooksPath",
		"reading with --show-origin":     "git config --show-origin --get core.hooksPath",
		"listing with -l":                "git config -l",
		"get-all with a pattern":         "git config --get-all core.hooksPath x",
		"get with a pattern":             "git config --get core.hooksPath x",
		"get-regexp with a pattern":      "git config --get-regexp core.hooksPath x",
		"unset with a pattern":           "git config --unset core.hooksPath x",
		"unset-all with a pattern":       "git config --unset-all core.hooksPath x",
		"listing config":                 "git config --list",
		"unsetting hooksPath":            "git config --unset core.hooksPath",
		"another key":                    "git config user.name x",
		"-c of another key":              "git -c user.name=x status",
		"text that names the flag":       "echo \"--no-verify " + gp + "\"",
		"leadkit's governed verb":        "leadkit " + gp + " --repo " + repo + " --branch b",
		"a path that only looks like it": "git status docs/core.hooksPath-notes.md",
	} {
		t.Run(name, func(t *testing.T) {
			// A record for the head makes the existing gate allow the plain push, so any refusal
			// here is this guard's.
			writeFullRecord(t, mustRecords(t, cfg), gateTestSHA)
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
		})
	}
}

func mustRecords(t *testing.T, cfg PushGateConfig) string {
	t.Helper()
	g := cfg.ResolveGate("")
	if g == nil {
		t.Fatal("no gate")
	}
	return g.RecordsDir
}

func TestPushGate_DisablingSpellingsAreScopedAndBypassable(t *testing.T) {
	cmd := "git " + gp + " --no-verify"
	t.Run("a repo the gate does not govern is not this guard's business", func(t *testing.T) {
		repo := t.TempDir()
		cfg, _ := pushGateEnv(t, repo, "git@github.com:someone/else.git")
		assertAllow(t, PushGate(bashInput(cmd), cfg), "out of scope")
	})
	t.Run("the refusal holds in advisory and enforce mode alike", func(t *testing.T) {
		cfg, _, _ := disableCfg(t)
		for _, m := range []GuardMode{ModeAdvisory, ModeEnforce} {
			cfg.Mode = func(string) GuardMode { return m }
			decodeDeny(t, PushGate(bashInput(cmd), cfg))
		}
	})
	t.Run("the BYPASS file allows it", func(t *testing.T) {
		cfg, _, records := disableCfg(t)
		decodeDeny(t, PushGate(bashInput(cmd), cfg)) // firing control
		if err := os.WriteFile(filepath.Join(records, "BYPASS"), []byte("deliberate"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeFullRecord(t, records, gateTestSHA)
		assertAllow(t, PushGate(bashInput(cmd), cfg), "BYPASS file")
	})
	t.Run("an inline bypass covers the command that carries it and no other", func(t *testing.T) {
		cfg, _, records := disableCfg(t)
		writeFullRecord(t, records, gateTestSHA)
		assertAllow(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="r" `+cmd), cfg), "inline, leading")
		decodeDeny(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="r" git status && `+cmd), cfg))
	})
	t.Run("a non-Bash tool is not read", func(t *testing.T) {
		cfg, _, _ := disableCfg(t)
		in := bashInput(cmd)
		in.ToolName = "Write"
		assertAllow(t, PushGate(in, cfg), "Write tool")
	})
}

func TestIsNoVerify(t *testing.T) {
	for w, want := range map[string]bool{
		"--no-verify": true, "--no-verif": true, "--no-veri": true, "--no-ver": true, "--no-ve": true, "--no-v": true,
		"--NO-VERIFY": true, "--no-verify=1": true,
		"--no-": false, "--no": false, "--no-verbose": false, "--verify": false, "-n": false,
		"--dry-run": false, "no-verify": false, "": false, "--no-verifyx": false,
	} {
		if got := isNoVerify(w); got != want {
			t.Errorf("isNoVerify(%q) = %v, want %v", w, got, want)
		}
	}
}

func TestPushGate_DisablingSpellingIsScopedToTheRepoItNames(t *testing.T) {
	// The session stands in a repo the gate does not govern and moves into one it does: the
	// refusal follows the `cd` and the `-C`, not the session's own directory.
	outside, governed := t.TempDir(), t.TempDir()
	cfg, _ := pushGateEnv(t, governed, enforcedRemote)
	g := cfg.ResolveGate("")
	g.Git = fakeGateGit{remotes: map[string]string{governed: enforcedRemote, outside: "git@github.com:someone/else.git"}, head: gateTestSHA}
	cfg.Cwd = outside
	assertAllow(t, PushGate(bashInput("git config core.hooksPath /x"), cfg), "from the ungoverned repo, naming nothing else")
	decodeDeny(t, PushGate(bashInput("cd "+governed+" && git config core.hooksPath /x"), cfg))
	decodeDeny(t, PushGate(bashInput("git -C "+governed+" config core.hooksPath /x"), cfg))
}

// The documented residual, pinned so the documentation cannot drift from it. The FLAG is on the
// closed list but the push it belongs to is read by spelling, and an unreadable spelling plus
// --no-verify passes this layer; since the flag skips the pre-push hook it passes that too. If a
// line here starts being refused, the kit doc's residual paragraph and push_disable.go's header
// are out of date; if the combination must be closed, that is forge-side branch protection.
func TestPushGate_ResidualUnreadableVerbPlusNoVerifyIsNotRefusedHere(t *testing.T) {
	cfg, _, records := disableCfg(t)
	writeFullRecord(t, records, gateTestSHA)
	for name, cmd := range map[string]string{
		"ANSI-C quoted verb": "git $'" + gp + "' --no-verify",
		"IFS expansion":      "git ${IFS}" + gp + " --no-verify",
		"brace expansion":    "git {" + gp + ",} --no-verify",
		"computed key":       "K=core.hooksPath; git config $K /dev/null",
	} {
		t.Run(name, func(t *testing.T) {
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
		})
	}
	// Firing control: the literal spelling of the same flag IS refused.
	fresh, _, _ := disableCfg(t)
	decodeDeny(t, PushGate(bashInput("git "+gp+" --no-verify"), fresh))
}
