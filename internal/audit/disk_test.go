package audit

import (
	"os"
	"path/filepath"
	"testing"
)

var canonicalStatusesFixture = []string{
	"pending", "in_progress", "in_review", "blocked",
	"needs_rework", "gated", "done", "dropped", "superseded",
}

func writeTask(t *testing.T, storeRoot, filename, status string) {
	t.Helper()
	content := "---\ntitle: t\nstatus: " + status + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(storeRoot, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkOverlay creates queueRoot/agent/status/base as a relative symlink into
// storeRoot/base — that target need not exist (dangling-symlink fixtures rely on
// linking, then removing, the store file).
func linkOverlay(t *testing.T, queueRoot, storeRoot, agent, status, base string) {
	t.Helper()
	dir := filepath.Join(queueRoot, agent, status)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(storeRoot, base))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(dir, base)); err != nil {
		t.Fatal(err)
	}
}

func TestDiskLens(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// orphaned-active: active status, no overlay placement anywhere.
	writeTask(t, storeRoot, "1-orphan.md", "pending")

	// placed, dir matches frontmatter — not flagged by anything.
	writeTask(t, storeRoot, "2-placed.md", "pending")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "2-placed.md")

	// dangling: symlink survives, store file is gone.
	writeTask(t, storeRoot, "3-wasplaced.md", "done")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "3-wasplaced.md")
	if err := os.Remove(filepath.Join(storeRoot, "3-wasplaced.md")); err != nil {
		t.Fatal(err)
	}

	// a dangling link under a "_"-prefixed lane — must NOT be flagged.
	writeTask(t, storeRoot, "4-gvtest.md", "done")
	linkOverlay(t, queueRoot, storeRoot, "_gvtest-cr", "pending", "4-gvtest.md")
	if err := os.Remove(filepath.Join(storeRoot, "4-gvtest.md")); err != nil {
		t.Fatal(err)
	}

	// status-drift: filed under pending/ but frontmatter says gated. Still counts as
	// "placed" (so it must NOT also show up as orphaned-active).
	writeTask(t, storeRoot, "5-drift.md", "gated")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "5-drift.md")

	// duplicate-id: two files sharing the leading id.
	writeTask(t, storeRoot, "6-first.md", "done")
	writeTask(t, storeRoot, "6-second-epic.md", "done")

	// noncanonical / malformed status.
	writeTask(t, storeRoot, "7-noncanonical.md", "LOCKED (2026-06-24, co-designed with user)")

	findings, err := DiskLens(queueRoot, storeRoot, canonicalStatusesFixture)
	if err != nil {
		t.Fatalf("DiskLens: %v", err)
	}

	byCategory := map[string][]Finding{}
	for _, f := range findings {
		byCategory[f.Category] = append(byCategory[f.Category], f)
	}

	assertExactIDs := func(cat string, wantIDs ...string) {
		t.Helper()
		got := map[string]bool{}
		for _, f := range byCategory[cat] {
			got[f.ID] = true
		}
		for _, id := range wantIDs {
			if !got[id] {
				t.Errorf("%s: missing finding for id %q (got %+v)", cat, id, byCategory[cat])
			}
		}
		if len(got) != len(wantIDs) {
			t.Errorf("%s: got %d findings %+v, want exactly ids %v", cat, len(got), byCategory[cat], wantIDs)
		}
	}

	assertExactIDs(CategoryOrphanedActive, "1")
	assertExactIDs(CategoryDanglingSymlink, "3-wasplaced.md")
	assertExactIDs(CategoryStatusDrift, "5")
	assertExactIDs(CategoryDuplicateID, "6")
	assertExactIDs(CategoryNoncanonicalStatus, "7")

	// Severity spot-checks: duplicate-id is explicitly "info" (may be a legitimate
	// multi-task epic), everything else here is "warn".
	for _, f := range byCategory[CategoryDuplicateID] {
		if f.Severity != SeverityInfo {
			t.Errorf("duplicate-id severity = %q, want %q", f.Severity, SeverityInfo)
		}
	}
	for _, f := range byCategory[CategoryOrphanedActive] {
		if f.Severity != SeverityWarn {
			t.Errorf("orphaned-active severity = %q, want %q", f.Severity, SeverityWarn)
		}
	}

	// Ref fields: the dash links a finding to a browsable file wherever exactly one
	// file is named. orphaned-active/noncanonical-status/status-drift point at the
	// store (the canonical copy always exists); dangling-symlink points at the
	// overlay entry itself (the viewer shows a dangling link as "unreadable" rather
	// than 404ing). duplicate-id names multiple files, so it stays unlinked.
	for _, f := range byCategory[CategoryOrphanedActive] {
		if f.RefSrc != "store" || f.RefPath != "1-orphan.md" {
			t.Errorf("orphaned-active ref = src=%q path=%q, want store/1-orphan.md", f.RefSrc, f.RefPath)
		}
	}
	for _, f := range byCategory[CategoryDanglingSymlink] {
		if f.RefSrc != "" || f.RefPath == "" {
			t.Errorf("dangling-symlink ref = src=%q path=%q, want an overlay-relative path", f.RefSrc, f.RefPath)
		}
	}
	for _, f := range byCategory[CategoryStatusDrift] {
		if f.RefSrc != "store" || f.RefPath != "5-drift.md" {
			t.Errorf("status-drift ref = src=%q path=%q, want store/5-drift.md", f.RefSrc, f.RefPath)
		}
	}
	for _, f := range byCategory[CategoryDuplicateID] {
		if f.RefPath != "" {
			t.Errorf("duplicate-id ref should stay empty (names multiple files), got %q", f.RefPath)
		}
	}
}

func TestDiskLens_StatusSeparatorNormalization(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Dir name "in-progress" (hyphen) vs frontmatter "in_progress" (underscore) must
	// compare EQUAL — no drift finding.
	writeTask(t, storeRoot, "1-x.md", "in_progress")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "in-progress", "1-x.md")

	findings, err := DiskLens(queueRoot, storeRoot, canonicalStatusesFixture)
	if err != nil {
		t.Fatalf("DiskLens: %v", err)
	}
	for _, f := range findings {
		if f.Category == CategoryStatusDrift {
			t.Errorf("want no drift finding for in-progress/in_progress (separator-only difference), got %+v", f)
		}
	}
}
