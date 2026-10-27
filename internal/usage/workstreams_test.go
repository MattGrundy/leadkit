package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/install"
)

var wsBaseNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func TestWriteWorkstreamEntry_EmptySlugIsANoOp(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "", 3, wsBaseNow); err != nil {
		t.Fatalf("WriteWorkstreamEntry with empty slug should be a no-op, got err: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("want nothing written for an empty slug, found: %v", entries)
	}
}

func TestWriteThenReadWorkstreamEntry_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "legacy-migration", 4, wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadWorkstreamRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(entries) != 1 || entries[0].Slug != "legacy-migration" || entries[0].N != 4 {
		t.Fatalf("got %+v", entries)
	}
}

func TestReadWorkstreamRegistry_MissingDir_OkTrueEmptyRegistry(t *testing.T) {
	entries, ok := ReadWorkstreamRegistry(filepath.Join(t.TempDir(), "does-not-exist"), wsBaseNow)
	if !ok {
		t.Error("a missing registry dir is the normal single-project state — want ok=true (fail OPEN, never block)")
	}
	if len(entries) != 0 {
		t.Errorf("want no entries, got %+v", entries)
	}
}

func TestReadWorkstreamRegistry_UnreadableDir_OkFalse(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits don't block reads")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // so t.TempDir() cleanup can remove it

	_, ok := ReadWorkstreamRegistry(locked, wsBaseNow)
	if ok {
		t.Error("an unreadable (permission-denied) registry dir must report ok=false so the caller falls back to in.N")
	}
}

// TestReadWorkstreamRegistry_TTL_StalePrunedFreshKept is the fake-clock TTL test: an
// entry older than the cutoff is dropped from the sum, a fresh one is kept — the
// mechanism that stops a crashed/idle project holding phantom account headroom.
func TestReadWorkstreamRegistry_TTL_StalePrunedFreshKept(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "stale-project", 5, wsBaseNow.Add(-45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkstreamEntry(dir, "fresh-project", 2, wsBaseNow.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	ttl := 30 * time.Minute
	entries, ok := ReadWorkstreamRegistry(dir, wsBaseNow.Add(-ttl))
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(entries) != 1 || entries[0].Slug != "fresh-project" {
		t.Fatalf("want only the fresh entry kept, got %+v", entries)
	}
}

func TestOtherProjectsTotal_ExcludesOwnSlug(t *testing.T) {
	entries := []WorkstreamEntry{
		{Slug: "legacy-migration", N: 3},
		{Slug: "another-project", N: 2},
	}
	if got := OtherProjectsTotal(entries, "legacy-migration"); got != 2 {
		t.Errorf("OtherProjectsTotal = %d, want 2 (another-project only)", got)
	}
	if got := OtherProjectsTotal(entries, "some-third-project"); got != 5 {
		t.Errorf("OtherProjectsTotal = %d, want 5 (neither entry excluded)", got)
	}
}

// TestSlugKey_MatchesResolveProjectSlug asserts the registry key is exactly what
// install.ResolveProjectSlug returns — cross-project-burn.md's hard requirement:
// REUSE the daemon namespacing's slug function, don't reimplement it, so a project's
// burn key and its daemon key can never drift apart.
func TestSlugKey_MatchesResolveProjectSlug(t *testing.T) {
	slug := install.ResolveProjectSlug("", "/Users/x/another-project/queue")
	if slug != "another-project" {
		t.Fatalf("setup check: ResolveProjectSlug = %q, want another-project", slug)
	}
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, slug, 1, wsBaseNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "another-project.json")); err != nil {
		t.Errorf("want the registry file keyed by ResolveProjectSlug's output, got: %v", err)
	}
}

// TestTwoSlugs_CapsAccountTotal is THE end-to-end cross-project regression: two real
// projects write their own live entries into ONE shared registry; the third project
// (B) reads it, sums the OTHERS via OtherProjectsTotal, and feeds the account total
// into burn.Advise — which must hold B rather than scale it into headroom project A
// has already claimed (the same core guarantee per-project-daemons.md's no-collision
// test proves for the daemon side, proven here for the burn governor side).
func TestTwoSlugs_CapsAccountTotal(t *testing.T) {
	dir := t.TempDir()

	slugA := install.ResolveProjectSlug("", "/Users/x/legacy-migration/queue")
	slugB := install.ResolveProjectSlug("", "/Users/x/another-project/queue")
	if slugA == slugB {
		t.Fatalf("setup check: want distinct slugs, got %q twice", slugA)
	}

	// Project A already has 7 live workstreams claimed, freshly reported.
	if err := WriteWorkstreamEntry(dir, slugA, burn.MaxN-1, wsBaseNow); err != nil {
		t.Fatal(err)
	}

	usage := burn.Usage{
		SessionPct: 50, SessionResetsAt: wsBaseNow.Add(1 * time.Hour), // ratio≈2.63 — B's OWN state alone would scale up
		WeekPct: 50, WeekResetsAt: wsBaseNow.Add(10 * time.Hour),
	}

	// Sanity: B's own state alone (no cross-project awareness) would scale up.
	solo := burn.Advise(burn.Input{Now: wsBaseNow, Usage: usage, N: 1})
	if solo.N != 2 {
		t.Fatalf("setup check: B alone should scale 1->2, got %d (verdict: %s)", solo.N, solo.Verdict)
	}

	// B computes the account total the way runBurn does: write its own entry, read
	// the registry, sum everyone else, add its own N.
	if err := WriteWorkstreamEntry(dir, slugB, 1, wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadWorkstreamRegistry(dir, wsBaseNow.Add(-burn.DefaultWorkstreamTTL))
	if !ok {
		t.Fatal("want ok=true")
	}
	accountN := 1 + OtherProjectsTotal(entries, slugB)
	if accountN != burn.MaxN {
		t.Fatalf("setup check: want accountN==MaxN (%d), got %d from entries %+v", burn.MaxN, accountN, entries)
	}

	held := burn.Advise(burn.Input{Now: wsBaseNow, Usage: usage, N: 1, AccountN: accountN})
	if held.N != 1 {
		t.Fatalf("want B held at 1 (account already at the ceiling via A's claimed workstreams), got %d — verdict: %s", held.N, held.Verdict)
	}
}

// --- session registry (session-priority-burn.md) ---

func TestWriteSessionEntry_EmptySlugOrSessionIDIsANoOp(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSessionEntry(dir, "", "sess1", 2, "high", wsBaseNow); err != nil {
		t.Fatalf("empty slug: %v", err)
	}
	if err := WriteSessionEntry(dir, "proj", "", 2, "high", wsBaseNow); err != nil {
		t.Fatalf("empty session id: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("want nothing written, found: %v", entries)
	}
}

func TestWriteThenReadSessionEntry_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSessionEntry(dir, "proj", "sess1", 3, "critical", wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadSessionRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(entries) != 1 || entries[0].Slug != "proj" || entries[0].SessionID != "sess1" || entries[0].N != 3 || entries[0].Priority != "critical" {
		t.Fatalf("got %+v", entries)
	}
}

// TestReadSessionRegistry_TwoSessionsSameProjectDoNotContend proves the actual
// point of per-session files: two concurrent sessions of ONE project each get
// their own live entry rather than one clobbering the other's file.
func TestReadSessionRegistry_TwoSessionsSameProjectDoNotContend(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSessionEntry(dir, "proj", "sess1", 2, "low", wsBaseNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionEntry(dir, "proj", "sess2", 3, "critical", wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadSessionRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok || len(entries) != 2 {
		t.Fatalf("want 2 independent entries, got ok=%v entries=%+v", ok, entries)
	}
}

// TestReadSessionRegistry_FallsBackToLegacyFlatFile is the backward-read-compat
// regression: a slug with ONLY a legacy WriteWorkstreamEntry-shaped
// <slug>.json (a not-yet-upgraded writer, e.g. #123's binary) must still be
// visible as one synthesized, normal-priority session entry.
func TestReadSessionRegistry_FallsBackToLegacyFlatFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "legacy-project", 4, wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadSessionRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok || len(entries) != 1 {
		t.Fatalf("want 1 synthesized entry, got ok=%v entries=%+v", ok, entries)
	}
	if entries[0].Slug != "legacy-project" || entries[0].N != 4 || entries[0].SessionID != "" || entries[0].Priority != "" {
		t.Errorf("synthesized entry = %+v, want Slug=legacy-project N=4 SessionID=\"\" Priority=\"\" (normal)", entries[0])
	}
}

// TestReadSessionRegistry_SessionDirSupersedesLegacyFile proves no double-
// counting when a slug has BOTH a stale legacy file and a live session dir
// (e.g. mid-upgrade) — only the session-dir entries count.
func TestReadSessionRegistry_SessionDirSupersedesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteWorkstreamEntry(dir, "proj", 99, wsBaseNow); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionEntry(dir, "proj", "sess1", 2, "normal", wsBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadSessionRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok || len(entries) != 1 || entries[0].N != 2 {
		t.Fatalf("want exactly the session-dir entry (N=2), never the stale legacy N=99 too — got ok=%v entries=%+v", ok, entries)
	}
}

// TestReadSessionRegistry_TTL_StalePrunedBeforePriorityMath: a stale session
// entry is dropped before it ever reaches priority/preemption math.
func TestReadSessionRegistry_TTL_StalePrunedBeforePriorityMath(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSessionEntry(dir, "proj", "stale-sess", 5, "critical", wsBaseNow.Add(-45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionEntry(dir, "proj", "fresh-sess", 2, "low", wsBaseNow.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadSessionRegistry(dir, wsBaseNow.Add(-30*time.Minute))
	if !ok || len(entries) != 1 || entries[0].SessionID != "fresh-sess" {
		t.Fatalf("want only the fresh session kept, got ok=%v entries=%+v", ok, entries)
	}
}

func TestReadSessionRegistry_MissingDir_OkTrueEmpty(t *testing.T) {
	entries, ok := ReadSessionRegistry(filepath.Join(t.TempDir(), "does-not-exist"), wsBaseNow)
	if !ok || len(entries) != 0 {
		t.Errorf("want ok=true, no entries; got ok=%v entries=%+v", ok, entries)
	}
}
