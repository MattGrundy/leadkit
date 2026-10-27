package hierarchy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgentDefForResolve(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStoreTaskForResolve(t *testing.T, storeRoot, filename, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(storeRoot, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSubtasksResolved_ExplicitCanonicalTypeWins(t *testing.T) {
	root := t.TempDir()
	storeRoot, queueRoot, agentsDir := filepath.Join(root, "store"), filepath.Join(root, "queue"), filepath.Join(root, "agents")
	writeAgentDefForResolve(t, agentsDir, "devops-agent")
	writeAgentDefForResolve(t, agentsDir, "ci-infra-agent")
	writeStoreTaskForResolve(t, storeRoot, "1-x.md", "id: 1\ntype: devops-agent\nowner: ci-infra")

	subs, err := LoadSubtasksResolved(storeRoot, queueRoot, filepath.Join(root, "ROLES"), agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Type != "devops-agent" {
		t.Fatalf("want explicit canonical type devops-agent to win over owner, got %+v", subs)
	}
}

func TestLoadSubtasksResolved_InfersFromOwnerWhenNoExplicitType(t *testing.T) {
	root := t.TempDir()
	storeRoot, queueRoot, agentsDir := filepath.Join(root, "store"), filepath.Join(root, "queue"), filepath.Join(root, "agents")
	writeAgentDefForResolve(t, agentsDir, "ci-infra-agent")
	writeStoreTaskForResolve(t, storeRoot, "1-x.md", "id: 1\nowner: ci-infra")

	subs, err := LoadSubtasksResolved(storeRoot, queueRoot, filepath.Join(root, "ROLES"), agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Type != "ci-infra-agent" {
		t.Fatalf("want type inferred from owner ci-infra -> ci-infra-agent, got %+v", subs)
	}
}

func TestLoadSubtasksResolved_InfersFromQueueOverlayLane(t *testing.T) {
	root := t.TempDir()
	storeRoot, queueRoot, agentsDir := filepath.Join(root, "store"), filepath.Join(root, "queue"), filepath.Join(root, "agents")
	writeAgentDefForResolve(t, agentsDir, "devops-agent")
	writeStoreTaskForResolve(t, storeRoot, "1-x.md", "id: 1") // no type, no owner

	// Overlay placement: queueRoot/devops/pending/1-x.md -> the real store file —
	// readOverlayPlacements reads the lane as two directories up from the symlink.
	laneDir := filepath.Join(queueRoot, "devops", "pending")
	if err := os.MkdirAll(laneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(storeRoot, "1-x.md"), filepath.Join(laneDir, "1-x.md")); err != nil {
		t.Fatal(err)
	}

	subs, err := LoadSubtasksResolved(storeRoot, queueRoot, filepath.Join(root, "ROLES"), agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Type != "devops-agent" {
		t.Fatalf("want type inferred from queue overlay lane devops -> devops-agent, got %+v", subs)
	}
}

func TestLoadSubtasksResolved_UnresolvableLeavesTypeEmpty(t *testing.T) {
	root := t.TempDir()
	storeRoot, queueRoot, agentsDir := filepath.Join(root, "store"), filepath.Join(root, "queue"), filepath.Join(root, "agents")
	writeAgentDefForResolve(t, agentsDir, "devops-agent")
	writeStoreTaskForResolve(t, storeRoot, "1-x.md", "id: 1\nowner: unassigned")

	subs, err := LoadSubtasksResolved(storeRoot, queueRoot, filepath.Join(root, "ROLES"), agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Type != "" {
		t.Fatalf("an unresolvable owner must leave Type empty (manual), not guess, got %+v", subs)
	}
}
