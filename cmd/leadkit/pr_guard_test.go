package main

// `hook pr-guard` is security-sensitive — this CLI-level regression test proves
// the real binary (not just the unit tests) preserves platform-infra' exact deny
// behavior end to end. Only `git remote get-url origin` is exercised for real
// (a local, offline git operation on a throwaway repo) — no gh/glab CLI call, no
// network, no real workspace.
//
// runHookCLI is its own small variant of apply_guard_test.go's runCLI: pr-guard
// needs BOTH a custom working directory (the throwaway git repo, so `git remote
// get-url origin` resolves against ITS origin — runCLI always pins cmd.Dir to
// its own sandboxed HOME) and stdin (the hook JSON payload, which runCLI doesn't
// wire at all). It keeps the SAME mandatory HOME/LEADKIT_PROFILE sandboxing
// runCLI enforces — only cwd and stdin differ.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runHookCLI(t *testing.T, bin, dir string, extraEnv []string, stdin string) (output string, exitCode int) {
	t.Helper()
	home := t.TempDir()

	overrides := map[string]string{
		"HOME":            home,
		"LEADKIT_PROFILE": filepath.Join(home, "leadkit.yaml"), // deliberately absent unless extraEnv overrides it
	}
	for _, kv := range extraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			overrides[k] = v
		}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, replaced := overrides[k]; replaced {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command(bin, "hook", "pr-guard")
	cmd.Dir = dir // the throwaway git repo — NEVER this leadkit repo itself
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %s hook pr-guard: %v\n%s", bin, err, out)
	return "", -1
}

// gitRepoWithOrigin creates a throwaway git repo whose origin remote is originURL
// — enough for `git remote get-url origin` to resolve, without any real network
// access (the remote is never fetched/pushed).
func gitRepoWithOrigin(t *testing.T, originURL string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", originURL)
	return repo
}

func TestCLI_HookPRGuard_CIServicesStillDeniesVerbatim(t *testing.T) {
	bin := testBinary(t)
	repo := gitRepoWithOrigin(t, "git@github.com:acmecorp/platform-infra.git")

	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nauthority:\n  platform-infra:\n    can_push: true\n    can_open_pr: true\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	stdin := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title x"},"session_id":"s1"}`
	out, code := runHookCLI(t, bin, repo, env, stdin)
	if code != 0 {
		t.Fatalf("want exit 0 (deny is via stdout JSON), got %d. output:\n%s", code, out)
	}
	wantSubstr := "Raw `gh pr create` / `glab mr create` is disabled. Use `leadkit pr create"
	if !strings.Contains(out, wantSubstr) {
		t.Errorf("want the VERBATIM platform-infra deny message, got:\n%s", out)
	}
}

func TestCLI_HookPRGuard_NonForgeCommandAllows(t *testing.T) {
	bin := testBinary(t)
	repo := gitRepoWithOrigin(t, "git@github.com:acmecorp/platform-infra.git")

	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	stdin := `{"tool_name":"Bash","tool_input":{"command":"npm test"},"session_id":"s1"}`
	out, code := runHookCLI(t, bin, repo, env, stdin)
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("want exit 0 and no output (allow), got code=%d out=%q", code, out)
	}
}
