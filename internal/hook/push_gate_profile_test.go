package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// Ticket 204: push-gate's three states for "is this repo governed?". The hook fires on every
// push on the machine, so it must fail OPEN for a repo that has nothing to do with leadkit;
// but an out-of-scope verdict is an exemption only if a profile was READ to reach it, so a
// repo whose profile could not be read is REFUSED. Each test drives hook.Dispatch.

// pushShapes are the three ways pushGateOne reaches a verdict: a bare push (judged on HEAD),
// a push naming a ref (judged on the commit it resolves to) and one whose refs cannot be named.
var pushShapes = []string{"git push", "git push origin main", "git push --all"}

const profileProblemWhy = "the profile /home/u/proj/leadkit.yaml was found but was not accepted: config: parse project config: yaml: line 1"

// dispatchPush runs a Bash command through Dispatch as the real binary does.
func dispatchPush(t *testing.T, cfg PushGateConfig, command string) Result {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	return Dispatch(NamePushGate, payload, Config{PushGate: cfg})
}

// outOfScopeEnv is a gate that does not govern repo (its origin is not enforced), with the
// profile-problem seam set to problem and recording every repo it is asked about. "main"
// resolves, so a push that names it is judged on a commit, not left undetermined.
func outOfScopeEnv(t *testing.T, problem string, asked *[]string) PushGateConfig {
	t.Helper()
	repo := t.TempDir()
	g := &gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{repo: "git@github.com:someorg/random.git"}, head: gateTestSHA, refs: map[string]string{"main": gateTestSHA}},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	return PushGateConfig{
		ResolveGate: staticGate(g),
		Cwd:         repo,
		ProfileProblem: func(r string) string {
			if asked != nil {
				*asked = append(*asked, r)
			}
			return problem
		},
	}
}

// State 1: not a leadkit project. Silent allow.
func TestPushGateProfile_NotALeadkitProject_AllowsSilently(t *testing.T) {
	cfg := outOfScopeEnv(t, "", nil)
	for _, cmd := range pushShapes {
		assertAllow(t, dispatchPush(t, cfg, cmd), "no sign of leadkit: "+cmd)
	}

	cfg.ProfileProblem = nil // an adopter's wiring that never set the seam
	assertAllow(t, dispatchPush(t, cfg, "git push"), "no seam wired")
}

// State 2: evidently a leadkit project whose profile could not be read. Refused, and the
// refusal carries what the operator needs: the path searched and the HOME.
func TestPushGateProfile_UnreadableProfile_RefusesAndNamesWhy(t *testing.T) {
	why := profileProblemWhy + `; HOME="/home/u"`
	cfg := outOfScopeEnv(t, why, nil)
	for _, cmd := range pushShapes {
		res := dispatchPush(t, cfg, cmd)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "/home/u/proj/leadkit.yaml") || !strings.Contains(res.Stdout, `HOME=`) {
			t.Errorf("%s: the refusal must name the path searched and the HOME, got:\n%s", cmd, res.Stdout)
		}
		if !strings.Contains(res.Stdout, "REVIEW_GATE_BYPASS") {
			t.Errorf("%s: the refusal must say how to override, got:\n%s", cmd, res.Stdout)
		}
	}
}

// The refusal is not an advisory-mode softness: it holds in both modes.
func TestPushGateProfile_UnreadableProfile_RefusesInEveryMode(t *testing.T) {
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		cfg := outOfScopeEnv(t, profileProblemWhy, nil)
		cfg.Mode = func(string) GuardMode { return mode }
		for _, cmd := range pushShapes {
			assertDeny(t, dispatchPush(t, cfg, cmd), cmd+" in mode "+mode.String())
		}
	}
}

// State 3: a profile was read and the repo is genuinely out of scope. Allowed, as before.
// (Same gate and same out-of-scope verdict as state 2; only the seam's answer differs.)
func TestPushGateProfile_ProfileReadRepoOutOfScope_Allows(t *testing.T) {
	var asked []string
	cfg := outOfScopeEnv(t, "", &asked)
	assertAllow(t, dispatchPush(t, cfg, "git push origin main"), "profile read, out of scope")
	if len(asked) != 1 {
		t.Errorf("the seam was asked %d times, want once for the one out-of-scope verdict", len(asked))
	}
}

func TestPushGateProfile_BypassStillOverrides(t *testing.T) {
	cfg := outOfScopeEnv(t, profileProblemWhy, nil)
	t.Setenv("REVIEW_GATE_BYPASS", "deliberate")
	assertAllow(t, dispatchPush(t, cfg, "git push"), "BYPASS env")

	t.Setenv("REVIEW_GATE_BYPASS", "")
	assertAllow(t, dispatchPush(t, cfg, `REVIEW_GATE_BYPASS="deliberate" git push`), "inline BYPASS")
	assertDeny(t, dispatchPush(t, cfg, `echo hi; REVIEW_GATE_BYPASS="deliberate" git push`), "inline bypass on a later command covers nothing")
}

// A push the gate judges on its record is not second-guessed: the seam only decides what an
// out-of-scope verdict means, and an in-scope repo's verdict is its record's.
func TestPushGateProfile_GatedAndUngatedVerdictsAreUnchanged(t *testing.T) {
	repo := t.TempDir()
	cfg, recordsDir := pushGateEnv(t, repo, enforcedRemote) // in scope
	var asked []string
	cfg.ProfileProblem = func(r string) string { asked = append(asked, r); return profileProblemWhy }

	res := dispatchPush(t, cfg, "git push")
	assertDeny(t, res, "in scope, no record")
	if !strings.Contains(res.Stdout, "no review record") || strings.Contains(res.Stdout, "cannot be known") {
		t.Errorf("an in-scope ungated push must be refused for its record, not the profile:\n%s", res.Stdout)
	}
	writeFullRecord(t, recordsDir, gateTestSHA)
	assertAllow(t, dispatchPush(t, cfg, "git push"), "in scope, gated")
	if len(asked) != 0 {
		t.Errorf("the seam was asked for an in-scope repo: %v", asked)
	}
}

// A command the parser cannot read may hide a push; in an unreadable-profile repo it is
// refused like the push it may be, and in a repo with no sign of leadkit it is allowed.
func TestPushGateProfile_UnreadableCommandFollowsTheSameRule(t *testing.T) {
	unsure := `bash -c "git push"`
	assertDeny(t, dispatchPush(t, outOfScopeEnv(t, profileProblemWhy, nil), unsure), "unreadable command, unreadable profile")
	assertAllow(t, dispatchPush(t, outOfScopeEnv(t, "", nil), unsure), "unreadable command, no sign of leadkit")
}

// The gate primitive the hook uses: bypass lets it through, nothing else does.
func TestGateCheckUnreadableProfile(t *testing.T) {
	g := &gate.Gate{RecordsDir: t.TempDir()}
	t.Setenv("REVIEW_GATE_BYPASS", "")
	if ok, outcome, reason := g.CheckUnreadableProfile("because"); ok || outcome != gate.OutcomeUngated || reason != "because" {
		t.Errorf("want a refusal carrying the reason, got %v %v %q", ok, outcome, reason)
	}
	t.Setenv("REVIEW_GATE_BYPASS", "x")
	if ok, outcome, _ := g.CheckUnreadableProfile("because"); !ok || outcome != gate.OutcomeGated {
		t.Errorf("want the bypass honoured, got %v %v", ok, outcome)
	}
}
