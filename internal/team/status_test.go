package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatus_CleanRightAfterCapture(t *testing.T) {
	root, cfg := buildFixture(t)
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	queueRoot := filepath.Join(root, "queue")
	mustMkdirAll(t, filepath.Join(queueRoot, "devops"))
	mustMkdirAll(t, filepath.Join(queueRoot, "cr-1"))
	mustMkdirAll(t, filepath.Join(queueRoot, "crit-2"))

	res, err := Status(cfg, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Clean() {
		t.Errorf("want a clean status right after capture (with lane dirs present), got %+v", res)
	}
}

func TestStatus_DetectsAddedChangedRemoved(t *testing.T) {
	root, cfg := buildFixture(t)
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	queueRoot := filepath.Join(root, "queue")

	// Changed: edit a live def after capture.
	mustWrite(t, filepath.Join(cfg.AgentsDir, "lead.md"), "# lead\nUPDATED CONTENT\n")
	// Added: a new live def that was never captured.
	mustWrite(t, filepath.Join(cfg.AgentsDir, "new-agent.md"), "# new\n")
	// Removed: remove a live def that WAS captured.
	if err := os.Remove(filepath.Join(cfg.AgentsDir, "devops-agent.md")); err != nil {
		t.Fatal(err)
	}

	res, err := Status(cfg, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "lead.md" {
		t.Errorf("Changed = %v, want [lead.md]", res.Changed)
	}
	if len(res.Added) != 1 || res.Added[0] != "new-agent.md" {
		t.Errorf("Added = %v, want [new-agent.md]", res.Added)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "devops-agent.md" {
		t.Errorf("Removed = %v, want [devops-agent.md]", res.Removed)
	}
	if res.Clean() {
		t.Error("Clean() should be false when there's drift")
	}
}

func TestStatus_DetectsMissingLane(t *testing.T) {
	root, cfg := buildFixture(t)
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	queueRoot := filepath.Join(root, "queue")
	// Only create 2 of the 3 expected lanes.
	mustMkdirAll(t, filepath.Join(queueRoot, "devops"))
	mustMkdirAll(t, filepath.Join(queueRoot, "cr-1"))
	// crit-2 deliberately absent.

	res, err := Status(cfg, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MissingLanes) != 1 || res.MissingLanes[0] != "crit-2" {
		t.Errorf("MissingLanes = %v, want [crit-2]", res.MissingLanes)
	}
}

func TestStatus_MissingTeamDirIsGraceful(t *testing.T) {
	_, cfg := buildFixture(t)
	// Never captured — cfg.TeamDir doesn't exist.
	res, err := Status(cfg, filepath.Join(t.TempDir(), "queue"))
	if err != nil {
		t.Fatalf("Status must not error on an uncaptured team_dir, got: %v", err)
	}
	if len(res.Added) != 2 {
		t.Errorf("with nothing captured, every live def should read as Added, got %v", res.Added)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
