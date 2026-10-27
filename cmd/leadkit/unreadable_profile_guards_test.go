package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hook"
)

// Ticket 240. owner-guard and task-status-guard resolve the profile of the file they are about to touch.
// A profile that EXISTS but did not load must not be replaced by the built-in defaults, which have no
// owners_file: a one-line YAML error would switch the boundary off in the mode meant to fail closed. A
// profile that did not load is read leniently and fail-closed; one nothing can be read from refuses;
// and a config that names another origin (a fork) is not this repo's profile and does not refuse.

type brokenWorld struct {
	home, repo string
}

func newBrokenWorld(t *testing.T, profile string) brokenWorld {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LEADKIT_PROFILE", "")
	t.Setenv("CLAUDE_AGENT_NAME", "dev-1")
	repo := filepath.Join(home, "Projects", "own")
	for _, d := range []string{filepath.Join(repo, ".leadkit"), filepath.Join(repo, "tasks")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := brokenWorld{home: home, repo: repo}
	w.write(t, "leadkit.yaml", profile)
	w.write(t, ".leadkit/owners.yaml", "hard_block:\n  - '*.pem'\n")
	return w
}

func (w brokenWorld) write(t *testing.T, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.repo, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w brokenWorld) ownerGuard(t *testing.T, tool string, input map[string]string) hook.Result {
	t.Helper()
	t.Chdir(w.repo)
	payload, err := json.Marshal(map[string]any{"cwd": w.repo, "tool_name": tool, "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return hook.Dispatch("owner-guard", payload, hook.Config{OwnerGuard: buildOwnerGuardConfig(loadProfileOrDefaults())})
}

func (w brokenWorld) writePem(t *testing.T) hook.Result {
	return w.ownerGuard(t, "Write", map[string]string{"file_path": filepath.Join(w.repo, "secret.pem")})
}

const (
	validEnforce  = "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: enforce\n"
	schemaBroken  = "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: enforce\n  push_gate: enforce\n"
	schemaAdvised = "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: advisory\n  push_gate: enforce\n"
	syntaxBroken  = "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards: {default_mode: enforce\n"
)

// THE ticket's reproduction: a hard-blocked write is refused, and stays refused when one schema error
// makes the profile unparseable.
func TestOwnerGuard_AnUnparseableProfileDoesNotSwitchTheBoundaryOff(t *testing.T) {
	// Control: the valid profile denies, so the rows below are about the profile and not the fixture.
	if got := newBrokenWorld(t, validEnforce).writePem(t); got.ExitCode != 2 {
		t.Fatalf("control: a hard-blocked write under a valid enforce profile must deny, got %+v", got)
	}

	// The same profile with one schema error, mode enforce: the lenient read keeps owners_file set and the
	// mode enforce, so the existing "owners policy unreadable" rule denies; the reason names the profile.
	w := newBrokenWorld(t, schemaBroken)
	got := w.writePem(t)
	if got.ExitCode != 2 || !strings.Contains(got.Stderr, "was not accepted") || !strings.Contains(got.Stderr, filepath.Join(w.repo, "leadkit.yaml")) {
		t.Errorf("an unparseable enforce profile must deny and name the profile, got %+v", got)
	}

	// Advisory (read exactly from the schema path) keeps the advisory posture: allowed.
	if got := newBrokenWorld(t, schemaAdvised).writePem(t); got.ExitCode != 0 {
		t.Errorf("an unparseable profile that says advisory leaves the boundary disabled, got %+v", got)
	}

	// Not even the lenient read works (a syntax error): refused in both modes.
	for _, profile := range []string{syntaxBroken, "<<<<<<< HEAD\n" + validEnforce + "=======\n" + schemaAdvised + ">>>>>>> b\n"} {
		w := newBrokenWorld(t, profile)
		got := w.ownerGuard(t, "Write", map[string]string{"file_path": filepath.Join(w.repo, "notes.md")})
		if got.ExitCode != 2 || !strings.Contains(got.Stderr, "was not accepted") {
			t.Errorf("a profile nothing can be read from must refuse even an ordinary write, got %+v for %q", got, profile)
		}
	}

	// No owners_file in a broken profile: there was never a boundary to lose.
	if got := newBrokenWorld(t, "project_id: own\nguards:\n  default_mode: enforce\n  push_gate: enforce\n").writePem(t); got.ExitCode != 0 {
		t.Errorf("a broken profile without owners_file has no boundary, got %+v", got)
	}
}

// A crafted file that defeats the strict parser and reads advisory only through a lookalike key must not
// disarm the guard.
func TestOwnerGuard_ACraftedProfileDoesNotYieldAdvisory(t *testing.T) {
	for name, profile := range map[string]string{
		"lookalike guards key":       "project_id: own\nowners_file: ./.leadkit/owners.yaml\nqueue_root: [1]\nGuards:\n  default_mode: advisory\n",
		"scalar owner_guard":         "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: advisory\n  owner_guard: advisory\n",
		"mode with a trailing blank": "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: 'advisory '\n  push_gate: enforce\n",
	} {
		if got := newBrokenWorld(t, profile).writePem(t); got.ExitCode != 2 {
			t.Errorf("%s: a crafted profile must not disarm the guard, got %+v", name, got)
		}
	}
}

// The two ways out of a broken profile: edit the profile file itself, or the project's bypass sentinel.
func TestOwnerGuard_ABrokenProfileCanBeRepaired(t *testing.T) {
	for name, profile := range map[string]string{"schema error": schemaBroken, "syntax error": syntaxBroken} {
		w := newBrokenWorld(t, profile)
		if got := w.ownerGuard(t, "Edit", map[string]string{"file_path": filepath.Join(w.repo, "leadkit.yaml")}); got.ExitCode != 0 {
			t.Errorf("%s: an edit of the profile file itself must be allowed so it can be repaired, got %+v", name, got)
		}
		if got := w.ownerGuard(t, "Edit", map[string]string{"file_path": filepath.Join(w.repo, "other.md")}); name == "syntax error" && got.ExitCode != 2 {
			t.Errorf("%s: the exception is for the profile file only, got %+v", name, got)
		}
		w.write(t, ".leadkit/bypass", "")
		if got := w.writePem(t); got.ExitCode != 0 {
			t.Errorf("%s: the bypass sentinel must lift it, got %+v", name, got)
		}
	}
}

// A fork: the config names the upstream origin. That is "this profile is not for this repo", determinate,
// so there is no boundary and writes are allowed, with a note that is not the parse-error wording.
func TestOwnerGuard_AConfigForAnotherOriginIsNotRefused(t *testing.T) {
	requireGit(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
	t.Setenv("LEADKIT_PROFILE", "")
	t.Setenv("CLAUDE_AGENT_NAME", "dev-1")
	repo := filepath.Join(t.TempDir(), "own")
	initRepoWithOrigin(t, repo, "https://github.com/fork-owner/own.git")
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	external := "project_id: own\nproject_origin: upstream-owner/own\nowners_file: ./owners.yaml\nguards:\n  default_mode: enforce\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "own.yaml"), []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "owners.yaml"), []byte("hard_block:\n  - '*.pem'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	payload, _ := json.Marshal(map[string]any{"cwd": repo, "tool_name": "Write", "tool_input": map[string]string{"file_path": filepath.Join(repo, "secret.pem")}})
	got := hook.Dispatch("owner-guard", payload, hook.Config{OwnerGuard: buildOwnerGuardConfig(loadProfileOrDefaults())})
	if got.ExitCode != 0 {
		t.Fatalf("a fork whose config names the upstream origin must write freely, got %+v", got)
	}
	if !strings.Contains(got.Stderr, "different origin") || strings.Contains(got.Stderr, "was not accepted") {
		t.Errorf("the allow must say the profile names a different origin, not the parse-error wording, got %q", got.Stderr)
	}
}

// task-status-guard takes the same treatment. The evidence rule applies to the project's own tickets: a
// profile that will not parse must not move its store to the built-in default and stop the rule applying.
func TestTaskStatusGuard_AnUnparseableProfileKeepsTheEvidenceRule(t *testing.T) {
	statusNoEvidence := "---\nid: 1\nstatus: completed\n---\n"
	run := func(w brokenWorld, path string) hook.Result {
		t.Helper()
		t.Chdir(w.repo)
		payload, _ := json.Marshal(map[string]any{"cwd": w.repo, "tool_name": "Write", "tool_input": map[string]string{"file_path": path, "content": statusNoEvidence}})
		return hook.Dispatch("task-status-guard", payload, hook.Config{TaskStatus: buildTaskStatusConfig()})
	}
	ticket := func(w brokenWorld) string { return filepath.Join(w.repo, "docs-tickets", "1.md") }

	for name, tc := range map[string]struct {
		profile string
		denies  bool
	}{
		"valid profile (control)":           {"project_id: own\nstore_root: ./docs-tickets\n", true},
		"schema error, store_root readable": {"project_id: own\nstore_root: ./docs-tickets\nguards:\n  push_gate: enforce\n", true},
		"schema error, store_root a list":   {"project_id: own\nstore_root: [x]\nguards:\n  push_gate: enforce\n", true},
		"syntax error":                      {"project_id: own\nstore_root: ./docs-tickets\nguards: {default_mode: enforce\n", true},
		"unprofiled control":                {"", false},
	} {
		w := newBrokenWorld(t, tc.profile)
		if tc.profile == "" {
			os.Remove(filepath.Join(w.repo, "leadkit.yaml"))
		}
		if err := os.MkdirAll(filepath.Dir(ticket(w)), 0o755); err != nil {
			t.Fatal(err)
		}
		got := run(w, ticket(w))
		if denied := got.ExitCode == 2 || strings.Contains(got.Stdout, "deny"); denied != tc.denies {
			t.Errorf("%s: denied=%v, want %v (%+v)", name, denied, tc.denies, got)
		}
	}
}

// The error may be in a layer the lenient read does not look at (here the user layer). The project file
// alone is then fine and says advisory, which says nothing about what the merged profile holds, so
// nothing is vouched for: the edit is refused.
func TestOwnerGuard_ABrokenLayerBeneathAValidProjectFileIsNotReadAsAdvisory(t *testing.T) {
	w := newBrokenWorld(t, "project_id: own\nowners_file: ./.leadkit/owners.yaml\nguards:\n  default_mode: advisory\n")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(w.home, ".config"))
	layer := filepath.Join(w.home, ".config", "leadkit", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(layer), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := w.ownerGuard(t, "Write", map[string]string{"file_path": filepath.Join(w.repo, "notes.md")}); got.ExitCode != 0 {
		t.Fatalf("control: with no broken layer an ordinary write is allowed, got %+v", got)
	}
	if err := os.WriteFile(layer, []byte("guards: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.ownerGuard(t, "Write", map[string]string{"file_path": filepath.Join(w.repo, "notes.md")}); got.ExitCode != 2 || !strings.Contains(got.Stderr, "was not accepted") {
		t.Errorf("a broken layer must not be read through the project file's advisory, got %+v", got)
	}
}
