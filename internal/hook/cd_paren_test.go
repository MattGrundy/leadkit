package hook

import "testing"

// A directory change survives an unrelated `( ... )` group that follows it: only the `)` that
// closes the group the change was made in ends it. Treating any `)` as the end hid the change, and
// the command was judged by the session's own directory.

func TestPRGuard_ACdSurvivesAnUnrelatedGroupAfterIt(t *testing.T) {
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

	// The session holds the grant; the victim does not. Each cd is outside the group that follows.
	for _, cmd := range []string{
		"cd " + victim + " && (true) && " + verb,
		"cd " + victim + " && (echo hi) && " + verb,
		"cd " + victim + " && ( true ) && " + verb,
		"cd " + victim + " && (true); " + verb,
		"cd " + victim + " && (cd " + granted + ") && " + verb, // the inner cd ends at its own ), and the outer one is back
		"cd " + victim + " && (true) && (true) && " + verb,
		"cd " + victim + " && ((true)) && " + verb,
		"cd " + victim + " && true && " + verb, // no group at all: the control that always held
		// An empty operand is a cd that changes nothing, so the directory in force is still the victim's.
		"cd " + victim + " && cd '' && " + verb,
		"cd " + victim + ` && cd "" && ` + verb,
		"cd " + victim + " && cd -- '' && " + verb,
		"cd " + victim + " && cd -P '' && " + verb,
		"cd " + victim + " && (cd '') && " + verb,
		// pushd with an empty operand is left unread: the shells differ on it (the ones tried here leave
		// the directory unchanged, and none that differ were available to check), so it names no directory.
		"cd " + victim + " && pushd '' && " + verb,
		// A `;` anywhere after a cd loosens every change in force, including the outer one, though
		// a `;` inside a later group cannot undo an outer cd that already succeeded. Refusing is
		// deliberate: the guard does not model which `;` belongs to which group.
		"cd " + granted + " && ( cd " + victim + " ; true ) && " + verb,
		"(true) && cd " + victim + " && " + verb,
	} {
		if reason := decodeDeny(t, run(granted, cmd)); reason == "" {
			t.Errorf("%q was allowed: the cd outside the group is still in force, and names a repo with no grant", cmd)
		}
	}

	// A cd made INSIDE a group ends at that group's `)`, so the verb after it runs where the
	// session stands. From the victim's own directory (no grant) that is refused; from the granted
	// repo it is allowed. These two are the same line judged by two different sessions.
	inside := "( cd " + granted + " && true ) && " + verb
	if reason := decodeDeny(t, run(victim, inside)); reason == "" {
		t.Errorf("%q from the victim's directory: the subshell's cd does not reach the verb, so the session's own (ungranted) directory decides", inside)
	}
	insideVictim := "( cd " + victim + " && true ) && " + verb
	if res := run(granted, insideVictim); res != allow {
		t.Errorf("%q from the granted repo: the subshell's cd does not reach the verb, so the session's directory decides; want allow, got %+v", insideVictim, res)
	}

	// CONTROL: an empty cd with no cd before it changes nothing, so the session's directory decides.
	if res := run(granted, "cd '' && "+verb); res != allow {
		t.Errorf("an empty cd alone leaves the session's directory in force; from the granted repo want allow, got %+v", res)
	}
	if reason := decodeDeny(t, run(victim, "cd '' && "+verb)); reason == "" {
		t.Errorf("an empty cd alone from the victim's directory: the session's own (ungranted) directory decides, want deny")
	}

	// A cd inside the same group as the verb reaches it.
	for cmd, wantAllow := range map[string]bool{
		"( cd " + granted + " && " + verb + " )":                      true,
		"( cd " + victim + " && " + verb + " )":                       false,
		"cd " + victim + " && ( cd " + granted + " && " + verb + " )": true,  // the inner change is the one in force
		"cd " + granted + " && ( cd " + victim + " && " + verb + " )": false, // ...whichever it names
	} {
		res := run(granted, cmd)
		if wantAllow && res != allow {
			t.Errorf("%q: want allow, got %+v", cmd, res)
		}
		if !wantAllow && decodeDeny(t, res) == "" {
			t.Errorf("%q: want deny", cmd)
		}
	}
}

func TestParsePush_ACdSurvivesAnUnrelatedGroupAfterIt(t *testing.T) {
	cases := []struct {
		cmd   string
		repos []string // the repoPath of each invocation, in order
	}{
		{"cd /v && git push origin x", []string{"/v"}},
		{"cd /v && (true) && git push origin x", []string{"/v"}},
		{"cd /v && cd '' && git push origin x", []string{"/v"}},      // an empty operand changes nothing
		{"cd /v && cd -- \"\" && git push origin x", []string{"/v"}}, // ...in either spelling
		{"cd '' && git push origin x", []string{""}},                 // with no cd before it, nothing is carried
		{"cd /v && ( true ) && git push origin x", []string{"/v"}},
		{"cd /v && (echo hi) && git push origin x", []string{"/v"}},
		{"cd /v && (true) && (true) && git push origin x", []string{"/v"}},
		{"cd /v && (true); git push origin x", []string{"/v", ""}}, // over `;` the cd may have failed: both
		{"cd /v && (cd /w) && git push origin x", []string{"/v"}},  // the inner cd ends at its own )
		{"(cd /v) && git push origin x", []string{""}},             // a cd inside a closed group reaches nothing
		{"( cd /v && true ) && git push origin x", []string{""}},
		{"( cd /v && git push origin x )", []string{"/v"}},
		{"cd /w && ( cd /v && git push origin x )", []string{"/v"}},
	}
	for _, c := range cases {
		p := parsePushCommands(c.cmd)
		if p.State != pushFound {
			t.Errorf("%q: want pushFound, got %+v", c.cmd, p)
			continue
		}
		var got []string
		for _, inv := range p.Invocations {
			got = append(got, inv.repoPath)
		}
		if len(got) != len(c.repos) {
			t.Errorf("%q: invocation repos %q, want %q", c.cmd, got, c.repos)
			continue
		}
		for i := range got {
			if got[i] != c.repos[i] {
				t.Errorf("%q: invocation repos %q, want %q", c.cmd, got, c.repos)
				break
			}
		}
	}
}

// pushd with an empty operand names no directory: after a cd it must not resolve to the session's.
func TestParsePush_PushdWithAnEmptyOperandIsUnknown(t *testing.T) {
	p := parsePushCommands("cd /v && pushd '' && git push origin x")
	if p.State != pushUnsure || p.Unknown == "" {
		t.Errorf("want pushUnsure with an Unknown reason, got %+v", p)
	}
}
