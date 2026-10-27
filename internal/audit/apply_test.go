package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/review"
)

var applyCanonicalStatuses = []string{
	"pending", "in_progress", "in_review", "blocked",
	"needs_rework", "gated", "completed", "dropped", "superseded",
}

var applyAliases = map[string]string{
	"done":            "completed",
	"awaiting_review": "in_review",
}

func allFixOptions(dryRun bool) ApplyOptions {
	return ApplyOptions{
		Dangling: true, Noncanonical: true, DoneDrift: true,
		Aliases: applyAliases, CanonicalStatuses: applyCanonicalStatuses,
		DryRun: dryRun,
	}
}

// snapshotTree walks root and returns every regular file's content plus every
// symlink's target, keyed by path relative to root — a cheap byte-for-byte
// before/after comparator for "dry-run changed nothing" assertions.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if info.Mode()&os.ModeSymlink != 0 {
			tgt, _ := os.Readlink(path)
			out[rel] = "symlink:" + tgt
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

func assertTreesEqual(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("tree changed size: before=%d after=%d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("tree entry %q changed:\n before=%q\n after=%q", k, v, after[k])
		}
	}
}

// buildApplyFixture lays out one instance of every category's trigger case:
//   - dangling: cr-1/pending/3-dangling.md, target removed
//   - resolvable: cr-1/pending/2-ok.md, stays untouched by every category
//   - noncanonical alias: 4-alias.md status=done, placed nowhere (so drift doesn't fire)
//   - noncanonical malformed: 5-malformed.md status="LOCKED (foo)"
//   - done-drift: 6-drifted.md status=completed, overlay in cr-1/pending (active)
//   - already-in-done (ambiguous reverse): 7-reopened.md status=pending, overlay in cr-1/done
func buildApplyFixture(t *testing.T) (root, queueRoot, storeRoot string) {
	t.Helper()
	root = t.TempDir()
	queueRoot = filepath.Join(root, "queue")
	storeRoot = filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTask(t, storeRoot, "2-ok.md", "pending")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "2-ok.md")

	writeTask(t, storeRoot, "3-dangling.md", "pending")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "3-dangling.md")
	if err := os.Remove(filepath.Join(storeRoot, "3-dangling.md")); err != nil {
		t.Fatal(err)
	}

	writeTask(t, storeRoot, "4-alias.md", "done")

	writeTask(t, storeRoot, "5-malformed.md", "LOCKED (by someone)")

	writeTask(t, storeRoot, "6-drifted.md", "completed")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "6-drifted.md")

	writeTask(t, storeRoot, "7-reopened.md", "pending")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "done", "7-reopened.md")

	return root, queueRoot, storeRoot
}

func TestApply_DryRun_ChangesNothingAndPlansEveryCategory(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	before := snapshotTree(t, filepath.Dir(queueRoot))

	res, err := Apply(queueRoot, storeRoot, allFixOptions(true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("DryRun result must report Applied=false")
	}

	after := snapshotTree(t, filepath.Dir(queueRoot))
	assertTreesEqual(t, before, after)

	byCategory := map[string]int{}
	for _, f := range res.Fixes {
		byCategory[f.Category]++
	}
	if byCategory["dangling"] != 1 {
		t.Errorf("dangling planned = %d, want 1", byCategory["dangling"])
	}
	if byCategory["noncanonical"] != 1 {
		t.Errorf("noncanonical planned = %d, want 1", byCategory["noncanonical"])
	}
	if byCategory["done-drift"] != 1 {
		t.Errorf("done-drift planned = %d, want 1", byCategory["done-drift"])
	}
	if len(res.Manual) != 1 {
		t.Errorf("want exactly 1 manual (malformed) report, got %d: %+v", len(res.Manual), res.Manual)
	}

	report := FormatApplyResult(res)
	for _, want := range []string{"dangling: 1 planned", "noncanonical: 1 planned", "done-drift: 1 planned", "[manual]"} {
		if !strings.Contains(report, want) {
			t.Errorf("FormatApplyResult missing %q:\n%s", want, report)
		}
	}
}

func TestApply_Dangling_RemovesOnlyBrokenLinks(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	res, err := Apply(queueRoot, storeRoot, ApplyOptions{Dangling: true, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixes) != 1 || res.Fixes[0].Category != "dangling" {
		t.Fatalf("want exactly 1 dangling fix, got %+v", res.Fixes)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "pending", "3-dangling.md")); !os.IsNotExist(err) {
		t.Error("the dangling symlink should be gone")
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "pending", "2-ok.md")); err != nil {
		t.Error("the resolvable symlink must be left alone")
	}
}

func TestApply_Noncanonical_RewritesAliasesLeavesMalformed(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	res, err := Apply(queueRoot, storeRoot, ApplyOptions{
		Noncanonical: true, Aliases: applyAliases, CanonicalStatuses: applyCanonicalStatuses, DryRun: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixes) != 1 {
		t.Fatalf("want exactly 1 noncanonical fix, got %+v", res.Fixes)
	}
	fm, err := mustReadFrontmatter(t, filepath.Join(storeRoot, "4-alias.md"))
	if err != nil {
		t.Fatal(err)
	}
	if fm["status"] != "completed" {
		t.Errorf("4-alias.md status = %q, want completed (done->completed)", fm["status"])
	}

	malformedFM, err := mustReadFrontmatter(t, filepath.Join(storeRoot, "5-malformed.md"))
	if err != nil {
		t.Fatal(err)
	}
	if malformedFM["status"] != "LOCKED (by someone)" {
		t.Errorf("malformed status must be left untouched, got %q", malformedFM["status"])
	}
	if len(res.Manual) != 1 || !strings.Contains(res.Manual[0].Detail, "LOCKED") {
		t.Errorf("want the malformed value reported in Manual, got %+v", res.Manual)
	}
}

func TestApply_AwaitingReviewAlias(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTask(t, storeRoot, "1-x.md", "awaiting_review")

	res, err := Apply(queueRoot, storeRoot, ApplyOptions{
		Noncanonical: true, Aliases: applyAliases, CanonicalStatuses: applyCanonicalStatuses, DryRun: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixes) != 1 || res.Fixes[0].To != "in_review" {
		t.Fatalf("want awaiting_review -> in_review, got %+v", res.Fixes)
	}
}

func TestApply_DoneDrift_RelocatesTerminalOutOfActive_LeavesReopenedAlone(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	res, err := Apply(queueRoot, storeRoot, ApplyOptions{
		DoneDrift: true, CanonicalStatuses: applyCanonicalStatuses, DryRun: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fixes) != 1 || res.Fixes[0].Category != "done-drift" {
		t.Fatalf("want exactly 1 done-drift fix, got %+v", res.Fixes)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "pending", "6-drifted.md")); !os.IsNotExist(err) {
		t.Error("6-drifted.md should no longer be in the active pending/ dir")
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "done", "6-drifted.md")); err != nil {
		t.Error("6-drifted.md should now be in done/")
	}
	// The ambiguous reverse case must be left exactly alone.
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "done", "7-reopened.md")); err != nil {
		t.Error("7-reopened.md (active status sitting in done/) must NOT be touched — ambiguous, report only")
	}
}

// TestApply_DoneDrift_ExcludesInProgress is the post-incident Fix 1 test: a
// terminal-status task sitting in in-progress/ (an active CLAIM) must be reported,
// never auto-moved — only a non-claim active dir (pending/) is still relocated.
func TestApply_DoneDrift_ExcludesInProgress(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTask(t, storeRoot, "8-claimed.md", "completed")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "in-progress", "8-claimed.md")

	writeTask(t, storeRoot, "9-pending-terminal.md", "dropped")
	linkOverlay(t, queueRoot, storeRoot, "cr-1", "pending", "9-pending-terminal.md")

	res, err := Apply(queueRoot, storeRoot, ApplyOptions{
		DoneDrift: true, CanonicalStatuses: applyCanonicalStatuses, DryRun: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Fixes) != 1 || res.Fixes[0].Target != "9-pending-terminal.md" {
		t.Fatalf("want exactly 1 done-drift fix (the pending/ one), got %+v", res.Fixes)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "done", "9-pending-terminal.md")); err != nil {
		t.Error("9-pending-terminal.md should have been relocated to done/")
	}

	// The in-progress/ entry must be left exactly where it was.
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "in-progress", "8-claimed.md")); err != nil {
		t.Error("8-claimed.md (terminal status, but in-progress/) must NOT be moved — active claim")
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-1", "done", "8-claimed.md")); !os.IsNotExist(err) {
		t.Error("8-claimed.md must not appear in done/")
	}

	found := false
	for _, m := range res.Manual {
		if strings.Contains(m.Detail, "8-claimed.md") {
			found = true
			if !strings.Contains(m.Detail, "in-progress") {
				t.Errorf("manual report for 8-claimed.md should mention in-progress: %q", m.Detail)
			}
		}
	}
	if !found {
		t.Errorf("want a Manual report for the in-progress/ terminal-status task, got %+v", res.Manual)
	}
}

func TestApply_ChangeLog_RecordsEachFix(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	logPath := filepath.Join(t.TempDir(), "audit-apply.jsonl")
	opts := allFixOptions(false)
	opts.LogPath = logPath
	res, err := Apply(queueRoot, storeRoot, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("change log not written: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(res.Fixes) {
		t.Fatalf("log has %d lines, want %d (one per fix): %s", len(lines), len(res.Fixes), data)
	}
	for _, want := range []string{`"category":"dangling"`, `"category":"noncanonical"`, `"category":"done-drift"`, `"action":"reap"`, `"action":"normalize"`, `"action":"relocate"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("change log missing %q:\n%s", want, data)
		}
	}
}

func TestApply_SecondApplyIsANoOp(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	logPath := filepath.Join(t.TempDir(), "audit-apply.jsonl")
	opts := allFixOptions(false)
	opts.LogPath = logPath

	if _, err := Apply(queueRoot, storeRoot, opts); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, filepath.Dir(queueRoot))

	res2, err := Apply(queueRoot, storeRoot, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Fixes) != 0 {
		t.Errorf("second Apply should find nothing left to fix, got %+v", res2.Fixes)
	}

	after := snapshotTree(t, filepath.Dir(queueRoot))
	assertTreesEqual(t, before, after)
}

func TestApply_FixSelection_NarrowsToOneCategory(t *testing.T) {
	_, queueRoot, storeRoot := buildApplyFixture(t)
	res, err := Apply(queueRoot, storeRoot, ApplyOptions{Dangling: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Fixes {
		if f.Category != "dangling" {
			t.Errorf("with only Dangling enabled, got a %s fix: %+v", f.Category, f)
		}
	}
	if len(res.Fixes) != 1 {
		t.Errorf("got %d fixes, want exactly the 1 dangling one", len(res.Fixes))
	}
}

// mustReadFrontmatter is review.ReadFrontmatter with the test's *testing.T threaded
// through for a Fatal on error, so call sites read as one line.
func mustReadFrontmatter(t *testing.T, path string) (map[string]string, error) {
	t.Helper()
	return review.ReadFrontmatter(path)
}
