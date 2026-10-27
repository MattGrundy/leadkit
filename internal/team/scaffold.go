package team

// `leadkit team scaffold` (queue-mode-enforcement item 1) closes the gap a second adopting project
// fell through: scaffolding a lightweight team the MESSAGE-DRIVEN way (SendMessage +
// session TaskList) was easier than wiring the disk queue by hand. scaffold makes
// falling INTO queue mode the path of least resistance — for a project that already
// has a leadkit.yaml (roster) but no live queue, it computes the MINIMUM viable queue
// on disk: one lane per roster role (+ orchestrator/finalise, always) with
// pending/in-progress/done, and a starter ROLES file so internal/roles.Resolve routes
// immediately. Guarded the same way team install/init/audit are: dry-run default,
// --apply --confirm required (cmd/leadkit's confirmGuard). Idempotent — re-running
// against an existing queue leaves every already-routed role's lane COMPLETELY
// untouched (even if its subdirs are incomplete) and only fills genuine gaps.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/roles"
)

// canonicalScaffoldRoles are the durable roles scaffold always ensures a lane for, in
// addition to whatever the profile roster declares. orchestrator/finalise aren't
// reviewer-pool roster entries (nobody sizes a "count" for them), but every queue-mode
// team needs both wired for routing (send-to-review's finalise hop, the
// orchestrator's own inbox) to work at all.
var canonicalScaffoldRoles = []string{"orchestrator", "finalise"}

// No two review layers (internal/gate.Vocabulary: code-review and critique, plus any
// roster role declared review_layer) may share a queue-consumer lane. They are the review
// gate's independent layers: a reviewer that sat in a lane serving both roles could plausibly
// record both under the same `by` identity, which gate.Record/GateVerdict already refuse —
// but scaffold must not even OFFER the binding that nudges a project toward that shape.
// GAP-4's "one agent does both" ask is a deliberately SEPARATE, config-gated,
// Matt-sign-off-required follow-up (see docs/build-tasks/queue-mode-enforcement.md item 4)
// — NOT built here, and the refusal in ComputeScaffoldPlan is what keeps it that way.

// ScaffoldLaneItem is one role's scaffold plan: the lane dir name it resolves to
// (existing, if the role already has a ROLES row — left untouched; a same-named new
// lane otherwise), and what (if anything) needs creating.
type ScaffoldLaneItem struct {
	Role       string
	Lane       string
	Subdirs    []string
	Missing    []string // full paths (under queueRoot) that don't exist yet
	RoleRowAdd bool     // true: role has no existing ROLES row — plan adds one
}

// ScaffoldPlan is `team scaffold`'s full plan — computed by ComputeScaffoldPlan,
// executed by ApplyScaffoldPlan.
type ScaffoldPlan struct {
	QueueRoot       string
	RolesPath       string
	RolesFileExists bool
	Lanes           []ScaffoldLaneItem

	// StoreRoot is the flat task store the lane entries link into; StoreMissing says it does not
	// exist yet. doctor reports a missing store CRITICAL, and `task create` cannot write a ticket
	// without it, so the command that builds the queue builds this too. Set by PlanStore.
	StoreRoot    string
	StoreMissing bool
}

// PlanStore records whether the task store at storeRoot still has to be created. It reads only.
func (p *ScaffoldPlan) PlanStore(storeRoot string) {
	if storeRoot == "" {
		return
	}
	p.StoreRoot = storeRoot
	if fi, err := os.Stat(storeRoot); err != nil || !fi.IsDir() {
		p.StoreMissing = true
	}
}

// AnyChanged reports whether Apply would create or write anything.
func (p ScaffoldPlan) AnyChanged() bool {
	if p.StoreMissing {
		return true
	}
	for _, l := range p.Lanes {
		if len(l.Missing) > 0 || l.RoleRowAdd {
			return true
		}
	}
	return false
}

// ComputeScaffoldPlan is pure except for reads (the existing ROLES file, and which
// lane dirs already exist under queueRoot) — it writes nothing. roster is normally
// cfg.Roster; canonicalScaffoldRoles are appended after it (roster order preserved,
// duplicates against the roster or each other skipped) so orchestrator/finalise
// always get a row without ever displacing a roster role's own.
//
// A roster entry's optional Lane field (GAP-4/item 4) lets more than one role bind
// to the SAME live lane — a lightweight team runs fewer standing consumers. The one
// combination this REFUSES outright (a non-nil error, no plan at all) is
// code-review and critique sharing a lane — see independencePairRoles; that pair is
// not auto-relaxable in this build. The refusal compares each role's FINAL
// RESOLVED lane (existing ROLES route, else an explicit override, else the role
// name itself) — not just "both have an explicit override set to the same string"
// — so it also catches a default-vs-override collision (code-review left at its
// default "code-review" while critique's override points AT "code-review") and an
// override landing on a role that's already routed elsewhere in ROLES.
func ComputeScaffoldPlan(roster []RosterSpec, queueRoot, rolesPath string) (ScaffoldPlan, error) {
	plan := ScaffoldPlan{QueueRoot: queueRoot, RolesPath: rolesPath}
	if fi, err := os.Stat(rolesPath); err == nil && !fi.IsDir() {
		plan.RolesFileExists = true
	}

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

	// resolveLane is the SAME precedence the per-role planning loop below applies
	// (existing ROLES route wins — a live lane is never re-routed by a later
	// override; else the roster's explicit override; else the role name itself) —
	// used here to compute where code-review and critique would ACTUALLY end up,
	// however each got there, so the independence refusal can't be dodged by
	// mixing "one explicit, one default" or "one already-routed, one overriding
	// onto that same live lane" (a narrower "both must have an explicit override"
	// check missed both of those).
	resolveLane := func(role string) string {
		if live, ok := existingByRole[role]; ok {
			return live
		}
		if l, ok := laneOverride[role]; ok {
			return l
		}
		return role
	}
	// Every review layer is held to the same rule as the built-in pair: no two may share a lane.
	var declared []string
	for _, r := range roster {
		if r.ReviewLayer {
			declared = append(declared, r.Type)
		}
	}
	layers := gate.Vocabulary(declared)
	for i := range layers {
		for j := i + 1; j < len(layers); j++ {
			if a, b := resolveLane(layers[i]), resolveLane(layers[j]); a == b {
				return ScaffoldPlan{}, fmt.Errorf(
					"team scaffold: refuse — %s and %s would both resolve to lane %q: review layers must never share a queue-consumer lane (layer independence) — this is not auto-relaxable, see docs/build-tasks/queue-mode-enforcement.md item 4",
					layers[i], layers[j], a,
				)
			}
		}
	}

	seen := map[string]bool{}
	var roleOrder []string
	for _, r := range roster {
		if r.Type == "" || seen[r.Type] {
			continue
		}
		seen[r.Type] = true
		roleOrder = append(roleOrder, r.Type)
	}
	for _, r := range canonicalScaffoldRoles {
		if seen[r] {
			continue
		}
		seen[r] = true
		roleOrder = append(roleOrder, r)
	}

	laneAlreadyPlanned := map[string]bool{}
	for _, role := range roleOrder {
		lane := role
		if l, ok := laneOverride[role]; ok {
			lane = l
		}
		item := ScaffoldLaneItem{Role: role, Lane: lane, Subdirs: append([]string(nil), canonicalSubdirs...)}
		if live, ok := existingByRole[role]; ok {
			// Already routed (possibly to a differently-named live lane, e.g. "cr-1")
			// — leave it COMPLETELY untouched, the idempotent "fills gaps only"
			// contract; never re-check or "complete" an existing lane's subdirs.
			item.Lane = live
			plan.Lanes = append(plan.Lanes, item)
			continue
		}
		item.RoleRowAdd = true
		if !laneAlreadyPlanned[lane] {
			// Only compute (and later create) a shared lane's subdirs ONCE — a
			// second role bound to the same lane still gets its own ROLES row, but
			// must not re-plan (or re-report) the same mkdirs.
			for _, sub := range canonicalSubdirs {
				dir := filepath.Join(queueRoot, lane, sub)
				if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
					item.Missing = append(item.Missing, dir)
				}
			}
			laneAlreadyPlanned[lane] = true
		}
		plan.Lanes = append(plan.Lanes, item)
	}
	return plan, nil
}

// ScaffoldResult is what ApplyScaffoldPlan actually did.
type ScaffoldResult struct {
	CreatedDirs []string
	RolesAdded  []roles.Entry
}

// ApplyScaffoldPlan performs plan's writes: every Missing lane subdir is created
// (os.MkdirAll — create-only), then every RoleRowAdd role is appended to the ROLES
// file as one new "role\tlane" row (the file is created with a small header comment
// if it didn't exist yet). Dirs are created before the ROLES rows are appended so a
// mid-failure state never has a ROLES row pointing at a lane that doesn't exist.
func ApplyScaffoldPlan(plan ScaffoldPlan) (ScaffoldResult, error) {
	var res ScaffoldResult

	if plan.StoreMissing {
		if err := os.MkdirAll(plan.StoreRoot, 0o755); err != nil {
			return res, fmt.Errorf("team scaffold: mkdir %s: %w", plan.StoreRoot, err)
		}
		res.CreatedDirs = append(res.CreatedDirs, plan.StoreRoot)
	}

	for _, lane := range plan.Lanes {
		for _, dir := range lane.Missing {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, fmt.Errorf("team scaffold: mkdir %s: %w", dir, err)
			}
			res.CreatedDirs = append(res.CreatedDirs, dir)
		}
	}

	var rows []string
	for _, lane := range plan.Lanes {
		if !lane.RoleRowAdd {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s\t%s", lane.Role, lane.Lane))
		res.RolesAdded = append(res.RolesAdded, roles.Entry{Role: lane.Role, Live: lane.Lane})
	}
	if len(rows) == 0 {
		return res, nil
	}

	if err := os.MkdirAll(filepath.Dir(plan.RolesPath), 0o755); err != nil {
		return res, fmt.Errorf("team scaffold: mkdir %s: %w", filepath.Dir(plan.RolesPath), err)
	}
	f, err := os.OpenFile(plan.RolesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return res, fmt.Errorf("team scaffold: open %s: %w", plan.RolesPath, err)
	}
	defer f.Close()
	var out strings.Builder
	if !plan.RolesFileExists {
		out.WriteString("# leadkit team scaffold — role -> live queue-lane routing (see internal/roles)\n")
	}
	for _, row := range rows {
		out.WriteString(row)
		out.WriteByte('\n')
	}
	if _, err := f.WriteString(out.String()); err != nil {
		return res, fmt.Errorf("team scaffold: write %s: %w", plan.RolesPath, err)
	}
	return res, nil
}

// FormatScaffoldPlan renders plan for `team scaffold`'s output — printed for BOTH
// dry-run and a real apply.
func FormatScaffoldPlan(plan ScaffoldPlan) string {
	var b strings.Builder
	if plan.StoreMissing {
		fmt.Fprintf(&b, "+ mkdir %s (task store: tickets live here and the lanes link into it)\n", plan.StoreRoot)
	}
	for _, lane := range plan.Lanes {
		for _, dir := range lane.Missing {
			fmt.Fprintf(&b, "+ mkdir %s (role %s)\n", dir, lane.Role)
		}
		if lane.RoleRowAdd {
			fmt.Fprintf(&b, "+ ROLES: %s\t%s\n", lane.Role, lane.Lane)
		} else {
			fmt.Fprintf(&b, "  (unchanged) %s -> %s already routed\n", lane.Role, lane.Lane)
		}
	}
	if !plan.AnyChanged() {
		b.WriteString("nothing to do — every canonical role already has a live lane\n")
	}
	return b.String()
}

// ChangeCount is how many things Apply would create: each lane that gains a ROLES row, plus the
// task store when it is missing. The `--apply` refusal reports it.
func (p ScaffoldPlan) ChangeCount() int {
	n := 0
	for _, l := range p.Lanes {
		if l.RoleRowAdd {
			n++
		}
	}
	if p.StoreMissing {
		n++
	}
	return n
}

// FirstEnqueueHint is the one-liner `team scaffold` prints on success: the frictionless path
// must be SHOWN, not just enabled. It names the command that actually puts a first task in an
// owner's lane, `task create --assign`, for the first roster work role (a reviewer, orchestrator
// or finalise lane is not where a first task goes). `review send` is not it: it sends a finished
// ticket to the code-review gate, and refuses a ticket that does not exist yet.
// "" when the plan has no work role: a first task does not belong in a reviewer, orchestrator or
// finalise lane, so no hint beats one that sends it there.
func FirstEnqueueHint(plan ScaffoldPlan) string {
	role := ""
	for _, l := range plan.Lanes {
		switch l.Role {
		case "code-review", "critique", "orchestrator", "finalise":
			continue
		}
		role = l.Role
		break
	}
	if role == "" {
		return ""
	}
	return fmt.Sprintf(
		"enqueue the first task: `leadkit task create --type %s --unparented --title \"<what to do>\" --assign %s` "+
			"(`leadkit review send` is for sending FINISHED work to the review gate, not for a first task)",
		role, role,
	)
}
