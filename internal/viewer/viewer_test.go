package viewer

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupHealthFixture builds a throwaway queue+store+session-tasks tree — never real
// data — with one finding from each lens: an orphaned-active store task, and a
// session-misuse (workish, non-gate) session task.
func setupHealthFixture(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	sessionRoot := filepath.Join(root, "session-tasks")
	mustMkdirAll(t, queueRoot)
	mustMkdirAll(t, storeRoot)

	mustWriteFile(t, filepath.Join(storeRoot, "1-orphan.md"), "---\ntitle: t\nstatus: pending\n---\n\nbody\n")

	longDesc := strings.Repeat("x", 300)
	mustWriteFile(t, filepath.Join(sessionRoot, "session-abc", "1.json"),
		`{"id":"1","subject":"real work item","description":"`+longDesc+`"}`)

	return &Server{
		Root: queueRoot, Store: storeRoot,
		SessionTasksRoot:  sessionRoot,
		CanonicalStatuses: []string{"pending", "in_progress", "done"},
		WorkishThreshold:  280,
		WorkishMarkers:    []string{"##", "```"},
	}
}

func TestHandleHealth_RendersFindingsWithLinksAndSummary(t *testing.T) {
	s := setupHealthFixture(t)
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"orphaned-active", "session-misuse", // category headers
		"1-orphan.md", "real work item", // the findings themselves
		"1 orphaned-active", "1 session-misuse", // the summary line's per-category counts
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health page missing %q\n%s", want, body)
		}
	}
	// The orphaned-active finding names exactly one store file — it must link there.
	// html/template HTML-escapes "&" to "&amp;" inside an href attribute (correct —
	// browsers decode it back when parsing the URL), hence the escaped form here.
	if !strings.Contains(body, `href="/task?src=store&amp;p=1-orphan.md"`) {
		t.Errorf("health page missing the orphaned-active task link:\n%s", body)
	}
	// session-misuse never links (not a queue file).
	if strings.Contains(body, `href="/task?p=1"`) {
		t.Error("session-misuse findings must not link to /task")
	}
}

func TestHandleHealth_ResilientToMissingPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	s := &Server{Root: missing, Store: missing, SessionTasksRoot: missing}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 — /health must never 500 on a missing/misconfigured path", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "0 findings") {
		t.Errorf("want an empty-but-well-formed report, got:\n%s", rec.Body.String())
	}
}

func TestHandleHealth_RendersSeededPushViolations(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	lines := []string{
		`{"ts":"2026-09-03T10:00:00Z","session_id":"sess-1","repo":"/repos/platform-infra","target":"master","command":"git push origin master","force":false,"reason":"whitelist-miss"}`,
		`{"ts":"2026-09-03T10:05:00Z","session_id":"sess-2","repo":"/repos/web-client","target":"","command":"git push","force":false,"reason":"unverifiable"}`,
	}
	mustWriteFile(t, logPath, strings.Join(lines, "\n")+"\n")

	s := &Server{PushViolationsLog: logPath}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"push violations",
		"sess-1", "sess-2",
		"whitelist-miss", "unverifiable",
		"platform-infra", "web-client",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health page missing %q from seeded push violations:\n%s", want, body)
		}
	}
	// Most-recent-first: sess-2 (later ts) must render before sess-1.
	if strings.Index(body, "sess-2") > strings.Index(body, "sess-1") {
		t.Errorf("want most-recent violation (sess-2) rendered before sess-1")
	}
}

func TestHandleHealth_NoPushViolationsLogRendersNoSection(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "push violations") {
		t.Error("no PushViolationsLog set — the push-violations section must not render at all")
	}
}

func TestHandleHealth_RendersSweepFindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep-status.json")
	mustWriteFile(t, path, `{"ts":"2026-09-13T12:00:00Z","stalled":1,"broken":1,`+
		`"stalled_lanes":[{"lane":"cr-1","pending":2}],`+
		`"broken_lanes":[{"lane":"gate-1","links":["x.md"]}]}`)

	s := &Server{SweepStatusFile: path}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"daemon diagnostics",
		"cr-1", "gate-1", "x.md",
		"leadkit queue repair", // the broken-link remediation is named
		"1 stalled", "1 broken",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health page missing %q from the sweep findings:\n%s", want, body)
		}
	}
}

func TestHandleHealth_NoSweepStatusFileRendersNoSection(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)
	if strings.Contains(rec.Body.String(), "daemon diagnostics") {
		t.Error("no SweepStatusFile set (surface_board off / unwired) — the section must not render")
	}
}

func TestHandleHealth_ZeroValueServerNeverPanics(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHandleBoard_StatusAndOwnerFiltersCombineWithAnd(t *testing.T) {
	queueRoot := t.TempDir()
	write := func(lane, status, name, owner string) {
		mustWriteFile(t, filepath.Join(queueRoot, lane, status, name),
			"---\ntitle: "+name+"\nowner: "+owner+"\n---\n\nbody\n")
	}
	write("devops", "pending", "a.md", "alice")
	write("devops", "done", "b.md", "alice")
	write("qa", "pending", "c.md", "bob")

	s := &Server{Root: queueRoot}
	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		rec := httptest.NewRecorder()
		s.handleBoard(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s: status = %d", target, rec.Code)
		}
		return rec.Body.String()
	}

	all := get("/")
	for _, want := range []string{"a.md", "b.md", "c.md"} {
		if !strings.Contains(all, want) {
			t.Errorf("unfiltered board missing %q:\n%s", want, all)
		}
	}

	filtered := get("/?status=pending&owner=alice")
	if !strings.Contains(filtered, "a.md") {
		t.Errorf("want a.md (pending, alice) in the filtered board:\n%s", filtered)
	}
	if strings.Contains(filtered, "b.md") {
		t.Error("b.md is done, status=pending must exclude it")
	}
	if strings.Contains(filtered, "c.md") {
		t.Error("c.md is owned by bob, owner=alice must exclude it")
	}
}

func TestHandleBoard_ChipsReflectActiveFilters(t *testing.T) {
	queueRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(queueRoot, "devops", "pending", "a.md"),
		"---\ntitle: a\nowner: alice\n---\n\nbody\n")

	s := &Server{Root: queueRoot}
	req := httptest.NewRequest("GET", "/?lane=devops", nil)
	rec := httptest.NewRecorder()
	s.handleBoard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="chip active" href="/"`) {
		t.Errorf("want the active devops lane chip to link to / (toggle off), got:\n%s", body)
	}
}

// TestHandleBoard_GroupsLegacyLanesUnderCanonicalRole is role-keyed-lanes.md's
// viewer requirement: a mixed queue's legacy instance lane ("cr-1") is labelled
// with the role it folds to, a project lane declared via KnownRoles ("ci-infra-1")
// folds to ITS declared role, an already-role-keyed lane ("code-review") renders
// plain, and a genuinely un-mappable ephemeral lane ("fix-1161") is tagged as
// non-role/legacy — while every lane stays independently filterable by its raw
// name (filtering behaviour must be unaffected by the display change).
func TestHandleBoard_GroupsLegacyLanesUnderCanonicalRole(t *testing.T) {
	queueRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(queueRoot, "code-review", "pending", "a.md"), "---\ntitle: a\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "cr-1", "pending", "b.md"), "---\ntitle: b\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "ci-infra-1", "pending", "c.md"), "---\ntitle: c\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "fix-1161", "pending", "d.md"), "---\ntitle: d\n---\n\nbody\n")

	s := &Server{Root: queueRoot, KnownRoles: []string{"ci-infra"}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleBoard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`>code-review<`,                  // already a role lane — plain
		`>cr-1 → code-review<`,           // legacy instance -> its role
		`>ci-infra-1 → ci-infra<`,        // project-declared owner instance -> its role
		`>fix-1161 (non-role / legacy)<`, // un-mappable ephemeral -> flagged group
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing lane chip label %q, got:\n%s", want, body)
		}
	}

	// Filtering still keys on the RAW lane name, unaffected by the label.
	req2 := httptest.NewRequest("GET", "/?lane=cr-1", nil)
	rec2 := httptest.NewRecorder()
	s.handleBoard(rec2, req2)
	filtered := rec2.Body.String()
	if !strings.Contains(filtered, "b.md") {
		t.Errorf("lane=cr-1 should still match the raw cr-1 lane's task, got:\n%s", filtered)
	}
	if strings.Contains(filtered, "a.md") || strings.Contains(filtered, "c.md") || strings.Contains(filtered, "d.md") {
		t.Errorf("lane=cr-1 should exclude other lanes' tasks, got:\n%s", filtered)
	}
}

// renderBoard is a small helper for the facet tests below.
func renderBoard(t *testing.T, s *Server, target string) string {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	rec := httptest.NewRecorder()
	s.handleBoard(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s: status = %d, want 200", target, rec.Code)
	}
	return rec.Body.String()
}

// TestHandleBoard_OwnerFacetBucketsByCanonicalRole is board-role-facets-hide-
// empty.md Change 1: many raw owner instances fold to a FEW role chips (the #123
// declutter). Owners [ci-infra, ci-infra-agent, owner-479a, devops] with roster
// [ci-infra, devops] produce owner chips {ci-infra, devops, non-role/legacy} —
// NOT one chip per raw instance. ci-infra + ci-infra-agent share the single
// ci-infra chip; owner-479a (no role) collapses into non-role/legacy.
func TestHandleBoard_OwnerFacetBucketsByCanonicalRole(t *testing.T) {
	queueRoot := t.TempDir()
	write := func(name, owner string) {
		mustWriteFile(t, filepath.Join(queueRoot, "backend", "pending", name),
			"---\ntitle: "+name+"\nowner: "+owner+"\n---\n\nbody\n")
	}
	write("a.md", "ci-infra")
	write("b.md", "ci-infra-agent") // -agent fold → ci-infra
	write("c.md", "owner-479a")     // no known role → non-role/legacy
	write("d.md", "devops")

	s := &Server{Root: queueRoot, KnownRoles: []string{"ci-infra", "devops"}}
	body := renderBoard(t, s, "/")

	// Role-bucketed owner chips present (one per canonical role + the legacy bucket).
	for _, bucket := range []string{"ci-infra", "devops"} {
		href := `href="` + filterHref("", "", bucket) + `"`
		if !strings.Contains(body, href) {
			t.Errorf("owner facet missing role bucket chip %q (%s):\n%s", bucket, href, body)
		}
	}
	// The legacy bucket chip carries the legacyLaneGroup label (its href %-encodes
	// the space/slash, so assert on the escape-free chip text instead).
	if !strings.Contains(body, `>`+legacyLaneGroup+`<`) {
		t.Errorf("owner facet missing the %q bucket chip:\n%s", legacyLaneGroup, body)
	}
	// The raw instance owners must NOT each get their own chip.
	for _, raw := range []string{"ci-infra-agent", "owner-479a"} {
		href := `href="` + filterHref("", "", raw) + `"`
		if strings.Contains(body, href) {
			t.Errorf("owner facet rendered a per-instance chip for %q — must fold to its role bucket:\n%s", raw, body)
		}
	}

	// Clicking the ci-infra bucket filters to EVERY owner that folds to ci-infra.
	folded := renderBoard(t, s, "/?owner=ci-infra")
	if !strings.Contains(folded, "a.md") || !strings.Contains(folded, "b.md") {
		t.Errorf("owner=ci-infra should match both ci-infra and ci-infra-agent tasks:\n%s", folded)
	}
	if strings.Contains(folded, "c.md") || strings.Contains(folded, "d.md") {
		t.Errorf("owner=ci-infra should exclude owners folding to other buckets:\n%s", folded)
	}
	// Raw owner filtering is still preserved (individual filterability, cheap).
	raw := renderBoard(t, s, "/?owner=owner-479a")
	if !strings.Contains(raw, "c.md") {
		t.Errorf("raw owner filter owner=owner-479a should still match its task:\n%s", raw)
	}
	if strings.Contains(raw, "a.md") {
		t.Errorf("raw owner filter owner=owner-479a should exclude other owners:\n%s", raw)
	}
}

// TestHandleBoard_HidesEmptyChips is Change 2: a facet value with zero LIVE
// (non-done) tasks renders no chip. An emptied lane (only done tasks) and an
// owner role bucket whose tasks are all done both vanish, while a lane/owner with
// a live task keeps its chip — no store edit needed.
func TestHandleBoard_HidesEmptyChips(t *testing.T) {
	queueRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(queueRoot, "gone", "done", "x.md"),
		"---\ntitle: x\nowner: ci-infra\n---\n\nbody\n") // lane gone: 0 live; owner ci-infra: 0 live
	mustWriteFile(t, filepath.Join(queueRoot, "active", "pending", "y.md"),
		"---\ntitle: y\nowner: devops\n---\n\nbody\n") // lane active: 1 live; owner devops: 1 live

	s := &Server{Root: queueRoot, KnownRoles: []string{"ci-infra", "devops"}}
	body := renderBoard(t, s, "/")

	// Empty (done-only) lane + owner bucket are hidden.
	if strings.Contains(body, `href="`+filterHref("gone", "", "")+`"`) {
		t.Errorf("lane 'gone' has 0 live tasks — its chip must be hidden:\n%s", body)
	}
	if strings.Contains(body, `href="`+filterHref("", "", "ci-infra")+`"`) {
		t.Errorf("owner bucket 'ci-infra' has 0 live tasks — its chip must be hidden:\n%s", body)
	}
	// Live lane + owner bucket keep their chips.
	if !strings.Contains(body, `href="`+filterHref("active", "", "")+`"`) {
		t.Errorf("lane 'active' has 1 live task — its chip must be present:\n%s", body)
	}
	if !strings.Contains(body, `href="`+filterHref("", "", "devops")+`"`) {
		t.Errorf("owner bucket 'devops' has 1 live task — its chip must be present:\n%s", body)
	}
	// An empty status column (nothing blocked) hides its chip too.
	if strings.Contains(body, `href="`+filterHref("", "blocked", "")+`"`) {
		t.Errorf("status 'blocked' has 0 tasks — its chip must be hidden:\n%s", body)
	}
	if !strings.Contains(body, `href="`+filterHref("", "pending", "")+`"`) {
		t.Errorf("status 'pending' has a task — its chip must be present:\n%s", body)
	}
}

// TestHandleBoard_ActiveFilterChipStaysVisibleAtZeroLive is Change 2's guard: a
// chip the user is CURRENTLY filtering on stays visible even at 0 live tasks, so
// the active filter is always clearable.
func TestHandleBoard_ActiveFilterChipStaysVisibleAtZeroLive(t *testing.T) {
	queueRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(queueRoot, "gone", "done", "x.md"),
		"---\ntitle: x\nowner: ci-infra\n---\n\nbody\n")

	s := &Server{Root: queueRoot, KnownRoles: []string{"ci-infra"}}

	// Filtering on the done-only lane keeps its (otherwise-hidden) chip visible &
	// active. The active chip's href toggles the filter OFF (links to "/"), so the
	// active filter is always clearable.
	laneBody := renderBoard(t, s, "/?lane=gone")
	if !strings.Contains(laneBody, `class="chip active" href="/"`) {
		t.Errorf("active lane filter 'gone' (0 live) must stay visible as a clearable active chip:\n%s", laneBody)
	}
	if !strings.Contains(laneBody, `>gone (`+legacyLaneGroup+`)<`) {
		t.Errorf("the active lane 'gone' chip must render with its role label:\n%s", laneBody)
	}

	// Same for an owner role bucket whose live count is 0.
	ownerBody := renderBoard(t, s, "/?owner=ci-infra")
	if !strings.Contains(ownerBody, `class="chip active" href="/"`) {
		t.Errorf("active owner filter 'ci-infra' (0 live) must stay visible as a clearable active chip:\n%s", ownerBody)
	}
}

// TestHandleBoard_LaneFacetHidesEmptyEphemeralKeepsRoleLabel is Change 3: the
// lane facet keeps its existing role LABEL (laneChipLabel) but also hides empty —
// an ephemeral lane with 0 live tasks (_gvtest-crit) is hidden, while a role lane
// with live work is shown with its role label. The lane facet is NOT itself
// role-bucketed (per-raw-lane chips stay, preserving raw-lane filterability); only
// hide-empty + the role label are added.
func TestHandleBoard_LaneFacetHidesEmptyEphemeralKeepsRoleLabel(t *testing.T) {
	queueRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(queueRoot, "_gvtest-crit", "done", "x.md"), "---\ntitle: x\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "code-review", "pending", "y.md"), "---\ntitle: y\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "ci-infra-1", "pending", "z.md"), "---\ntitle: z\n---\n\nbody\n")

	s := &Server{Root: queueRoot, KnownRoles: []string{"ci-infra"}}
	body := renderBoard(t, s, "/")

	// The lane CHIP is gone (the done task's card may still name its lane in the
	// collapsed done column — hide-empty is about the filter chips, not the column).
	if strings.Contains(body, `href="`+filterHref("_gvtest-crit", "", "")+`"`) {
		t.Errorf("ephemeral lane '_gvtest-crit' (0 live) must have no filter chip:\n%s", body)
	}
	if !strings.Contains(body, `>code-review<`) {
		t.Errorf("role lane 'code-review' (live) must be shown with its plain role label:\n%s", body)
	}
	if !strings.Contains(body, `>ci-infra-1 → ci-infra<`) {
		t.Errorf("instance lane 'ci-infra-1' (live) must be shown with its role label:\n%s", body)
	}
}
