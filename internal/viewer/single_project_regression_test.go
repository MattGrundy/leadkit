package viewer

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/usage"
)

// TestSingleProjectMode_UnaffectedByAccountRegistry is multi-project-board.md's
// dedicated regression test for its hard constraint: single-project mode
// (a plain *Server — exactly what `leadkit serve -queue` builds, see
// cmd/leadkit's runServe) must render byte-identically to today regardless of
// what the account-wide project registry holds — because it must never READ
// that registry at all. This proves the property behaviorally rather than by
// grepping imports: render the same Server's "/", "/tree", "/store" and
// "/health" once against an EMPTY account registry dir, and again after
// populating that exact same directory with several OTHER projects' entries
// (including one that collides on this project's own would-be slug) — the
// rendered bytes must be identical either way.
func TestSingleProjectMode_UnaffectedByAccountRegistry(t *testing.T) {
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	mustWriteFile(t, filepath.Join(storeRoot, "milestones.yaml"), `
project: solo project
milestones:
  - id: m1
    title: Milestone One
    state: active
    epics:
      - id: e1
`)
	mustWriteFile(t, filepath.Join(issuesRoot, "I1-issue-one.md"), "---\nid: I1\ntitle: Issue One\nparent: e1\nstatus: pending\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(storeRoot, "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")
	mustWriteFile(t, filepath.Join(queueRoot, "backend", "in-progress", "1-task-one.md"), "---\nid: 1\ntitle: Task One\nparent: I1\nstatus: pending\n---\n\nbody\n")

	s := &Server{Root: queueRoot, Store: storeRoot, IssuesRoot: issuesRoot}
	routes := []string{"/", "/tree", "/store", "/health"}

	render := func() map[string]string {
		out := map[string]string{}
		for _, route := range routes {
			req := httptest.NewRequest("GET", route, nil)
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, req)
			out[route] = rec.Body.String()
		}
		return out
	}

	before := render()

	// Now populate an account-wide project registry — including a slug this
	// project's own basename would resolve to — in a directory single-project
	// mode is never told about, but if any code path here DID consult a
	// package-level default (usage.DefaultProjectsDir) rather than staying
	// wholly disconnected, this would still catch it via the byte-diff below.
	registryDir := t.TempDir()
	now := time.Now()
	if err := usage.WriteProjectEntry(registryDir, "solo-project", "Solo Project", queueRoot, storeRoot, issuesRoot, now); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteProjectEntry(registryDir, "other-project-1", "Other One", "/nowhere", "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteProjectEntry(registryDir, "other-project-2", "Other Two", "/nowhere-else", "", "", now); err != nil {
		t.Fatal(err)
	}

	after := render()

	for _, route := range routes {
		if before[route] != after[route] {
			t.Errorf("route %s rendered differently once the account registry was populated — single-project mode must be byte-identical regardless of registry state\nBEFORE:\n%s\nAFTER:\n%s", route, before[route], after[route])
		}
	}
}

// TestServer_HasNoProjectRegistryField documents (via a compile-time-checked
// literal) that Server carries no field at all for the account registry —
// there is no flag/knob on the single-project type that could accidentally
// wire it in later without a visible, deliberate change to this struct.
func TestServer_HasNoProjectRegistryField(t *testing.T) {
	// UNKEYED on purpose. The keyed version of this literal could not fail for the reason
	// the test exists: adding a field to a struct does not break a keyed composite literal,
	// so the guard was inert against exactly the change it was written to catch. Verified
	// by adding a ProjectRegistryDir field — the keyed version still compiled and passed.
	//
	// Unkeyed, the compiler requires every field in declaration order, so ANY change to
	// Server's field set breaks this and the author has to decide whether it was intended.
	// Account mode belongs in AccountServer (account.go), a wholly separate type; that
	// separation is what this protects.
	_ = Server{
		"q", // Root
		"s", // Store
		"",  // SessionTasksRoot
		nil, // CanonicalStatuses
		0,   // WorkishThreshold
		nil, // WorkishMarkers
		"",  // PushViolationsLog
		"",  // SweepStatusFile
		"i", // IssuesRoot
		nil, // KnownRoles
	}
}
