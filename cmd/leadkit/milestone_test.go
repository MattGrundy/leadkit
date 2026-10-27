package main

// End-to-end proof of `leadkit milestone status`/`check` against the ACTUAL binary:
// the GO/BLOCK matrix and, crucially, the CONFIGURABLE mode (block exits non-zero, warn
// exits 0 on the SAME report; resolution flag > profile > default-block). Fully
// hermetic — the fixtures use only tasks + manual DoD conditions, so no gh/CI/gate/git
// is ever touched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMilestoneProject scaffolds a temp project (leadkit.yaml + .leadkit/milestones.yaml
// + tasks/) and returns the profile path to pass as LEADKIT_PROFILE.
func writeMilestoneProject(t *testing.T, profileMode, artifact string, tasks map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".leadkit"))
	mustMkdir(t, filepath.Join(root, "tasks"))

	yaml := "store_root: tasks\nmilestones_file: .leadkit/milestones.yaml\n"
	if profileMode != "" {
		yaml += "milestone:\n  idle_gate_mode: " + profileMode + "\n"
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".leadkit", "milestones.yaml"), []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range tasks {
		if err := os.WriteFile(filepath.Join(root, "tasks", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return profilePath
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// An unmet milestone: a manual condition is unverified by definition (keeps it blocked).
const unmetArtifact = "current_milestone: m1\ndod:\n  - name: tasks-closed\n    type: tasks\n  - name: login-smoke\n    type: manual\n"

// A clean milestone: a single tasks condition where m1 is a KNOWN milestone (it has a
// task, all terminal) → 0 open → GENUINE-zero met → GO. (A bare id that matched NOTHING
// would be unverified, not met — the anti-drift guard; see the hierarchy-rollup tests.)
const cleanArtifactYAML = "current_milestone: m1\ndod:\n  - name: tasks-closed\n    type: tasks\n"

// cleanTasks makes m1 a known milestone with all-terminal tasks (the genuine-zero MET path).
var cleanTasks = map[string]string{"100-done.md": "---\nid: 100\nmilestone: m1\nstatus: completed\n---\ndone\n"}

func TestCLI_MilestoneCheck_ModeBlockVsWarn(t *testing.T) {
	bin := testBinary(t)

	// Unmet milestone, --mode block → exit non-zero.
	prof := writeMilestoneProject(t, "", unmetArtifact, nil)
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + prof}, "milestone", "check", "--mode", "block")
	if code == 0 {
		t.Errorf("--mode block on an unmet milestone must exit non-zero, got 0\n%s", out)
	}
	if !contains(out, "login-smoke") || !contains(out, "BLOCK") {
		t.Errorf("block report must name the blocking signal:\n%s", out)
	}
	blockReport := out

	// SAME unmet milestone, --mode warn → exit 0, SAME blockers named.
	out, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + prof}, "milestone", "check", "--mode", "warn")
	if code != 0 {
		t.Errorf("--mode warn must exit 0, got %d\n%s", code, out)
	}
	if !contains(out, "login-smoke") {
		t.Errorf("warn report must name the SAME blockers:\n%s", out)
	}
	if contains(blockReport, "login-smoke") != contains(out, "login-smoke") {
		t.Error("block and warn must report the same blockers, differing only in exit code")
	}

	// Resolution order: profile idle_gate_mode=warn, no flag → exit 0.
	profWarn := writeMilestoneProject(t, "warn", unmetArtifact, nil)
	_, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profWarn}, "milestone", "check")
	if code != 0 {
		t.Errorf("profile idle_gate_mode=warn (no flag) must exit 0, got %d", code)
	}

	// Flag beats profile: profile warn, --mode block → non-zero.
	_, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profWarn}, "milestone", "check", "--mode", "block")
	if code == 0 {
		t.Error("--mode block must override profile warn (flag > profile), want non-zero")
	}

	// Default is block: no flag, no profile mode → non-zero.
	_, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + prof}, "milestone", "check")
	if code == 0 {
		t.Error("default mode must be block (non-zero) with no flag and no profile mode")
	}
}

func TestCLI_MilestoneCheck_GO_in_either_mode(t *testing.T) {
	bin := testBinary(t)
	prof := writeMilestoneProject(t, "", cleanArtifactYAML, cleanTasks)
	for _, mode := range []string{"block", "warn"} {
		out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + prof}, "milestone", "check", "--mode", mode)
		if code != 0 {
			t.Errorf("met milestone must be GO (exit 0) in mode %s, got %d\n%s", mode, code, out)
		}
		if !contains(out, "GO") {
			t.Errorf("met milestone report should say GO in mode %s:\n%s", mode, out)
		}
	}
}

func TestCLI_MilestoneStatus_RendersAndAbsentDegrades(t *testing.T) {
	bin := testBinary(t)

	// Renders the re-anchor with the open count + per-condition lines.
	prof := writeMilestoneProject(t, "", unmetArtifact, map[string]string{
		"100-a.md": "---\nid: 100\nmilestone: m1\nstatus: in_progress\n---\nwork\n",
	})
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + prof}, "milestone", "status")
	if code != 0 {
		t.Errorf("status must exit 0, got %d\n%s", code, out)
	}
	for _, sub := range []string{"milestone: m1", "open milestone tasks: 1", "login-smoke"} {
		if !contains(out, sub) {
			t.Errorf("status missing %q:\n%s", sub, out)
		}
	}

	// Absent artifact → degrade, NOT a crash, NOT a false GO.
	root := t.TempDir()
	profAbsent := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profAbsent, []byte("store_root: tasks\nmilestones_file: .leadkit/milestones.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profAbsent}, "milestone", "status")
	if code != 0 || !contains(out, "no milestone DoD declared") {
		t.Errorf("absent artifact status = code %d out %q, want 0 + no-milestone notice", code, out)
	}
	// check on absent → block mode (default) → non-zero, not a false GO.
	_, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profAbsent}, "milestone", "check")
	if code == 0 {
		t.Error("check on an absent artifact must NOT be a false GO (default block → non-zero)")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
