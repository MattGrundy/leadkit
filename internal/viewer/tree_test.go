package viewer

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// setupTreeFixture builds a throwaway store+issues+queue tree — never real data —
// with a full Milestone->Epic->Issue->Task chain, plus one task with no parent:
// and one with a dangling parent: (names an Issue that doesn't exist), so both
// land under the orphan bucket rather than vanishing.
func setupTreeFixture(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	queueRoot := filepath.Join(root, "queue")

	mustWriteFile(t, filepath.Join(storeRoot, "milestones.yaml"), `
project: test project
milestones:
  - id: m1
    title: Milestone One
    state: active
    epics:
      - id: e1
        note: epic one note
`)
	mustWriteFile(t, filepath.Join(issuesRoot, "I1-issue-one.md"), "---\nid: I1\ntitle: Issue One\nparent: e1\nstatus: pending\n---\n\nbody\n")

	// 1: parented to I1, placed in the queue overlay (in-progress) — its row must
	// show that queue lane/status, not its store frontmatter status.
	mustWriteFile(t, filepath.Join(storeRoot, "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "backend", "in-progress", "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")

	// 2: parented to I1, done, never placed in the queue overlay — falls back to
	// its own frontmatter status.
	mustWriteFile(t, filepath.Join(storeRoot, "2-task-two.md"), "---\nid: 2\ntitle: Task Two\nparent: I1\nstatus: completed\n---\n\nbody\n")

	// 3: no parent: at all.
	mustWriteFile(t, filepath.Join(storeRoot, "3-task-noparent.md"), "---\nid: 3\ntitle: Task No Parent\nstatus: pending\n---\n\nbody\n")

	// 4: parent: names an Issue that doesn't exist (dangling).
	mustWriteFile(t, filepath.Join(storeRoot, "4-task-dangling.md"), "---\nid: 4\ntitle: Task Dangling\nparent: I999\nstatus: pending\n---\n\nbody\n")

	return &Server{Root: queueRoot, Store: storeRoot, IssuesRoot: issuesRoot}
}

func getTree(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/tree", nil)
	rec := httptest.NewRecorder()
	s.handleTree(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /tree: status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestHandleTree_RendersMilestoneEpicIssueTaskNestingWithRollups(t *testing.T) {
	s := setupTreeFixture(t)
	body := getTree(t, s)

	for _, want := range []string{
		"Milestone One", "e1", "epic one note", "Issue One", "Task One", "Task Two",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tree missing %q:\n%s", want, body)
		}
	}
	// Issue I1 rolls up 2 tasks, 1 done (Task Two/completed), 1 active (Task One).
	if !strings.Contains(body, "2 tasks") {
		t.Errorf("tree missing the Issue's rollup count (2 tasks):\n%s", body)
	}
	if !strings.Contains(body, "1 done") {
		t.Errorf("tree missing the Issue's done rollup (1 done):\n%s", body)
	}
}

func TestHandleTree_TaskRowShowsQueueLaneAndStatus(t *testing.T) {
	s := setupTreeFixture(t)
	body := getTree(t, s)

	// Task One is placed in queue at backend/in-progress — the row must show that,
	// not its store frontmatter status (pending).
	i := strings.Index(body, "Task One")
	if i < 0 {
		t.Fatalf("Task One not found in tree:\n%s", body)
	}
	row := body[i : i+200]
	if !strings.Contains(row, "in-progress") || !strings.Contains(row, "backend") {
		t.Errorf("Task One's row must show its queue lane/status (backend/in-progress), got:\n%s", row)
	}
}

func TestHandleTree_OrphanTasksNeverDropped(t *testing.T) {
	s := setupTreeFixture(t)
	body := getTree(t, s)

	if !strings.Contains(body, "orphan") {
		t.Errorf("tree missing an orphan section:\n%s", body)
	}
	for _, want := range []string{"Task No Parent", "Task Dangling"} {
		if !strings.Contains(body, want) {
			t.Errorf("orphan task %q must still appear in the tree, never dropped:\n%s", want, body)
		}
	}
}

func TestHandleTree_MissingMilestonesAndIssuesRendersEmptyTreeNoCrash(t *testing.T) {
	storeRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(storeRoot, "1-solo.md"), "---\nid: 1\ntitle: Solo\nstatus: pending\n---\n\nbody\n")
	s := &Server{Store: storeRoot} // no milestones.yaml, no issues/, no Root
	body := getTree(t, s)

	if !strings.Contains(body, "no hierarchy seeded") {
		t.Errorf("want the no-hierarchy empty state, got:\n%s", body)
	}
	// The lone unparented task must still surface as an orphan, not vanish.
	if !strings.Contains(body, "Solo") {
		t.Errorf("want the unparented task to still appear under orphans:\n%s", body)
	}
}

func TestHandleTree_ZeroValueServerNeverPanics(t *testing.T) {
	s := &Server{}
	body := getTree(t, s)
	if !strings.Contains(body, "no hierarchy seeded") {
		t.Errorf("want the no-hierarchy empty state for a zero-value Server, got:\n%s", body)
	}
}

func TestHandleBoard_UnaffectedByTreeFixture(t *testing.T) {
	// Regression: adding /tree must not change the existing queue view's behaviour.
	s := setupTreeFixture(t)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleBoard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "1-task-one.md") {
		t.Errorf("queue board must still list the queue-placed task:\n%s", body)
	}
	if strings.Contains(body, "Issue One") || strings.Contains(body, "epic one note") {
		t.Error("the queue board must not render any hierarchy tree content")
	}
}
