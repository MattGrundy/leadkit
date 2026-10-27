package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDanglingLink creates a live symlink then removes its target, leaving a dangling
// link at linkPath.
func setupDanglingLink(t *testing.T, store, linkPath, name string) {
	t.Helper()
	target := setupSymlinkEntry(t, store, linkPath, name)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
}

// repairFixture builds a queue with one LIVE ticket (cr-1) and two DANGLING links
// (cr-1's second entry + gate-1's entry), returning the root.
func repairFixture(t *testing.T) (root, store string) {
	t.Helper()
	root = t.TempDir()
	store = filepath.Join(root, "tasks")
	// live ticket — must survive a reap
	setupSymlinkEntry(t, store, filepath.Join(root, "cr-1", "pending", "live.md"), "live.md")
	// dangling in the same lane
	setupDanglingLink(t, store, filepath.Join(root, "cr-1", "pending", "dead1.md"), "dead1.md")
	// dangling in another lane, in-progress this time
	setupDanglingLink(t, store, filepath.Join(root, "gate-1", "in-progress", "dead2.md"), "dead2.md")
	return root, store
}

func TestFindDangling_OnlyDanglingLinks(t *testing.T) {
	root, _ := repairFixture(t)
	// A plain (non-symlink) file must never be a target.
	if err := os.WriteFile(filepath.Join(root, "cr-1", "pending", "plain.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dangling, err := FindDangling(root)
	if err != nil {
		t.Fatalf("FindDangling: %v", err)
	}
	if len(dangling) != 2 {
		t.Fatalf("found %d dangling, want 2: %+v", len(dangling), dangling)
	}
	names := map[string]bool{}
	for _, d := range dangling {
		names[d.Name] = true
	}
	if !names["dead1.md"] || !names["dead2.md"] {
		t.Errorf("wrong dangling set: %+v", dangling)
	}
	if names["live.md"] || names["plain.md"] {
		t.Errorf("a live ticket or plain file was flagged dangling: %+v", dangling)
	}
}

func TestRepair_DryRunListsButMutatesNothing(t *testing.T) {
	root, store := repairFixture(t)

	res, err := Repair(root, store, false, false)
	if err != nil {
		t.Fatalf("Repair dry-run: %v", err)
	}
	if !res.DryRun {
		t.Error("DryRun = false for a dry-run")
	}
	if len(res.Dangling) != 2 {
		t.Errorf("dry-run should list 2 dangling, got %d", len(res.Dangling))
	}
	if len(res.Reaped) != 0 {
		t.Errorf("dry-run must reap nothing, reaped %d", len(res.Reaped))
	}
	// Both dangling links still present on disk (Lstat succeeds).
	for _, rel := range []string{"cr-1/pending/dead1.md", "gate-1/in-progress/dead2.md"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("dry-run removed %s: %v", rel, err)
		}
	}
}

func TestRepair_ApplyReapsOnlyDanglingAndKeepsLive(t *testing.T) {
	root, store := repairFixture(t)

	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.Reaped) != 2 {
		t.Errorf("apply should reap 2, reaped %d", len(res.Reaped))
	}
	// Dangling links gone.
	for _, rel := range []string{"cr-1/pending/dead1.md", "gate-1/in-progress/dead2.md"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("apply left dangling %s (err=%v)", rel, err)
		}
	}
	// The LIVE ticket's link survives.
	if _, err := os.Lstat(filepath.Join(root, "cr-1", "pending", "live.md")); err != nil {
		t.Errorf("apply reaped a LIVE ticket's link: %v", err)
	}
}

func TestFormatRepair_LogsReap(t *testing.T) {
	root, store := repairFixture(t)
	res, _ := Repair(root, store, true, false)
	out := FormatRepair(res)
	if !strings.Contains(out, "reaped") {
		t.Errorf("FormatRepair should log the reap: %q", out)
	}
	if !strings.Contains(out, "reaped 2") {
		t.Errorf("FormatRepair missing summary: %q", out)
	}
}

// --- --prune-empty-lanes: empty-lane + stray-dir prune ---

// makeLane creates a lane dir with its three empty status buckets under root.
func makeLane(t *testing.T, root, lane string) string {
	t.Helper()
	dir := filepath.Join(root, lane)
	for _, b := range []string{"pending", "in-progress", "done"} {
		if err := os.MkdirAll(filepath.Join(dir, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeRoles writes a <root>/ROLES file with the given "role live" rows.
func writeRoles(t *testing.T, root string, rows ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "ROLES"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// TestPrune_EmptyUnregisteredLanePruned: an empty lane not in ROLES is listed in
// dry-run and removed with apply.
func TestPrune_EmptyUnregisteredLanePruned(t *testing.T) {
	root := t.TempDir()
	dir := makeLane(t, root, "_gvtest-crit")

	// dry-run: listed, mutates nothing.
	res, err := Repair(root, filepath.Join(root, "tasks"), false, true)
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	if len(res.EmptyLanes) != 1 || res.EmptyLanes[0].Lane != "_gvtest-crit" || res.EmptyLanes[0].Registered {
		t.Fatalf("dry-run should list one unregistered empty lane, got %+v", res.EmptyLanes)
	}
	if len(res.PrunedLanes) != 0 {
		t.Errorf("dry-run must prune nothing, pruned %+v", res.PrunedLanes)
	}
	if !exists(t, dir) {
		t.Error("dry-run removed the lane dir")
	}

	// apply: removed.
	res, err = Repair(root, filepath.Join(root, "tasks"), true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.PrunedLanes) != 1 || res.PrunedLanes[0].Lane != "_gvtest-crit" {
		t.Fatalf("apply should prune the empty lane, got %+v", res.PrunedLanes)
	}
	if exists(t, dir) {
		t.Error("apply left the empty unregistered lane on disk")
	}
}

// TestPrune_RegisteredLaneSkipped: an empty lane present in ROLES is never removed,
// even with apply — reported skipped-registered.
func TestPrune_RegisteredLaneSkipped(t *testing.T) {
	root := t.TempDir()
	dir := makeLane(t, root, "cr-1")
	writeRoles(t, root, "code-review cr-1")

	res, err := Repair(root, filepath.Join(root, "tasks"), true, true) // apply — still must not remove
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.EmptyLanes) != 1 || !res.EmptyLanes[0].Registered {
		t.Fatalf("cr-1 should be reported registered, got %+v", res.EmptyLanes)
	}
	if len(res.PrunedLanes) != 0 {
		t.Errorf("a registered lane must never be pruned, pruned %+v", res.PrunedLanes)
	}
	if !exists(t, dir) {
		t.Error("apply removed a registered (in-ROLES) empty lane")
	}
	if out := FormatRepair(res); !strings.Contains(out, "skipped cr-1") {
		t.Errorf("FormatRepair should report the registered skip: %q", out)
	}
}

// TestPrune_LiveLaneNeverCandidate: a lane with ≥1 live symlink is never a candidate.
func TestPrune_LiveLaneNeverCandidate(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	makeLane(t, root, "owner-1")
	setupSymlinkEntry(t, store, filepath.Join(root, "owner-1", "pending", "live.md"), "live.md")

	res, err := Repair(root, store, true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.EmptyLanes) != 0 {
		t.Errorf("a lane with a live ticket is not empty, got %+v", res.EmptyLanes)
	}
	if !exists(t, filepath.Join(root, "owner-1")) {
		t.Error("apply removed a live lane")
	}
}

// TestPrune_DoneOnlyLaneNotPruned: a lane with only done/ entries has entries in a
// bucket → NOT all-empty → not pruned by this pass (archive job's domain).
func TestPrune_DoneOnlyLaneNotPruned(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	makeLane(t, root, "owner-2")
	setupSymlinkEntry(t, store, filepath.Join(root, "owner-2", "done", "shipped.md"), "shipped.md")

	res, err := Repair(root, store, true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.EmptyLanes) != 0 || len(res.PrunedLanes) != 0 {
		t.Errorf("a done-only lane must not be pruned, got empties=%+v pruned=%+v", res.EmptyLanes, res.PrunedLanes)
	}
	if !exists(t, filepath.Join(root, "owner-2")) {
		t.Error("apply removed a lane that still has done/ entries")
	}
}

// TestPrune_StrayEmptyRemoved: an empty double-nested stray dir (queue/queue/…) is
// detected and removed with apply.
func TestPrune_StrayEmptyRemoved(t *testing.T) {
	root := t.TempDir()
	// a stray double-nested root: <root>/queue/_gvtest-owner/pending (all empty dirs)
	stray := filepath.Join(root, "queue")
	if err := os.MkdirAll(filepath.Join(stray, "_gvtest-owner", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}

	// dry-run: reported, not removed.
	res, err := Repair(root, filepath.Join(root, "tasks"), false, true)
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	if len(res.StrayDirs) != 1 || res.StrayDirs[0].Lane != "queue" || !res.StrayDirs[0].Empty {
		t.Fatalf("dry-run should report one empty stray dir, got %+v", res.StrayDirs)
	}
	if len(res.PrunedStray) != 0 || !exists(t, stray) {
		t.Error("dry-run removed a stray dir")
	}

	// apply: removed.
	res, err = Repair(root, filepath.Join(root, "tasks"), true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.PrunedStray) != 1 {
		t.Fatalf("apply should remove the empty stray, got %+v", res.PrunedStray)
	}
	if exists(t, stray) {
		t.Error("apply left the empty stray dir on disk")
	}
}

// TestPrune_StrayWithFileReportedNotRemoved: a stray dir containing ANY file is
// reported but NEVER removed, even with apply.
func TestPrune_StrayWithFileReportedNotRemoved(t *testing.T) {
	root := t.TempDir()
	stray := filepath.Join(root, "queue")
	if err := os.MkdirAll(filepath.Join(stray, "_gvtest-owner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "_gvtest-owner", "leftover.md"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Repair(root, filepath.Join(root, "tasks"), true, true) // apply — must still not remove
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.StrayDirs) != 1 || res.StrayDirs[0].Empty {
		t.Fatalf("a file-holding stray must be reported non-empty, got %+v", res.StrayDirs)
	}
	if len(res.PrunedStray) != 0 {
		t.Errorf("a stray holding a file must never be removed, pruned %+v", res.PrunedStray)
	}
	if !exists(t, filepath.Join(stray, "_gvtest-owner", "leftover.md")) {
		t.Error("apply destroyed a file inside a stray dir")
	}
	if out := FormatRepair(res); !strings.Contains(out, "HOLDS files") {
		t.Errorf("FormatRepair should report the file-holding stray: %q", out)
	}
}

// TestPrune_DryRunMutatesNothing: with candidates of every kind present, a no-apply
// prune run leaves every dir on disk.
func TestPrune_DryRunMutatesNothing(t *testing.T) {
	root := t.TempDir()
	empty := makeLane(t, root, "_ephemeral")
	reg := makeLane(t, root, "cr-1")
	writeRoles(t, root, "code-review cr-1")
	stray := filepath.Join(root, "queue")
	if err := os.MkdirAll(filepath.Join(stray, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Repair(root, filepath.Join(root, "tasks"), false, true); err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	for _, p := range []string{empty, reg, stray, filepath.Join(stray, "nested")} {
		if !exists(t, p) {
			t.Errorf("dry-run removed %s", p)
		}
	}
}

// TestPrune_DanglingReapRegression: with --prune-empty-lanes on, the existing
// dangling-link reap still reaps exactly the dangling set and spares live tickets —
// the additive pass changes nothing about the reap.
func TestPrune_DanglingReapRegression(t *testing.T) {
	root, store := repairFixture(t) // cr-1 (live + dangling), gate-1 (dangling)

	res, err := Repair(root, store, true, true)
	if err != nil {
		t.Fatalf("repair+prune apply: %v", err)
	}
	if len(res.Reaped) != 2 {
		t.Errorf("reap should still remove 2 dangling links, reaped %d", len(res.Reaped))
	}
	for _, rel := range []string{"cr-1/pending/dead1.md", "gate-1/in-progress/dead2.md"} {
		if exists(t, filepath.Join(root, filepath.FromSlash(rel))) {
			t.Errorf("dangling %s survived the reap", rel)
		}
	}
	// The live ticket's link survives — and cr-1 is therefore NOT an empty-lane candidate.
	if !exists(t, filepath.Join(root, "cr-1", "pending", "live.md")) {
		t.Error("reap removed a LIVE ticket's link")
	}
	for _, l := range res.EmptyLanes {
		if l.Lane == "cr-1" {
			t.Errorf("cr-1 still holds a live ticket — must not be an empty-lane candidate: %+v", res.EmptyLanes)
		}
	}
}

// --- Fix 1: RECURSIVE dangling reaper (any depth) ---

// relSet collects the Rel() (full nested path) of every dangling link found.
func relSet(dls []DanglingLink) map[string]bool {
	m := map[string]bool{}
	for _, d := range dls {
		m[d.Rel()] = true
	}
	return m
}

// TestFindDangling_Recursive_NestedDepthReaped: a dangling link at the canonical 3 levels
// AND one nested 4 levels deep (the #123 queue/queue/… case) are BOTH found with their
// full nested paths, and BOTH reaped with --apply. The old fixed 3-level walk missed the
// nested one entirely ("0 dangling" while a broken link persisted).
func TestFindDangling_Recursive_NestedDepthReaped(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	// 3-level (today): <lane>/<status>/<entry>
	setupDanglingLink(t, store, filepath.Join(root, "cr-1", "pending", "shallow.md"), "shallow.md")
	// nested 4-level under a stray double-nested queue/: queue/<lane>/<status>/<entry>
	nested := filepath.Join(root, "queue", "_gvtest-owner", "pending", "gate-_gvtest.md")
	setupDanglingLink(t, store, nested, "gate-_gvtest.md")

	dangling, err := FindDangling(root)
	if err != nil {
		t.Fatalf("FindDangling: %v", err)
	}
	if len(dangling) != 2 {
		t.Fatalf("recursive walk should find 2 dangling (shallow + nested), got %d: %+v", len(dangling), dangling)
	}
	rels := relSet(dangling)
	if !rels["cr-1/pending/shallow.md"] {
		t.Errorf("missing the 3-level dangling link: %+v", dangling)
	}
	if !rels["queue/_gvtest-owner/pending/gate-_gvtest.md"] {
		t.Errorf("missing the NESTED 4-level dangling link (the #123 case): %+v", dangling)
	}

	// dry-run names the full nested path and mutates nothing.
	dry, err := Repair(root, store, false, false)
	if err != nil {
		t.Fatalf("Repair dry-run: %v", err)
	}
	if out := FormatRepair(dry); !strings.Contains(out, "queue/_gvtest-owner/pending/gate-_gvtest.md") {
		t.Errorf("dry-run must name the full nested path, got: %q", out)
	}
	if !exists(t, nested) {
		t.Error("dry-run removed the nested dangling link")
	}

	// apply reaps BOTH.
	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.Reaped) != 2 {
		t.Fatalf("apply should reap both dangling links, reaped %d", len(res.Reaped))
	}
	if exists(t, nested) || exists(t, filepath.Join(root, "cr-1", "pending", "shallow.md")) {
		t.Error("apply left a dangling link on disk")
	}
}

// TestFindDangling_Recursive_LiveAndPlainNeverReaped: a LIVE (resolving) symlink nested at
// any depth is never reaped, and a plain file is never a target.
func TestFindDangling_Recursive_LiveAndPlainNeverReaped(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	// a live symlink nested deep
	liveLink := filepath.Join(root, "a", "b", "c", "live.md")
	setupSymlinkEntry(t, store, liveLink, "live.md")
	// a plain file nested deep
	plain := filepath.Join(root, "a", "b", "c", "plain.md")
	if err := os.WriteFile(plain, []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}

	dangling, err := FindDangling(root)
	if err != nil {
		t.Fatalf("FindDangling: %v", err)
	}
	if len(dangling) != 0 {
		t.Fatalf("a nested live symlink / plain file must never be dangling, got %+v", dangling)
	}
	if _, err := Repair(root, store, true, false); err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if !exists(t, liveLink) {
		t.Error("apply reaped a nested LIVE symlink")
	}
	if !exists(t, plain) {
		t.Error("apply removed a nested plain file")
	}
}

// TestFindDangling_Recursive_SymlinkedDirNotDescended: a symlinked STATUS/alias dir
// (pointing OUTSIDE the queue root) is not descended — nothing reachable only through it
// is reaped or reported, and the walk never escapes the tree via the dir symlink.
func TestFindDangling_Recursive_SymlinkedDirNotDescended(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir() // an out-of-tree dir the alias points at
	// a dangling link that lives ONLY under the external dir (no in-root path to it)
	extStore := filepath.Join(external, "store")
	setupDanglingLink(t, extStore, filepath.Join(external, "pending", "dead-outside.md"), "dead-outside.md")

	// a symlinked status dir inside the queue → the external pending dir (a live dir symlink)
	if err := os.MkdirAll(filepath.Join(root, "cr-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "cr-1", "in-progress")
	if err := os.Symlink(filepath.Join(external, "pending"), aliasDir); err != nil {
		t.Fatal(err)
	}

	dangling, err := FindDangling(root)
	if err != nil {
		t.Fatalf("FindDangling: %v", err)
	}
	if len(dangling) != 0 {
		t.Fatalf("walk followed a dir symlink out of the tree (found %+v) — must NOT descend a symlinked dir", dangling)
	}
	// The alias dir symlink itself resolves (live) → never reaped.
	if _, err := Repair(root, filepath.Join(root, "tasks"), true, false); err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if !exists(t, aliasDir) {
		t.Error("apply reaped a LIVE (resolving) alias-inbox dir symlink")
	}
	// The external dangling link is untouched (never reached).
	if !exists(t, filepath.Join(external, "pending", "dead-outside.md")) {
		t.Error("walk escaped the tree and reaped an out-of-tree link")
	}
}

// --- Fix 2: redundant bucket-alias reap (opt-in with --prune-empty-lanes) ---

// symlink is a small helper that creates a symlink at linkPath → target (abs).
func symlink(t *testing.T, target, linkPath string) {
	t.Helper()
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatal(err)
	}
}

// TestPrune_BucketAliasReapedThenLanePruned: an unambiguous same-lane bucket alias
// (<lane>/in_progress → same-lane in-progress) in an UNREGISTERED lane is reported in
// dry-run and, under --apply, reaped BEFORE the empty-lane check — so the de-aliased,
// all-buckets-empty, unregistered lane prunes on the SAME pass.
func TestPrune_BucketAliasReapedThenLanePruned(t *testing.T) {
	root := t.TempDir()
	dir := makeLane(t, root, "ci-dx-agent")
	alias := filepath.Join(dir, "in_progress")
	symlink(t, filepath.Join(dir, "in-progress"), alias) // → same-lane canonical bucket

	// dry-run: reported, mutates nothing.
	res, err := Repair(root, filepath.Join(root, "tasks"), false, true)
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	if len(res.BucketAliases) != 1 || !res.BucketAliases[0].Reap || res.BucketAliases[0].Canon != "in-progress" {
		t.Fatalf("dry-run should report one reapable same-lane bucket alias, got %+v", res.BucketAliases)
	}
	if len(res.ReapedAliases) != 0 {
		t.Errorf("dry-run must reap nothing, reaped %+v", res.ReapedAliases)
	}
	if !exists(t, alias) || !exists(t, dir) {
		t.Error("dry-run mutated the alias or the lane")
	}

	// apply: alias reaped, then the emptied unregistered lane pruned — same pass.
	res, err = Repair(root, filepath.Join(root, "tasks"), true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.ReapedAliases) != 1 || res.ReapedAliases[0].Name != "in_progress" {
		t.Fatalf("apply should reap the same-lane bucket alias, got %+v", res.ReapedAliases)
	}
	if exists(t, alias) {
		t.Error("apply left the redundant bucket alias on disk")
	}
	pruned := false
	for _, l := range res.PrunedLanes {
		if l.Lane == "ci-dx-agent" {
			pruned = true
		}
	}
	if !pruned {
		t.Errorf("the de-aliased, empty, unregistered lane must prune on the same pass, got PrunedLanes=%+v", res.PrunedLanes)
	}
	if exists(t, dir) {
		t.Error("apply left the de-aliased empty lane on disk")
	}
}

// TestPrune_BucketAliasStrictness_LeftAlone: every alias that is NOT an unambiguous
// same-lane redundant bucket alias is LEFT ALONE (never removed), even with --apply:
//   - a same-name variant pointing to a DIFFERENT lane,
//   - a same-lane variant in a REGISTERED lane,
//   - a real alias-inbox symlink NAMED after a canonical bucket (pending → elsewhere),
//   - a non-bucket-variant symlink.
//
// A genuine same-lane alias in an unregistered lane (the control) IS reaped.
func TestPrune_BucketAliasStrictness_LeftAlone(t *testing.T) {
	root := t.TempDir()

	// keepFile stops the empty-lane prune from touching a lane so the test isolates the
	// alias-reap decision (a live entry in pending disqualifies the empty check).
	keep := func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "pending", "keep.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// (control) unregistered same-lane alias → reaped
	ctrl := makeLane(t, root, "ctrl")
	keep(ctrl)
	ctrlAlias := filepath.Join(ctrl, "in_progress")
	symlink(t, filepath.Join(ctrl, "in-progress"), ctrlAlias)

	// (a) different-lane target → left alone
	laneA := makeLane(t, root, "lane-a")
	laneB := makeLane(t, root, "lane-b")
	keep(laneA)
	keep(laneB)
	diffLane := filepath.Join(laneA, "in_progress")
	symlink(t, filepath.Join(laneB, "in-progress"), diffLane) // points at ANOTHER lane

	// (b) registered lane, correct same-lane target → left alone
	reg := makeLane(t, root, "reg-lane")
	keep(reg)
	regAlias := filepath.Join(reg, "in_progress")
	symlink(t, filepath.Join(reg, "in-progress"), regAlias)
	writeRoles(t, root, "owner reg-lane")

	// (c) real alias inbox: a symlinked bucket NAMED canonically (pending → elsewhere)
	inbox := filepath.Join(root, "inbox-lane")
	for _, b := range []string{"in-progress", "done"} {
		if err := os.MkdirAll(filepath.Join(inbox, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inboxPending := filepath.Join(inbox, "pending")
	symlink(t, filepath.Join(laneB, "pending"), inboxPending) // canonical NAME → not a variant

	// (d) non-bucket-variant symlink → left alone
	nonBucket := filepath.Join(ctrl, "foo_bar")
	symlink(t, filepath.Join(ctrl, "in-progress"), nonBucket)

	res, err := Repair(root, filepath.Join(root, "tasks"), true, true) // apply — strictness must hold under -apply
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}

	// Only the control was reaped.
	if len(res.ReapedAliases) != 1 || res.ReapedAliases[0].Lane != "ctrl" {
		t.Fatalf("only the unregistered same-lane alias may be reaped, got %+v", res.ReapedAliases)
	}
	if exists(t, ctrlAlias) {
		t.Error("control alias should have been reaped")
	}

	// Everything else survives on disk.
	for _, p := range []string{diffLane, regAlias, inboxPending, nonBucket} {
		if !exists(t, p) {
			t.Errorf("a non-reapable alias was removed: %s", p)
		}
	}

	// The different-lane and registered candidates are REPORTED as left-alone (Reap=false);
	// the real inbox (canonical name) and non-bucket variant are not alias candidates at all.
	reported := map[string]bool{}
	for _, a := range res.BucketAliases {
		if !a.Reap {
			reported[a.Lane+"/"+a.Name] = true
		}
	}
	if !reported["lane-a/in_progress"] {
		t.Error("the different-lane bucket-alias should be reported (left alone), not silently dropped")
	}
	if !reported["reg-lane/in_progress"] {
		t.Error("the registered-lane bucket-alias should be reported (left alone)")
	}
	for _, a := range res.BucketAliases {
		if a.Name == "pending" || a.Name == "foo_bar" {
			t.Errorf("a canonical-named inbox / non-bucket variant must not be an alias candidate: %+v", a)
		}
	}
}

// --- Fix 1+2: flat-store safety ---

// TestFindDangling_FlatStoreSafety_SiblingNeverReached: the real flat tasks/ store, a
// SIBLING of the queue root (not under it), is never reached by the reaper's walk.
func TestFindDangling_FlatStoreSafety_SiblingNeverReached(t *testing.T) {
	parent := t.TempDir()
	queueRoot := filepath.Join(parent, "queue")
	siblingStore := filepath.Join(parent, "tasks") // sibling of queueRoot, NOT under it
	// a live queue link into the sibling store
	setupSymlinkEntry(t, siblingStore, filepath.Join(queueRoot, "cr-1", "pending", "live.md"), "live.md")
	// an unreferenced real file in the sibling store
	orphan := filepath.Join(siblingStore, "orphan.md")
	if err := os.WriteFile(orphan, []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}

	dangling, err := FindDangling(queueRoot)
	if err != nil {
		t.Fatalf("FindDangling: %v", err)
	}
	if len(dangling) != 0 {
		t.Fatalf("the sibling store must never be walked, got %+v", dangling)
	}
	if _, err := Repair(queueRoot, siblingStore, true, true); err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if !exists(t, orphan) || !exists(t, filepath.Join(siblingStore, "live.md")) {
		t.Error("the reaper reached and mutated the sibling flat store")
	}
}

// TestPrune_NestedTasksStrayReapedAndPruned: a NESTED stray queue/tasks/ (an artifact,
// under the overlay root) holding a dangling link → the link is reaped by the recursive
// walk and the emptied stray dir becomes prune-eligible on the same --apply pass. The
// direct-child name-skip only protects a top-level tasks/, not a nested one.
func TestPrune_NestedTasksStrayReapedAndPruned(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	// stray double-nested: root/queue/tasks/dead.md (dangling)
	nested := filepath.Join(root, "queue", "tasks", "dead.md")
	setupDanglingLink(t, store, nested, "dead.md")

	res, err := Repair(root, store, true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.Reaped) != 1 || res.Reaped[0].Rel() != "queue/tasks/dead.md" {
		t.Fatalf("the nested dangling link should be reaped, got %+v", res.Reaped)
	}
	// The emptied stray queue/ dir prunes on the same pass.
	if exists(t, filepath.Join(root, "queue")) {
		t.Error("the emptied nested-stray dir should have been pruned")
	}
}

// TestPrune_NestedTasksWithRealFileReportedNotRemoved: a nested queue/tasks/ holding a
// REAL file is reported (file-holding stray) and NEVER removed — unchanged conservatism.
func TestPrune_NestedTasksWithRealFileReportedNotRemoved(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "queue", "tasks", "real.md")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Repair(root, filepath.Join(root, "tasks"), true, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.PrunedStray) != 0 {
		t.Errorf("a file-holding nested stray must never be removed, pruned %+v", res.PrunedStray)
	}
	if !exists(t, real) {
		t.Error("apply destroyed a real file inside a nested queue/tasks/ stray")
	}
}

// TestPrune_BucketAliasDryRunMutatesNothing: a dry-run with a reapable bucket alias AND
// prune candidates present removes NOTHING (both fixes) — reports the alias, keeps disk.
func TestPrune_BucketAliasDryRunMutatesNothing(t *testing.T) {
	root := t.TempDir()
	dir := makeLane(t, root, "ephemeral")
	alias := filepath.Join(dir, "in_progress")
	symlink(t, filepath.Join(dir, "in-progress"), alias)

	res, err := Repair(root, filepath.Join(root, "tasks"), false, true)
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	if len(res.BucketAliases) != 1 || !res.BucketAliases[0].Reap {
		t.Fatalf("dry-run should report the reapable alias, got %+v", res.BucketAliases)
	}
	if len(res.ReapedAliases) != 0 || len(res.PrunedLanes) != 0 {
		t.Errorf("dry-run must mutate nothing, reaped=%+v pruned=%+v", res.ReapedAliases, res.PrunedLanes)
	}
	if !exists(t, alias) || !exists(t, dir) {
		t.Error("dry-run mutated the alias or the lane")
	}
}

// --- Repair-first reaper (docs/build-tasks/reaper-repair-first.md) ---

// mispointedLiveLink creates a LIVE store file <store>/<name> and a BROKEN placement
// symlink at <root>/<lane>/<bucket>/<name> pointing at relTarget (a WRONG relative path
// that overshoots the store, so os.Stat fails) — i.e. the task is alive but the link is
// mispointed (the #123 _held/1457 doc-strip case). Returns the link path and store file.
func mispointedLiveLink(t *testing.T, root, store, lane, bucket, name, relTarget string) (linkPath, storeFile string) {
	t.Helper()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	storeFile = filepath.Join(store, name)
	if err := os.WriteFile(storeFile, []byte("live-deliverable"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, lane, bucket)
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath = filepath.Join(linkDir, name)
	if err := os.Symlink(relTarget, linkPath); err != nil {
		t.Fatal(err)
	}
	// It MUST be dangling as written (overshoots to a gone path).
	if _, err := os.Stat(linkPath); err == nil {
		t.Fatalf("fixture link %s must be dangling but it resolves", linkPath)
	}
	return linkPath, storeFile
}

// TestRepair_RepairableRepairedNotReaped: a mispointed-but-LIVE link (../../../tasks/1457.md
// overshooting from _held/pending, whose CORRECT target <store>/1457.md EXISTS) is REPAIRED
// (re-pointed), NEVER reaped — dry-run says REPAIR, -apply re-points it so it RESOLVES, the
// store file is untouched, and the placement is preserved. (The #123 doc-strip deliverable.)
func TestRepair_RepairableRepairedNotReaped(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	link, storeFile := mispointedLiveLink(t, root, store, "_held", "pending", "1457.md", filepath.FromSlash("../../../tasks/1457.md"))

	// dry-run: REPAIR, not REAP; mutates nothing.
	dry, err := Repair(root, store, false, false)
	if err != nil {
		t.Fatalf("Repair dry-run: %v", err)
	}
	if len(dry.Repairs) != 1 || len(dry.Orphans) != 0 || len(dry.Reaped) != 0 {
		t.Fatalf("dry-run should plan exactly one REPAIR and no reap, got repairs=%+v orphans=%+v reaped=%+v", dry.Repairs, dry.Orphans, dry.Reaped)
	}
	if out := FormatRepair(dry); !strings.Contains(out, "REPAIR ") || strings.Contains(out, "REAP _held") {
		t.Errorf("dry-run must report REPAIR (not REAP) for the live link: %q", out)
	}
	// Still dangling on disk, still pointing at the OLD target (nothing changed).
	if _, err := os.Stat(link); err == nil {
		t.Error("dry-run repaired the link (it now resolves) — must mutate nothing")
	}
	if tgt, _ := os.Readlink(link); tgt != filepath.FromSlash("../../../tasks/1457.md") {
		t.Errorf("dry-run changed the link target: %q", tgt)
	}

	// apply: re-pointed atomically; NEVER reaped.
	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.Repairs) != 1 || !res.Repairs[0].Applied || res.Repairs[0].Failed {
		t.Fatalf("apply should re-point the live link, got %+v", res.Repairs)
	}
	if len(res.Reaped) != 0 || len(res.Orphans) != 0 {
		t.Fatalf("a repairable link must NEVER be reaped, got reaped=%+v orphans=%+v", res.Reaped, res.Orphans)
	}
	// The link now RESOLVES (placement restored, never dropped).
	if _, err := os.Stat(link); err != nil {
		t.Errorf("after repair the link must resolve: %v", err)
	}
	if !exists(t, link) {
		t.Error("the placement (link) is missing after repair")
	}
	// The store file is untouched.
	b, err := os.ReadFile(storeFile)
	if err != nil || string(b) != "live-deliverable" {
		t.Errorf("the store file was mutated/removed: err=%v content=%q", err, string(b))
	}
}

// TestRepair_OrphanedReaped: a broken FILE link whose basename is ABSENT from the store is a
// true orphan → dry-run says REAP, -apply removes it (the gate-_gvtest.md case).
func TestRepair_OrphanedReaped(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	setupDanglingLink(t, store, filepath.Join(root, "_gvtest-owner", "pending", "gate-_gvtest.md"), "gate-_gvtest.md")

	dry, err := Repair(root, store, false, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(dry.Orphans) != 1 || len(dry.Repairs) != 0 {
		t.Fatalf("dry-run should plan one REAP and no repair, got orphans=%+v repairs=%+v", dry.Orphans, dry.Repairs)
	}
	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.Reaped) != 1 {
		t.Fatalf("apply should reap the orphan, reaped %+v", res.Reaped)
	}
	if exists(t, filepath.Join(root, "_gvtest-owner", "pending", "gate-_gvtest.md")) {
		t.Error("apply left the orphaned link on disk")
	}
}

// TestRepair_DirAliasNotReaped: a dangling DIRECTORY/inbox alias (cr-1-2/pending → cr-2/pending,
// target gone) is reported broken-dir-alias and NEVER removed, even with -apply.
func TestRepair_DirAliasNotReaped(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pool alias inbox: <lane>/pending is itself a symlink at the bucket level, pointing at
	// another lane's (now gone) pending — dangling, but a DIR alias, never a task placement.
	laneDir := filepath.Join(root, "cr-1-2")
	if err := os.MkdirAll(laneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasLink := filepath.Join(laneDir, "pending")
	if err := os.Symlink(filepath.FromSlash("../cr-2/pending"), aliasLink); err != nil {
		t.Fatal(err)
	}

	res, err := Repair(root, store, true, true) // -apply AND prune — must still not remove it
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.DirAliases) != 1 {
		t.Fatalf("the dangling dir alias must be reported broken-dir-alias, got %+v", res.DirAliases)
	}
	if len(res.Reaped) != 0 || len(res.Orphans) != 0 || len(res.Repairs) != 0 {
		t.Errorf("a dir alias must be neither reaped nor repaired, got reaped=%+v orphans=%+v repairs=%+v", res.Reaped, res.Orphans, res.Repairs)
	}
	if !exists(t, aliasLink) {
		t.Error("apply removed a broken DIR alias — must never auto-reap one")
	}
	if out := FormatRepair(res); !strings.Contains(out, "broken-dir-alias") {
		t.Errorf("FormatRepair should report the dir alias: %q", out)
	}
}

// TestRepair_AtomicRepointFailureLeavesOriginal: when the atomic re-point cannot complete
// (its temp name is squatted by a non-empty dir so the temp-create fails), the repair is
// reported failed and the ORIGINAL link is left EXACTLY as it was — never a missing
// placement. (Also asserts the happy-path re-point resolves at all times, above.)
func TestRepair_AtomicRepointFailureLeavesOriginal(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	link, _ := mispointedLiveLink(t, root, store, "_held", "pending", "1425.md", filepath.FromSlash("../../../tasks/1425.md"))

	// Squat the temp name (<link>.repair-tmp) with a NON-EMPTY directory: os.Remove refuses it
	// and os.Symlink(tmp) fails EEXIST → the re-point cannot even create its temp.
	tmp := link + ".repair-tmp"
	if err := os.MkdirAll(filepath.Join(tmp, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.Repairs) != 1 || !res.Repairs[0].Failed || res.Repairs[0].Applied {
		t.Fatalf("the re-point should be reported FAILED (not applied), got %+v", res.Repairs)
	}
	if len(res.Reaped) != 0 {
		t.Error("a failed repair must never fall through to a reap")
	}
	// The ORIGINAL link still exists and still points at the OLD (wrong) target — never dropped.
	if !exists(t, link) {
		t.Fatal("the ORIGINAL placement is missing after a failed repair")
	}
	if tgt, _ := os.Readlink(link); tgt != filepath.FromSlash("../../../tasks/1425.md") {
		t.Errorf("a failed repair changed the original link target: %q", tgt)
	}
	if out := FormatRepair(res); !strings.Contains(out, "repair-failed") {
		t.Errorf("FormatRepair should report the failed repair: %q", out)
	}
}

// TestRepair_StoreRootUnknownReapsNothing: with no resolvable/readable store root, the
// reaper reaps NOTHING and reports every broken FILE link UNVERIFIABLE (fail-safe).
func TestRepair_StoreRootUnknownReapsNothing(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	// One truly-orphaned link and one mispointed-but-live link — NEITHER may be touched
	// when the store root can't be read.
	setupDanglingLink(t, store, filepath.Join(root, "gate-1", "pending", "orphan.md"), "orphan.md")
	live, _ := mispointedLiveLink(t, root, store, "_held", "pending", "1331.md", filepath.FromSlash("../../../tasks/1331.md"))

	for _, sr := range []string{"", filepath.Join(root, "does-not-exist")} {
		res, err := Repair(root, sr, true, false) // -apply, but store unknown/unreadable
		if err != nil {
			t.Fatalf("Repair apply (store=%q): %v", sr, err)
		}
		if res.StoreReadable {
			t.Errorf("store=%q must be unreadable", sr)
		}
		if len(res.Reaped) != 0 || len(res.Orphans) != 0 || len(res.Repairs) != 0 {
			t.Fatalf("store unknown must reap/repair NOTHING, got reaped=%+v orphans=%+v repairs=%+v", res.Reaped, res.Orphans, res.Repairs)
		}
		if len(res.Unverifiable) != 2 {
			t.Fatalf("both broken FILE links must be UNVERIFIABLE, got %+v", res.Unverifiable)
		}
		if !exists(t, live) || !exists(t, filepath.Join(root, "gate-1", "pending", "orphan.md")) {
			t.Error("a link was removed despite the store being unverifiable")
		}
		if out := FormatRepair(res); !strings.Contains(out, "UNVERIFIABLE") {
			t.Errorf("FormatRepair should report UNVERIFIABLE: %q", out)
		}
	}
}

// TestRepair_BasenameMatchDesignatedStoreOnly: a same-name file living OUTSIDE the
// designated store must NOT make a broken link look repairable — the basename is looked up
// in the STORE ROOT only, so an absent-from-store link is reaped (orphan), never falsely
// repaired against the impostor elsewhere.
func TestRepair_BasenameMatchDesignatedStoreOnly(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	// A same-name file in a DIFFERENT dir (not the designated store).
	other := filepath.Join(root, "somewhere-else")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "1393.md"), []byte("impostor"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A broken link named 1393.md — its basename is present ELSEWHERE but ABSENT from store.
	link := filepath.Join(root, "_stale", "pending", "1393.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.FromSlash("../../../tasks/1393.md"), link); err != nil {
		t.Fatal(err)
	}

	res, err := Repair(root, store, true, false)
	if err != nil {
		t.Fatalf("Repair apply: %v", err)
	}
	if len(res.Repairs) != 0 {
		t.Fatalf("a same-name file OUTSIDE the store must not make the link repairable, got %+v", res.Repairs)
	}
	if len(res.Reaped) != 1 || res.Reaped[0].Name != "1393.md" {
		t.Fatalf("the link (absent from the designated store) should be reaped as an orphan, got %+v", res.Reaped)
	}
	// The impostor file is untouched.
	if !exists(t, filepath.Join(other, "1393.md")) {
		t.Error("the reaper touched a file outside the store")
	}
}
