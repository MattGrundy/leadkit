package burn

import (
	"strings"
	"testing"
)

func TestApplyPreemption_LowShedToZeroCriticalUntouched(t *testing.T) {
	sessions := []SessionWorkstream{
		{Key: "proj/low-sess", N: 3, Priority: PriorityLow},
		{Key: "proj/crit-sess", N: 4, Priority: PriorityCritical},
	}
	// total=7, afford only 4 -> shed 3, exactly low's whole count.
	low := ApplyPreemption(sessions, "proj/low-sess", 4)
	if !low.Shed || low.N != 0 {
		t.Errorf("low = %+v, want Shed=true N=0", low)
	}
	if !strings.Contains(low.Reason, "YIELD") {
		t.Errorf("low.Reason = %q, want it to say YIELD", low.Reason)
	}

	crit := ApplyPreemption(sessions, "proj/crit-sess", 4)
	if crit.Shed || crit.N != 4 {
		t.Errorf("critical = %+v, want Shed=false N=4 (untouched — preemptive protection)", crit)
	}
}

func TestApplyPreemption_ThreeTiersShedStrictlyLowThenNormalThenHigh(t *testing.T) {
	sessions := []SessionWorkstream{
		{Key: "a/low", N: 2, Priority: PriorityLow},
		{Key: "b/normal", N: 2, Priority: PriorityNormal},
		{Key: "c/high", N: 2, Priority: PriorityHigh},
	}
	// total=6, afford 3 -> shed 3: low fully (2), then 1 off normal, high untouched.
	low := ApplyPreemption(sessions, "a/low", 3)
	if low.N != 0 || !low.Shed {
		t.Errorf("low = %+v, want N=0 Shed=true (fully zeroed first)", low)
	}
	normal := ApplyPreemption(sessions, "b/normal", 3)
	if normal.N != 1 || !normal.Shed {
		t.Errorf("normal = %+v, want N=1 Shed=true (partially shed only after low is zero)", normal)
	}
	high := ApplyPreemption(sessions, "c/high", 3)
	if high.N != 2 || high.Shed {
		t.Errorf("high = %+v, want N=2 Shed=false (untouched — lower tiers weren't enough to need it)", high)
	}
}

func TestApplyPreemption_CriticalShedOnlyAsLastResort(t *testing.T) {
	sessions := []SessionWorkstream{
		{Key: "a/low", N: 1, Priority: PriorityLow},
		{Key: "b/crit", N: 5, Priority: PriorityCritical},
	}
	// total=6, afford only 2 -> shedding low's entire 1 still isn't enough (need
	// to shed 4); critical must absorb the remaining 3 (last resort).
	low := ApplyPreemption(sessions, "a/low", 2)
	if low.N != 0 {
		t.Errorf("low = %+v, want N=0 (fully zeroed first, still not enough)", low)
	}
	crit := ApplyPreemption(sessions, "b/crit", 2)
	if crit.N != 2 || !crit.Shed {
		t.Errorf("crit = %+v, want N=2 Shed=true — only shed because every lower tier was already zero and it still wasn't enough", crit)
	}
}

// --- byte-identical / backward-compat no-op cases ---

func TestApplyPreemption_SingleSessionIsNoOp(t *testing.T) {
	sessions := []SessionWorkstream{{Key: "solo", N: 3, Priority: PriorityLow}}
	got := ApplyPreemption(sessions, "solo", 1) // "over budget" per accountTarget, but nothing to preempt against
	if got.Shed || got.N != 3 {
		t.Errorf("single session = %+v, want a no-op (N unchanged, Shed=false) — nothing to preempt FOR", got)
	}
}

func TestApplyPreemption_EmptyRegistryIsNoOp(t *testing.T) {
	got := ApplyPreemption(nil, "anything", 1)
	if got.Shed || got.N != 0 {
		t.Errorf("empty registry = %+v, want a no-op", got)
	}
}

func TestApplyPreemption_AllNormalIsNoOpEvenOverBudget(t *testing.T) {
	sessions := []SessionWorkstream{
		{Key: "a/s1", N: 3, Priority: PriorityNormal},
		{Key: "b/s2", N: 3, Priority: PriorityNormal},
	}
	// total=6, way over a target of 2 — but every session is the SAME tier, so
	// this must be a no-op: Advise's own symmetric ±1 stepping governs alone,
	// exactly as it did before priority existed.
	for _, key := range []string{"a/s1", "b/s2"} {
		got := ApplyPreemption(sessions, key, 2)
		if got.Shed {
			t.Errorf("key %s: got %+v, want Shed=false — an all-normal account must never preempt", key, got)
		}
	}
}

func TestApplyPreemption_UnderBudgetIsNoOpEvenWithMixedTiers(t *testing.T) {
	sessions := []SessionWorkstream{
		{Key: "a/low", N: 1, Priority: PriorityLow},
		{Key: "b/crit", N: 1, Priority: PriorityCritical},
	}
	got := ApplyPreemption(sessions, "a/low", 5) // plenty of headroom
	if got.Shed || got.N != 1 {
		t.Errorf("under budget = %+v, want a no-op", got)
	}
}

func TestClampPriority(t *testing.T) {
	cases := []struct {
		declared, ceiling PriorityTier
		want              PriorityTier
	}{
		{PriorityCritical, PriorityHigh, PriorityHigh},         // above ceiling -> clamped
		{PriorityLow, PriorityHigh, PriorityLow},               // below ceiling -> unchanged
		{PriorityNormal, PriorityNormal, PriorityNormal},       // exactly at ceiling -> unchanged
		{PriorityCritical, PriorityCritical, PriorityCritical}, // ceiling=critical -> no clamp, ever
		{"CRITICAL", PriorityHigh, PriorityHigh},               // case trick -> still clamped
		{"nonsense", PriorityLow, PriorityLow},                 // garbage declared normalizes to normal, then clamped down to the low ceiling
		{PriorityLow, "garbage-ceiling", PriorityLow},          // garbage ceiling normalizes to normal -> below-normal declared stays unchanged
	}
	for _, c := range cases {
		if got := ClampPriority(c.declared, c.ceiling); got != c.want {
			t.Errorf("ClampPriority(%q, %q) = %q, want %q", c.declared, c.ceiling, got, c.want)
		}
	}
}

func TestAnyCriticalCollision(t *testing.T) {
	one := []SessionWorkstream{{Key: "a", Priority: PriorityCritical}, {Key: "b", Priority: PriorityLow}}
	if AnyCriticalCollision(one) {
		t.Error("one critical session must not be flagged")
	}
	two := []SessionWorkstream{{Key: "a", Priority: PriorityCritical}, {Key: "b", Priority: PriorityCritical}}
	if !AnyCriticalCollision(two) {
		t.Error("two critical sessions must be flagged — the self-promotion smell")
	}
}

func TestNormalizePriority(t *testing.T) {
	cases := map[string]PriorityTier{
		"":         PriorityNormal,
		"normal":   PriorityNormal,
		"Low":      PriorityLow,
		" HIGH ":   PriorityHigh,
		"critical": PriorityCritical,
		"CRITICAL": PriorityCritical,
		"nonsense": PriorityNormal,
		"cRiTiCaL": PriorityCritical,
	}
	for in, want := range cases {
		if got := NormalizePriority(in); got != want {
			t.Errorf("NormalizePriority(%q) = %q, want %q", in, got, want)
		}
	}
}
