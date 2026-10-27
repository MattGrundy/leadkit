package hierarchy

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleMilestonesYAML = `
project: Test project

milestones:
  - id: shadow-ci
    title: Shadow CI
    state: in-progress
    epics:
      - {id: dev-portal, state: core-done, note: built dark}
      - remove-legacy-nexus
      - id: quarantine
        state: '?'
        desc: quarantine desc
        depends_on: [dev-portal]
        tasks:
          - {id: allure, note: the mechanism}
          - quarantine-heuristic

  - id: future-work
    title: Future work
    state: backlog
    epics:
      - warp-dev-demo-container-access
`

func mustWriteMilestones(t *testing.T, storeRoot, content string) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParse_BareStringAndMapEpicsBothWork(t *testing.T) {
	root := t.TempDir()
	mustWriteMilestones(t, root, sampleMilestonesYAML)
	model, found, err := Parse(filepath.Join(root, "milestones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("want found=true")
	}
	if model.Project != "Test project" {
		t.Errorf("Project = %q, want %q", model.Project, "Test project")
	}
	if len(model.Milestones) != 2 {
		t.Fatalf("want 2 milestones, got %d", len(model.Milestones))
	}
	ms := model.Milestones[0]
	if len(ms.Epics) != 3 {
		t.Fatalf("want 3 epics under shadow-ci, got %d: %+v", len(ms.Epics), ms.Epics)
	}
	if ms.Epics[0].ID != "dev-portal" || ms.Epics[0].State != "core-done" {
		t.Errorf("epic[0] = %+v, want dev-portal/core-done (map form)", ms.Epics[0])
	}
	if ms.Epics[1].ID != "remove-legacy-nexus" {
		t.Errorf("epic[1] = %+v, want bare-string remove-legacy-nexus", ms.Epics[1])
	}
	q := ms.Epics[2]
	if q.ID != "quarantine" || len(q.Tasks) != 2 {
		t.Fatalf("quarantine epic = %+v, want 2 nested tasks", q)
	}
	if q.Tasks[0].ID != "allure" || q.Tasks[0].Note != "the mechanism" {
		t.Errorf("nested task[0] = %+v, want allure/the mechanism (map form)", q.Tasks[0])
	}
	if q.Tasks[1].ID != "quarantine-heuristic" {
		t.Errorf("nested task[1] = %+v, want bare-string quarantine-heuristic", q.Tasks[1])
	}
}

func TestParse_MissingFileIsNotAnError(t *testing.T) {
	model, found, err := Parse(filepath.Join(t.TempDir(), "milestones.yaml"))
	if err != nil {
		t.Fatalf("want no error for a missing file, got %v", err)
	}
	if found {
		t.Error("want found=false")
	}
	if model.Project != "" {
		t.Errorf("want a zero-value Model, got %+v", model)
	}
}

func TestParse_LegacyGoalFieldStillParsesAsProject(t *testing.T) {
	root := t.TempDir()
	mustWriteMilestones(t, root, "goal: Legacy goal field\nmilestones: []\n")
	model, _, err := Parse(filepath.Join(root, "milestones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if model.Project != "Legacy goal field" {
		t.Errorf("Project = %q, want the deprecated goal: field's value read as Project", model.Project)
	}
}

func TestParse_ProjectWinsOverGoalWhenBothPresent(t *testing.T) {
	root := t.TempDir()
	mustWriteMilestones(t, root, "project: New project field\ngoal: Old goal field\nmilestones: []\n")
	model, _, err := Parse(filepath.Join(root, "milestones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if model.Project != "New project field" {
		t.Errorf("Project = %q, want project: to win over the deprecated goal:", model.Project)
	}
}

func TestModel_KnownIDsIncludesEpicsAndNestedTasks(t *testing.T) {
	root := t.TempDir()
	mustWriteMilestones(t, root, sampleMilestonesYAML)
	model, _, err := Parse(filepath.Join(root, "milestones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	known := model.KnownIDs()
	for _, id := range []string{"dev-portal", "remove-legacy-nexus", "quarantine", "allure", "quarantine-heuristic", "warp-dev-demo-container-access"} {
		if !known[id] {
			t.Errorf("KnownIDs() missing %q", id)
		}
	}
}

func TestParse_EpicAliasesOptionalField(t *testing.T) {
	root := t.TempDir()
	mustWriteMilestones(t, root, `
project: p
milestones:
  - id: m1
    epics:
      - {id: e1, aliases: [20608, 18883, phase-2]}
      - e2
`)
	model, _, err := Parse(filepath.Join(root, "milestones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	epics := model.Milestones[0].Epics
	if len(epics[0].Aliases) != 3 || epics[0].Aliases[0] != "20608" {
		t.Errorf("epic e1 Aliases = %+v, want [20608 18883 phase-2]", epics[0].Aliases)
	}
	if len(epics[1].Aliases) != 0 {
		t.Errorf("bare-string epic e2 should have no aliases, got %+v", epics[1].Aliases)
	}
}
