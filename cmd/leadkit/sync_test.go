package main

// `sync record-id` writes one frontmatter field — every test here goes through
// runCLI's mandatory sandbox for the same reason task create/issue create's own
// tests do. `sync plan` is read-only but sandboxed the same way regardless.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSyncFixture(t *testing.T, root string) (profilePath string) {
	t.Helper()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(issuesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "milestones.yaml"),
		[]byte("project: p\nmilestones:\n  - id: m1\n    title: M1\n    state: pending\n    epics:\n      - id: dev-portal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issuesRoot, "I1-x.md"),
		[]byte("---\nid: I1\ntitle: An issue\nstatus: pending\nparent: dev-portal\ntrackers: {}\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, "1-a.md"),
		[]byte("---\nid: 1\ntitle: A task\nstatus: pending\nparent: I1\ntrackers: {}\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	profilePath = filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nissues_root: " + issuesRoot + "\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath
}

func TestCLI_SyncPlan_EmitsCreateOpsForUnsyncedNodes(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "sync", "plan", "--output", "linear")
	if code != 0 {
		t.Fatalf("sync plan: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "create") {
		t.Errorf("want create ops (nothing has a tracker id yet), got:\n%s", out)
	}
	if !strings.Contains(out, "dev-portal") || !strings.Contains(out, "I1") {
		t.Errorf("want dev-portal epic and issue I1 in the plan, got:\n%s", out)
	}
}

func TestCLI_SyncPlan_DefaultsOutputFromProfileOutputs(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, append(data, []byte("outputs: [linear]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "sync", "plan") // no --output given
	if code != 0 {
		t.Fatalf("want exit 0 using the profile's outputs: [linear], got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "dev-portal") {
		t.Errorf("want the plan to have actually run against the linear adapter, got:\n%s", out)
	}
}

func TestCLI_SyncPlan_NoOutputAndNoProfileOutputsRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "sync", "plan")
	if code == 0 {
		t.Error("want a non-zero exit when neither --output nor the profile's outputs: is set")
	}
}

func TestCLI_SyncPlan_UnknownOutputRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "sync", "plan", "--output", "bogus")
	if code == 0 {
		t.Error("want a non-zero exit for an unknown --output")
	}
}

func TestCLI_SyncRecordID_WritesIntoTempStoreOnlyThenPlanSeesUpdate(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "sync", "record-id", "--output", "linear", "--local", "I1", "--tracker-id", "LIN-1")
	if code != 0 {
		t.Fatalf("record-id: want exit 0, got %d. output:\n%s", code, out)
	}
	data, err := os.ReadFile(filepath.Join(root, "issues", "I1-x.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "trackers: {linear: LIN-1}") {
		t.Fatalf("want trackers: {linear: LIN-1} written, got:\n%s", data)
	}

	out, code = runCLI(t, bin, env, "sync", "plan", "--output", "linear")
	if code != 0 {
		t.Fatalf("sync plan after record-id: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "update") {
		t.Errorf("after recording I1's tracker id, its op should now be update, got:\n%s", out)
	}
}

func TestCLI_SyncRecordID_UnknownLocalIDRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "sync", "record-id", "--output", "linear", "--local", "I999", "--tracker-id", "LIN-1")
	if code == 0 {
		t.Error("want a non-zero exit for an unknown local id")
	}
}

// `sync plan` is mode-agnostic: its output is byte-identical whatever mode the
// env/config would select. Only EXECUTION differs.
func TestCLI_SyncPlan_ByteIdenticalAcrossModes(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	base, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}

	plan := func(envKey string, extraYAML string) string {
		if err := os.WriteFile(profilePath, append(append([]byte{}, base...), []byte(extraYAML)...), 0o644); err != nil {
			t.Fatal(err)
		}
		out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath, "LINEAR_API_KEY=" + envKey}, "sync", "plan", "--output", "linear", "--json")
		if code != 0 {
			t.Fatalf("sync plan: exit %d:\n%s", code, out)
		}
		return out
	}
	ref := plan("", "")
	for name, got := range map[string]string{
		"env key (auto-direct)":   plan("lin_api_x", ""),
		"config direct":           plan("", "linear:\n  sync_mode: direct\n"),
		"config mcp + env key":    plan("lin_api_x", "linear:\n  sync_mode: mcp\n"),
		"config direct + env key": plan("lin_api_x", "linear:\n  sync_mode: direct\n"),
	} {
		if got != ref {
			t.Errorf("%s: plan differs from the default-mode plan\nwant:\n%s\ngot:\n%s", name, ref, got)
		}
	}
}

// Default (no key, no config) is MCP: `sync run` executes nothing itself and
// points at the sync-executor agent — today's behaviour.
func TestCLI_SyncRun_DefaultIsMCP(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath, "LINEAR_API_KEY="}, "sync", "run", "--output", "linear")
	if code != 0 || !strings.Contains(out, "mode=mcp") || !strings.Contains(out, "sync-executor") {
		t.Errorf("default must be mcp (exit 0, executor-agent pointer), got %d:\n%s", code, out)
	}
	if _, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath, "LINEAR_API_KEY=lin_x"}, "sync", "run", "--output", "linear", "--mode", "mcp"); code != 0 {
		t.Error("--mode mcp must force mcp even with a key in the env")
	}
}

// Forced direct with no key anywhere is a LOUD refusal — never a silent MCP
// fallback — and the refusal never names a key value. keychain_service points at
// a service that cannot exist so a developer's real keychain can't satisfy it.
func TestCLI_SyncRun_ForcedDirectWithoutKeyRefuses(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeSyncFixture(t, root)
	data, _ := os.ReadFile(profilePath)
	if err := os.WriteFile(profilePath, append(data, []byte("keychain_service: leadkit-test-no-such-service\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath, "LINEAR_API_KEY="}
	for _, args := range [][]string{
		{"sync", "run", "--output", "linear", "--mode", "direct"},
	} {
		out, code := runCLI(t, bin, env, args...)
		if code == 0 || !strings.Contains(out, "LINEAR_API_KEY") || strings.Contains(out, "mode=mcp") {
			t.Errorf("want a non-zero refusal naming LINEAR_API_KEY with no mcp fallback, got %d:\n%s", code, out)
		}
	}
	// Same via config.
	if err := os.WriteFile(profilePath, append(data, []byte("keychain_service: leadkit-test-no-such-service\nlinear:\n  sync_mode: direct\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, bin, env, "sync", "run", "--output", "linear")
	if code == 0 || strings.Contains(out, "mode=mcp") {
		t.Errorf("config-forced direct without a key must refuse, got %d:\n%s", code, out)
	}
}
