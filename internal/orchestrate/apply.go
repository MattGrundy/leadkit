package orchestrate

// `leadkit orchestrate apply` executes ONLY a plan's Route + Requeue items, via
// queue.Assign/queue.Move — the same two mutating primitives every other queue
// pipeline move in this codebase uses. Capacity/Escalate/Triage/Hold are NEVER
// acted on here (the lead's/orchestrator agent's own job); no agent is ever spawned
// (leadkit is a CLI, spawning isn't its job either). Guarded the same way audit/init
// are: dry-run default, --apply --confirm required (cmd/leadkit's confirmGuard).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/queue"
)

// BacklogLane is the reserved queue lane name ApplyPlan requeues an orphaned
// in-flight ticket into. This is a DESIGN DECISION the spec doesn't name explicitly
// (it only says requeue "to ready" via queue.Move): a dedicated, non-agent holding
// lane keeps requeue expressible with the SAME queue.Move primitive route uses,
// keeps a requeued ticket visibly on disk for a human glancing at the queue (rather
// than deleted), and — matched by BuildPlanInput's overlay reader treating this lane
// as "not a real owner" — makes a requeued ticket read as ordinary backlog again on
// the next planning cycle, which is what makes a second `orchestrate apply` run
// idempotent (nothing left under a dead lane to requeue).
const BacklogLane = "backlog"

// ApplyResult is what ApplyPlan actually did (or, with dryRun, would do).
type ApplyResult struct {
	Routed   []RouteItem
	Requeued []RequeueItem
	Skipped  []SkipItem
	Errors   []string
}

// SkipItem records a plan item ApplyPlan intentionally did NOT act on because it
// was ALREADY HANDLED on the live queue — a moot requeue (its ticket moved out of
// From in the plan→apply window, so an owner or a prior apply already took it) or a
// Route whose ticket is already queued somewhere. A skip is the NORMAL live-queue /
// re-run-after-partial-progress state, never a failure: it goes here, NOT in
// Errors, which is what keeps `orchestrate apply` idempotent and safe to re-run on a
// queue whose owners are actively pulling.
type SkipItem struct {
	Kind   string // "requeue" | "route"
	ID     string // ticket id
	Reason string
}

// applyLogEntry is one orchestrate_apply_log JSONL record.
type applyLogEntry struct {
	TS     string `json:"ts"`
	Action string `json:"action"` // "route" | "requeue"
	TaskID string `json:"task_id"`
	Detail string `json:"detail"`
}

// ApplyPlan executes plan's Route + Requeue items. dryRun runs every resolution/
// refusal check unchanged (queue.Assign/queue.Move's own dryRun) but performs no
// symlink/rename — mutates nothing; logPath receives one JSONL record per item
// ACTUALLY applied (never during a dry run), best-effort (a log write failure is
// reported in Errors but never undoes or blocks the mutation it's recording).
func ApplyPlan(queueRoot, storeRoot, rolesPath string, rosterTypes []string, plan PlanResult, dryRun bool, logPath string) ApplyResult {
	var res ApplyResult
	var logged []applyLogEntry
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }

	// Role-keyed lanes (docs/build-tasks/role-keyed-lanes.md): route straight to
	// r.Type's own role lane. Previously this reverse-resolved r.Agent (the live
	// instance BuildPlanInput's capacity-aware picker chose) back to a role via
	// queue/ROLES, then handed THAT role to queue.Assign so it could re-resolve it
	// forward to a live instance — a round trip through ROLES that's now pure
	// overhead: r.Type already IS the canonical role (RouteItem.Type, set from the
	// task's own type at plan-build time), and queue.Assign no longer resolves a
	// role to a live instance at all — the role lane IS the destination.
	for _, r := range plan.Route {
		if _, err := queue.Assign(queueRoot, storeRoot, rolesPath, rosterTypes, r.TaskID, r.Type, dryRun); err != nil {
			// A ticket already queued somewhere is the "already handled" case (a
			// prior apply routed it, or an owner claimed it) — surface it as a SKIP,
			// not a fatal error, so re-running apply after partial progress is clean.
			var ae *queue.AssignError
			if errors.As(err, &ae) && ae.AlreadyQueued {
				res.Skipped = append(res.Skipped, SkipItem{Kind: "route", ID: r.TaskID, Reason: fmt.Sprintf("already queued — route to %s moot (live queue)", r.Type)})
				continue
			}
			res.Errors = append(res.Errors, fmt.Sprintf("route %s -> %s: %v", r.TaskID, r.Type, err))
			continue
		}
		res.Routed = append(res.Routed, r)
		if !dryRun {
			logged = append(logged, applyLogEntry{TS: now(), Action: "route", TaskID: r.TaskID, Detail: fmt.Sprintf("-> role %s (planner picked live agent %s)", r.Type, r.Agent)})
		}
	}

	for _, rq := range plan.Requeue {
		from, ok := findInLane(queueRoot, rq.From, rq.ID)
		if !ok {
			// The ticket is no longer under rq.From — an owner claimed/moved it (or a
			// prior apply handled it) in the plan→apply window. That's the NORMAL live-
			// queue state: the ticket already moved on, so the requeue is MOOT, not a
			// failure. Record it as a SKIP, never an Error — Errors is reserved for a
			// genuine queue.Move failure below.
			res.Skipped = append(res.Skipped, SkipItem{Kind: "requeue", ID: rq.ID, Reason: fmt.Sprintf("already moved out of %s — requeue moot (live queue)", rq.From)})
			continue
		}
		to := filepath.Join(queueRoot, BacklogLane, "pending", filepath.Base(from))
		if err := queue.Move(queueRoot, from, to, dryRun); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("requeue %s: %v", rq.ID, err))
			continue
		}
		res.Requeued = append(res.Requeued, rq)
		if !dryRun {
			logged = append(logged, applyLogEntry{TS: now(), Action: "requeue", TaskID: rq.ID, Detail: fmt.Sprintf("%s -> %s (attempt %d)", rq.From, BacklogLane, rq.Attempts)})
		}
	}

	if len(logged) > 0 && logPath != "" {
		if err := writeApplyLog(logPath, logged); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("change log: %v", err))
		}
	}

	return res
}

func writeApplyLog(path string, entries []applyLogEntry) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// findInLane locates ticketID's current overlay symlink under lane's pending/ or
// in-progress/ dirs — the exact path RequeueItem's {From, ID} pair doesn't itself
// carry (Plan works off pre-resolved facts, not paths).
func findInLane(queueRoot, lane, ticketID string) (path string, ok bool) {
	for _, status := range []string{"pending", "in-progress"} {
		dir := filepath.Join(queueRoot, lane, status)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if name == ticketID+".md" || strings.HasPrefix(name, ticketID+"-") {
				return filepath.Join(dir, name), true
			}
		}
	}
	return "", false
}
