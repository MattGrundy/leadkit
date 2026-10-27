package main

// End-to-end guard test for `team install` — same class of risk as init/audit
// --apply (see apply_guard_test.go's package doc for the sandboxing rationale): this
// reuses runCLI, which sandboxes HOME/cwd/LEADKIT_PROFILE unconditionally, so this
// test can never touch the real ~/.claude or a real queue regardless of what it
// forgets to pass explicitly.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_TeamInstall_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()

	// A source team_dir, as if `team capture` had already run.
	teamDir := filepath.Join(root, "team")
	agentsSrc := filepath.Join(teamDir, "agents")
	if err := os.MkdirAll(agentsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsSrc, "lead.md"), []byte("# lead\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "inbox-manifest.yaml"), []byte("lanes:\n  - role: code-review\n    lane: cr-1\n    subdirs: [pending, in-progress, done]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The install DESTINATIONS — a separate, empty "fresh machine" tree.
	destAgentsDir := filepath.Join(root, "dest-agents")
	queueRoot := filepath.Join(root, "queue")

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\n" +
		"team:\n  agents_dir: dest-agents\n  protocol_file: dest-agents/_team-protocol.md\n" +
		"  sop_file: SOP.md\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	before := snapshotFiles(t, root)

	out, code := runCLI(t, bin, env, "team", "install", "--apply")
	if code == 0 {
		t.Errorf("`team install --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line, got:\n%s", out)
	}
	after := snapshotFiles(t, root)
	if len(before) != len(after) {
		t.Fatalf("`team install --apply` alone must not touch anything\nbefore=%v\nafter=%v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("file %q changed after a bare --apply", k)
		}
	}
	if _, err := os.Stat(destAgentsDir); !os.IsNotExist(err) {
		t.Error("`team install --apply` alone must not create the dest agents dir")
	}

	outApplied, codeApplied := runCLI(t, bin, env, "team", "install", "--apply", "--confirm")
	if codeApplied != 0 {
		t.Fatalf("`team install --apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	if _, err := os.Stat(filepath.Join(destAgentsDir, "lead.md")); err != nil {
		t.Error("lead.md should have been installed")
	}
	if _, err := os.Stat(filepath.Join(queueRoot, "cr-1", "pending")); err != nil {
		t.Error("the cr-1 lane's pending/ dir should have been created")
	}
}

// TestCLI_TeamScaffold_BareApplyRefusesAndNeverMutates is `team install`'s sibling
// guard test for `team scaffold` (queue-mode-enforcement item 1) — same
// dry-run-default / bare-apply-refuses / --apply --confirm-mutates shape.
func TestCLI_TeamScaffold_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()

	queueRoot := filepath.Join(root, "queue")
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nroles_path: queue/ROLES\n" +
		"roster:\n  - type: code-review\n    count: 1\n  - type: developer\n    count: 1\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	before := snapshotFiles(t, root)

	out, code := runCLI(t, bin, env, "team", "scaffold", "--apply")
	if code == 0 {
		t.Errorf("`team scaffold --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line, got:\n%s", out)
	}
	// Each lane missing a ROLES row, plus the missing task store, is one item; the wording is "item(s)"
	// because a store is not a lane. The count is asserted here.
	if !strings.Contains(out, "would change 5 item(s)") {
		t.Errorf("the refusal must count the lanes and the store as items, got:\n%s", out)
	}
	after := snapshotFiles(t, root)
	if len(before) != len(after) {
		t.Fatalf("`team scaffold --apply` alone must not touch anything\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Stat(queueRoot); !os.IsNotExist(err) {
		t.Error("`team scaffold --apply` alone must not create the queue root")
	}

	outApplied, codeApplied := runCLI(t, bin, env, "team", "scaffold", "--apply", "--confirm")
	if codeApplied != 0 {
		t.Fatalf("`team scaffold --apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	for _, role := range []string{"code-review", "orchestrator", "finalise"} {
		if _, err := os.Stat(filepath.Join(queueRoot, role, "pending")); err != nil {
			t.Errorf("the %s lane's pending/ dir should have been created", role)
		}
	}
	if !strings.Contains(outApplied, "enqueue the first task") {
		t.Errorf("want the frictionless-path hint printed on success, got:\n%s", outApplied)
	}
}

// TestCLI_TeamStandupScaffold_GuardAndCreatesMissingLane covers `team standup
// --scaffold` (team-standup-scaffold-lanes.md): dry-run default creates nothing but
// lists what it would; bare --scaffold refuses; --scaffold --confirm creates only the
// missing lane and leaves the present one untouched.
func TestCLI_TeamStandupScaffold_GuardAndCreatesMissingLane(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()

	queueRoot := filepath.Join(root, "queue")
	// ci-pipeline present (empty stages), qa absent.
	for _, sub := range []string{"pending", "in-progress", "done"} {
		if err := os.MkdirAll(filepath.Join(queueRoot, "ci-pipeline", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nroles_path: queue/ROLES\n" +
		"roster:\n  - type: ci-pipeline\n    count: 1\n  - type: qa\n    count: 1\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	// Bare --scaffold (no --confirm) follows the init/audit convention: it REFUSES
	// (exit 1), lists exactly what it WOULD create, and creates nothing.
	outDry, codeDry := runCLI(t, bin, env, "team", "standup", "--scaffold")
	if codeDry == 0 {
		t.Errorf("`team standup --scaffold` alone must exit non-zero, got 0:\n%s", outDry)
	}
	if !strings.Contains(outDry, "REFUSED") || !strings.Contains(outDry, "qa") {
		t.Errorf("bare --scaffold must REFUSE and list what it would create (qa):\n%s", outDry)
	}
	if _, err := os.Stat(filepath.Join(queueRoot, "qa")); !os.IsNotExist(err) {
		t.Error("bare --scaffold must not create queue/qa")
	}

	// --scaffold --confirm: creates queue/qa/{...}, ci-pipeline untouched.
	outApply, codeApply := runCLI(t, bin, env, "team", "standup", "--scaffold", "--confirm")
	if codeApply != 0 {
		t.Fatalf("`team standup --scaffold --confirm` should exit 0, got %d:\n%s", codeApply, outApply)
	}
	for _, sub := range []string{"pending", "in-progress", "done"} {
		if _, err := os.Stat(filepath.Join(queueRoot, "qa", sub)); err != nil {
			t.Errorf("queue/qa/%s should have been created", sub)
		}
	}
}

// A roster with no work role has no lane a first task belongs in, so the scaffold says so instead of
// printing a command that would send the task to a reviewer lane.
func TestCLI_TeamScaffold_NoWorkRoleSaysSoInsteadOfPrintingAWrongHint(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nroles_path: queue/ROLES\nroster:\n  - type: code-review\n    count: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath}, "team", "scaffold", "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "enqueue the first task") || strings.Contains(out, "--type code-review") {
		t.Errorf("no first-task command may name a reviewer lane:\n%s", out)
	}
	if !strings.Contains(out, "no work role is declared") {
		t.Errorf("it must say why there is no hint:\n%s", out)
	}
}

// After a successful install, `team validate` must not say the starters are not installed. It used to
// compare the starters with the repo's captured snapshot, a directory install never writes, so the report
// described something unrelated to whether the definitions were installed. This installs into a temp
// agents_dir (the sandbox pins HOME there too) and validates it.
func TestCLI_TeamValidate_ReportsNothingNotInstalledAfterASuccessfulInstall(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\n" +
		"team:\n  agents_dir: installed-agents\n  protocol_file: installed-agents/_team-protocol.md\n" +
		"  sop_file: SOP.md\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	// Before: nothing installed, and validate says so.
	out, _ := runCLI(t, bin, env, "team", "validate")
	if !strings.Contains(out, "not installed, left alone") {
		t.Fatalf("with nothing installed validate must say so:\n%s", out)
	}

	if out, code := runCLI(t, bin, env, "team", "install", "--apply", "--confirm"); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	out, _ = runCLI(t, bin, env, "team", "validate")
	if strings.Contains(out, "not installed, left alone") || !strings.Contains(out, "0 not installed") {
		t.Errorf("after a successful install validate must report zero not installed:\n%s", out)
	}
	if !strings.Contains(out, "installed in "+filepath.Join(root, "installed-agents")) {
		t.Errorf("the report must name the directory it compared with:\n%s", out)
	}
}

// TestCLI_TeamInstall_KeepsACustomisedDefinition_ReplaceExistingOverwritesWithBackup pins the
// adopter-visible half of ticket 135 S0 through the real binary: a differing definition is kept
// byte-for-byte with a "kept yours" report and no .bak, and only the explicit
// --replace-existing replaces it, taking a .bak. The library tests cannot see the flag wiring
// or the post-apply report, which live in runTeamInstall. Sandboxed by runCLI.
func TestCLI_TeamInstall_KeepsACustomisedDefinition_ReplaceExistingOverwritesWithBackup(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()

	shipped := "# lead\nthe shipped definition\n"
	teamDir := filepath.Join(root, "team")
	if err := os.MkdirAll(filepath.Join(teamDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "agents", "lead.md"), []byte(shipped), 0o644); err != nil {
		t.Fatal(err)
	}

	agentsDir := filepath.Join(root, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "# lead\nMY OWN CUSTOM RULES\n"
	leadPath := filepath.Join(agentsDir, "lead.md")
	if err := os.WriteFile(leadPath, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\n" +
		"team:\n  agents_dir: agents\n  protocol_file: agents/_team-protocol.md\n" +
		"  sop_file: SOP.md\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}
	baks := func() []string {
		m, _ := filepath.Glob(filepath.Join(agentsDir, "*.bak-*"))
		return m
	}

	out, code := runCLI(t, bin, env, "team", "install", "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("install --apply --confirm exit %d:\n%s", code, out)
	}
	if got, _ := os.ReadFile(leadPath); string(got) != mine {
		t.Errorf("a customised definition was replaced by a default install: %q", got)
	}
	if !strings.Contains(out, "= kept yours agents/lead.md") {
		t.Errorf("the plan must say lead.md was kept yours, got:\n%s", out)
	}
	if !strings.Contains(out, "kept yours lead.md (exists and differs; not replaced)") {
		t.Errorf("the post-apply report must name the kept file, got:\n%s", out)
	}
	if b := baks(); len(b) != 0 {
		t.Errorf("keeping a file must write no .bak, got %v", b)
	}

	out, code = runCLI(t, bin, env, "team", "install", "--replace-existing", "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("install --replace-existing exit %d:\n%s", code, out)
	}
	if got, _ := os.ReadFile(leadPath); string(got) != shipped {
		t.Errorf("--replace-existing must replace the file with the source, got %q", got)
	}
	b := baks()
	if len(b) != 1 {
		t.Fatalf("--replace-existing must write exactly one .bak for lead.md, got %v\n%s", b, out)
	}
	if got, _ := os.ReadFile(b[0]); string(got) != mine {
		t.Errorf("the .bak must hold the pre-install customisation, got %q", got)
	}
	if !strings.Contains(out, "backed up lead.md") {
		t.Errorf("the apply report must name the backup, got:\n%s", out)
	}
}
