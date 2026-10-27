package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// writeLaneTicket drops a real ticket file into queueRoot/<lane>/<status>/, creating
// the dirs — a t.TempDir queue fixture the sweep can census.
func writeLaneTicket(t *testing.T, queueRoot, lane, status, name string) {
	t.Helper()
	dir := filepath.Join(queueRoot, lane, status)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("ticket"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestComputeStandupPlan_RosterEmitsOwnersAndFixedConsumers(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES") // absent → lane == role name

	// ci-pipeline: pending, nothing in-progress → UNSTAFFED.
	writeLaneTicket(t, queueRoot, "ci-pipeline", "pending", "t1.md")
	// devops: an in-progress ticket → already staffed.
	writeLaneTicket(t, queueRoot, "devops", "in-progress", "t2.md")

	res, err := queue.Sweep(queueRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	roster := []RosterSpec{{Type: "ci-pipeline", Count: 1}, {Type: "devops", Count: 1}}
	plan := ComputeStandupPlan(roster, queueRoot, rolesPath, res.Lanes)

	if !plan.HasRoster {
		t.Fatal("HasRoster = false, want true")
	}

	// Every consumer the plan must name, with the exact idle `queue wait <lane>`.
	// The review consumers are the gate's required layers (default code-review +
	// critique) — NOT outcome-verifier, which the gate retired.
	wantWait := map[string]string{
		"ci-pipeline": "leadkit queue wait " + queueRoot + "/ci-pipeline",
		"devops":      "leadkit queue wait " + queueRoot + "/devops",
		"code-review": "leadkit queue wait " + queueRoot + "/code-review",
		"critique":    "leadkit queue wait " + queueRoot + "/critique",
		"finalise":    "leadkit queue wait " + queueRoot + "/finalise",
	}
	got := map[string]StandupConsumer{}
	for _, c := range plan.Consumers {
		got[c.Role] = c
	}
	for role, wait := range wantWait {
		c, ok := got[role]
		if !ok {
			t.Errorf("plan has no consumer for %q", role)
			continue
		}
		if c.WaitCmd != wait {
			t.Errorf("%s WaitCmd = %q, want %q", role, c.WaitCmd, wait)
		}
	}

	// Kinds: owners for roster roles, review for the trio, finalise last.
	if got["ci-pipeline"].Kind != "owner" || got["devops"].Kind != "owner" {
		t.Errorf("roster roles must be kind=owner: %+v %+v", got["ci-pipeline"], got["devops"])
	}
	if got["code-review"].Kind != "review" || got["finalise"].Kind != "finalise" {
		t.Errorf("fixed consumer kinds wrong: cr=%q finalise=%q", got["code-review"].Kind, got["finalise"].Kind)
	}

	// Live annotation: devops staffed, ci-pipeline unstaffed.
	if got["devops"].Staffed != "staffed" {
		t.Errorf("devops Staffed = %q, want staffed (it has an in-progress ticket)", got["devops"].Staffed)
	}
	if got["ci-pipeline"].Staffed != "unstaffed" {
		t.Errorf("ci-pipeline Staffed = %q, want unstaffed (pending, nothing in-progress)", got["ci-pipeline"].Staffed)
	}

	out := FormatStandupPlan(plan)
	for _, want := range []string{
		"ci-pipeline", "devops", "code-review", "critique", "finalise",
		"leadkit queue wait " + queueRoot + "/devops",
		"already staffed", "UNSTAFFED",
		"queue assign", "never exits", // owner loop + dispatch-only recipe
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStandupPlan output missing %q\n---\n%s", want, out)
		}
	}
	// The #123 fix: the retired outcome-verifier must NOT appear (neither as a consumer
	// nor in the emitted plan) while the gate is 2-layer.
	if _, ok := got["outcome-verifier"]; ok {
		t.Errorf("plan must NOT list the retired outcome-verifier consumer")
	}
	if strings.Contains(out, "outcome-verifier") {
		t.Errorf("FormatStandupPlan output must NOT mention outcome-verifier\n---\n%s", out)
	}
}

// TestFormatStandupPlan_EmitsForegroundOwnerLoopContract is the Fix-2 assertion: the
// emitted owner-loop plan must state the FOREGROUND `queue wait` instruction and the
// post-commit contract (after commit → review send → re-arm; commit is not the end),
// naming the background-wait wedge as the anti-pattern. The wedge is an owner ending
// its turn mid-loop, so the emitted text has to make the continuous-turn loop explicit.
func TestFormatStandupPlan_EmitsForegroundOwnerLoopContract(t *testing.T) {
	queueRoot := t.TempDir()
	plan := ComputeStandupPlan(
		[]RosterSpec{{Type: "devops", Count: 1}},
		queueRoot, filepath.Join(queueRoot, "ROLES"), nil,
	)
	out := FormatStandupPlan(plan)
	for _, want := range []string{
		"in the FOREGROUND",                      // run queue wait in the foreground
		"Do NOT background it",                   // the named foreground requirement
		"ONE CONTINUOUS TURN",                    // the whole loop in one turn
		"git commit",                             // commit is a loop step, not the end
		"review send",                            // enqueue the gate
		"re-arm",                                 // re-arm the wait
		"commit is not the end",                  // the post-commit contract, verbatim
		"ANTI-PATTERN",                           // named anti-pattern
		"FIRST ACTION ON SPAWN",                  // the post-spawn first-action directive
		"before anything else",                   // step 0, before anything else
		"STARTING the loop is your first action", // do not read the contract and end the turn
		"post-spawn-dormancy",                    // the named post-spawn anti-pattern
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStandupPlan output missing owner-loop contract %q\n---\n%s", want, out)
		}
	}
}

// `queue move` resolves its arguments against the cwd, not the queue root, so the claim step
// in the owner loop must spell out <queue_root>/ exactly as the `queue wait` step does. A bare
// <role>/pending/<t> fails with "source does not exist" from any directory but the root.
func TestFormatStandupPlan_ClaimStepNamesTheQueueRoot(t *testing.T) {
	queueRoot := t.TempDir()
	plan := ComputeStandupPlan(
		[]RosterSpec{{Type: "devops", Count: 1}},
		queueRoot, filepath.Join(queueRoot, "ROLES"), nil,
	)
	out := FormatStandupPlan(plan)
	want := "`leadkit queue move <queue_root>/<role>/pending/<t> <queue_root>/<role>/in-progress/`"
	if !strings.Contains(out, want) {
		t.Errorf("owner loop must claim with %s\n---\n%s", want, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "leadkit queue move") && !strings.Contains(line, "<queue_root>/") {
			t.Errorf("queue move line without <queue_root>/: %q", line)
		}
	}
}

// TestFormatStandupPlan_EmitsNonInteractiveNoBlockingContract is the Part-2 assertion
// (docs/build-tasks/frozen-claim-and-noninteractive.md): the emitted owner contract must
// mandate NON-INTERACTIVE commands, TIME-BOUNDED long/heavy commands, NO inline-blocking
// call, and REPORT-DON'T-RETRY on a hang — and NAME the failure (a blocking call wedges
// the owner INSIDE the call, unreachable to messages that drain only at a tool boundary).
func TestFormatStandupPlan_EmitsNonInteractiveNoBlockingContract(t *testing.T) {
	queueRoot := t.TempDir()
	plan := ComputeStandupPlan(
		[]RosterSpec{{Type: "devops", Count: 1}},
		queueRoot, filepath.Join(queueRoot, "ROLES"), nil,
	)
	out := FormatStandupPlan(plan)
	for _, want := range []string{
		"NON-INTERACTIVE",      // the non-interactive mandate
		"--yes",                // an assume-yes flag named
		"CI=1",                 // the CI flag named
		"interactive approval", // never a blocking approval prompt
		"TIME-BOUNDED",         // long/heavy commands are timeout-bounded
		"timeout",              // the timeout mechanism named
		"inline-blocking",      // never run inline-blocking in the turn
		"INSIDE",               // the failure named: wedged INSIDE the call
		"tool boundary",        // messages drain only at a tool boundary
		"REPORT, DON'T RETRY",  // report-don't-retry-on-hang clause
		"retry or sit",         // the discipline: report, don't retry or sit
		"build/test suites",    // generic wording — no adopter build tool named
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStandupPlan output missing non-interactive contract %q\n---\n%s", want, out)
		}
	}
	// Project-agnostic: the standup text must name NO specific adopter build tool.
	//
	// These needles are the REAL names on purpose, read from the scrub term file rather than
	// written here: a generic stand-in would make this assertion vacuously true, since the
	// stand-in never appeared in any output, and a literal would put the name in the repo.
	// Without the file there is nothing specific to look for, so this checks nothing (the
	// ratchet in cmd/leadkit says so by skipping).
	for _, banned := range scrubterms.Optional(t).ForTests() {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("FormatStandupPlan output must not name a specific adopter build tool %q\n---\n%s", banned, out)
		}
	}
}

// dirExists is a small stat helper for the scaffold tests.
func dirExists(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// TestStandupScaffold_MissingLaneCreated is the core spec case: roster
// [ci-pipeline, qa] with queue/ci-pipeline present but no queue/qa. The no-flag plan
// must report qa's lane as missing and create nothing; --scaffold (applied) must
// create queue/qa/{pending,in-progress,done} and leave ci-pipeline untouched.
func TestStandupScaffold_MissingLaneCreated(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES") // absent → lane == role name

	// ci-pipeline lane present (empty stages); qa lane absent.
	for _, sub := range []string{"pending", "in-progress", "done"} {
		if err := os.MkdirAll(filepath.Join(queueRoot, "ci-pipeline", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	res, err := queue.Sweep(queueRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	roster := []RosterSpec{{Type: "ci-pipeline", Count: 1}, {Type: "qa", Count: 1}}
	plan := ComputeStandupPlan(roster, queueRoot, rolesPath, res.Lanes)

	// No-flag annotation: qa's lane reads as missing; nothing is created by computing.
	out := FormatStandupPlan(plan)
	if !strings.Contains(out, "lane missing") {
		t.Errorf("no-flag plan must annotate the missing lane; output:\n%s", out)
	}
	if dirExists(t, filepath.Join(queueRoot, "qa")) {
		t.Fatal("computing the plan must not create queue/qa")
	}

	// Scaffold plan: qa is missing (will create), ci-pipeline is present.
	sp := ComputeStandupScaffoldPlan(plan)
	byLane := map[string]StandupScaffoldLane{}
	for _, l := range sp.Lanes {
		byLane[l.Lane] = l
	}
	if l, ok := byLane["qa"]; !ok || l.Present || l.Refused || len(l.Create) != 3 {
		t.Fatalf("qa lane should be a fresh create of 3 stage dirs, got %+v", l)
	}
	if l, ok := byLane["ci-pipeline"]; !ok || !l.Present {
		t.Fatalf("ci-pipeline lane should be present/untouched, got %+v", l)
	}

	applied, err := ApplyStandupScaffoldPlan(sp)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"pending", "in-progress", "done"} {
		if !dirExists(t, filepath.Join(queueRoot, "qa", sub)) {
			t.Errorf("--scaffold must create queue/qa/%s", sub)
		}
	}
	// qa must be among the created lanes; ci-pipeline must NOT be (it was present).
	createdQA, createdCI := false, false
	for _, l := range applied.CreatedLanes {
		if l == "qa" {
			createdQA = true
		}
		if l == "ci-pipeline" {
			createdCI = true
		}
	}
	if !createdQA {
		t.Errorf("CreatedLanes = %v, want it to include qa", applied.CreatedLanes)
	}
	if createdCI {
		t.Errorf("CreatedLanes = %v, must NOT include the already-present ci-pipeline", applied.CreatedLanes)
	}
	// ci-pipeline present and untouched (an empty-but-present lane is never rewritten).
	present := false
	for _, l := range applied.PresentLanes {
		if l == "ci-pipeline" {
			present = true
		}
	}
	if !present {
		t.Errorf("PresentLanes must include ci-pipeline, got %v", applied.PresentLanes)
	}
}

// TestStandupScaffold_IdempotentRerun: re-running --scaffold against an
// already-scaffolded queue is a no-op — it creates nothing.
func TestStandupScaffold_IdempotentRerun(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")

	roster := []RosterSpec{{Type: "qa", Count: 1}}
	plan := ComputeStandupPlan(roster, queueRoot, rolesPath, nil)

	// First apply creates the missing lanes.
	first, err := ApplyStandupScaffoldPlan(ComputeStandupScaffoldPlan(plan))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.CreatedDirs) == 0 {
		t.Fatal("first --scaffold should have created dirs")
	}

	// Second run: recompute against the now-populated tree — nothing to create.
	sp2 := ComputeStandupScaffoldPlan(ComputeStandupPlan(roster, queueRoot, rolesPath, nil))
	if sp2.WouldCreate() != 0 {
		t.Errorf("re-run WouldCreate = %d, want 0 (idempotent)", sp2.WouldCreate())
	}
	second, err := ApplyStandupScaffoldPlan(sp2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.CreatedDirs) != 0 {
		t.Errorf("idempotent re-run created %v, want nothing", second.CreatedDirs)
	}
}

// TestStandupScaffold_FixedConsumersScaffolded: the review-gate consumers (the gate's
// required layers) plus finalise get their lanes created too — and the retired
// outcome-verifier does NOT get a lane while the gate is 2-layer.
func TestStandupScaffold_FixedConsumersScaffolded(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")

	plan := ComputeStandupPlan(nil, queueRoot, rolesPath, nil)
	if _, err := ApplyStandupScaffoldPlan(ComputeStandupScaffoldPlan(plan)); err != nil {
		t.Fatal(err)
	}
	for _, role := range append(append([]string{}, gate.RequiredLayers...), "finalise") {
		for _, sub := range []string{"pending", "in-progress", "done"} {
			if !dirExists(t, filepath.Join(queueRoot, role, sub)) {
				t.Errorf("fixed consumer lane queue/%s/%s not scaffolded", role, sub)
			}
		}
	}
	if dirExists(t, filepath.Join(queueRoot, "outcome-verifier")) {
		t.Errorf("retired outcome-verifier must NOT be scaffolded under a 2-layer gate")
	}
}

// TestStandupScaffold_LaneMatchesCanonicalLane: the scaffolded lane is exactly the
// lane roles.CanonicalLane resolves the role to — so an owner `queue wait <lane>`
// finds the dir standup made.
func TestStandupScaffold_LaneMatchesCanonicalLane(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")

	roster := []RosterSpec{{Type: "qa", Count: 1}}
	plan := ComputeStandupPlan(roster, queueRoot, rolesPath, nil)
	if _, err := ApplyStandupScaffoldPlan(ComputeStandupScaffoldPlan(plan)); err != nil {
		t.Fatal(err)
	}

	known := roles.KnownRoles([]string{"qa"})
	canon, ok := roles.CanonicalLane("qa", known, nil)
	if !ok {
		t.Fatal("CanonicalLane(qa) should resolve")
	}
	if !dirExists(t, filepath.Join(queueRoot, canon, "pending")) {
		t.Errorf("scaffolded lane must match CanonicalLane %q — queue/%s not found", canon, canon)
	}
	// And it matches the lane the plan's wait invocation names.
	for _, c := range plan.Consumers {
		if c.Role == "qa" && c.Lane != canon {
			t.Errorf("plan lane %q != CanonicalLane %q for qa", c.Lane, canon)
		}
	}
}

// TestStandupScaffold_UnsafeLaneRefused: a role name with a path separator / ".." is
// refused — nothing is created outside queue_root.
func TestStandupScaffold_UnsafeLaneRefused(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	if err := os.MkdirAll(queueRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// A canary file outside queue_root that a traversal would clobber/reach.
	outside := filepath.Join(root, "evil")

	roster := []RosterSpec{{Type: "../evil", Count: 1}, {Type: "a/b", Count: 1}}
	plan := ComputeStandupPlan(roster, queueRoot, rolesPath, nil)
	sp := ComputeStandupScaffoldPlan(plan)

	refused := map[string]bool{}
	for _, l := range sp.Lanes {
		if l.Refused {
			refused[l.Lane] = true
		}
	}
	if !refused["../evil"] || !refused["a/b"] {
		t.Fatalf("unsafe lanes must be refused, got %+v", sp.Lanes)
	}

	if _, err := ApplyStandupScaffoldPlan(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("a traversal lane created a dir outside queue_root at %s", outside)
	}
	// Under queue_root, only the safe fixed consumers may appear — never a dir for an
	// unsafe roster name (no "a" from "a/b", no traversal artifact).
	entries, _ := os.ReadDir(queueRoot)
	allowed := map[string]bool{"finalise": true}
	for _, l := range gate.RequiredLayers {
		allowed[l] = true
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("unexpected dir under queue_root from an unsafe lane: %q", e.Name())
		}
	}
}

// TestStandupScaffold_SafeSegmentGuard exercises safeLaneSegment directly.
func TestStandupScaffold_SafeSegmentGuard(t *testing.T) {
	for _, lane := range []string{"qa", "code-review", "ci-pipeline", "..foo"} {
		if !safeLaneSegment(lane) {
			t.Errorf("safeLaneSegment(%q) = false, want true", lane)
		}
	}
	for _, lane := range []string{"", ".", "..", "a/b", "../x", "/abs", `a\b`} {
		if safeLaneSegment(lane) {
			t.Errorf("safeLaneSegment(%q) = true, want false", lane)
		}
	}
}

// TestComputeStandupPlan_ReviewConsumersTrackGateInOrder asserts the review consumers
// are EXACTLY gate.RequiredLayers, in that order, and sit before finalise — proving the
// standup review set is sourced from the gate (single source of truth), not a constant.
func TestComputeStandupPlan_ReviewConsumersTrackGateInOrder(t *testing.T) {
	queueRoot := t.TempDir()
	rolesPath := filepath.Join(queueRoot, "ROLES")
	plan := ComputeStandupPlan(
		[]RosterSpec{{Type: "devops", Count: 1}},
		queueRoot, rolesPath, nil,
	)

	// The review-kind consumers, in plan order, must equal gate.RequiredLayers verbatim.
	var reviewRoles []string
	finaliseIdx, lastReviewIdx := -1, -1
	for i, c := range plan.Consumers {
		switch c.Kind {
		case "review":
			reviewRoles = append(reviewRoles, c.Role)
			lastReviewIdx = i
		case "finalise":
			finaliseIdx = i
		}
	}
	if strings.Join(reviewRoles, ",") != strings.Join(gate.RequiredLayers, ",") {
		t.Errorf("review consumers = %v, want gate.RequiredLayers %v (same set, same order)", reviewRoles, gate.RequiredLayers)
	}
	if finaliseIdx < 0 || lastReviewIdx < 0 || finaliseIdx < lastReviewIdx {
		t.Errorf("finalise (idx %d) must come after the review layers (last review idx %d)", finaliseIdx, lastReviewIdx)
	}
}

// TestComputeStandupPlan_ThreeLayerGateListsOutcomeVerifier injects a 3-layer
// RequiredLayers (incl. outcome-verifier) and proves standup then lists it — i.e. the
// review-consumer set tracks the gate, so a future 3-layer project flows through with
// zero code change. Restores RequiredLayers so no other test is affected.
func TestComputeStandupPlan_ThreeLayerGateListsOutcomeVerifier(t *testing.T) {
	saved := gate.RequiredLayers
	gate.RequiredLayers = []string{"code-review", "critique", "outcome-verifier"}
	t.Cleanup(func() { gate.RequiredLayers = saved })

	queueRoot := t.TempDir()
	plan := ComputeStandupPlan(nil, queueRoot, filepath.Join(queueRoot, "ROLES"), nil)

	var reviewRoles []string
	for _, c := range plan.Consumers {
		if c.Kind == "review" {
			reviewRoles = append(reviewRoles, c.Role)
		}
	}
	if strings.Join(reviewRoles, ",") != "code-review,critique,outcome-verifier" {
		t.Errorf("3-layer gate: review consumers = %v, want the 3 injected layers in order", reviewRoles)
	}
	if !strings.Contains(FormatStandupPlan(plan), "outcome-verifier") {
		t.Errorf("3-layer gate: emitted plan must list outcome-verifier when the gate requires it")
	}
}

func TestComputeStandupPlan_NoRosterEmitsFixedConsumersAndDeclareNote(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	if err := os.MkdirAll(queueRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	rolesPath := filepath.Join(queueRoot, "ROLES")

	plan := ComputeStandupPlan(nil, queueRoot, rolesPath, nil)
	if plan.HasRoster {
		t.Fatal("HasRoster = true, want false (no roster)")
	}

	// The review (gate-derived) + finalise consumers must still be emitted; NO owners.
	roles := map[string]string{}
	for _, c := range plan.Consumers {
		roles[c.Role] = c.Kind
	}
	for _, fixed := range append(append([]string{}, gate.RequiredLayers...), "finalise") {
		if _, ok := roles[fixed]; !ok {
			t.Errorf("no-roster plan missing fixed consumer %q", fixed)
		}
	}
	if _, ok := roles["outcome-verifier"]; ok {
		t.Errorf("no-roster plan must NOT list the retired outcome-verifier")
	}
	for role, kind := range roles {
		if kind == "owner" {
			t.Errorf("no-roster plan must have no owners, got owner %q", role)
		}
	}

	out := FormatStandupPlan(plan)
	for _, want := range []string{"no roster declared", "Declare work-roles", "leadkit team standup"} {
		if !strings.Contains(out, want) {
			t.Errorf("no-roster output missing %q\n---\n%s", want, out)
		}
	}
}

// A roster role declared review_layer is a review consumer, listed after the built-in layers in roster
// order, and not also an owner: it records a layer, it does not take work.
func TestComputeStandupPlan_ADeclaredReviewRoleIsAReviewConsumerNotAnOwner(t *testing.T) {
	queueRoot := t.TempDir()
	plan := ComputeStandupPlan(
		[]RosterSpec{{Type: "devops", Count: 1}, {Type: "security", Count: 1, ReviewLayer: true}, {Type: "perf", Count: 1, ReviewLayer: true}},
		queueRoot, filepath.Join(queueRoot, "ROLES"), nil,
	)
	var owners, review []string
	for _, c := range plan.Consumers {
		switch c.Kind {
		case "owner":
			owners = append(owners, c.Role)
		case "review":
			review = append(review, c.Role)
		}
	}
	if strings.Join(owners, ",") != "devops" {
		t.Errorf("owners = %v, want only devops: a review layer is not a work owner", owners)
	}
	if strings.Join(review, ",") != "code-review,critique,security,perf" {
		t.Errorf("review consumers = %v, want the built-in layers then the declared roles in roster order", review)
	}
}
