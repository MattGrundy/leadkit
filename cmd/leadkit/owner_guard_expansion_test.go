package main

// End-to-end: a Bash deletion spelled with ~, $HOME or quotes reaches the same verdict as
// the plain spelling, through the real resolvers (see owner_guard_paths_test.go). The cwd is
// the target project throughout: a Bash command is governed by the policy of the project it
// runs in, so from a foreign session it is not governed at all.

import (
	"strings"
	"testing"
)

func TestOwnerGuardEndToEnd_BashExpansionAndQuotingOfAnOwnedFile(t *testing.T) {
	_, target := ownerGuardWorld(t)
	rm := func(cwd, cmd string) (int, string) {
		got := runOwnerGuard(t, cwd, "Bash", map[string]string{"command": cmd})
		return got.ExitCode, got.Stderr
	}

	// Controls: the plain spellings must FIRE, or the rows below prove nothing.
	if code, err := rm(target, "rm tasks/1.md"); code != 2 || !strings.Contains(err, "owned by 'tasks-agent'") {
		t.Fatalf("control (plain relative) = %d %q, want the ownership DENY", code, err)
	}
	if code, err := rm(target, "rm "+target+"/tasks/1.md"); code != 2 || !strings.Contains(err, "owned by 'tasks-agent'") {
		t.Fatalf("control (plain absolute) = %d %q, want the ownership DENY", code, err)
	}
	const deny = "Projects/targetproj/tasks/1.md is owned by 'tasks-agent'"
	for _, c := range []struct{ name, cwd, cmd string }{
		{"tilde", target, "rm ~/Projects/targetproj/tasks/1.md"},
		{"$HOME", target, "rm $HOME/Projects/targetproj/tasks/1.md"},
		{"${HOME}", target, "rm ${HOME}/Projects/targetproj/tasks/1.md"},
		{"double-quoted $HOME", target, `rm "$HOME/Projects/targetproj/tasks/1.md"`},
		{"quoted relative", target, `rm "tasks/1.md"`},
		{"single-quoted relative", target, `rm 'tasks/1.md'`},
		{"single-quoted absolute", target, "rm '" + target + "/tasks/1.md'"},
		{"quoted verb", target, `"rm" tasks/1.md`},
		{"tilde then ..", target, "rm ~/Projects/sess/../targetproj/tasks/1.md"},
		{"$HOME then ..", target, "rm $HOME/Projects/../Projects/targetproj/tasks/1.md"},
		{"$PWD", target, "rm $PWD/tasks/1.md"},
		{"mv with tilde", target, "mv ~/Projects/targetproj/tasks/1.md /tmp/x"},
		{"after a separator", target, "echo hi && rm ~/Projects/targetproj/tasks/1.md"},
	} {
		if code, err := rm(c.cwd, c.cmd); code != 2 || !strings.Contains(err, deny) {
			t.Errorf("%s: %q = %d %q, want the ownership DENY on the expanded path", c.name, c.cmd, code, err)
		}
	}

	// Not over-refused: the same spellings of a file the policy does not own, and spellings
	// that quote the expansion away, are allowed.
	for _, c := range []struct{ name, cwd, cmd string }{
		{"tilde, unowned file", target, "rm ~/Projects/targetproj/docs/a.md"},
		{"$HOME, unowned file", target, "rm $HOME/Projects/targetproj/docs/a.md"},
		{"quoted, unowned file", target, `rm "docs/a.md"`},
		{"tilde then .. out of the owned tree", target, "rm ~/Projects/targetproj/tasks/../docs/a.md"},
		{"single-quoted $HOME is a literal name", target, `rm '$HOME/Projects/targetproj/tasks/1.md'`},
		{"quoted ~ is a literal name", target, `rm "~/Projects/targetproj/tasks/1.md"`},
		{"unresolvable word, but not a destructive verb", target, "echo $UNSET_VAR"},
	} {
		if code, err := rm(c.cwd, c.cmd); code != 0 {
			t.Errorf("%s: %q = %d %q, want allow", c.name, c.cmd, code, err)
		}
	}

	// The owner is let through by the expanded spelling.
	t.Setenv("CLAUDE_AGENT_NAME", "tasks-agent")
	if code, err := rm(target, "rm ~/Projects/targetproj/tasks/1.md"); code != 0 {
		t.Errorf("owner, tilde spelling = %d %q, want allow", code, err)
	}
}

func TestOwnerGuardEndToEnd_BashUnresolvableExpansionFailsClosed(t *testing.T) {
	_, target := ownerGuardWorld(t)
	t.Setenv("DIR_UNDER_TARGET", "tasks") // set in the hook's env: still not an authority for the shell's
	rm := func(cmd string) (int, string) {
		got := runOwnerGuard(t, target, "Bash", map[string]string{"command": cmd})
		return got.ExitCode, got.Stderr
	}
	for _, cmd := range []string{
		"rm " + target + "/$DIR_UNDER_TARGET/1.md",
		`rm "` + target + `/${DIR_UNDER_TARGET}/1.md"`,
		"rm $(echo " + target + "/tasks/1.md)",
		"rm ~root/x",
		"D=tasks; rm " + target + "/$D/1.md",
	} {
		for _, agent := range []string{"other-agent", "lead"} {
			t.Setenv("CLAUDE_AGENT_NAME", agent)
			if code, err := rm(cmd); code != 2 || !strings.Contains(err, "cannot resolve") {
				t.Errorf("%s: %q = %d %q, want the cannot-resolve DENY", agent, cmd, code, err)
			}
		}
	}
}

func TestOwnerGuardEndToEnd_HardBlockHoldsForTheLeadThroughBashExpansion(t *testing.T) {
	_, target := ownerGuardWorld(t)
	t.Setenv("CLAUDE_AGENT_NAME", "lead")
	rm := func(cmd string) (int, string) {
		got := runOwnerGuard(t, target, "Bash", map[string]string{"command": cmd})
		return got.ExitCode, got.Stderr
	}
	if code, err := rm("rm " + target + "/secrets/k"); code != 2 || !strings.Contains(err, "HARD-BLOCK") {
		t.Fatalf("control (plain absolute, lead) = %d %q, want HARD-BLOCK", code, err)
	}
	if code, err := rm("rm " + target + "/docs/a.md"); code != 0 {
		t.Fatalf("control (lead, unowned file) = %d %q, want allow", code, err)
	}
	for _, cmd := range []string{
		"rm ~/Projects/targetproj/secrets/k",
		"rm $HOME/Projects/targetproj/secrets/k",
		`rm "$HOME/Projects/targetproj/secrets/k"`,
		`rm '` + target + `/secrets/k'`,
	} {
		if code, err := rm(cmd); code != 2 || !strings.Contains(err, "HARD-BLOCK Projects/targetproj/secrets/k") {
			t.Errorf("lead %q = %d %q, want HARD-BLOCK on the expanded path", cmd, code, err)
		}
	}
}

// Words that cannot be paths do not make a deletion unjudgeable; the same deletion with a
// real owned path in it still fires, for the lead's hard-block too. Each allowed row has a
// refused twin so the allow is not simply the guard being off.
func TestOwnerGuardEndToEnd_WordsThatCannotBePathsAreNotRefused(t *testing.T) {
	_, target := ownerGuardWorld(t)
	run := func(cmd string) (int, string) {
		got := runOwnerGuard(t, target, "Bash", map[string]string{"command": cmd})
		return got.ExitCode, got.Stderr
	}
	for _, cmd := range []string{
		"rm -f /tmp/leadkit-157-$$",
		"rm docs/a.md # remove $OLD",
		`rm docs/a.md > "$OUT"`,
		`rm docs/a.md 2>&1`,
		`git commit -m "fix: tidy; rm $TMP handled"`,
		`rm "a;b" docs/a.md`,
	} {
		if code, err := run(cmd); code != 0 {
			t.Errorf("%q = %d %q, want allow", cmd, code, err)
		}
	}
	for _, c := range []struct{ cmd, want string }{
		{`rm "a;b" tasks/1.md`, "owned by 'tasks-agent'"}, // a quoted `;` must not drop the argument after it
		{`rm docs/a.md 2>&1 tasks/1.md`, "owned by 'tasks-agent'"},
		{`rm tasks/1.md > "$OUT"`, "owned by 'tasks-agent'"},
		{`rm tasks/1.md # remove $OLD`, "owned by 'tasks-agent'"},
		{`echo "$(true; rm tasks/1.md)"`, "owned by 'tasks-agent'"},
		{`rm docs/a.md > out $X`, "cannot resolve"},
		{`rm -f /tmp/leadkit-157-$X`, "cannot resolve"},
	} {
		if code, err := run(c.cmd); code != 2 || !strings.Contains(err, c.want) {
			t.Errorf("%q = %d %q, want a DENY containing %q", c.cmd, code, err, c.want)
		}
	}
	t.Setenv("CLAUDE_AGENT_NAME", "lead")
	for _, cmd := range []string{`rm "a;b" secrets/k`, `rm secrets/k 2>&1`, `rm secrets/k # c`} {
		if code, err := run(cmd); code != 2 || !strings.Contains(err, "HARD-BLOCK") {
			t.Errorf("lead %q = %d %q, want HARD-BLOCK", cmd, code, err)
		}
	}
}

// $!, $? and $# can be forced to a value by whoever writes the command, so `rm .env$!` is
// really `rm .env`. Judging them as a digit let it through; they must fail closed for the
// lead (whose hard-block it would dodge) and for a non-owner (whose refusal it would dodge).
// The world owns EXACT names (.env, docs/pinned.md), so the digit-placeholder mutant allows them.
func TestOwnerGuardEndToEnd_ForceableSpecialsCannotDodgeAnExactNameMatch(t *testing.T) {
	_, target := ownerGuardWorld(t)
	run := func(agent, cmd string) (int, string) {
		t.Setenv("CLAUDE_AGENT_NAME", agent)
		got := runOwnerGuard(t, target, "Bash", map[string]string{"command": cmd})
		return got.ExitCode, got.Stderr
	}
	// Controls: the plain names fire, and a PID suffix (not forceable) does not.
	if code, err := run("lead", "rm .env"); code != 2 || !strings.Contains(err, "HARD-BLOCK") {
		t.Fatalf("control (lead, .env) = %d %q, want HARD-BLOCK", code, err)
	}
	if code, err := run("other-agent", "rm docs/pinned.md"); code != 2 || !strings.Contains(err, "owned by 'tasks-agent'") {
		t.Fatalf("control (non-owner, docs/pinned.md) = %d %q, want the ownership DENY", code, err)
	}
	if code, err := run("lead", "rm .env$$"); code != 0 {
		t.Errorf("lead rm .env$$ = %d %q, want allow (a PID suffix is not the exact name)", code, err)
	}
	for _, sp := range []string{"$!", "$?", "$#"} {
		for _, cmd := range []string{"rm .env" + sp, "rm docs/pinned.md" + sp, "rm tasks/157.md" + sp, "(exit 7); rm tasks/15" + sp + ".md"} {
			for _, agent := range []string{"lead", "other-agent"} {
				if code, err := run(agent, cmd); code != 2 || !strings.Contains(err, "cannot resolve") {
					t.Errorf("%s %q = %d %q, want the cannot-resolve DENY", agent, cmd, code, err)
				}
			}
		}
	}
}
