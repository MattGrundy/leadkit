package usage

import (
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
)

var presenceBaseNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func TestWritePresenceEntry_RefreshesUpdatedAt(t *testing.T) {
	dir := t.TempDir()

	if err := WritePresenceEntry(dir, "proj", "s1", presenceBaseNow.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadPresence(dir, time.Time{})
	if !ok || len(entries) != 1 {
		t.Fatalf("want 1 entry, got ok=%v entries=%+v", ok, entries)
	}
	if !entries[0].UpdatedAt.Equal(presenceBaseNow.Add(-10 * time.Minute)) {
		t.Fatalf("first updated_at = %s, want the -10m timestamp", entries[0].UpdatedAt)
	}

	// A second activity refresh overwrites the SAME (slug, session) file with a fresh
	// updated_at — auto-register on activity.
	if err := WritePresenceEntry(dir, "proj", "s1", presenceBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, _ = ReadPresence(dir, time.Time{})
	if len(entries) != 1 {
		t.Fatalf("refresh must overwrite in place, got %d entries", len(entries))
	}
	if !entries[0].UpdatedAt.Equal(presenceBaseNow) {
		t.Errorf("updated_at not refreshed: got %s, want %s", entries[0].UpdatedAt, presenceBaseNow)
	}
}

// TestPresenceEntry_HarnessFieldsRoundTrip: WritePresenceEntryWithHarness persists and
// ReadPresence reads back the harness handle (messaging_socket + harness_session); when
// the env is absent the fields are OMITTED, never errored, and an old entry reads clean.
func TestPresenceEntry_HarnessFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WritePresenceEntryWithHarness(dir, "proj", "s1", "/tmp/cc-socks/1.sock", "uuid-1", presenceBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadPresence(dir, time.Time{})
	if !ok || len(entries) != 1 {
		t.Fatalf("want 1 entry, got ok=%v n=%d", ok, len(entries))
	}
	if entries[0].MessagingSocket != "/tmp/cc-socks/1.sock" || entries[0].HarnessSession != "uuid-1" {
		t.Fatalf("harness handle not round-tripped: %+v", entries[0])
	}

	// Empty handle → fields omitted (backward-safe), still a valid readable entry.
	dir2 := t.TempDir()
	if err := WritePresenceEntryWithHarness(dir2, "proj", "s2", "", "", presenceBaseNow); err != nil {
		t.Fatal(err)
	}
	e2, ok := ReadPresence(dir2, time.Time{})
	if !ok || len(e2) != 1 {
		t.Fatalf("want 1 entry, got ok=%v n=%d", ok, len(e2))
	}
	if e2[0].MessagingSocket != "" || e2[0].HarnessSession != "" {
		t.Errorf("absent handle must stay empty, got %+v", e2[0])
	}
	// And the legacy WritePresenceEntry wrapper never sets the fields.
	dir3 := t.TempDir()
	if err := WritePresenceEntry(dir3, "proj", "s3", presenceBaseNow); err != nil {
		t.Fatal(err)
	}
	e3, _ := ReadPresence(dir3, time.Time{})
	if len(e3) != 1 || e3[0].MessagingSocket != "" || e3[0].HarnessSession != "" {
		t.Errorf("legacy writer must leave handle empty, got %+v", e3)
	}
}

func TestReadPresence_StaleEntryDropped(t *testing.T) {
	dir := t.TempDir()
	// One fresh (5m old) and one stale (20m old) session against a 15m window.
	if err := WritePresenceEntry(dir, "live-proj", "s1", presenceBaseNow.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := WritePresenceEntry(dir, "dead-proj", "s9", presenceBaseNow.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cutoff := presenceBaseNow.Add(-burn.DefaultWorkstreamStaleWindow) // 15m
	entries, ok := ReadPresence(dir, cutoff)
	if !ok {
		t.Fatal("ReadPresence ok=false")
	}
	if len(entries) != 1 || entries[0].Slug != "live-proj" {
		t.Fatalf("want only the fresh live-proj entry, got %+v", entries)
	}
}

func TestReadPresence_MissingDirIsOK(t *testing.T) {
	entries, ok := ReadPresence(t.TempDir()+"/never", time.Time{})
	if !ok || len(entries) != 0 {
		t.Fatalf("missing dir should be ok=true, empty; got ok=%v entries=%+v", ok, entries)
	}
}

// TestPresence_DoesNotFeedAccountN is the anti-inflation guarantee: writing presence
// heartbeats (auto-register) leaves the DECLARED workstream registry — the only thing
// burn's account_n is summed from — completely untouched, so a reviewer/watcher session
// refreshing presence can never bump n.
func TestPresence_DoesNotFeedAccountN(t *testing.T) {
	dir := t.TempDir()

	// One declared workstream (via the workstream registry) → account_n contribution 3.
	if err := WriteWorkstreamEntry(dir, "owner-proj", 3, presenceBaseNow); err != nil {
		t.Fatal(err)
	}
	// Several infra sessions refresh PRESENCE (a separate dir/shape). These must not
	// change account_n.
	presenceDir := t.TempDir()
	for _, s := range []string{"reviewer", "watcher", "poller"} {
		if err := WritePresenceEntry(presenceDir, "infra", s, presenceBaseNow); err != nil {
			t.Fatal(err)
		}
	}

	entries, ok := ReadWorkstreamRegistry(dir, presenceBaseNow.Add(-burn.DefaultWorkstreamStaleWindow))
	if !ok {
		t.Fatal("ReadWorkstreamRegistry ok=false")
	}
	accountN := 0
	for _, e := range entries {
		accountN += e.N
	}
	if accountN != 3 {
		t.Fatalf("account_n = %d, want 3 — presence heartbeats must never inflate n", accountN)
	}
	// And presence itself sees all 3 active infra sessions (visibility without count).
	pres, _ := ReadPresence(presenceDir, presenceBaseNow.Add(-burn.DefaultWorkstreamStaleWindow))
	if len(pres) != 3 {
		t.Fatalf("want 3 active presence entries, got %d", len(pres))
	}
}

// TestWorkstreamStaleEvict_IgnoredByAccountN proves the governor ignores an entry past
// the stale window when computing account_n — a fresh one counts, a stale one doesn't.
func TestWorkstreamStaleEvict_IgnoredByAccountN(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "fresh", 2, presenceBaseNow.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkstreamEntry(dir, "crashed", 4, presenceBaseNow.Add(-40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cutoff := presenceBaseNow.Add(-burn.DefaultWorkstreamStaleWindow)
	entries, ok := ReadWorkstreamRegistry(dir, cutoff)
	if !ok {
		t.Fatal("ok=false")
	}
	total := 0
	for _, e := range entries {
		total += e.N
	}
	if total != 2 {
		t.Fatalf("account_n = %d, want 2 (only the fresh entry; the 40m-stale crashed session is ignored)", total)
	}
}
