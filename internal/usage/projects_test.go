package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var projBaseNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func TestWriteProjectEntry_EmptySlugIsANoOp(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProjectEntry(dir, "", "some-project", "/q", "/s", "/i", projBaseNow); err != nil {
		t.Fatalf("WriteProjectEntry with empty slug should be a no-op, got err: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("want nothing written for an empty slug, found: %v", entries)
	}
}

func TestWriteThenReadProjectEntry_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProjectEntry(dir, "legacy-migration", "legacy-migration", "/proj/queue", "/proj/tasks", "/proj/issues", projBaseNow); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadProjectRegistry(dir)
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(entries) != 1 {
		t.Fatalf("got %+v", entries)
	}
	e := entries[0]
	if e.Slug != "legacy-migration" || e.Name != "legacy-migration" ||
		e.QueueRoot != "/proj/queue" || e.StoreRoot != "/proj/tasks" || e.IssuesRoot != "/proj/issues" ||
		!e.UpdatedAt.Equal(projBaseNow) {
		t.Fatalf("got %+v", e)
	}
}

func TestReadProjectRegistry_MissingDir_OkTrueEmptyRegistry(t *testing.T) {
	entries, ok := ReadProjectRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	if !ok {
		t.Error("a missing registry dir is the normal 'nothing registered yet' state — want ok=true (fail OPEN, never crash the board)")
	}
	if len(entries) != 0 {
		t.Errorf("want no entries, got %+v", entries)
	}
}

func TestReadProjectRegistry_UnreadableDir_OkFalse(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits don't block reads")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	_, ok := ReadProjectRegistry(locked)
	if ok {
		t.Error("an unreadable (permission-denied) registry dir must report ok=false")
	}
}

// TestReadProjectRegistry_MalformedFileSkipped mirrors the workstream/session
// registries' "one bad entry shouldn't blind the whole read" rule.
func TestReadProjectRegistry_MalformedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := WriteProjectEntry(dir, "good-project", "Good Project", "/q", "/s", "/i", projBaseNow); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad-project.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadProjectRegistry(dir)
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(entries) != 1 || entries[0].Slug != "good-project" {
		t.Fatalf("want only the well-formed entry, got %+v", entries)
	}
}

// TestReadProjectRegistry_DoesNotDropStaleEntries: unlike ReadWorkstreamRegistry
// (which prunes stale entries so they can't hold phantom account headroom),
// ReadProjectRegistry returns EVERY entry regardless of age — the board is the
// one that decides to grey a stale project, never the registry read itself
// (multi-project-board.md: "Stale entries ... shown greyed / omitted, not
// errored").
func TestReadProjectRegistry_DoesNotDropStaleEntries(t *testing.T) {
	dir := t.TempDir()
	veryOld := projBaseNow.Add(-30 * 24 * time.Hour)
	if err := WriteProjectEntry(dir, "ancient-project", "Ancient Project", "/q", "/s", "/i", veryOld); err != nil {
		t.Fatal(err)
	}
	entries, ok := ReadProjectRegistry(dir)
	if !ok || len(entries) != 1 {
		t.Fatalf("want the stale entry still returned, got ok=%v entries=%+v", ok, entries)
	}
}

func TestWriteThenReadProjectEntry_SortedBySlug(t *testing.T) {
	dir := t.TempDir()
	for _, slug := range []string{"zeta", "alpha", "mu"} {
		if err := WriteProjectEntry(dir, slug, slug, "/q/"+slug, "", "", projBaseNow); err != nil {
			t.Fatal(err)
		}
	}
	entries, ok := ReadProjectRegistry(dir)
	if !ok || len(entries) != 3 {
		t.Fatalf("got ok=%v entries=%+v", ok, entries)
	}
	want := []string{"alpha", "mu", "zeta"}
	for i, w := range want {
		if entries[i].Slug != w {
			t.Errorf("entries[%d].Slug = %q, want %q (want registry sorted by slug)", i, entries[i].Slug, w)
		}
	}
}
