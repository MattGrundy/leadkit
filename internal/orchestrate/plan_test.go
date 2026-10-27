package orchestrate

import "testing"

func TestPlan_OrphanDeadOwnerRequeues(t *testing.T) {
	input := PlanInput{
		// devops is the only live role; the in-flight ticket's lane cr-2 reconciles to
		// code-review, which has NO live agent — a genuinely dead role, so it requeues.
		Roster:   []RosterEntry{{Name: "dv-1", Type: "devops", Status: "idle"}},
		InFlight: []Task{{ID: "1", Lane: "cr-2", Attempts: 0}},
	}
	res := Plan(input)
	if len(res.Requeue) != 1 || res.Requeue[0].ID != "1" || res.Requeue[0].From != "cr-2" || res.Requeue[0].Attempts != 1 {
		t.Fatalf("Requeue = %+v, want one entry for task 1 from cr-2 attempt 1", res.Requeue)
	}
	if len(res.Escalate) != 0 {
		t.Errorf("want no escalation below the attempt threshold, got %+v", res.Escalate)
	}
}

func TestPlan_OrphanPastMaxAttemptsEscalatesAsPoisonInstead(t *testing.T) {
	input := PlanInput{
		MaxAttempts: 3,
		// code-review (the ticket's reconciled role) has no live agent — genuinely dead.
		Roster:   []RosterEntry{{Name: "dv-1", Type: "devops", Status: "idle"}},
		InFlight: []Task{{ID: "1", Lane: "cr-2", Attempts: 3}},
	}
	res := Plan(input)
	if len(res.Requeue) != 0 {
		t.Errorf("want no more requeue attempts past the threshold, got %+v", res.Requeue)
	}
	if len(res.Escalate) != 1 || res.Escalate[0].ID != "1" {
		t.Fatalf("want task 1 escalated as poison, got %+v", res.Escalate)
	}
}

func TestPlan_AliveOwnerInFlightIsLeftAlone(t *testing.T) {
	input := PlanInput{
		Roster:   []RosterEntry{{Name: "cr-2", Type: "code-review", Status: "busy"}},
		InFlight: []Task{{ID: "1", Lane: "cr-2", Attempts: 0}},
	}
	res := Plan(input)
	if len(res.Requeue) != 0 || len(res.Escalate) != 0 {
		t.Errorf("an alive owner's in-flight task must not be touched, got requeue=%+v escalate=%+v", res.Requeue, res.Escalate)
	}
}

func TestPlan_ReadyDepsMetLiveIdle_Routes(t *testing.T) {
	input := PlanInput{
		Roster:  []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "idle"}},
		Backlog: []Task{{ID: "10", Priority: "P1", DepsMet: true, Type: "code-review"}},
	}
	res := Plan(input)
	if len(res.Route) != 1 || res.Route[0].TaskID != "10" || res.Route[0].Agent != "cr-1" || res.Route[0].Type != "code-review" {
		t.Fatalf("want task 10 routed to cr-1, got %+v", res.Route)
	}
	if len(res.Capacity) != 0 || len(res.Triage) != 0 {
		t.Errorf("clean route should produce no capacity/triage, got %+v %+v", res.Capacity, res.Triage)
	}
}

func TestPlan_DepsNotMet_NoActionThisCycle(t *testing.T) {
	input := PlanInput{
		Roster:  []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "idle"}},
		Backlog: []Task{{ID: "10", DepsMet: false, Type: "code-review"}},
	}
	res := Plan(input)
	if len(res.Route)+len(res.Capacity)+len(res.Escalate)+len(res.Triage) != 0 {
		t.Errorf("a not-ready task should produce no action at all this cycle, got %+v", res)
	}
}

func TestPlan_BurnHold_NoRouteJustHold(t *testing.T) {
	input := PlanInput{
		Roster:         []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "idle"}},
		Backlog:        []Task{{ID: "10", DepsMet: true, Type: "code-review"}},
		BurnHold:       true,
		BurnHoldReason: "at cap",
	}
	res := Plan(input)
	if !res.Hold || res.HoldReason != "at cap" {
		t.Errorf("want Hold=true reason=\"at cap\", got hold=%v reason=%q", res.Hold, res.HoldReason)
	}
	if len(res.Route) != 0 {
		t.Errorf("hold must suppress routing entirely, got %+v", res.Route)
	}
	if len(res.Capacity) != 0 {
		t.Errorf("hold should not ALSO produce a redundant capacity signal, got %+v", res.Capacity)
	}
}

func TestPlan_NoIdleOfType_CapacityEscalate(t *testing.T) {
	input := PlanInput{
		Roster:  []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "busy"}}, // busy, not idle
		Backlog: []Task{{ID: "10", DepsMet: true, Type: "code-review"}},
	}
	res := Plan(input)
	if len(res.Route) != 0 {
		t.Errorf("no idle agent of the needed type — must not route, got %+v", res.Route)
	}
	if len(res.Capacity) != 1 || res.Capacity[0].TaskID != "10" || res.Capacity[0].Type != "code-review" {
		t.Fatalf("want a capacity escalation for task 10 needing code-review, got %+v", res.Capacity)
	}
}

func TestPlan_UnownedReady_Triage(t *testing.T) {
	input := PlanInput{
		Backlog: []Task{{ID: "10", DepsMet: true, Type: ""}},
	}
	res := Plan(input)
	if len(res.Triage) != 1 || res.Triage[0].ID != "10" {
		t.Fatalf("want task 10 in Triage (no determinable type), got %+v", res.Triage)
	}
	if len(res.Route)+len(res.Capacity)+len(res.Escalate) != 0 {
		t.Errorf("triage should be the ONLY outcome for an unowned ready task, got %+v", res)
	}
}

func TestPlan_EscalateReasonAlwaysWins(t *testing.T) {
	input := PlanInput{
		Backlog:  []Task{{ID: "10", DepsMet: true, Type: "code-review", EscalateReason: "ambiguous priority"}},
		InFlight: []Task{{ID: "20", Lane: "cr-1", EscalateReason: "poison marker"}},
		Roster:   []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "idle"}},
	}
	res := Plan(input)
	if len(res.Escalate) != 2 {
		t.Fatalf("want both tasks escalated, got %+v", res.Escalate)
	}
	if len(res.Route) != 0 || len(res.Requeue) != 0 {
		t.Errorf("EscalateReason must pre-empt route/requeue entirely, got route=%+v requeue=%+v", res.Route, res.Requeue)
	}
}

// TestPlan_InFlightRoleLive_NotRequeued_OwnerReconciliation is the 142-false-requeue
// regression (Fix 1): heterogeneous placed-lane/owner spellings (the -agent variant
// lane, an owner-instance name, a bare gate role) all reconcile to a LIVE role, so
// NONE is requeued. Before owner->role reconciliation the live-check compared these
// against agent NAMES and requeued every one as a "dead owner".
func TestPlan_InFlightRoleLive_NotRequeued_OwnerReconciliation(t *testing.T) {
	roster := []RosterEntry{
		{Name: "owner-devops", Type: "devops", Status: "busy"},
		{Name: "crit-1", Type: "critique", Status: "busy"},
	}
	for _, lane := range []string{"devops-agent", "owner-devops", "critique"} {
		input := PlanInput{
			Roster:   roster,
			InFlight: []Task{{ID: "1", Lane: lane, Attempts: 0}},
		}
		res := Plan(input)
		if len(res.Requeue) != 0 || len(res.Escalate) != 0 {
			t.Errorf("lane %q reconciles to a LIVE role — must NOT requeue/escalate, got requeue=%+v escalate=%+v",
				lane, res.Requeue, res.Escalate)
		}
	}
}

// TestPlan_BeltAndBraces_TerminalOrReviewNeverRequeuedOrRouted is Fix 2's second
// layer inside Plan: even if a terminal/review ticket reaches Plan (build.go's skip
// bypassed), Plan never requeues an in-flight review ticket (whose role is even
// dead) nor routes a completed backlog ticket (whose type has a live idle agent).
func TestPlan_BeltAndBraces_TerminalOrReviewNeverRequeuedOrRouted(t *testing.T) {
	input := PlanInput{
		Roster:   []RosterEntry{{Name: "dv-1", Type: "devops", Status: "idle"}},
		InFlight: []Task{{ID: "1", Lane: "cr-9", Status: "in_review"}}, // dead role, but in review
		Backlog:  []Task{{ID: "2", Status: "completed", DepsMet: true, Type: "devops"}},
	}
	res := Plan(input)
	if len(res.Requeue) != 0 || len(res.Escalate) != 0 {
		t.Errorf("a review-status in-flight ticket must never be requeued/escalated, got requeue=%+v escalate=%+v", res.Requeue, res.Escalate)
	}
	if len(res.Route) != 0 {
		t.Errorf("a terminal-status backlog ticket must never be routed, got %+v", res.Route)
	}
}

// TestPlan_AllInFlightRolesLive_ZeroRequeues is the safe-to-apply property: over a
// mixed in-flight set whose every lane reconciles to a live role, Plan proposes ZERO
// requeues — nothing destructive to apply.
func TestPlan_AllInFlightRolesLive_ZeroRequeues(t *testing.T) {
	input := PlanInput{
		Roster: []RosterEntry{
			{Name: "dv-1", Type: "devops", Status: "busy"},
			{Name: "crit-1", Type: "critique", Status: "busy"},
			{Name: "cr-1", Type: "code-review", Status: "idle"},
		},
		InFlight: []Task{
			{ID: "1", Lane: "devops-agent"},
			{ID: "2", Lane: "owner-devops"},
			{ID: "3", Lane: "critique"},
			{ID: "4", Lane: "cr-2"},
		},
	}
	res := Plan(input)
	if len(res.Requeue) != 0 || len(res.Escalate) != 0 {
		t.Fatalf("every in-flight role is live — a safe plan requeues nothing, got requeue=%+v escalate=%+v", res.Requeue, res.Escalate)
	}
}

// TestPlan_BacklogAgentOwnerRoutesToLiveRole is the #123 residual: a backlog ticket
// typed with the `<role>-agent` owner-instance form ("qa-agent") reconciles to the
// live `qa` role and ROUTES to it — not a phantom "need more qa-agent" Capacity.
// Fails before the routing/capacity fold, passes after.
func TestPlan_BacklogAgentOwnerRoutesToLiveRole(t *testing.T) {
	_ = t.TempDir()
	input := PlanInput{
		Roster: []RosterEntry{
			{Name: "qa-1", Type: "qa", Status: "idle"},
			{Name: "fe-1", Type: "frontend", Status: "idle"},
		},
		Backlog: []Task{{ID: "10", DepsMet: true, Type: "qa-agent"}},
	}
	res := Plan(input)
	if len(res.Route) != 1 || res.Route[0].TaskID != "10" || res.Route[0].Agent != "qa-1" || res.Route[0].Type != "qa" {
		t.Fatalf("want task 10 (qa-agent) routed to live qa agent qa-1 as role qa, got %+v", res.Route)
	}
	if len(res.Capacity) != 0 {
		t.Errorf("a qa-agent ticket with a live qa agent must not fall to Capacity, got %+v", res.Capacity)
	}
}

// TestPlan_CapacityNamesReconciledRole: a role with NO idle agent produces a Capacity
// signal naming the RECONCILED role ("need more qa"), not the phantom "qa-agent" —
// so capacity throttling counts against the real owner.
func TestPlan_CapacityNamesReconciledRole(t *testing.T) {
	_ = t.TempDir()
	input := PlanInput{
		// qa is live but BUSY (no idle); frontend idle but irrelevant.
		Roster: []RosterEntry{
			{Name: "qa-1", Type: "qa", Status: "busy"},
			{Name: "fe-1", Type: "frontend", Status: "idle"},
		},
		Backlog: []Task{{ID: "10", DepsMet: true, Type: "qa-agent"}},
	}
	res := Plan(input)
	if len(res.Route) != 0 {
		t.Errorf("no idle qa agent — must not route, got %+v", res.Route)
	}
	if len(res.Capacity) != 1 || res.Capacity[0].TaskID != "10" || res.Capacity[0].Type != "qa" {
		t.Fatalf("want a capacity signal for task 10 naming the reconciled role qa, got %+v", res.Capacity)
	}
	if want := "spawn-or-escalate: need more qa"; res.Capacity[0].Reason != want {
		t.Fatalf("capacity reason = %q, want %q (reconciled role, not qa-agent)", res.Capacity[0].Reason, want)
	}
}

// TestPlan_BacklogBareRoleStillRoutes: the pre-fold path is unregressed — a bare-role
// ticket (type "qa") routes to the live qa agent, and an "owner-qa" form folds too.
func TestPlan_BacklogBareRoleStillRoutes(t *testing.T) {
	_ = t.TempDir()
	roster := []RosterEntry{
		{Name: "qa-1", Type: "qa", Status: "idle"},
		{Name: "fe-1", Type: "frontend", Status: "idle"},
	}
	for _, typ := range []string{"qa", "owner-qa"} {
		input := PlanInput{Roster: roster, Backlog: []Task{{ID: "10", DepsMet: true, Type: typ}}}
		res := Plan(input)
		if len(res.Route) != 1 || res.Route[0].TaskID != "10" || res.Route[0].Agent != "qa-1" || res.Route[0].Type != "qa" {
			t.Fatalf("type %q: want task 10 routed to qa-1 as role qa, got %+v", typ, res.Route)
		}
		if len(res.Capacity) != 0 {
			t.Errorf("type %q: no capacity gap expected, got %+v", typ, res.Capacity)
		}
	}
}

// TestPlan_BacklogUndeterminableTypeUnchanged: a type that reconciles to NO known
// role keeps today's behavior — a Capacity signal under the raw name (not folded).
func TestPlan_BacklogUndeterminableTypeUnchanged(t *testing.T) {
	_ = t.TempDir()
	input := PlanInput{
		Roster:  []RosterEntry{{Name: "qa-1", Type: "qa", Status: "idle"}},
		Backlog: []Task{{ID: "10", DepsMet: true, Type: "wombat"}},
	}
	res := Plan(input)
	if len(res.Route) != 0 {
		t.Errorf("an unknown type must not route to any live agent, got %+v", res.Route)
	}
	if len(res.Capacity) != 1 || res.Capacity[0].TaskID != "10" || res.Capacity[0].Type != "wombat" {
		t.Fatalf("want a capacity signal naming the raw unknown type wombat, got %+v", res.Capacity)
	}
}

// TestPlan_IdleByRoleCollapsesSameReconciledRole: two roster idle entries whose Types
// reconcile to the SAME role ("qa" and the instance spelling "qa-1", both folding to
// qa) collapse under ONE "qa" key in idleByRole, so two qa-serving tickets — a bare
// "qa" and the "qa-agent" owner form — both route (one to each idle instance) with no
// phantom capacity gap. (A roster Type literally "qa-agent" would be its own known
// role and NOT fold — the -agent fold only rescues a ticket/lane name that is not
// already a known role — so the collapse is shown with folding spellings.)
func TestPlan_IdleByRoleCollapsesSameReconciledRole(t *testing.T) {
	_ = t.TempDir()
	input := PlanInput{
		Roster: []RosterEntry{
			{Name: "qa-1", Type: "qa", Status: "idle"},
			{Name: "qa-2", Type: "qa-1", Status: "idle"}, // instance spelling → folds to qa
		},
		Backlog: []Task{
			{ID: "10", DepsMet: true, Type: "qa"},
			{ID: "11", DepsMet: true, Type: "qa-agent"},
		},
	}
	res := Plan(input)
	if len(res.Route) != 2 {
		t.Fatalf("both qa tickets should route to the two collapsed qa idle agents, got %+v (capacity=%+v)", res.Route, res.Capacity)
	}
	if len(res.Capacity) != 0 {
		t.Errorf("two idle qa agents collapsed under one key cover two tickets — no capacity gap, got %+v", res.Capacity)
	}
	for _, r := range res.Route {
		if r.Type != "qa" {
			t.Errorf("route %+v: want reconciled role qa", r)
		}
	}
}

func TestPlan_RoutesInPriorityOrder(t *testing.T) {
	input := PlanInput{
		Roster: []RosterEntry{{Name: "cr-1", Type: "code-review", Status: "idle"}}, // only one idle slot
		Backlog: []Task{
			{ID: "low", Priority: "P3", DepsMet: true, Type: "code-review"},
			{ID: "high", Priority: "P0", DepsMet: true, Type: "code-review"},
		},
	}
	res := Plan(input)
	if len(res.Route) != 1 || res.Route[0].TaskID != "high" {
		t.Fatalf("want the P0 task routed first (only 1 idle slot), got %+v", res.Route)
	}
	if len(res.Capacity) != 1 || res.Capacity[0].TaskID != "low" {
		t.Fatalf("want the P3 task left as a capacity gap (no idle slots left), got %+v", res.Capacity)
	}
}
