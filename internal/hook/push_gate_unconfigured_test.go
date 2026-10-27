package hook

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// Ticket 302: a .leadkit/ directory with no profile anywhere is its own state, UNCONFIGURED. It
// is not refused (that blocked a live CVE patch), and it is not silently allowed either: the
// allow carries the state on stderr, on every decision. These tests are written against the
// mutant that makes the state a plain allow, and the one that makes it a refusal.

const unconfiguredNoteText = "/srv/proj has a .leadkit directory but no leadkit profile exists for it"

// unconfiguredEnv is outOfScopeEnv with the new seam answering note for every repo.
func unconfiguredEnv(t *testing.T, problem, note string, asked *[]string) PushGateConfig {
	t.Helper()
	cfg := outOfScopeEnv(t, problem, nil)
	cfg.Unconfigured = func(r string) string {
		if asked != nil {
			*asked = append(*asked, r)
		}
		return note
	}
	return cfg
}

// assertAllowReporting is an allow that is explicit about it, where it can be read. On a PreToolUse
// hook that exits 0, stderr reaches neither the user nor the model: stdout JSON does
// (systemMessage to the user, hookSpecificOutput.additionalContext to the model). So the report is
// asserted on STDOUT, and the allow must not carry a permissionDecision, which would override the
// permission flow. Stderr is checked too, as the debug-log copy.
func assertAllowReporting(t *testing.T, res Result, ctx string) {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("%s: want exit 0, got %+v", ctx, res)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
		t.Fatalf("%s: the allow must report on stdout as JSON, where the harness reads it: %v\nstdout=%q", ctx, err, res.Stdout)
	}
	hso, _ := payload["hookSpecificOutput"].(map[string]any)
	if hso == nil {
		t.Fatalf("%s: no hookSpecificOutput in %s", ctx, res.Stdout)
	}
	if _, has := hso["permissionDecision"]; has {
		t.Errorf("%s: the report must not carry a permissionDecision (it would override the permission flow): %s", ctx, res.Stdout)
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Errorf("%s: hookEventName = %v", ctx, hso["hookEventName"])
	}
	system, _ := payload["systemMessage"].(string)
	context, _ := hso["additionalContext"].(string)
	for field, text := range map[string]string{"systemMessage (shown to the user)": system, "additionalContext (given to the model)": context, "stderr": res.Stderr} {
		for _, want := range []string{"UNCONFIGURED", "[push-gate state: unconfigured]", "ALLOWED WITHOUT a review-gate check", "leadkit config init", unconfiguredNoteText} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: %s lacks %q:\n%s", ctx, field, want, text)
			}
		}
	}
}

// The state allows, for every shape of push, and says so. A silent allow fails the report check;
// a refusal fails the allow check.
func TestPushGateUnconfigured_AllowsAndReportsOnEveryDecision(t *testing.T) {
	cfg := unconfiguredEnv(t, "", unconfiguredNoteText, nil)
	for _, cmd := range pushShapes {
		assertAllowReporting(t, dispatchPush(t, cfg, cmd), cmd)
		// Every decision, not the first: the report does not depend on a once-per-session memory.
		assertAllowReporting(t, dispatchPush(t, cfg, cmd), cmd+" (again)")
	}
}

// The report is the state, and the state is not the same as a repo with no sign of leadkit,
// which stays a silent allow: the hook fires on every push on the machine.
func TestPushGateUnconfigured_NoStateNoReport(t *testing.T) {
	cfg := unconfiguredEnv(t, "", "", nil)
	for _, cmd := range pushShapes {
		assertAllow(t, dispatchPush(t, cfg, cmd), cmd)
	}
	cfg.Unconfigured = nil
	assertAllow(t, dispatchPush(t, cfg, "git push"), "seam not wired")
}

// A profile that exists and was not accepted is a refusal, and the unconfigured state cannot
// soften it, whatever order the seams are asked in (tickets 204, 260, 228).
func TestPushGateUnconfigured_AProfileProblemStillRefuses(t *testing.T) {
	cfg := unconfiguredEnv(t, profileProblemWhy, unconfiguredNoteText, nil)
	for _, cmd := range pushShapes {
		res := dispatchPush(t, cfg, cmd)
		assertDeny(t, res, cmd)
		if !strings.Contains(res.Stdout, "cannot be known") || strings.Contains(res.Stderr, "UNCONFIGURED") || strings.Contains(res.Stdout, "additionalContext") {
			t.Errorf("%s: the refusal must be for the profile, with no unconfigured report:\n%+v", cmd, res)
		}
	}
}

// The state is asked only about a push the gate would allow as out of scope. An in-scope repo
// is judged on its record whatever the seam says, so the state is never a way round a gate.
func TestPushGateUnconfigured_NeverConsultedForAnInScopeRepo(t *testing.T) {
	repo := t.TempDir()
	cfg, recordsDir := pushGateEnv(t, repo, enforcedRemote)
	var asked []string
	cfg.Unconfigured = func(r string) string { asked = append(asked, r); return unconfiguredNoteText }

	res := dispatchPush(t, cfg, "git push")
	assertDeny(t, res, "in scope, no record")
	if strings.Contains(res.Stdout, "UNCONFIGURED") || !strings.Contains(res.Stdout, "no review record") {
		t.Errorf("an in-scope ungated push is refused for its record:\n%s", res.Stdout)
	}
	writeFullRecord(t, recordsDir, gateTestSHA)
	assertAllow(t, dispatchPush(t, cfg, "git push"), "in scope, gated: nothing to report")
	if len(asked) != 0 {
		t.Errorf("the unconfigured seam was asked for an in-scope repo: %v", asked)
	}
}

// One verdict for the line, decided by its worst constituent: the report from an unconfigured
// repo does not end the judging, so a later in-scope ungated push on the same line is refused.
func TestPushGateUnconfigured_DoesNotShortCircuitTheRestOfTheLine(t *testing.T) {
	loose, scoped := t.TempDir(), t.TempDir()
	g := &gate.Gate{
		RecordsDir: t.TempDir(),
		Git: fakeGateGit{
			remotes: map[string]string{loose: "git@github.com:someorg/random.git", scoped: enforcedRemote},
			head:    gateTestSHA,
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{
		ResolveGate:  staticGate(g),
		Cwd:          loose,
		Unconfigured: func(r string) string { return map[string]string{loose: unconfiguredNoteText}[r] },
	}

	looseOnly := dispatchPush(t, cfg, "git -C "+loose+" push")
	assertAllowReporting(t, looseOnly, "the unconfigured repo alone")

	both := dispatchPush(t, cfg, "git -C "+loose+" push && git -C "+scoped+" push")
	assertDeny(t, both, "an unconfigured push followed by an in-scope ungated one")
	if !strings.Contains(both.Stdout, "no review record") {
		t.Errorf("the line is refused for the in-scope push:\n%s", both.Stdout)
	}
	assertDeny(t, dispatchPush(t, cfg, "git -C "+scoped+" push && git -C "+loose+" push"), "same, other order")

	// Two pushes into the one unconfigured repo report it once, not twice.
	twice := dispatchPush(t, cfg, "git -C "+loose+" push && git -C "+loose+" push origin main")
	if n := strings.Count(twice.Stderr, "[push-gate state: unconfigured]"); n != 1 {
		t.Errorf("want the state reported once for one repo, got %d times:\n%s", n, twice.Stderr)
	}
}

// A command the parser cannot read is judged on the repos it visibly touches; an unconfigured
// one is reported there too, and a profile problem still refuses.
func TestPushGateUnconfigured_UnreadableCommandFollowsTheSameRule(t *testing.T) {
	unsure := `bash -c "git push"`
	assertAllowReporting(t, dispatchPush(t, unconfiguredEnv(t, "", unconfiguredNoteText, nil), unsure), "unreadable command, unconfigured")
	assertDeny(t, dispatchPush(t, unconfiguredEnv(t, profileProblemWhy, unconfiguredNoteText, nil), unsure), "unreadable command, unreadable profile")
}

// The override still reads as an override: BYPASS lets it through and the state is still stated.
func TestPushGateUnconfigured_BypassStillReportsTheState(t *testing.T) {
	t.Setenv("REVIEW_GATE_BYPASS", "deliberate")
	assertAllowReporting(t, dispatchPush(t, unconfiguredEnv(t, "", unconfiguredNoteText, nil), "git push"), "BYPASS env")
}

// The report names the repo it is about, not the session's, and where to go from there.
func TestPushGateUnconfigured_ReportNamesTheRepo(t *testing.T) {
	note := UnconfiguredWarning(filepath.Join("/srv", "widgets"), "no profile")
	if !strings.Contains(note, "widgets is UNCONFIGURED") {
		t.Errorf("unexpected report: %q", note)
	}
}
