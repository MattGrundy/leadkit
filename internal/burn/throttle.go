package burn

import (
	"fmt"
	"time"
)

// ThrottleDecision is what runBurn's throttle integration should do this tick, given
// the current pause-lock state and session usage — pure, so it's directly testable
// without a real lockfile.
type ThrottleDecision struct {
	Action string // "LOCK" | "RESUME" | "HELD" | "" (no lock-relevant change this tick)
	Reason string // for LOCK: the lock's meta.reason; for HELD: the full verdict line
}

// Throttle decides LOCK/RESUME/HELD/none from session usage and the current lock
// state, with pausePct/resumePct hysteresis so the lock doesn't flap on/off right at
// a single threshold:
//   - not locked, session% >= pausePct  → LOCK
//   - locked, session% < resumePct      → RESUME
//   - locked, resumePct <= session% < pausePct → HELD (stay locked)
//   - not locked, session% < pausePct   → no change
//
// Callers own the fail-safe rule this doesn't enforce itself: never call Throttle (or
// act on its result) after a usage-fetch failure — leave lock state untouched then.
func Throttle(locked bool, sessionPct, pausePct, resumePct float64, resumeAt time.Time) ThrottleDecision {
	switch {
	case !locked && sessionPct >= pausePct:
		return ThrottleDecision{
			Action: "LOCK",
			Reason: fmt.Sprintf("session %.1f%% ≥ throttle, resumes when <%.0f%%; window resets %s",
				sessionPct, resumePct, resumeAt.Format(time.RFC3339)),
		}
	case locked && sessionPct < resumePct:
		return ThrottleDecision{Action: "RESUME"}
	case locked:
		return ThrottleDecision{
			Action: "HELD",
			Reason: fmt.Sprintf("HELD (locked, %.1f%%, resumes <%.0f%%)", sessionPct, resumePct),
		}
	default:
		return ThrottleDecision{}
	}
}

// AccountThrottle is Throttle's account-scoped twin: the same LOCK/RESUME/HELD
// hysteresis, but driven by the account WEEKLY usage % (not the 5h session %) and
// carrying an account-throttle reason. It backs the token-free burn-enforce daemon
// (docs/build-tasks/burn-auto-enforce.md), which reads real account weekly usage each
// tick and writes/clears the account pause lock from this decision — so the throttle
// fires for EVERY session regardless of whether any session runs `leadkit burn`. The
// weekly budget is the account-wide one, which is why the account-level pause keys off
// it rather than the per-session 5h window.
//
// Fail-safe (the caller owns it, this pure function can't): NEVER call AccountThrottle
// after a usage-read failure — an unreadable usage signal must leave the lock state
// exactly as-is (fail toward NOT-locking), never synthesize a weekPct that could pause.
func AccountThrottle(locked bool, weekPct, pausePct, resumePct float64, resetsAt time.Time) ThrottleDecision {
	switch {
	case !locked && weekPct >= pausePct:
		return ThrottleDecision{
			Action: "LOCK",
			Reason: fmt.Sprintf("account burn throttle: weekly usage %.1f%% >= pause_pct %.0f%%, resumes when <%.0f%%; weekly window resets %s",
				weekPct, pausePct, resumePct, resetsAt.Format(time.RFC3339)),
		}
	case locked && weekPct < resumePct:
		return ThrottleDecision{Action: "RESUME"}
	case locked:
		return ThrottleDecision{
			Action: "HELD",
			Reason: fmt.Sprintf("account burn throttle HELD (locked, weekly %.1f%%, resumes <%.0f%%)", weekPct, resumePct),
		}
	default:
		return ThrottleDecision{}
	}
}
