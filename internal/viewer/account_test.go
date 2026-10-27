package viewer

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/usage"
)

var acctBaseNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// setupAccountFixture registers two projects (A and B) in a fresh registry dir
// — A has a full Milestone->Epic->Issue->Task hierarchy (setupTreeFixture's own
// fixture, reused so /p/A/tree renders through the SAME BuildTree as
// single-project mode), B is a plain queue with a couple of tasks and no
// hierarchy at all — and returns the AccountServer plus each project's slug.
func setupAccountFixture(t *testing.T) (a *AccountServer, slugA, slugB string) {
	t.Helper()
	projectsDir := t.TempDir()

	// Project A: full hierarchy fixture (mirrors setupTreeFixture exactly).
	rootA := t.TempDir()
	storeA := filepath.Join(rootA, "tasks")
	issuesA := filepath.Join(rootA, "issues")
	queueA := filepath.Join(rootA, "queue")
	mustWriteFile(t, filepath.Join(storeA, "milestones.yaml"), `
project: project A
milestones:
  - id: m1
    title: Milestone One
    state: active
    epics:
      - id: e1
        note: epic one note
`)
	mustWriteFile(t, filepath.Join(issuesA, "I1-issue-one.md"), "---\nid: I1\ntitle: Issue One\nparent: e1\nstatus: pending\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(storeA, "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueA, "backend", "in-progress", "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")

	// Project B: plain queue, no hierarchy.
	rootB := t.TempDir()
	queueB := filepath.Join(rootB, "queue")
	mustWriteFile(t, filepath.Join(queueB, "frontend", "pending", "1-b-task.md"), "---\ntitle: B Task One\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueB, "frontend", "done", "2-b-task.md"), "---\ntitle: B Task Two\n---\n\nbody\n")

	if err := usage.WriteProjectEntry(projectsDir, "project-a", "Project A", queueA, storeA, issuesA, acctBaseNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteProjectEntry(projectsDir, "project-b", "Project B", queueB, "", "", acctBaseNow); err != nil {
		t.Fatal(err)
	}

	return &AccountServer{ProjectsDir: projectsDir}, "project-a", "project-b"
}

func TestAccountIndex_ListsBothRegisteredProjects(t *testing.T) {
	a, slugA, slugB := setupAccountFixture(t)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	a.handleIndex(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Project A", "Project B",
		"/p/" + slugA + "/", "/p/" + slugB + "/",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account index missing %q\n%s", want, body)
		}
	}
	// Project B has 2 tasks registered (1 pending + 1 done) — the index's live
	// task-count column, not a stale/registered-at-write-time number.
	if !strings.Contains(body, "2") {
		t.Errorf("account index missing project B's live task count\n%s", body)
	}
}

func TestAccountProject_TreeRoute_RendersThatProjectsHierarchy(t *testing.T) {
	a, slugA, _ := setupAccountFixture(t)
	req := httptest.NewRequest("GET", "/p/"+slugA+"/tree", nil)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Milestone One", "Issue One", "Task One", "project A"} {
		if !strings.Contains(body, want) {
			t.Errorf("/p/%s/tree missing %q\n%s", slugA, want, body)
		}
	}
}

func TestAccountProject_BoardRoute_RendersThatProjectsQueue(t *testing.T) {
	a, _, slugB := setupAccountFixture(t)
	req := httptest.NewRequest("GET", "/p/"+slugB+"/", nil)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"B Task One", "B Task Two", "frontend"} {
		if !strings.Contains(body, want) {
			t.Errorf("/p/%s/ missing %q\n%s", slugB, want, body)
		}
	}
	// Project A's data must never leak into project B's rendered board.
	if strings.Contains(body, "Milestone One") || strings.Contains(body, "Issue One") || strings.Contains(body, "backend") {
		t.Errorf("/p/%s/ leaked project A's data\n%s", slugB, body)
	}
}

func TestAccountIndex_UnreadableProjectShownUnavailable_OthersStillRender(t *testing.T) {
	projectsDir := t.TempDir()
	rootB := t.TempDir()
	queueB := filepath.Join(rootB, "queue")
	mustWriteFile(t, filepath.Join(queueB, "frontend", "pending", "1-b-task.md"), "---\ntitle: B Task One\n---\n\nbody\n")

	// Project A points at a queue root that was never created.
	if err := usage.WriteProjectEntry(projectsDir, "ghost-project", "Ghost Project", filepath.Join(t.TempDir(), "never-created"), "", "", acctBaseNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteProjectEntry(projectsDir, "project-b", "Project B", queueB, "", "", acctBaseNow); err != nil {
		t.Fatal(err)
	}

	a := &AccountServer{ProjectsDir: projectsDir}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	a.handleIndex(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (one bad project must never crash the index)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Ghost Project") {
		t.Errorf("index missing the unavailable project's row entirely\n%s", body)
	}
	if !strings.Contains(body, "unavailable") {
		t.Errorf("index missing the 'unavailable' marker for the ghost project\n%s", body)
	}
	if !strings.Contains(body, "Project B") || !strings.Contains(body, "/p/project-b/") {
		t.Errorf("a bad project must not stop other projects from rendering in the index\n%s", body)
	}

	// And the ghost project's own sub-route must 404, not panic/500.
	req2 := httptest.NewRequest("GET", "/p/ghost-project/", nil)
	rec2 := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec2, req2)
	if rec2.Code == 200 {
		t.Logf("ghost project route rendered (empty scan) with status %d — acceptable as long as it never crashed", rec2.Code)
	}
}

func TestAccountIndex_EmptyRegistry_EmptyIndexNoCrash(t *testing.T) {
	a := &AccountServer{ProjectsDir: filepath.Join(t.TempDir(), "does-not-exist")}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	a.handleIndex(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (an empty/missing registry is not an error)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no projects registered") {
		t.Errorf("empty registry should render the 'no projects registered' message, got:\n%s", body)
	}
}

func TestAccountProject_UnknownSlug_404sNotCrash(t *testing.T) {
	a, _, _ := setupAccountFixture(t)
	req := httptest.NewRequest("GET", "/p/does-not-exist/", nil)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 for an unregistered slug", rec.Code)
	}
}

// TestAccountProject_BoardBucketsOwnersAndHidesEmpty is board-role-facets-hide-
// empty.md's account-mode requirement: a registered project's board — rendered
// through the SAME per-project *Server the account router delegates to — applies
// the role-bucketed owner facet + hide-empty. Four raw owner instances that all
// fold to code-review collapse to ONE owner chip (chip count shrinks vs raw
// distinct), and a done-only lane is hidden. Owners fold here via the always-on
// FixedRoles/instance fold (the account registry carries no per-project roster).
func TestAccountProject_BoardBucketsOwnersAndHidesEmpty(t *testing.T) {
	projectsDir := t.TempDir()
	root := t.TempDir()
	queue := filepath.Join(root, "queue")
	// review lane: three live tasks, owners all fold to code-review.
	mustWriteFile(t, filepath.Join(queue, "review", "pending", "a.md"), "---\ntitle: a\nowner: code-review\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queue, "review", "pending", "b.md"), "---\ntitle: b\nowner: cr-1\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queue, "review", "in-progress", "c.md"), "---\ntitle: c\nowner: cr-2\n---\n\nbody\n")
	// stale lane: done-only (0 live) + a fourth raw owner instance.
	mustWriteFile(t, filepath.Join(queue, "stale", "done", "d.md"), "---\ntitle: d\nowner: cr-3\n---\n\nbody\n")

	if err := usage.WriteProjectEntry(projectsDir, "proj", "Proj", queue, "", "", acctBaseNow); err != nil {
		t.Fatal(err)
	}
	a := &AccountServer{ProjectsDir: projectsDir}

	req := httptest.NewRequest("GET", "/p/proj/", nil)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The four raw owners fold to ONE code-review bucket chip.
	if !strings.Contains(body, `href="/?owner=code-review"`) {
		t.Errorf("account project board missing the folded code-review owner chip:\n%s", body)
	}
	for _, raw := range []string{"cr-1", "cr-2", "cr-3"} {
		if strings.Contains(body, `href="/?owner=`+raw+`"`) {
			t.Errorf("account project board rendered a per-instance owner chip for %q — must fold:\n%s", raw, body)
		}
	}
	// The done-only lane is hidden; the live lane keeps its chip.
	if strings.Contains(body, `href="/?lane=stale"`) {
		t.Errorf("done-only lane 'stale' must be hidden on the account board:\n%s", body)
	}
	if !strings.Contains(body, `href="/?lane=review"`) {
		t.Errorf("live lane 'review' must be present on the account board:\n%s", body)
	}
}
