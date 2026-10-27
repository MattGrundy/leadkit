package main

// Source-comment history ratchet: comments (in _test.go too) may not narrate history or state a
// spelled-out count. A file not on historyCommentAllowlist must have no hit; a file on it must
// still have one, so the list only shrinks. A hit is a candidate for a human: rewrite a real one
// as the constraint it states, and allowlist a false one.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/commentscan"
)

// historyCommentAllowlist is the set of Go files KNOWN to still carry a history marker or a
// spelled-out count in a comment. Every entry is debt, not a permission: shrink this list,
// never grow it. Adding a path to make a failing build green reverses the ratchet.
var historyCommentAllowlist = []string{
	"cmd/leadkit/burn_priority_test.go",
	"cmd/leadkit/cmd_burn.go",
	"cmd/leadkit/cmd_misc.go",
	"cmd/leadkit/cmd_queue.go",
	"cmd/leadkit/cmd_registry.go",
	"cmd/leadkit/guard_target_path_norm_test.go",
	"cmd/leadkit/guards_target_resolve_test.go",
	"cmd/leadkit/merge_gate_line_test.go",
	"cmd/leadkit/owner_guard_sentinel_test.go",
	"cmd/leadkit/project_registry_test.go",
	"cmd/leadkit/push_guard_whitelist_test.go",
	"cmd/leadkit/resolve.go",
	"cmd/leadkit/session_identity_origin_test.go",
	"internal/burn/burn_test.go",
	"internal/burn/priority.go",
	"internal/config/config.go",
	"internal/doctor/doctor.go",
	"internal/doctor/doctor_test.go",
	"internal/gate/gate_test.go",
	"internal/hierarchy/propose.go",
	"internal/hierarchy/subtasks_test.go",
	"internal/hook/brief_length.go",
	"internal/hook/owner_guard.go",
	"internal/hook/pr_guard.go",
	"internal/hook/pr_guard_test.go",
	"internal/hook/push_guard_test.go",
	"internal/hook/task_status_guard_test.go",
	"internal/install/daemon.go",
	"internal/install/daemon_test.go",
	"internal/install/install_test.go",
	"internal/milestone/resolvers.go",
	"internal/orchestrate/apply.go",
	"internal/orchestrate/apply_test.go",
	"internal/orchestrate/plan.go",
	"internal/queue/assign.go",
	"internal/queue/assign_test.go",
	"internal/queue/queue_test.go",
	"internal/queue/remove_test.go",
	"internal/queue/repair.go",
	"internal/queue/repair_test.go",
	"internal/queue/watch_test.go",
	"internal/review/send.go",
	"internal/review/send_test.go",
	"internal/review/verdict.go",
	"internal/routing/addressing_test.go",
	"internal/routing/routing.go",
	"internal/state/handover.go",
	"internal/team/scaffold_test.go",
	"internal/team/validate.go",
	"internal/team/validate_starter_test.go",
	"internal/viewer/account_test.go",
}

// commentHistoryFinding is one candidate: where, which shape, and the offending comment line.
type commentHistoryFinding struct {
	rel  string
	line int
	kind string // "history" or "count"
	text string
}

// scanCommentsForHistory returns every comment line, in every tracked .go file including
// _test.go, that matches either shape. It does not apply the allowlist.
func scanCommentsForHistory(t *testing.T, root string) []commentHistoryFinding {
	t.Helper()
	files, err := commentscan.Files(root, ".go")
	if err != nil {
		t.Fatalf("list %s: %v", root, err)
	}
	var out []commentHistoryFinding
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if commentscan.IsGenerated(data) {
			continue
		}
		lines, err := commentscan.CommentLines(rel, data)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for _, l := range lines {
			if commentscan.IsHistory(l.Text) {
				out = append(out, commentHistoryFinding{rel, l.Line, "history", l.Text})
			}
			if commentscan.SpelledCount.MatchString(l.Text) {
				out = append(out, commentHistoryFinding{rel, l.Line, "count", l.Text})
			}
		}
	}
	return out
}

func historyFilesWithHits(fs []commentHistoryFinding) map[string][]commentHistoryFinding {
	by := map[string][]commentHistoryFinding{}
	for _, f := range fs {
		by[f.rel] = append(by[f.rel], f)
	}
	return by
}

func onHistoryAllowlist(rel string) bool {
	for _, e := range historyCommentAllowlist {
		if e == rel {
			return true
		}
	}
	return false
}

// TestComments_NewHistoryOrSpelledCountFails is the forward half: a file that is not on the
// allowlist may not narrate history or state a count in a comment.
func TestComments_NewHistoryOrSpelledCountFails(t *testing.T) {
	var leaked []string
	for rel, fs := range historyFilesWithHits(scanCommentsForHistory(t, repoRootFromCwd(t))) {
		if onHistoryAllowlist(rel) {
			continue
		}
		for _, f := range fs {
			leaked = append(leaked, rel+":"+itoa(f.line)+": ["+f.kind+"] "+f.text)
		}
	}
	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Fatalf("%d comment line(s) narrate history or state a count nothing checks.\n"+
			"History goes in the commit message; a comment states the constraint on the next change. "+
			"Do NOT add the file to historyCommentAllowlist, which only records debt that predates this check:\n%s",
			len(leaked), strings.Join(leaked, "\n"))
	}
}

// TestComments_AllowlistHasNoStaleEntries is the reverse half: an allowlisted file with no hit
// left is a cleaned-up file, and keeping it listed would exempt it from the forward half forever.
func TestComments_AllowlistHasNoStaleEntries(t *testing.T) {
	by := historyFilesWithHits(scanCommentsForHistory(t, repoRootFromCwd(t)))
	var stale []string
	for _, e := range historyCommentAllowlist {
		if len(by[e]) == 0 {
			stale = append(stale, e)
		}
	}
	if len(stale) > 0 {
		t.Fatalf("historyCommentAllowlist lists %d file(s) with nothing left to flag; delete the entries:\n%s",
			len(stale), strings.Join(stale, "\n"))
	}
	seen := map[string]bool{}
	for _, e := range historyCommentAllowlist {
		if seen[e] {
			t.Errorf("duplicate allowlist entry %q", e)
		}
		seen[e] = true
	}
	if !sort.StringsAreSorted(historyCommentAllowlist) {
		t.Error("historyCommentAllowlist must stay sorted so a diff shows what was added or removed")
	}
}

// TestComments_ScannerIsNotInert proves the scan flags history and counts in _test.go and skips
// present-tense comments, string literals and state directories.
func TestComments_ScannerIsNotInert(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", "package a\n\n// This used to SUCCEED before the guard existed.\n"+
		"// An earlier version of this comment claimed otherwise.\n// wrong four times running\n"+
		"// eleven hooks are wired.\n// a dozen times in one run.\nfunc TestX() {}\n")
	write("b.go", "package a\n\n// Heartbeat is used to re-arm the loop.\n// It rejects the id.\nfunc B() {}\nvar s = \"used to and eleven hooks\"\n")
	// State directories are excluded at the root only: a package that happens to be named queue
	// or tasks is source.
	for _, d := range []string{"queue", "tasks", "bin", "internal/queue", "internal/tasks", "internal/bin", "internal/vendor"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(d, "c.go"), "package c\n\n// This used to be eleven bugs.\nfunc C() {}\n")
	}
	// Local state at the root is not source either.
	for _, d := range []string{".claude/worktrees/w", "team"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(d, "c.go"), "package c\n\n// This used to be eleven bugs.\nfunc C() {}\n")
	}
	// Only the generated-file header exempts a file; mentioning the phrases does not.
	write("gen.go", "// Code generated by x. DO NOT EDIT.\n\npackage a\n\n// This used to be eleven bugs.\nfunc G() {}\n")
	write("mention.go", "package a\n\n// A header reads Code generated ... DO NOT EDIT. This used to be eleven bugs.\nfunc M() {}\n")
	got := historyFilesWithHits(scanCommentsForHistory(t, dir))
	if len(got["gen.go"]) != 0 || len(got["mention.go"]) == 0 {
		t.Errorf("generated header must exempt gen.go only, got gen=%d mention=%d", len(got["gen.go"]), len(got["mention.go"]))
	}
	for _, d := range []string{"queue", "tasks", "bin", "internal/vendor", ".claude/worktrees/w", "team"} {
		if n := len(got[d+"/c.go"]); n != 0 {
			t.Errorf("%s/ is skipped and must not be flagged, got %d", d, n)
		}
	}
	for _, d := range []string{"internal/queue", "internal/tasks", "internal/bin"} {
		if len(got[d+"/c.go"]) == 0 {
			t.Errorf("%s/ is a package, not state, and must be scanned", d)
		}
	}
	if len(got["b.go"]) != 0 {
		t.Errorf("present-tense comments and string literals must not be flagged, got %v", got["b.go"])
	}
	kinds := map[string]int{}
	for _, f := range got["a_test.go"] {
		kinds[f.kind]++
	}
	if kinds["history"] != 3 || kinds["count"] != 3 {
		t.Errorf("a_test.go: want 3 history + 3 count lines (a _test.go is scanned), got %v from %+v", kinds, got["a_test.go"])
	}
}
