package orchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// applyFixture lays out a queue+store tree with: one ready backlog ticket (10,
// unplaced) and one in-flight ticket (20, under a dead lane "cr-old"). Role-keyed
// lanes: the code-review ROLE lane itself is what's live (queue/code-review/) —
// no ROLES file needed for ApplyPlan's routing any more.
func applyFixture(t *testing.T) (queueRoot, storeRoot, rolesPath string) {
	t.Helper()
	root := t.TempDir()
	storeRoot = filepath.Join(root, "tasks")
	queueRoot = filepath.Join(root, "queue")
	rolesPath = filepath.Join(queueRoot, "ROLES")

	writeStoreTask(t, storeRoot, "10-ready.md", "id: 10\nstatus: pending\nowner: code-review")
	if err := os.MkdirAll(filepath.Join(queueRoot, "code-review", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeStoreTask(t, storeRoot, "20-orphan.md", "id: 20\nstatus: in_progress\nowner: cr-old")
	linkOverlayTask(t, queueRoot, storeRoot, "cr-old", "in-progress", "20-orphan.md")

	return queueRoot, storeRoot, rolesPath
}

func TestApplyPlan_DryRun_WritesNothing(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{
		Route:   []RouteItem{{TaskID: "10", Agent: "cr-1", Type: "code-review"}},
		Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}},
	}

	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, true, "")
	if len(res.Errors) != 0 {
		t.Fatalf("dry-run should produce no errors, got %v", res.Errors)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "code-review", "pending", "10-ready.md")); !os.IsNotExist(err) {
		t.Error("dry-run must not actually create the route symlink")
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-old", "in-progress", "20-orphan.md")); err != nil {
		t.Error("dry-run must not move the orphaned ticket out of its dead lane")
	}
}

func TestApplyPlan_RoutesAndRequeuesForReal(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	logPath := filepath.Join(t.TempDir(), "orchestrate-apply.jsonl")
	plan := PlanResult{
		Route:   []RouteItem{{TaskID: "10", Agent: "cr-1", Type: "code-review"}},
		Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}},
	}

	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, logPath)
	if len(res.Errors) != 0 {
		t.Fatalf("want no errors, got %v", res.Errors)
	}
	if len(res.Routed) != 1 || len(res.Requeued) != 1 {
		t.Fatalf("want 1 routed + 1 requeued, got %+v", res)
	}

	if _, err := os.Lstat(filepath.Join(queueRoot, "code-review", "pending", "10-ready.md")); err != nil {
		t.Errorf("task 10 should now be routed into the code-review role lane: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-old", "in-progress", "20-orphan.md")); !os.IsNotExist(err) {
		t.Error("task 20 should no longer sit under the dead cr-old lane")
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, BacklogLane, "pending", "20-orphan.md")); err != nil {
		t.Errorf("task 20 should now sit in the backlog holding lane: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("change log not written: %v", err)
	}
	for _, want := range []string{`"action":"route"`, `"action":"requeue"`, `"task_id":"10"`, `"task_id":"20"`} {
		if !strings.Contains(string(logData), want) {
			t.Errorf("change log missing %q:\n%s", want, logData)
		}
	}
}

func TestApplyPlan_RequeuedTicketIsRoutableAgainNextCycle(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}}}
	if res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, ""); len(res.Errors) != 0 {
		t.Fatalf("want no errors, got %v", res.Errors)
	}

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, rolesPath, "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tk := range input.Backlog {
		if tk.ID == "20" {
			found = true
		}
	}
	if !found {
		t.Error("a requeued ticket should read as ordinary backlog on the next BuildPlanInput pass")
	}
}

func TestApplyPlan_NeverTouchesEscalateOrTriage(t *testing.T) {
	// PlanResult carrying Escalate/Triage items but no Route/Requeue — ApplyPlan has
	// no code path that could act on them at all (no field of PlanResult beyond
	// Route/Requeue is ever read by ApplyPlan); this just documents/asserts the
	// no-op explicitly.
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{
		Escalate: []EscalateItem{{ID: "99", Reason: "needs a human"}},
		Triage:   []TriageItem{{ID: "98", Reason: "no type"}},
	}
	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, "")
	if len(res.Routed) != 0 || len(res.Requeued) != 0 || len(res.Errors) != 0 {
		t.Errorf("escalate/triage-only plan should produce no action at all, got %+v", res)
	}
}

// --- routing-consistency.md Fix 2: apply is live-queue-safe ---

// TestApplyPlan_MovedRequeueSourceIsSkippedNotError is the core Fix-2 case: a
// requeue whose ticket is NO LONGER under rq.From (an owner claimed/moved it in the
// plan→apply window) is the NORMAL live-queue state — the requeue is moot. It must
// be recorded as a SKIP, never an Error, and must NOT mutate anything.
func TestApplyPlan_MovedRequeueSourceIsSkippedNotError(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	// Simulate the live-queue move: ticket 20 is no longer under cr-old — an owner
	// claimed it into code-review's in-progress since the plan was computed.
	if err := os.Remove(filepath.Join(queueRoot, "cr-old", "in-progress", "20-orphan.md")); err != nil {
		t.Fatal(err)
	}
	linkOverlayTask(t, queueRoot, storeRoot, "code-review", "in-progress", "20-orphan.md")

	plan := PlanResult{Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}}}
	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, "")

	if len(res.Errors) != 0 {
		t.Fatalf("a moved requeue-source must NOT be an error, got %v", res.Errors)
	}
	if len(res.Requeued) != 0 {
		t.Fatalf("a moot requeue must not be counted as requeued, got %+v", res.Requeued)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Kind != "requeue" || res.Skipped[0].ID != "20" {
		t.Fatalf("want exactly one requeue skip for ticket 20, got %+v", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Reason, "moot") {
		t.Errorf("skip reason should explain the requeue is moot, got %q", res.Skipped[0].Reason)
	}
	// Nothing must have been dropped: the ticket still sits where the owner moved it.
	if _, err := os.Lstat(filepath.Join(queueRoot, "code-review", "in-progress", "20-orphan.md")); err != nil {
		t.Errorf("the moved ticket must be left untouched where the owner has it: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, BacklogLane, "pending", "20-orphan.md")); !os.IsNotExist(err) {
		t.Error("a moot requeue must not fabricate a backlog placement")
	}
}

// TestApplyPlan_RequeueStillUnderFromMovesNormally proves the skip is narrow: a
// requeue whose ticket IS still under rq.From is moved for real, with no skip.
func TestApplyPlan_RequeueStillUnderFromMovesNormally(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}}}
	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, "")
	if len(res.Errors) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("a live requeue-source must move cleanly (no skip, no error), got skipped=%+v errors=%v", res.Skipped, res.Errors)
	}
	if len(res.Requeued) != 1 {
		t.Fatalf("want 1 requeued, got %+v", res.Requeued)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, BacklogLane, "pending", "20-orphan.md")); err != nil {
		t.Errorf("ticket 20 should have moved to the backlog holding lane: %v", err)
	}
}

// TestApplyPlan_GenuineMoveFailureIsStillError proves Errors is preserved for a REAL
// failure: the ticket IS under rq.From (found), but queue.Move can't complete — here
// because the backlog holding lane path is occupied by a FILE, so MkdirAll fails.
// That must land in Errors, not Skipped.
func TestApplyPlan_GenuineMoveFailureIsStillError(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	// Occupy queue/backlog with a regular file so MkdirAll(queue/backlog/pending) fails.
	if err := os.WriteFile(filepath.Join(queueRoot, BacklogLane), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := PlanResult{Requeue: []RequeueItem{{ID: "20", From: "cr-old", Attempts: 1}}}
	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, "")

	if len(res.Errors) != 1 {
		t.Fatalf("a genuine queue.Move failure must be an Error, got errors=%v skipped=%+v", res.Errors, res.Skipped)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("a genuine failure must not be miscounted as a skip, got %+v", res.Skipped)
	}
}

// TestApplyPlan_AlreadyQueuedRouteIsSkippedAndReRunIdempotent: a Route whose ticket
// is already queued somewhere (queue.Assign refuses "already queued") is surfaced as
// a skip in the summary, not a fatal error — so re-running apply after partial
// progress is clean/idempotent.
func TestApplyPlan_AlreadyQueuedRouteIsSkippedAndReRunIdempotent(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{Route: []RouteItem{{TaskID: "10", Agent: "cr-1", Type: "code-review"}}}

	// First apply routes ticket 10 into code-review for real.
	if res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, ""); len(res.Errors) != 0 || len(res.Routed) != 1 {
		t.Fatalf("first apply should route cleanly, got routed=%+v errors=%v", res.Routed, res.Errors)
	}

	// Second apply of the SAME plan: ticket 10 is already queued → skip, not error.
	res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, "")
	if len(res.Errors) != 0 {
		t.Fatalf("re-running apply after partial progress must be clean, got errors %v", res.Errors)
	}
	if len(res.Routed) != 0 {
		t.Errorf("an already-queued route must not re-route, got %+v", res.Routed)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Kind != "route" || res.Skipped[0].ID != "10" {
		t.Fatalf("want exactly one route skip for ticket 10, got %+v", res.Skipped)
	}
}

func TestApplyPlan_IdempotentSecondRunFindsNothingLeftToRoute(t *testing.T) {
	queueRoot, storeRoot, rolesPath := applyFixture(t)
	plan := PlanResult{Route: []RouteItem{{TaskID: "10", Agent: "cr-1", Type: "code-review"}}}
	if res := ApplyPlan(queueRoot, storeRoot, rolesPath, nil, plan, false, ""); len(res.Errors) != 0 {
		t.Fatalf("first apply: want no errors, got %v", res.Errors)
	}

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, rolesPath, "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range input.Backlog {
		if tk.ID == "10" {
			t.Error("task 10 should no longer be unplaced backlog after routing")
		}
	}
}
