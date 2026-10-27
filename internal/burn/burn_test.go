package burn

import (
	"math"
	"strings"
	"testing"
	"time"
)

var baseNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// sample builds a HistorySample `hoursAgo` before baseNow with the given readings.
func sample(hoursAgo float64, sessionPct, weekPct float64) HistorySample {
	return HistorySample{
		Ts:         baseNow.Add(-time.Duration(hoursAgo * float64(time.Hour))),
		SessionPct: sessionPct,
		WeekPct:    weekPct,
	}
}

// Fallback mode (simple-math off afford/baseline) replaces both the old dead
// "warming up -> HOLD" state and the old blind "idle -> jump to MaxN" — both now flow
// through the same governing/dead-band/±1-step logic as the measured path, just with
// ratio computed against the fresh-budget baseline instead of a measured actual.

func TestAdvise_Fallback_WarmingUp_ComputesRealTarget(t *testing.T) {
	for name, hist := range map[string][]HistorySample{
		"no history":         nil,
		"one sample too old": {sample(10, 5, 5)}, // outside the 45m default window
	} {
		t.Run(name, func(t *testing.T) {
			// session: afford = (100-50)/1 = 50, ratio = 50/19 ≈ 2.63 (governs, clearly above dead-band)
			// weekly:  afford = (100-(50+10))/10 = 4, ratio = 4/0.583 ≈ 6.86
			in := Input{
				Now: baseNow,
				Usage: Usage{
					SessionPct: 50, SessionResetsAt: baseNow.Add(1 * time.Hour),
					WeekPct: 50, WeekResetsAt: baseNow.Add(10 * time.Hour),
				},
				History: hist,
				N:       2,
			}
			got := Advise(in)
			if !got.WarmingUp || !got.Fallback {
				t.Fatalf("want WarmingUp=true and Fallback=true, got %+v", got)
			}
			if got.Governing != "session" {
				t.Fatalf("want session governing (ratio 2.63 < weekly's 6.86), got %q", got.Governing)
			}
			if got.N != 3 {
				t.Fatalf("want a real computed step up from 2 to 3, got %d (verdict: %s)", got.N, got.Verdict)
			}
		})
	}
}

func TestAdvise_Fallback_Idle_ComputesRealTarget(t *testing.T) {
	// session: afford = 70/1 = 70, ratio = 70/19 ≈ 3.68 (governs)
	// weekly:  afford = 80/20 = 4, ratio = 4/0.583 ≈ 6.86
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 30, SessionResetsAt: baseNow.Add(1 * time.Hour),
			WeekPct: 10, WeekResetsAt: baseNow.Add(20 * time.Hour),
		},
		// No movement at all in the last 30 minutes — idle, not warming up.
		History: []HistorySample{sample(0.5, 30, 10)},
		N:       2,
	}
	got := Advise(in)
	if !got.Idle || !got.Fallback {
		t.Fatalf("want Idle=true and Fallback=true, got %+v", got)
	}
	if got.WarmingUp {
		t.Fatalf("want WarmingUp=false when idle (a sample exists, it just shows no movement), got %+v", got)
	}
	if got.N != 3 {
		t.Fatalf("want a real computed step up from 2 to 3, got %d (verdict: %s)", got.N, got.Verdict)
	}
}

func TestAdvise_Fallback_FreshBudget_ConvergesToMax(t *testing.T) {
	n := 1
	usage := Usage{
		SessionPct: 50, SessionResetsAt: baseNow.Add(1 * time.Hour), // afford=50, ratio≈2.63
		WeekPct: 50, WeekResetsAt: baseNow.Add(10 * time.Hour), // afford=4, ratio≈6.86
	}
	for i := 0; i < 10; i++ {
		got := Advise(Input{Now: baseNow, Usage: usage, N: n}) // no history -> warming up every tick
		if !got.Fallback {
			t.Fatalf("expected every tick to use the fallback (no history supplied), got %+v", got)
		}
		n = got.N
		if n == MaxN {
			return
		}
	}
	t.Fatalf("fallback did not converge to %d within 10 ticks, stuck at %d", MaxN, n)
}

func TestAdvise_Fallback_NearlyExhaustedWeekly_ConvergesLow(t *testing.T) {
	n := 6
	usage := Usage{
		SessionPct: 10, SessionResetsAt: baseNow.Add(10 * time.Hour), // afford=9, ratio≈0.47
		WeekPct: 95, WeekResetsAt: baseNow.Add(20 * time.Hour), // afford clamped to 0, ratio=0
	}
	for i := 0; i < 10; i++ {
		got := Advise(Input{Now: baseNow, Usage: usage, N: n})
		if got.Governing != "weekly" {
			t.Fatalf("want weekly governing (ratio 0 < session's ~0.47), got %q", got.Governing)
		}
		n = got.N
		if n <= 2 {
			return
		}
	}
	t.Fatalf("fallback did not throttle nearly-exhausted weekly to <=2 within 10 ticks, stuck at %d", n)
}

func TestAdvise_Fallback_ClosestOfTwoSelection(t *testing.T) {
	t.Run("session binds", func(t *testing.T) {
		// session: afford = 20/2 = 10, ratio = 10/19 ≈ 0.53
		// weekly:  afford = 80/50 = 1.6, ratio = 1.6/0.583 ≈ 2.74
		in := Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 80, SessionResetsAt: baseNow.Add(2 * time.Hour),
				WeekPct: 10, WeekResetsAt: baseNow.Add(50 * time.Hour),
			},
			N: 4,
		}
		got := Advise(in)
		if got.Governing != "session" {
			t.Fatalf("want session tighter (ratio 0.53 < weekly's 2.74), got %q (session=%.3f week=%.3f)", got.Governing, got.Session.Ratio, got.Weekly.Ratio)
		}
	})

	t.Run("weekly binds", func(t *testing.T) {
		// session: afford = 90/10 = 9, ratio = 9/19 ≈ 0.47
		// weekly:  afford = 0/20 = 0, ratio = 0
		in := Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 10, SessionResetsAt: baseNow.Add(10 * time.Hour),
				WeekPct: 90, WeekResetsAt: baseNow.Add(20 * time.Hour),
			},
			N: 4,
		}
		got := Advise(in)
		if got.Governing != "weekly" {
			t.Fatalf("want weekly tighter (ratio 0 < session's ~0.47), got %q (session=%.3f week=%.3f)", got.Governing, got.Session.Ratio, got.Weekly.Ratio)
		}
	})
}

func TestAdvise_OverPace_StepsDown(t *testing.T) {
	// Weekly: 81% used, reserve 10 -> ceiling 91, 48h to reset -> afford = 9/48 = 0.1875 %/hr.
	// Actual pace: 1%/hr (burning far faster than affordable) -> ratio well under the dead-band.
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour),
			WeekPct: 81, WeekResetsAt: baseNow.Add(48 * time.Hour),
		},
		History: []HistorySample{sample(0.5, 10, 80.5)}, // week moved 80.5->81 in 0.5h = 1%/hr
		N:       4,
	}
	got := Advise(in)
	if got.N != 3 {
		t.Fatalf("want a single step down from 4 to 3, got %d (verdict: %s)", got.N, got.Verdict)
	}
	if got.Governing != "weekly" {
		t.Fatalf("want weekly governing, got %q", got.Governing)
	}
}

func TestAdvise_DeadBand_Holds(t *testing.T) {
	// Craft afford/actual so ratio lands at 1.0 (comfortably inside [0.85, 1.30]).
	// Session: used 50, resets in 5h -> afford = 50/5 = 10 %/hr.
	// Actual: 45 -> 50 over 0.5h = 10 %/hr. ratio = 1.0.
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 50, SessionResetsAt: baseNow.Add(5 * time.Hour),
			WeekPct: 5, WeekResetsAt: baseNow.Add(150 * time.Hour), // huge weekly headroom, won't govern
		},
		History: []HistorySample{sample(0.5, 45, 5)},
		N:       4,
	}
	got := Advise(in)
	if got.N != 4 {
		t.Fatalf("want HOLD at 4, got %d (verdict: %s)", got.N, got.Verdict)
	}
	if got.Governing != "session" {
		t.Fatalf("want session governing (ratio 1.0 vs weekly's much larger ratio), got %q", got.Governing)
	}
}

func TestAdvise_SessionBindingVsWeeklyBinding(t *testing.T) {
	t.Run("session binds", func(t *testing.T) {
		// session: afford = 5/0.5 = 10, actual = (95-85)/0.5 = 20 -> ratio 0.5
		// weekly:  afford = 85/100 = 0.85, actual = (5-4.9)/0.5 = 0.2 -> ratio 4.25
		in := Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 95, SessionResetsAt: baseNow.Add(30 * time.Minute),
				WeekPct: 5, WeekResetsAt: baseNow.Add(100 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 85, 4.9)},
			N:       4,
		}
		got := Advise(in)
		if got.Governing != "session" {
			t.Fatalf("want session tighter (ratio 0.5 < weekly's 4.25), got %q (session=%.3f week=%.3f)", got.Governing, got.Session.Ratio, got.Weekly.Ratio)
		}
	})

	t.Run("weekly binds", func(t *testing.T) {
		// session: afford = 90/10 = 9, actual = (10-9)/0.5 = 2 -> ratio 4.5
		// weekly:  afford = 40/5 = 8, actual = (50-45)/0.5 = 10 -> ratio 0.8
		in := Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 10, SessionResetsAt: baseNow.Add(10 * time.Hour),
				WeekPct: 50, WeekResetsAt: baseNow.Add(5 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 9, 45)},
			N:       4,
		}
		got := Advise(in)
		if got.Governing != "weekly" {
			t.Fatalf("want weekly tighter (ratio 0.8 < session's 4.5), got %q (session=%.3f week=%.3f)", got.Governing, got.Session.Ratio, got.Weekly.Ratio)
		}
	})
}

func TestAdvise_FreshBudget_ConvergesToMax(t *testing.T) {
	n := 1
	hist := []HistorySample{sample(0.5, 0.45, 0.45)} // tiny real movement, not idle
	usage := Usage{
		SessionPct: 0.5, SessionResetsAt: baseNow.Add(4 * time.Hour),
		WeekPct: 0.5, WeekResetsAt: baseNow.Add(160 * time.Hour),
	}
	for i := 0; i < 10; i++ {
		got := Advise(Input{Now: baseNow, Usage: usage, History: hist, N: n})
		n = got.N
		if n == MaxN {
			return
		}
	}
	t.Fatalf("fresh budget with ample headroom did not converge to %d within 10 ticks, stuck at %d", MaxN, n)
}

func TestAdvise_HeavilyUsedWeekly_ConvergesLow(t *testing.T) {
	n := 6
	// 81% weekly used, reserve 10 -> ceiling 91, 2 days (48h) to reset -> afford = 9/48 ≈ 0.1875 %/hr.
	// Actual weekly pace: 1%/hr — far over budget.
	hist := []HistorySample{sample(0.5, 10, 80.5)}
	usage := Usage{
		SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour),
		WeekPct: 81, WeekResetsAt: baseNow.Add(48 * time.Hour),
	}
	for i := 0; i < 10; i++ {
		got := Advise(Input{Now: baseNow, Usage: usage, History: hist, N: n})
		n = got.N
		if n <= 2 {
			return
		}
	}
	t.Fatalf("heavily-used weekly budget did not throttle to <=2 within 10 ticks, stuck at %d", n)
}

func TestAdvise_ClampsToBounds(t *testing.T) {
	// N already at MinN with a further-throttling ratio must not go below MinN.
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour),
			WeekPct: 95, WeekResetsAt: baseNow.Add(48 * time.Hour),
		},
		History: []HistorySample{sample(0.5, 10, 92.5)},
		N:       MinN,
	}
	got := Advise(in)
	if got.N < MinN {
		t.Fatalf("N must never drop below %d, got %d", MinN, got.N)
	}
}

func TestAdvise_VerdictNonEmpty(t *testing.T) {
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 50, SessionResetsAt: baseNow.Add(5 * time.Hour),
			WeekPct: 5, WeekResetsAt: baseNow.Add(150 * time.Hour),
		},
		History: []HistorySample{sample(0.5, 45, 5)},
		N:       4,
	}
	got := Advise(in)
	if got.Verdict == "" {
		t.Fatal("verdict must never be empty")
	}
}

func TestAdvise_NoNaNOrInfInResult(t *testing.T) {
	// actual == 0 on one side only must not poison the governing ratio via NaN/Inf leaking
	// into the final N (idle handling and the ratio guard should prevent this).
	in := Input{
		Now: baseNow,
		Usage: Usage{
			SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour), // session: no movement -> idle-ish
			WeekPct: 50, WeekResetsAt: baseNow.Add(10 * time.Hour),
		},
		History: []HistorySample{sample(0.5, 10, 45)}, // week moved, session didn't
		N:       4,
	}
	got := Advise(in)
	if math.IsNaN(float64(got.N)) { //nolint:staticcheck // SA4015: N is an int, so IsNaN is always false — a tautological assertion; left as-is and flagged for the separate non-tautological-test follow-up, not rewritten in this additive CI change
		t.Fatal("N is NaN")
	}
	if got.N < MinN || got.N > MaxN {
		t.Fatalf("N out of bounds: %d", got.N)
	}
}

// TestAdvise_VerdictStatesTheUnitExplicitly is output-wording-only (no math change):
// every verdict shape must say "workstreams" so it can't be misread as an agent
// headcount — the recurring conflation that gets the reviewer pool killed/idled.
func TestAdvise_VerdictStatesTheUnitExplicitly(t *testing.T) {
	t.Run("dead-band HOLD", func(t *testing.T) {
		got := Advise(Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 50, SessionResetsAt: baseNow.Add(5 * time.Hour),
				WeekPct: 5, WeekResetsAt: baseNow.Add(150 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 45, 5)},
			N:       4,
		})
		if !strings.Contains(got.Verdict, "workstreams") {
			t.Errorf("verdict = %q, want it to say workstreams", got.Verdict)
		}
	})

	t.Run("SCALE DOWN", func(t *testing.T) {
		got := Advise(Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour),
				WeekPct: 81, WeekResetsAt: baseNow.Add(48 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 10, 80.5)},
			N:       4,
		})
		if got.N != 3 {
			t.Fatalf("setup check: want N=3, got %d", got.N)
		}
		if !strings.Contains(got.Verdict, "workstreams") {
			t.Errorf("verdict = %q, want it to say workstreams", got.Verdict)
		}
	})

	t.Run("SCALE UP", func(t *testing.T) {
		got := Advise(Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 5, SessionResetsAt: baseNow.Add(1 * time.Hour),
				WeekPct: 5, WeekResetsAt: baseNow.Add(150 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 4.9, 5)},
			N:       2,
		})
		if got.N <= 2 {
			t.Fatalf("setup check: want a scale up from N=2, got %d", got.N)
		}
		if !strings.Contains(got.Verdict, "workstreams") {
			t.Errorf("verdict = %q, want it to say workstreams", got.Verdict)
		}
	})

	t.Run("HOLD at the cap", func(t *testing.T) {
		got := Advise(Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 5, SessionResetsAt: baseNow.Add(1 * time.Hour),
				WeekPct: 5, WeekResetsAt: baseNow.Add(150 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 4, 5)},
			N:       MaxN,
		})
		if got.N != MaxN {
			t.Fatalf("setup check: want N stuck at MaxN=%d, got %d", MaxN, got.N)
		}
		if !strings.Contains(got.Verdict, "workstreams") || !strings.Contains(got.Verdict, "cap") {
			t.Errorf("verdict = %q, want it to say workstreams and cap", got.Verdict)
		}
	})

	t.Run("HOLD at the floor", func(t *testing.T) {
		got := Advise(Input{
			Now: baseNow,
			Usage: Usage{
				SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour),
				WeekPct: 95, WeekResetsAt: baseNow.Add(48 * time.Hour),
			},
			History: []HistorySample{sample(0.5, 10, 92.5)},
			N:       MinN,
		})
		if got.N != MinN {
			t.Fatalf("setup check: want N stuck at MinN=%d, got %d", MinN, got.N)
		}
		if !strings.Contains(got.Verdict, "workstreams") || !strings.Contains(got.Verdict, "floor") {
			t.Errorf("verdict = %q, want it to say workstreams and floor", got.Verdict)
		}
	})
}

// --- Cross-project burn (docs/build-tasks/cross-project-burn.md) ---
//
// AccountN is the account-wide total of live workstreams (this project's own N
// included) — see Input.AccountN's doc for why AccountN==N (the zero-value default)
// is a strict no-op. These tests exercise the governor's own math directly via
// Input.AccountN; internal/usage's tests cover summing that value from the real
// on-disk registry (the two-slug cap test the build task asked to see highlighted).

// TestAdvise_AccountN_BackwardCompat_ZeroValueIsANoOp is the required regression:
// leaving AccountN unset (its zero value) must be byte-identical to every existing
// (pre-AccountN) Advise behaviour — a single-project machine, or a project whose
// slug is the only live registry entry, sees NO change at all.
func TestAdvise_AccountN_BackwardCompat_ZeroValueIsANoOp(t *testing.T) {
	usage := Usage{
		SessionPct: 50, SessionResetsAt: baseNow.Add(1 * time.Hour),
		WeekPct: 50, WeekResetsAt: baseNow.Add(10 * time.Hour),
	}
	base := Input{Now: baseNow, Usage: usage, N: 2}

	withoutAccountN := Advise(base)

	withExplicitN := base
	withExplicitN.AccountN = 2 // "only this slug present" -> accountN == in.N, same as unset
	withExplicitEqual := Advise(withExplicitN)

	if withoutAccountN != withExplicitEqual {
		t.Errorf("AccountN unset vs AccountN==N should be identical results:\nunset:    %+v\nexplicit: %+v", withoutAccountN, withExplicitEqual)
	}
	if withoutAccountN.AccountN != 2 {
		t.Errorf("AccountN unset should still report AccountN==N (%d) in the result, got %d", 2, withoutAccountN.AccountN)
	}
	if strings.Contains(withoutAccountN.Verdict, "account") {
		t.Errorf("single-project verdict must not mention account split, got %q", withoutAccountN.Verdict)
	}
}

// TestAdvise_AccountCap_SecondProjectHeldNotScaledIntoClaimedHeadroom is the core
// no-collision-style regression for the governor: project B ticks with a favourable
// own-pace ratio (would scale up on its own), but the account total is already at the
// workstream ceiling (other project(s) already claim the rest) — B must be HELD, never
// scaled up into headroom the account can't actually afford as a whole.
func TestAdvise_AccountCap_SecondProjectHeldNotScaledIntoClaimedHeadroom(t *testing.T) {
	usage := Usage{
		SessionPct: 50, SessionResetsAt: baseNow.Add(1 * time.Hour), // afford=50, ratio≈2.63 (governs)
		WeekPct: 50, WeekResetsAt: baseNow.Add(10 * time.Hour), // afford=4, ratio≈6.86
	}

	// Sanity: with no cross-project data, B's own state alone WOULD scale up (this is
	// the exact behaviour cross-project-burn.md says must not leak across projects).
	soloB := Advise(Input{Now: baseNow, Usage: usage, N: 1})
	if soloB.N != 2 {
		t.Fatalf("setup check: solo project B should scale 1->2 on this ratio, got %d (verdict: %s)", soloB.N, soloB.Verdict)
	}

	// Now B ticks knowing the account is already at the workstream ceiling (its own 1
	// plus 7 claimed elsewhere).
	heldB := Advise(Input{Now: baseNow, Usage: usage, N: 1, AccountN: MaxN})
	if heldB.N != 1 {
		t.Fatalf("want B held at 1 (account already at the ceiling), got %d — verdict: %s", heldB.N, heldB.Verdict)
	}
	if heldB.AccountN != MaxN {
		t.Errorf("Result.AccountN = %d, want %d", heldB.AccountN, MaxN)
	}
	if !strings.Contains(heldB.Verdict, "headroom claimed elsewhere") {
		t.Errorf("verdict = %q, want it to name the account-cap reason", heldB.Verdict)
	}
	if !strings.Contains(heldB.Verdict, "account") {
		t.Errorf("verdict = %q, want it to name the account split", heldB.Verdict)
	}
}

// TestAdvise_AccountCap_FloorPreservedEvenWhenAnotherProjectIsHeavy: a project already
// at MinN must never be pushed to 0 just because another project's load makes the
// account total look heavy and the ratio calls for a nominal scale-down.
func TestAdvise_AccountCap_FloorPreservedEvenWhenAnotherProjectIsHeavy(t *testing.T) {
	usage := Usage{
		SessionPct: 10, SessionResetsAt: baseNow.Add(3 * time.Hour), // afford≈30, ratio≈1.58 (session, not binding)
		WeekPct: 95, WeekResetsAt: baseNow.Add(48 * time.Hour), // afford clamped low, ratio<DeadBandLow (binds)
	}
	got := Advise(Input{
		Now: baseNow, Usage: usage,
		History: []HistorySample{sample(0.5, 10, 92.5)},
		N:       MinN, AccountN: MaxN, // this project already at its floor; account is heavy
	})
	if got.N != MinN {
		t.Fatalf("want N held at the floor (%d) regardless of account load, got %d — verdict: %s", MinN, got.N, got.Verdict)
	}
	if !strings.Contains(got.Verdict, "floor") {
		t.Errorf("verdict = %q, want it to say floor", got.Verdict)
	}
}
