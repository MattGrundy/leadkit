package hook

import "testing"

// A cd with an option, or a quoted or computed path, must still name the repo the raw PR verb
// runs in: judged instead by the session's own directory, the grant the session's repo holds
// would authorise the verb in the repo the cd moved to.
func TestPRGuard_CdWithOptionsOrComputedPath_IsJudgedByTheRepoItMovesTo(t *testing.T) {
	const granted, victim = "/work/granted", "/work/victim"
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) {
		switch repoPath {
		case granted:
			return "git@github.com:org/another-project.git", true // holds the raw-create grant
		case victim:
			return "git@github.com:org/victim.git", true // holds none
		}
		return "", false
	}
	t.Cleanup(func() { runGit = orig })
	run := func(cmd string) Result {
		return PRGuard(Input{ToolInput: toolInput{Command: cmd}}, teamsBridgeProfile(), granted)
	}
	const verb = " && gh pr create --draft --title x"

	// CONTROLS: standing in the granted repo, or cd-ing to it by any spelling, is allowed.
	// Without them a guard that refused every cd would pass the cases below.
	for _, cmd := range []string{
		"gh pr create --draft --title x",
		"cd " + granted + verb,
		"cd -P " + granted + verb,
		"cd -- " + granted + verb,
		`cd "` + granted + `"` + verb,
	} {
		if res := run(cmd); res != allow {
			t.Errorf("%q = %+v, want allow (the target is the granted repo)", cmd, res)
		}
	}
	for _, cmd := range []string{
		"cd " + victim + verb,
		"cd -- " + victim + verb,
		"cd -P " + victim + verb,
		"cd -L -- " + victim + verb,
		`cd "` + victim + `"` + verb,
		// the target cannot be named at all
		`cd "$(echo ` + victim + `)"` + verb,
		"cd $(echo " + victim + ")" + verb,
		"cd $R" + verb,
		"cd -" + verb,
		// a cd that shares its line with other commands is not read
		"cd " + victim + " ; gh pr create --draft --title x",
		"cd " + victim + " || exit 1 ; gh pr create --draft --title x",
	} {
		if reason := decodeDeny(t, run(cmd)); reason == "" {
			t.Errorf("%q was allowed: the verb must be judged by the repo the cd moves to, or refused when that repo cannot be named", cmd)
		}
	}

	// A cd in a closed subshell moves nothing for the verb after it. From the victim's own
	// directory, reading it as a carried cd to the granted repo would borrow that grant.
	for _, cmd := range []string{
		"(cd " + granted + ")" + verb,
		"{ cd " + granted + "; }" + verb,
		// a cd that may have failed leaves the verb where the session stands, here the victim's
		"cd " + granted + " ; " + verb[len(" && "):],
		"cd " + granted + " || exit 1; " + verb[len(" && "):],
		"cd " + granted + " & " + verb[len(" && "):],
		// directory changes the guard does not read the target of
		"pushd " + granted + verb,
		"builtin cd " + granted + verb,
		"command cd " + granted + verb,
		"X=1 cd " + granted + verb,
	} {
		res := PRGuard(Input{ToolInput: toolInput{Command: cmd}}, teamsBridgeProfile(), victim)
		if reason := decodeDeny(t, res); reason == "" {
			t.Errorf("%q from the victim's directory was allowed on the strength of a cd that does not carry", cmd)
		}
	}
}
