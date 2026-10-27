package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/roles"
)

func TestComputeScaffoldPlan_EmptyProjectPlansEveryLaneAndRoleRow(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{{Type: "code-review", Count: 1}, {Type: "critique", Count: 1}}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("ComputeScaffoldPlan: %v", err)
	}
	if plan.RolesFileExists {
		t.Error("want RolesFileExists=false — nothing on disk yet")
	}
	wantRoles := map[string]bool{"code-review": true, "critique": true, "orchestrator": true, "finalise": true}
	if len(plan.Lanes) != len(wantRoles) {
		t.Fatalf("Lanes = %+v, want one entry per %v", plan.Lanes, wantRoles)
	}
	for _, l := range plan.Lanes {
		if !wantRoles[l.Role] {
			t.Errorf("unexpected role %q in plan", l.Role)
		}
		if !l.RoleRowAdd {
			t.Errorf("role %q: want RoleRowAdd=true on an empty project", l.Role)
		}
		if len(l.Missing) != 3 {
			t.Errorf("role %q: Missing = %v, want 3 (pending/in-progress/done)", l.Role, l.Missing)
		}
		if l.Lane != l.Role {
			t.Errorf("role %q: Lane = %q, want it to default to the role name", l.Role, l.Lane)
		}
	}
}

func TestApplyScaffoldPlan_CreatesLanesAndROLESRows(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{{Type: "code-review", Count: 1}}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("ComputeScaffoldPlan: %v", err)
	}
	res, err := ApplyScaffoldPlan(plan)
	if err != nil {
		t.Fatalf("ApplyScaffoldPlan: %v", err)
	}
	if len(res.CreatedDirs) != 3*3 { // code-review, orchestrator, finalise x 3 subdirs
		t.Errorf("CreatedDirs = %v, want 9 dirs", res.CreatedDirs)
	}
	for _, role := range []string{"code-review", "orchestrator", "finalise"} {
		for _, sub := range []string{"pending", "in-progress", "done"} {
			dir := filepath.Join(queueRoot, role, sub)
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				t.Errorf("want %s to exist as a dir, err=%v", dir, err)
			}
		}
	}

	live, ok := roles.Resolve(rolesPath, "code-review")
	if !ok || live != "code-review" {
		t.Errorf("roles.Resolve(code-review) = %q,%v, want code-review,true", live, ok)
	}
	if _, ok := roles.Resolve(rolesPath, "finalise"); !ok {
		t.Error("want a finalise row in ROLES")
	}
	if _, ok := roles.Resolve(rolesPath, "orchestrator"); !ok {
		t.Error("want an orchestrator row in ROLES")
	}
}

func TestComputeScaffoldPlan_ReRunIsIdempotentAndFillsOnlyGaps(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{{Type: "code-review", Count: 1}, {Type: "critique", Count: 1}}

	// First run, but only code-review is actually applied for real — simulating a
	// LIVE lane that already exists with a non-default name (as if a human had
	// pointed code-review at "cr-1" by hand), which a re-run must leave untouched.
	if err := os.MkdirAll(filepath.Join(queueRoot, "cr-1", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rolesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolesPath, []byte("code-review\tcr-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("ComputeScaffoldPlan: %v", err)
	}
	var crItem, critItem *ScaffoldLaneItem
	for i := range plan.Lanes {
		switch plan.Lanes[i].Role {
		case "code-review":
			crItem = &plan.Lanes[i]
		case "critique":
			critItem = &plan.Lanes[i]
		}
	}
	if crItem == nil || critItem == nil {
		t.Fatalf("plan missing code-review/critique: %+v", plan.Lanes)
	}
	if crItem.RoleRowAdd || crItem.Lane != "cr-1" || len(crItem.Missing) != 0 {
		t.Errorf("code-review (already live at cr-1) must be left untouched, got %+v", crItem)
	}
	if !critItem.RoleRowAdd || critItem.Lane != "critique" || len(critItem.Missing) != 3 {
		t.Errorf("critique (gap) must be planned fresh, got %+v", critItem)
	}

	if _, err := ApplyScaffoldPlan(plan); err != nil {
		t.Fatalf("ApplyScaffoldPlan: %v", err)
	}

	// Re-run: nothing left to do for code-review OR critique now.
	plan2, err2 := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err2 != nil {
		t.Fatalf("ComputeScaffoldPlan (re-run): %v", err2)
	}
	if plan2.AnyChanged() {
		t.Errorf("re-run should be a no-op, got plan: %+v", plan2.Lanes)
	}
	// The originally-live cr-1 lane must still be the ONLY code-review lane —
	// scaffold must never have created a second "code-review" dir alongside it.
	if _, err := os.Stat(filepath.Join(queueRoot, "code-review")); err == nil {
		t.Error("scaffold must not create a competing code-review/ lane when cr-1 already serves that role")
	}
}

func TestComputeScaffoldPlan_DryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{{Type: "code-review", Count: 1}}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("ComputeScaffoldPlan: %v", err)
	}
	if !plan.AnyChanged() {
		t.Fatal("want a nonempty plan on an empty project")
	}
	// ComputeScaffoldPlan itself must never write — confirm nothing landed on disk.
	if _, err := os.Stat(queueRoot); err == nil {
		t.Error("ComputeScaffoldPlan must not create the queue root")
	}
	if _, err := os.Stat(rolesPath); err == nil {
		t.Error("ComputeScaffoldPlan must not create ROLES")
	}
}

// --- GAP-4/item 4: multi-role binding, guarded ---

func TestComputeScaffoldPlan_TwoNonIndependenceRolesShareOneLane(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{
		{Type: "code-review", Count: 1},
		{Type: "critique", Count: 1, Lane: "crit-ov"},
		{Type: "outcome-verifier", Count: 1, Lane: "crit-ov"},
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("critique+outcome-verifier sharing a lane must be accepted, got: %v", err)
	}
	byRole := map[string]ScaffoldLaneItem{}
	for _, l := range plan.Lanes {
		byRole[l.Role] = l
	}
	if byRole["critique"].Lane != "crit-ov" || byRole["outcome-verifier"].Lane != "crit-ov" {
		t.Errorf("both roles should resolve to lane crit-ov, got critique=%q outcome-verifier=%q",
			byRole["critique"].Lane, byRole["outcome-verifier"].Lane)
	}
	if !byRole["critique"].RoleRowAdd || !byRole["outcome-verifier"].RoleRowAdd {
		t.Error("both roles must get their own ROLES row even though they share a lane")
	}
	// The shared lane's mkdirs must be planned exactly once, not duplicated per role.
	if len(byRole["critique"].Missing) != 3 {
		t.Errorf("critique.Missing = %v, want the 3 canonical subdirs (first role to claim the shared lane)", byRole["critique"].Missing)
	}
	if len(byRole["outcome-verifier"].Missing) != 0 {
		t.Errorf("outcome-verifier.Missing = %v, want empty — the lane was already planned by critique", byRole["outcome-verifier"].Missing)
	}

	res, err := ApplyScaffoldPlan(plan)
	if err != nil {
		t.Fatalf("ApplyScaffoldPlan: %v", err)
	}
	for _, sub := range []string{"pending", "in-progress", "done"} {
		if _, err := os.Stat(filepath.Join(queueRoot, "crit-ov", sub)); err != nil {
			t.Errorf("want %s/crit-ov/%s to exist: %v", queueRoot, sub, err)
		}
	}
	liveCrit, ok := roles.Resolve(rolesPath, "critique")
	if !ok || liveCrit != "crit-ov" {
		t.Errorf("roles.Resolve(critique) = %q,%v, want crit-ov,true", liveCrit, ok)
	}
	liveOV, ok := roles.Resolve(rolesPath, "outcome-verifier")
	if !ok || liveOV != "crit-ov" {
		t.Errorf("roles.Resolve(outcome-verifier) = %q,%v, want crit-ov,true", liveOV, ok)
	}
	// code-review, critique, outcome-verifier, orchestrator, finalise — every
	// canonical role gets its own ROLES row regardless of lane sharing.
	if len(res.RolesAdded) != 5 {
		t.Errorf("RolesAdded = %+v, want 5 rows", res.RolesAdded)
	}
}

// TestComputeScaffoldPlan_RefusesCodeReviewAndCritiqueSharingALane is the item-4
// GUARD: the ONE combination that must never be offered, even via explicit roster
// config — code-review and critique sharing a queue-consumer lane. This must be
// refused outright (an error, no plan produced), not silently accepted.
func TestComputeScaffoldPlan_RefusesCodeReviewAndCritiqueSharingALane(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{
		{Type: "code-review", Count: 1, Lane: "one-agent"},
		{Type: "critique", Count: 1, Lane: "one-agent"},
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err == nil {
		t.Fatalf("want a refusal binding code-review+critique to the same lane, got a plan: %+v", plan)
	}
	if !strings.Contains(err.Error(), "layer independence") {
		t.Errorf("err = %v, want it to name layer independence as the reason", err)
	}
	if _, statErr := os.Stat(queueRoot); statErr == nil {
		t.Error("a refused plan must never have touched disk")
	}
}

// TestComputeScaffoldPlan_RefusesMixedDefaultAndOverrideCollision covers the
// narrower bug the first pass of the refusal missed: code-review has NO explicit
// override (so it resolves to its default lane, its own role name,
// "code-review") while critique's explicit override points AT that very string.
// The two roles never look "the same" syntactically (only one has a Lane set at
// all) but resolve to the identical live lane — must still be refused.
func TestComputeScaffoldPlan_RefusesMixedDefaultAndOverrideCollision(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{
		{Type: "code-review", Count: 1}, // no override — defaults to lane "code-review"
		{Type: "critique", Count: 1, Lane: "code-review"},
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err == nil {
		t.Fatalf("want a refusal — critique's override lands on code-review's default lane, got a plan: %+v", plan)
	}
	if !strings.Contains(err.Error(), "layer independence") {
		t.Errorf("err = %v, want it to name layer independence as the reason", err)
	}
}

// TestComputeScaffoldPlan_RefusesOverrideOntoAnAlreadyRoutedRole covers the other
// bug: code-review is ALREADY routed live (in ROLES, at "cr-1" — as if a human had
// wired it by hand before ever running scaffold), and critique's roster override
// points directly at that same live lane name. Neither role has a matching
// EXPLICIT override (code-review has none at all), so the resolved-lane
// comparison, not a raw override-string comparison, is what has to catch this.
func TestComputeScaffoldPlan_RefusesOverrideOntoAnAlreadyRoutedRole(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	if err := os.MkdirAll(filepath.Join(queueRoot, "cr-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolesPath, []byte("code-review\tcr-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roster := []RosterSpec{
		{Type: "code-review", Count: 1}, // already live at cr-1, no roster override needed
		{Type: "critique", Count: 1, Lane: "cr-1"},
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err == nil {
		t.Fatalf("want a refusal — critique's override lands on code-review's already-routed live lane, got a plan: %+v", plan)
	}
	if !strings.Contains(err.Error(), "layer independence") {
		t.Errorf("err = %v, want it to name layer independence as the reason", err)
	}
}

// TestComputeScaffoldPlan_DefaultCodeReviewAndCritiqueLanesStillScaffold is the
// control: the ordinary, non-colliding case (no overrides at all — each role
// resolves to its own default lane) must still scaffold cleanly after the
// resolved-lane refusal fix.
func TestComputeScaffoldPlan_DefaultCodeReviewAndCritiqueLanesStillScaffold(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	roster := []RosterSpec{
		{Type: "code-review", Count: 1},
		{Type: "critique", Count: 1},
	}

	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("want no refusal for the ordinary non-colliding case, got: %v", err)
	}
	byRole := map[string]ScaffoldLaneItem{}
	for _, l := range plan.Lanes {
		byRole[l.Role] = l
	}
	if byRole["code-review"].Lane != "code-review" || byRole["critique"].Lane != "critique" {
		t.Errorf("want distinct default lanes, got code-review=%q critique=%q",
			byRole["code-review"].Lane, byRole["critique"].Lane)
	}
}

// TestComputeScaffoldPlan_MultiRoleBindingCannotSatisfyBothLayersFromOneReviewer is
// the ADVERSARIAL question item 4's spec demands an answer to: can the multi-role
// lane binding be used to route a cr-role agent's verdict into the crit lane and
// thereby satisfy both layers from one reviewer? Answer: NO — gate.Record/
// GateVerdict's layer-independence refusal is keyed on the recording agent's
// CANONICAL ROLE (internal/gate.CanonicalRole applied to the `by` string), never on
// which queue lane a ticket physically sits in. Scaffold's lane binding only
// controls ROUTING (which inbox a ticket lands in) — it has no path to the `by`
// value gate.Record checks, so no lane topology scaffold can produce changes that
// verdict. This test proves the guard scaffold DOES enforce (refusing cr+crit
// sharing a lane, see the sibling test above) is defense-in-depth, not the only
// thing standing between a reviewer and defeating the gate — the real enforcement
// lives in internal/gate and is untouched by anything in this file.
func TestComputeScaffoldPlan_MultiRoleBindingCannotSatisfyBothLayersFromOneReviewer(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	// The only multi-role binding scaffold will ever produce for critique is paired
	// with a NON-independence role (outcome-verifier) — confirm that binding alone,
	// plus a single `by` identity acting as both roles, still cannot satisfy BOTH
	// required gate layers: outcome-verifier is not even in gate.RequiredLayers, and
	// critically, gate.Record's independence check runs on `by`'s canonical role,
	// never on lane membership — so nothing scaffold plans here can touch it.
	roster := []RosterSpec{
		{Type: "critique", Count: 1, Lane: "crit-ov"},
		{Type: "outcome-verifier", Count: 1, Lane: "crit-ov"},
	}
	plan, err := ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		t.Fatalf("ComputeScaffoldPlan: %v", err)
	}
	for _, l := range plan.Lanes {
		if l.Role == "code-review" {
			t.Fatal("code-review must never appear bound into a shared lane via this roster")
		}
	}
	// Nothing about ScaffoldLaneItem/ROLES routing carries or influences a `by`
	// value at all — that field simply doesn't exist here, which is the point: the
	// independence guard lives entirely in internal/gate, untouched by this package.
}

// The printed one-liner is what a new adopter runs for their first task, so it has to be a command
// that works: a ticket that does not exist yet cannot be sent to review, and a reviewer lane is
// not where a first task goes.
func TestFirstEnqueueHint_IsTheCommandThatCreatesAndAssignsAFirstTask(t *testing.T) {
	plan := ScaffoldPlan{QueueRoot: "/q", Lanes: []ScaffoldLaneItem{
		{Role: "code-review", Lane: "code-review"},
		{Role: "developer", Lane: "dev-1"},
		{Role: "orchestrator", Lane: "orchestrator"},
	}}
	hint := FirstEnqueueHint(plan)
	for _, want := range []string{"leadkit task create", "--type developer", "--assign developer", "--unparented", "<what to do>"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint lacks %q: %s", want, hint)
		}
	}
	if strings.Contains(hint, "`leadkit review send --task") {
		t.Errorf("the hint must not offer review send as the way to enqueue a first task: %s", hint)
	}
	if !strings.Contains(hint, "FINISHED work") {
		t.Errorf("the hint should say what review send is for, so it is not reached for by mistake: %s", hint)
	}
}

func TestFirstEnqueueHint_NeverSendsAFirstTaskToAFixedRoleLane(t *testing.T) {
	for _, fixed := range []string{"code-review", "critique", "orchestrator", "finalise"} {
		only := ScaffoldPlan{Lanes: []ScaffoldLaneItem{{Role: fixed, Lane: fixed}}}
		if hint := FirstEnqueueHint(only); hint != "" {
			t.Errorf("with only the %s lane there is no work role, so no hint: %s", fixed, hint)
		}
	}
	if FirstEnqueueHint(ScaffoldPlan{}) != "" {
		t.Error("no lanes, no hint")
	}
}

func TestScaffoldPlan_ChangeCountIsLanesPlusAMissingStore(t *testing.T) {
	p := ScaffoldPlan{Lanes: []ScaffoldLaneItem{{Role: "a", RoleRowAdd: true}, {Role: "b", RoleRowAdd: false}, {Role: "c", RoleRowAdd: true}}}
	if got := p.ChangeCount(); got != 2 {
		t.Errorf("two lanes gain a row: got %d", got)
	}
	p.StoreMissing = true
	if got := p.ChangeCount(); got != 3 {
		t.Errorf("a missing store is one more change: got %d", got)
	}
}

// doctor reports a missing task store CRITICAL and `task create` cannot write without it, so the
// command that builds the queue builds the store too. It shows in the plan, is created on apply,
// and a store that already exists is left alone.
func TestScaffold_CreatesTheTaskStoreTheQueueLinksInto(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	store := filepath.Join(root, "tasks")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	plan, err := ComputeScaffoldPlan([]RosterSpec{{Type: "developer", Count: 1}}, queueRoot, rolesPath)
	if err != nil {
		t.Fatal(err)
	}
	plan.PlanStore(store)
	if !plan.StoreMissing || !plan.AnyChanged() {
		t.Fatalf("a missing store must be planned: %+v", plan)
	}
	if out := FormatScaffoldPlan(plan); !strings.Contains(out, "+ mkdir "+store) || !strings.Contains(out, "task store") {
		t.Errorf("the plan must show the store:\n%s", out)
	}
	if _, err := os.Stat(store); err == nil {
		t.Fatal("planning created the store")
	}
	res, err := ApplyScaffoldPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(store); err != nil || !fi.IsDir() {
		t.Fatalf("apply did not create the store: %v", err)
	}
	found := false
	for _, d := range res.CreatedDirs {
		if d == store {
			found = true
		}
	}
	if !found {
		t.Errorf("the store must be reported as created: %v", res.CreatedDirs)
	}

	again, _ := ComputeScaffoldPlan([]RosterSpec{{Type: "developer", Count: 1}}, queueRoot, rolesPath)
	again.PlanStore(store)
	if again.StoreMissing || again.AnyChanged() {
		t.Errorf("an existing store and a built queue is a no-op: %+v", again)
	}
}

func TestScaffold_AStoreOnlyChangeIsStillAChange(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	plan, _ := ComputeScaffoldPlan([]RosterSpec{{Type: "developer", Count: 1}}, queueRoot, filepath.Join(queueRoot, "ROLES"))
	if _, err := ApplyScaffoldPlan(plan); err != nil {
		t.Fatal(err)
	}
	next, _ := ComputeScaffoldPlan([]RosterSpec{{Type: "developer", Count: 1}}, queueRoot, filepath.Join(queueRoot, "ROLES"))
	next.PlanStore(filepath.Join(root, "tasks"))
	if next.AnyChanged() != true {
		t.Error("a built queue with no store still has something to do")
	}
	var empty ScaffoldPlan
	empty.PlanStore("")
	if empty.StoreMissing {
		t.Error("an empty store path means 'not asked about', never 'missing'")
	}
}

// The independence refusal is not special to the built-in pair: any two review layers sharing a
// queue-consumer lane are refused, and a declared review role on its own lane is accepted.
func TestComputeScaffoldPlan_RefusesAnyTwoReviewLayersSharingALane(t *testing.T) {
	for name, roster := range map[string][]RosterSpec{
		"a declared layer onto code-review's lane": {{Type: "security", Count: 1, ReviewLayer: true, Lane: "code-review"}},
		"a declared layer onto critique's lane":    {{Type: "security", Count: 1, ReviewLayer: true, Lane: "critique"}},
		"two declared layers onto one lane": {
			{Type: "security", Count: 1, ReviewLayer: true, Lane: "shared"},
			{Type: "perf", Count: 1, ReviewLayer: true, Lane: "shared"},
		},
	} {
		root := t.TempDir()
		queueRoot := filepath.Join(root, "queue")
		_, err := ComputeScaffoldPlan(roster, queueRoot, filepath.Join(queueRoot, "ROLES"))
		if err == nil || !strings.Contains(err.Error(), "layer independence") {
			t.Errorf("%s: want a layer-independence refusal, got %v", name, err)
		}
		if _, statErr := os.Stat(queueRoot); statErr == nil {
			t.Errorf("%s: a refused plan must never have touched disk", name)
		}
	}
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	if _, err := ComputeScaffoldPlan([]RosterSpec{{Type: "security", Count: 1, ReviewLayer: true}}, queueRoot, filepath.Join(queueRoot, "ROLES")); err != nil {
		t.Errorf("a declared review role on its own lane must be accepted: %v", err)
	}
	// Without the declaration the same lane share is the ordinary lightweight-team binding.
	if _, err := ComputeScaffoldPlan([]RosterSpec{{Type: "security", Count: 1, Lane: "code-review"}}, queueRoot, filepath.Join(queueRoot, "ROLES")); err != nil {
		t.Errorf("a work role sharing a lane with a review layer is not this rule's business: %v", err)
	}
}
