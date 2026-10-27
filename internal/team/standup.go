package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/roles"
)

// StandupConsumer is one persistent owner the lead must stand up: the role it
// serves, the live LANE it consumes, its zero-token idle invocation, and — from the
// live queue census — whether an owner already appears to be consuming that lane.
type StandupConsumer struct {
	Role string `json:"role"`
	Lane string `json:"lane"`
	// Kind is "owner" (a roster work-role), "review" (a review-gate consumer —
	// exactly the gate's required review layers, gate.RequiredLayers), or "finalise".
	Kind string `json:"kind"`
	// WaitCmd is the exact idle invocation: `leadkit queue wait <queue_root>/<lane>`.
	WaitCmd string `json:"wait_cmd"`
	// Staffed reflects the live queue: "staffed" (in-progress present → an owner
	// looks alive), "unstaffed" (pending waiting, nothing in-progress → NObody is
	// consuming; stand one up now), "idle" (lane exists but empty — no work yet),
	// or "no-lane" (lane dir not scaffolded yet — `team standup --scaffold` creates it).
	Staffed    string `json:"staffed"`
	Pending    int    `json:"pending"`
	InProgress int    `json:"in_progress"`
}

// StandupPlan is `leadkit team standup`'s emitted plan — the FULL consumer set a
// lead must stand up, annotated with live liveness. It READS state and emits a
// plan; leadkit has no agent-spawn primitive, so spawning stays the lead's action
// in its own harness. Computed by ComputeStandupPlan, rendered by FormatStandupPlan.
type StandupPlan struct {
	QueueRoot string            `json:"queue_root"`
	HasRoster bool              `json:"has_roster"`
	Consumers []StandupConsumer `json:"consumers"`
}

// The review-gate consumer set standup lists is NOT a hardcoded constant — it is
// derived live from gate.RequiredLayers (the ordered layers the review gate actually
// enforces), so standup names exactly the consumers the gate requires and no more.
// A 2-layer gate (today's default) lists code-review + critique; a 3-layer project
// (RequiredLayers including outcome-verifier) lists that automatically. orchestrator
// is the LEAD itself (dispatch, not a stood-up consumer) and is excluded; finalise is
// handled separately so it always sorts last, after the review layers.

// standupFinaliseRole is the finalise consumer — a fixed consumer, listed after the
// gate's required review layers (it's the merge/finalise step, not a gate layer).
const standupFinaliseRole = "finalise"

// ComputeStandupPlan builds the standup plan from the roster, the ROLES routing
// file, and the live per-lane census (queue.Sweep's LaneCount slice — reused, never
// re-counted). Lane resolution mirrors ComputeScaffoldPlan's precedence exactly: an
// existing ROLES route wins, else the roster's explicit Lane override, else the role
// name itself — so standup names the SAME lane scaffold created / assign routes to.
func ComputeStandupPlan(roster []RosterSpec, queueRoot, rolesPath string, lanes []queue.LaneCount) StandupPlan {
	plan := StandupPlan{QueueRoot: queueRoot, HasRoster: false}

	existingByRole := map[string]string{}
	for _, e := range roles.List(rolesPath) {
		existingByRole[e.Role] = e.Live
	}
	laneOverride := map[string]string{}
	for _, r := range roster {
		if r.Type != "" && r.Lane != "" {
			laneOverride[r.Type] = r.Lane
		}
	}
	resolveLane := func(role string) string {
		if live, ok := existingByRole[role]; ok {
			return live
		}
		if l, ok := laneOverride[role]; ok {
			return l
		}
		return role
	}

	census := map[string]queue.LaneCount{}
	for _, lc := range lanes {
		census[lc.Lane] = lc
	}
	consumer := func(role, kind string) StandupConsumer {
		lane := resolveLane(role)
		c := StandupConsumer{
			Role:    role,
			Lane:    lane,
			Kind:    kind,
			WaitCmd: fmt.Sprintf("leadkit queue wait %s", joinLane(queueRoot, lane)),
		}
		lc, present := census[lane]
		c.Pending, c.InProgress = lc.Pending, lc.InProgress
		switch {
		case !present:
			c.Staffed = "no-lane"
		case lc.InProgress > 0:
			c.Staffed = "staffed"
		case lc.Pending > 0:
			c.Staffed = "unstaffed"
		default:
			c.Staffed = "idle"
		}
		return c
	}

	// Work-role owners first, in roster order, deduped.
	seen := map[string]bool{}
	var reviewRoles []string
	for _, r := range roster {
		if r.Type == "" || seen[r.Type] {
			continue
		}
		seen[r.Type] = true
		plan.HasRoster = true
		if r.ReviewLayer {
			reviewRoles = append(reviewRoles, r.Type)
			continue
		}
		plan.Consumers = append(plan.Consumers, consumer(r.Type, "owner"))
	}
	// Then the review consumers — the gate's built-in layers in gate order, then the roster roles
	// declared review layers, in roster order — then finalise; always present, roster or not.
	for _, role := range gate.Vocabulary(reviewRoles) {
		plan.Consumers = append(plan.Consumers, consumer(role, "review"))
	}
	plan.Consumers = append(plan.Consumers, consumer(standupFinaliseRole, "finalise"))
	return plan
}

// joinLane joins queueRoot and lane with a single "/" — a display path for the wait
// invocation, not a filesystem op (kept slash-joined so the emitted command reads
// the same on every OS).
func joinLane(queueRoot, lane string) string {
	return strings.TrimRight(queueRoot, "/") + "/" + lane
}

// staffedNote renders the live-liveness annotation for one consumer.
func staffedNote(c StandupConsumer) string {
	switch c.Staffed {
	case "staffed":
		return fmt.Sprintf("already staffed — %d in-progress", c.InProgress)
	case "unstaffed":
		return fmt.Sprintf("UNSTAFFED — %d pending, 0 in-progress", c.Pending)
	case "idle":
		return "idle — no work waiting yet"
	default:
		return "lane missing — will be created with `--scaffold`"
	}
}

// FormatStandupPlan renders the copy-pasteable human plan a lead reads. It names the
// full consumer set (each with its exact idle invocation), the owner LOOP, the
// lead's dispatch-only role, and the cluster-vs-code split — the recipe a lead
// currently has to be TOLD, now emitted from state.
func FormatStandupPlan(plan StandupPlan) string {
	var b strings.Builder
	b.WriteString("leadkit team standup — persistent-owner standup plan\n")
	fmt.Fprintf(&b, "queue root: %s\n\n", plan.QueueRoot)
	b.WriteString("Stand up ONE persistent owner per consumer below. Each idles at zero token\n")
	b.WriteString("cost in `queue wait` and LOOPS forever — it never exits after one task.\n\n")

	owners := consumersOfKind(plan, "owner")
	if len(owners) > 0 {
		b.WriteString("Work-role owners (from the roster):\n")
		writeConsumers(&b, owners)
	} else {
		b.WriteString("Work-role owners: NONE — no roster declared.\n")
		b.WriteString("  Declare work-roles in leadkit.yaml `roster:` (then `leadkit team scaffold`)\n")
		b.WriteString("  and re-run `leadkit team standup` to get an owner per role.\n")
	}
	b.WriteString("\nReview-gate consumers (always required, roster or not):\n")
	writeConsumers(&b, consumersOfKind(plan, "review"))
	b.WriteString("\nFinalise consumer:\n")
	writeConsumers(&b, consumersOfKind(plan, "finalise"))

	b.WriteString("\nThe owner loop (EVERY consumer above runs this, forever, IN ONE CONTINUOUS TURN):\n")
	b.WriteString("  FIRST ACTION ON SPAWN (step 0, before anything else): the instant you spawn,\n")
	b.WriteString("  immediately arm `leadkit queue wait <queue_root>/<your lane>` in the FOREGROUND — it\n")
	b.WriteString("  drains any already-pending ticket instantly and then blocks for new ones. Do NOT\n")
	b.WriteString("  read this contract and end your turn: STARTING the loop is your first action. An\n")
	b.WriteString("  owner that spawns and ends its first turn WITHOUT arming the wait or claiming a\n")
	b.WriteString("  ticket is the post-spawn-dormancy anti-pattern — it sits idle with pending work\n")
	b.WriteString("  until nudged. Spawn → arm the foreground wait → run the loop below, all in turn 1.\n")
	b.WriteString("  1. Run `leadkit queue wait <queue_root>/<role>` in the FOREGROUND — it blocks the\n")
	b.WriteString("     agent's turn at ZERO token cost on fsnotify and RESUMES the turn when work lands.\n")
	b.WriteString("     Do NOT background it: a backgrounded wait relies on the harness re-invoking the\n")
	b.WriteString("     owner when the bg task completes, which can silently NOT fire → the owner goes\n")
	b.WriteString("     dormant while the wait still shows \"running\" (the observed owner wedge).\n")
	b.WriteString("  2. `leadkit queue move <queue_root>/<role>/pending/<t> <queue_root>/<role>/in-progress/`  — claim the ticket\n")
	b.WriteString("  3. do the work\n")
	b.WriteString("  4. `git commit`  — commit is NOT the end of the turn\n")
	b.WriteString("  5. `leadkit review send ...`  — enqueue the review gate (then record the verdict\n")
	b.WriteString("     with `leadkit gate verdict ...`)\n")
	b.WriteString("  6. re-arm `leadkit queue wait <queue_root>/<role>` (back to step 1) — all in the\n")
	b.WriteString("     SAME turn; do NOT exit after one task.\n")
	b.WriteString("  ANTI-PATTERN (the wedge): ending the turn after `git commit`, before `review send`\n")
	b.WriteString("  + re-arm. After commit → review send → re-arm; commit is not the end. An owner that\n")
	b.WriteString("  stops after committing goes dormant with its lane still pending — Fix it by always\n")
	b.WriteString("  proceeding to gate-enqueue and re-arm the foreground wait before the turn ends.\n")

	b.WriteString("\nThe lead's role is DISPATCH + arbitration ONLY — NOT per-task spawning:\n")
	b.WriteString("  - dispatch:  `leadkit queue assign <ticket#> <role>`  (owners self-pull the rest)\n")
	b.WriteString("  - arbitrate review verdicts / reconcile liveness\n")
	b.WriteString("  - the lead NEVER spawns a fresh agent per task — if it does, work piles unconsumed\n")

	b.WriteString("\nCluster vs code split:\n")
	b.WriteString("  - cluster / read-only escalations → the lead's background Bash\n")
	b.WriteString("  - code tasks → owned end-to-end by the persistent owner that claimed them\n")

	b.WriteString("\nNON-INTERACTIVE / NO-BLOCKING command contract (every owner, every command):\n")
	b.WriteString("  THE FAILURE, BY NAME: a blocking or approval-gated call wedges the owner INSIDE\n")
	b.WriteString("  the call — messages drain only at a tool boundary a hung call never reaches, so\n")
	b.WriteString("  the owner is unreachable to a status probe and its loop never resumes. This is a\n")
	b.WriteString("  DIFFERENT wedge than ending the turn after commit: here the turn never ends.\n")
	b.WriteString("  - Run EVERY command NON-INTERACTIVELY: pass the tool's assume-yes / no-input /\n")
	b.WriteString("    CI flags (`--yes`, `--no-input`, `CI=1`, etc.). NEVER run a command that can stop\n")
	b.WriteString("    at an interactive approval/confirmation prompt — unanswerable unattended = an\n")
	b.WriteString("    instant unreachable wedge.\n")
	b.WriteString("  - Run long/heavy commands (build/test suites, deploys) TIME-BOUNDED: a\n")
	b.WriteString("    `timeout`, a `--timeout` flag, or delegated — so a hang can't freeze the turn\n")
	b.WriteString("    indefinitely.\n")
	b.WriteString("  - Extend the cluster-vs-code split: any hangable / long / interactive / cluster\n")
	b.WriteString("    command is EITHER timeout-wrapped + non-interactive, OR delegated to the lead's\n")
	b.WriteString("    background Bash — NEVER run inline-blocking in the owner's turn.\n")
	b.WriteString("  - REPORT, DON'T RETRY, ON HANG: if a hardened command still HANGS or FAILS despite\n")
	b.WriteString("    non-interactive flags + a timeout, REPORT the exact command to the lead — do not\n")
	b.WriteString("    retry or sit. A genuinely un-hardenable blocker must surface as a REPORT, not a\n")
	b.WriteString("    silent wedge.\n")
	return b.String()
}

// standupStageSubdirs is the per-lane overlay shape internal/queue's Assign/Move/Wait
// expect — the SAME canonical stage set `team scaffold` writes (canonicalSubdirs).
// `team standup --scaffold` creates exactly these under a missing lane, never more.
var standupStageSubdirs = []string{"pending", "in-progress", "done"}

// StandupScaffoldLane is one lane the scaffold considered: the role that maps to it,
// its resolved lane name (reused verbatim from the StandupPlan — never recomputed, so
// it agrees with the owner's `queue wait <lane>` invocation and with where
// Assign/Wait/review-send route), and the outcome — Present (lane dir already exists,
// left untouched), Refused (an unsafe lane name that would escape queue_root, skipped
// and NEVER created), or a set of stage dirs to create.
type StandupScaffoldLane struct {
	Role    string   `json:"role"`
	Lane    string   `json:"lane"`
	Present bool     `json:"present"` // lane dir already existed — untouched (never disturb its tickets)
	Refused bool     `json:"refused"` // unsafe lane name (separator / ".." / absolute) — skipped, never created
	Create  []string `json:"create"`  // stage dirs to create (dry-run) / created (apply) when the lane is missing
}

// StandupScaffoldPlan is the missing-lane provisioning plan derived from a
// StandupPlan — computed by ComputeStandupScaffoldPlan (reads the filesystem to see
// which lane dirs exist), executed by ApplyStandupScaffoldPlan. Every lane it names
// is confined strictly under QueueRoot.
type StandupScaffoldPlan struct {
	QueueRoot string                `json:"queue_root"`
	Lanes     []StandupScaffoldLane `json:"lanes"`
}

// WouldCreate counts the lanes this plan would newly create (missing, safe) — the
// change-count the --scaffold guard reports.
func (p StandupScaffoldPlan) WouldCreate() int {
	n := 0
	for _, l := range p.Lanes {
		if !l.Present && !l.Refused && len(l.Create) > 0 {
			n++
		}
	}
	return n
}

// safeLaneSegment reports whether lane is a single, clean path segment safe to join
// directly under queue_root: non-empty, not "."/"..", not absolute, and containing no
// path separator. This is the security guard — a role/lane name with a separator or a
// ".." component (which would let a scaffold escape queue_root) is refused, never made.
func safeLaneSegment(lane string) bool {
	if lane == "" || lane == "." || lane == ".." {
		return false
	}
	if filepath.IsAbs(lane) {
		return false
	}
	if strings.ContainsAny(lane, `/\`) {
		return false
	}
	// A clean single segment equals its own Base; anything else is composite.
	return lane == filepath.Base(lane)
}

// ComputeStandupScaffoldPlan derives the missing-lane provisioning plan from an
// already-computed StandupPlan. It REUSES each consumer's resolved Lane verbatim (the
// SAME lane the plan's `queue wait` invocation names — roles.CanonicalLane's answer via
// ComputeStandupPlan's resolver), deduplicates lanes, and for each: refuses an unsafe
// name (safeLaneSegment — never escapes queue_root); marks an existing lane dir Present
// (left completely untouched — its tickets are never disturbed); else plans the three
// canonical stage dirs. Reads the filesystem (stat) but writes nothing.
func ComputeStandupScaffoldPlan(plan StandupPlan) StandupScaffoldPlan {
	sp := StandupScaffoldPlan{QueueRoot: plan.QueueRoot}
	cleanRoot := filepath.Clean(plan.QueueRoot)
	seen := map[string]bool{}
	for _, c := range plan.Consumers {
		if seen[c.Lane] {
			continue
		}
		seen[c.Lane] = true
		item := StandupScaffoldLane{Role: c.Role, Lane: c.Lane}
		laneDir := filepath.Join(plan.QueueRoot, c.Lane)
		// Confinement: the name must be a clean single segment AND the joined lane dir
		// must sit directly under queue_root (belt-and-suspenders against traversal).
		if !safeLaneSegment(c.Lane) || filepath.Dir(laneDir) != cleanRoot {
			item.Refused = true
			sp.Lanes = append(sp.Lanes, item)
			continue
		}
		if fi, err := os.Stat(laneDir); err == nil && fi.IsDir() {
			item.Present = true
			sp.Lanes = append(sp.Lanes, item)
			continue
		}
		for _, sub := range standupStageSubdirs {
			item.Create = append(item.Create, filepath.Join(laneDir, sub))
		}
		sp.Lanes = append(sp.Lanes, item)
	}
	return sp
}

// StandupScaffoldResult is what ApplyStandupScaffoldPlan actually did — the stage dirs
// created and the lane names bucketed by outcome, for the command's report.
type StandupScaffoldResult struct {
	CreatedDirs  []string `json:"created_dirs"`  // full stage-dir paths created
	CreatedLanes []string `json:"created_lanes"` // lanes newly scaffolded
	PresentLanes []string `json:"present_lanes"` // lanes already present, untouched
	RefusedLanes []string `json:"refused_lanes"` // unsafe lanes skipped
}

// ApplyStandupScaffoldPlan performs plan's writes: for every missing (safe) lane it
// creates the three canonical stage dirs (os.MkdirAll — create-only, additive), and
// leaves Present lanes completely untouched. Refused lanes are recorded but NEVER
// created. Idempotent: re-running against an already-scaffolded queue creates nothing.
func ApplyStandupScaffoldPlan(plan StandupScaffoldPlan) (StandupScaffoldResult, error) {
	var res StandupScaffoldResult
	for _, l := range plan.Lanes {
		switch {
		case l.Refused:
			res.RefusedLanes = append(res.RefusedLanes, l.Lane)
		case l.Present:
			res.PresentLanes = append(res.PresentLanes, l.Lane)
		default:
			for _, dir := range l.Create {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return res, fmt.Errorf("team standup --scaffold: mkdir %s: %w", dir, err)
				}
				res.CreatedDirs = append(res.CreatedDirs, dir)
			}
			if len(l.Create) > 0 {
				res.CreatedLanes = append(res.CreatedLanes, l.Lane)
			}
		}
	}
	return res, nil
}

// FormatStandupScaffoldPlan renders the scaffold plan for the command — printed for
// BOTH the dry-run preview and a real apply, so a lead sees exactly which lanes would
// be / were created vs already-present vs refused.
func FormatStandupScaffoldPlan(plan StandupScaffoldPlan) string {
	var b strings.Builder
	b.WriteString("lane scaffold (missing role lanes under queue_root):\n")
	any := false
	for _, l := range plan.Lanes {
		switch {
		case l.Refused:
			fmt.Fprintf(&b, "  REFUSED %-18s lane %q escapes queue_root — skipped (nothing created)\n", l.Role, l.Lane)
			any = true
		case l.Present:
			fmt.Fprintf(&b, "  present %-18s %s (untouched)\n", l.Role, l.Lane)
		default:
			fmt.Fprintf(&b, "  + create %-16s %s/{pending,in-progress,done}\n", l.Role, filepath.Join(plan.QueueRoot, l.Lane))
			any = true
		}
	}
	if !any {
		b.WriteString("  (nothing to create — every planned lane already exists)\n")
	}
	return b.String()
}

func consumersOfKind(plan StandupPlan, kind string) []StandupConsumer {
	var out []StandupConsumer
	for _, c := range plan.Consumers {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func writeConsumers(b *strings.Builder, cs []StandupConsumer) {
	for _, c := range cs {
		fmt.Fprintf(b, "  %-18s [%s]\n      %s\n", c.Role, staffedNote(c), c.WaitCmd)
	}
}
