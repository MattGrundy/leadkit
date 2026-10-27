package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoreTask writes a real flat-store file <store>/<name> with a frontmatter status,
// returning its path. Mirrors a real store task (leading --- block + body).
func writeStoreTask(t *testing.T, store, name, status string) string {
	t.Helper()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(store, name)
	content := "---\nid: " + strings.TrimSuffix(name, ".md") + "\nstatus: " + status + "\n---\nbody\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// linkInLane creates an overlay symlink <root>/<lane>/<bucket>/<name> with a RELATIVE
// target pointing at <store>/<name> (exactly the shape a real overlay link has), so the
// match logic is exercised via resolution, not textual comparison. Returns the link path.
func linkInLane(t *testing.T, root, store, name, lane, bucket string) string {
	t.Helper()
	linkPath := filepath.Join(root, lane, bucket, name)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(linkPath), filepath.Join(store, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, linkPath); err != nil {
		t.Fatal(err)
	}
	return linkPath
}

// TestRemoveTask_HappyPathRemovesFileAndLinks: a terminal task (done) with 2 overlay
// links → dry-run lists the store file + 2 links + status; --apply removes all 3; a
// DIFFERENT task is never touched.
func TestRemoveTask_HappyPathRemovesFileAndLinks(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	storeFile := writeStoreTask(t, store, "gate-x.md", "done")
	l1 := linkInLane(t, root, store, "gate-x.md", "laneA", "done")
	l2 := linkInLane(t, root, store, "gate-x.md", "laneB", "done")

	// A different task with its own link — must survive untouched.
	otherFile := writeStoreTask(t, store, "other.md", "done")
	otherLink := linkInLane(t, root, store, "other.md", "laneA", "done")

	// Dry-run (task given WITHOUT .md — must resolve to gate-x.md).
	res, err := RemoveTask(root, store, "gate-x", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !res.DryRun || res.Applied {
		t.Errorf("dry-run flags wrong: DryRun=%v Applied=%v", res.DryRun, res.Applied)
	}
	if res.Status != "done" || !res.Terminal {
		t.Errorf("status/terminal wrong: %q terminal=%v", res.Status, res.Terminal)
	}
	if len(res.Links) != 2 {
		t.Fatalf("dry-run found %d links, want 2: %+v", len(res.Links), res.Links)
	}
	// Nothing removed by the dry-run.
	for _, p := range []string{storeFile, l1, l2} {
		if !exists(t, p) {
			t.Errorf("dry-run removed %s", p)
		}
	}

	// Apply.
	res, err = RemoveTask(root, store, "gate-x", false, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.RemovedStore {
		t.Errorf("apply did not remove the store file (StoreErr=%q)", res.StoreErr)
	}
	if len(res.RemovedLinks) != 2 {
		t.Errorf("apply removed %d links, want 2 (errors: %v)", len(res.RemovedLinks), res.LinkErrors)
	}
	for _, p := range []string{storeFile, l1, l2} {
		if exists(t, p) {
			t.Errorf("apply left %s on disk", p)
		}
	}
	// The other task and its link are untouched.
	if !exists(t, otherFile) || !exists(t, otherLink) {
		t.Errorf("a different task was touched: other.md=%v otherLink=%v", exists(t, otherFile), exists(t, otherLink))
	}
}

// TestRemoveTask_NonTerminalGuard: a needs_rework task → --apply WITHOUT
// --allow-non-terminal REFUSES (nothing removed); WITH the flag removes it. Dry-run
// shows the non-terminal status either way.
func TestRemoveTask_NonTerminalGuard(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	storeFile := writeStoreTask(t, store, "live.md", "needs_rework")
	link := linkInLane(t, root, store, "live.md", "laneA", "pending")

	// Dry-run shows the guard status (non-terminal) and mutates nothing.
	res, err := RemoveTask(root, store, "live.md", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Terminal {
		t.Error("needs_rework must be non-terminal")
	}
	if !strings.Contains(FormatRemove(res), "NON-TERMINAL") {
		t.Error("dry-run report should flag the non-terminal guard")
	}

	// Apply WITHOUT the flag → guarded, nothing removed.
	res, err = RemoveTask(root, store, "live.md", false, true)
	if err != nil {
		t.Fatalf("apply (guarded): %v", err)
	}
	if !res.Guarded || res.Applied || res.RemovedStore || len(res.RemovedLinks) != 0 {
		t.Errorf("guard did not block: Guarded=%v Applied=%v RemovedStore=%v RemovedLinks=%d",
			res.Guarded, res.Applied, res.RemovedStore, len(res.RemovedLinks))
	}
	if !exists(t, storeFile) || !exists(t, link) {
		t.Error("guarded apply removed something")
	}

	// Apply WITH the flag → removed.
	res, err = RemoveTask(root, store, "live.md", true, true)
	if err != nil {
		t.Fatalf("apply (allowed): %v", err)
	}
	if res.Guarded {
		t.Error("Guarded set despite --allow-non-terminal")
	}
	if !res.RemovedStore || len(res.RemovedLinks) != 1 {
		t.Errorf("allowed apply removed wrong: RemovedStore=%v RemovedLinks=%d", res.RemovedStore, len(res.RemovedLinks))
	}
	if exists(t, storeFile) || exists(t, link) {
		t.Error("allowed apply left the task on disk")
	}
}

// TestRemoveTask_DeleteOrder: the overlay symlinks are removed BEFORE the store file. We
// inject a failure on the store-file remove (make its parent dir read-only) and confirm
// the links were already gone while the store file remains — the exact "no window where a
// live link points at a deleted store file" invariant.
func TestRemoveTask_DeleteOrder(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	storeFile := writeStoreTask(t, store, "order.md", "done")
	l1 := linkInLane(t, root, store, "order.md", "laneA", "done")
	l2 := linkInLane(t, root, store, "order.md", "laneB", "done")

	// Read-only store dir → unlink of the store file fails; the links (in the lane dirs,
	// which stay writable) are removed first regardless.
	if err := os.Chmod(store, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store, 0o755) })

	res, err := RemoveTask(root, store, "order.md", false, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.RemovedStore {
		t.Error("store file was reported removed despite the injected failure")
	}
	if res.StoreErr == "" {
		t.Error("store-remove failure was not reported")
	}
	if len(res.RemovedLinks) != 2 {
		t.Errorf("links not all removed before the store file: RemovedLinks=%d", len(res.RemovedLinks))
	}
	// Links gone, store file still present.
	if exists(t, l1) || exists(t, l2) {
		t.Error("overlay links were NOT removed before the store-file attempt")
	}
	if !exists(t, storeFile) {
		t.Error("store file unexpectedly removed")
	}
}

// TestRemoveTask_ResolutionRefused: a missing --task and an ambiguous --task each refuse
// and remove nothing.
func TestRemoveTask_ResolutionRefused(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")

	// Missing.
	if _, err := RemoveTask(root, store, "ghost", false, true); err == nil {
		t.Error("missing task did not refuse")
	}

	// Ambiguous: both `amb` and `amb.md` exist as regular files.
	ambBare := writeStoreTask(t, store, "amb", "done")
	ambMd := writeStoreTask(t, store, "amb.md", "done")
	if _, err := RemoveTask(root, store, "amb", false, true); err == nil {
		t.Error("ambiguous task did not refuse")
	}
	if !exists(t, ambBare) || !exists(t, ambMd) {
		t.Error("a refused ambiguous resolution removed something")
	}
}

// TestRemoveTask_MultiLaneAndIsolation: a task with links in multiple lanes has ALL its
// links found + removed; a DIFFERENT task with a similar name is never touched; and a
// real (non-symlink) file elsewhere named like the task is not removed.
func TestRemoveTask_MultiLaneAndIsolation(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	fooFile := writeStoreTask(t, store, "foo.md", "done")
	fooA := linkInLane(t, root, store, "foo.md", "laneA", "done")
	fooB := linkInLane(t, root, store, "foo.md", "laneB", "in-progress")
	fooC := linkInLane(t, root, store, "foo.md", "laneC", "pending")

	// A similarly-named different task — must survive.
	barFile := writeStoreTask(t, store, "foo-bar.md", "done")
	barLink := linkInLane(t, root, store, "foo-bar.md", "laneA", "done")

	// A real (non-symlink) file in a lane named exactly like the task — must survive.
	plainDir := filepath.Join(root, "laneZ", "done")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainFoo := filepath.Join(plainDir, "foo.md")
	if err := os.WriteFile(plainFoo, []byte("not a symlink"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RemoveTask(root, store, "foo.md", false, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.RemovedLinks) != 3 {
		t.Errorf("multi-lane: removed %d links, want 3: %+v (errors %v)", len(res.RemovedLinks), res.RemovedLinks, res.LinkErrors)
	}
	if !res.RemovedStore {
		t.Error("store file not removed")
	}
	for _, p := range []string{fooFile, fooA, fooB, fooC} {
		if exists(t, p) {
			t.Errorf("left %s on disk", p)
		}
	}
	// Isolation: the different task, its link, and the plain file all survive.
	if !exists(t, barFile) || !exists(t, barLink) {
		t.Error("a similarly-named task was touched")
	}
	if !exists(t, plainFoo) {
		t.Error("a real (non-symlink) file named like the task was removed")
	}
}

// linkNested creates an overlay symlink at an ARBITRARY (possibly deep) queue-relative
// path with a RELATIVE target pointing at <store>/<name>. Returns the link path.
func linkNested(t *testing.T, root, store, name, relDir string) string {
	t.Helper()
	dir := filepath.Join(root, relDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, filepath.Join(store, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, linkPath); err != nil {
		t.Fatal(err)
	}
	return linkPath
}

// TestRemoveTask_RelativeQueueDefaultStore_FindsAllLinks is the #123 case: a RELATIVE
// --queue root walked from cwd, with an ABSOLUTE store file (the shape a defaulted
// --store takes via resolveStoreRoot→filepath.Abs), and a 4-deep nested overlay link.
// Under the old string-EvalSymlinks compare this found ZERO links — EvalSymlinks on the
// relative walked link stays relative ("tasks/x.md") while EvalSymlinks on the absolute
// store canonicalizes (/private/var/...), so the strings differ for the SAME file. With
// os.SameFile (device+inode) every link is FOUND regardless of path representation. This
// test FAILS before the fix and PASSES after (verified against pre-change code).
func TestRemoveTask_RelativeQueueDefaultStore_FindsAllLinks(t *testing.T) {
	base := t.TempDir()
	// Store is ABSOLUTE (as a defaulted --store is, via filepath.Abs). On macOS base is
	// itself under /var → /private/var, so the /var↔/private/var split is live here.
	store := filepath.Join(base, "tasks")
	storeFile := writeStoreTask(t, store, "gate-_gvtest.md", "done")

	// Overlay links live under a queue root that we will address RELATIVELY from cwd,
	// including one nested 4 levels deep (the full nested-location shape).
	queueAbs := filepath.Join(base, "queue")
	l1 := linkNested(t, queueAbs, store, "gate-_gvtest.md", filepath.Join("laneA", "active"))
	l2 := linkNested(t, queueAbs, store, "gate-_gvtest.md", filepath.Join("epic", "milestone", "laneB", "gated"))

	// Walk the queue via a RELATIVE root from cwd — the exact condition that broke the old
	// string compare (relative walk root + absolute store).
	t.Chdir(base)
	relQueue := "queue"

	res, err := RemoveTask(relQueue, store, "gate-_gvtest", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(res.Links) != 2 {
		t.Fatalf("relative-queue+abs-store found %d links, want 2 (the #123 silent-zero-links bug): %+v", len(res.Links), res.Links)
	}

	// Apply removes links THEN the store file — none of the live links outlives its target.
	res, err = RemoveTask(relQueue, store, "gate-_gvtest", false, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.RemovedLinks) != 2 {
		t.Errorf("apply removed %d links, want 2 (errors: %v)", len(res.RemovedLinks), res.LinkErrors)
	}
	if !res.RemovedStore {
		t.Errorf("store file not removed (StoreErr=%q)", res.StoreErr)
	}
	for _, p := range []string{storeFile, l1, l2} {
		if exists(t, p) {
			t.Errorf("apply left %s on disk", p)
		}
	}
}

// TestRemoveTask_DifferentFileNotMatched: a symlink that resolves to a DIFFERENT store
// file is never collected — os.SameFile is false for a different inode, so isolation
// holds even when the two files share a directory / similar name.
func TestRemoveTask_DifferentFileNotMatched(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	writeStoreTask(t, store, "target.md", "done")
	otherFile := writeStoreTask(t, store, "other.md", "done")

	// A link to the target and a link to a DIFFERENT file, both under the queue.
	tgtLink := linkInLane(t, root, store, "target.md", "laneA", "done")
	otherLink := linkInLane(t, root, store, "other.md", "laneA", "done")

	res, err := RemoveTask(root, store, "target.md", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(res.Links) != 1 {
		t.Fatalf("found %d links, want exactly 1 (only the target's): %+v", len(res.Links), res.Links)
	}
	if res.Links[0].Path != tgtLink {
		t.Errorf("collected the wrong link: got %s, want %s", res.Links[0].Path, tgtLink)
	}
	if !exists(t, otherFile) || !exists(t, otherLink) {
		t.Error("a different file's link must never be collected/removed")
	}
}

// TestRemoveTask_DanglingNotMatched: a dangling symlink (target gone) fails os.Stat and
// is never collected — it cannot be a live link to the existing store file.
func TestRemoveTask_DanglingNotMatched(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	writeStoreTask(t, store, "live.md", "done")
	goodLink := linkInLane(t, root, store, "live.md", "laneA", "done")

	// A dangling link: point at a store file, then delete that file.
	writeStoreTask(t, store, "ghost.md", "done")
	danglingLink := linkInLane(t, root, store, "ghost.md", "laneB", "done")
	if err := os.Remove(filepath.Join(store, "ghost.md")); err != nil {
		t.Fatal(err)
	}

	res, err := RemoveTask(root, store, "live.md", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(res.Links) != 1 || res.Links[0].Path != goodLink {
		t.Fatalf("dangling link must be skipped; got %d links: %+v", len(res.Links), res.Links)
	}
	if !exists(t, danglingLink) {
		t.Error("the dangling link should be untouched by a dry-run")
	}
}

// TestRemoveTask_DryRunMutatesNothing: a full plan is reported but nothing on disk changes.
func TestRemoveTask_DryRunMutatesNothing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	storeFile := writeStoreTask(t, store, "dry.md", "done")
	l1 := linkInLane(t, root, store, "dry.md", "laneA", "done")
	l2 := linkInLane(t, root, store, "dry.md", "laneB", "done")

	res, err := RemoveTask(root, store, "dry.md", false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Applied || res.RemovedStore || len(res.RemovedLinks) != 0 {
		t.Errorf("dry-run mutated: Applied=%v RemovedStore=%v RemovedLinks=%d", res.Applied, res.RemovedStore, len(res.RemovedLinks))
	}
	if len(res.Links) != 2 {
		t.Errorf("dry-run should plan 2 links, got %d", len(res.Links))
	}
	for _, p := range []string{storeFile, l1, l2} {
		if !exists(t, p) {
			t.Errorf("dry-run removed %s", p)
		}
	}
}
