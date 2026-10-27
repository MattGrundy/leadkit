// Package burn implements the parallel-workstream governor: given current account
// usage (session + weekly %) and a short rolling history of past readings, it advises
// how many parallel workstreams (1-8) the lead should run so the team stays under both
// the 5h-session and weekly account limits, leaving a reserve for the user's own use.
//
// Advisory only — Advise never errors and never blocks; callers decide what to do with
// the recommendation.
package burn

import (
	"fmt"
	"math"
	"time"
)

const (
	// MinN and MaxN bound the recommended workstream count.
	MinN = 1
	MaxN = 8

	// DeadBandLow and DeadBandHigh: no change is recommended while the governing
	// ratio falls in this range — avoids flapping on noise.
	DeadBandLow  = 0.85
	DeadBandHigh = 1.30

	// DefaultReserve is the % of weekly budget left untouched for the user's own
	// sessions, applied as an effective ceiling on top of weekly usage.
	DefaultReserve = 10.0

	// DefaultWindow is how far back the rolling history is searched for a prior
	// sample to compute actual pace against.
	DefaultWindow = 45 * time.Minute

	// DefaultWorkstreamTTL is how stale an account-wide workstream-registry entry
	// (docs/build-tasks/cross-project-burn.md) may be before Advise's caller prunes it
	// from the account total — a crashed/idle project's last-written count can't hold
	// phantom headroom forever.
	DefaultWorkstreamTTL = 30 * time.Minute

	// DefaultWorkstreamStaleWindow is how stale an account-wide workstream/presence
	// entry (docs/build-tasks/burn-auto-enforce.md) may be before the GOVERNOR ignores
	// it when computing account_n — tighter than DefaultWorkstreamTTL so a crashed or
	// exited session stops inflating the account count (and stops holding the floor)
	// promptly, without waiting the full registry-prune TTL. The auto-register presence
	// heartbeat is refreshed on every session activity (the usage-logger PreToolUse
	// hook), so a genuinely-live session never falls past this window.
	DefaultWorkstreamStaleWindow = 15 * time.Minute

	// DefaultEnforceInterval is how often the account-level burn-enforce daemon
	// (dev.leadkit.burn, docs/build-tasks/burn-auto-enforce.md) ticks: each tick reads
	// real account weekly usage and writes/clears the account pause lock from the
	// pause_pct/resume_pct hysteresis, so the throttle fires WITHOUT any session running
	// `leadkit burn`. Short because the tick is token-free (one usage read + at most one
	// lock write) and the account-wide pause must not lag real usage crossing the pause
	// threshold.
	DefaultEnforceInterval = 2 * time.Minute

	// DefaultProductivityStallWindow is how long an agent may be ACTIVE (alive +
	// churning) with NO deliverable — no new commit, gate verdict, queue advancement,
	// or session-task→done — before the productivity watcher
	// (docs/build-tasks/productivity-watcher.md) flags a productive-stall ("busy is not
	// productive"). Coarse and generous: this is an advisory diagnostic, not a precise
	// probe, so a brief churn between deliverables never trips it.
	DefaultProductivityStallWindow = 3 * time.Hour

	// DefaultInprogressStallWindow is how long a ticket may sit CLAIMED in an
	// in-progress lane (its symlink mtime) with NO deliverable advancement — no new
	// commit, gate verdict, or queue stage-move — before the frozen-claim detector
	// (docs/build-tasks/frozen-claim-and-noninteractive.md) flags it as a hung owner's
	// frozen claim. DISTINCT from the productivity window (which needs an ACTIVITY
	// signal a hung agent doesn't emit): a frozen claim is a held in-progress ticket
	// standing still. Generous by default so a legitimately long task (a real 45m+
	// build) doesn't nag — the detector is a WARN whose remedy is "probe the owner",
	// and a live owner answers.
	DefaultInprogressStallWindow = 45 * time.Minute

	// idleEpsilon: a measured pace at or below this (%/hr) is treated as idle —
	// i.e. no measurable burn, triggering the baseline fallback below.
	idleEpsilon = 0.01

	// BaselineSessionPerHr / BaselineWeeklyPerHr are the fresh-budget (0% used, full
	// window remaining) paces the user already uses as a crude heuristic (~19%/hr
	// session, ~14%/day weekly). Used as the SIMPLE-MATH fallback denominator when
	// there is no measured actual pace to divide by — see Advise's fallback branch.
	BaselineSessionPerHr = 19.0
	BaselineWeeklyPerHr  = 14.0 / 24.0
)

// Usage is a single point-in-time reading of account utilization.
type Usage struct {
	SessionPct      float64   // five_hour.utilization
	SessionResetsAt time.Time // five_hour.resets_at
	WeekPct         float64   // seven_day.utilization
	WeekResetsAt    time.Time // seven_day.resets_at
}

// HistorySample is one rolling-log entry: a past Usage reading at a point in time.
type HistorySample struct {
	Ts         time.Time
	SessionPct float64
	WeekPct    float64
}

// Input is everything one Advise() tick needs.
type Input struct {
	Now     time.Time
	Usage   Usage
	History []HistorySample // past samples, any order, any age — Advise windows them
	N       int             // current workstream count (THIS project's own)
	Reserve float64         // USER_RESERVE; 0 means "use DefaultReserve"
	Window  time.Duration   // history lookback window; 0 means "use DefaultWindow"

	// AccountN is the account-wide total of live active workstreams across every
	// project sharing this account's budget (this project's own N included) — see
	// docs/build-tasks/cross-project-burn.md. The afford→target computation is run
	// against AccountN, not N alone, so two projects ticking independently don't each
	// scale into the same shared headroom. 0 (or anything < N) means "no cross-project
	// data" and is treated as exactly N — this project's own live count is always
	// authoritative for at least itself, and this is also what makes a single-project
	// setup (no registry, or a registry containing only this project) byte-identical
	// to the pre-AccountN behaviour: AccountN==N reduces every formula below back to
	// the original per-project-only math.
	AccountN int
}

// BudgetResult is the per-budget (session or weekly) math for one tick. Ratio is
// computed against the measured Actual pace when one is available, or against the
// fresh-budget baseline when Fallback is true (see Advise).
type BudgetResult struct {
	Name   string // "session" | "weekly"
	Afford float64
	Actual float64 // measured pace; 0 in fallback mode (no usable delta yet)
	Ratio  float64
}

// Result is one Advise() tick's full output.
type Result struct {
	N         int // recommended count (THIS project's own)
	NPrev     int
	Governing string // "session" | "weekly"
	Session   BudgetResult
	Weekly    BudgetResult
	Fallback  bool // true if Ratio came from the baseline, not a measured pace
	WarmingUp bool // true if Fallback because there's no in-window prior sample yet
	Idle      bool // true if Fallback because measured pace was ~0 on both budgets
	Verdict   string
	// AccountN is the account-wide total Advise actually governed against (see
	// Input.AccountN) — always >= N. Equal to N whenever there's no cross-project data
	// (single project, empty/absent registry, or a registry with only this project).
	AccountN int
	// AccountTarget is the afforded-max account-wide total this tick's afford/
	// governing-ratio math actually computed (accountN*governing.Ratio, clamped
	// [MinN,MaxN]) — exposed so a caller can run priority-ordered preemptive
	// shedding (see ApplyPreemption) against the EXACT same ceiling Advise
	// itself governed by, rather than recomputing it a second, potentially
	// divergent way (session-priority-burn.md).
	AccountTarget int
}

// Advise computes the next recommended workstream count from in.
//
// Two modes, same dead-band/±1-step transition logic either way:
//   - Measured: ≥2 in-window samples exist and at least one budget shows real burn —
//     ratio = afford / actual (the precise mode).
//   - Fallback (simple-math): fewer than 2 samples, or measured pace is ~0 on both
//     budgets — ratio = afford / baseline, using the user's fresh-budget heuristic
//     (~19%/hr session, ~14%/day weekly) as the reference pace. This gives a real,
//     non-degenerate recommendation from a single usage read — no dead "warming up"
//     hold and no blind jump to MaxN on idle.
func Advise(in Input) Result {
	reserve := in.Reserve
	if reserve == 0 {
		reserve = DefaultReserve
	}
	window := in.Window
	if window == 0 {
		window = DefaultWindow
	}
	nPrev := clamp(in.N, MinN, MaxN)

	// accountN is the total this tick actually governs against — floored at nPrev so
	// this project's own live count is always counted for at least itself. That floor
	// is also what makes the whole AccountN mechanism a strict no-op (accountN==nPrev
	// exactly) whenever there's no cross-project data — see Input.AccountN's doc.
	accountN := in.AccountN
	if accountN < nPrev {
		accountN = nPrev
	}
	otherN := accountN - nPrev // other projects' live workstreams, per the registry

	sessionAfford := affordFor(in.Usage.SessionPct, in.Usage.SessionResetsAt, in.Now, 0)
	weeklyAfford := affordFor(in.Usage.WeekPct, in.Usage.WeekResetsAt, in.Now, reserve)

	// One history entry carries both session% and week% at the same timestamp, so
	// there is a single "prior" sample to find, not one per budget.
	prior, ok := priorInWindow(in.History, in.Now, window)

	var session, weekly BudgetResult
	warmingUp := !ok
	idle := false

	if ok {
		hoursSincePrior := in.Now.Sub(prior.Ts).Hours()
		sessionActual := actualFor(in.Usage.SessionPct, prior.SessionPct, hoursSincePrior)
		weeklyActual := actualFor(in.Usage.WeekPct, prior.WeekPct, hoursSincePrior)
		idle = sessionActual <= idleEpsilon && weeklyActual <= idleEpsilon

		if !idle {
			session = BudgetResult{Name: "session", Afford: sessionAfford, Actual: sessionActual, Ratio: ratioOf(sessionAfford, sessionActual)}
			weekly = BudgetResult{Name: "weekly", Afford: weeklyAfford, Actual: weeklyActual, Ratio: ratioOf(weeklyAfford, weeklyActual)}
		}
	}

	fallback := warmingUp || idle
	if fallback {
		session = BudgetResult{Name: "session", Afford: sessionAfford, Actual: 0, Ratio: ratioOf(sessionAfford, BaselineSessionPerHr)}
		weekly = BudgetResult{Name: "weekly", Afford: weeklyAfford, Actual: 0, Ratio: ratioOf(weeklyAfford, BaselineWeeklyPerHr)}
	}

	governing := session
	if weekly.Ratio < session.Ratio {
		governing = weekly
	}

	mode := ""
	if warmingUp {
		mode = "warming up, "
	} else if idle {
		mode = "idle, "
	}

	// Unit made explicit in every verdict below (workstreams, not agents) — this is
	// the count of parallel WORK OWNERS the lead should run, not a headcount of
	// agents in the team. Reviewers/watchers/pollers/the orchestrator are free infra
	// and are never part of n; see cmd/leadkit's guardFooter ACCOUNTING line.

	// The afford->target computation runs against accountN (the account-wide total),
	// not nPrev alone — so a second project ticking sees the first's already-claimed
	// workstreams and won't independently conclude there's headroom to scale into.
	// accountN==nPrev (no cross-project data) collapses this back to the original
	// per-project-only formula exactly. Computed unconditionally (even inside the
	// dead-band branch below) so accountSuffix can report it either way.
	target := clamp(int(math.Round(float64(accountN)*governing.Ratio)), MinN, MaxN)

	// accountSuffix names the cross-project split — appended only when another
	// project's live entry actually contributes (otherN>0); "" otherwise, so every
	// verdict string is byte-identical to the pre-AccountN wording in the
	// single-project case (the required backward-compat regression).
	accountSuffix := ""
	if otherN > 0 {
		accountSuffix = fmt.Sprintf(" (account %d/%d workstreams — +%d in other project(s))", accountN, target, otherN)
	}

	if governing.Ratio >= DeadBandLow && governing.Ratio <= DeadBandHigh {
		return Result{
			N: nPrev, NPrev: nPrev, Governing: governing.Name,
			Session: session, Weekly: weekly,
			Fallback: fallback, WarmingUp: warmingUp, Idle: idle,
			AccountN: accountN, AccountTarget: target,
			Verdict: fmt.Sprintf("HOLD at %d workstreams — %s%s ratio %.2f in dead-band%s", nPrev, mode, governing.Name, governing.Ratio, accountSuffix),
		}
	}

	// The DIRECTION comes from the account-level target vs the account-level total;
	// the STEP is always applied to THIS project's own nPrev (a project can only grow
	// or shed its own workstreams) and is always clamped to [MinN,MaxN] — so a project
	// already at its floor is never pushed below it just because another project is
	// heavy, and one already at the ceiling is never pushed above it.
	var next int
	switch {
	case target > accountN:
		next = clamp(nPrev+1, MinN, MaxN)
	case target < accountN:
		next = clamp(nPrev-1, MinN, MaxN)
	default:
		next = nPrev
	}

	verdict := fmt.Sprintf("HOLD at %d workstreams", next)
	switch {
	case next > nPrev:
		verdict = fmt.Sprintf("SCALE UP to %d workstreams — %s%s headroom (ratio %.2f)%s", next, mode, governing.Name, governing.Ratio, accountSuffix)
	case next < nPrev:
		hrs := in.Usage.WeekResetsAt.Sub(in.Now).Hours()
		if governing.Name == "session" {
			hrs = in.Usage.SessionResetsAt.Sub(in.Now).Hours()
		}
		verdict = fmt.Sprintf("SCALE DOWN to %d workstreams — %s%s binding, %.1fh to reset (ratio %.2f)%s", next, mode, governing.Name, hrs, governing.Ratio, accountSuffix)
	case next == MaxN && governing.Ratio > DeadBandHigh:
		// Wants to scale up further but is already at the ceiling — distinct from a
		// generic hold, so the lead doesn't read "no change" as "no headroom exists".
		verdict = fmt.Sprintf("HOLD at %d workstreams — at workstream cap (ratio %.2f)%s", next, governing.Ratio, accountSuffix)
	case next == MinN && governing.Ratio < DeadBandLow:
		verdict = fmt.Sprintf("HOLD at %d workstreams — at workstream floor (ratio %.2f)%s", next, governing.Ratio, accountSuffix)
	case otherN > 0 && governing.Ratio > DeadBandHigh:
		// This project's OWN ratio shows headroom (it would have scaled up on its own),
		// but the ACCOUNT total is already at/over the afforded max — held specifically
		// because another project already claimed the shared headroom, not because of
		// anything about this project's own state. Named distinctly so the split is
		// visible rather than reading as an ordinary, unexplained hold.
		verdict = fmt.Sprintf("HOLD at %d workstreams — headroom claimed elsewhere%s (ratio %.2f)", next, accountSuffix, governing.Ratio)
	}

	return Result{
		N: next, NPrev: nPrev, Governing: governing.Name,
		Session: session, Weekly: weekly,
		Fallback: fallback, WarmingUp: warmingUp, Idle: idle,
		AccountN: accountN, AccountTarget: target,
		Verdict: verdict,
	}
}

// affordFor computes the %/hr that can still be spent and land exactly at the cap by
// resetsAt. reserve is applied only to the effective ceiling (0 for session,
// USER_RESERVE for weekly). Floored at 0 — a budget already past its (reserved) ceiling
// can't "afford" a negative rate.
func affordFor(usedPct float64, resetsAt time.Time, now time.Time, reserve float64) float64 {
	hrsToReset := resetsAt.Sub(now).Hours()
	if hrsToReset <= 0 {
		return 0
	}
	afford := (100 - (usedPct + reserve)) / hrsToReset
	if afford < 0 {
		return 0
	}
	return afford
}

// actualFor computes measured %/hr pace since priorPct. Floored at 0 — usage never
// legitimately decreases within a window (resets are handled by the reset-time check,
// not a mid-window drop), so a negative reading is noise, not a real pace.
func actualFor(usedPct, priorPct, hoursSincePrior float64) float64 {
	if hoursSincePrior <= 0 {
		return 0
	}
	actual := (usedPct - priorPct) / hoursSincePrior
	if actual < 0 {
		return 0
	}
	return actual
}

// ratioOf is afford/pace, guarding the idle case (pace ~0) with +Inf rather than a
// divide-by-zero NaN — min() naturally excludes +Inf whenever the other side is finite.
func ratioOf(afford, pace float64) float64 {
	if pace <= idleEpsilon {
		return math.Inf(1)
	}
	return afford / pace
}

// priorInWindow finds the OLDEST sample within [now-window, now] — using the oldest
// (not the newest) gives the longest, most stable baseline for a pace calculation.
func priorInWindow(history []HistorySample, now time.Time, window time.Duration) (HistorySample, bool) {
	var best HistorySample
	found := false
	cutoff := now.Add(-window)
	for _, s := range history {
		if s.Ts.After(now) || s.Ts.Before(cutoff) {
			continue
		}
		if !found || s.Ts.Before(best.Ts) {
			best = s
			found = true
		}
	}
	return best, found
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
