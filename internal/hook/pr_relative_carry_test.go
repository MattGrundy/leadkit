package hook

import "testing"

// Ticket 215, the raw-create-verb side. A relative operand on a second `cd` moves from the directory the
// first one carried, not from the session's, so `cd V && cd . && C` runs C in V. Resolving the `.` against
// the session judged C by the session's own grant.
//
// The PLAIN twin, `cd V && C`, has always been judged by V: these spellings only had to reach the same place.

func TestPRGuard_ARelativeCdMovesFromTheDirectoryInForce(t *testing.T) {
	const granted, victim = "/work/granted", "/work/victim"
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) {
		switch repoPath {
		case granted:
			return "git@github.com:org/another-project.git", true
		case victim:
			return "git@github.com:org/victim.git", true
		}
		return "", false
	}
	t.Cleanup(func() { runGit = orig })
	const verb = "gh pr create --draft --title x"
	run := func(cwd, cmd string) Result {
		return PRGuard(Input{ToolInput: toolInput{Command: cmd}}, teamsBridgeProfile(), cwd)
	}

	// The session holds the grant; the victim does not. Each line ends in the victim's directory.
	for _, cmd := range []string{
		"cd " + victim + " && " + verb, // the plain twin, judged by the victim since before this ticket
		"cd " + victim + " && cd . && " + verb,
		"cd " + victim + " && cd ./ && " + verb,
		"cd " + victim + " && cd -L . && " + verb,
		"cd " + victim + " && cd -- . && " + verb,
		"cd " + victim + " && cd -P -- . && " + verb,
		"cd " + victim + " && cd '.' && " + verb,
		`cd ` + victim + ` && cd "." && ` + verb,
		"cd " + victim + " && ( cd . && " + verb + " )",
		"cd " + victim + "/sub && cd .. && " + verb,
		"cd " + victim + "/sub/deeper && cd ../.. && " + verb,
		"cd /work && cd victim && " + verb,
		"cd /work && cd victim && cd . && " + verb,
		"cd " + granted + " && cd ../victim && " + verb,
	} {
		if reason := decodeDeny(t, run(granted, cmd)); reason == "" {
			t.Errorf("%q was allowed from the granted session: the relative cd lands in the victim's repo, which has no grant", cmd)
		}
	}

	// A relative operand that lands in the GRANTED repo still allows: the refusals above are the
	// victim's, not a refusal of every relative operand.
	for _, cmd := range []string{
		"cd " + granted + " && cd . && " + verb,
		"cd " + granted + "/sub && cd .. && " + verb,
		"cd " + victim + " && cd ../granted && " + verb,
		"cd /work && cd granted && " + verb,
	} {
		if res := run(victim, cmd); res != allow {
			t.Errorf("%q from the victim's directory: the carried cd lands in the granted repo, want allow, got %+v", cmd, res)
		}
	}

	// A carry that is not named, or may not have happened, makes the relative cd unknown.
	for _, cmd := range []string{
		"cd $(mktemp -d) && cd . && " + verb,
		"cd " + granted + " ; cd . && " + verb,
		"cd - && cd . && " + verb,
		"cd " + granted + " || cd . && " + verb,
	} {
		if reason := decodeDeny(t, run(granted, cmd)); reason == "" {
			t.Errorf("%q was allowed: a relative cd after a change the guard cannot name must be unknown", cmd)
		}
	}
}
