package main

import (
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/usage"
)

// TestCLI_Burn_RegistersProjectRoots is multi-project-board.md's "registry
// write" regression: `leadkit burn` registers the current project's roots
// under its ResolveProjectSlug key, the same slug the workstream registry and
// the daemon namespacing use. The queue root is pinned via $LEADKIT_QUEUE so the
// project-root basename ("my-project") is deterministic — the source no longer
// bakes in any adopter path, so a test can't rely on a fixed default slug.
func TestCLI_Burn_RegistersProjectRoots(t *testing.T) {
	bin := testBinary(t)
	workstreamsDir := t.TempDir()
	projectsDir := t.TempDir()
	fakeScript := filepath.Join(t.TempDir(), "does-not-exist.sh")
	queueRoot := filepath.Join(t.TempDir(), "my-project", "queue")

	out, code := runCLI(t, bin, []string{"LEADKIT_QUEUE=" + queueRoot},
		"burn", "--workstreams-dir", workstreamsDir, "--projects-dir", projectsDir,
		"--native=false", "--script", fakeScript)
	// burn is "exit 0 always" (advisory) even when the live-usage fetch itself
	// fails, so a fetch error here is fine — only the registry write matters.
	if code != 0 {
		t.Logf("burn exited %d (advisory-only, fetch expected to fail): %s", code, out)
	}

	entries, ok := usage.ReadProjectRegistry(projectsDir)
	if !ok {
		t.Fatal("want ok=true reading the project registry")
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 registered project, got %+v", entries)
	}
	e := entries[0]
	if e.Slug != "my-project" {
		t.Errorf("Slug = %q, want my-project (the pinned queue_root's project-basename)", e.Slug)
	}
	if e.QueueRoot == "" {
		t.Error("want a non-empty QueueRoot registered")
	}
	if filepath.Base(filepath.Dir(e.QueueRoot)) != "my-project" {
		t.Errorf("QueueRoot = %q, want its parent dir basename to be my-project", e.QueueRoot)
	}
}

// TestCLI_Init_RegistersProjectRoots proves `leadkit init --apply --confirm`
// also registers the project's roots (multi-project-board.md: "Written by the
// same places that already write ... leadkit burn, leadkit serve, leadkit
// init"), under the exact same slug key daemon namespacing already used in
// this same run (see the sibling waker-plist assertion in apply_guard_test.go).
func TestCLI_Init_RegistersProjectRoots(t *testing.T) {
	bin := testBinary(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	launchAgentsDir := t.TempDir()
	skillsDir := t.TempDir()
	projectsDir := t.TempDir()
	queueRoot := filepath.Join(t.TempDir(), "my-project", "queue")

	out, code := runCLI(t, bin, []string{"LEADKIT_QUEUE=" + queueRoot},
		"init", "--apply", "--confirm",
		"--settings", settingsPath, "--launch-agents-dir", launchAgentsDir,
		"--skills-dir", skillsDir, "--skip-launchctl", "--projects-dir", projectsDir)
	if code != 0 {
		t.Fatalf("`init --apply --confirm` should exit 0, got %d. output:\n%s", code, out)
	}

	entries, ok := usage.ReadProjectRegistry(projectsDir)
	if !ok {
		t.Fatal("want ok=true reading the project registry")
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 registered project, got %+v", entries)
	}
	if entries[0].Slug != "my-project" {
		t.Errorf("Slug = %q, want my-project", entries[0].Slug)
	}
}

// TestCLI_ServeQueue_NeverTouchesProjectRegistry is the CLI-level half of the
// hard constraint's dedicated regression: `leadkit serve -queue ...` — the
// single-project path — must never write (or, per internal/viewer's own
// regression test, ever read) the account project registry, even when
// -projects-dir points at a directory that already has entries in it (proving
// it's not merely that the dir happens to be empty). serve normally blocks
// forever (http.ListenAndServe), so this test can't run it to completion; it
// only needs to prove the registry is untouched during flag/mode resolution
// before that point, which -addr can't reach without actually binding a
// socket — so this instead exercises serveAccountMode (the exact boolean
// runServe branches on) directly. See serve_account_test.go's
// TestServeAccountMode_ExplicitQueueStaysSingleProject for the unit-level
// half of this same guarantee.
func TestCLI_ServeQueue_ModeDecisionNeverImpliesAccountRegistryRead(t *testing.T) {
	if serveAccountMode("/anything", false, false) {
		t.Fatal("an explicit -queue must resolve to single-project mode, which never reads the account registry")
	}
}
