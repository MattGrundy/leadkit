package hook

import (
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// perRepoGateGit is a gate.Git that serves a DIFFERENT HEAD per repo path, so a test can
// prove push-gate judges a `cd <other-clone> && git push` against the OTHER clone's HEAD,
// not the session cwd's.
type perRepoGateGit struct {
	heads   map[string]string // repo path -> HEAD sha
	remotes map[string]string // repo path -> origin url
}

func (f perRepoGateGit) RevParse(repo, ref string) (string, bool) {
	if ref == "HEAD" {
		if s, ok := f.heads[repo]; ok && s != "" {
			return s, true
		}
		return "", false
	}
	return ref, true // an already-full sha normalizes to itself
}

func (f perRepoGateGit) RemoteURL(repo string) string { return f.remotes[repo] }

// TestPushGate_CdOtherClone_JudgedAgainstOtherCloneHEAD is the Part-2 sha-resolution proof:
// a session whose cwd is clone-A runs `cd <clone-B> && git push`. Clone-A's HEAD is gated
// (a record exists); clone-B's HEAD is NOT. The push must be judged against clone-B's HEAD
// (→ DENY) — which only happens if push-gate resolved the target clone's OWN HEAD, not the
// session cwd's. A plain `git push` from clone-A (its gated HEAD) still ALLOWS.
func TestPushGate_CdOtherClone_JudgedAgainstOtherCloneHEAD(t *testing.T) {
	cloneA := t.TempDir() // the session cwd — its HEAD is gated
	cloneB := t.TempDir() // the cd target — its HEAD is ungated
	headA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	recordsDir := t.TempDir()
	writeFullRecord(t, recordsDir, headA) // ONLY clone-A's HEAD is gated

	g := &gate.Gate{
		RecordsDir: recordsDir,
		Git: perRepoGateGit{
			heads:   map[string]string{cloneA: headA, cloneB: headB},
			remotes: map[string]string{cloneA: enforcedRemote, cloneB: enforcedRemote},
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{ResolveGate: staticGate(g), Cwd: cloneA}

	// `cd clone-B && git push` → judged against clone-B's (ungated) HEAD → DENY.
	res := PushGate(bashInput("cd "+cloneB+" && git push"), cfg)
	assertDeny(t, res, "cd other-clone push judged against the OTHER clone's HEAD")

	// A plain `git push` from the session cwd (clone-A) → its gated HEAD → ALLOW.
	assertAllow(t, PushGate(bashInput("git push"), cfg), "plain push from clone-A's gated HEAD")
}

// TestPushGate_LeadkitPush_Allowed is the Part-2 self-recognition proof: a `leadkit push …`
// Bash command is ALLOWED by the hook without re-deriving — the primitive gates in-process,
// so double-blocking it would be wrong. Proven against a cfg that would DENY a real git push
// (enforced repo, no record).
func TestPushGate_LeadkitPush_Allowed(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote) // in scope, no record → a real git push DENIES

	// Control: a real `git push` in this repo denies.
	assertDeny(t, PushGate(bashInput("git push"), cfg), "control: ungated git push denies")

	for _, cmd := range []string{
		"leadkit push --repo " + repo + " --branch feat/x --sha " + gateTestSHA,
		"REVIEW_GATE_BYPASS=x leadkit push --repo " + repo + " --branch feat/x",
		"cd " + repo + " && leadkit push --repo " + repo + " --branch feat/x",
	} {
		assertAllow(t, PushGate(bashInput(cmd), cfg), "leadkit push allowed: "+cmd)
	}

	// A co-present REAL git push is NOT whitewashed by a trailing leadkit push — the raw
	// push is still judged (and here denied).
	assertDeny(t, PushGate(bashInput("git push && leadkit push --repo "+repo+" --branch feat/x"), cfg),
		"a real git push co-present with leadkit push is still judged")
}

// TestPushGate_InlineBypass_Reachable is the Part-3 proof: the INLINE
// `REVIEW_GATE_BYPASS="reason" git push` form the deny message advertises is honored by the
// hook (allow), even though the hook runs in its own env before the pushed subprocess.
func TestPushGate_InlineBypass_Reachable(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote) // enforced + no record → normally DENY

	// Control: without the inline assignment it denies.
	assertDeny(t, PushGate(bashInput("git push"), cfg), "control: ungated push denies")

	// Inline bypass (simple, quoted-with-space, and among multiple assignments) → allow.
	for _, cmd := range []string{
		`REVIEW_GATE_BYPASS=hotfix git push`,
		`REVIEW_GATE_BYPASS="hotfix in progress" git push origin master`,
		`FOO=bar REVIEW_GATE_BYPASS='urgent fix' git -C ` + repo + ` push`,
	} {
		assertAllow(t, PushGate(bashInput(cmd), cfg), "inline bypass honored: "+cmd)
	}
}

// TestPushGate_AssignmentAfterCommandWord_NotBypass proves an assignment that is NOT a
// leading one (it appears AFTER the command word, e.g. as a push arg) does NOT bypass.
func TestPushGate_AssignmentAfterCommandWord_NotBypass(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote) // enforced + no record → DENY

	// `REVIEW_GATE_BYPASS=x` here is a push ARGUMENT, not a leading env assignment → no bypass.
	assertDeny(t, PushGate(bashInput("git push origin REVIEW_GATE_BYPASS=x"), cfg),
		"an assignment after the command word is not a leading bypass")
	// An empty inline value is not a bypass either.
	assertDeny(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="" git push`), cfg),
		"an empty inline bypass value is not a bypass")
}

// TestLeadingAssignments_ShellWordCorrect is a direct unit test for the leading-assignment
// parser — quoted values (with spaces the coarse lexShell would split), multiple
// assignments, concatenation, and stopping at the command word.
func TestLeadingAssignments_ShellWordCorrect(t *testing.T) {
	cases := []struct {
		cmd  string
		want map[string]string
	}{
		{`REVIEW_GATE_BYPASS=x git push`, map[string]string{"REVIEW_GATE_BYPASS": "x"}},
		{`REVIEW_GATE_BYPASS="a reason" git push`, map[string]string{"REVIEW_GATE_BYPASS": "a reason"}},
		{`FOO=bar REVIEW_GATE_BYPASS='b c' git push`, map[string]string{"FOO": "bar", "REVIEW_GATE_BYPASS": "b c"}},
		{`A="x"y'z' git push`, map[string]string{"A": "xyz"}},
		{`git push`, map[string]string{}},
		{`git push FOO=bar`, map[string]string{}}, // after the command word — not leading
	}
	for _, c := range cases {
		got := leadingAssignments(c.cmd)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.cmd, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%q: [%s] = %q, want %q", c.cmd, k, got[k], v)
			}
		}
	}
}

func TestCommandIsLeadkitPush(t *testing.T) {
	yes := []string{
		"leadkit push --repo /x --branch b",
		"FOO=bar leadkit push --repo /x",
		"cd /x && leadkit push --repo /x",
		"git status && leadkit push --repo /x",
	}
	no := []string{
		"git push",
		"leadkit status",
		"leadkitpush",           // not a word boundary
		"echo leadkit push",     // not the command word
		"git -C /x push origin", // a real git push
	}
	for _, c := range yes {
		if !commandIsLeadkitPush(c) {
			t.Errorf("commandIsLeadkitPush(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if commandIsLeadkitPush(c) {
			t.Errorf("commandIsLeadkitPush(%q) = true, want false", c)
		}
	}
}
