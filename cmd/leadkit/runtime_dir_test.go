package main

// Ticket 302, objective 2: a command must not leave a repo in a state the push gate cannot decide.
// A profile-less session resolves `.leadkit/...` against its working directory, so recording a
// decision there would create the very directory the gate reads as "a leadkit project".

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

func TestOrphanRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	defaults := config.Defaults()
	target := filepath.Join(dir, ".leadkit", "decisions.jsonl")

	if err := orphanRuntimeDir(&defaults, target); err == nil || !strings.Contains(err.Error(), "leadkit config init") {
		t.Errorf("a profile-less write that would create .leadkit/ must be refused with the remedy, got %v", err)
	}
	if err := orphanRuntimeDir(&defaults, filepath.Join(dir, "elsewhere", "decisions.jsonl")); err != nil {
		t.Errorf("only the .leadkit/ runtime directory is guarded, got %v", err)
	}
	if err := orphanRuntimeDir(&defaults, ""); err != nil {
		t.Errorf("no path, nothing to guard: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := orphanRuntimeDir(&defaults, target); err != nil {
		t.Errorf("a directory that is already there is not this guard's business, got %v", err)
	}

	profiled := t.TempDir()
	if err := os.WriteFile(filepath.Join(profiled, "leadkit.yaml"), []byte("project_id: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prof, _, err := config.LoadFrom(profiled)
	if err != nil {
		t.Fatal(err)
	}
	if err := orphanRuntimeDir(prof, filepath.Join(profiled, ".leadkit", "decisions.jsonl")); err != nil {
		t.Errorf("a profiled repo may create its own runtime directory, got %v", err)
	}
}

// Through the binary: every command that creates `.leadkit/` refuses in a repo with no profile and
// leaves no directory behind, and the same command works once a profile exists.
func TestCLI_RuntimeDirIsNotCreatedWithoutAProfile(t *testing.T) {
	bin := testBinary(t)
	commands := map[string][]string{
		"decision record": {"decision", "record", "--what", "w", "--by", "b"},
		"spike record":    {"spike", "record", "--ref", "abc", "--verdict", "green"},
		"milestone init":  {"milestone", "init"},
		"goal ensure":     {"goal", "ensure"},
		"lock":            {"lock"},
	}
	run := func(dir string, args []string) (string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = sandboxedEnv(t.TempDir(), []string{"LEADKIT_PROFILE="})
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	for name, args := range commands {
		t.Run(name, func(t *testing.T) {
			bare := t.TempDir()
			out, code := run(bare, args)
			if code == 0 || !strings.Contains(out, "leadkit config init") {
				t.Fatalf("with no profile it must refuse and name the remedy (exit %d):\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(bare, ".leadkit")); err == nil {
				t.Fatal("the refused command left a .leadkit/ directory behind")
			}

			profiled := t.TempDir()
			mustWrite(t, filepath.Join(profiled, "leadkit.yaml"), "project_id: p\n")
			if out, code := run(profiled, args); code != 0 {
				t.Fatalf("with a profile the same command must work (exit %d):\n%s", code, out)
			}
			if fi, err := os.Stat(filepath.Join(profiled, ".leadkit")); err != nil || !fi.IsDir() {
				t.Error("a profiled repo gets its .leadkit/ directory")
			}

			existing := t.TempDir()
			mustWrite(t, filepath.Join(existing, ".leadkit", "keep"), "x")
			if out, code := run(existing, args); code != 0 {
				t.Fatalf("a .leadkit/ that is already there is not the guard's business (exit %d):\n%s", code, out)
			}
		})
	}
}
