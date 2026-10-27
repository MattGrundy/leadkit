package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/roles"
)

// consolidateStatuses is every status dir a role-keyed lane migration walks —
// same set countQueueTickets/team.canonicalSubdirs use for "the durable lane
// structure".
var consolidateStatuses = []string{"pending", "in-progress", "done"}

// ConsolidateMove is one planned (or applied) move: an entry found under a
// non-role lane's status dir, relinked into the SAME status dir under its
// resolved role lane.
type ConsolidateMove struct {
	FromLane string          // the legacy/instance/ephemeral lane dir it's moving OUT of
	ToRole   string          // the canonical role lane it's moving INTO
	Status   string          // pending|in-progress|done — unchanged by the move
	Name     string          // entry basename
	From     string          // full source path
	To       string          // full destination path
	Via      roles.LaneMatch // how FromLane resolved to ToRole (canonical / -agent fold / alias)
}

// ConsolidatePlan is what `queue consolidate` computed: the individual entry
// moves implied by folding every non-role lane into its resolved role lane, plus
// any lane names that resolved to nothing at all (need a human to map).
type ConsolidatePlan struct {
	Moves    []ConsolidateMove
	Unmapped []string // lane names with no canonical-role home
}

// ComputeConsolidatePlan is the READ-ONLY planning half of the role-keyed-lanes
// migration tool (docs/build-tasks/role-keyed-lanes.md): for every lane dir
// directly under queueRoot whose name does NOT already resolve to a canonical
// role (via roles.CanonicalLane — the SAME name→role resolver send-to-review/
// gate-verdict/assign/the viewer/doctor's queue-lanes check all route through, so
// this plan can never disagree with them about what counts as a role lane), plan
// relinking every entry under it into the equivalent status dir under its
// resolved role lane. A lane that already resolves to itself (a proper role lane)
// is left alone entirely — never re-planned, never touched. known is the
// project's additional declared roles (e.g. roster Type values, or
// roles.List(rolesPath) role names) — roles.FixedRoles is always implicit, via
// CanonicalLane. aliases is the project's optional lane_aliases map (raw-or-folded
// lane name → canonical role), consulted LAST for a stray the built-in `-agent`
// lane-fold can't rescue; pass nil when the project declares none. Each planned move
// records HOW its lane resolved (roles.LaneMatch: canonical / `-agent` fold / alias)
// so the dry-run can label lanes mapped via the fold or an alias vs canonically.
//
// Nothing here touches disk beyond the initial directory listing — this is the
// dry-run plan every `queue consolidate` invocation computes; ApplyConsolidatePlan
// is the only function that mutates anything.
func ComputeConsolidatePlan(queueRoot string, known []string, aliases map[string]string) (ConsolidatePlan, error) {
	entries, err := os.ReadDir(queueRoot)
	if err != nil {
		return ConsolidatePlan{}, fmt.Errorf("queue consolidate: read queue root %s: %w", queueRoot, err)
	}
	var lanes []string
	for _, e := range entries {
		if e.IsDir() {
			lanes = append(lanes, e.Name())
		}
	}
	sort.Strings(lanes)

	var plan ConsolidatePlan
	for _, lane := range lanes {
		role, via, ok := roles.ResolveLane(lane, known, aliases)
		if !ok {
			plan.Unmapped = append(plan.Unmapped, lane)
			continue
		}
		if role == lane {
			continue // already a proper role lane — nothing to migrate
		}
		for _, status := range consolidateStatuses {
			dir := filepath.Join(queueRoot, lane, status)
			names := listEntries(dir)
			sort.Strings(names)
			for _, name := range names {
				plan.Moves = append(plan.Moves, ConsolidateMove{
					FromLane: lane, ToRole: role, Status: status, Name: name,
					From: filepath.Join(dir, name),
					To:   filepath.Join(queueRoot, role, status, name),
					Via:  via,
				})
			}
		}
	}
	return plan, nil
}

// ApplyConsolidatePlan performs plan's moves for real: relinks (os.Rename — the
// same primitive Move uses) every entry from its non-role lane into the resolved
// role lane's SAME status dir, creating the destination dir as needed. Idempotent
// — an entry whose source is already gone (already relinked by a prior partial
// apply, or moved by hand) is skipped rather than erroring, so a re-run after an
// interruption just finishes the rest. Drained (now-empty) instance/ephemeral
// lanes are left in place on disk — removing directories is deliberately out of
// scope here (see `queue consolidate`'s own doc comment); they simply end up with
// nothing under pending/in-progress/done once every move completes.
//
// NEVER call this against the real project queue as part of building this tool —
// the live migration is a separate, later, coordinated step.
func ApplyConsolidatePlan(plan ConsolidatePlan) (moved int, err error) {
	for _, m := range plan.Moves {
		if _, statErr := os.Lstat(m.From); statErr != nil {
			continue // already moved (or gone) — idempotent
		}
		if err := os.MkdirAll(filepath.Dir(m.To), 0o755); err != nil {
			return moved, fmt.Errorf("queue consolidate: mkdir %s: %w", filepath.Dir(m.To), err)
		}
		if err := os.Rename(m.From, m.To); err != nil {
			return moved, fmt.Errorf("queue consolidate: move %s -> %s: %w", m.From, m.To, err)
		}
		moved++
	}
	return moved, nil
}

// FormatConsolidatePlan renders plan as the human-readable report `queue
// consolidate` prints — dry-run and applied share the same shape; applied just
// says so up front and uses the past tense.
func FormatConsolidatePlan(plan ConsolidatePlan, applied bool) string {
	var b strings.Builder
	if applied {
		b.WriteString("queue consolidate: APPLIED\n")
	} else {
		b.WriteString("queue consolidate: DRY RUN (nothing moved; re-run with --apply --confirm)\n")
	}
	if len(plan.Moves) == 0 {
		b.WriteString("  no non-role lane entries found to move\n")
	}
	verb := "would move"
	if applied {
		verb = "moved"
	}
	for _, m := range plan.Moves {
		fmt.Fprintf(&b, "  %s %s/%s/%s -> %s/%s/%s%s\n", verb, m.FromLane, m.Status, m.Name, m.ToRole, m.Status, m.Name, laneViaSuffix(m.Via))
	}
	if len(plan.Unmapped) > 0 {
		fmt.Fprintf(&b, "  UNMAPPABLE lane(s) — no canonical-role home, needs human mapping: %s\n", strings.Join(plan.Unmapped, ", "))
	}
	return b.String()
}

// laneViaSuffix annotates a move line with HOW its lane resolved when that wasn't a
// plain canonical/instance-fold match — "(via -agent fold)" or "(via alias)" — for
// transparency about lanes rescued by the `-agent` lane-fold or an explicit
// lane_aliases entry. A canonical match adds nothing (the common, unremarkable case).
func laneViaSuffix(via roles.LaneMatch) string {
	switch via {
	case roles.LaneAgentFold:
		return "  (via -agent fold)"
	case roles.LaneAlias:
		return "  (via alias)"
	default:
		return ""
	}
}
