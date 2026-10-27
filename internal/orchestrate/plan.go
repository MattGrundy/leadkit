// Package orchestrate is `leadkit orchestrate plan|apply` — the DETERMINISTIC
// planning core the orchestrator agent (docs/OPERATING-MODEL.md) calls on each wake
// instead of re-deriving a plan by model judgement. leadkit cannot call ListAgents
// itself, so the agent captures the live roster and pipes it in (--roster); Plan is
// then a pure function over that roster + disk state — no model, no I/O, no
// randomness. Every rule below is explicit and independently testable; see plan_test.go.
//
// apply.go is the ONLY part of this package that mutates anything, and only for
// route + requeue (via queue.Assign/queue.Move) — never escalate/triage (the lead's
// call) and never spawns an agent (the orchestrator agent's own job, not leadkit's).
package orchestrate

import (
	"fmt"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/roles"
)

// RosterEntry is one live agent, as the orchestrator agent captured it from
// ListAgents and piped in via --roster.
type RosterEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Status   string `json:"status"` // "idle" | "busy"
	LastSeen string `json:"last_seen,omitempty"`
}

// Task is one backlog or in-flight ticket, ALREADY reduced to what Plan needs to
// decide — see BuildPlanInput (build.go) for how these are actually resolved from
// disk (deps/type/escalation are genuinely messy real-world inference; keeping Plan
// itself a pure function over pre-resolved facts is what makes it "no model
// judgement inside, no I/O" testable in isolation).
type Task struct {
	ID       string
	Priority string // "P0".."P3"; "" = unset, treated as lowest
	DepsMet  bool   // blockedBy is empty, or every blocking id is already done
	Type     string // the agent TYPE this task needs; "" = undeterminable -> triage
	Lane     string // "" = unrouted backlog; else the placed queue LANE holding it (in-flight)
	Attempts int    // requeue attempts so far, for in-flight orphan tracking
	Status   string // normalized store status (see build.go normalizeStatus); "" when unknown

	// EscalateReason, when non-empty, sends this task straight to Escalate and skips
	// every other rule — a user-decision marker, an ambiguous/malformed priority, or
	// anything else the disk-reading layer decided a human must look at.
	EscalateReason string
}

// PlanInput is everything Plan needs, already resolved.
type PlanInput struct {
	Roster         []RosterEntry
	Backlog        []Task            // Lane == "" (not yet placed in any queue lane)
	InFlight       []Task            // Lane != "" (currently placed/claimed)
	LaneAliases    map[string]string // project lane_aliases, for the owner/lane->role reconciler (Fix 1)
	MaxAttempts    int               // requeue attempts before an orphan is treated as poison (default 3 if 0)
	BurnHold       bool              // true = burn says HOLD (at/over the workstream cap)
	BurnHoldReason string
}

// RequeueItem is an in-flight task whose owner is no longer live.
type RequeueItem struct {
	ID       string `json:"id"`
	From     string `json:"from"` // the dead lane/agent name it's being requeued off of
	Attempts int    `json:"attempts"`
}

// RouteItem is a backlog task assigned to a live idle agent.
type RouteItem struct {
	TaskID string `json:"task_id"`
	Agent  string `json:"agent"`
	Type   string `json:"type"`
}

// CapacityItem is a backlog task whose type has no idle agent live right now.
type CapacityItem struct {
	TaskID string `json:"task_id"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// EscalateItem is anything that needs the lead, with why.
type EscalateItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// TriageItem is a ready task with no determinable type/owner — leadkit refuses to
// guess; a human sorts it.
type TriageItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// PlanResult is the full deterministic plan — read-only until `orchestrate apply`
// (or the orchestrator agent, for escalate/triage) acts on it.
type PlanResult struct {
	Requeue    []RequeueItem  `json:"requeue"`
	Route      []RouteItem    `json:"route"`
	Capacity   []CapacityItem `json:"capacity"`
	Escalate   []EscalateItem `json:"escalate"`
	Triage     []TriageItem   `json:"triage"`
	Hold       bool           `json:"hold"`
	HoldReason string         `json:"hold_reason,omitempty"`
}

const defaultMaxAttempts = 3

// Plan computes a PlanResult from input — pure, deterministic, no I/O. Rule order
// (each independently testable, see plan_test.go):
//  1. In-flight tasks: an EscalateReason always wins (poison/decision markers set by
//     the disk layer); a terminal-or-review status is NEVER touched (belt-and-braces
//     with build.go's own skip); else the ticket's placed lane/owner is reconciled
//     to a canonical ROLE (roles.CanonicalLane + the -agent fold + a stripped
//     leading `owner-`) and requeued ONLY when that role has NO live agent — a
//     genuinely dead role, not a mere agent-instance name mismatch. Past MaxAttempts
//     a genuinely-dead role's ticket escalates as poison instead of requeuing again.
//  2. Backlog tasks, in Priority order (P0 first, unset last): EscalateReason wins;
//     deps not yet met is silently skipped (not actionable this cycle, not an
//     error); an undeterminable Type goes to Triage; while BurnHold is true, NO new
//     routing happens at all (the task simply stays backlog for the next cycle); a
//     Type with no live idle agent right now is a Capacity signal; otherwise the
//     task is Routed to the first available idle agent of that Type (consumed, so a
//     second task needing the same type doesn't double-book it).
func Plan(input PlanInput) PlanResult {
	res := PlanResult{Hold: input.BurnHold, HoldReason: input.BurnHoldReason}

	maxAttempts := input.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	// Reconcile every heterogeneous owner/lane name to a canonical ROLE via the SAME
	// resolver assign/review-send use (roster as `known` + the -agent fold +
	// lane_aliases), so the live-check is role-keyed, not agent-instance-name-keyed.
	var rosterTypes []string
	for _, r := range input.Roster {
		if r.Type != "" {
			rosterTypes = append(rosterTypes, r.Type)
		}
	}
	known := roles.KnownRoles(rosterTypes)

	// roleLive: a role is live if ANY roster agent's Type canonicalizes to it (idle
	// OR busy) — an in-flight ticket of that role is being worked, not orphaned.
	roleLive := map[string]bool{}
	idleByRole := map[string][]RosterEntry{}
	for _, r := range input.Roster {
		role, ok := reconcileRole(r.Type, known, input.LaneAliases)
		if ok {
			roleLive[role] = true
		}
		// Index idle agents by their RECONCILED role (the -agent fold + roster +
		// lane_aliases + owner- strip), so two roster entries that fold to the same
		// role (e.g. "qa" and "qa-agent") collapse under one key and the backlog
		// match is role-keyed, not raw-Type-keyed.
		if r.Status == "idle" && ok {
			idleByRole[role] = append(idleByRole[role], r)
		}
	}

	for _, t := range input.InFlight {
		if t.EscalateReason != "" {
			res.Escalate = append(res.Escalate, EscalateItem{ID: t.ID, Reason: t.EscalateReason})
			continue
		}
		if isTerminalOrReview(t.Status) {
			continue // belt-and-braces: a done/reviewed ticket is never requeued (Fix 2)
		}
		if role, ok := reconcileRole(t.Lane, known, input.LaneAliases); ok && roleLive[role] {
			continue // the reconciled role has a live agent — being worked, nothing to do
		}
		// A genuinely dead role: no live agent reconciles to this ticket's lane/owner.
		if t.Attempts >= maxAttempts {
			res.Escalate = append(res.Escalate, EscalateItem{
				ID:     t.ID,
				Reason: fmt.Sprintf("poison — %d requeue attempt(s) exceeded (dead role, lane %q)", t.Attempts, t.Lane),
			})
			continue
		}
		res.Requeue = append(res.Requeue, RequeueItem{ID: t.ID, From: t.Lane, Attempts: t.Attempts + 1})
	}

	backlog := append([]Task(nil), input.Backlog...)
	sort.SliceStable(backlog, func(i, j int) bool {
		return priorityRank(backlog[i].Priority) < priorityRank(backlog[j].Priority)
	})

	for _, t := range backlog {
		if t.EscalateReason != "" {
			res.Escalate = append(res.Escalate, EscalateItem{ID: t.ID, Reason: t.EscalateReason})
			continue
		}
		if isTerminalOrReview(t.Status) {
			continue // belt-and-braces: a done/reviewed ticket is never routed (Fix 2)
		}
		if !t.DepsMet {
			continue // not ready yet — no action this cycle
		}
		if t.Type == "" {
			res.Triage = append(res.Triage, TriageItem{ID: t.ID, Reason: "no determinable type/owner"})
			continue
		}
		if input.BurnHold {
			continue // hold: never start a new workstream; task stays backlog
		}
		// Reconcile the ticket's Type to a canonical ROLE before the idle lookup, the
		// same fold the roster is indexed under — so a `<role>-agent`-owned backlog
		// ticket (e.g. "qa-agent") finds the live `qa` agent and ROUTES; only a role
		// with genuinely no idle agent falls to Capacity, named by the reconciled role.
		// A t.Type that reconciles to no known role keeps the raw name (unchanged
		// undeterminable path).
		role := t.Type
		if r, ok := reconcileRole(t.Type, known, input.LaneAliases); ok {
			role = r
		}
		avail := idleByRole[role]
		if len(avail) == 0 {
			res.Capacity = append(res.Capacity, CapacityItem{
				TaskID: t.ID, Type: role, Reason: fmt.Sprintf("spawn-or-escalate: need more %s", role),
			})
			continue
		}
		res.Route = append(res.Route, RouteItem{TaskID: t.ID, Agent: avail[0].Name, Type: role})
		idleByRole[role] = avail[1:]
	}

	return res
}

// FormatPlanResult renders res as `orchestrate plan`'s human-readable output.
func FormatPlanResult(res PlanResult) string {
	var b strings.Builder
	if res.Hold {
		fmt.Fprintf(&b, "HOLD: %s — no new routing this cycle\n", res.HoldReason)
	}
	if len(res.Requeue) > 0 {
		fmt.Fprintf(&b, "== requeue (%d) ==\n", len(res.Requeue))
		for _, r := range res.Requeue {
			fmt.Fprintf(&b, "  %s: dead owner %q -> backlog (attempt %d)\n", r.ID, r.From, r.Attempts)
		}
	}
	if len(res.Route) > 0 {
		fmt.Fprintf(&b, "== route (%d) ==\n", len(res.Route))
		for _, r := range res.Route {
			fmt.Fprintf(&b, "  %s -> %s (type %s)\n", r.TaskID, r.Agent, r.Type)
		}
	}
	if len(res.Capacity) > 0 {
		fmt.Fprintf(&b, "== capacity (%d) ==\n", len(res.Capacity))
		for _, c := range res.Capacity {
			fmt.Fprintf(&b, "  %s: %s\n", c.TaskID, c.Reason)
		}
	}
	if len(res.Escalate) > 0 {
		fmt.Fprintf(&b, "== escalate (%d) ==\n", len(res.Escalate))
		for _, e := range res.Escalate {
			fmt.Fprintf(&b, "  %s: %s\n", e.ID, e.Reason)
		}
	}
	if len(res.Triage) > 0 {
		fmt.Fprintf(&b, "== triage (%d) ==\n", len(res.Triage))
		for _, t := range res.Triage {
			fmt.Fprintf(&b, "  %s: %s\n", t.ID, t.Reason)
		}
	}
	fmt.Fprintf(&b, "summary: hold=%v requeue=%d route=%d capacity=%d escalate=%d triage=%d\n",
		res.Hold, len(res.Requeue), len(res.Route), len(res.Capacity), len(res.Escalate), len(res.Triage))
	return b.String()
}

// reconcileRole resolves a heterogeneous owner/lane name (a placed queue lane like
// "devops-agent" or bare "critique", an owner-instance like "owner-devops", or a
// roster agent Type) to its canonical ROLE via the SAME resolver assign/review-send
// use: strip a leading "owner-" (owner-devops -> devops), then roles.CanonicalLane
// with the roster as `known` (+ the always-on -agent fold) + the project
// lane_aliases. ok is false for an ephemeral/unmappable name with no canonical-role
// home — the caller then treats the ticket's role as not-live (genuinely dead).
func reconcileRole(name string, known []string, aliases map[string]string) (string, bool) {
	n := strings.TrimPrefix(strings.TrimSpace(name), "owner-")
	if n == "" {
		return "", false
	}
	return roles.CanonicalLane(n, known, aliases)
}

// priorityRank orders P0 first, P3 last, unset/unrecognized lowest of all — a
// stable sort, so equal-priority tasks keep their input (e.g. id) order.
func priorityRank(p string) int {
	switch p {
	case "P0":
		return 0
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	default:
		return 99
	}
}
