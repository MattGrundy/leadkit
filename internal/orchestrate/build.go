package orchestrate

// BuildPlanInput resolves the genuinely messy real-world disk state (store
// frontmatter, the queue overlay, ROLES) into the clean, pre-resolved PlanInput
// Plan() consumes. This is deliberately separate from plan.go: Plan is a pure,
// exhaustively-testable function over simple facts; everything here is best-effort
// inference over real files, with its assumptions documented inline rather than
// hidden — see each resolver below, and cmd/leadkit's own report on how well the
// Type-resolution heuristic performed against the live queue (the exact question
// this task asks: would an explicit task `type` field help).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/review"
	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/typeinfer"
)

// finaliseLane is the fixed canonical finalise role lane — a ticket physically
// placed here (finalise/pending or finalise/in-progress) is the finalise consumer's
// in-flight (awaiting push), NOT routable backlog (Fix 2,
// docs/build-tasks/routing-resolver-hardening-2.md). It matches roles.FixedRoles'
// "finalise" — kept as a named constant so the skip below reads intentionally.
const finaliseLane = "finalise"

// terminalStatuses mirrors internal/audit's own (unexported) set — duplicated
// rather than imported, matching this codebase's convention of small internal
// packages staying decoupled from each other's specifics (see e.g. internal/hook's
// relationship to internal/config).
var terminalStatuses = map[string]bool{
	"completed": true, "done": true, "dropped": true, "superseded": true,
}

// recognizedActiveStatuses are the statuses BuildPlanInput treats as "still live
// work" at all (backlog candidate if status is pending-like, in-flight if placed).
// Anything else (a noncanonical value, a "LOCKED (...)" prose status, ...) is routed
// straight to Escalate — BuildPlanInput never guesses at an unrecognized status.
var recognizedActiveStatuses = map[string]bool{
	"pending": true, "in_progress": true, "blocked": true, "gated": true,
	"awaiting_pr_review": true, "in_review": true,
}

// reviewStatuses are the near-terminal REVIEW states: a ticket at/past review is
// being handled by the review gate, NOT orphaned — it must never be yanked to
// backlog or routed. These built-in defaults are the common ones; reviewStatusSet
// expands them with any profile status_aliases that resolve to them, keeping the
// review vocabulary profile-driven rather than forked here (Fix 2 of
// docs/build-tasks/orchestrate-plan-safety.md).
var reviewStatuses = map[string]bool{
	"in_review": true, "awaiting_pr_review": true,
}

// reviewStatusSet unions the built-in reviewStatuses with any status_aliases whose
// TARGET normalizes to a review state (e.g. a project's "awaiting_review" ->
// "in_review"), all normalized — the effective review-state set for one plan pass.
func reviewStatusSet(statusAliases map[string]string) map[string]bool {
	out := make(map[string]bool, len(reviewStatuses)+len(statusAliases))
	for s := range reviewStatuses {
		out[s] = true
	}
	for src, dst := range statusAliases {
		if reviewStatuses[normalizeStatus(dst)] {
			out[normalizeStatus(src)] = true
		}
	}
	return out
}

// isTerminalOrReview reports whether a normalized status is terminal (completed/
// dropped/...) or a built-in review state — Plan's belt-and-braces guard uses it
// so a finished/reviewed ticket can never be requeued or routed even if it somehow
// reached Plan (the two layers together make such a ticket untouchable).
func isTerminalOrReview(norm string) bool {
	return terminalStatuses[norm] || reviewStatuses[norm]
}

func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// storeRecord is one store task's frontmatter, reduced to what BuildPlanInput needs.
type storeRecord struct {
	ID        string
	Status    string
	Owner     string
	Priority  string
	Type      string // frontmatter `type:`, if the task has been backfilled (see leadkit/internal/typeinfer)
	Repo      string // frontmatter `repo:` — the repo the gate record's sha lives in (Fix 2 gated check)
	Sha       string // frontmatter `sha:` — the HEAD sha whose gate record decides "gated" (Fix 2)
	BlockedBy []string
	Attempts  int
}

// BuildPlanInput reads storeRoot's frontmatter + queueRoot's overlay placements +
// rolesPath's lane->role table + agentsDir's canonical type set (team/agents/*.md),
// and reduces them to a PlanInput. roster/maxAttempts/burnHold/burnHoldReason are
// passed straight through — computing those is the CLI's job (ListAgents piped in by
// the orchestrator agent; burn's own verdict).
//
// TYPE RESOLUTION (see internal/typeinfer.Resolve, shared with `queue
// backfill-type` so the planner and the migration agree on exactly the same rule):
// a task's frontmatter `type:` wins when canonical (Part A of
// docs/build-tasks/type-field-backfill.md) — this is the ONLY change that task made
// here; everything below the type-preference check is the original owner/lane
// fallback, unchanged, so a task without `type:` behaves exactly as before.
// g supplies the gate engine (records dir + git) for the Fix 2 "gated" skip — a
// ticket whose HEAD sha has a COMPLETE gate record is past review/awaiting push and
// is never a routing candidate. Degrade-safe: a nil g, or a record that can't be
// read (out-of-scope repo, unreadable records), simply falls back to the existing
// status/placement signals — it never errors the plan.
func BuildPlanInput(g *gate.Gate, queueRoot, storeRoot, rolesPath, agentsDir string, roster []RosterEntry, laneAliases, statusAliases map[string]string, maxAttempts int, burnHold bool, burnHoldReason string) (PlanInput, error) {
	input := PlanInput{Roster: roster, LaneAliases: laneAliases, MaxAttempts: maxAttempts, BurnHold: burnHold, BurnHoldReason: burnHoldReason}
	review := reviewStatusSet(statusAliases)

	// The `known` role set for reconciling a placed lane to its canonical role — the
	// SAME resolver Plan/assign use (roster types + FixedRoles). Used by the Fix 2
	// finalise-placed skip below.
	var rosterTypes []string
	for _, r := range roster {
		if r.Type != "" {
			rosterTypes = append(rosterTypes, r.Type)
		}
	}
	known := roles.KnownRoles(rosterTypes)

	records, err := readStoreRecords(storeRoot)
	if err != nil {
		return input, err
	}

	placedLane, err := readOverlayPlacements(queueRoot)
	if err != nil {
		return input, err
	}

	laneToRole := map[string]string{} // reverse of roles.List: lane -> role
	for _, e := range roles.List(rolesPath) {
		laneToRole[e.Live] = e.Role
	}

	canonical := typeinfer.LoadCanonical(agentsDir)

	for id, rec := range records {
		lane := placedLane[id]

		// Fix 3: a ticket physically placed in a leading-`_` holding pen (_held/
		// _parked/_stale/...) was deliberately set aside — it is neither routable
		// backlog nor in-flight. Skip it from both (assign refuses it at apply; this
		// stops plan proposing the no-op ROUTE/requeue in the first place).
		if strings.HasPrefix(lane, "_") {
			continue
		}

		// Fix 2 (gated): a ticket whose HEAD sha has a COMPLETE gate record (every
		// RequiredLayers layer ready) is past review, awaiting push — never a routing
		// candidate. This is the robust general signal, INDEPENDENT of status (a
		// gated-awaiting-push ticket's status can still read in_progress/pending) and
		// of placement. Exclude it from BOTH backlog routing and in-flight requeue.
		// Degrade-safe: an unreadable/absent record reads as not-gated and falls
		// through to the status/placement signals below (never errors the plan).
		if hasCompleteGateRecord(g, rec.Repo, rec.Sha) {
			continue
		}

		// Fix 2 (finalise-placed): a ticket physically in the finalise lane
		// (finalise/pending or finalise/in-progress) is the finalise consumer's
		// in-flight (awaiting push), not routable backlog — reconcile the placed lane
		// to its canonical role and, when it IS the finalise role, exclude it from
		// planning entirely so it is never requeued and never a backlog route to its
		// work-owner (the 1454 case, robust even when the gate record can't be read).
		if lane != "" {
			if role, ok := reconcileRole(lane, known, laneAliases); ok && role == finaliseLane {
				continue
			}
		}

		norm := normalizeStatus(rec.Status)

		if terminalStatuses[norm] || review[norm] {
			// Already finished, OR at/past review (in_review/awaiting_pr_review + any
			// profile alias for them) — being handled by the gate, not orphaned. Not
			// a planning candidate: neither backlog nor requeue-eligible (Fix 2).
			continue
		}
		if !recognizedActiveStatuses[norm] {
			// An unrecognized/prose status ("LOCKED (co-designed with user)", a
			// stray value, ...) — never guess at intent, straight to the lead.
			appendTask(&input, lane, Task{ID: id, Lane: lane, EscalateReason: fmt.Sprintf("unrecognized status %q — needs manual triage", rec.Status)})
			continue
		}

		depsMet := true
		for _, dep := range rec.BlockedBy {
			if !terminalStatuses[normalizeStatus(records[dep].Status)] {
				depsMet = false
				break
			}
		}

		// Part A of docs/build-tasks/type-field-backfill.md: an explicit, CANONICAL
		// `type:` field wins outright; anything else falls through to the ORIGINAL,
		// unchanged owner/lane inference below — a task without `type:` (or one
		// whose value isn't canonical) behaves exactly as it did before this field
		// existed.
		taskType, ok := typeinfer.IsCanonical(rec.Type, canonical)
		if !ok {
			taskType = resolveType(rec.Owner, lane, laneToRole)
		}

		t := Task{
			ID: id, Priority: rec.Priority, DepsMet: depsMet, Type: taskType,
			Lane: lane, Attempts: rec.Attempts, Status: norm,
		}
		appendTask(&input, lane, t)
	}

	return input, nil
}

// hasCompleteGateRecord reports whether sha (resolved through repo's git when it
// can be) has a gate record with EVERY gate.RequiredLayers layer recorded ready —
// the "past review, awaiting push" signal Fix 2 skips on. It reads the record
// directly (gate.ReadRecord + gate.RequiredLayers) rather than gate.Check, so scope
// and BYPASS — which make gate.Check fail-OPEN (ok=true with no record at all) —
// can never be mistaken for "gated". Degrade-safe by construction: a nil gate, an
// empty sha, or any read error yields false, and the caller falls back to the
// existing status/placement signals.
func hasCompleteGateRecord(g *gate.Gate, repo, sha string) bool {
	if g == nil || strings.TrimSpace(sha) == "" {
		return false
	}
	full := sha
	if g.Git != nil && repo != "" {
		if resolved, ok := g.Git.RevParse(repo, sha); ok && len(resolved) == 40 {
			full = resolved
		}
	}
	rec, err := g.ReadRecord(full)
	if err != nil {
		return false
	}
	return len(gate.Missing(rec, gate.RequiredLayers)) == 0
}

func appendTask(input *PlanInput, lane string, t Task) {
	if lane == "" {
		input.Backlog = append(input.Backlog, t)
	} else {
		input.InFlight = append(input.InFlight, t)
	}
}

// resolveType is the messy part: a store task's `owner` field names an AGENT
// INSTANCE/TYPE-ish value (e.g. "ci-infra-agent"), which matches neither the
// profile roster's type vocabulary (e.g. "code-review") nor ROLES' role names
// directly. Tried in order:
//  1. owner as-is (works when the roster's Type happens to equal the owner value).
//  2. owner with a trailing "-agent" suffix stripped (matches ROLES lanes like
//     "ci-infra"/"devops" whose corresponding agent def is "*-agent.md").
//  3. if the task is already placed (lane != ""), reverse-resolve the lane through
//     ROLES to its role name.
//
// Returns "" (undeterminable -> Triage) if owner is empty/"unassigned" and no lane
// resolves either. This exact ambiguity is why the build task asks whether an
// explicit `type` field would help — see the CLI report.
func resolveType(owner, lane string, laneToRole map[string]string) string {
	o := strings.TrimSpace(owner)
	if o != "" && !strings.EqualFold(o, "unassigned") {
		return o
	}
	if lane != "" {
		if role, ok := laneToRole[lane]; ok {
			return role
		}
	}
	return ""
}

// readStoreRecords reads every *.md in storeRoot via review.ReadFrontmatter,
// best-effort (a file with no/malformed frontmatter is skipped, matching
// internal/audit's own DiskLens convention).
func readStoreRecords(storeRoot string) (map[string]storeRecord, error) {
	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: read store root %s: %w", storeRoot, err)
	}
	out := map[string]storeRecord{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm, err := review.ReadFrontmatter(filepath.Join(storeRoot, e.Name()))
		if err != nil {
			continue
		}
		id := strings.TrimSpace(fm["id"])
		if id == "" {
			id = idFromFilename(e.Name())
		}
		if id == "" {
			continue
		}
		out[id] = storeRecord{
			ID: id, Status: fm["status"], Owner: fm["owner"], Priority: fm["priority"], Type: fm["type"],
			Repo: fm["repo"], Sha: fm["sha"],
			BlockedBy: parseIDList(fm["blockedBy"]),
			Attempts:  parseIntDefault(fm["attempts"], 0),
		}
	}
	return out, nil
}

// idFromFilename extracts a store task's leading "NNN-" id from its filename —
// used only when the frontmatter itself carries no explicit `id:` field.
func idFromFilename(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(name) || name[i] != '-' {
		return ""
	}
	return name[:i]
}

// parseIDList parses a frontmatter list-shaped value like "[47]", "[47, 89]", or
// "[]" into its elements — review.ReadFrontmatter hands back the raw scalar text,
// this package's own minimal parse of that one shape (no general YAML needed).
func parseIDList(raw string) []string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIntDefault(raw string, def int) int {
	s := strings.TrimSpace(raw)
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// readOverlayPlacements walks queueRoot for symlinks (the queue overlay) and
// returns id -> lane (the symlink's grandparent dir name, e.g. "cr-1", "devops") for
// every RESOLVABLE entry. A dangling symlink is skipped — that's audit/apply's job
// to reap, not orchestrate's to route around. A ticket sitting under BacklogLane
// (ApplyPlan's requeue holding lane) is never recorded — see BacklogLane's own doc
// comment for why that's what makes a requeued ticket routable again.
//
// A leading-`_` holding-pen lane (_held/_parked/_stale/...) IS recorded here (with
// its `_`-prefixed lane name) rather than skipped, so BuildPlanInput can recognize
// the ticket as deliberately set aside and exclude it from BOTH backlog and
// in-flight (Fix 3). If it were skipped, the ticket would read as unplaced and be
// proposed for routing — exactly the no-op assign refuses at apply.
func readOverlayPlacements(queueRoot string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(queueRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return nil // dangling — not orchestrate's job
		}
		base := d.Name()
		id := idFromFilename(base)
		if id == "" {
			return nil
		}
		lane := filepath.Base(filepath.Dir(filepath.Dir(path)))
		if lane == BacklogLane {
			return nil // the requeue holding lane — reads as unplaced backlog, not a real owner
		}
		out[id] = lane
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrate: walk queue root %s: %w", queueRoot, err)
	}
	return out, nil
}
