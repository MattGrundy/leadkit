package doctor

import (
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/queue"
)

// Frozen-claim detector (docs/build-tasks/frozen-claim-and-noninteractive.md).
//
// These tests drive the pure frozenClaimsCheck / FrozenClaimReport with a FAKE CLOCK
// and injected claim-census + deliverable sources (FrozenClaimProbe), so the
// hung-owner logic is exercised without touching fs/git.

// fakeFrozenProbe builds a FrozenClaimProbe. claimsOK=false means the census couldn't
// be read at all; a nil deliv means "no deliverable signal readable" (ok=false).
func fakeFrozenProbe(claims []queue.InProgressClaim, claimsOK bool, deliv *ProductivitySignal) FrozenClaimProbe {
	return FrozenClaimProbe{
		Claims: func() ([]queue.InProgressClaim, bool) {
			return claims, claimsOK
		},
		Deliverable: func() (ProductivitySignal, bool) {
			if deliv == nil {
				return ProductivitySignal{}, false
			}
			return *deliv, true
		},
	}
}

func claim(lane, ticket string, claimedAt time.Time) queue.InProgressClaim {
	return queue.InProgressClaim{Lane: lane, Ticket: ticket, ClaimedAt: claimedAt, ClaimedOK: true}
}

// TestFrozenClaimsCheck_ClaimedPastWindowNoAdvancementWarns is the core signal (a hung
// owner): an in-progress ticket claimed longer than the window with NO deliverable
// advancement in it → Warn, naming the ticket + lane + age + the probe→TaskStop→
// reassign remedy. The sweep report line surfaces the same finding.
func TestFrozenClaimsCheck_ClaimedPastWindowNoAdvancementWarns(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	claims := []queue.InProgressClaim{claim("devops", "t42", now.Add(-2*time.Hour))}
	// A deliverable exists but predates the window → no advancement in the window.
	deliv := fakeSig(now.Add(-3*time.Hour), "", "new commit")

	c := frozenClaimsCheck(now, window, fakeFrozenProbe(claims, true, deliv))
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (claimed past window, no advancement)", c)
	}
	for _, want := range []string{"devops", "t42", "PROBE the owner", "TaskStop", "reassign"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}

	// The sweep report line renders the same finding as a FROZEN-CLAIM line + structured claims.
	lines, frozen := FrozenClaimReport(now, window, fakeFrozenProbe(claims, true, deliv))
	if len(lines) != 1 || !strings.Contains(lines[0], "FROZEN-CLAIM devops t42") || !strings.Contains(lines[0], "no advancement") {
		t.Errorf("sweep line = %q, want a FROZEN-CLAIM devops t42 ... no advancement line", lines)
	}
	if len(frozen) != 1 || frozen[0].Lane != "devops" || frozen[0].Ticket != "t42" {
		t.Errorf("structured frozen claims = %+v, want one for devops/t42", frozen)
	}
}

// TestFrozenClaimsCheck_ClaimedPastWindowButAdvancingNotFrozen: a ticket claimed past
// the window is NOT frozen when the project produced a deliverable within the window
// (advancing — a real long task, not a hang). No Warn, no sweep line.
func TestFrozenClaimsCheck_ClaimedPastWindowButAdvancingNotFrozen(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	claims := []queue.InProgressClaim{claim("devops", "t42", now.Add(-2*time.Hour))}
	// A recent commit within the window → the project IS advancing.
	deliv := fakeSig(now.Add(-10*time.Minute), "", "new commit")

	c := frozenClaimsCheck(now, window, fakeFrozenProbe(claims, true, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (advancing — not frozen)", c)
	}
	if !strings.Contains(c.Detail, "advancing") {
		t.Errorf("detail = %q, want it to read as advancing", c.Detail)
	}
	if lines, frozen := FrozenClaimReport(now, window, fakeFrozenProbe(claims, true, deliv)); len(lines) != 0 || len(frozen) != 0 {
		t.Errorf("advancing project: want no frozen sweep line, got (%q, %+v)", lines, frozen)
	}
}

// TestFrozenClaimsCheck_WithinWindowNotFlagged: a claim younger than the window is not
// flagged even with no advancement — no premature nag.
func TestFrozenClaimsCheck_WithinWindowNotFlagged(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	claims := []queue.InProgressClaim{claim("devops", "t42", now.Add(-10*time.Minute))}
	deliv := fakeSig(now.Add(-3*time.Hour), "", "new commit")

	c := frozenClaimsCheck(now, window, fakeFrozenProbe(claims, true, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (claim within the window)", c)
	}
	if lines, _ := FrozenClaimReport(now, window, fakeFrozenProbe(claims, true, deliv)); len(lines) != 0 {
		t.Errorf("within-window claim: want no frozen line, got %q", lines)
	}
}

// TestFrozenClaimsCheck_UnreadableDeliverableSignalIsInfo: a claim past the window but
// with NO readable deliverable signal degrades to Info (can't confirm no-advancement) —
// never a false Warn, never a hard error.
func TestFrozenClaimsCheck_UnreadableDeliverableSignalIsInfo(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	claims := []queue.InProgressClaim{claim("devops", "t42", now.Add(-2*time.Hour))}

	c := frozenClaimsCheck(now, window, fakeFrozenProbe(claims, true, nil)) // deliverable unreadable
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (unreadable deliverable → degrade)", c)
	}
	if !strings.Contains(c.Detail, "unreadable") {
		t.Errorf("detail = %q, want it to note the unreadable signal", c.Detail)
	}
	if lines, _ := FrozenClaimReport(now, window, fakeFrozenProbe(claims, true, nil)); len(lines) != 0 {
		t.Errorf("unreadable deliverable: want no false frozen line, got %q", lines)
	}
}

// TestFrozenClaimsCheck_UnreadableClaimAgeIsInfo: a claim whose mtime couldn't be read
// (ClaimedOK=false) contributes no Warn — degrade to Info.
func TestFrozenClaimsCheck_UnreadableClaimAgeIsInfo(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 45 * time.Minute
	claims := []queue.InProgressClaim{{Lane: "devops", Ticket: "t42", ClaimedOK: false}}
	deliv := fakeSig(now.Add(-3*time.Hour), "", "new commit")

	c := frozenClaimsCheck(now, window, fakeFrozenProbe(claims, true, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (unreadable claim age → degrade)", c)
	}
}

// TestFrozenClaimsCheck_UnreadableCensusIsInfo: when the census itself is unreadable
// (queue unreachable) the check degrades to Info, never a false Warn.
func TestFrozenClaimsCheck_UnreadableCensusIsInfo(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	c := frozenClaimsCheck(now, 45*time.Minute, fakeFrozenProbe(nil, false, nil))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (census unreadable)", c)
	}
	if !strings.Contains(c.Detail, "never a false Warn") {
		t.Errorf("detail = %q, want the never-a-false-Warn degrade note", c.Detail)
	}
}

// TestFrozenClaimsCheck_DistinctFromOwnerLiveness proves the detector fires exactly
// where owner-liveness is silent: a CLAIMED (in-progress) ticket reads as "consuming"
// to owner-liveness (in-progress>0 → OK) yet is frozen (no advancement in the window).
func TestFrozenClaimsCheck_DistinctFromOwnerLiveness(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}}
	writeLaneTicket(t, prof.QueueRoot, "devops", "in-progress", "t1.md")

	// owner-liveness reads the in-progress ticket as a live consumer → OK.
	if c := ownerLivenessCheck(prof); !c.OK {
		t.Fatalf("owner-liveness should be OK (a ticket is in-progress); got %+v", c)
	}
	// Yet if that claim is old and nothing has advanced, frozen-claims WARNs.
	claims := []queue.InProgressClaim{claim("devops", "t1", now.Add(-90*time.Minute))}
	deliv := fakeSig(now.Add(-5*time.Hour), "", "new commit")
	if c := frozenClaimsCheck(now, 45*time.Minute, fakeFrozenProbe(claims, true, deliv)); c.OK || c.Severity != SeverityWarn {
		t.Fatalf("frozen-claims should WARN where owner-liveness is silent; got %+v", c)
	}
}

// TestInprogressStallWindowDur_Configurable pins the config parse + fallback the
// detector reads its window through.
func TestInprogressStallWindowDur_Configurable(t *testing.T) {
	if got := (config.Burn{InprogressStallWindow: "90m"}).InprogressStallWindowDur(); got != 90*time.Minute {
		t.Errorf("parsed window = %v, want 90m", got)
	}
	if got := (config.Burn{}).InprogressStallWindowDur(); got != burn.DefaultInprogressStallWindow {
		t.Errorf("empty window = %v, want the default %v", got, burn.DefaultInprogressStallWindow)
	}
	if got := burn.DefaultInprogressStallWindow; got != 45*time.Minute {
		t.Errorf("default inprogress stall window = %v, want 45m", got)
	}
	if got := (config.Burn{InprogressStallWindow: "nonsense"}).InprogressStallWindowDur(); got != burn.DefaultInprogressStallWindow {
		t.Errorf("unparseable window = %v, want the default %v", got, burn.DefaultInprogressStallWindow)
	}
}
