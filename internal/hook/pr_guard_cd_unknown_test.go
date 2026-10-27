package hook

import "testing"

// A directory change that reaches the raw verb names the repo it runs in; one the guard cannot read
// is a repo it cannot name, so the verb is refused rather than judged by the session's own
// directory (whose grant it has not earned).
func TestPRGuard_AnyOtherDirectoryChangeBeforeTheVerbIsRefused(t *testing.T) {
	const granted, victim = "/work/granted", "/work/victim"
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) {
		switch repoPath {
		case granted, granted + "*", "$R":
			// The pattern and the variable are stubbed as resolving to the granted repo, as a
			// lookup of the literal text could: the shell may expand them to another repo.
			return "git@github.com:org/another-project.git", true
		case victim:
			return "git@github.com:org/victim.git", true
		}
		return "", false
	}
	t.Cleanup(func() { runGit = orig })
	run := func(cmd string) Result {
		return PRGuard(Input{ToolInput: toolInput{Command: cmd}}, teamsBridgeProfile(), granted)
	}
	const verb = "gh pr create --draft --title x"

	for _, cmd := range []string{
		"true && cd " + victim + " && " + verb,
		"echo hi; cd " + victim + " && " + verb,
		"X=1 cd " + victim + " && " + verb,
		"R=" + victim + "; cd $R && " + verb,
		"HOME=" + victim + "; cd $HOME && " + verb,
		"pushd " + victim + " && " + verb,
		"builtin cd " + victim + " && " + verb,
		"command cd " + victim + " && " + verb,
		"cd " + victim + "* && " + verb,
		"cd $'" + victim + "' && " + verb,
		"cd " + victim + " ; " + verb,
		"cd " + victim + " || exit 1; " + verb,
		"( cd " + victim + " && " + verb + " )",
		"popd && " + verb,
		// an earlier gh or glab command is not the verb: a cd after it still moves the verb
		"gh auth status && cd " + victim + " && " + verb,
		"gh pr list; cd " + victim + " && " + verb,
		"glab auth status && cd " + victim + " && " + verb,
		"gh repo view >/dev/null && cd " + victim + " && " + verb,
		"gh auth status && cd -P " + victim + " && " + verb,
		// a segment that MENTIONS the verb runs nothing and is no anchor: a decoy must not hide the real one
		"echo gh pr create && cd " + victim + " && " + verb,
		`echo "gh pr create" && cd ` + victim + " && " + verb,
		`git commit -m "gh pr create" && cd ` + victim + " && " + verb,
		"echo glab mr create; cd " + victim + " && " + verb,
		// two verbs are read only if they run in the same place
		verb + " && cd " + victim + " && " + verb,
		"cd " + granted + " && " + verb + " && cd " + victim + " && " + verb,
		// a wrapper hides which command runs the verb: with a cd in force that is unknown
		"cd " + victim + " && env " + verb,
		"cd " + granted + " && xargs " + verb,
		"cd " + granted + "* && " + verb,
		"R=" + victim + "; cd $R && " + verb,
	} {
		if reason := decodeDeny(t, run(cmd)); reason == "" {
			t.Errorf("%q was allowed on the strength of the session's grant, though it moves the shell elsewhere", cmd)
		}
	}

	// CONTROLS. A cd that reaches the verb over && names the repo whatever precedes it, a redirect
	// on a cd is dropped, and a cd after the verb's own segment cannot move it; the granted repo
	// stays usable.
	for _, cmd := range []string{
		verb,
		"gh auth status && cd " + granted + " && " + verb, // an earlier gh moves nothing, and this cd is to the granted repo
		"gh auth status && " + verb,
		"echo gh pr create && cd " + granted + " && " + verb, // a mention is not the verb, and this cd names the granted repo
		"cd " + granted + " && " + verb + " && " + verb,      // two verbs, one place
		"true && cd " + granted + " && " + verb,              // a cd after another command reaches the verb over &&, and names the granted repo
		"cd " + granted + " && " + verb,
		"cd " + victim + " && cd " + granted + " && " + verb,
		"cd " + granted + " 2>/dev/null && " + verb,
		verb + " && cd /tmp",
		verb + " ; cd " + victim,
	} {
		if res := run(cmd); res != allow {
			t.Errorf("%q = %+v, want allow", cmd, res)
		}
	}
}

// Where no segment carries the verb, a directory change in force cannot be shown irrelevant to it.
func TestCdPrefix_NoVerbSegmentWithACdInForceIsUnknown(t *testing.T) {
	if _, ok, unknown := cdPrefix("cd /x && echo y"); ok || unknown == "" {
		t.Errorf("a cd in force with no verb segment must be unknown, got ok=%v unknown=%q", ok, unknown)
	}
	if repo, ok, unknown := cdPrefix("echo y"); repo != "" || ok || unknown != "" {
		t.Errorf("no cd, no verb: nothing to say, got %q %v %q", repo, ok, unknown)
	}
	if repo, ok, unknown := cdPrefix("cd /x && gh pr create"); repo != "/x" || !ok || unknown != "" {
		t.Errorf("a cd over && to the verb names the repo, got %q %v %q", repo, ok, unknown)
	}
}
