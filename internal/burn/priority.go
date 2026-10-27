package burn

// Per-session priority + preemptive shedding (session-priority-burn.md): under
// account budget pressure, ADVISE (above) already shrinks THIS project's own
// count ±1 toward a shared target — symmetric, priority-blind. ApplyPreemption
// is a SEPARATE, additive layer on top: when the live sessions on the account
// actually carry DIFFERENT priority tiers, it decides whether THIS session in
// particular should be preemptively driven toward zero to protect a
// higher-priority one, using the EXACT afforded ceiling (Result.AccountTarget)
// Advise's own math already computed for this tick.
//
// DELIBERATELY A NO-OP whenever there's nothing to preempt FOR: fewer than 2
// live sessions, every live session sharing one priority tier (the common
// "everyone is normal" case), or the account total already fitting the
// afforded ceiling. That's what makes the single-session / all-normal /
// empty-registry case byte-identical to pre-priority burn — Advise's own ±1
// stepping is left as the ONLY thing governing those cases, unchanged.

import (
	"fmt"
	"sort"
	"strings"
)

// PriorityTier is one of the four self-declared session priority tiers.
// String order is NOT rank order — use priorityRank (via less) for that.
type PriorityTier string

const (
	PriorityLow      PriorityTier = "low"
	PriorityNormal   PriorityTier = "normal"
	PriorityHigh     PriorityTier = "high"
	PriorityCritical PriorityTier = "critical"
)

// priorityRank is the shed order: LOWER rank sheds FIRST. Ties (same tier)
// have no defined order between sessions beyond Key, so within one tier
// ApplyPreemption sheds in Key order — arbitrary but deterministic.
var priorityRank = map[PriorityTier]int{
	PriorityLow: 0, PriorityNormal: 1, PriorityHigh: 2, PriorityCritical: 3,
}

// NormalizePriority maps a raw declared string (case/space-insensitive) to a
// known PriorityTier — anything unrecognized, or empty (unset, or an old
// pre-priority entry), is PriorityNormal. NEVER defaults to critical: an
// absent declaration must never accidentally protect itself from preemption
// by accident of a missing field.
func NormalizePriority(raw string) PriorityTier {
	switch PriorityTier(strings.ToLower(strings.TrimSpace(raw))) {
	case PriorityLow, PriorityHigh, PriorityCritical:
		return PriorityTier(strings.ToLower(strings.TrimSpace(raw)))
	default:
		return PriorityNormal
	}
}

// SessionWorkstream is one live session's registry entry, as the preemption
// math needs it — decoupled from internal/usage's on-disk shape (a plain
// struct the caller wires in, matching this package's existing convention;
// internal/burn does not import internal/usage).
type SessionWorkstream struct {
	Key      string // a stable, unique identifier across the account, e.g. "<slug>/<session_id>"
	N        int
	Priority PriorityTier
}

// ShedResult is what preemptive shedding decided for ONE session (the one
// named by forKey in ApplyPreemption).
type ShedResult struct {
	N      int    // this session's shed-adjusted allowed workstream count
	Shed   bool   // true iff N < that session's own live/declared count
	Reason string // populated iff Shed — an ACTIONABLE yield message, never silent
}

// ApplyPreemption computes priority-ordered preemptive shedding across every
// live session in `sessions` (forKey's own entry must be present in it) and
// returns forKey's resulting ceiling. accountTarget is the SAME afforded-max
// total Advise's own afford->target math produced this tick (Result.
// AccountTarget).
//
// Sessions are shed lowest-priority-tier FIRST, in Key order within a tier,
// each one reduced toward (and potentially all the way to) ZERO, before the
// next tier is touched AT ALL — so a `critical` session is only ever shed if
// every lower tier is already at zero and the account is STILL over the
// target (the explicit "last resort" the spec calls for).
//
// A deliberate no-op (ShedResult{N: <the session's own N>}, Shed=false) when:
// forKey is not found; fewer than 2 live sessions; every live session shares
// ONE priority tier (nothing to preempt FOR — Advise's own ±1 stepping is
// left to govern alone, unchanged); or the live total already fits
// accountTarget. This is what keeps the single-session/all-normal/no-pressure
// cases byte-identical to pre-priority burn.
func ApplyPreemption(sessions []SessionWorkstream, forKey string, accountTarget int) ShedResult {
	var self *SessionWorkstream
	total := 0
	tiers := map[PriorityTier]bool{}
	for i := range sessions {
		total += sessions[i].N
		tiers[sessions[i].Priority] = true
		if sessions[i].Key == forKey {
			self = &sessions[i]
		}
	}
	if self == nil {
		return ShedResult{}
	}
	if len(sessions) < 2 || len(tiers) < 2 || total <= accountTarget {
		return ShedResult{N: self.N}
	}

	ordered := append([]SessionWorkstream(nil), sessions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if priorityRank[ordered[i].Priority] != priorityRank[ordered[j].Priority] {
			return priorityRank[ordered[i].Priority] < priorityRank[ordered[j].Priority]
		}
		return ordered[i].Key < ordered[j].Key
	})

	toShed := total - accountTarget
	remaining := make(map[string]int, len(ordered))
	for _, s := range ordered {
		remaining[s.Key] = s.N
	}
	var lastTouchedTier PriorityTier
	anyShed := false
	for _, s := range ordered {
		if toShed <= 0 {
			break
		}
		cut := s.N
		if cut > toShed {
			cut = toShed
		}
		if cut > 0 {
			remaining[s.Key] = s.N - cut
			toShed -= cut
			lastTouchedTier = s.Priority
			anyShed = true
		}
	}

	newN := remaining[forKey]
	if newN >= self.N {
		return ShedResult{N: self.N}
	}
	reason := fmt.Sprintf(
		"YIELD to n=%d — higher-priority work is claiming the account budget; pause new workstreams until headroom returns.",
		newN,
	)
	if anyShed && lastTouchedTier == self.Priority {
		reason = fmt.Sprintf(
			"YIELD to n=%d — a higher-priority session (or a peer in your own %q tier) is claiming the account budget; pause new workstreams until headroom returns.",
			newN, self.Priority,
		)
	}
	return ShedResult{N: newN, Shed: true, Reason: reason}
}

// ClampPriority returns min(declared, ceiling) by shed-order rank — the
// account-level guardrail (priority-ceiling.md) that closes session-priority-
// burn's self-promotion hole: a session that declares above the account
// owner's ceiling is treated as being AT the ceiling, never above it. Both
// arguments are re-normalized via NormalizePriority defensively (case/
// whitespace/garbage can never sneak a tier past the rank comparison) — so
// callers may pass a raw, unnormalized string cast to PriorityTier safely.
// ApplyPreemption itself is untouched; callers clamp BEFORE building the
// []SessionWorkstream it receives.
func ClampPriority(declared, ceiling PriorityTier) PriorityTier {
	d := NormalizePriority(string(declared))
	c := NormalizePriority(string(ceiling))
	if priorityRank[d] > priorityRank[c] {
		return c
	}
	return d
}

// ShedOrderRank exposes the tier->rank mapping ApplyPreemption sorts by, for
// callers outside this package (`leadkit burn priorities`, doctor) that need
// to sort/report shed order without duplicating the mapping. raw is
// normalized via NormalizePriority first, so any string this accepts too.
func ShedOrderRank(raw string) int {
	return priorityRank[NormalizePriority(raw)]
}

// AnyCriticalCollision reports whether more than one session in sessions
// declares PriorityCritical — the self-promotion smell doctor/burn-priorities
// flag (session-priority-burn.md's flagged tension: self-declared priority
// lets any session claim critical and preemptively starve the rest).
func AnyCriticalCollision(sessions []SessionWorkstream) bool {
	count := 0
	for _, s := range sessions {
		if s.Priority == PriorityCritical {
			count++
		}
	}
	return count > 1
}
