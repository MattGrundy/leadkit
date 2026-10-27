// Package doctor is leadkit's preflight self-check — "run this first to confirm
// wiring" before trusting leadkit against a real project. Every check here is
// READ-ONLY: a stat, a parse, opening+closing an fsnotify watcher, a keychain read
// whose result is discarded. Nothing here mutates the profile, the queue, the store,
// or the keychain.
//
// Unlike most other internal packages (which take primitive path params so callers
// can wire in whatever profile fields they need), doctor takes the whole
// *config.Profile directly — its entire job is validating the profile itself, and it
// touches nearly every field, so a primitive-per-field signature would just be the
// same information restated as ~10 parameters.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"mgpp.dev/leadkit/internal/audit"
	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/burnreport"
	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/hookwire"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/linearapi"
	"mgpp.dev/leadkit/internal/milestone"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/redact"
	"mgpp.dev/leadkit/internal/registry"
	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// Severity classifies a Check: Critical checks gate doctor's exit code, Warn/Info
// checks are surfaced but never fail the run.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarn     Severity = "warn"
	SeverityInfo     Severity = "info"
)

// Check is one preflight observation.
type Check struct {
	Name     string   `json:"name"`
	OK       bool     `json:"ok"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail"`
}

// Report is doctor's full run.
type Report struct {
	Checks []Check `json:"checks"`
}

// AllCriticalPass reports whether every Severity-critical check passed — doctor's
// exit-code gate (exit 0 iff true).
func (r Report) AllCriticalPass() bool {
	for _, c := range r.Checks {
		if !c.OK && c.Severity == SeverityCritical {
			return false
		}
	}
	return true
}

// Run executes every preflight check against prof (discovered at profilePath, or ""
// for a Defaults()-only profile) and returns the full report. settingsPaths are the
// settings.json locations the hook-wiring check reads (see hookWiringChecks) — pass
// nil to skip it entirely (every hook then reports MISSING, which is the correct,
// honest answer when no settings.json was even looked at... though callers that care
// about the distinction should prefer passing the real standard paths). launchAgentsDir
// is the daemon-agents check's target (see daemonAgentsCheck) — pass "" to skip it
// (e.g. non-macOS callers, or a caller that doesn't want a real directory read).
// workstreamsDir is the cross-project burn registry's target (see
// workstreamRegistryCheck) — pass "" to skip it the same way. git is the seam the
// branch-whitelist check uses to read the current branch/origin remote (see
// branchWhitelistCheck) — pass nil to skip it (e.g. a caller with no real git
// binary available); the real CLI passes RealGit(). priorityCeilingPath is the
// account-scoped priority ceiling file (see sessionPriorityCheck) — pass "" to
// use the "" resolution (usage.ReadPriorityCeiling("") = no clamp, same as
// unset/unreadable); the real CLI passes usage.DefaultPriorityCeilingPath.
// projectsDir is the multi-project board's project registry (see
// projectRegistryCheck) — pass "" to skip it the same way as workstreamsDir.
// presenceDir is the account-wide active-session presence registry
// (docs/build-tasks/burn-auto-enforce.md) — pass "" to skip the presence check the same
// way as workstreamsDir; the real CLI passes usage.DefaultPresenceDir.
// routingDir is the account-wide routing store (docs/build-tasks/routing-substrate.md) —
// pass "" to skip the routing-inbox check the same way; the real CLI passes
// usage.DefaultRoutingDir.
// registryRepo is the git-backed lead & session identity registry
// (docs/build-tasks/lead-identity-registry.md) — pass "" to skip the lead-registry check;
// registryHost/registrySessionID are THIS session's own identity, used to detect the
// this-session-unregistered warn (both "" skips only that sub-signal).
// transcriptsDir is the Claude Code transcripts root (default ~/.claude/projects) the
// subagent-model check samples for its recent-spend line — "" skips the line (fail-soft).
func Run(prof *config.Profile, profilePath, version string, settingsPaths []string, launchAgentsDir, workstreamsDir string, git Git, priorityCeilingPath, projectsDir, presenceDir, routingDir, registryRepo, registryHost, registrySessionID, transcriptsDir string) Report {
	var r Report
	// FIRST assertion, deliberately: the whole operating model (reviewer pool,
	// orchestrator, SendMessage) silently fails without Agent Teams enabled — a
	// broken profile downstream is a distraction if THIS is why nothing's working.
	r.Checks = append(r.Checks, agentTeamsCheck(settingsPaths))
	r.Checks = append(r.Checks, subagentModelCheck(settingsPaths, transcriptsDir, time.Now()))
	r.Checks = append(r.Checks, profileCheck(prof, profilePath))
	r.Checks = append(r.Checks, externalConfigIntegrityCheck(prof, profilePath, time.Now()))
	r.Checks = append(r.Checks, pathChecks(prof)...)
	r.Checks = append(r.Checks, rolesCheck(prof))
	r.Checks = append(r.Checks, queueLanesCheck(prof))
	r.Checks = append(r.Checks, laneAliasesCheck(prof))
	r.Checks = append(r.Checks, fsnotifyCheck())
	r.Checks = append(r.Checks, keychainCheck(prof))
	r.Checks = append(r.Checks, wakeCommandCheck(prof))
	r.Checks = append(r.Checks, linearOutputCheck(prof))
	r.Checks = append(r.Checks, prGuardRawVerbsCheck(prof))
	r.Checks = append(r.Checks, daemonAgentsCheck(prof, launchAgentsDir))
	r.Checks = append(r.Checks, wakerInstalledCheck(prof, launchAgentsDir))
	r.Checks = append(r.Checks, workstreamRegistryCheck(prof, workstreamsDir))
	r.Checks = append(r.Checks, sessionPriorityCheck(prof, workstreamsDir, usage.ReadPriorityCeiling(priorityCeilingPath)))
	r.Checks = append(r.Checks, projectRegistryCheck(prof, projectsDir))
	r.Checks = append(r.Checks, queueBypassCheck(prof))
	r.Checks = append(r.Checks, ownerLivenessCheck(prof))
	r.Checks = append(r.Checks, productivityCheck(time.Now(), prof.Burn.ProductivityStallWindowDur(), RealProductivityProbe(prof)))
	r.Checks = append(r.Checks, frozenClaimsCheck(time.Now(), prof.Burn.InprogressStallWindowDur(), RealFrozenClaimProbe(prof)))
	r.Checks = append(r.Checks, daemonDiagnosticsCheck(prof, time.Now()))
	r.Checks = append(r.Checks, milestoneAnchorCheck(prof))
	r.Checks = append(r.Checks, scopeOriginCheck(prof, profilePath, git))
	r.Checks = append(r.Checks, branchWhitelistCheck(prof, git))
	r.Checks = append(r.Checks, authorityKeyCollisionCheck(prof, git))
	r.Checks = append(r.Checks, forgeBranchProtectionCheck(prof, git))
	r.Checks = append(r.Checks, forgeTokenBreadthCheck(prof, git))
	r.Checks = append(r.Checks, guardEnforcementCheck(prof))
	r.Checks = append(r.Checks, hookWiringChecks(settingsPaths)...)
	r.Checks = append(r.Checks, presenceCheck(prof, presenceDir, time.Now()))
	r.Checks = append(r.Checks, routingInboxCheck(routingDir, routingInboxStaleWindow, time.Now()))
	r.Checks = append(r.Checks, leadRegistryCheck(prof, registryRepo, registryHost, registrySessionID, time.Now()))
	r.Checks = append(r.Checks, binarySkewCheck(RealBinarySkewInput()))
	r.Checks = append(r.Checks, Check{Name: "binary", OK: true, Severity: SeverityInfo, Detail: version})
	return r
}

// milestoneAnchorCheck (docs/build-tasks/milestone-anchor-idle-gate.md) extends the
// goal/anchor family: it WARNs when no milestone DoD artifact is declared (the
// root-cause "no anchor" state that let a lead lose the milestone goal), and when a DoD
// condition is manual/unverified (surfacing what still can't be auto-verified). NEVER
// errors — a project without milestones is valid, so absence and unverifiable signals
// are advisory, never a critical failure. Read-only: it parses the artifact and inspects
// condition TYPES only; it never evaluates external signals (no gh/CI/git), so doctor
// stays hermetic.
func milestoneAnchorCheck(prof *config.Profile) Check {
	path := prof.Resolve(prof.MilestonesFile)
	art, present, err := milestone.Parse(path)
	if err != nil {
		return Check{Name: "milestone-anchor", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("milestones_file %s is present but unparseable (%v) — fix or remove it; `milestone status`/`check` will read it as absent until then", prof.MilestonesFile, err)}
	}
	if !present {
		return Check{Name: "milestone-anchor", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("no milestone DoD artifact declared (%s absent) — the loop has no machine-readable goal to re-anchor or gate against; run `leadkit milestone init`", prof.MilestonesFile)}
	}
	var manual []string
	for _, c := range art.DoD {
		if c.Type == milestone.TypeManual {
			manual = append(manual, c.Name)
		}
	}
	if len(manual) > 0 {
		return Check{Name: "milestone-anchor", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("milestone %s declared, but %d DoD condition(s) are manual/unverified (can't be auto-verified yet): %s — build a check to graduate them, or a human must mark them", art.CurrentMilestone, len(manual), strings.Join(manual, ", "))}
	}
	return Check{Name: "milestone-anchor", OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("milestone %s declared with %d DoD condition(s), none manual/unverified", art.CurrentMilestone, len(art.DoD))}
}

// daemonDiagnosticsStaleWindow is how old the sweep findings artifact may be before
// daemonDiagnosticsCheck treats it as stale (the sweep daemon may be stopped). Coarse
// and generous — this is an advisory preflight signal, not a precise liveness probe.
const daemonDiagnosticsStaleWindow = time.Hour

// daemonDiagnosticsCheck (docs/build-tasks/self-surfacing-diagnostics.md) reads the
// findings artifact the sweep writes and surfaces its stalls/broken-links WITH the
// remediation — so a lead-less adopter sees the problem AND the fix from `leadkit
// doctor`, no meta-orchestrator required. Read-only (a plain file read via
// queue.ReadSweepStatus). It WARNs only on real, fresh findings; an absent or STALE
// artifact is Info ("the daemon may not be running", with how to check) — never a
// false Warn. The surface_doctor toggle gates it: off → a single Info noting surfacing
// is disabled, nothing read.
func daemonDiagnosticsCheck(prof *config.Profile, now time.Time) Check {
	if !prof.Diagnostics.SurfaceDoctor {
		return Check{Name: "daemon-diagnostics", OK: true, Severity: SeverityInfo,
			Detail: "surfacing disabled (diagnostics.surface_doctor=false) — the sweep still writes its findings artifact; `leadkit queue sweep`/`serve` /health can still surface them"}
	}
	path := prof.Resolve(prof.SweepStatusFile)
	status, present, err := queue.ReadSweepStatus(path)
	if err != nil {
		return Check{Name: "daemon-diagnostics", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("sweep findings artifact %s is present but unreadable (%v) — treating as no findings; re-run `leadkit queue sweep` to rewrite it", prof.SweepStatusFile, err)}
	}
	if !present {
		return Check{Name: "daemon-diagnostics", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("no sweep findings artifact yet (%s absent) — the sweep daemon may not have run; check with `leadkit queue sweep`", prof.SweepStatusFile)}
	}
	if age, ok := status.Age(now); ok && age > daemonDiagnosticsStaleWindow {
		return Check{Name: "daemon-diagnostics", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("sweep findings are stale (last run %s ago, past %s) — the sweep daemon may be stopped; check with `leadkit queue sweep`", age.Round(time.Second), daemonDiagnosticsStaleWindow)}
	}
	if status.Clean() {
		return Check{Name: "daemon-diagnostics", OK: true, Severity: SeverityInfo,
			Detail: "last sweep found no stalls, no broken links and no stranded tickets — queue healthy"}
	}
	return Check{Name: "daemon-diagnostics", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("last sweep found %d stalled lane(s), %d broken link(s), %d stranded ticket(s) — %s",
			status.Stalled, status.Broken, status.Stray, status.Remediation(prof.Diagnostics.SelfRepair))}
}

// queueBypassRecentRecordWindow is how far back "gate records touched recently"
// looks for queueBypassCheck — a coarse, generous window (this is an advisory
// preflight signal, not a precise audit).
const queueBypassRecentRecordWindow = 24 * time.Hour

// queueBypassCheck (GAP-6/queue-mode-enforcement item 3) fires when a project is
// configured for a team (roster declared, queue reachable) but active work looks
// like it's running OFF the queue: the queue itself holds no tickets while session
// tasks show real (non-gate) work-in-progress. doctor is read-only and can't see
// live agents, so this is the detectable DISK signal — the same "wrong mechanism"
// tell internal/audit's session lens already computes via IsWorkish, reused here
// rather than reinvented.
//
// Deliberately conservative about false positives: a queue holding ANY ticket
// (pending/in-progress/done, in any lane) is treated as "in use" and never flags,
// even if gate records are stale — a healthy queue with tickets not yet gated must
// never read as a bypass. The spec's second signal (recent gate records) is
// surfaced in the Warn detail for context when it also fires, not as an
// independent trigger — see docs/build-tasks/queue-mode-enforcement.md item 3.
func queueBypassCheck(prof *config.Profile) Check {
	if len(prof.Roster) == 0 {
		return Check{Name: "queue-bypass", OK: true, Severity: SeverityInfo, Detail: "no roster declared — nothing to check"}
	}
	queueRoot := prof.Resolve(prof.QueueRoot)
	if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
		return Check{Name: "queue-bypass", OK: true, Severity: SeverityInfo, Detail: "queue_root not reachable — nothing to check (see the queue_root check above)"}
	}

	workish, findingCount := sessionTasksWorkish(prof)
	if !workish {
		return Check{Name: "queue-bypass", OK: true, Severity: SeverityInfo, Detail: "no work-ish session-task activity — nothing to check"}
	}

	tickets := countQueueTickets(queueRoot)
	recentRecords := recentGateRecordCount(prof.Resolve(prof.RecordsDir), queueBypassRecentRecordWindow)
	if tickets == 0 {
		return Check{
			Name: "queue-bypass", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"roster declared (%d role(s)) and the queue at %s is reachable but holds ZERO tickets, while %d session task(s) look work-ish (%d recent gate record(s) in the last %s) — active work appears to be running OFF the queue — the waker daemon, PM-board sync, audit, and merge-guard will not see it; enqueue work via the queue (see `leadkit team scaffold`).",
				len(prof.Roster), queueRoot, findingCount, recentRecords, queueBypassRecentRecordWindow,
			),
		}
	}
	return Check{
		Name: "queue-bypass", OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("queue holds %d ticket(s) across all lanes, %d recent gate record(s) — no bypass signal", tickets, recentRecords),
	}
}

// ownerLivenessCheck (docs/build-tasks/team-standup-teach-check-scaffold.md) is the
// runtime nudge for the classic failure: the lead had a roster and a full queue but no
// persistent owners consuming it — work sat in the queue unconsumed. It fires when
// the roster's work-role lanes hold PENDING tickets while ZERO are in-progress across
// them — nobody appears to be pulling — and points at the scaffold (`leadkit team
// standup`). It reuses queue.Sweep's per-lane census (LaneCount) — never re-counts.
//
// DISTINCT from queueBypassCheck: that catches work running OFF the queue (roster
// declared, queue reachable, but the queue holds ZERO tickets while session tasks
// look work-ish). THIS catches work SITTING IN the queue with no consumer (tickets
// pending, nothing in-progress). Both can point at the operating model; neither
// subsumes the other.
//
// Conservative to the point of never false-WARNing: any in-progress ticket across
// the roster lanes means owners appear to BE consuming → OK (work legitimately in
// flight). No roster, an unreachable queue, or no pending work at all → Info. Only
// the exact pending>0 / in-progress==0 shape WARNs.
func ownerLivenessCheck(prof *config.Profile) Check {
	if len(prof.Roster) == 0 {
		return Check{Name: "owner-liveness", OK: true, Severity: SeverityInfo, Detail: "no roster declared — no work-roles that need persistent owners; nothing to check"}
	}
	queueRoot := prof.Resolve(prof.QueueRoot)
	if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
		return Check{Name: "owner-liveness", OK: true, Severity: SeverityInfo, Detail: "queue_root not reachable — nothing to check (see the queue_root check above)"}
	}
	res, err := queue.Sweep(queueRoot, false)
	if err != nil {
		return Check{Name: "owner-liveness", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("queue at %s could not be swept (%v) — nothing to check", queueRoot, err)}
	}

	census := map[string]queue.LaneCount{}
	for _, lc := range res.Lanes {
		census[lc.Lane] = lc
	}
	rolesPath := prof.Resolve(prof.RolesPath)
	laneOverride := map[string]string{}
	for _, r := range prof.Roster {
		if r.Type != "" && r.Lane != "" {
			laneOverride[r.Type] = r.Lane
		}
	}
	resolveLane := func(role string) string {
		if live, ok := roles.Resolve(rolesPath, role); ok {
			return live
		}
		if l, ok := laneOverride[role]; ok {
			return l
		}
		return role
	}

	totalPending, totalInProgress, roleCount := 0, 0, 0
	var starved []string
	seen := map[string]bool{}
	for _, r := range prof.Roster {
		if r.Type == "" || seen[r.Type] {
			continue
		}
		seen[r.Type] = true
		roleCount++
		lc := census[resolveLane(r.Type)]
		totalPending += lc.Pending
		totalInProgress += lc.InProgress
		if lc.Pending > 0 && lc.InProgress == 0 {
			starved = append(starved, r.Type)
		}
	}

	if totalPending == 0 {
		return Check{Name: "owner-liveness", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("roster declares %d work-role(s); their lanes hold no pending work — nothing waiting for an owner", roleCount)}
	}
	// Per-role, NOT gated on GLOBAL in-progress: WARN whenever ANY role is starved
	// (pending>0 AND in-progress==0), even while OTHER lanes have in-progress work — a
	// single dormant owner amid busy peers is the exact ci-infra wedge. Only when NO
	// role is starved does every pending lane have a consumer → OK.
	if len(starved) == 0 {
		return Check{Name: "owner-liveness", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("roster declares %d work-role(s); %d pending, %d in-progress across their lanes — every lane with pending work has an in-progress consumer", roleCount, totalPending, totalInProgress)}
	}
	named := starved
	if len(named) > 2 {
		named = named[:2]
	}
	return Check{Name: "owner-liveness", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("roster declares %d role(s); %d role lane(s) hold pending ticket(s) with 0 in-progress — no persistent owners appear to be consuming them (even if other lanes are busy). Stand them up: `leadkit team standup` (and `leadkit team standup --scaffold --confirm` to create any missing role lane dirs first) (starved: %s).",
			roleCount, len(starved), strings.Join(named, ", "))}
}

// sessionTasksWorkish reports whether prof.SessionTasksRoot has any non-gate session
// task that reads as real work — via internal/audit.SessionLens/IsWorkish, the SAME
// heuristic internal/hook's task-mechanism guard and `leadkit audit`'s session lens
// use, so this check can never disagree with them about what counts as workish.
func sessionTasksWorkish(prof *config.Profile) (bool, int) {
	tasksRoot := prof.Resolve(prof.SessionTasksRoot)
	cfg := audit.WorkishConfig{
		DescriptionLengthThreshold: prof.SessionTaskWorkish.DescriptionLengthThreshold,
		Markers:                    prof.SessionTaskWorkish.Markers,
	}
	findings, _, err := audit.SessionLens(tasksRoot, "", cfg)
	if err != nil || len(findings) == 0 {
		return false, 0
	}
	return true, len(findings)
}

// countQueueTickets counts every non-dotfile entry under
// queueRoot/<lane>/{pending,in-progress,done} — a plain os.ReadDir, no symlink
// resolution (a dangling entry still counts as "the queue has something in it";
// internal/queue.Sweep is the tool that flags dangling links specifically).
func countQueueTickets(queueRoot string) int {
	lanes, err := os.ReadDir(queueRoot)
	if err != nil {
		return 0
	}
	total := 0
	for _, lane := range lanes {
		if !lane.IsDir() {
			continue // e.g. the ROLES file itself
		}
		for _, status := range []string{"pending", "in-progress", "done"} {
			entries, err := os.ReadDir(filepath.Join(queueRoot, lane.Name(), status))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), ".") {
					total++
				}
			}
		}
	}
	return total
}

// recentGateRecordCount counts recordsDir/*.json files modified within window of
// now — a plain mtime read, no parsing of the record contents (queueBypassCheck
// only needs "was anything gated recently at all", not what).
func recentGateRecordCount(recordsDir string, window time.Duration) int {
	entries, err := os.ReadDir(recordsDir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-window)
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			count++
		}
	}
	return count
}

// --- productivity watcher (docs/build-tasks/productivity-watcher.md) ---
//
// The gap this closes: leadkit watches queue LIVENESS (stalled inboxes, broken
// links via sweep), roster-declared-but-empty-queue bypass (queueBypassCheck), and
// pending-work-with-no-consumer (ownerLivenessCheck) — but NOT PROGRESS. An
// orchestrator can show "running" for 12h, spawn subagents (transient churn), touch
// the fs, and yet produce NO verifiable deliverable — no commit, no recorded gate
// verdict, no queue advancement, no session-task→done. Busy is not productive. This
// watcher correlates ACTIVITY (liveness + churn) against DELIVERABLES and WARNs when
// activity is recent but the last deliverable is older than the stall window.
//
// DISTINCT from the two neighbours (neither subsumes it):
//   - ownerLivenessCheck: PENDING work with NO in-progress consumer (nobody pulling).
//   - queueBypassCheck: roster declared but the queue is empty + work runs off-queue.
//   - THIS: an agent IS active/churning (liveness present) but produced NO deliverable
//     over a window. Activity present + deliverables absent = productive-stall.

// ProductivitySignal is one observed timestamp the productivity watcher correlates:
// When it happened, a best-effort agent/session label (Who), and a human note (What)
// of what produced it.
type ProductivitySignal struct {
	When time.Time
	Who  string // best-effort: the active session id / "an agent"
	What string // human note: "3 subagent output file(s)" / "commit ab12cd" / ...
}

// ProductivityProbe is the injectable seam for the productivity check's two clocks:
// when an agent last showed ACTIVITY (liveness + churn) and when it last produced a
// DELIVERABLE. Each func returns ok=false when NO signal of that kind is readable —
// which the check honours as "can't tell" (Info), never a false Warn. Tests inject a
// fake probe with a fake clock; the real CLI wires RealProductivityProbe.
type ProductivityProbe struct {
	Activity    func() (ProductivitySignal, bool)
	Deliverable func() (ProductivitySignal, bool)
}

// productivityCheck (Warn) is the busy≠productive diagnostic. It reads two clocks off
// the probe and reasons purely over now/window, so it's fully testable with a fake
// clock and injected sources:
//   - no activity signal readable at all → Info ("can't tell"), NEVER a false Warn.
//   - activity older than the window → no agent is currently active → Info.
//   - activity recent AND a deliverable within the window → OK (productive).
//   - activity recent AND the last deliverable is older than the window (or none) →
//     Warn, naming the agent + the gap + the off-queue-invisibility cross-link.
func productivityCheck(now time.Time, window time.Duration, probe ProductivityProbe) Check {
	act, ok := probe.Activity()
	if !ok {
		return Check{Name: "productivity", OK: true, Severity: SeverityInfo,
			Detail: "no readable activity signal (no live session churn, no recent subagent output or session-task writes) — can't tell whether any agent is active; nothing to check (never a false Warn)"}
	}
	activeAgo := now.Sub(act.When)
	if activeAgo > window {
		return Check{Name: "productivity", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("last activity was %s ago (past the %s window) — no agent appears currently active; nothing to check", activeAgo.Round(time.Second), window)}
	}
	who := act.Who
	if who == "" {
		who = "an agent"
	}
	deliv, delivOK := probe.Deliverable()
	if delivOK && now.Sub(deliv.When) <= window {
		return Check{Name: "productivity", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("%s is active (last activity %s ago) and produced a deliverable %s ago (%s) — productive", who, activeAgo.Round(time.Second), now.Sub(deliv.When).Round(time.Second), deliv.What)}
	}
	var gap string
	if delivOK {
		gap = fmt.Sprintf("%s (last: %s)", now.Sub(deliv.When).Round(time.Second), deliv.What)
	} else {
		gap = "no deliverable on record within the window"
	}
	note := ""
	if act.What != "" {
		note = " (" + act.What + ")"
	}
	return Check{Name: "productivity", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s is ACTIVE (last activity %s ago%s) but has produced NO deliverable — no new commit, recorded gate verdict, queue advancement, or session-task→done — for %s (past the %s stall window). Busy is not productive. If it's running OFF the queue its progress is invisible to leadkit (see the queue-bypass check) — drive work through the queue so advancement is a visible deliverable.",
			who, activeAgo.Round(time.Second), note, gap, window)}
}

// ProductivityStallLine renders the SAME compute as productivityCheck for the
// sweep-daemon's per-project report: a single line, returned stalled=true ONLY when
// the check WARNs (a real productive-stall). A productive or can't-tell project
// returns ("", false). It is a REPORT line only — never an action (no reassign/kill;
// that's the human's/lead's call), matching the watcher's read-only design.
func ProductivityStallLine(now time.Time, window time.Duration, probe ProductivityProbe) (string, bool) {
	c := productivityCheck(now, window, probe)
	if c.OK {
		return "", false
	}
	return "  PRODUCTIVE-STALL " + c.Detail, true
}

// --- frozen-claim detector (docs/build-tasks/frozen-claim-and-noninteractive.md) ---
//
// The blind spot this closes: a hung owner stuck INSIDE a foreground tool call (a
// build/suite that never returns, or an approval-gated command unanswerable
// unattended) holds its claimed ticket frozen in in-progress — and no current check
// sees it. owner-liveness reads a CLAIMED in-progress ticket as "consuming"
// (in-progress>0 → OK); productivity needs an ACTIVITY/churn signal a HUNG agent
// doesn't emit (no new tool calls) → it degrades to Info. So a claimed-but-frozen
// in-progress ticket is invisible. This detector catches exactly that shape: an
// in-progress ticket claimed longer than the window (its symlink mtime) with NO
// deliverable advancement (commit / gate record / stage-move) in that window.
//
// HONEST about false positives: a legitimately long task (a real 45m+ build) also
// trips it — that's acceptable, because it's a WARN whose remedy is "PROBE the owner"
// (a live owner answers, confirming busy-not-hung). It degrades to Info — never a
// false Warn, never a hard error — when the claim age or the deliverable signal can't
// be read, and the window is generous by default so a normal task doesn't nag.

// FrozenClaimProbe is the injectable seam for the frozen-claim detector's two sources:
// the in-progress claim census (each ticket + its claim time, reusing queue.Sweep's
// InProgressClaims — never re-walked) and the project-wide DELIVERABLE/advancement
// clock (the SAME deliverableSignal the productivity check reuses). Claims ok=false
// means the census couldn't be read at all (queue unreachable) → the check degrades to
// Info. Tests inject a fake clock + fixed sources; the real CLI wires
// RealFrozenClaimProbe.
type FrozenClaimProbe struct {
	Claims      func() ([]queue.InProgressClaim, bool)
	Deliverable func() (ProductivitySignal, bool)
}

// frozenScan is the shared compute behind both the doctor check and the sweep report
// line, so the two can never disagree about what's frozen. It partitions the claim
// census against the window + the deliverable clock: a claim is FROZEN only when its
// age exceeds the window AND we can AFFIRM no advancement (the deliverable signal is
// readable and older than the window). An unreadable claim mtime or an unreadable
// deliverable signal is a degrade (Info), never a frozen Warn.
type frozenScan struct {
	frozen     []queue.FrozenClaim
	claimsOK   bool // the claim census was readable at all
	delivOK    bool // a deliverable/advancement timestamp was readable
	advancing  bool // a deliverable landed within the window
	total      int  // in-progress claims observed
	stale      int  // claims past the window (frozen candidates), mtime readable
	unreadable int  // claims whose mtime could not be read
}

func frozenClaimScan(now time.Time, window time.Duration, probe FrozenClaimProbe) frozenScan {
	var s frozenScan
	claims, ok := probe.Claims()
	s.claimsOK = ok
	if !ok {
		return s
	}
	s.total = len(claims)
	deliv, delivOK := probe.Deliverable()
	s.delivOK = delivOK
	s.advancing = delivOK && now.Sub(deliv.When) <= window
	for _, c := range claims {
		if !c.ClaimedOK {
			s.unreadable++
			continue
		}
		age := now.Sub(c.ClaimedAt)
		if age <= window {
			continue // within the window — not flagged (no premature nag)
		}
		s.stale++
		// FROZEN only when we can affirm no advancement: the deliverable clock is
		// readable AND older than the window. A missing deliverable signal is
		// "can't tell" → degrade, NOT frozen (never a false Warn).
		if delivOK && !s.advancing {
			s.frozen = append(s.frozen, queue.FrozenClaim{
				Lane: c.Lane, Ticket: c.Ticket, Age: age.Round(time.Second).String(),
			})
		}
	}
	return s
}

// frozenClaimsCheck (Warn) is the frozen-claim doctor check. It reasons purely over
// now/window and the injected sources, so it's fully testable with a fake clock:
//   - census unreadable (queue unreachable) → Info ("can't tell"), never a false Warn.
//   - a claim past the window WITH the project advancing (a deliverable within it) → OK.
//   - a claim past the window but the deliverable signal is unreadable → Info (degrade).
//   - a claim past the window AND the project shows no advancement in it → Warn, naming
//     the ticket + lane + age + the probe→TaskStop→reassign remediation.
func frozenClaimsCheck(now time.Time, window time.Duration, probe FrozenClaimProbe) Check {
	s := frozenClaimScan(now, window, probe)
	if !s.claimsOK {
		return Check{Name: "frozen-claims", OK: true, Severity: SeverityInfo,
			Detail: "no readable in-progress claim census (no roster/queue reachable, or the queue could not be swept) — nothing to check (never a false Warn)"}
	}
	if len(s.frozen) > 0 {
		named := make([]string, 0, len(s.frozen))
		for _, fc := range s.frozen {
			named = append(named, fmt.Sprintf("%s/%s (claimed %s)", fc.Lane, fc.Ticket, fc.Age))
		}
		shown := named
		if len(shown) > 3 {
			shown = shown[:3]
		}
		return Check{Name: "frozen-claims", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("%d in-progress ticket(s) claimed longer than the %s window with NO deliverable advancement (no new commit, gate verdict, or queue stage-move) — the shape of a HUNG owner holding a frozen claim: %s. %s",
				len(s.frozen), window, strings.Join(shown, ", "), queue.FrozenClaimRemediation)}
	}
	if s.stale > 0 && !s.delivOK {
		return Check{Name: "frozen-claims", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("%d in-progress ticket(s) claimed longer than the %s window, but the deliverable/advancement signal is unreadable — can't confirm no-advancement; degrading to Info (never a false Warn)", s.stale, window)}
	}
	if s.stale > 0 && s.advancing {
		return Check{Name: "frozen-claims", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("%d in-progress ticket(s) claimed longer than the %s window, but the project produced a deliverable within it — advancing, not frozen", s.stale, window)}
	}
	if s.unreadable > 0 {
		return Check{Name: "frozen-claims", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("%d in-progress claim(s), but %d claim age(s) could not be read — degrading to Info (never a false Warn)", s.total, s.unreadable)}
	}
	return Check{Name: "frozen-claims", OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("%d in-progress claim(s), none claimed past the %s window — no frozen claims", s.total, window)}
}

// FrozenClaimReport renders the SAME compute as frozenClaimsCheck for the sweep
// daemon's per-project report: one `FROZEN-CLAIM <lane> <ticket> — claimed <age>, no
// advancement` line per frozen claim, plus the structured claims for the findings
// artifact (so status/board see them, self-surfacing). Returns (nil, nil) when nothing
// is frozen. A REPORT only — never an action (the probe→TaskStop→reassign is the
// human's/lead's call), matching the watcher's read-only design.
func FrozenClaimReport(now time.Time, window time.Duration, probe FrozenClaimProbe) ([]string, []queue.FrozenClaim) {
	s := frozenClaimScan(now, window, probe)
	if len(s.frozen) == 0 {
		return nil, nil
	}
	lines := make([]string, 0, len(s.frozen))
	for _, fc := range s.frozen {
		lines = append(lines, fmt.Sprintf("  FROZEN-CLAIM %s %s — claimed %s, no advancement", fc.Lane, fc.Ticket, fc.Age))
	}
	return lines, s.frozen
}

// RealFrozenClaimProbe wires the frozen-claim detector against real fs/git for prof:
// the claim census from queue.Sweep's InProgressClaims (the in-progress symlink mtimes,
// reused — never re-walked), and the deliverable clock from the SAME deliverableSignal
// the productivity check uses (gate records + queue advancement + git commits +
// session-task→done). Every read is best-effort — an unreachable queue reports
// claimsOK=false and an unreadable deliverable reports ok=false, so the check degrades
// to Info rather than a false Warn.
func RealFrozenClaimProbe(prof *config.Profile) FrozenClaimProbe {
	return FrozenClaimProbe{
		Claims: func() ([]queue.InProgressClaim, bool) {
			queueRoot := prof.Resolve(prof.QueueRoot)
			if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
				return nil, false
			}
			res, err := queue.Sweep(queueRoot, false)
			if err != nil {
				return nil, false
			}
			return res.InProgressClaims, true
		},
		Deliverable: func() (ProductivitySignal, bool) {
			return deliverableSignal(prof)
		},
	}
}

// RealProductivityProbe wires the productivity watcher against real fs/git for prof:
// ACTIVITY from session-task churn (transient subagent .output files + recent task
// writes under session_tasks_root), DELIVERABLES from the SAME plumbing the other
// checks reuse — gate records (records_dir/*.json mtimes), queue advancement
// (in-progress/done ticket mtimes), git commits (HEAD committer time), and
// session-tasks that reached a done status (the audit session lens's SessionTask
// shape). Every read is best-effort: an unreadable source contributes no signal
// rather than erroring, so the check degrades to Info, never a false Warn.
func RealProductivityProbe(prof *config.Profile) ProductivityProbe {
	return ProductivityProbe{
		Activity: func() (ProductivitySignal, bool) {
			return sessionActivitySignal(prof.Resolve(prof.SessionTasksRoot))
		},
		Deliverable: func() (ProductivitySignal, bool) {
			return deliverableSignal(prof)
		},
	}
}

// sessionActivitySignal is the ACTIVITY clock: the newest mtime across every
// session-<id>/{*.json task, *.output subagent transcript} under tasksRoot, labelling
// the most-recently-touched session as the active agent and counting its churn. No
// session dirs / no files → ok=false (can't tell it's active).
func sessionActivitySignal(tasksRoot string) (ProductivitySignal, bool) {
	entries, err := os.ReadDir(tasksRoot)
	if err != nil {
		return ProductivitySignal{}, false
	}
	var best ProductivitySignal
	found := false
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "session-") {
			continue
		}
		dir := filepath.Join(tasksRoot, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var newest time.Time
		outputs, tasks := 0, 0
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			switch {
			case strings.HasSuffix(f.Name(), ".output"):
				outputs++
			case strings.HasSuffix(f.Name(), ".json"):
				tasks++
			default:
				continue
			}
			if info, ierr := f.Info(); ierr == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if newest.IsZero() {
			continue
		}
		found = true
		if newest.After(best.When) {
			session := strings.TrimPrefix(e.Name(), "session-")
			best = ProductivitySignal{
				When: newest, Who: "session " + session,
				What: fmt.Sprintf("%d transient subagent output file(s), %d session task(s)", outputs, tasks),
			}
		}
	}
	if !found {
		return ProductivitySignal{}, false
	}
	return best, true
}

// deliverableSignal is the DELIVERABLE clock: the NEWEST timestamp across all four
// deliverable kinds, so a productive agent reads productive no matter WHICH kind of
// deliverable it produces (an off-queue agent with commits still reads productive —
// deliverables are not only queue advancement). ok=false only when NONE is readable.
func deliverableSignal(prof *config.Profile) (ProductivitySignal, bool) {
	var best ProductivitySignal
	found := false
	consider := func(t time.Time, what string, ok bool) {
		if !ok || t.IsZero() {
			return
		}
		if !found || t.After(best.When) {
			best = ProductivitySignal{When: t, What: what}
			found = true
		}
	}

	// Gate records — the same records_dir/*.json plumbing queueBypassCheck reads.
	if t, name, ok := newestJSONMtime(prof.Resolve(prof.RecordsDir)); ok {
		consider(t, "gate verdict "+strings.TrimSuffix(name, ".json"), true)
	}
	// Queue advancement — the mtime of the newest ticket that has moved into an
	// in-progress/ or done/ lane (a stage change is the on-queue deliverable).
	if t, ok := newestQueueAdvance(prof.Resolve(prof.QueueRoot)); ok {
		consider(t, "queue advancement", true)
	}
	// Commits — HEAD committer time in the process cwd (the project repo), same git
	// convention as the branch-whitelist check.
	if t, ok := lastCommitTime(""); ok {
		consider(t, "new commit", true)
	}
	// Session-tasks that reached a done status — reuses audit.SessionTask's shape (the
	// same lens queue-bypass uses), timestamped by the task file's mtime.
	if t, ok := newestDoneSessionTask(prof.Resolve(prof.SessionTasksRoot)); ok {
		consider(t, "session-task→done", true)
	}

	return best, found
}

// newestJSONMtime returns the newest-modified *.json file's mtime + name under dir.
func newestJSONMtime(dir string) (time.Time, string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, "", false
	}
	var newest time.Time
	var name string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil && info.ModTime().After(newest) {
			newest, name = info.ModTime(), e.Name()
		}
	}
	if name == "" {
		return time.Time{}, "", false
	}
	return newest, name, true
}

// newestQueueAdvance returns the newest mtime among tickets already advanced into an
// in-progress/ or done/ lane under queueRoot — a stage change is the on-queue
// deliverable. pending/ is deliberately excluded (enqueued ≠ advanced).
func newestQueueAdvance(queueRoot string) (time.Time, bool) {
	lanes, err := os.ReadDir(queueRoot)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	for _, lane := range lanes {
		if !lane.IsDir() {
			continue
		}
		for _, status := range []string{"in-progress", "done"} {
			ents, err := os.ReadDir(filepath.Join(queueRoot, lane.Name(), status))
			if err != nil {
				continue
			}
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if info, ierr := e.Info(); ierr == nil && info.ModTime().After(newest) {
					newest, found = info.ModTime(), true
				}
			}
		}
	}
	return newest, found
}

// newestDoneSessionTask returns the newest mtime among session tasks whose Status is a
// terminal/done status (in-progress→done is a deliverable) — parsing the SAME
// audit.SessionTask JSON shape the session lens uses, so the two never disagree about
// a task's shape.
func newestDoneSessionTask(tasksRoot string) (time.Time, bool) {
	entries, err := os.ReadDir(tasksRoot)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "session-") {
			continue
		}
		dir := filepath.Join(tasksRoot, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(dir, f.Name()))
			if rerr != nil {
				continue
			}
			var t audit.SessionTask
			if json.Unmarshal(data, &t) != nil {
				continue
			}
			if !isDoneStatus(t.Status) {
				continue
			}
			if info, ierr := f.Info(); ierr == nil && info.ModTime().After(newest) {
				newest, found = info.ModTime(), true
			}
		}
	}
	return newest, found
}

// isDoneStatus reports whether a session-task status reads as terminal/complete.
func isDoneStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "complete", "finished":
		return true
	}
	return false
}

// branchWhitelistCheck (GAP-2/queue-mode-enforcement item 5) warns when the repo's
// CURRENT branch matches no push-guard whitelist pattern — surfacing at doctor/init
// time the exact deny push-guard would produce at push time, before it's a confusing
// surprise. git=="" (nil, the caller declined — see Run's doc) reports Info, not a
// failure: this is an optional check, same convention as launchAgentsDir/
// workstreamsDir=="".
//
// REUSES hook.MatchWhitelist (never reimplements glob matching here) and the SAME
// per-repo override resolution push-guard itself uses (authority.<repo>.push_whitelist
// wins over the top-level default when the origin remote resolves to that repo) — so
// this warning can never disagree with what push-guard actually decides.
func branchWhitelistCheck(prof *config.Profile, git Git) Check {
	if git == nil {
		return Check{Name: "branch-whitelist", OK: true, Severity: SeverityInfo, Detail: "not checked (no git seam given)"}
	}
	branch, ok := git.CurrentBranch("")
	if !ok || branch == "" || branch == "HEAD" {
		return Check{Name: "branch-whitelist", OK: true, Severity: SeverityInfo, Detail: "could not resolve the current branch (detached HEAD, or no git repo here) — nothing to check"}
	}

	whitelist := prof.PushWhitelist
	source := "the top-level push_whitelist"
	if url := git.RemoteURL(""); url != "" {
		// Same shared, deterministic resolver push-guard's target-repo whitelist
		// resolution uses (config.MatchAuthorityKey, filtered to entries with a push_whitelist) — so
		// this warning can never disagree with what push-guard actually decides, and
		// picks the SAME (longest-key) entry when several match.
		if repoKey, a, ok := config.MatchAuthorityKey(url, prof.Authority, true); ok {
			whitelist = a.PushWhitelist
			source = fmt.Sprintf("authority.%s.push_whitelist", repoKey)
		}
	}

	if hook.MatchWhitelist(branch, whitelist) {
		return Check{Name: "branch-whitelist", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("current branch %q matches %s (%s)", branch, source, strings.Join(whitelist, ", "))}
	}
	return Check{
		Name: "branch-whitelist", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("current branch %q matches no push_whitelist pattern (%s) — a `git push` of it will be denied by push-guard; add a pattern or push a whitelisted branch.", branch, strings.Join(whitelist, ", ")),
	}
}

// scopeOriginCheck says whether the review gate can read the origin it scopes this repo by. The gate
// decides whether a push is its business from the remote named `origin`; a repo with a profile and no
// readable `origin` (the remote is named `github`, or there is none) cannot have its scope confirmed,
// so every push is refused in enforce mode and allowed, unjudged, in advisory mode. guard-enforcement
// reports the modes and says nothing about this, so a profile that reads as fully enforcing can still
// be gating nothing.
func scopeOriginCheck(prof *config.Profile, profilePath string, git Git) Check {
	const name = "scope-origin"
	switch {
	case profilePath == "" || git == nil:
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked (no profile found here, or no git seam given)"}
	case git.RemoteURL("") != "":
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "the origin remote is readable, so the review gate can confirm this repo's scope"}
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: "a profile governs this repo (" + profilePath + ") but its `origin` remote cannot be read, so the review gate cannot confirm scope: under guards in enforce mode every push is REFUSED, and in advisory mode pushes are allowed without being judged. Name the remote origin (`git remote rename <name> origin`)"}
}

// guardEnforcementCheck (docs/build-tasks/configurable-guard-enforcement.md) reports each
// security guard's EFFECTIVE enforcement mode (advisory fail-open | enforce fail-closed)
// resolved from prof, so an adopter can answer "are my guards actually enforcing?". It is
// the visibility half of the enforcement config: a guard left advisory is fail-OPEN — an
// error/unverifiable condition ALLOWS — which is exactly the "advisory guard did nothing"
// gap the config exists to close. FAIL-OPEN, never a hard error: it's a gentle nudge, so
// it is Info when every guard enforces and a soft Warn (never Critical) while any security
// guard is still advisory. Read-only: it inspects config only, no git/fs.
func guardEnforcementCheck(prof *config.Profile) Check {
	const name = "guard-enforcement"
	var lines []string
	var advisory []string
	for _, g := range config.SecurityGuards {
		mode := prof.EffectiveGuardMode(g)
		lines = append(lines, g+"="+mode)
		if mode != config.GuardModeEnforce {
			advisory = append(advisory, g)
		}
	}
	posture := strings.Join(lines, ", ")
	if len(advisory) == 0 {
		return Check{Name: name, OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("all security guards ENFORCE (fail-closed): %s — an unverifiable push/edit is DENIED, authoritative", posture)}
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s — %s still advisory (fail-OPEN: an error/unverifiable condition ALLOWS, so a malformed/edge action can slip through). Set guards.default_mode: enforce (or guards.<name>.mode: enforce) for real blocking.",
			posture, strings.Join(advisory, ", "))}
}

// authorityKeyCollisionCheck warns about the two shapes of an accidental
// authority.<repo> ↔ origin match that the loose (substring) convention allows and the
// deterministic longest-key resolver now RESOLVES but does not make safe: (a) a key that
// matches this repo's origin only MID-TOKEN (e.g. `infra` inside `shared-infra`)
// — the fingerprint of an accidental collision, and (b) MULTIPLE keys matching one origin
// (ambiguity — now resolved by longest-wins, but the operator should confirm it's
// intended). Names the offending key(s) + the origin + the remedy. NEVER a hard error
// (fail-open): a missing git seam, an unresolvable origin, or no authority entry is
// Info/OK. Boundary-aligned single matches are OK — this does NOT drop loose matches
// (that would break configs); it SURFACES them.
func authorityKeyCollisionCheck(prof *config.Profile, git Git) Check {
	const name = "authority-key-collision"
	if git == nil {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked (no git seam given)"}
	}
	url := git.RemoteURL("")
	if url == "" {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "no origin remote resolved here — nothing to check"}
	}
	shown := redact.Origin(url) // the remote can carry a credential; every message below shows this
	matches := config.MatchingAuthorityKeys(url, prof.Authority)
	if len(matches) == 0 {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no authority.<repo> key matches origin %q", shown)}
	}

	var loose []string
	for _, k := range matches {
		if !config.AuthorityKeyBoundaryAligned(url, k) {
			loose = append(loose, k)
		}
	}

	var problems []string
	if len(loose) > 0 {
		problems = append(problems, fmt.Sprintf(
			"authority key(s) %s match origin %q only as a LOOSE (mid-word) substring — the shape of an accidental collision; scope the authority key to the repo's path segment, or add an explicit <repo> entry",
			strings.Join(quoteAll(loose), ", "), shown))
	}
	if len(matches) > 1 {
		winner, _, _ := config.MatchAuthorityKey(url, prof.Authority, false)
		problems = append(problems, fmt.Sprintf(
			"multiple authority keys match origin %q: %s — resolution is deterministic (longest key wins: %q), but confirm that ambiguity is intended",
			shown, strings.Join(quoteAll(matches), ", "), winner))
	}
	if len(problems) > 0 {
		return Check{Name: name, OK: false, Severity: SeverityWarn, Detail: strings.Join(problems, "; ")}
	}
	return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("authority key %q matches origin %q at a path/host boundary", matches[0], shown)}
}

// quoteAll returns ss with each element %q-quoted — for stable, readable key lists in
// the authority-key-collision detail.
func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// hookWiringChecks reads every settings.json in settingsPaths (a missing file is
// silently skipped, not an error — this is a preflight, not a strict validator) and
// reports each leadkit guard hook's install status via hookwire.EvaluateWiring.
// Always Warn severity — even a fully-missing set of hooks doesn't fail doctor's
// exit code (leadkit is legitimately usable pre-install, e.g. right after `go
// install` and before running `leadkit init --apply`) — but push-guard's line is
// worded louder: it's the security guard, silently inert is the exact failure mode
// the incident that prompted it was about.
func hookWiringChecks(settingsPaths []string) []Check {
	var maps []map[string][]hookwire.MatcherGroup
	var readErrs []string
	anyFound := false
	for _, p := range settingsPaths {
		hooks, found, err := hookwire.LoadHooks(p)
		if err != nil {
			readErrs = append(readErrs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if found {
			anyFound = true
			maps = append(maps, hooks)
		}
	}

	checks := make([]Check, 0, len(hookwire.ExpectedHooks)+1)
	if !anyFound && len(readErrs) == 0 {
		checks = append(checks, Check{
			Name: "hooks", OK: true, Severity: SeverityWarn,
			Detail: "no settings.json found at any checked location — leadkit hooks not yet installed (run `leadkit init --apply`)",
		})
	}
	for _, e := range readErrs {
		checks = append(checks, Check{Name: "hooks", OK: false, Severity: SeverityWarn, Detail: "could not read " + e})
	}

	merged := hookwire.MergeHooks(maps...)
	statuses := hookwire.EvaluateWiring(merged)
	for _, st := range statuses {
		checks = append(checks, hookWireCheck(st))
	}
	// Wiring is what settings.json SAYS; the probe is whether it would actually run.
	// An unwired guard and a wired-but-unresolvable one look identical from settings.json
	// and identical in behaviour (both enforce nothing, silently).
	checks = append(checks, hookProbeChecks(statuses, hookProber)...)
	return checks
}

// agentTeamsEnvVar is the flag Claude Code reads to enable Agent Teams
// (SendMessage, the reviewer pool, the orchestrator) — set directly in the
// process environment, or via a settings.json `env` block (the standard way
// leadkit's own hooks configure env vars too).
const agentTeamsEnvVar = "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"

// agentTeamsCheck reports whether Agent Teams is enabled — CRITICAL, since the
// whole operating model (reviewer pool, orchestrator, SendMessage) silently fails
// without it. Checked in order: (1) the process's own environment; (2) each
// settingsPaths file's `env` block. Found-and-off is a hard Critical fail
// (explicit or by omission from every readable settings.json — both mean the
// feature isn't on); genuinely unable to check ANY source (no settings.json
// readable anywhere AND the env var unset) degrades to Info, never a false
// Critical fail.
func agentTeamsCheck(settingsPaths []string) Check {
	if v := os.Getenv(agentTeamsEnvVar); v != "" {
		if isTruthy(v) {
			return Check{Name: "agent-teams", OK: true, Severity: SeverityCritical, Detail: agentTeamsEnvVar + " set in the process environment"}
		}
		return agentTeamsOffCheck(fmt.Sprintf("%s=%q in the process environment", agentTeamsEnvVar, v))
	}

	anyReadable := false
	for _, p := range settingsPaths {
		v, found, err := readSettingsEnvVar(p, agentTeamsEnvVar)
		if err != nil || !found {
			continue
		}
		anyReadable = true
		if isTruthy(v) {
			return Check{Name: "agent-teams", OK: true, Severity: SeverityCritical, Detail: fmt.Sprintf("%s=%q in %s", agentTeamsEnvVar, v, p)}
		}
	}
	// Read at least one settings.json successfully (whether or not it mentioned
	// the key) — an absent key is a real, checkable "off", not "couldn't verify".
	for _, p := range settingsPaths {
		if _, err := os.Stat(p); err == nil {
			anyReadable = true
			break
		}
	}
	if anyReadable {
		return agentTeamsOffCheck(fmt.Sprintf("%s not set (not in the environment, and not found in any of: %s)", agentTeamsEnvVar, strings.Join(settingsPaths, ", ")))
	}
	return Check{
		Name: "agent-teams", OK: true, Severity: SeverityInfo,
		Detail: "couldn't verify — no settings.json found and " + agentTeamsEnvVar + " isn't set in the environment; ensure Agent Teams is enabled",
	}
}

func agentTeamsOffCheck(detail string) Check {
	return Check{
		Name: "agent-teams", OK: false, Severity: SeverityCritical,
		Detail: detail + " — Agent Teams is OFF. Enable it: add \"" + agentTeamsEnvVar + "\": \"1\" to settings.json's \"env\" block (or export it in your shell), then restart the session.",
	}
}

// subagentModelEnvVar is the env var (settable directly, or via settings.json's
// `env` block — same convention as agentTeamsEnvVar) Claude Code reads for the
// DEFAULT model subagents/Task-tool agents run on when their own definition
// carries no `model` frontmatter override. Confirmed against Claude Code's
// current docs (code.claude.com/docs/en/sub-agents.md, "Choosing a Model for
// Subagents"; code.claude.com/docs/en/settings-reference.md, "Subagent
// Configuration") — this is NOT the top-level `model` settings key, which governs
// only the main session; subagents have their own separate resolution anchored on
// this var, falling through to the main session's model (often opus, since
// that's usually the lead) only when this is unset entirely — precisely the
// "unset silently means opus" failure mode this check exists to catch.
const subagentModelEnvVar = "CLAUDE_CODE_SUBAGENT_MODEL"

// subagentModelCheck (docs/build-tasks/doctor-subagent-model.md) warns when the
// resolved default subagent model is opus, or unset (which inherits the main
// session's model — usually opus for a lead session): the operating model
// delegates token-heavy IO (polling, research, review, test-running) to cheap
// sonnet/haiku workers specifically so the expensive model isn't burned on it;
// either bad state silently defeats that. Same shape as agentTeamsCheck: the
// process environment first, then each settingsPaths file's `env` block (the
// SAME plumbing/readSettingsEnvVar helper — no second settings-parse path);
// degrades to Info only when genuinely nothing is readable, never a false Warn.
func subagentModelCheck(settingsPaths []string, transcriptsDir string, now time.Time) Check {
	c, resolved := subagentModelSetting(settingsPaths)
	if !resolved && c.OK { // "couldn't verify" — no verdict to back with spend
		return c
	}
	if line, ok := recentSpendLine(transcriptsDir, now, !c.OK); ok {
		c.Detail += " " + line
	}
	return c
}

// subagentModelSetting is the setting-only verdict. resolved is true when a model id
// was actually found (env or settings.json).
func subagentModelSetting(settingsPaths []string) (Check, bool) {
	if v := os.Getenv(subagentModelEnvVar); v != "" {
		return subagentModelVerdict(v, subagentModelEnvVar+"="+strconv.Quote(v)+" in the process environment"), true
	}

	anyReadable := false
	for _, p := range settingsPaths {
		v, found, err := readSettingsEnvVar(p, subagentModelEnvVar)
		if err != nil || !found {
			continue
		}
		return subagentModelVerdict(v, fmt.Sprintf("%s=%q in %s", subagentModelEnvVar, v, p)), true
	}
	// Read at least one settings.json successfully (whether or not it mentioned
	// the key) — an absent key is a real, checkable "not set", not "couldn't
	// verify" — same distinction agentTeamsCheck draws.
	for _, p := range settingsPaths {
		if _, err := os.Stat(p); err == nil {
			anyReadable = true
			break
		}
	}
	if anyReadable {
		return Check{
			Name: "subagent-model", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"default subagent model not set — delegated subagents may inherit the session's model (opus); set it explicitly to sonnet/haiku (%s not in the environment, and not found in any of: %s).",
				subagentModelEnvVar, strings.Join(settingsPaths, ", "),
			),
		}, false
	}
	return Check{
		Name: "subagent-model", OK: true, Severity: SeverityInfo,
		Detail: "couldn't verify — no settings.json found and " + subagentModelEnvVar + " isn't set in the environment; ensure the default subagent model is sonnet/haiku, not opus",
	}, false
}

// Recent-spend snapshot bounds: small window + file cap + wall clock, so doctor stays
// snappy on a large transcripts tree.
const (
	burnWindowDays    = 3
	burnMaxFiles      = 400
	burnScanDeadline  = 4 * time.Second
	burnPointerSuffix = "(see `leadkit burn-report --tools` for the full breakdown)"
)

// recentSpendLine renders the model-mix over the last burnWindowDays days of transcripts
// under dir. FAIL-SOFT: ok=false (no line, no error) for an empty dir, an absent /
// unreadable / empty tree, no in-window spend, or a hit scan bound (a partial total
// would mislead). gap picks the wording: true = the setting is unset/opus (evidence for
// the fix), false = confirmation of a cheap default.
func recentSpendLine(dir string, now time.Time, gap bool) (string, bool) {
	if dir == "" {
		return "", false
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", false
	}
	if now.IsZero() {
		now = time.Now()
	}
	since := now.UTC().AddDate(0, 0, -burnWindowDays)
	dayStart := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	seq, truncated := burnreport.WalkTranscriptsBounded(dir, false, burnreport.Bounds{
		ModifiedSince: dayStart, MaxFiles: burnMaxFiles, Deadline: burnScanDeadline,
	})
	rep := burnreport.Aggregate(seq, burnreport.DefaultPrices, since, time.Time{}, false)
	if truncated() || rep.TotalCost <= 0 {
		return "", false
	}
	pct := func(fam string) (float64, float64) {
		for _, r := range rep.ByModel {
			if r.Key == fam {
				return r.Pct, r.Cost
			}
		}
		return 0, 0
	}
	op, oc := pct("opus")
	sp, _ := pct("sonnet")
	hp, _ := pct("haiku")
	if gap {
		return fmt.Sprintf("recent spend (last %dd): opus %.0f%% (~$%.0f), sonnet %.0f%%, haiku %.0f%% — builders inheriting opus is the gap this setting closes %s",
			burnWindowDays, op, oc, sp, hp, burnPointerSuffix), true
	}
	return fmt.Sprintf("recent spend (last %dd): opus %.0f%% (~$%.0f), sonnet %.0f%%, haiku %.0f%% %s",
		burnWindowDays, op, oc, sp, hp, burnPointerSuffix), true
}

// subagentModelVerdict renders the Warn/OK split once id is actually resolved
// (from wherever — env or a settings.json file); source names exactly where it
// came from, matching agentTeamsCheck's own "cite the source in Detail" habit.
func subagentModelVerdict(id, source string) Check {
	if strings.Contains(strings.ToLower(id), "opus") {
		return Check{
			Name: "subagent-model", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"default subagent model is opus (%s) — the operating model delegates heavy IO to cheap workers; set the default subagent model to sonnet or haiku so delegated agents don't run on opus. (%s)",
				id, source,
			),
		}
	}
	return Check{Name: "subagent-model", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("default subagent model = %s (%s)", id, source)}
}

// isTruthy mirrors how Claude Code itself treats this flag: "1"/"true" (any
// case) enable it, anything else (including "0"/"false"/empty) does not.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// readSettingsEnvVar reads path's top-level `env` object and returns key's raw
// string value. found is false if the file doesn't exist, isn't valid JSON, has
// no `env` object, or `env` doesn't contain key — err is only ever a genuine I/O
// failure (permission denied, ...), never "file absent" (that's found=false).
func readSettingsEnvVar(path, key string) (value string, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return "", false, nil // malformed settings.json — not this check's job to report
	}
	v, ok := s.Env[key]
	if !ok {
		return "", false, nil
	}
	return v, true, nil
}

// linearOutputCheck warns when the merged outputs: includes "linear" but
// linear.team_id is unset — sync plan can still COMPUTE (it doesn't need a
// target), but the sync-executor agent has no workspace to point its Linear MCP
// calls at. That's the only genuine problem state (Warn, not Critical — nothing
// about leadkit itself is broken, only the outward-facing Linear execution step
// is unusable until configured); "linear not in use" and "correctly configured"
// are both healthy states and report Info, never a warning — a passing check
// must never read as noise in an otherwise-clean doctor run.
func linearOutputCheck(prof *config.Profile) Check {
	return linearOutputCheckFor(prof, os.Getenv, linearapi.SecurityKeychain)
}

// linearOutputCheckFor is linearOutputCheck with env/keychain injected. In DIRECT
// mode (linear.sync_mode: direct, or LINEAR_API_KEY in the env) it checks the key
// is reachable — mirroring keychainCheck's "reachable, value discarded": the key
// is never printed. In MCP mode the original team_id check runs unchanged.
func linearOutputCheckFor(prof *config.Profile, getenv func(string) string, kc linearapi.KeychainReader) Check {
	mode, err := linearapi.ResolveMode("", prof.Linear.SyncMode, strings.TrimSpace(getenv(linearapi.EnvKey)) != "")
	if err != nil {
		return Check{Name: "linear-output", OK: false, Severity: SeverityWarn, Detail: "linear.sync_mode is invalid: " + err.Error()}
	}
	if mode == linearapi.ModeDirect && linearUsed(prof) {
		if !linearapi.KeyReachable(getenv, kc, prof.KeychainService) {
			return Check{Name: "linear-output", OK: false, Severity: SeverityWarn,
				Detail: "direct sync mode but no Linear API key is reachable — set LINEAR_API_KEY in the env or add a \"linear\" entry under the configured keychain_service (or use --mode mcp)"}
		}
		if prof.Linear.TeamID == "" {
			return Check{Name: "linear-output", OK: false, Severity: SeverityWarn, Detail: "direct sync mode: key reachable, but linear.team_id is unset — set it before `sync run`"}
		}
		return Check{Name: "linear-output", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("direct sync mode: API key reachable (value discarded), team_id=%s", prof.Linear.TeamID)}
	}
	return mcpLinearOutputCheck(prof)
}

func linearUsed(prof *config.Profile) bool {
	for _, o := range prof.Outputs {
		if o == "linear" {
			return true
		}
	}
	return false
}

func mcpLinearOutputCheck(prof *config.Profile) Check {
	usesLinear := false
	for _, o := range prof.Outputs {
		if o == "linear" {
			usesLinear = true
			break
		}
	}
	if !usesLinear {
		return Check{Name: "linear-output", OK: true, Severity: SeverityInfo, Detail: "outputs: doesn't include linear — nothing to check"}
	}
	if prof.Linear.TeamID == "" {
		return Check{
			Name: "linear-output", OK: false, Severity: SeverityWarn,
			Detail: "outputs includes \"linear\" but linear.team_id is unset — sync plan can still compute, but the sync-executor agent has no workspace target. Set linear.team_id (e.g. team config, project override) before running the executor.",
		}
	}
	return Check{Name: "linear-output", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("outputs includes linear, team_id=%s", prof.Linear.TeamID)}
}

// prGuardRawVerbsCheck warns about a MISCONFIGURED authority entry: any repo
// whose raw_pr_verbs lists "merge" — internal/hook.PRGuard already ignores that
// entry outright (raw merge is never grantable, guard-railed in the guard
// itself), so this is purely a "you wrote something that does nothing" nudge,
// never a security gap (the guard-rail holds regardless of whether this check
// even runs).
func prGuardRawVerbsCheck(prof *config.Profile) Check {
	var misconfigured []string
	for repo, spec := range prof.Authority {
		for _, v := range spec.RawPRVerbs {
			if v == "merge" {
				misconfigured = append(misconfigured, repo)
				break
			}
		}
	}
	if len(misconfigured) == 0 {
		return Check{Name: "pr-guard-raw-verbs", OK: true, Severity: SeverityInfo, Detail: "no authority entry lists merge in raw_pr_verbs"}
	}
	sort.Strings(misconfigured)
	return Check{
		Name: "pr-guard-raw-verbs", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("authority.<repo>.raw_pr_verbs lists \"merge\" for: %s — this has NO effect (PRGuard ignores it; raw merge is never grantable) and should be removed to avoid confusion.", strings.Join(misconfigured, ", ")),
	}
}

// daemonAgentsCheck lists the per-project waker/sweep LaunchAgents found in
// launchAgentsDir (a plain os.ReadDir — read-only, no launchctl involved) by their
// project slug, and flags a bare (un-slugged) legacy agent as a collision risk to
// migrate — see docs/build-tasks/per-project-daemons.md. launchAgentsDir=="" (caller
// declined to check, e.g. non-macOS) and "the dir doesn't exist yet" (nothing
// installed) both report a clean Info, never a failure — this is a preflight
// observation, not a requirement that daemons be installed.
func daemonAgentsCheck(prof *config.Profile, launchAgentsDir string) Check {
	if launchAgentsDir == "" {
		return Check{Name: "daemon-agents", OK: true, Severity: SeverityInfo, Detail: "not checked (no launch-agents-dir given)"}
	}
	entries, err := os.ReadDir(launchAgentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return Check{Name: "daemon-agents", OK: true, Severity: SeverityInfo, Detail: launchAgentsDir + " does not exist yet — no daemons installed (run `leadkit init --apply`)"}
		}
		return Check{Name: "daemon-agents", OK: false, Severity: SeverityWarn, Detail: fmt.Sprintf("could not read %s: %v", launchAgentsDir, err)}
	}

	sluggedSet := map[string]bool{}
	bareWaker, bareSweep := false, false
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".plist") {
			continue
		}
		base := strings.TrimSuffix(name, ".plist")
		switch {
		case base == install.WakerLabelBase:
			bareWaker = true
		case base == install.SweepLabelBase:
			bareSweep = true
		case strings.HasPrefix(base, install.WakerLabelBase+"."):
			sluggedSet[strings.TrimPrefix(base, install.WakerLabelBase+".")] = true
		case strings.HasPrefix(base, install.SweepLabelBase+"."):
			sluggedSet[strings.TrimPrefix(base, install.SweepLabelBase+".")] = true
		}
	}
	slugs := make([]string, 0, len(sluggedSet))
	for s := range sluggedSet {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	bareLegacy := bareWaker || bareSweep

	thisSlug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	thisLabel := thisSlug
	if thisSlug == "" {
		thisLabel = "bare/legacy (no resolvable slug — set project_id to avoid collisions)"
	}

	switch {
	case !bareLegacy && len(slugs) == 0:
		return Check{Name: "daemon-agents", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no leadkit LaunchAgents found in %s", launchAgentsDir)}
	case bareLegacy:
		detail := fmt.Sprintf("bare (un-slugged) legacy waker/sweep agent found in %s — COLLISION RISK: it has no project namespace, so any project's `leadkit init` without a resolvable project_id/queue_root can silently overwrite it", launchAgentsDir)
		if len(slugs) > 0 {
			detail += fmt.Sprintf("; per-project agents also present for: %s", strings.Join(slugs, ", "))
		}
		detail += fmt.Sprintf(". This project resolves to %s — re-run `leadkit init --apply` to migrate off the bare label.", thisLabel)
		return Check{Name: "daemon-agents", OK: false, Severity: SeverityWarn, Detail: detail}
	default:
		return Check{Name: "daemon-agents", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("per-project agents managed: %s (this project: %s)", strings.Join(slugs, ", "), thisLabel)}
	}
}

// wakerInstalledCheck flags the poller gap (docs/build-tasks/adopt-installs-daemons.md):
// a project that HAS a queue (queue_root exists) AND rostered work-roles that need waking,
// but NO waker LaunchAgent installed for its slug, will never have its owners woken when
// work lands in their lanes — the exact adoption gap where a project was scaffolded and
// stood up but nothing ever installed its per-project daemons.
//
// Installed-ness is detected by the waker plist's PRESENCE in launchAgentsDir for this
// project's slug (a read-only os.Stat) — NOT a running PID, since a KeepAlive daemon can
// be mid-restart, and NOT a launchctl call, so doctor stays hermetic and this stays
// test-injectable via the launch-agents dir. WARN (never Critical: a missing daemon
// degrades wake-latency, it doesn't corrupt anything) with the `leadkit init
// --daemons-only --confirm` remediation.
//
// A queue-LESS / gate-only project is NOT flagged (INFO), consistent with the absent-queue
// degradation the rest of doctor honours ([[leadkit-doctor-gate-only-project]]): it has no
// lanes to watch, so no waker is expected. launchAgentsDir=="" (non-macOS / caller
// declined) and "no roster work-roles" are likewise Info — nothing to wake.
func wakerInstalledCheck(prof *config.Profile, launchAgentsDir string) Check {
	const name = "waker-installed"
	if launchAgentsDir == "" {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked (no launch-agents-dir given)"}
	}
	queueRoot := prof.Resolve(prof.QueueRoot)
	if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "no queue (gate-only project) — no lanes to watch, so no waker is expected; nothing to check"}
	}
	// Rostered work-roles are the owners a waker exists to wake. No roster → nobody to
	// wake → not the gap this check is for.
	roleCount := 0
	for _, r := range prof.Roster {
		if r.Type != "" {
			roleCount++
		}
	}
	if roleCount == 0 {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "queue present but no roster work-roles declared — no owners to wake; nothing to check"}
	}

	// The SAME slug ComputeDaemonPlan namespaces the waker plist under — so the label we
	// look for is exactly the one `leadkit init` would install for this project.
	slug := install.ResolveProjectSlug(prof.ProjectID, queueRoot)
	label := install.WakerLabelBase
	if slug != "" {
		label = install.WakerLabelBase + "." + slug
	}
	plistPath := filepath.Join(launchAgentsDir, label+".plist")
	if _, err := os.Stat(plistPath); err == nil {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("waker daemon installed for %s (%s)", label, plistPath)}
	}

	slugName := slug
	if slugName == "" {
		slugName = "this project (no resolvable slug — set project_id)"
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("no waker daemon installed for %s — owners won't be woken when work lands in their lanes (%d roster work-role(s), queue at %s); run `leadkit init --daemons-only --confirm`", slugName, roleCount, queueRoot)}
}

// workstreamRegistryCheck lists the live entries in the account-wide workstream
// registry (docs/build-tasks/cross-project-burn.md) by slug + age, mirroring
// daemonAgentsCheck's per-slug daemon listing. A plain os.ReadDir/os.ReadFile — no
// mutation, and every failure mode here is informational only (this registry is a
// budget optimiser burn.Advise fails open on, never a gate doctor should fail over).
func workstreamRegistryCheck(prof *config.Profile, workstreamsDir string) Check {
	if workstreamsDir == "" {
		return Check{Name: "workstream-registry", OK: true, Severity: SeverityInfo, Detail: "not checked (no workstreams-dir given)"}
	}
	now := time.Now()
	ttl := burn.DefaultWorkstreamTTL
	if prof.Burn.WorkstreamTTL != "" {
		if d, err := time.ParseDuration(prof.Burn.WorkstreamTTL); err == nil {
			ttl = d
		}
	}

	// No cutoff here — doctor wants to SEE stale entries (to flag them), not have
	// them silently dropped the way burn.Advise's own read does.
	entries, ok := usage.ReadWorkstreamRegistry(workstreamsDir, time.Time{})
	if !ok {
		return Check{Name: "workstream-registry", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("could not read %s — burn falls back to this project's own N (fail-open, not a failure)", workstreamsDir)}
	}
	if len(entries) == 0 {
		return Check{Name: "workstream-registry", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no live entries in %s", workstreamsDir)}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	var parts []string
	staleCount := 0
	for _, e := range entries {
		age := now.Sub(e.UpdatedAt)
		stale := age > ttl
		if stale {
			staleCount++
			parts = append(parts, fmt.Sprintf("%s=%d (STALE, age %s)", e.Slug, e.N, age.Round(time.Second)))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%d (age %s)", e.Slug, e.N, age.Round(time.Second)))
		}
	}
	detail := "live workstream entries: " + strings.Join(parts, ", ")
	if staleCount > 0 {
		return Check{Name: "workstream-registry", OK: false, Severity: SeverityWarn, Detail: detail + fmt.Sprintf(" — %d stale (past the %s TTL; excluded from burn's account total)", staleCount, ttl)}
	}
	return Check{Name: "workstream-registry", OK: true, Severity: SeverityInfo, Detail: detail}
}

// presenceCheck lists the account-wide active-session presence registry
// (docs/build-tasks/burn-auto-enforce.md) — the auto-register heartbeats the
// usage-logger PreToolUse hook refreshes on every session activity. Purely
// informational: presence carries NO N and never feeds burn's account_n (a
// reviewer/watcher session is free infra), so it is only a liveness aid, never a gate.
// Read-only (usage.ReadPresence), and the stale window matches the governor's so what
// doctor calls "active" agrees with what the governor counts as live.
func presenceCheck(prof *config.Profile, presenceDir string, now time.Time) Check {
	if presenceDir == "" {
		return Check{Name: "presence", OK: true, Severity: SeverityInfo, Detail: "not checked (no presence-dir given)"}
	}
	window := prof.Burn.WorkstreamStaleWindowDur()
	entries, ok := usage.ReadPresence(presenceDir, now.Add(-window))
	if !ok {
		return Check{Name: "presence", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("could not read %s — presence is advisory-only, not a failure", presenceDir)}
	}
	if len(entries) == 0 {
		return Check{Name: "presence", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no active sessions in %s (within the %s stale window)", presenceDir, window)}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Slug != entries[j].Slug {
			return entries[i].Slug < entries[j].Slug
		}
		return entries[i].SessionID < entries[j].SessionID
	})
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s/%s (age %s)", e.Slug, e.SessionID, now.Sub(e.UpdatedAt).Round(time.Second)))
	}
	return Check{Name: "presence", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("%d active session(s): %s", len(entries), strings.Join(parts, ", "))}
}

// SessionIdentityCheck (docs/build-tasks/session-identity-cwd-honest.md) compares the
// session's cwd-honest IDENTITY slug against the ambient POLICY slug (config.Load →
// install.ResolveProjectSlug). FAIL-OPEN and never critical: a WARN when they DIFFER —
// an inherited $LEADKIT_PROFILE/env or an ancestor leadkit.yaml is shadowing the
// session's real project — naming BOTH slugs + the remedy; Info/OK when they match.
// Identity is resolved by cmd/leadkit (it needs the git-toplevel helper), so the two
// slugs are passed in rather than recomputed here — keeping this a pure, table-testable
// comparison.
func SessionIdentityCheck(identitySlug, identitySource, policySlug string) Check {
	if identitySlug == "" {
		return Check{Name: "session-identity", OK: true, Severity: SeverityInfo,
			Detail: "identity slug unresolved (no git-repo/cwd basename) — addressing falls back to the ambient policy slug"}
	}
	if policySlug != "" && policySlug != identitySlug {
		return Check{Name: "session-identity", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"identity=%s (source: %s) but the ambient profile resolves to %s — an inherited $LEADKIT_PROFILE/$LEADKIT_QUEUE or an ancestor leadkit.yaml is shadowing this session's real project. Your inbox/addressing now uses %s (its own cwd/git-repo); check $LEADKIT_PROFILE/$LEADKIT_QUEUE aren't pointing elsewhere, or add an own leadkit.yaml with project_id: %s.",
				identitySlug, identitySource, policySlug, identitySlug, identitySlug)}
	}
	return Check{Name: "session-identity", OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("identity=%s (source: %s) agrees with the ambient policy slug", identitySlug, identitySource)}
}

// routingInboxStaleWindow is how long a routing item may sit NON-terminal before doctor's
// routing-inbox check flags it as a possible dropped hand-off. Generous — this is an
// advisory nudge to probe the recipient lead, not a precise SLA.
const routingInboxStaleWindow = 24 * time.Hour

// routingInboxCheck (docs/build-tasks/routing-substrate.md) WARNs when any routing item
// has sat NON-terminal past routingInboxStaleWindow — a hand-off the recipient project
// hasn't acted on — naming the kind/id/to_slug/age and the remedy (probe that lead). It
// mirrors the presence/project registry staleness checks and is FAIL-OPEN: an absent
// store, an unreadable one, or no stale items → Info/OK, never a hard error (routing is
// advisory coordination, never a gate). Read-only (a plain store scan).
func routingInboxCheck(routingDir string, window time.Duration, now time.Time) Check {
	if routingDir == "" {
		return Check{Name: "routing-inbox", OK: true, Severity: SeverityInfo, Detail: "not checked (no routing-dir given)"}
	}
	stale, ok := routing.Open(routingDir).StaleNonTerminal(window, now)
	if !ok {
		return Check{Name: "routing-inbox", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("could not read %s — routing is advisory-only, not a failure", routingDir)}
	}
	if len(stale) == 0 {
		return Check{Name: "routing-inbox", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no routing items sitting non-terminal past the %s window", window)}
	}
	var parts []string
	for _, e := range stale {
		parts = append(parts, fmt.Sprintf("%s/%s -> %s (status %q, age %s)", e.Kind, e.ID, e.ToSlug, e.Status, now.Sub(e.UpdatedAt).Round(time.Second)))
	}
	return Check{Name: "routing-inbox", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%d routing item(s) sitting non-terminal past %s — the recipient project hasn't acted on it; probe that lead: %s", len(stale), window, strings.Join(parts, ", "))}
}

// leadRegistryStaleSyncWindow is how long the git-backed registry may go without a
// successful fetch/pull before doctor treats its sync as stale (the shared remote may be
// diverged or offline). Generous — this is an advisory nudge, not an SLA.
const leadRegistryStaleSyncWindow = 2 * time.Hour

// leadRegistryCheck (docs/build-tasks/lead-identity-registry.md, registry-local-default.md)
// is FAIL-OPEN and WARNs on: (a) AMBIGUOUS-LEADERSHIP — a workstream/project with 2+ live
// leads (the core signal, both LOCAL and SYNC mode); (b) a STALE registry sync — SYNC MODE
// ONLY (a configured remote or a git-repo dir), no successful fetch/pull within
// leadRegistryStaleSyncWindow; a LOCAL registry has nothing to sync and never nags about
// staleness/uninitialization; (c) this session UNREGISTERED — no entry for its
// host+session_id. Clean or an empty registry is OK/Info, never a hard error (identity is
// best-effort coordination infra). Read-only: a plain store scan, a filesystem is-git-repo
// check, plus a FETCH_HEAD mtime stat — it never shells out to git.
func leadRegistryCheck(prof *config.Profile, repo, host, sessionID string, now time.Time) Check {
	if repo == "" {
		return Check{Name: "lead-registry", OK: true, Severity: SeverityInfo, Detail: "not checked (no registry repo given)"}
	}
	store := registry.Open(repo)
	// SYNC intent is detected WITHOUT shelling git (doctor's read-only contract): a configured
	// remote, or the dir being a git repo (filesystem check). LOCAL is the default otherwise.
	syncMode := prof.RegistryRemote != "" || store.IsGitRepo()
	entries, ok := store.ReadAll()
	if !ok || len(entries) == 0 {
		if syncMode {
			return Check{Name: "lead-registry", OK: true, Severity: SeverityInfo,
				Detail: fmt.Sprintf("registry %s not initialized or empty — run `leadkit registry init --remote <url>` then `leadkit register` (advisory, not a failure)", repo)}
		}
		return Check{Name: "lead-registry", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("local registry %s has no sessions yet — run `leadkit register --role <lead|ic|reviewer|watcher>` (a remote is optional, only for cross-machine; advisory, not a failure)", repo)}
	}
	ttl := prof.RegistryTTLDur()
	live := registry.Live(entries, now.Add(-ttl))

	var warns []string
	if amb := registry.AmbiguousLeadership(live); len(amb) > 0 {
		var parts []string
		for _, c := range amb {
			var who []string
			for _, e := range c.Leaders {
				who = append(who, e.DisplayName()+"@"+e.Host)
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Slug, strings.Join(who, ", ")))
		}
		warns = append(warns, "AMBIGUOUS-LEADERSHIP — 2+ live leads claim: "+strings.Join(parts, "; "))
	}
	// Stale-sync is a SYNC-mode-only signal: a LOCAL registry (no remote) has nothing to sync,
	// so it is never stale. (In LOCAL mode there is no .git/FETCH_HEAD anyway; the syncMode
	// guard makes the intent explicit and covers a git-repo-without-remote edge.)
	if syncMode {
		if fi, err := os.Stat(filepath.Join(repo, ".git", "FETCH_HEAD")); err == nil {
			if age := now.Sub(fi.ModTime()); age > leadRegistryStaleSyncWindow {
				warns = append(warns, fmt.Sprintf("stale sync — last registry fetch/pull was %s ago (>%s); the shared registry may be diverged or offline (run `leadkit who` to re-sync)",
					age.Round(time.Minute), leadRegistryStaleSyncWindow))
			}
		}
	}
	if sessionID != "" && host != "" {
		found := false
		for _, e := range entries {
			if e.SessionID == sessionID && e.Host == host {
				found = true
				break
			}
		}
		if !found {
			warns = append(warns, fmt.Sprintf("this session (%s@%s) is UNREGISTERED — run `leadkit register --role <lead|ic|reviewer|watcher>`", sessionID, host))
		}
	}

	if len(warns) > 0 {
		return Check{Name: "lead-registry", OK: false, Severity: SeverityWarn, Detail: strings.Join(warns, "; ")}
	}
	return Check{Name: "lead-registry", OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("%d live session(s) registered, no ambiguous leadership", len(live))}
}

// projectRegistryCheck (docs/build-tasks/multi-project-board.md) lists the
// account-wide project registry the multi-project board reads in ACCOUNT
// mode — name/slug/roots/freshness for every registered project, mirroring
// workstreamRegistryCheck's listing so what the board will show is
// inspectable from the CLI without starting it. Read-only (a plain
// os.ReadDir/os.ReadFile via usage.ReadProjectRegistry) and purely
// informational — this registry is a discovery aid the board fails open on
// (an empty/unreadable registry just means an empty index), never a gate.
func projectRegistryCheck(prof *config.Profile, projectsDir string) Check {
	if projectsDir == "" {
		return Check{Name: "project-registry", OK: true, Severity: SeverityInfo, Detail: "not checked (no projects-dir given)"}
	}
	entries, ok := usage.ReadProjectRegistry(projectsDir)
	if !ok {
		return Check{Name: "project-registry", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("could not read %s — the account board falls back to an empty index (fail-open, not a failure)", projectsDir)}
	}
	if len(entries) == 0 {
		return Check{Name: "project-registry", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no projects registered in %s — run `leadkit burn`/`leadkit init` in a project to register it", projectsDir)}
	}

	ttl := burn.DefaultWorkstreamTTL
	if prof.Burn.WorkstreamTTL != "" {
		if d, err := time.ParseDuration(prof.Burn.WorkstreamTTL); err == nil {
			ttl = d
		}
	}
	now := time.Now()
	var parts []string
	staleCount := 0
	for _, e := range entries {
		age := now.Sub(e.UpdatedAt)
		if age > ttl {
			staleCount++
			parts = append(parts, fmt.Sprintf("%s=%s (STALE, age %s)", e.Slug, e.QueueRoot, age.Round(time.Second)))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s (age %s)", e.Slug, e.QueueRoot, age.Round(time.Second)))
		}
	}
	detail := "registered projects: " + strings.Join(parts, ", ")
	if staleCount > 0 {
		return Check{Name: "project-registry", OK: false, Severity: SeverityWarn, Detail: detail + fmt.Sprintf(" — %d stale (past the %s TTL; the board still shows them, greyed)", staleCount, ttl)}
	}
	return Check{Name: "project-registry", OK: true, Severity: SeverityInfo, Detail: detail}
}

// sessionPriorityCheck (session-priority-burn.md + priority-ceiling.md) warns
// on two self-promotion smells: more than one live session DECLARING
// priority=critical, and — the stronger, priority-ceiling.md signal — any
// session whose declared priority exceeds the account-scoped ceiling and was
// therefore clamped. Surfaces the effective ceiling either way, so the
// allocation is visible, not mysterious. Reads the SAME session registry
// `leadkit burn priorities` does (usage.ReadSessionRegistry) — no second read
// path. Fail-open/Info on any read failure, same posture as
// workstreamRegistryCheck (a budget optimiser, never a gate).
func sessionPriorityCheck(prof *config.Profile, workstreamsDir string, ceiling burn.PriorityTier) Check {
	if workstreamsDir == "" {
		return Check{Name: "session-priority", OK: true, Severity: SeverityInfo, Detail: "not checked (no workstreams-dir given)"}
	}
	ttl := burn.DefaultWorkstreamTTL
	if prof.Burn.WorkstreamTTL != "" {
		if d, err := time.ParseDuration(prof.Burn.WorkstreamTTL); err == nil {
			ttl = d
		}
	}
	entries, ok := usage.ReadSessionRegistry(workstreamsDir, time.Now().Add(-ttl))
	if !ok {
		return Check{Name: "session-priority", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("could not read %s — skipped (ceiling: %s)", workstreamsDir, ceiling)}
	}
	if len(entries) == 0 {
		return Check{Name: "session-priority", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("no live sessions registered (ceiling: %s)", ceiling)}
	}

	var criticalKeys, clampedKeys []string
	for _, e := range entries {
		declared := burn.NormalizePriority(e.Priority)
		key := e.Slug + "/" + e.SessionID
		if declared == burn.PriorityCritical {
			criticalKeys = append(criticalKeys, key)
		}
		if burn.ClampPriority(declared, ceiling) != declared {
			clampedKeys = append(clampedKeys, fmt.Sprintf("%s (declared %s)", key, declared))
		}
	}
	sort.Strings(criticalKeys)
	sort.Strings(clampedKeys)

	var problems []string
	if len(clampedKeys) > 0 {
		problems = append(problems, fmt.Sprintf("%d session(s) declared above the ceiling and were clamped: %s", len(clampedKeys), strings.Join(clampedKeys, ", ")))
	}
	if len(criticalKeys) > 1 {
		problems = append(problems, fmt.Sprintf("%d sessions declare priority=critical: %s", len(criticalKeys), strings.Join(criticalKeys, ", ")))
	}
	if len(problems) > 0 {
		return Check{
			Name: "session-priority", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"ceiling: %s — %s — self-declared priority lets any session preemptively shed lower tiers; see `leadkit burn priorities` for the full listing",
				ceiling, strings.Join(problems, "; "),
			),
		}
	}
	return Check{Name: "session-priority", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("ceiling: %s — %d live session(s), none over-ceiling, no more than one critical", ceiling, len(entries))}
}

// hookWireCheck renders one hookwire.WireStatus as a doctor Check.
func hookWireCheck(st hookwire.WireStatus) Check {
	name := "hook:" + st.Name
	switch {
	case st.Wired:
		return Check{Name: name, OK: true, Severity: SeverityWarn, Detail: fmt.Sprintf("wired (matcher=%q)", st.FoundMatcher)}
	case st.Misconfigured():
		return Check{
			Name: name, OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("MISCONFIGURED — found under matcher=%q, which doesn't cover what this hook needs; fix by hand or re-run `leadkit init --apply` after removing the bad entry", st.FoundMatcher),
		}
	case st.Name == "push-guard":
		return Check{
			Name: name, OK: false, Severity: SeverityWarn,
			Detail: "MISSING — built but NOT installed. This is the branch-protection guard (denies pushes off the whitelist); until it's wired, nothing stops a raw `git push` to a protected branch. Run `leadkit init --apply` to install it.",
		}
	default:
		return Check{
			Name: name, OK: false, Severity: SeverityWarn,
			Detail: "MISSING — built but not installed. Run `leadkit init --apply` to install it.",
		}
	}
}

// profileCheck reports where the profile was found (or that Defaults() is in play)
// and whether Validate() is clean. A profile using Defaults() is a legitimate,
// fully-usable state, not itself a failure — only actual Validate() warnings (a
// referenced path that doesn't exist) fail this check, at Warn (pathChecks below
// covers the same ground at the right severity per path).
func profileCheck(prof *config.Profile, profilePath string) Check {
	cwd, _ := os.Getwd()
	return profileCheckAt(prof, profilePath, cwd)
}

// profileCheckAt is profileCheck with the directory the config resolves from explicit.
// Under defaults it NAMES the exact external file leadkit would read plus the command
// that creates it (INFO-level actionable, never a failure by itself).
func profileCheckAt(prof *config.Profile, profilePath, dir string) Check {
	where := profilePath
	if where == "" {
		where = "(no leadkit.yaml found — using built-in defaults)"
		if loc := config.ResolveLocations(dir); loc.ExternalPath != "" {
			where = "no config file — using built-in defaults; would read " + loc.ExternalPath +
				" if created: run `leadkit config init --external` (see `leadkit config path`)"
		}
	}
	warnings := prof.Validate()
	if profilePath == "" {
		// No profile was found. That is not the same as a repo that is deliberately
		// ungoverned: scope and authority both read as "none" under the built-in defaults, so
		// an unresolved profile looks exactly like a project that chose not to gate (ticket 197).
		// Say so, with the HOME the external location came from, before anything acts on it.
		detail := where + "; HOME=" + strconv.Quote(os.Getenv("HOME")) +
			". With no profile `leadkit push` REFUSES a repo it would otherwise call out of scope, and authority-gated verbs (`pr create`, `pr merge`) refuse; if this repo should be governed, the profile is not being found (check HOME and the working directory)"
		if len(warnings) > 0 {
			detail += " — " + fmt.Sprintf("%d validation warning(s): %s", len(warnings), strings.Join(warnings, "; "))
		}
		return Check{Name: "profile", OK: false, Severity: SeverityWarn, Detail: detail}
	}
	if len(warnings) == 0 {
		return Check{Name: "profile", OK: true, Severity: SeverityWarn, Detail: where}
	}
	return Check{
		Name: "profile", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s — %d validation warning(s): %s", where, len(warnings), strings.Join(warnings, "; ")),
	}
}

// pathSpec is one profile path field pathChecks stats.
type pathSpec struct {
	Label    string
	Value    string
	Severity Severity
}

// pathChecks stats every profile-referenced path. store_root/records_dir are CRITICAL
// — leadkit can't do anything useful without them. telemetry_log/session_tasks_root/
// goal_file/handover_file are all files/dirs leadkit (or another tool) creates on first
// use, so their absence is normal on a fresh project — Warn, not Critical.
//
// queue_root/roles_path are CRITICAL only when the queue is PRESENT-but-broken. An
// ABSENT queue (queue_root path does not exist) is NOT corruption: a gate-only /
// queue-less project (push-gate/whitelist scope only) legitimately has no queue, and
// the queue commands self-error if used. So when queue_root is absent, both it and the
// roles_path derived under it degrade to INFO (queue_root resolved to its default —
// nobody declared a queue) or WARN (the adopter EXPLICITLY set queue_root, but the path
// is missing) — never Critical, so a gate-only project passes doctor. See
// docs/build-tasks/doctor-gate-only-project.md.
func pathChecks(prof *config.Profile) []Check {
	queueRoot := prof.Resolve(prof.QueueRoot)
	_, qErr := os.Stat(queueRoot)
	queueAbsent := qErr != nil
	explicit := queueRootExplicit(prof)

	specs := []pathSpec{
		{"queue_root", prof.QueueRoot, SeverityCritical},
		{"store_root", prof.StoreRoot, SeverityCritical},
		{"records_dir", prof.RecordsDir, SeverityCritical},
		{"roles_path", prof.RolesPath, SeverityCritical},
		{"telemetry_log", prof.TelemetryLog, SeverityWarn},
		{"session_tasks_root", prof.SessionTasksRoot, SeverityWarn},
		{"goal_file", prof.GoalFile, SeverityWarn},
		{"handover_file", prof.HandoverFile, SeverityWarn},
	}
	checks := make([]Check, 0, len(specs))
	for _, s := range specs {
		full := prof.Resolve(s.Value)
		_, err := os.Stat(full)
		ok := err == nil
		// ABSENT queue: queue_root itself, and the roles_path derived under it, are not
		// corruption — degrade, never Critical. A PRESENT-but-broken queue (dir exists
		// but roles_path missing under it) falls through to the real Critical below.
		if !ok && queueAbsent && (s.Label == "queue_root" || s.Label == "roles_path") {
			checks = append(checks, absentQueuePathCheck(s.Label, full, explicit))
			continue
		}
		detail := full
		if !ok {
			note := "not found"
			if s.Severity == SeverityWarn {
				note = "not found — created on demand"
			}
			// Name the command that creates it: a CRITICAL with no way out is a dead end.
			switch s.Label {
			case "store_root":
				note += " — `leadkit team scaffold --apply --confirm` creates it"
			case "records_dir":
				note += " — `leadkit init --apply --confirm` creates it"
			}
			detail = fmt.Sprintf("%s (%s)", full, note)
		}
		checks = append(checks, Check{Name: s.Label, OK: ok, Severity: s.Severity, Detail: detail})
	}
	return checks
}

// queueRootExplicit reports whether the adopter DECLARED a queue_root (vs leaving it at
// the built-in default). It compares the profile's queue_root to config.Defaults()'s —
// an unset queue_root stays equal to the default, so a gate-only project that never
// mentioned a queue reads as non-explicit (→ INFO), while a project that set queue_root
// to a real path reads as explicit (→ WARN when that path is missing).
func queueRootExplicit(prof *config.Profile) bool {
	return prof.QueueRoot != config.Defaults().QueueRoot
}

// absentQueuePathCheck renders the degraded queue_root/roles_path check for an ABSENT
// queue: INFO (default resolution — a gate-only/queue-less project) or WARN (the adopter
// explicitly set queue_root but the path is missing). Never Critical — a missing queue
// dir is not corruption; the queue commands error clearly if used.
func absentQueuePathCheck(label, full string, explicit bool) Check {
	if explicit {
		return Check{Name: label, OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("%s (configured queue_root is missing — scaffold it with `leadkit init` or fix the path; a missing queue dir is not corruption, the queue commands error clearly if used)", full)}
	}
	return Check{Name: label, OK: true, Severity: SeverityInfo,
		Detail: fmt.Sprintf("no queue at %s — gate-only/queue-less project (queue commands are unused here), or run `leadkit init` to scaffold if you intended a queue", full)}
}

// rolesCheck parses roles_path itself (not via internal/roles.Resolve/List, which
// silently skip anything that isn't a clean 2-field row) so it can CATCH and report
// exactly the malformed rows those helpers quietly ignore — the live-pollution class
// of bug (prose paragraphs appended into ROLES that parse as bogus role→dir rows).
// A valid row is exactly two whitespace tokens whose 2nd token names a directory that
// EXISTS under queue_root. Any malformed row is Critical: routing (assign/review
// send/gate verdict, in both leadkit and the bash it interops with) resolves through
// this file, so a bad row silently breaks routing rather than erroring loudly.
func rolesCheck(prof *config.Profile) Check {
	queueRoot := prof.Resolve(prof.QueueRoot)
	rolesPath := prof.Resolve(prof.RolesPath)
	// ABSENT queue: there is no ROLES to sanity-check. A gate-only/queue-less project is
	// valid, so this is INFO (default resolution) or WARN (the adopter declared a
	// queue_root that's missing) — never Critical. A PRESENT-but-broken queue (dir
	// exists but ROLES is unreadable/malformed) falls through to the real Critical below.
	if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
		if queueRootExplicit(prof) {
			return Check{Name: "roles-sanity", OK: false, Severity: SeverityWarn,
				Detail: fmt.Sprintf("no queue at %s (configured queue_root is missing) — no ROLES to check; scaffold it with `leadkit init` or fix the path", queueRoot)}
		}
		return Check{Name: "roles-sanity", OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("no queue at %s — no ROLES to check (gate-only/queue-less project), or run `leadkit init` to scaffold if you intended a queue", queueRoot)}
	}
	data, err := os.ReadFile(rolesPath)
	if err != nil {
		return Check{Name: "roles-sanity", OK: false, Severity: SeverityCritical, Detail: fmt.Sprintf("could not read %s: %v", rolesPath, err)}
	}

	var malformed []string
	anyMissingDir := false
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		bad := len(fields) != 2
		missingDir := false
		if !bad {
			if fi, statErr := os.Stat(filepath.Join(queueRoot, fields[1])); statErr != nil || !fi.IsDir() {
				bad = true
				missingDir = true
			}
		}
		if bad {
			malformed = append(malformed, fmt.Sprintf("line %d: %q", i+1, trimmed))
			if missingDir {
				anyMissingDir = true
			}
		}
	}

	if len(malformed) > 0 {
		detail := fmt.Sprintf("%d malformed row(s) in %s: %s", len(malformed), rolesPath, strings.Join(malformed, " | "))
		if anyMissingDir {
			// The exact papercut: a ROLES row references a role lane whose dir was
			// never created. `team standup --scaffold` creates the missing lane dirs.
			detail += " — a row whose 2nd field names a non-existent lane dir is fixed by `leadkit team standup --scaffold --confirm` (creates the missing role lane dirs)"
		}
		return Check{
			Name: "roles-sanity", OK: false, Severity: SeverityCritical,
			Detail: detail,
		}
	}
	return Check{Name: "roles-sanity", OK: true, Severity: SeverityCritical, Detail: rolesPath}
}

// rosterRoleNames extracts prof.Roster's declared Type values — the project's
// canonical owner/specialist roles beyond roles.FixedRoles — for feeding into
// roles.CanonicalLane/roles.KnownRoles. Empty Type entries are skipped.
func rosterRoleNames(roster []config.RosterSpec) []string {
	var out []string
	for _, r := range roster {
		if r.Type != "" {
			out = append(out, r.Type)
		}
	}
	return out
}

// queueLanesCheck (role-keyed-lanes.md) flags any queue/<lane>/ directory whose
// name is NOT a canonical role — the instance-suffixed/ephemeral/per-owner-
// instance sprawl `leadkit queue consolidate` migrates away. Read-only: a plain
// os.ReadDir + the SAME name→role resolver (roles.CanonicalLane) send-to-review/
// gate-verdict/assign/the viewer route through, so this can never disagree with
// them about what counts as a role lane. Warn, not Critical — a mixed queue
// mid-migration (the expected state until the live coordinated pass runs)
// is not itself broken.
func queueLanesCheck(prof *config.Profile) Check {
	queueRoot := prof.Resolve(prof.QueueRoot)
	entries, err := os.ReadDir(queueRoot)
	if err != nil {
		return Check{Name: "queue-lanes", OK: true, Severity: SeverityInfo, Detail: "queue_root not reachable — nothing to check (see the queue_root check above)"}
	}
	known := roles.KnownRoles(rosterRoleNames(prof.Roster))

	var nonRole []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := roles.CanonicalLane(e.Name(), known, prof.LaneAliases); !ok {
			nonRole = append(nonRole, e.Name())
		}
	}
	sort.Strings(nonRole)

	if len(nonRole) == 0 {
		return Check{Name: "queue-lanes", OK: true, Severity: SeverityInfo, Detail: "every queue lane resolves to a canonical role — no legacy/instance sprawl"}
	}
	return Check{
		Name: "queue-lanes", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf(
			"%d non-role queue lane(s) (instance-suffixed/ephemeral/per-owner-instance) still on disk: %s — run `leadkit queue consolidate` (dry-run by default) to plan the move into canonical role lanes. (Conversely, a rostered role that is MISSING its lane dir is created by `leadkit team standup --scaffold --confirm`.)",
			len(nonRole), strings.Join(nonRole, ", "),
		),
	}
}

// laneAliasesCheck (lane-aliases.md) warns on a lane_aliases entry whose TARGET
// isn't a known role (a FixedRole or a roster type) — a dangling alias the LANE
// resolver refuses to honor (it never mints a phantom lane), surfaced here so a
// mis-typed alias is caught before `leadkit queue consolidate` silently leaves the
// lane unmapped. Warn, not Critical — a dangling alias degrades to "no aliasing for
// that lane", never a crash. No lane_aliases configured is a clean pass.
func laneAliasesCheck(prof *config.Profile) Check {
	if len(prof.LaneAliases) == 0 {
		return Check{Name: "lane-aliases", OK: true, Severity: SeverityInfo, Detail: "no lane_aliases configured — nothing to validate"}
	}
	known := roles.KnownRoles(rosterRoleNames(prof.Roster))
	bad := roles.InvalidLaneAliases(prof.LaneAliases, known)
	if len(bad) == 0 {
		return Check{Name: "lane-aliases", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("all %d lane_aliases target a known role", len(prof.LaneAliases))}
	}
	var pairs []string
	for _, k := range bad {
		pairs = append(pairs, fmt.Sprintf("%s -> %s", k, prof.LaneAliases[k]))
	}
	return Check{
		Name: "lane-aliases", OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf(
			"%d lane_aliases entr(y/ies) target a NON-known role (dangling — the resolver refuses them, no lane is created): %s — fix the target to a FixedRole or a roster type.",
			len(bad), strings.Join(pairs, ", "),
		),
	}
}

// fsnotifyCheck confirms a watcher can actually be created and closed — the waker
// (queue.Watch) and the pause lock (lock.Wait) both depend on it, so a broken
// fsnotify here means those silently fall back to polling or fail outright.
func fsnotifyCheck() Check {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return Check{Name: "fsnotify", OK: false, Severity: SeverityCritical, Detail: err.Error()}
	}
	_ = w.Close()
	return Check{Name: "fsnotify", OK: true, Severity: SeverityCritical, Detail: "watcher create+close ok"}
}

// keychainCheck attempts to read the configured keychain_service entry and reports
// only whether it succeeded — the token itself is discarded, never printed or
// stored. Non-critical: only `burn --native` needs this; it falls back to the
// usage-check.sh script on failure.
func keychainCheck(prof *config.Profile) Check {
	if runtime.GOOS != "darwin" {
		return Check{Name: "keychain", OK: true, Severity: SeverityInfo, Detail: "not macOS — skipped"}
	}
	if prof.KeychainService == "" {
		// An EMPTY -s isn't "no filter" to the security CLI the way it might read —
		// it can match an ambiguous set of entries and pop a GUI authorization
		// prompt, which then hangs forever in a non-interactive context (doctor
		// included). Nothing configured means nothing to check.
		return Check{Name: "keychain", OK: true, Severity: SeverityInfo, Detail: "no keychain_service configured — skipped"}
	}
	if _, err := exec.LookPath("security"); err != nil {
		return Check{Name: "keychain", OK: false, Severity: SeverityWarn, Detail: "the `security` binary was not found on PATH"}
	}
	cmd := exec.Command("security", "find-generic-password", "-s", prof.KeychainService, "-w")
	cmd.Stdout = io.Discard // the token itself — never captured, never printed
	if err := cmd.Run(); err != nil {
		return Check{
			Name: "keychain", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("entry %q not reachable (burn --native falls back to -script)", prof.KeychainService),
		}
	}
	return Check{Name: "keychain", OK: true, Severity: SeverityWarn, Detail: fmt.Sprintf("entry %q reachable", prof.KeychainService)}
}

// wakeCommandCheck is the light waker check: an OPTIONAL wake_command that IS set but
// isn't resolvable on PATH surfaces as a Warn — so a misconfigured notifier is caught
// at preflight instead of failing silently on every arrival at runtime. UNSET (the
// default, and correct default now) needs no check at all: the fsnotify `queue wait`
// path is the real wake and depends on nothing external. Always Warn, never an error —
// a broken wake_command degrades to the same event-driven wake an empty one relies on.
func wakeCommandCheck(prof *config.Profile) Check {
	if prof.WakeCommand == "" {
		return Check{Name: "wake-command", OK: true, Severity: SeverityInfo, Detail: "no wake_command configured — the waker relies on `queue wait`'s fsnotify wake path (nothing to check)"}
	}
	fields := strings.Fields(prof.WakeCommand)
	if len(fields) == 0 {
		return Check{Name: "wake-command", OK: true, Severity: SeverityInfo, Detail: "wake_command is blank — nothing to check"}
	}
	bin := fields[0]
	if _, err := exec.LookPath(bin); err != nil {
		return Check{
			Name: "wake-command", OK: false, Severity: SeverityWarn,
			Detail: fmt.Sprintf("wake_command %q is set but %q is not resolvable on PATH — arrivals will fail to exec it (the event-driven `queue wait` fsnotify wake still works regardless); fix the command or unset it.", prof.WakeCommand, bin),
		}
	}
	return Check{Name: "wake-command", OK: true, Severity: SeverityInfo, Detail: fmt.Sprintf("wake_command %q resolvable (%s)", prof.WakeCommand, bin)}
}

// FormatReport renders r as the ✓/✗ checklist + summary `doctor` prints.
func FormatReport(r Report) string {
	var b strings.Builder
	critFail := 0
	for _, c := range r.Checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
			if c.Severity == SeverityCritical {
				critFail++
			}
		}
		fmt.Fprintf(&b, "%s [%s] %-16s %s\n", mark, c.Severity, c.Name, c.Detail)
	}
	if critFail == 0 {
		b.WriteString("doctor: OK — all critical checks pass\n")
	} else {
		fmt.Fprintf(&b, "doctor: FAIL — %d critical check(s) failing\n", critFail)
	}
	return b.String()
}
