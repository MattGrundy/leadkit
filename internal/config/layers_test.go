package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxUserLayer points the user layer at an empty (nonexistent) dir so these
// tests never pick up a REAL ~/.config/leadkit/config.yaml on the machine running
// them, then optionally writes a fixture user config at the returned path.
func sandboxUserLayer(t *testing.T) (xdgHome string) {
	t.Helper()
	xdgHome = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
	t.Setenv("HOME", t.TempDir()) // isolate the legacy ~/.leadkit/config.yaml fallback too
	return xdgHome
}

func writeUserConfig(t *testing.T, xdgHome, content string) {
	t.Helper()
	dir := filepath.Join(xdgHome, "leadkit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeProject writes root/leadkit.yaml (the project layer) and points
// LEADKIT_PROFILE at it — the simplest way to make Load() find exactly this file.
func writeProject(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, DefaultProfileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)
	return path
}

func writeTeamConfig(t *testing.T, root, content string) string {
	t.Helper()
	path := filepath.Join(root, "team-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_ProjectOnlyIsUnchangedFromToday(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	writeProject(t, root, "queue_root: my-queue\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.QueueRoot != "my-queue" {
		t.Errorf("QueueRoot = %q, want my-queue (no regression: project-only must behave exactly as before)", prof.QueueRoot)
	}
	if prof.WorkspaceModel != DefaultWorkspaceModel {
		t.Errorf("WorkspaceModel = %q, want the default %q", prof.WorkspaceModel, DefaultWorkspaceModel)
	}
}

func TestLoad_NoProfileFoundStillMergesUserLayer(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "workspace_model: worktrees\n")
	t.Setenv("LEADKIT_PROFILE", "")
	chdir(t, t.TempDir())

	prof, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty (no project file)", path)
	}
	if prof.WorkspaceModel != "worktrees" {
		t.Errorf("WorkspaceModel = %q, want worktrees from the user layer even with no project file", prof.WorkspaceModel)
	}
}

func TestLoad_OutputsTeamSetsProjectUnsetTeamWins(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\n")
	writeProject(t, root, "team_config: "+teamPath+"\nqueue_root: q\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Outputs) != 1 || prof.Outputs[0] != "linear" {
		t.Errorf("Outputs = %v, want [linear] from the team layer", prof.Outputs)
	}
}

func TestLoad_OutputsProjectOverridesTeam(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\n")
	writeProject(t, root, "team_config: "+teamPath+"\noutputs: [jira]\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Outputs) != 1 || prof.Outputs[0] != "jira" {
		t.Errorf("Outputs = %v, want [jira] (project overrides the team default)", prof.Outputs)
	}
}

func TestLoad_OutputsTeamLockWinsOverProject(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\nlocked: [outputs]\n")
	writeProject(t, root, "team_config: "+teamPath+"\noutputs: [jira]\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Outputs) != 1 || prof.Outputs[0] != "linear" {
		t.Errorf("Outputs = %v, want [linear] — team locks outputs, so it wins over the project's [jira]", prof.Outputs)
	}
}

func TestLoad_WorkspaceModelUserBeatsProject(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "workspace_model: worktrees\n")
	root := t.TempDir()
	writeProject(t, root, "workspace_model: clones\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.WorkspaceModel != "worktrees" {
		t.Errorf("WorkspaceModel = %q, want worktrees — preference key: user beats project", prof.WorkspaceModel)
	}
}

func TestLoad_WorkspaceModelTeamLockWinsOverUser(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "workspace_model: worktrees\n")
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "workspace_model: clones\nlocked: [workspace_model]\n")
	writeProject(t, root, "team_config: "+teamPath+"\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.WorkspaceModel != "clones" {
		t.Errorf("WorkspaceModel = %q, want clones — a team lock beats even the user's own preference", prof.WorkspaceModel)
	}
}

func TestLoadWithOrigin_ReportsLayerAndLock(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "workspace_model: worktrees\n")
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\nlocked: [outputs]\n")
	writeProject(t, root, "team_config: "+teamPath+"\nqueue_root: q\n")

	_, _, origins, err := LoadWithOrigin()
	if err != nil {
		t.Fatal(err)
	}
	if o := origins["queue_root"]; o.Layer != "project" || o.Locked {
		t.Errorf("queue_root origin = %+v, want project/unlocked", o)
	}
	if o := origins["outputs"]; o.Layer != "team" || !o.Locked {
		t.Errorf("outputs origin = %+v, want team/locked", o)
	}
	if o := origins["workspace_model"]; o.Layer != "user" {
		t.Errorf("workspace_model origin = %+v, want user", o)
	}
}

func TestLoad_MissingUserAndTeamFilesTolerated(t *testing.T) {
	sandboxUserLayer(t) // points XDG/HOME at empty temp dirs — neither file exists
	root := t.TempDir()
	writeProject(t, root, "team_config: "+filepath.Join(root, "does-not-exist.yaml")+"\nqueue_root: q\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatalf("a missing user/team file must be tolerated, not an error: %v", err)
	}
	if prof.QueueRoot != "q" {
		t.Errorf("QueueRoot = %q, want q", prof.QueueRoot)
	}
}

func TestFormatShowOrigin_AnnotatesLayerAndLock(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\nlocked: [outputs]\n")
	writeProject(t, root, "team_config: "+teamPath+"\nqueue_root: q\n")

	prof, path, origins, err := LoadWithOrigin()
	if err != nil {
		t.Fatal(err)
	}
	out, err := FormatShowOrigin(prof, path, origins)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "queue_root: q  # layer=project") {
		t.Errorf("want queue_root annotated as project, got:\n%s", out)
	}
	if !strings.Contains(out, "layer=team, locked") {
		t.Errorf("want outputs annotated as team/locked, got:\n%s", out)
	}
}

func TestLoad_BurnSessionReserveUserWinsOverProject(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "burn:\n  session_reserve: 25\n")
	root := t.TempDir()
	writeProject(t, root, "burn:\n  session_reserve: 5\n  op_rate_warn: 999\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.Burn.SessionReserve != 25 {
		t.Errorf("Burn.SessionReserve = %v, want 25 — the user's personal budget wins over the project's", prof.Burn.SessionReserve)
	}
	if prof.Burn.OpRateWarn != 999 {
		t.Errorf("Burn.OpRateWarn = %v, want 999 — a normal (non-preference) burn field must still resolve project-wins", prof.Burn.OpRateWarn)
	}
}

func TestLoad_BurnSessionReserveTeamLockWinsOverUser(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "burn:\n  session_reserve: 25\n")
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "burn:\n  session_reserve: 5\nlocked: [burn.session_reserve]\n")
	writeProject(t, root, "team_config: "+teamPath+"\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.Burn.SessionReserve != 5 {
		t.Errorf("Burn.SessionReserve = %v, want 5 — a team lock beats even the user's own preference", prof.Burn.SessionReserve)
	}
}

func TestLoad_BriefLengthMaxAndMessagingModelResolveProjectTeamUser(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "brief_length_max: 2000\nmessaging_model: message\n")
	writeProject(t, root, "team_config: "+teamPath+"\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.BriefLengthMax != 2000 {
		t.Errorf("BriefLengthMax = %d, want 2000 from the team layer (project doesn't set it)", prof.BriefLengthMax)
	}
	if prof.MessagingModel != "message" {
		t.Errorf("MessagingModel = %q, want message from the team layer", prof.MessagingModel)
	}
}

func TestLoad_BriefLengthMaxProjectOverridesTeam(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "brief_length_max: 2000\n")
	writeProject(t, root, "team_config: "+teamPath+"\nbrief_length_max: 1000\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.BriefLengthMax != 1000 {
		t.Errorf("BriefLengthMax = %d, want 1000 (project overrides team)", prof.BriefLengthMax)
	}
}

func TestLoad_BriefLengthMaxDefaultWhenUnset(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	writeProject(t, root, "queue_root: q\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.BriefLengthMax != DefaultBriefLengthMax {
		t.Errorf("BriefLengthMax = %d, want the default %d", prof.BriefLengthMax, DefaultBriefLengthMax)
	}
	if prof.MessagingModel != DefaultMessagingModel {
		t.Errorf("MessagingModel = %q, want the default %q", prof.MessagingModel, DefaultMessagingModel)
	}
}

func TestLoad_LinearConfigParsesAndResolvesProjectTeamUser(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "linear:\n  team_id: CI\n  closed_state: Archived\n")
	writeProject(t, root, "team_config: "+teamPath+"\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.Linear.TeamID != "CI" {
		t.Errorf("Linear.TeamID = %q, want CI from the team layer", prof.Linear.TeamID)
	}
	if prof.Linear.ClosedState != "Archived" {
		t.Errorf("Linear.ClosedState = %q, want Archived (team override)", prof.Linear.ClosedState)
	}
	if prof.Linear.DefaultState != DefaultLinearDefaultState {
		t.Errorf("Linear.DefaultState = %q, want the default %q (team didn't set it)", prof.Linear.DefaultState, DefaultLinearDefaultState)
	}
}

func TestLoad_LinearConfigProjectOverridesTeam(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "linear:\n  team_id: TEAM-DEFAULT\n")
	writeProject(t, root, "team_config: "+teamPath+"\nlinear:\n  team_id: PROJECT-OVERRIDE\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if prof.Linear.TeamID != "PROJECT-OVERRIDE" {
		t.Errorf("Linear.TeamID = %q, want PROJECT-OVERRIDE", prof.Linear.TeamID)
	}
}

func TestLoad_OutputsLockAlsoLocksLinearBlockInPractice(t *testing.T) {
	// outputs: [linear] locked means a project can't turn sync off — but
	// linear.team_id ITSELF is a normal (unlocked) key, so a project CAN still
	// override the target team_id even when outputs is locked.
	sandboxUserLayer(t)
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\nlinear:\n  team_id: TEAM-DEFAULT\nlocked: [outputs]\n")
	writeProject(t, root, "team_config: "+teamPath+"\nlinear:\n  team_id: PROJECT-OVERRIDE\n")

	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Outputs) != 1 || prof.Outputs[0] != "linear" {
		t.Errorf("Outputs = %v, want [linear] (locked)", prof.Outputs)
	}
	if prof.Linear.TeamID != "PROJECT-OVERRIDE" {
		t.Errorf("Linear.TeamID = %q, want PROJECT-OVERRIDE — team_id itself isn't locked", prof.Linear.TeamID)
	}
}
