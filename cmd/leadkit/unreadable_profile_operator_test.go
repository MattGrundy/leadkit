package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hook"
)

// The grant keys are the operator's when an external config exists for the repo, which is why a committed
// file cannot disarm them. A committed profile that does not LOAD must not either: reading the committed
// file for owners_file found none, and one key of the wrong shape switched the operator's boundary off.
func TestOwnerGuard_ABrokenCommittedProfileDoesNotDisarmTheOperatorsBoundary(t *testing.T) {
	requireGit(t)
	setup := func(t *testing.T, committed string, operator bool) (repo string, write func(path string) hook.Result) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
		t.Setenv("LEADKIT_PROFILE", "")
		t.Setenv("CLAUDE_AGENT_NAME", "dev-1")
		repo = filepath.Join(t.TempDir(), "own")
		initRepoWithOrigin(t, repo, "https://github.com/acme/own.git")
		if committed != "" {
			if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte(committed), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if operator {
			cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
			owners := filepath.Join(cfgDir, "owners.yaml")
			if err := os.WriteFile(owners, []byte("hard_block:\n  - '*.pem'\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			external := "project_id: own\nproject_origin: acme/own\nowners_file: " + owners + "\nguards:\n  default_mode: enforce\n"
			if err := os.WriteFile(filepath.Join(cfgDir, "own.yaml"), []byte(external), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return repo, func(path string) hook.Result {
			t.Chdir(repo)
			payload, _ := json.Marshal(map[string]any{"cwd": repo, "tool_name": "Write", "tool_input": map[string]string{"file_path": path}})
			return hook.Dispatch("owner-guard", payload, hook.Config{OwnerGuard: buildOwnerGuardConfig(loadProfileOrDefaults())})
		}
	}

	rows := []struct {
		name, committed  string
		denyWithOperator bool
	}{
		{"no committed profile (control)", "", true},
		{"valid profile (control)", "project_id: own\n", true},
		{"valid profile, advisory in the committed file (control)", "project_id: own\nguards:\n  default_mode: advisory\n", true},
		{"schema error", "project_id: own\nguards:\n  push_gate: enforce\n", true},
		{"schema error and an advisory default", "project_id: own\nguards:\n  default_mode: advisory\n  push_gate: enforce\n", true},
		{"schema error and an enforce default", "project_id: own\nguards:\n  default_mode: enforce\n  push_gate: enforce\n", true},
		{"a committed owners_file and a schema error", "project_id: own\nowners_file: ./.leadkit/none.yaml\nguards:\n  push_gate: enforce\n", true},
		{"syntax error", "project_id: own\nguards: {default_mode: enforce\n", true},
		{"nothing readable", "<<<<<<< HEAD\nproject_id: own\n=======\nproject_id: x\n>>>>>>> b\n", true},
	}
	for _, r := range rows {
		repo, write := setup(t, r.committed, true)
		if got := write(filepath.Join(repo, "secret.pem")); (got.ExitCode == 2) != r.denyWithOperator {
			t.Errorf("%s: with the operator's config the hard-blocked write must be denied=%v, got %+v", r.name, r.denyWithOperator, got)
		}
	}

	// Without an operator config the committed file is all there is, as before: a broken file that
	// declares no owners_file has no boundary to lose, and one that declares it keeps it.
	repo, write := setup(t, "project_id: own\nguards:\n  push_gate: enforce\n", false)
	if got := write(filepath.Join(repo, "secret.pem")); got.ExitCode != 0 {
		t.Errorf("no operator config and no owners_file anywhere: nothing to enforce, got %+v", got)
	}
	repo, write = setup(t, "project_id: own\nowners_file: ./o.yaml\nguards:\n  default_mode: enforce\n  push_gate: enforce\n", false)
	if got := write(filepath.Join(repo, "secret.pem")); got.ExitCode != 2 || !strings.Contains(got.Stderr, "was not accepted") {
		t.Errorf("no operator config: the committed owners_file keeps its boundary through the lenient read, got %+v", got)
	}
}

// When the operator's own config is the unreadable one, nothing about the grants can be vouched for and the
// strictest reading applies, whatever the committed file says.
func TestOwnerGuard_AnUnreadableOperatorConfigTakesTheStrictestReading(t *testing.T) {
	requireGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
	t.Setenv("LEADKIT_PROFILE", "")
	t.Setenv("CLAUDE_AGENT_NAME", "dev-1")
	repo := filepath.Join(t.TempDir(), "own")
	initRepoWithOrigin(t, repo, "https://github.com/acme/own.git")
	if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte("project_id: own\nguards:\n  default_mode: advisory\n  push_gate: enforce\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	if err := os.WriteFile(filepath.Join(cfgDir, "own.yaml"), []byte("project_id: own\nproject_origin: acme/own\nguards: {default_mode: enforce\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	payload, _ := json.Marshal(map[string]any{"cwd": repo, "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join(repo, "notes.md")}})
	got := hook.Dispatch("owner-guard", payload, hook.Config{OwnerGuard: buildOwnerGuardConfig(loadProfileOrDefaults())})
	if got.ExitCode != 2 || !strings.Contains(got.Stderr, "was not accepted") {
		t.Errorf("an unreadable operator config must refuse (strictest reading), got %+v", got)
	}
}
