package hook

import (
	"os"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// A push whose target repo the hooks cannot pin down must be refused, not judged by nothing. The
// target is read the way cd reads it (options and `--` are not the directory), and a target built
// from something the hook cannot evaluate (a command substitution, a variable the command set,
// `cd -`) is unknown. A plain path the command spells as its first command, that does not exist,
// is a different thing and stays allowed: the cd or `git -C` fails at check time and no push runs.
// The rule is
// in pathUnknownWhy; the wider shapes are pinned in push_cd_unknown_test.go.

func TestCdTarget_ReadsTheDirectoryTheWayCdDoes(t *testing.T) {
	cases := []struct {
		cmd        string
		wantTarget string
		wantUnsure bool
	}{
		{"cd /r && git push origin x", "/r", false},
		{"cd -- /r && git push origin x", "/r", false},
		{"cd -P /r && git push origin x", "/r", false},
		{"cd -L /r && git push origin x", "/r", false},
		{"cd -e /r && git push origin x", "/r", false},
		{"cd -@ /r && git push origin x", "/r", false},
		{"cd -LP /r && git push origin x", "/r", false},
		{"cd -L -P /r && git push origin x", "/r", false},
		{"cd -P -- /r && git push origin x", "/r", false},
		{`cd -- "/r" && git push origin x`, "/r", false},
		{"cd -- -weird && git push origin x", "-weird", false}, // after `--` a dash is a name
		{"cd && git push origin x", "~", false},                // no operand is HOME
		{"cd -- && git push origin x", "~", false},
		{"cd - && git push origin x", "", true},               // $OLDPWD
		{"cd -x /r && git push origin x", "", true},           // an option cd does not have
		{"cd /a /b && git push origin x", "", true},           // bash refuses, zsh does not
		{`cd "$(echo /r)" && git push origin x`, "", true},    // computed
		{"cd -- `echo /r` && git push origin x", "", true},    // computed
		{`cd -P "$(echo /r)" && git push origin x`, "", true}, // computed after an option
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			p := parsePushCommands(c.cmd)
			if c.wantUnsure {
				if p.State != pushUnsure {
					t.Fatalf("want pushUnsure, got %+v", p)
				}
				return
			}
			if p.State != pushFound || len(p.Invocations) != 1 || p.Invocations[0].repoPath != c.wantTarget {
				t.Fatalf("want one push in %q, got %+v", c.wantTarget, p)
			}
		})
	}
}

// repoHints is how an unreadable command's `cd` target is found; it skipped `--` only.
func TestRepoHints_SkipCdOptions(t *testing.T) {
	for _, cmd := range []string{
		"bash -c 'cd /r && x'", "bash -c 'cd -- /r && x'", "bash -c 'cd -P /r && x'",
		"bash -c 'cd -LP /r && x'", "bash -c 'cd -L -- /r && x'",
	} {
		if got := repoHints(cmd); len(got) != 1 || got[0] != "/r" {
			t.Errorf("repoHints(%q) = %q, want [/r]", cmd, got)
		}
	}
}

// cdTargetEnv is a session in `session` and a gated repo `target`. feat/ungated has no
// record, feat/gated has one. The session repo is in scope when sessionGated, else out.
func cdTargetEnv(t *testing.T, sessionGated bool) (cfg PushGateConfig, session, target string) {
	t.Helper()
	t.Setenv("REVIEW_GATE_BYPASS", "")
	session, target = t.TempDir(), t.TempDir()
	records := t.TempDir()
	writeFullRecord(t, records, shaGated2)
	sessionRemote := "git@github.com:other/unrelated.git"
	if sessionGated {
		sessionRemote = enforcedRemote
	}
	g := &gate.Gate{
		RecordsDir: records,
		Git: fakeGateGit{
			remotes: map[string]string{session: sessionRemote, target: enforcedRemote},
			head:    shaGated,
			refs:    map[string]string{"feat/ungated": shaUngated, "feat/gated": shaGated2},
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	return PushGateConfig{ResolveGate: staticGate(g), Cwd: session}, session, target
}

// THE defect: from any session, an option on the cd made the push unjudged. The in-scope
// session is the case that needed no setup at all.
func TestPushGate_CdWithOptions_IsJudgedByTheRepoItMovesTo(t *testing.T) {
	for _, sessionGated := range []bool{true, false} {
		cfg, _, target := cdTargetEnv(t, sessionGated)
		for _, prefix := range []string{"cd ", "cd -- ", "cd -P ", "cd -L ", "cd -LP ", "cd -e -- ", "cd -P -- "} {
			cmd := prefix + target + " && git push origin feat/ungated"
			assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
			// CONTROL: the same shapes publishing a gated commit pass, so the refusal above
			// is a judgement of the target and not a refusal of every option-shaped cd.
			ok := prefix + target + " && git push origin feat/gated"
			assertAllow(t, PushGate(bashInput(ok), cfg), ok)
		}
	}
}

// A target the hook cannot name is refused, whatever the session's scope, unless bypassed.
func TestPushGate_UnnameableTarget_IsRefusedUnlessBypassed(t *testing.T) {
	cmds := func(target string) []string {
		return []string{
			`cd "$(echo ` + target + `)" && git push origin feat/ungated`,
			`git -C "$(echo ` + target + `)" push origin feat/ungated`,
			`cd -- "$(echo ` + target + `)" && git push origin feat/ungated`,
			"cd - && git push origin feat/ungated",
			"cd $LK_169_SURELY_UNSET/x && git push origin feat/ungated",
			"R=" + target + "; git -C $R push origin feat/ungated",
			"R=" + target + "; cd $R && git push origin feat/ungated",
			// the unreadable command, whose hint is computed
			`bash -c 'cd "$(echo ` + target + `)" && git push origin feat/ungated'`,
			`bash -c 'cd $LK_169_SURELY_UNSET && git push origin feat/ungated'`,
		}
	}
	for _, sessionGated := range []bool{true, false} {
		for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
			cfg, _, target := cdTargetEnv(t, sessionGated)
			cfg.Mode = func(string) GuardMode { return mode }
			for _, cmd := range cmds(target) {
				assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
			}
			// The escape hatch lets it through, like every other refusal of this hook.
			t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
			for _, cmd := range cmds(target) {
				assertAllow(t, PushGate(bashInput(cmd), cfg), "bypassed: "+cmd)
			}
			t.Setenv("REVIEW_GATE_BYPASS", "")
		}
	}
}

// The distinction the fix rests on, and its limits. A PLAIN path (no shell metacharacter) that
// is the first command's directory and is not one is a cd or `git -C` that fails at check time, so
// no push runs then: that stays the mode-dependent bad-repo answer (advisory allows). Refusing it would
// break ordinary use. Without this control a fix that refused every unresolvable path would
// pass the tests above. The cases that are NOT this one (a glob, a directory an earlier command
// may create, ...) are in push_cd_unknown_test.go.
func TestPushGate_SpelledPathThatDoesNotExist_StaysTheModeDependentBadRepoAnswerAllow(t *testing.T) {
	cfg, _, _ := cdTargetEnv(t, false)
	for _, cmd := range []string{
		"cd /no/such/dir/169 && git push origin feat/ungated",
		"cd -- /no/such/dir/169 && git push origin feat/ungated",
		"git -C /no/such/dir/169 push origin feat/ungated",
	} {
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd+" (advisory)")
	}
	cfg.Mode = func(string) GuardMode { return ModeEnforce }
	assertDeny(t, PushGate(bashInput("cd /no/such/dir/169 && git push origin feat/ungated"), cfg), "enforce still fails closed on a bad repo")
}

// A variable the hook DOES hold is read, not refused: `cd $HOME/x` is ordinary.
func TestPushGate_VariableTheHookHolds_IsExpanded(t *testing.T) {
	cfg, _, target := cdTargetEnv(t, false)
	t.Setenv("LK_169_TARGET", target)
	assertDeny(t, PushGate(bashInput("cd $LK_169_TARGET && git push origin feat/ungated"), cfg), "set variable, ungated push")
	assertAllow(t, PushGate(bashInput("cd $LK_169_TARGET && git push origin feat/gated"), cfg), "set variable, gated push")
	if _, known := resolveRepoPath("$LK_169_NEVER_SET/x", "", ""); known {
		t.Errorf("an unset variable names no directory, so the path is unknown")
	}
	t.Setenv("LK_169_EMPTY", "")
	if _, known := resolveRepoPath("$LK_169_EMPTY/x", "", ""); known {
		t.Errorf("an empty variable names no directory, so the path is unknown")
	}
	if p, known := resolveRepoPath("~/x", "", "/home/u"); p != "/home/u/x" || !known {
		t.Errorf("~ expands, got %q %v", p, known)
	}
}

func TestGateCheckUnknownTarget(t *testing.T) {
	t.Setenv("REVIEW_GATE_BYPASS", "")
	g := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeGateGit{}}
	ok, outcome, reason := g.CheckUnknownTarget("because")
	if ok || outcome != gate.OutcomeUngated || !strings.Contains(reason, "because") {
		t.Errorf("no bypass: want a refusal in every mode, got ok=%v outcome=%v %q", ok, outcome, reason)
	}
	if err := os.WriteFile(g.RecordsDir+"/BYPASS", []byte("why"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, outcome, _ := g.CheckUnknownTarget("because"); !ok || outcome != gate.OutcomeGated {
		t.Errorf("BYPASS file: want allow, got ok=%v outcome=%v", ok, outcome)
	}
}
