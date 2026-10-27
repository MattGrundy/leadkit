package main

// End-to-end owner-guard path spellings through the real resolvers (the target project's
// owners_file, the session's cwd and HOME), not an injected policy.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hook"
)

func ownerGuardWorld(t *testing.T) (session, target string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LEADKIT_PROFILE", "")
	t.Setenv("CLAUDE_AGENT_NAME", "other-agent")
	session = filepath.Join(home, "Projects", "sess")
	target = filepath.Join(home, "Projects", "targetproj")
	for _, d := range []string{session, filepath.Join(target, ".leadkit"), filepath.Join(target, "tasks"), filepath.Join(target, "docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(session, "leadkit.yaml"), "project_id: sess\n")
	write(filepath.Join(target, "leadkit.yaml"), "project_id: targetproj\nowners_file: .leadkit/owners.yaml\n")
	write(filepath.Join(target, ".leadkit", "owners.yaml"),
		"hard_block:\n  - Projects/targetproj/secrets/**\n  - Projects/targetproj/.env\npaths:\n  - glob: Projects/targetproj/tasks/**\n    owner: tasks-agent\n  - glob: Projects/targetproj/docs/pinned.md\n    owner: tasks-agent\n")
	return session, target
}

func runOwnerGuard(t *testing.T, cwd, tool string, input map[string]string) hook.Result {
	t.Helper()
	t.Chdir(cwd)
	payload, err := json.Marshal(map[string]any{"cwd": cwd, "tool_name": tool, "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return hook.Dispatch("owner-guard", payload, hook.Config{OwnerGuard: buildOwnerGuardConfig(loadProfileOrDefaults())})
}

func TestOwnerGuardEndToEnd_PathSpellingsOfAnOwnedFile(t *testing.T) {
	session, target := ownerGuardWorld(t)
	edit := func(cwd, path string) hook.Result {
		return runOwnerGuard(t, cwd, "Edit", map[string]string{"file_path": path})
	}

	// Controls: each must FIRE, or the rows below prove nothing.
	if got := edit(session, filepath.Join(target, "tasks", "1.md")); got.ExitCode != 2 || !strings.Contains(got.Stderr, "owned by 'tasks-agent'") {
		t.Fatalf("control (absolute, other agent) = %+v, want the ownership DENY", got)
	}
	if got := edit(target, "tasks/1.md"); got.ExitCode != 2 {
		t.Fatalf("control (relative, inside the target) = %+v, want DENY", got)
	}
	t.Setenv("CLAUDE_AGENT_NAME", "tasks-agent")
	if got := edit(session, filepath.Join(target, "tasks", "1.md")); got.ExitCode != 0 {
		t.Fatalf("control (absolute, owner) = %+v, want allow", got)
	}
	t.Setenv("CLAUDE_AGENT_NAME", "other-agent")

	for _, c := range []struct{ name, cwd, path string }{
		{"relative ../ from the session", session, "../targetproj/tasks/1.md"},
		{"absolute with ..", session, session + "/../targetproj/tasks/1.md"},
		{"dot segment inside the target", target, "./tasks/1.md"},
		{"dotdot inside the target", target, "docs/../tasks/1.md"},
		{"absolute dot segment", target, target + "/./tasks/1.md"},
	} {
		got := edit(c.cwd, c.path)
		if got.ExitCode != 2 || !strings.Contains(got.Stderr, "Projects/targetproj/tasks/1.md is owned by 'tasks-agent'") {
			t.Errorf("%s: %q = %+v, want the ownership DENY on the cleaned path", c.name, c.path, got)
		}
	}

	// And the reverse: a `..` out of the owned tree is not owned.
	if got := edit(target, "tasks/../docs/a.md"); got.ExitCode != 0 {
		t.Errorf("tasks/../docs/a.md = %+v, want allow (not in the owned tree)", got)
	}

	// Bash deletion takes the same route, with the target's policy governing a session
	// whose cwd is the target.
	if got := runOwnerGuard(t, target, "Bash", map[string]string{"command": "rm tasks/1.md"}); got.ExitCode != 2 {
		t.Fatalf("control (Bash rm, plain) = %+v, want DENY", got)
	}
	if got := runOwnerGuard(t, target, "Bash", map[string]string{"command": "rm ../targetproj/tasks/1.md"}); got.ExitCode != 2 {
		t.Errorf("Bash rm ../targetproj/tasks/1.md = %+v, want DENY", got)
	}
}

func TestOwnerGuardEndToEnd_HardBlockHoldsForTheLeadThroughDotDot(t *testing.T) {
	session, target := ownerGuardWorld(t)
	t.Setenv("CLAUDE_AGENT_NAME", "lead")
	edit := func(path string) hook.Result {
		return runOwnerGuard(t, session, "Edit", map[string]string{"file_path": path})
	}
	if got := edit(filepath.Join(target, "secrets", "k")); got.ExitCode != 2 || !strings.Contains(got.Stderr, "HARD-BLOCK") {
		t.Fatalf("control (absolute, lead) = %+v, want HARD-BLOCK", got)
	}
	for _, path := range []string{"../targetproj/secrets/k", session + "/../targetproj/secrets/k"} {
		if got := edit(path); got.ExitCode != 2 || !strings.Contains(got.Stderr, "HARD-BLOCK Projects/targetproj/secrets/k") {
			t.Errorf("lead editing %q = %+v, want HARD-BLOCK on the cleaned path", path, got)
		}
	}
}
