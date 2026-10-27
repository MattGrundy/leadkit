package roles

import (
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
)

// FixedRoles is the canonical review-gate role set — always present in every
// project regardless of roster config (docs/build-tasks/role-keyed-lanes.md).
// code-review/critique carry a short form in gate.CanonicalRole (cr/crit, used for
// the gate's layer-independence rule); finalise/orchestrator don't (they're never
// layer-independence-checked) but are just as fixed for LANE purposes — every
// project has them, roster or not.
var FixedRoles = []string{"code-review", "critique", "finalise", "orchestrator"}

// shortFormLane maps gate.CanonicalRole's review-layer short forms to their full LANE
// name — the queue/<role>/ directory role-keyed lanes route to. Reconciling here
// (instead of changing gate.CanonicalRole to return the full name) is deliberate:
// gate.CanonicalRole exists to key the gate's verdict layer-independence rule on
// `by`, and role-keyed-lanes.md's hard constraint forbids touching that logic —
// this map is the seam that turns its short form into the full role name the lane
// resolver needs, without changing gate.CanonicalRole itself.
var shortFormLane = map[string]string{"cr": "code-review", "crit": "critique"}

// agentLaneSuffix is the widespread owner-instance lane naming CONVENTION the LANE
// resolver folds away (step 2): `<role>-agent` lanes (devops-agent, qa-agent,
// ci-pipeline-agent…) map onto `<role>` from the roster with ZERO per-lane config.
// This fold lives ONLY here in the LANE resolver — never in gate.CanonicalRole,
// which role-keyed-lanes.md forbids touching (it keys the verdict layer-
// independence rule).
const agentLaneSuffix = "-agent"

// LaneMatch records HOW ResolveLane resolved a lane name to its role — so callers
// (e.g. `queue consolidate`'s dry-run) can report WHICH mechanism mapped a lane:
// an already-canonical/instance-fold match, the `-agent` lane-fold, or an explicit
// lane_aliases entry.
type LaneMatch int

const (
	// LaneNone is the zero value — no match (ok=false).
	LaneNone LaneMatch = iota
	// LaneCanonical is the existing fold (gate.CanonicalRole: instance counters +
	// `-relief`) + FixedRoles + roster match — step 1, runs first.
	LaneCanonical
	// LaneAgentFold is a match found only after stripping a trailing `-agent` —
	// step 2.
	LaneAgentFold
	// LaneAlias is a match found only via the lane_aliases map — step 3.
	LaneAlias
)

// matchKnownLane is the step-1 resolver core: fold name via gate.CanonicalRole
// (the SAME instance-counter + `-relief` fold the gate's layer-independence rule
// uses, so this resolver can never disagree with it about what counts as the same
// role), then match the folded OR raw name against the short forms, FixedRoles,
// and the project roster (known). Returns the full canonical LANE name.
func matchKnownLane(name string, known []string) (string, bool) {
	folded := gate.CanonicalRole(name)
	if full, isShort := shortFormLane[folded]; isShort {
		return full, true
	}
	for _, r := range FixedRoles {
		if folded == r || name == r {
			return r, true
		}
	}
	for _, r := range known {
		if folded == r || name == r {
			return r, true
		}
	}
	return "", false
}

// ResolveLane resolves name — a live agent/instance name (e.g. "cr-1-3",
// "crit-2r", "ci-infra-1"), an owner-instance `<role>-agent` variant lane (e.g.
// "devops-agent", "ci-pipeline-agent"), an ephemeral/per-owner-instance name (e.g.
// "fix-1161", "owner-479a"), or an already-canonical role name — to its canonical
// LANE (the full role name the queue/<role>/ directory role-keyed lanes route to),
// AND reports which mechanism matched (see LaneMatch).
//
// Resolution order — each step runs only if the previous left it unmatched:
//  1. LaneCanonical — the existing fold (gate.CanonicalRole) + short forms +
//     FixedRoles + known (roster). An already-canonical or instance-countered name
//     — AND an explicitly-declared roster role that literally ends in "-agent" —
//     match exactly as before, because this step runs FIRST (the `-agent` strip
//     below can never hijack a roster role that really is named "<x>-agent").
//  2. LaneAgentFold — strip a single trailing "-agent" and retry the match:
//     "devops-agent" → "devops", "qa-agent" → "qa", "ci-pipeline-agent" →
//     "ci-pipeline". The `-agent` owner-instance naming convention folds lane-side
//     against the roster with ZERO per-lane config.
//  3. LaneAlias — consult aliases (see CanonicalLane) for the raw name AND its
//     folded form. The alias VALUE must itself be a known role (FixedRole or
//     roster type); a dangling alias (target not known) is REFUSED here (ok=false)
//     rather than silently minting a phantom lane — see InvalidLaneAliases, which
//     surfaces such misconfigurations for doctor. An alias never OVERRIDES a
//     step-1/step-2 match (it can never redirect e.g. "code-review").
//
// This only decides where a ticket's queue ENTRY lives; it never touches
// gate.CanonicalRole or any verdict logic (role-keyed-lanes.md's hard constraint).
func ResolveLane(name string, known []string, aliases map[string]string) (role string, match LaneMatch, ok bool) {
	// Step 1: existing/explicit match — runs first, so a roster role literally
	// named "foo-agent" keeps mapping to "foo-agent" (the strip can't hijack it).
	if r, matched := matchKnownLane(name, known); matched {
		return r, LaneCanonical, true
	}
	// Step 2: `-agent` LANE-fold — strip a single trailing "-agent" and retry.
	if base, cut := strings.CutSuffix(name, agentLaneSuffix); cut && base != "" {
		if r, matched := matchKnownLane(base, known); matched {
			return r, LaneAgentFold, true
		}
	}
	// Step 3: lane_aliases — match the raw name AND its folded form; the target
	// must be a known role, else the alias is dangling and refused.
	if len(aliases) > 0 {
		folded := gate.CanonicalRole(name)
		for _, cand := range []string{name, folded} {
			target, has := aliases[cand]
			if !has {
				continue
			}
			if r, matched := matchKnownLane(target, known); matched {
				return r, LaneAlias, true
			}
			// dangling alias (target not a known role) — refuse; no phantom lane.
			break
		}
	}
	return "", LaneNone, false
}

// CanonicalLane resolves name to its canonical LANE, discarding ResolveLane's
// match-mechanism detail — the resolver send-to-review/gate-verdict/assign/the
// viewer/doctor's queue-lanes check all route through. aliases is the project's
// optional lane_aliases map (raw-or-folded lane name → canonical role); pass nil
// when a caller has none — behavior is then byte-identical to the pre-aliases
// resolver plus the always-on `-agent` lane-fold. See ResolveLane for the full
// resolution order and constraints.
//
// known is the additional canonical role set declared by the project — owner/
// specialist roles (e.g. "ci-infra", "devops", "qa") from the roster — beyond the
// always-implicit FixedRoles; callers need not repeat FixedRoles in it (see
// KnownRoles for building this list from a roster's role names).
//
// ok is false for anything left over: an ephemeral or per-owner-instance name
// (fix-1161, spike-merge, owner-479a) with no canonical-role home, and a lane whose
// only candidate was a dangling alias. Callers should flag it for human mapping
// (see `queue consolidate`) rather than silently giving it a lane of its own.
func CanonicalLane(name string, known []string, aliases map[string]string) (role string, ok bool) {
	r, _, matched := ResolveLane(name, known, aliases)
	return r, matched
}

// InvalidLaneAliases returns the alias KEYS in aliases whose target role is NOT a
// known role (a FixedRole or a roster type in known) — a dangling alias that would
// map a lane onto a phantom role. ResolveLane already REFUSES to honor these
// (ok=false, no silent phantom lane); this surfaces them, sorted, for validation
// (`leadkit doctor` warns on a mis-typed alias before consolidate ever runs).
// Returns nil for an empty/all-valid map.
func InvalidLaneAliases(aliases map[string]string, known []string) []string {
	var bad []string
	for k, target := range aliases {
		if _, ok := matchKnownLane(target, known); !ok {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// KnownRoles unions extra (a project's declared owner/specialist role names, e.g.
// from config.Profile.Roster's Type values) with FixedRoles, deduped and in a
// stable order (FixedRoles first, then extra in the order given) — the `known`
// slice CanonicalLane wants when a caller has a project roster to feed it, so each
// call site doesn't have to build this union by hand.
func KnownRoles(extra []string) []string {
	seen := make(map[string]bool, len(FixedRoles)+len(extra))
	out := make([]string, 0, len(FixedRoles)+len(extra))
	for _, r := range FixedRoles {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range extra {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
