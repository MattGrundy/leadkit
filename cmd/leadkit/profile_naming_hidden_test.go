package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 319: the profile named for a remote is found by name, so nothing about the file or its
// directory may hide it. Each case is a clone with no .leadkit/, named differently from the
// profile, whose remote was renamed, in enforce mode, with a profile that carries no project_id or
// project_origin (so only its NAME connects it to the repo). All three enforcers must refuse.

// hiddenWorld is such a clone, with the profile rewritten under fileName with the given mode.
func hiddenWorld(t *testing.T, fileName, body string, mode os.FileMode) namingWorld {
	t.Helper()
	w := newNamingWorld(t, namingCell{mode: "enforce", leadkit: false, op: "rename", sameNamed: false})
	cfg := filepath.Join(w.home, ".claude", "leadkit", "config")
	if err := os.Remove(filepath.Join(cfg, "proj.yaml")); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		body = "records_dir: " + filepath.Join(w.work, "records") + "\nguards:\n  default_mode: enforce\n"
	}
	path := filepath.Join(cfg, fileName)
	mustWrite(t, path, body)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w namingWorld) assertRefused(t *testing.T, bin string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"cwd": w.repo, "tool_name": "Bash", "tool_input": map[string]string{"command": "git push x main"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _, code := w.run(t, bin, string(payload), "hook", "push-gate")
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "cannot be known") {
		t.Errorf("hook: want a refusal that says the gate cannot be known (exit %d):\n%s", code, out)
	}
	stdin := "refs/heads/main " + w.sha + " refs/heads/main 0000000000000000000000000000000000000000\n"
	out, errOut, code := w.run(t, bin, stdin, "gate", "check-push")
	if code == 0 || !strings.Contains(out+errOut, "cannot be known") {
		t.Errorf("pre-push check: want a refusal (exit %d):\n%s%s", code, out, errOut)
	}
}

func TestNamingHidden_ACaseVariantProfileOnACaseInsensitiveFilesystem(t *testing.T) {
	bin := testBinary(t)
	w := hiddenWorld(t, "Proj.yaml", "", 0o644)
	if _, err := os.Stat(filepath.Join(w.home, ".claude", "leadkit", "config", "proj.yaml")); err != nil {
		t.Skip("this filesystem is case-sensitive, so the lookup would not find Proj.yaml either")
	}
	w.assertRefused(t, bin)
}

func TestNamingHidden_AConfigDirectoryThatCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	bin := testBinary(t)
	w := hiddenWorld(t, "proj.yaml", "", 0o644)
	cfg := filepath.Join(w.home, ".claude", "leadkit", "config")
	if err := os.Chmod(cfg, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg, 0o755) })
	if _, err := os.ReadDir(cfg); err == nil {
		t.Skip("this filesystem lists a mode 0311 directory")
	}
	w.assertRefused(t, bin)
}

func TestNamingHidden_AProfileNamedForTheRemoteThatCannotBeParsedOrRead(t *testing.T) {
	bin := testBinary(t)
	t.Run("unparseable", func(t *testing.T) {
		hiddenWorld(t, "proj.yaml", "project_id: [unclosed\n", 0o644).assertRefused(t, bin)
	})
	t.Run("mode 000", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permissions do not bind root")
		}
		hiddenWorld(t, "proj.yaml", "", 0).assertRefused(t, bin)
	})
}
