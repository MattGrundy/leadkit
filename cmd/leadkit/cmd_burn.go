package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/lock"
	"mgpp.dev/leadkit/internal/recordlog"
	"mgpp.dev/leadkit/internal/usage"
)

// jsonOutput is the machine-readable line printed alongside the human verdict.
type jsonOutput struct {
	N             int     `json:"n"`
	Unit          string  `json:"unit"` // always "workstreams" — n is a workstream count, not an agent headcount
	NPrev         int     `json:"n_prev"`
	Governing     string  `json:"governing,omitempty"`
	Fallback      bool    `json:"fallback"`
	Idle          bool    `json:"idle"`
	WarmingUp     bool    `json:"warming_up"`
	SessionAfford float64 `json:"session_afford"`
	SessionActual float64 `json:"session_actual"`
	SessionRatio  float64 `json:"session_ratio"`
	WeeklyAfford  float64 `json:"weekly_afford"`
	WeeklyActual  float64 `json:"weekly_actual"`
	WeeklyRatio   float64 `json:"weekly_ratio"`
	Verdict       string  `json:"verdict"`
	Locked        bool    `json:"locked"`
	PausePct      float64 `json:"pause_pct"`
	ResumePct     float64 `json:"resume_pct"`
	AccountN      int     `json:"account_n"` // account-wide total governed against; == n when no cross-project data (docs/build-tasks/cross-project-burn.md)
	// Preempted/PreemptionReason (session-priority-burn.md): true when N above
	// was reduced by preemptive priority-ordered shedding rather than Advise's
	// own ±1 stepping — PreemptionReason is the same actionable YIELD text
	// printed to stdout, never silent.
	Preempted        bool   `json:"preempted"`
	PreemptionReason string `json:"preemption_reason,omitempty"`
}

func runBurn(args []string) {
	if len(args) > 0 && args[0] == "priorities" {
		runBurnPriorities(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "enforce" {
		runBurnEnforce(args[1:])
		return
	}
	prof := loadProfileOrDefaults()

	fs := flag.NewFlagSet("burn", flag.ContinueOnError)
	n := fs.Int("n", 1, "current number of parallel workstreams")
	reserve := fs.Float64("reserve", burn.DefaultReserve, "USER_RESERVE: %% weekly headroom left for the user's own sessions")
	script := fs.String("script", usage.DefaultScript, "path to usage-check.sh")
	historyPath := fs.String("history", usage.DefaultHistoryPath, "path to the rolling usage-history log")
	retain := fs.Duration("retain", 24*time.Hour, "how long history entries are kept before pruning")
	native := fs.Bool("native", true, "try the native (non-script) live-usage fetch first, falling back to -script on error; -native=false forces the script path")
	pausePct := fs.Float64("pause-pct", prof.Throttle.PausePct, "session %% at which burn locks the team (default: the profile's throttle.pause_pct)")
	resumePct := fs.Float64("resume-pct", prof.Throttle.ResumePct, "session %% below which burn clears the lock (default: the profile's throttle.resume_pct)")
	workstreamsDir := fs.String("workstreams-dir", usage.DefaultWorkstreamsDir, "account-wide dir this project's live workstream count is registered into (docs/build-tasks/cross-project-burn.md)")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "account-wide dir this project's queue/store/issues roots are registered into for the multi-project board (docs/build-tasks/multi-project-board.md)")
	priorityCeilingPath := fs.String("priority-ceiling-path", usage.DefaultPriorityCeilingPath, "account-scoped priority ceiling file (docs/build-tasks/priority-ceiling.md) — owner-only, never a project config field")
	// Advisory only: even a flag-parse error should not be treated as a hard failure
	// upstream — print to stderr and hold, exit 0 either way (spec: "Exit 0 always").
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: %v\n", err)
		os.Exit(0)
	}

	now := time.Now().UTC()

	// Cross-project burn (docs/build-tasks/cross-project-burn.md): register this
	// project's live workstream count into the account-wide registry, keyed by the
	// SAME slug the daemon namespacing uses (install.ResolveProjectSlug — reused, not
	// reimplemented, so the two keys can never drift apart), then sum every OTHER
	// live project's count into accountN. Every failure mode here fails OPEN
	// (accountN stays 0, which burn.Advise treats as "use N" — see Input.AccountN) —
	// this is a budget optimiser, never a gate, and must never block work.
	slug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	workstreamTTL := burn.DefaultWorkstreamTTL
	if prof.Burn.WorkstreamTTL != "" {
		if d, err := time.ParseDuration(prof.Burn.WorkstreamTTL); err == nil {
			workstreamTTL = d
		} else {
			fmt.Fprintf(os.Stderr, "leadkit burn: burn.workstream_ttl %q unparseable, using default %s: %v\n", prof.Burn.WorkstreamTTL, burn.DefaultWorkstreamTTL, err)
		}
	}
	// The GOVERNOR ignores an entry past the (tighter) stale window when computing
	// account_n (docs/build-tasks/burn-auto-enforce.md): a crashed/exited session must
	// stop inflating the account count promptly, and a genuinely-live session refreshes
	// well within this window (its own `leadkit burn` tick + the presence hook). The
	// looser workstream_ttl still governs registry PRUNING (doctor's stale listing) +
	// the preemption read below.
	staleWindow := prof.Burn.WorkstreamStaleWindowDur()
	accountN := 0
	otherProjectsN := 0
	if slug != "" {
		if err := usage.WriteWorkstreamEntry(*workstreamsDir, slug, *n, now); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not write workstream registry entry: %v\n", err)
		}
		// Multi-project board (multi-project-board.md): register this project's
		// roots + a display name under the same slug, so `leadkit serve` in
		// ACCOUNT mode can discover and render it. A separate, additive write
		// from the workstream one above — failure here is likewise advisory-only
		// (never blocks burn's own advice).
		projectName := prof.ProjectID
		if projectName == "" {
			projectName = filepath.Base(filepath.Dir(prof.Resolve(prof.QueueRoot)))
		}
		if err := usage.WriteProjectEntry(*projectsDir, slug, projectName, prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot), now); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not write project registry entry: %v\n", err)
		}
		if entries, ok := usage.ReadWorkstreamRegistry(*workstreamsDir, now.Add(-staleWindow)); ok {
			otherProjectsN = usage.OtherProjectsTotal(entries, slug)
			accountN = *n + otherProjectsN
		} else {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not read workstream registry %s — falling back to this project's own N\n", *workstreamsDir)
		}
	}

	// Per-session priority (session-priority-burn.md) — a SEPARATE, additive
	// registry write from the per-project one above (which stays byte-for-byte
	// unchanged): sessionID follows resolveSessionID's chain ($LEADKIT_SESSION_ID →
	// $CLAUDE_CODE_SESSION_ID → slug). With the harness UUID present (the norm) two live
	// instances of one project get DISTINCT session entries instead of clobbering a
	// single slug-keyed one; only the last-resort slug fallback reproduces the legacy
	// one-entry-per-project granularity. priority defaults to the profile's
	// burn.session_priority, then $LEADKIT_SESSION_PRIORITY (env wins, per the spec).
	sessionID, _ := resolveSessionID(slug)
	priority := prof.Burn.SessionPriority
	if envPriority := os.Getenv(sessionPriorityEnvVar); envPriority != "" {
		priority = envPriority
	}
	sessionKey := slug + "/" + sessionID
	if slug != "" && sessionID != "" {
		if err := usage.WriteSessionEntry(*workstreamsDir, slug, sessionID, *n, priority, now); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not write session registry entry: %v\n", err)
		}
	}

	var live burn.Usage
	var err error
	if *native {
		live, err = usage.FetchLiveNative()
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: native usage fetch failed, falling back to script: %v\n", err)
			live, err = usage.FetchLive(*script)
		}
	} else {
		live, err = usage.FetchLive(*script)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: could not fetch live usage: %v\n", err)
		fmt.Printf(`{"error":"fetch_failed"}`)
		fmt.Println()
		fmt.Println("HOLD — usage fetch failed, see stderr")
		os.Exit(0)
	}

	// Throttle check runs AFTER a successful fetch only — a usage-fetch failure
	// above already exits, so a fetch failure can never reach applyThrottle's
	// lock.Set/lock.Clear (fail safe: leave the lock state exactly as it was).
	lockPath := prof.Resolve(prof.Throttle.LockFile)
	nowLocked, decision, statusErr, actionErr := applyThrottle(lockPath, live.SessionPct, *pausePct, *resumePct, live.SessionResetsAt, now)
	if statusErr != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: lock status: %v\n", statusErr)
	}
	if actionErr != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: %v\n", actionErr)
	}

	history, err := usage.LoadHistory(*historyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: could not load history: %v\n", err)
	}

	result := burn.Advise(burn.Input{
		Now:      now,
		Usage:    live,
		History:  history,
		N:        *n,
		Reserve:  *reserve,
		AccountN: accountN,
	})

	// Preemptive priority-ordered shedding (session-priority-burn.md) — a
	// SEPARATE layer on top of Advise's own ±1 stepping above, using the exact
	// afforded ceiling (result.AccountTarget) Advise just computed. A no-op
	// (finalN==result.N) whenever there's nothing to preempt for — see
	// burn.ApplyPreemption's own doc for the full byte-identical/no-op list.
	ceiling := usage.ReadPriorityCeiling(*priorityCeilingPath)
	shed := runPreemptionCheck(*workstreamsDir, prof.Resolve(prof.PreemptionLog), sessionKey, slug, sessionID, priority, ceiling, result.AccountTarget, now, now.Add(-workstreamTTL))
	finalN := result.N
	if shed.Shed {
		finalN = shed.N
	}

	// Record this tick AFTER computing the recommendation — the next tick's "prior"
	// candidate, not this one's.
	if err := usage.AppendHistory(*historyPath, burn.HistorySample{
		Ts:         now,
		SessionPct: live.SessionPct,
		WeekPct:    live.WeekPct,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: could not append history: %v\n", err)
	}
	if err := usage.PruneOlderThan(*historyPath, now.Add(-*retain)); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn: could not prune history: %v\n", err)
	}

	out := jsonOutput{
		N: finalN, Unit: "workstreams", NPrev: result.NPrev, Governing: result.Governing,
		Fallback: result.Fallback, Idle: result.Idle, WarmingUp: result.WarmingUp,
		SessionAfford: result.Session.Afford, SessionActual: result.Session.Actual, SessionRatio: finiteOr(result.Session.Ratio, -1),
		WeeklyAfford: result.Weekly.Afford, WeeklyActual: result.Weekly.Actual, WeeklyRatio: finiteOr(result.Weekly.Ratio, -1),
		Verdict: result.Verdict,
		Locked:  nowLocked, PausePct: *pausePct, ResumePct: *resumePct,
		AccountN:  result.AccountN,
		Preempted: shed.Shed, PreemptionReason: shed.Reason,
	}
	line, _ := json.Marshal(out)
	fmt.Println(string(line))
	for _, l := range humanOutputLines(result.Verdict, result.AccountN, result.N, otherProjectsN) {
		fmt.Println(l)
	}
	if throttleLine := throttleVerdictLine(decision); throttleLine != "" {
		fmt.Println(throttleLine)
	}
	if shed.Shed {
		fmt.Println(shed.Reason)
	}
	os.Exit(0)
}

// sessionIDEnvVar/sessionPriorityEnvVar are session-priority-burn.md's
// per-session identity/priority overrides — env wins over the profile's
// burn.session_priority (lets a session set its priority for one run without
// editing leadkit.yaml). sessionIDEnvVar unset falls back to the project
// slug — the SAME one-entry-per-project granularity the pre-priority registry
// always had, which is what makes an un-adopted project's registry entry
// byte-identical to today.
const (
	sessionIDEnvVar       = "LEADKIT_SESSION_ID"
	sessionPriorityEnvVar = "LEADKIT_SESSION_PRIORITY"
	// harnessSessionEnvVar is Claude Code's real, unique per-session UUID (present in
	// every session's env) — the correct INSTANCE identity, used as the session-id
	// fallback below $LEADKIT_SESSION_ID so two live instances of one project no longer
	// clobber a single presence file. messagingSocketEnvVar is this session's own Claude
	// SendMessage socket — recorded into presence as the HARNESS ADDRESSING HANDLE so a
	// sender can map a delivery hint to a real SendMessage target
	// (docs/build-tasks/session-identity-cwd-honest.md).
	harnessSessionEnvVar  = "CLAUDE_CODE_SESSION_ID"
	messagingSocketEnvVar = "CLAUDE_CODE_MESSAGING_SOCKET"
)

// preemptionRecord is one JSONL line runPreemptionCheck appends to
// prof.PreemptionLog every time it actually sheds — auditable, never silent
// (session-priority-burn.md's flagged self-promotion tension: at minimum, log
// every preemptive shed).
type preemptionRecord struct {
	Ts        time.Time `json:"ts"`
	Slug      string    `json:"slug"`
	SessionID string    `json:"session_id"`
	Priority  string    `json:"priority"`
	ShedTo    int       `json:"shed_to"`
	Reason    string    `json:"reason"`
}

// runPreemptionCheck reads the account-wide SESSION registry and applies
// priority-ordered preemptive shedding (burn.ApplyPreemption) for sessionKey
// against accountTarget — the exact afforded ceiling burn.Advise's own tick
// just computed (Result.AccountTarget). Logs every actual shed to
// preemptionLogPath (best-effort — a log failure never blocks the advisory).
// Split out from runBurn so it's directly testable against a t.TempDir()
// registry without a real usage fetch. Fails OPEN (a no-op ShedResult{}) on
// any registry-read failure — this is a budget optimiser, never a gate.
func runPreemptionCheck(workstreamsDir, preemptionLogPath, sessionKey, slug, sessionID, priority string, ceiling burn.PriorityTier, accountTarget int, now, cutoff time.Time) burn.ShedResult {
	entries, ok := usage.ReadSessionRegistry(workstreamsDir, cutoff)
	if !ok {
		return burn.ShedResult{}
	}
	sessions := make([]burn.SessionWorkstream, 0, len(entries))
	for _, e := range entries {
		declared := burn.NormalizePriority(e.Priority)
		sessions = append(sessions, burn.SessionWorkstream{
			Key: e.Slug + "/" + e.SessionID, N: e.N, Priority: burn.ClampPriority(declared, ceiling),
		})
	}

	// Log THIS session's own clamp, if its declared priority exceeded the
	// ceiling — never another session's (each session's own tick is
	// responsible for reporting its own attempt; logging every OTHER
	// session's clamp too would duplicate the same event once per live
	// session, every tick).
	declaredSelf := burn.NormalizePriority(priority)
	effectiveSelf := burn.ClampPriority(declaredSelf, ceiling)
	if effectiveSelf != declaredSelf {
		if err := recordlog.Append(preemptionLogPath, priorityClampRecord{
			Ts: now, Slug: slug, SessionID: sessionID,
			Declared: string(declaredSelf), Ceiling: string(ceiling), Effective: string(effectiveSelf),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not log priority clamp: %v\n", err)
		}
	}

	shed := burn.ApplyPreemption(sessions, sessionKey, accountTarget)
	if shed.Shed {
		if err := recordlog.Append(preemptionLogPath, preemptionRecord{
			Ts: now, Slug: slug, SessionID: sessionID, Priority: string(effectiveSelf),
			ShedTo: shed.N, Reason: shed.Reason,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn: could not log preemptive shed: %v\n", err)
		}
	}
	return shed
}

// priorityClampRecord is one JSONL line runPreemptionCheck appends (to the
// SAME preemption log sheds use) every time a session's own declared
// priority exceeds the account-scoped ceiling and gets clamped down —
// auditable, never silent (priority-ceiling.md).
type priorityClampRecord struct {
	Ts        time.Time `json:"ts"`
	Slug      string    `json:"slug"`
	SessionID string    `json:"session_id"`
	Declared  string    `json:"declared"`
	Ceiling   string    `json:"ceiling"`
	Effective string    `json:"effective"`
}

// priorityRow is one line of `leadkit burn priorities`' listing.
type priorityRow struct {
	Slug      string `json:"slug"`
	SessionID string `json:"session_id"`
	// DeclaredPriority is what the session itself wrote to the registry;
	// Priority is the EFFECTIVE (post-ceiling) tier preemption actually uses
	// — what ShedOrderRank is computed from. Clamped is true iff the two
	// differ (priority-ceiling.md: the account owner's ceiling capped a
	// self-declared attempt above it).
	DeclaredPriority string  `json:"declared_priority"`
	Priority         string  `json:"priority"`
	Clamped          bool    `json:"clamped"`
	N                int     `json:"n"`
	AgeSeconds       float64 `json:"age_seconds"`
	ShedOrderRank    int     `json:"shed_order_rank"` // 0 sheds first — from the EFFECTIVE priority
}

// priorityListing is `leadkit burn priorities --json`'s stable shape.
type priorityListing struct {
	Rows     []priorityRow `json:"rows"`
	AccountN int           `json:"account_n"`
	// Ceiling is the account-scoped ceiling in effect (priority-ceiling.md) —
	// "critical" means no clamp is active (unset/absent/unreadable).
	Ceiling string `json:"ceiling"`
	// MultipleCritical flags the self-promotion smell (session-priority-
	// burn.md's flagged tension): more than one live session DECLARES
	// priority=critical (checked on the declared value — a claim worth
	// seeing even if the ceiling neutralizes it).
	MultipleCritical bool `json:"multiple_critical"`
	// AnyClamped is true iff at least one live session's declared priority
	// exceeded the ceiling — the "someone tried to self-promote past the
	// ceiling" signal.
	AnyClamped bool `json:"any_clamped"`
}

// buildPriorityListing sorts entries by shed order (by EFFECTIVE, post-
// ceiling priority — lowest first, then slug, then session id —
// deterministic) and computes the account total + the multiple-critical/
// any-clamped smell flags. Pure — no I/O.
func buildPriorityListing(entries []usage.SessionEntry, ceiling burn.PriorityTier, now time.Time) priorityListing {
	declaredSessions := make([]burn.SessionWorkstream, 0, len(entries)) // for the DECLARED-priority critical-collision check
	accountN := 0
	rows := make([]priorityRow, 0, len(entries))
	anyClamped := false
	for _, e := range entries {
		accountN += e.N
		declared := burn.NormalizePriority(e.Priority)
		effective := burn.ClampPriority(declared, ceiling)
		clamped := effective != declared
		anyClamped = anyClamped || clamped
		declaredSessions = append(declaredSessions, burn.SessionWorkstream{Key: e.Slug + "/" + e.SessionID, N: e.N, Priority: declared})
		rows = append(rows, priorityRow{
			Slug: e.Slug, SessionID: e.SessionID, DeclaredPriority: string(declared), Priority: string(effective),
			Clamped: clamped, N: e.N, AgeSeconds: now.Sub(e.UpdatedAt).Seconds(), ShedOrderRank: burn.ShedOrderRank(string(effective)),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ShedOrderRank != rows[j].ShedOrderRank {
			return rows[i].ShedOrderRank < rows[j].ShedOrderRank
		}
		if rows[i].Slug != rows[j].Slug {
			return rows[i].Slug < rows[j].Slug
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	return priorityListing{
		Rows: rows, AccountN: accountN, Ceiling: string(ceiling),
		MultipleCritical: burn.AnyCriticalCollision(declaredSessions), AnyClamped: anyClamped,
	}
}

// formatPriorityListing renders l as `leadkit burn priorities`' human table.
func formatPriorityListing(l priorityListing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "account total: %d workstream(s) across %d session(s) — ceiling: %s\n", l.AccountN, len(l.Rows), l.Ceiling)
	if l.MultipleCritical {
		b.WriteString("WARNING: more than one session declares priority=critical — the self-promotion smell (neither protects against the other)\n")
	}
	if l.AnyClamped {
		b.WriteString("NOTE: at least one session declared above the ceiling and was clamped — see the \"clamped\" column\n")
	}
	if len(l.Rows) == 0 {
		b.WriteString("(no live sessions registered)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-4s  %-24s  %-20s  %-8s  %-8s  %-7s  %-3s  %s\n", "rank", "slug", "session", "declared", "effective", "clamped", "n", "age")
	for _, r := range l.Rows {
		age := time.Duration(r.AgeSeconds * float64(time.Second)).Round(time.Second)
		fmt.Fprintf(&b, "%-4d  %-24s  %-20s  %-8s  %-8s  %-7t  %-3d  %s\n", r.ShedOrderRank, r.Slug, r.SessionID, r.DeclaredPriority, r.Priority, r.Clamped, r.N, age)
	}
	return b.String()
}

// runBurnPriorities is `leadkit burn priorities [--json]` (session-priority-
// burn.md + priority-ceiling.md) — a read-only, fast listing of every live
// session on the account, sorted by EFFECTIVE (post-ceiling) shed order —
// lowest first, who yields first under pressure. Shows declared vs
// effective priority and flags clamped rows. Never writes; a registry read
// failure exits 0 (advisory, same posture as doctor) with a stderr note,
// never a hard failure.
func runBurnPriorities(args []string) {
	fs := flag.NewFlagSet("burn priorities", flag.ContinueOnError)
	workstreamsDir := fs.String("workstreams-dir", usage.DefaultWorkstreamsDir, "account-wide workstream registry dir")
	priorityCeilingPath := fs.String("priority-ceiling-path", usage.DefaultPriorityCeilingPath, "account-scoped priority ceiling file")
	jsonOut := fs.Bool("json", false, "emit the listing as JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn priorities: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	workstreamTTL := burn.DefaultWorkstreamTTL
	if prof.Burn.WorkstreamTTL != "" {
		if d, err := time.ParseDuration(prof.Burn.WorkstreamTTL); err == nil {
			workstreamTTL = d
		}
	}
	now := time.Now().UTC()
	entries, ok := usage.ReadSessionRegistry(*workstreamsDir, now.Add(-workstreamTTL))
	if !ok {
		fmt.Fprintf(os.Stderr, "leadkit burn priorities: could not read %s\n", *workstreamsDir)
		os.Exit(0)
	}

	ceiling := usage.ReadPriorityCeiling(*priorityCeilingPath)
	listing := buildPriorityListing(entries, ceiling, now)
	if *jsonOut {
		data, err := json.Marshal(listing)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn priorities: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(formatPriorityListing(listing))
	os.Exit(0)
}

// applyThrottle runs the lock.Status → burn.Throttle → lock.Set/lock.Clear sequence
// for one burn tick, returning the resulting lock state and the decision that
// produced it. Split out from runBurn so it's directly testable against a
// t.TempDir() lock path and a stubbed session%/resets-at — no real usage fetch or
// live lockfile needed. Callers own the fail-safe rule this doesn't enforce itself:
// only call this after a successful usage fetch (runBurn's fetch-failure path exits
// before ever reaching here, so a fetch failure can't reach lock.Set/lock.Clear).
func applyThrottle(lockPath string, sessionPct, pausePct, resumePct float64, sessionResetsAt, now time.Time) (nowLocked bool, decision burn.ThrottleDecision, statusErr, actionErr error) {
	wasLocked, _, statusErr := lock.Status(lockPath)
	if statusErr != nil {
		return false, burn.ThrottleDecision{}, statusErr, nil
	}
	decision = burn.Throttle(wasLocked, sessionPct, pausePct, resumePct, sessionResetsAt)
	nowLocked = wasLocked
	switch decision.Action {
	case "LOCK":
		if err := lock.Set(lockPath, lock.Meta{
			Reason: decision.Reason, SetBy: "burn", PausedAt: now,
			PausePct: pausePct, ResumePct: resumePct, ResumeAt: sessionResetsAt,
		}); err != nil {
			actionErr = fmt.Errorf("lock set: %w", err)
		} else {
			nowLocked = true
		}
	case "RESUME":
		if err := lock.Clear(lockPath); err != nil {
			actionErr = fmt.Errorf("lock clear: %w", err)
		} else {
			nowLocked = false
		}
	}
	return nowLocked, decision, nil, actionErr
}

// applyAccountThrottle is applyThrottle's account-scoped twin: it runs lock.Status →
// burn.AccountThrottle → lock.Set/lock.Clear against the ACCOUNT pause lock, keyed off
// the account WEEKLY usage %. Split out from runBurnEnforce so it's directly testable
// against a t.TempDir() lock path + a stubbed weekPct. Same fail-safe contract as
// applyThrottle: only call this after a SUCCESSFUL usage fetch — enforceOnce's
// fetch-failure path returns before ever reaching here, so an unreadable usage signal
// can never reach lock.Set/lock.Clear (fail toward not-locking).
func applyAccountThrottle(lockPath string, weekPct, pausePct, resumePct float64, weekResetsAt, now time.Time) (nowLocked bool, decision burn.ThrottleDecision, statusErr, actionErr error) {
	wasLocked, _, statusErr := lock.Status(lockPath)
	if statusErr != nil {
		// A lock-read error is NOT a usage-read error — but we still must not guess the
		// lock state. Leave it untouched (report the error); the next tick retries.
		return false, burn.ThrottleDecision{}, statusErr, nil
	}
	decision = burn.AccountThrottle(wasLocked, weekPct, pausePct, resumePct, weekResetsAt)
	nowLocked = wasLocked
	switch decision.Action {
	case "LOCK":
		if err := lock.Set(lockPath, lock.Meta{
			Reason: decision.Reason, SetBy: "burn-enforce", PausedAt: now,
			PausePct: pausePct, ResumePct: resumePct, ResumeAt: weekResetsAt,
		}); err != nil {
			actionErr = fmt.Errorf("account lock set: %w", err)
		} else {
			nowLocked = true
		}
	case "RESUME":
		if err := lock.Clear(lockPath); err != nil {
			actionErr = fmt.Errorf("account lock clear: %w", err)
		} else {
			nowLocked = false
		}
	}
	return nowLocked, decision, nil, actionErr
}

// enforceResult is one burn-enforce tick's outcome.
type enforceResult struct {
	Fetched   bool // false when the usage read failed (lock left untouched)
	Locked    bool // resulting account-lock state (meaningful only when Fetched)
	Decision  burn.ThrottleDecision
	FetchErr  error
	StatusErr error
	ActionErr error
}

// enforceOnce is one account-level burn-enforce tick, factored out of runBurnEnforce so
// it's testable with an injectable usage source + clock and a t.TempDir() lock — no real
// usage fetch or live lockfile needed.
//
// CRITICAL FAIL-SAFE (account-wide pause): when fetch returns an error the account usage
// signal is UNREADABLE, so the lock is left EXACTLY as-is — never written, never cleared
// (a read failure must fail toward NOT-locking, and equally must not spuriously RESUME a
// legitimate pause). It is logged (by the caller) and retried next tick. Only a
// successful read ever reaches applyAccountThrottle.
func enforceOnce(lockPath string, fetch func() (burn.Usage, error), pausePct, resumePct float64, now time.Time) enforceResult {
	live, err := fetch()
	if err != nil {
		return enforceResult{FetchErr: err}
	}
	locked, decision, statusErr, actionErr := applyAccountThrottle(lockPath, live.WeekPct, pausePct, resumePct, live.WeekResetsAt, now)
	return enforceResult{Fetched: true, Locked: locked, Decision: decision, StatusErr: statusErr, ActionErr: actionErr}
}

// runBurnEnforce is `leadkit burn enforce`: the ACCOUNT-LEVEL, token-free burn-enforce
// tick the dev.leadkit.burn daemon fires (docs/build-tasks/burn-auto-enforce.md). It
// reads real account WEEKLY usage and writes/clears the account pause lock from the
// pause_pct/resume_pct hysteresis — so the throttle fires WITHOUT any session running
// `leadkit burn`. Idempotent, best-effort, and ALWAYS exits 0 (a daemon tick is
// advisory infrastructure, never a gate). Fail-safe: a usage-read failure writes NO
// pause (it is logged and retried next tick).
func runBurnEnforce(args []string) {
	prof := loadProfileOrDefaults()

	fs := flag.NewFlagSet("burn enforce", flag.ContinueOnError)
	script := fs.String("script", usage.DefaultScript, "path to usage-check.sh (used only if the native fetch fails)")
	native := fs.Bool("native", true, "try the native (non-script) live-usage fetch first, falling back to -script on error")
	pausePct := fs.Float64("pause-pct", prof.Throttle.PausePct, "weekly %% at which the account throttle pauses the account (default: the profile's throttle.pause_pct)")
	resumePct := fs.Float64("resume-pct", prof.Throttle.ResumePct, "weekly %% below which the account throttle clears the pause (default: the profile's throttle.resume_pct)")
	accountLock := fs.String("account-lock", lock.DefaultAccountLockPath, "path to the account-level pause lock this tick writes/clears")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn enforce: %v\n", err)
		os.Exit(0)
	}

	now := time.Now().UTC()
	fetch := func() (burn.Usage, error) {
		if *native {
			if live, err := usage.FetchLiveNative(); err == nil {
				return live, nil
			} else {
				fmt.Fprintf(os.Stderr, "leadkit burn enforce: native usage fetch failed, falling back to script: %v\n", err)
			}
		}
		return usage.FetchLive(*script)
	}

	res := enforceOnce(*accountLock, fetch, *pausePct, *resumePct, now)
	if res.FetchErr != nil {
		// FAIL SAFE: usage unreadable — account lock left exactly as-is, NOT written.
		fmt.Fprintf(os.Stderr, "leadkit burn enforce: could not read account usage — leaving the pause lock untouched (fail-safe): %v\n", res.FetchErr)
		fmt.Println("HOLD — account usage unreadable, lock untouched (fail-safe), see stderr")
		os.Exit(0)
	}
	if res.StatusErr != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn enforce: account lock status: %v\n", res.StatusErr)
	}
	if res.ActionErr != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn enforce: %v\n", res.ActionErr)
	}
	if line := throttleVerdictLine(res.Decision); line != "" {
		fmt.Println(line)
	} else {
		fmt.Println("no change (account weekly usage below pause_pct, not locked)")
	}
	os.Exit(0)
}

// throttleVerdictLine renders d for the human-readable output, alongside (not
// instead of) burn's usual N-recommendation verdict — the pause lock and the
// workstream count are two independent signals.
func throttleVerdictLine(d burn.ThrottleDecision) string {
	switch d.Action {
	case "LOCK":
		return "LOCK — " + d.Reason
	case "RESUME":
		return "RESUME — lock cleared"
	case "HELD":
		return d.Reason
	default:
		return ""
	}
}

// guardFooter re-surfaces the standing token-burn guards on every human-readable run —
// over a long session these decay from active context even when memory-encoded, so the
// tool the lead reads each heartbeat re-injects them at decision time. JSON output is
// unaffected (machine consumers don't need prose reminders).
const guardFooter = `GUARDS: reuse standing team — idle, don't kill/respawn (context reload = $$) · work content in TASK FILES, not SendMessage briefs · queue-move + minimal wake triggers agent work · file tasks not session tasks.`

// accountingLine disambiguates burn's n from a headcount — the recurring conflation
// where the lead reads n WORKSTREAMS as n AGENTS and kills or idles the reviewer pool
// (or refuses to fan out) to "stay under n". Kept as its own re-injected line, same
// reasoning as guardFooter: the disambiguation has to live in the output the lead
// reads every heartbeat, not only in a doc that decays from context.
const accountingLine = `ACCOUNTING: n = WORKSTREAMS, not agents. 1 workstream = 1 owner actively working a task. Reviewers, watchers, pollers, the orchestrator = free infra, NOT counted — never kill or idle them to hit n. n workstreams ≠ n agents.`

// humanOutputLines is the verdict + guard footer + accounting line (+ an optional
// cross-project note) printed after the JSON line. Pulled out as a pure function so
// their presence is unit-testable without exec'ing the built binary. accountN/n/
// otherProjectsN default to their zero values for every existing caller — a
// single-project run (otherProjectsN==0) prints exactly the original three lines,
// unchanged (docs/build-tasks/cross-project-burn.md's backward-compat requirement
// extends to this footer, not just Advise's own Verdict text).
func humanOutputLines(verdict string, accountN, n, otherProjectsN int) []string {
	lines := []string{verdict, guardFooter, accountingLine}
	if otherProjectsN > 0 {
		lines = append(lines, fmt.Sprintf("ACCOUNTING: +%d workstream(s) claimed by other project(s) — account total %d (this project's own N: %d).", otherProjectsN, accountN, n))
	}
	return lines
}

// finiteOr replaces +Inf (idle-side ratio) with sentinel so it survives JSON encoding
// (encoding/json rejects Inf/NaN outright).
func finiteOr(v float64, sentinel float64) float64 {
	if math.IsInf(v, 0) {
		return sentinel
	}
	return v
}
