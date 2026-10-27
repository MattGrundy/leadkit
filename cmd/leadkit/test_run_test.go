package main

// `test run` execs the actual repo's own test command — these CLI-level tests use
// a tiny throwaway git repo with a trivial go.mod so the "suite" it runs is just
// `go test ./...` against an empty package (fast, real, harmless), never touching
// this leadkit repo or any real clone. Sandboxed via runCLI like every other test
// in this package regardless.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitFixtureRepo creates a tiny git repo with one commit and returns its dir + HEAD sha.
func gitFixtureRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@x.com", "GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@x.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "fixture_test.go"), []byte("package fixture\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "initial")

	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

func TestCLI_TestRun_GreenOnEmptyGoRepo(t *testing.T) {
	bin := testBinary(t)
	repo, sha := gitFixtureRepo(t)
	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath, "GOTELEMETRY=off"}

	out, code := runCLI(t, bin, env, "test", "run", "--repo", repo, "--sha", sha, "--affected=false")
	if code != 0 {
		t.Fatalf("want exit 0 (green), got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "tests: green") {
		t.Errorf("want a green verdict line, got:\n%s", out)
	}
}

func TestCLI_TestRun_ShaMismatchRefusesWithNonZeroExit(t *testing.T) {
	bin := testBinary(t)
	repo, _ := gitFixtureRepo(t)
	root := t.TempDir()
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nteam:\n  team_dir: team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "test", "run", "--repo", repo, "--sha", "0000000000000000000000000000000000000000")
	if code == 0 {
		t.Errorf("want a non-zero exit for a sha mismatch, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Errorf("want a refusing message, got:\n%s", out)
	}
}
