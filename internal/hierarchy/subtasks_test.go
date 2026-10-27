package hierarchy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedHierarchy lays out a store + issues dir modelling #123's shape: an epic 123 under
// milestone m1, an Issue I78 parented to that epic, and tasks tagged various ways.
func seedHierarchy(t *testing.T) (store, issues string) {
	t.Helper()
	store = t.TempDir()
	issues = t.TempDir()
	// milestones.yaml is parsed by yaml.v3 (quotes stripped → epic id "123"). Task/issue
	// frontmatter is parsed by review.ReadFrontmatter (raw text, quotes NOT stripped), so
	// their tags are UNQUOTED — matching #123's real store data (`epic: 123`, `parent: I78`).
	writeFile(t, filepath.Join(store, "milestones.yaml"),
		"project: demo\nmilestones:\n  - id: m1\n    title: One\n    state: active\n    epics:\n      - id: \"123\"\n")
	writeFile(t, filepath.Join(issues, "I78-extract.md"),
		"---\nid: I78\nparent: 123\nstatus: in_progress\n---\nissue\n")
	return store, issues
}

// #123 regression: a task tagged `epic: 123` (its milestone:/parent: hold sub-values,
// NOT the epic id) must count for id 123. Fails before the rollup fix (flat match saw 0).
func TestOpenTasks_EpicTaggedTask_countsForEpic(t *testing.T) {
	store, issues := seedHierarchy(t)
	// milestone: holds a sub-number, parent: an Issue — neither equals "123".
	writeFile(t, filepath.Join(store, "500-a.md"),
		"---\nid: 500\nepic: 123\nmilestone: 2\nparent: I63\nstatus: in_progress\n---\nwork\n")

	n, known, err := OpenTasksForMilestone(store, issues, "123")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !known {
		t.Fatalf("epic-tagged task: count=%d known=%v, want 1/true (the #123 regression)", n, known)
	}
}

// A task tagged ONLY `parent: I78` rolls up through I78 -> epic 123 -> milestone m1, so it
// counts for the epic id AND the milestone id.
func TestOpenTasks_ParentOnlyTask_rollsUpThroughIssue(t *testing.T) {
	store, issues := seedHierarchy(t)
	writeFile(t, filepath.Join(store, "501-b.md"),
		"---\nid: 501\nparent: I78\nstatus: in_progress\n---\nwork\n")

	for _, id := range []string{"123", "m1"} {
		n, known, err := OpenTasksForMilestone(store, issues, id)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 || !known {
			t.Errorf("parent-only task rollup to %q: count=%d known=%v, want 1/true", id, n, known)
		}
	}
}

// Direct `milestone:`/`parent:` matches still count (no regression on the old flat paths).
func TestOpenTasks_DirectTags_stillCounted(t *testing.T) {
	store, issues := seedHierarchy(t)
	writeFile(t, filepath.Join(store, "502-c.md"),
		"---\nid: 502\nmilestone: m1\nstatus: in_progress\n---\nwork\n")
	writeFile(t, filepath.Join(store, "503-d.md"),
		"---\nid: 503\nparent: 123\nstatus: todo\n---\nwork\n")

	nMs, _, err := OpenTasksForMilestone(store, issues, "m1")
	if err != nil {
		t.Fatal(err)
	}
	// 502 (milestone: m1) + 503 (parent: epic 123 -> m1) both roll up to m1.
	if nMs != 2 {
		t.Errorf("direct-tag count for m1 = %d, want 2", nMs)
	}
	nEpic, _, _ := OpenTasksForMilestone(store, issues, "123")
	if nEpic != 1 {
		t.Errorf("direct parent:123 count = %d, want 1", nEpic)
	}
}

// The crux: 0 because DONE vs 0 because MIS-KEYED.
// A KNOWN epic whose only task is terminal → count 0 but known=true (genuine zero → MET).
func TestOpenTasks_KnownIdAllTerminal_isGenuineZero(t *testing.T) {
	store, issues := seedHierarchy(t)
	writeFile(t, filepath.Join(store, "504-e.md"),
		"---\nid: 504\nepic: 123\nstatus: completed\n---\ndone\n")

	n, known, err := OpenTasksForMilestone(store, issues, "123")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || !known {
		t.Fatalf("known epic, all terminal: count=%d known=%v, want 0/true (genuine zero, stays MET)", n, known)
	}
}

// An id that matches NOTHING in the hierarchy (no model node, no issue, no task rolls up
// to it) → count 0 AND known=false, so the caller reads it Unverified, never a false MET.
func TestOpenTasks_UnknownId_isNotKnown(t *testing.T) {
	store, issues := seedHierarchy(t)
	writeFile(t, filepath.Join(store, "505-f.md"),
		"---\nid: 505\nepic: 123\nstatus: in_progress\n---\nwork\n")

	n, known, err := OpenTasksForMilestone(store, issues, "999")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || known {
		t.Fatalf("mis-keyed id 999: count=%d known=%v, want 0/false (must BLOCK as unverified)", n, known)
	}
}

// Open vs terminal honored via the existing normalizer (hyphen/case variants terminal).
func TestOpenTasks_TerminalStatusesHonored(t *testing.T) {
	store, issues := seedHierarchy(t)
	writeFile(t, filepath.Join(store, "506-g.md"),
		"---\nid: 506\nepic: 123\nstatus: In-Progress\n---\nopen\n")
	writeFile(t, filepath.Join(store, "507-h.md"),
		"---\nid: 507\nepic: 123\nstatus: Done\n---\nterminal\n")

	n, _, err := OpenTasksForMilestone(store, issues, "123")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("terminal-status honoring: count=%d, want 1 (only the in-progress one)", n)
	}
}

// Empty id matches nothing and is not known.
func TestOpenTasks_EmptyId(t *testing.T) {
	store, issues := seedHierarchy(t)
	n, known, err := OpenTasksForMilestone(store, issues, "")
	if err != nil || n != 0 || known {
		t.Fatalf("empty id = (%d,%v,%v), want (0,false,nil)", n, known, err)
	}
}
