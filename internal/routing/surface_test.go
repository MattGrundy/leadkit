package routing

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestSurfaceOnceOpenItemThenDedupe: an open item surfaces once; a repeat within the
// window is silent (deduped); after the window it surfaces again.
func TestSurfaceOnceOpenItemThenDedupe(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	mustCreate(t, s, "brief", "recv", "sent", nil, now.Add(-time.Hour))

	line, ok := SurfaceOnce(dir, "recv", "recv-1", 30*time.Minute, now)
	if !ok || !strings.Contains(line, "open routing item") {
		t.Fatalf("first activity with an open item must surface, got ok=%v line=%q", ok, line)
	}

	// Within the window → deduped silent.
	if _, ok := SurfaceOnce(dir, "recv", "recv-1", 30*time.Minute, now.Add(5*time.Minute)); ok {
		t.Error("a repeat within the dedupe window must be silent")
	}

	// After the window → surfaces again.
	if _, ok := SurfaceOnce(dir, "recv", "recv-1", 30*time.Minute, now.Add(31*time.Minute)); !ok {
		t.Error("after the dedupe window it must surface again")
	}

	// A DIFFERENT session is not deduped by the first session's marker.
	if _, ok := SurfaceOnce(dir, "recv", "recv-2", 30*time.Minute, now.Add(5*time.Minute)); !ok {
		t.Error("a different session must surface independently (per-session dedupe)")
	}
}

// TestSurfaceOnceOnlyOpenItems: a project whose only items are terminal stays silent.
func TestSurfaceOnceOnlyOpenItems(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()
	env := mustCreate(t, s, "brief", "recv", "sent", nil, now)
	if _, err := s.Transition(env.Kind, env.ToSlug, env.ID, TransitionParams{
		ExpectedStatus: "sent", NewStatus: "closed", Terminal: true, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := SurfaceOnce(dir, "recv", "recv-1", 30*time.Minute, now); ok {
		t.Error("only-terminal items must not surface")
	}
	// A slug with no items at all → silent.
	if _, ok := SurfaceOnce(dir, "empty-slug", "s-1", 30*time.Minute, now); ok {
		t.Error("no items must be silent")
	}
}

// TestSurfaceOnceFailOpen: an unreadable store, or an unresolvable identity, is silent
// (never an error surfaced onto the tool call).
func TestSurfaceOnceFailOpen(t *testing.T) {
	// Unresolvable identity.
	if _, ok := SurfaceOnce(t.TempDir(), "", "s-1", time.Minute, time.Now()); ok {
		t.Error("empty slug must be silent (fail-open)")
	}
	if _, ok := SurfaceOnce(t.TempDir(), "recv", "", time.Minute, time.Now()); ok {
		t.Error("empty sessionID must be silent (fail-open)")
	}
	if _, ok := SurfaceOnce("", "recv", "s-1", time.Minute, time.Now()); ok {
		t.Error("empty dir must be silent (fail-open)")
	}

	// Unreadable store dir (a file where the store root should be) → silent, not a panic.
	tmp := t.TempDir()
	fileStore := tmp + "/notadir"
	if err := os.WriteFile(fileStore, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := SurfaceOnce(fileStore, "recv", "s-1", time.Minute, time.Now()); ok {
		t.Error("an unreadable store must be silent (fail-open)")
	}
}
