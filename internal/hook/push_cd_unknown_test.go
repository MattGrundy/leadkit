package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// One rule: a line that moves the shell somewhere the hook cannot pin down is UNKNOWN. The hook sees the text, the shell rewrites it, so a path is only known
// when the text is what cd will receive and nothing earlier could have changed what it names.

var pathUnknownCases = []struct {
	path    string
	assign  string // a variable the line assigns
	unknown bool
}{
	{"/repo", "", false},
	{"/repo/sub dir", "", false},
	{"~", "", false},
	{"~/work", "", false},
	{"$HOME/work", "", false}, // HOME the hook holds and the line does not assign
	{"$(echo /r)", "", true},
	{"`echo /r`", "", true},
	{"/r*", "", true},
	{"/r?", "", true},
	{"/r[a-z]", "", true},
	{"{/r,/s}", "", true},
	{"$'/r'", "", true},
	{`$"/r"`, "", true},
	{"~root/x", "", true},
	{"~-", "", true},
	{"~+", "", true},
	{"$R", "R", true},
	{"${R}/x", "R", true},
	{"$HOME", "HOME", true},
	{"~/x", "HOME", true},
	{"$PWD", "PWD", true},
	{"$R", "OTHER", false},
}

func TestPathUnknownWhy(t *testing.T) {
	for _, c := range pathUnknownCases {
		assigned := map[string]bool{}
		if c.assign != "" {
			assigned[c.assign] = true
		}
		if got := pathUnknownWhy(c.path, assigned) != ""; got != c.unknown {
			t.Errorf("pathUnknownWhy(%q, assigned=%q) unknown=%v, want %v", c.path, c.assign, got, c.unknown)
		}
	}
}

func TestAssignedVars_ReadsEveryWayALineSetsAVariable(t *testing.T) {
	for cmd, want := range map[string]string{
		"R=/r; cd $R":                 "R",
		"X=1 R=/r cd $R":              "R",
		"export R=/r && cd $R":        "R",
		"declare -x R=/r; cd $R":      "R",
		"cd /x && R+=/y; cd $R":       "R",
		"read R <<< /r; cd $R":        "R",
		"for R in /r; do cd $R; done": "R",
		"printf -v R /r; cd $R":       "R",
	} {
		if !assignedVars(cmd)[want] {
			t.Errorf("assignedVars(%q) misses %s", cmd, want)
		}
	}
	// Option values and config keys are not assignments to a shell variable.
	for _, cmd := range []string{"git -c user.name=x push origin b", "make --jobs=4", "git push --force-with-lease=b origin b"} {
		if got := assignedVars(cmd); len(got) != 0 {
			t.Errorf("assignedVars(%q) = %v, want none", cmd, got)
		}
	}
}

// The carry: a change of directory reaches a push over `&&` alone; over anything else the
// change may have failed, so the push is judged both where it went and where the session stands.
func TestParsePush_CarryOverLinks(t *testing.T) {
	cases := []struct {
		cmd   string
		repos []string // the repoPath of each invocation, in order
	}{
		{"cd /t && git push origin x", []string{"/t"}},
		{"cd /t && true && git push origin x", []string{"/t"}},
		{"cd /t && echo hi && git push origin x", []string{"/t"}},
		{"cd /t ; git push origin x", []string{"/t", ""}},
		{"cd /t; git push origin x", []string{"/t", ""}},
		{"cd /t\ngit push origin x", []string{"/t", ""}},
		{"cd /t || exit 1; git push origin x", []string{"/t", ""}},
		{"cd /t & git push origin x", []string{"/t", ""}},
		{"cd /t && echo hi ; git push origin x", []string{"/t", ""}},
		{"pushd /t && git push origin x", []string{"/t"}},
		{"builtin cd /t && git push origin x", []string{"/t"}},
		{"command cd /t && git push origin x", []string{"/t"}},
		{"X=1 cd /t && git push origin x", []string{"/t"}},
		{"(cd /t) && git push origin x", []string{""}},                            // the subshell ended
		{"cd /t && git -C /s push origin x", []string{"/s"}},                      // -C names the repo
		{"cd /t && git push origin a && git push origin b", []string{"/t", "/t"}}, // both run in /t
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
		if strings.Join(got, "|") != strings.Join(c.repos, "|") {
			t.Errorf("%q: invocation repos %q, want %q", c.cmd, got, c.repos)
		}
	}
}

func TestParsePush_UnnameableDirectoryChangeIsUnknown(t *testing.T) {
	for _, cmd := range []string{
		"popd && git push origin x",
		"pushd && git push origin x",
		"pushd +1 && git push origin x",
		"cd /t* && git push origin x",
		"cd {/t,/u} && git push origin x",
		"cd $'/t' && git push origin x",
		"git -C /t* push origin x",
		"git -C $'/t' push origin x",
		"cd ~root && git push origin x",
		"HOME=/t; cd && git push origin x",
		"HOME=/t; cd $HOME && git push origin x",
		"PWD=/t; cd $PWD && git push origin x",
		"export R=/t; cd $R && git push origin x",
		"R=/t; git -C $R push origin x",
		"bash -c 'cd /t* && git push origin x'",
		"bash -c 'HOME=/t; cd $HOME && git push origin x'",
	} {
		p := parsePushCommands(cmd)
		if p.State != pushUnsure || p.Unknown == "" {
			t.Errorf("%q: want pushUnsure with an Unknown reason, got %+v", cmd, p)
		}
	}
	// CONTROLS: nothing here is unknown.
	for _, cmd := range []string{"cd /t && git push origin x", "git -C /t push origin x", "cd $HOME/t && git push origin x", "R=/t; git push origin x"} {
		if p := parsePushCommands(cmd); p.State != pushFound {
			t.Errorf("%q: want pushFound, got %+v", cmd, p)
		}
	}
}

// looseEnv: the session sits in a gated repo, the target is out of scope. The two are told apart
// only by the repo each push is judged in.
func looseEnv(t *testing.T) (cfg PushGateConfig, other string) {
	t.Helper()
	t.Setenv("REVIEW_GATE_BYPASS", "")
	session, other := t.TempDir(), t.TempDir()
	records := t.TempDir()
	writeFullRecord(t, records, shaGated2)
	g := &gate.Gate{
		RecordsDir: records,
		Git: fakeGateGit{
			remotes: map[string]string{session: enforcedRemote, other: "git@github.com:other/unrelated.git"},
			head:    shaGated,
			refs:    map[string]string{"feat/ungated": shaUngated, "feat/gated": shaGated2},
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	return PushGateConfig{ResolveGate: staticGate(g), Cwd: session}, other
}

// Over `;` (or `||`, a newline, `&`) the cd may have failed and the push then runs where the
// session stands. The push gate carried a cd over `&&` alone and judged the rest by the session
// cwd, so a cd to a gated repo before a `;` was not seen.
func TestPushGate_CdOverANonAndLink_JudgesTheTargetAndTheSession(t *testing.T) {
	cfg, _, target := cdTargetEnv(t, false) // out-of-scope session, gated target
	for _, link := range []string{" ; ", "; ", ";", " || exit 1; ", "\n", " & ", " && true && ", " && echo hi ; "} {
		cmd := "cd " + target + link + "git push origin feat/ungated"
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
		ok := "cd " + target + link + "git push origin feat/gated"
		assertAllow(t, PushGate(bashInput(ok), cfg), ok)
	}
	for _, cmd := range []string{
		"pushd " + target + " && git push origin feat/ungated",
		"builtin cd " + target + " && git push origin feat/ungated",
		"command cd " + target + " && git push origin feat/ungated",
		"X=1 cd " + target + " && git push origin feat/ungated",
	} {
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
	}

	// The session half: session gated, target not. `;` may leave the push in the session.
	lcfg, other := looseEnv(t)
	assertAllow(t, PushGate(bashInput("cd "+other+" && git push origin feat/ungated"), lcfg), "over && the push runs in the out-of-scope target only")
	assertDeny(t, PushGate(bashInput("cd "+other+" ; git push origin feat/ungated"), lcfg), "over ; the cd may fail and the push run in the gated session")
	assertAllow(t, PushGate(bashInput("(cd "+other+") && git push origin feat/gated"), lcfg), "the subshell's cd does not carry: judged in the session (gated sha)")
	assertDeny(t, PushGate(bashInput("(cd "+other+") && git push origin feat/ungated"), lcfg), "the subshell's cd does not carry: judged in the gated session")
}

// A shell-rewritten path is unknown, from both kinds of session, in both modes, and the bypass
// lets it through. The plain nonexistent path stays allowed (cd_target_test.go).
func TestPushGate_ShellRewrittenOrAssignedPath_IsUnknown(t *testing.T) {
	cmds := func(target string) []string {
		return []string{
			"cd " + target + "* && git push origin feat/ungated",
			"cd " + target + "? && git push origin feat/ungated",
			"git -C " + target + "* push origin feat/ungated",
			"cd {" + target + ",/nonexistent} && git push origin feat/ungated",
			"cd $'" + target + "' && git push origin feat/ungated",
			"git -C $'" + target + "' push origin feat/ungated",
			"cd " + target + "/../targe[t] && git push origin feat/ungated",
			"cd ~root/../../" + target + " && git push origin feat/ungated",
			"HOME=" + target + "; cd $HOME && git push origin feat/ungated",
			"HOME=" + target + "; git -C $HOME push origin feat/ungated",
			"PWD=" + target + "; cd $PWD && git push origin feat/ungated",
			"export R=" + target + " && cd $R && git push origin feat/ungated",
			"bash -c 'cd " + target + "* && git push origin feat/ungated'",
			"bash -c 'HOME=" + target + "; cd $HOME && git push origin feat/ungated'",
			"popd && git push origin feat/ungated",
		}
	}
	for _, sessionGated := range []bool{true, false} {
		for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
			cfg, _, target := cdTargetEnv(t, sessionGated)
			cfg.Mode = func(string) GuardMode { return mode }
			for _, cmd := range cmds(target) {
				assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
			}
			t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
			for _, cmd := range cmds(target) {
				assertAllow(t, PushGate(bashInput(cmd), cfg), "bypassed: "+cmd)
			}
			t.Setenv("REVIEW_GATE_BYPASS", "")
		}
	}
}

// A directory an earlier command on the line may create is not a path that cannot be entered.
func TestPushGate_DirectoryAnEarlierCommandMayCreate_IsUnknown(t *testing.T) {
	cfg, _, _ := cdTargetEnv(t, false)
	for _, cmd := range []string{
		"mkdir /no/such/dir/169 && cd /no/such/dir/169 && git push origin feat/ungated",
		"cp -r /a /no/such/dir/169 && cd /no/such/dir/169 && git push origin feat/ungated",
		"git clone u /no/such/dir/169 && git -C /no/such/dir/169 push origin feat/ungated",
		"git worktree add /no/such/dir/169 && cd /no/such/dir/169 && git push origin feat/ungated",
		"true && cd /no/such/dir/169 && git push origin feat/ungated",
	} {
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
	}
	// CONTROL: the same plain nonexistent path as the first command is a failed cd.
	for _, cmd := range []string{
		"cd /no/such/dir/169 && git push origin feat/ungated",
		"git -C /no/such/dir/169 push origin feat/ungated",
	} {
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
	}
}
