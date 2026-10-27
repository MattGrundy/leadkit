package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The findings artifact (docs/build-tasks/self-surfacing-diagnostics.md, Part 2) is
// the single machine-readable source every self-surfacing reader consumes — doctor's
// daemon-diagnostics check, `leadkit status`, and the board's /health all read the
// SAME file, exactly as push-guard's violations-log feeds both doctor and the board.
// Each `leadkit queue sweep` run writes it best-effort: a write failure NEVER fails
// the sweep (the sweep's own exit code is a daemon-health signal, decoupled from what
// it finds). Written whole (marshal + replace), not appended — it's a current-state
// snapshot, not an event log.

// SweepStatus is one sweep run's findings, serialized to sweep_status_file. Field
// names are the file's stable JSON contract; readers in other packages
// (internal/viewer's /health) may decode it into their own struct rather than import
// this type, the same file-based-contract convention push-violations uses.
type SweepStatus struct {
	TS           string           `json:"ts"` // RFC3339 UTC — when the sweep ran
	Stalled      int              `json:"stalled"`
	Broken       int              `json:"broken"`
	Blocked      []BlockedTicket  `json:"blocked,omitempty"`      // pending tickets that are status: blocked (reported, never stalled)
	Finished     []FinishedTicket `json:"finished,omitempty"`     // pending tickets in a terminal status (reported, never stalled)
	Stray        int              `json:"stray,omitempty"`        // tickets stranded in non-canonical state dirs
	StrayStates  []StrayState     `json:"stray_states,omitempty"` // where, so a reader can name them
	StalledLanes []StalledLane    `json:"stalled_lanes,omitempty"`
	BrokenLanes  []BrokenLane     `json:"broken_lanes,omitempty"`
	Milestone    string           `json:"milestone,omitempty"`     // the per-tick re-anchor line
	FrozenClaims []FrozenClaim    `json:"frozen_claims,omitempty"` // hung-owner held tickets (frozen-claim detector)
}

// FrozenClaim is one IN-PROGRESS ticket whose claim age exceeds
// burn.inprogress_stall_window with NO deliverable advancement in that window — the
// shape of a hung owner holding a ticket that isn't moving. Recorded in the findings
// artifact so status/board see it, exactly as stalls/broken-links are. Age is the
// human claim age ("1h2m") the report line prints; the detector that produced it is
// internal/doctor's frozen-claims check (queue can't import doctor — so the detector
// writes THIS shape).
type FrozenClaim struct {
	Lane   string `json:"lane"`
	Ticket string `json:"ticket"`
	Age    string `json:"age"`
}

// BuildSweepStatus assembles the artifact from a SweepResult, the milestone re-anchor
// line, and the run's timestamp (injectable so tests use a fixed clock).
func BuildSweepStatus(res SweepResult, milestone string, now time.Time) SweepStatus {
	return SweepStatus{
		TS:           now.UTC().Format(time.RFC3339),
		Stalled:      res.Stalled,
		Broken:       res.Broken,
		Blocked:      res.Blocked,
		Finished:     res.Finished,
		Stray:        res.Stray,
		StrayStates:  res.StrayStates,
		StalledLanes: res.StalledLanes,
		BrokenLanes:  res.BrokenLanes,
		Milestone:    strings.TrimSpace(milestone),
	}
}

// WriteSweepStatus writes s as indented JSON to path (creating the parent dir). The
// caller treats this as best-effort — a returned error must never fail the sweep; it's
// returned only so a test/caller that DOES care can assert on it. A "" path is a no-op.
func WriteSweepStatus(path string, s SweepStatus) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadSweepStatus reads the findings artifact at path. present=false (with a nil
// error) means the artifact is absent or path is "" — the "daemon may not be running"
// degrade every reader treats as Info, never a false alarm. A genuine parse/read error
// is returned so a corrupt artifact is reported, not silently read as "all clear".
func ReadSweepStatus(path string) (status SweepStatus, present bool, err error) {
	if path == "" {
		return SweepStatus{}, false, nil
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return SweepStatus{}, false, nil
		}
		return SweepStatus{}, false, rerr
	}
	var s SweepStatus
	if uerr := json.Unmarshal(data, &s); uerr != nil {
		return SweepStatus{}, false, uerr
	}
	return s, true, nil
}

// Age returns how old the artifact is relative to now, and whether its timestamp
// parsed. An unparseable/empty TS reports ok=false — a reader then treats it as
// unknown-age rather than assuming freshness.
func (s SweepStatus) Age(now time.Time) (age time.Duration, ok bool) {
	if s.TS == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s.TS)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

// Clean reports whether the sweep found nothing to surface (no stalls, no broken
// links, no stranded tickets) — the healthy steady state. Frozen claims are surfaced separately (they have
// a dedicated doctor check + their own status/board block), so they do NOT flip Clean
// — the daemon-diagnostics stall/broken surface stays focused on those two.
func (s SweepStatus) Clean() bool {
	return s.Stalled == 0 && s.Broken == 0 && s.Stray == 0
}

// FrozenClaimRemediation is the one-line probe→TaskStop→reassign remedy every
// frozen-claim surface prints — the WHOLE point of the signal: it names what
// owner-liveness (reads in-progress>0 as "consuming") and productivity (needs an
// activity signal a hung agent doesn't emit) cannot. A live owner answers a probe; a
// hung one is silent (stuck INSIDE a blocking tool call, unreachable until it returns).
const FrozenClaimRemediation = "PROBE the owner — a live owner answers, a HUNG one is silent (stuck in a blocking tool call, unreachable until it returns). If silent, `TaskStop` the owner + reassign the ticket (`queue move <queue_root>/<lane>/in-progress/<t> <queue_root>/<lane>/pending/` then re-assign, or `queue assign`)."

// stalledLaneNames / brokenLaneNames flatten the structured findings into short
// "lane" and "lane: link link" lists for the one-line remediation strings.
func (s SweepStatus) stalledLaneNames() []string {
	out := make([]string, 0, len(s.StalledLanes))
	for _, l := range s.StalledLanes {
		out = append(out, l.Lane)
	}
	return out
}

func (s SweepStatus) brokenLaneNames() []string {
	out := make([]string, 0, len(s.BrokenLanes))
	for _, l := range s.BrokenLanes {
		out = append(out, l.Lane)
	}
	return out
}

// Remediation renders the human "here's what's wrong AND the fix" line a lead-less
// adopter reads — the whole point of self-surfacing. selfRepair (report|command|auto)
// steers the broken-link fix wording: `command` names `leadkit queue repair`, `auto`
// says the sweep reaps them itself, `report` just surfaces. Returns "" when there's
// nothing to remediate (a clean sweep).
func (s SweepStatus) Remediation(selfRepair string) string {
	if s.Clean() && len(s.FrozenClaims) == 0 {
		return ""
	}
	var parts []string
	if s.Broken > 0 {
		fix := "run `leadkit queue repair --apply` to reap them"
		switch selfRepair {
		case "auto":
			fix = "the sweep reaps them automatically (diagnostics.self_repair=auto)"
		case "report":
			fix = "reap them with `leadkit queue repair --apply` (surfacing only — diagnostics.self_repair=report)"
		}
		parts = append(parts, fmt.Sprintf("%d broken link(s) in %s → %s",
			s.Broken, strings.Join(s.brokenLaneNames(), ", "), fix))
	}
	if s.Stalled > 0 {
		parts = append(parts, fmt.Sprintf("%d stalled lane(s) %s → converge/assign work or `leadkit queue sweep --poke` to re-fire the watcher",
			s.Stalled, strings.Join(s.stalledLaneNames(), ", ")))
	}
	if s.Stray > 0 {
		var where []string
		for _, ss := range s.StrayStates {
			where = append(where, ss.Lane+"/"+ss.Dir+"/"+strings.Join(ss.Tickets, ","))
		}
		parts = append(parts, fmt.Sprintf("%d ticket(s) stranded in a non-canonical state dir (%s) — no owner is woken for them → `leadkit queue move <queue_root>/<lane>/<dir>/<t> <queue_root>/<lane>/pending/`, or `leadkit queue unlink <path> --apply` for a duplicate link",
			s.Stray, strings.Join(where, "; ")))
	}
	if len(s.FrozenClaims) > 0 {
		names := make([]string, 0, len(s.FrozenClaims))
		for _, fc := range s.FrozenClaims {
			names = append(names, fc.Lane+"/"+fc.Ticket)
		}
		parts = append(parts, fmt.Sprintf("%d frozen claim(s) %s → %s",
			len(s.FrozenClaims), strings.Join(names, ", "), FrozenClaimRemediation))
	}
	return strings.Join(parts, "; ")
}

// FormatStatusSurface renders the operator-facing block `leadkit status` prints (and
// the shape a SessionStart hook wiring `leadkit status` shows on check-in): the
// findings, then the remediation, degrading to a one-liner when the artifact is
// absent/stale/clean. staleWindow bounds "stale" (0 disables the staleness note).
func FormatStatusSurface(path string, s SweepStatus, present bool, selfRepair string, staleWindow time.Duration, now time.Time) string {
	var b strings.Builder
	b.WriteString("--- daemon diagnostics ---\n")
	if !present {
		fmt.Fprintf(&b, "no sweep findings yet (%s absent) — the sweep daemon may not have run; check with `leadkit queue sweep`\n", path)
		return b.String()
	}
	if age, ok := s.Age(now); ok && staleWindow > 0 && age > staleWindow {
		fmt.Fprintf(&b, "sweep findings are STALE (last run %s ago, past %s) — the sweep daemon may be stopped; check with `leadkit queue sweep`\n", age.Round(time.Second), staleWindow)
	}
	if s.Clean() && len(s.FrozenClaims) == 0 {
		b.WriteString("no stalls, no broken links — queue healthy\n")
		// Blocked tickets are expected, not broken, so they do not make the queue unhealthy, but
		// they are still listed: an owner is not woken for them, so this is the only place they show.
		for _, bt := range s.Blocked {
			fmt.Fprintf(&b, "  BLOCKED %s — %s%s\n", bt.Lane, bt.Ticket, blockedOnClause(bt.BlockedOn))
		}
		for _, ft := range s.Finished {
			fmt.Fprintf(&b, "  FINISHED %s — %s (status: %s) left in pending\n", ft.Lane, ft.Ticket, ft.Status)
		}
		return b.String()
	}
	for _, l := range s.BrokenLanes {
		fmt.Fprintf(&b, "  BROKEN  %s — %s\n", l.Lane, strings.Join(l.Links, " "))
	}
	for _, l := range s.StalledLanes {
		fmt.Fprintf(&b, "  STALLED %s — %d pending, 0 in-progress\n", l.Lane, l.Pending)
	}
	for _, bt := range s.Blocked {
		fmt.Fprintf(&b, "  BLOCKED %s — %s%s\n", bt.Lane, bt.Ticket, blockedOnClause(bt.BlockedOn))
	}
	for _, ft := range s.Finished {
		fmt.Fprintf(&b, "  FINISHED %s — %s (status: %s) left in pending\n", ft.Lane, ft.Ticket, ft.Status)
	}
	for _, ss := range s.StrayStates {
		fmt.Fprintf(&b, "  STRAY   %s/%s — %d ticket(s) in a directory no owner watches: %s\n", ss.Lane, ss.Dir, len(ss.Tickets), strings.Join(ss.Tickets, " "))
	}
	for _, fc := range s.FrozenClaims {
		fmt.Fprintf(&b, "  FROZEN-CLAIM %s %s — claimed %s, no advancement\n", fc.Lane, fc.Ticket, fc.Age)
	}
	if rem := s.Remediation(selfRepair); rem != "" {
		fmt.Fprintf(&b, "remediation: %s\n", rem)
	}
	return b.String()
}
