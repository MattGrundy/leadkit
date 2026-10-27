package orchestrate

import (
	"os"
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// gatedTestSHA is a full 40-char sha for the Fix 2 gated-record tests.
const gatedTestSHA = "abababababababababababababababababababab"

// fakeGateGit is a stub gate.Git — RevParse echoes a full sha (no real git repo),
// so the gated-record tests never touch a real repository.
type fakeGateGit struct{}

func (fakeGateGit) RevParse(repo, ref string) (string, bool) {
	if len(ref) == 40 {
		return ref, true
	}
	return "", false
}
func (fakeGateGit) RemoteURL(repo string) string { return "" }

// newCompleteGateRecord builds a *gate.Gate whose records dir holds a COMPLETE
// record for gatedTestSHA (every RequiredLayers layer ready) — the "past review,
// awaiting push" state Fix 2 skips on.
func newCompleteGateRecord(t *testing.T) *gate.Gate {
	t.Helper()
	g := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeGateGit{}}
	if _, err := g.Record(gate.RecordOpts{SHA: gatedTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(gate.RecordOpts{SHA: gatedTestSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestBuildPlanInput_GatedTicketIsNeitherRoutedNorRequeued is Fix 2 (gated): a ticket
// whose HEAD sha has a COMPLETE gate record is past review/awaiting push — excluded
// from BOTH backlog routing and in-flight requeue, INDEPENDENT of status (which can
// still read in_progress) and placement. The 1454 case.
func TestBuildPlanInput_GatedTicketIsNeitherRoutedNorRequeued(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	repo := t.TempDir()
	g := newCompleteGateRecord(t)

	// Status still reads in_progress; the gated skip must key on the record, not status.
	writeStoreTask(t, storeRoot, "1454-x.md", "id: 1454\nstatus: in_progress\nowner: ci-infra\nrepo: "+repo+"\nsha: "+gatedTestSHA)
	linkOverlayTask(t, queueRoot, storeRoot, "ci-infra", "in-progress", "1454-x.md")

	input, err := BuildPlanInput(g, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 0 || len(input.InFlight) != 0 {
		t.Fatalf("a fully-gated ticket must be excluded from BOTH backlog and in-flight, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
	if res := Plan(input); len(res.Route) != 0 || len(res.Requeue) != 0 {
		t.Fatalf("gated ticket: want no route/requeue from Plan, got route=%+v requeue=%+v", res.Route, res.Requeue)
	}
}

// TestBuildPlanInput_FinalisePlacedTicketIsNotRoutedToOwner is Fix 2 (finalise-placed):
// a ticket physically in finalise/pending is the finalise consumer's in-flight
// (awaiting push) — never a backlog route/requeue to its work-owner (ci-infra). Uses a
// nil gate so ONLY the finalise-placement signal excludes it (the degrade-safe path).
func TestBuildPlanInput_FinalisePlacedTicketIsNotRoutedToOwner(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")

	writeStoreTask(t, storeRoot, "1454-x.md", "id: 1454\nstatus: in_progress\nowner: ci-infra")
	linkOverlayTask(t, queueRoot, storeRoot, "finalise", "pending", "1454-x.md")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 0 || len(input.InFlight) != 0 {
		t.Fatalf("a finalise-placed ticket must not be a routing/requeue candidate, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
	if res := Plan(input); len(res.Route) != 0 || len(res.Requeue) != 0 {
		t.Fatalf("finalise-placed ticket: want no route/requeue to owner, got route=%+v requeue=%+v", res.Route, res.Requeue)
	}
}

// TestBuildPlanInput_NonGatedNonFinaliseBacklogStillRoutes: the no-regression case —
// an ordinary pending backlog ticket (no gate record, not in finalise) still routes to
// its idle role agent exactly as before.
func TestBuildPlanInput_NonGatedNonFinaliseBacklogStillRoutes(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	repo := t.TempDir()
	g := newCompleteGateRecord(t) // a complete record exists, but for a DIFFERENT sha

	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: ci-infra\nrepo: "+repo+"\nsha: cccccccccccccccccccccccccccccccccccccccc")
	roster := []RosterEntry{{Name: "ci-infra-1", Type: "ci-infra", Status: "idle"}}

	input, err := BuildPlanInput(g, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", roster, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].ID != "1" {
		t.Fatalf("want the normal ticket in Backlog, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
	res := Plan(input)
	if len(res.Route) != 1 || res.Route[0].TaskID != "1" {
		t.Fatalf("want task 1 routed to the idle ci-infra agent, got %+v", res)
	}
}

// TestBuildPlanInput_UnreadableGateRecordDegradesToStatusPlacement: degrade-safe — a
// ticket whose sha has NO readable record must not error the plan and must fall
// through to the normal status/placement path (here: ordinary backlog).
func TestBuildPlanInput_UnreadableGateRecordDegradesToStatusPlacement(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	repo := t.TempDir()
	g := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeGateGit{}} // empty records dir

	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: ci-infra\nrepo: "+repo+"\nsha: "+gatedTestSHA)

	input, err := BuildPlanInput(g, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatalf("an unreadable/absent gate record must never error the plan: %v", err)
	}
	if len(input.Backlog) != 1 {
		t.Fatalf("absent record must degrade to the normal backlog path, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
}

func writeStoreTask(t *testing.T, storeRoot, filename, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(storeRoot, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func linkOverlayTask(t *testing.T, queueRoot, storeRoot, lane, status, base string) {
	t.Helper()
	dir := filepath.Join(queueRoot, lane, status)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(storeRoot, base))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(dir, base)); err != nil {
		t.Fatal(err)
	}
}

func writeRoles(t *testing.T, rolesPath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(rolesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolesPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeAgentDefFile is a minimal canonical-type-set fixture entry, for the
// type-field-preference tests below.
func writeAgentDefFile(t *testing.T, agentsDir, name string) {
	t.Helper()
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, name+".md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPlanInput_ExplicitCanonicalTypeWinsOverOwner(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	agentsDir := filepath.Join(root, "team-agents")
	writeAgentDefFile(t, agentsDir, "devops-agent")
	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: ci-infra-agent\ntype: devops-agent")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), agentsDir, nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].Type != "devops-agent" {
		t.Fatalf("want the explicit canonical type (devops-agent) to win over owner (ci-infra-agent), got %+v", input.Backlog)
	}
}

func TestBuildPlanInput_NonCanonicalTypeFallsBackToOwner(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	agentsDir := filepath.Join(root, "team-agents")
	writeAgentDefFile(t, agentsDir, "devops-agent")
	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: ci-infra-agent\ntype: not-a-real-type")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), agentsDir, nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].Type != "ci-infra-agent" {
		t.Fatalf("a non-canonical type must fall back to owner (unchanged), got %+v", input.Backlog)
	}
}

func TestBuildPlanInput_NoTypeFieldBehavesExactlyAsBefore(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	agentsDir := filepath.Join(root, "team-agents")
	writeAgentDefFile(t, agentsDir, "devops-agent")
	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: ci-infra-agent")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), agentsDir, nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].Type != "ci-infra-agent" {
		t.Fatalf("no type field: want the original owner/lane fallback (ci-infra-agent as-is, unchanged), got %+v", input.Backlog)
	}
}

func TestBuildPlanInput_UnplacedPendingTaskIsBacklog(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "1-x.md", "id: 1\nstatus: pending\nowner: code-review\npriority: P1")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].ID != "1" {
		t.Fatalf("want task 1 in Backlog, got %+v", input.Backlog)
	}
	if input.Backlog[0].Type != "code-review" || input.Backlog[0].Priority != "P1" || !input.Backlog[0].DepsMet {
		t.Errorf("Backlog[0] = %+v, want Type=code-review Priority=P1 DepsMet=true", input.Backlog[0])
	}
	if len(input.InFlight) != 0 {
		t.Errorf("want no in-flight tasks, got %+v", input.InFlight)
	}
}

func TestBuildPlanInput_PlacedTaskIsInFlight(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "2-y.md", "id: 2\nstatus: in_progress\nowner: cr-1")
	linkOverlayTask(t, queueRoot, storeRoot, "cr-1", "in-progress", "2-y.md")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.InFlight) != 1 || input.InFlight[0].ID != "2" || input.InFlight[0].Lane != "cr-1" {
		t.Fatalf("want task 2 in-flight under lane cr-1, got %+v", input.InFlight)
	}
}

func TestBuildPlanInput_DepsNotMetWhenBlockerUnfinished(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "1-blocker.md", "id: 1\nstatus: pending")
	writeStoreTask(t, storeRoot, "2-blocked.md", "id: 2\nstatus: pending\nowner: devops\nblockedBy: [1]")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Task{}
	for _, tk := range input.Backlog {
		byID[tk.ID] = tk
	}
	if byID["2"].DepsMet {
		t.Error("task 2's blocker (task 1) is still pending — DepsMet should be false")
	}
}

func TestBuildPlanInput_DepsMetWhenBlockerCompleted(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "1-blocker.md", "id: 1\nstatus: completed")
	writeStoreTask(t, storeRoot, "2-blocked.md", "id: 2\nstatus: pending\nowner: devops\nblockedBy: [1]")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Task{}
	for _, tk := range input.Backlog {
		byID[tk.ID] = tk
	}
	if !byID["2"].DepsMet {
		t.Error("task 2's blocker (task 1) is completed — DepsMet should be true")
	}
}

func TestBuildPlanInput_TypeResolvesViaLaneReverseLookupWhenOwnerUnassigned(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "3-z.md", "id: 3\nstatus: in_progress\nowner: unassigned")
	linkOverlayTask(t, queueRoot, storeRoot, "cr-1", "in-progress", "3-z.md")
	writeRoles(t, filepath.Join(queueRoot, "ROLES"), "code-review cr-1\n")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.InFlight) != 1 || input.InFlight[0].Type != "code-review" {
		t.Fatalf("want Type resolved via ROLES reverse lookup to code-review, got %+v", input.InFlight)
	}
}

func TestBuildPlanInput_UnresolvableTypeIsEmpty(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "4-w.md", "id: 4\nstatus: pending\nowner: unassigned")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].Type != "" {
		t.Fatalf("want an empty Type when owner is unassigned and there's no lane to reverse-resolve, got %+v", input.Backlog)
	}
}

func TestBuildPlanInput_UnrecognizedStatusEscalates(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "5-locked.md", "id: 5\nstatus: \"LOCKED (co-designed with user)\"\nowner: devops")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].EscalateReason == "" {
		t.Fatalf("want the LOCKED task to carry an EscalateReason, got %+v", input.Backlog)
	}
}

func TestBuildPlanInput_TerminalStatusIsExcludedEntirely(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "6-done.md", "id: 6\nstatus: completed\nowner: devops")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 0 || len(input.InFlight) != 0 {
		t.Errorf("a completed task should not appear anywhere in the plan input, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
}

// TestBuildPlanInput_ReviewAndTerminalStatusesAreNotPlanningCandidates is Fix 2's
// build.go layer: in_review / awaiting_pr_review / completed tickets — and a project
// status_aliases spelling that resolves to a review state — never appear in Backlog
// or InFlight, even when placed in a live role lane.
func TestBuildPlanInput_ReviewAndTerminalStatusesAreNotPlanningCandidates(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "1-r.md", "id: 1\nstatus: in_review\nowner: devops")
	writeStoreTask(t, storeRoot, "2-p.md", "id: 2\nstatus: awaiting_pr_review\nowner: devops")
	writeStoreTask(t, storeRoot, "3-d.md", "id: 3\nstatus: completed\nowner: devops")
	writeStoreTask(t, storeRoot, "4-a.md", "id: 4\nstatus: awaiting_review\nowner: devops") // profile alias -> in_review
	linkOverlayTask(t, queueRoot, storeRoot, "devops", "in-review", "1-r.md")               // placed, would be in-flight if not skipped

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil,
		map[string]string{"awaiting_review": "in_review"}, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 0 || len(input.InFlight) != 0 {
		t.Fatalf("terminal/review tickets must never be planning candidates, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
}

// TestBuildPlanInput_HeldLaneTicketIsNeitherBacklogNorInFlight is Fix 3: a ticket
// physically placed in a leading-`_` holding pen is set aside — excluded from BOTH
// backlog and in-flight, so plan never proposes routing it.
func TestBuildPlanInput_HeldLaneTicketIsNeitherBacklogNorInFlight(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "1-h.md", "id: 1\nstatus: pending\nowner: devops")
	linkOverlayTask(t, queueRoot, storeRoot, "_held", "pending", "1-h.md")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 0 || len(input.InFlight) != 0 {
		t.Fatalf("a _held/ ticket is set aside — neither routable backlog nor in-flight, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
}

func TestBuildPlanInput_BacklogLanePlacementReadsAsUnplaced(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	writeStoreTask(t, storeRoot, "7-req.md", "id: 7\nstatus: pending\nowner: devops")
	linkOverlayTask(t, queueRoot, storeRoot, BacklogLane, "pending", "7-req.md")

	input, err := BuildPlanInput(nil, queueRoot, storeRoot, filepath.Join(queueRoot, "ROLES"), "", nil, nil, nil, 0, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Backlog) != 1 || input.Backlog[0].ID != "7" {
		t.Errorf("a ticket parked under BacklogLane should read as ordinary backlog, got backlog=%+v inflight=%+v", input.Backlog, input.InFlight)
	}
}
