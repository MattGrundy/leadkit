package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/routing"
)

// TestRoutingInboxCheck_WarnsOnStaleNonTerminal: a non-terminal item sitting past the
// window → WARN naming it + the remedy.
func TestRoutingInboxCheck_WarnsOnStaleNonTerminal(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := routing.Open(dir)
	if _, err := s.Create(routing.CreateParams{
		Kind: "brief", FromSlug: "sender", ToSlug: "container-base", Subject: "x",
		Status: "sent", Now: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	c := routingInboxCheck(dir, 24*time.Hour, now)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("want a WARN for a stale non-terminal item, got %+v", c)
	}
	for _, want := range []string{"brief", "container-base", "probe that lead"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("WARN detail must name %q: %q", want, c.Detail)
		}
	}
}

// TestRoutingInboxCheck_CleanAndAbsentAreOK: a fresh non-terminal item and an absent
// store both read OK/Info — never a hard error (fail-open).
func TestRoutingInboxCheck_CleanAndAbsentAreOK(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	// Fresh item → OK.
	dir := t.TempDir()
	if _, err := routing.Open(dir).Create(routing.CreateParams{
		Kind: "brief", ToSlug: "recv", Status: "sent", Now: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if c := routingInboxCheck(dir, 24*time.Hour, now); !c.OK || c.Severity == SeverityWarn {
		t.Errorf("a fresh non-terminal item must be OK, got %+v", c)
	}

	// Absent store → OK/Info, not an error.
	absent := filepath.Join(t.TempDir(), "no-store")
	if c := routingInboxCheck(absent, 24*time.Hour, now); !c.OK {
		t.Errorf("an absent routing store must be OK (fail-open), got %+v", c)
	}

	// Empty routing-dir arg → skipped, OK.
	if c := routingInboxCheck("", 24*time.Hour, now); !c.OK {
		t.Errorf("empty routing-dir must skip cleanly, got %+v", c)
	}
}

// TestRoutingInboxCheck_TerminalNeverWarns: a long-closed terminal item is not a dropped
// hand-off — it must never WARN (retention, not staleness, governs terminal items).
func TestRoutingInboxCheck_TerminalNeverWarns(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := routing.Open(dir)
	env, err := s.Create(routing.CreateParams{Kind: "brief", ToSlug: "recv", Status: "sent", Now: now.Add(-100 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(env.Kind, env.ToSlug, env.ID, routing.TransitionParams{
		ExpectedStatus: "sent", NewStatus: "closed", Terminal: true, Now: now.Add(-99 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if c := routingInboxCheck(dir, 24*time.Hour, now); !c.OK {
		t.Errorf("a terminal item must never WARN as a stale hand-off, got %+v", c)
	}
}
