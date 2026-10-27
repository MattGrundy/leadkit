package main

// `config show --show-origin` is read-only, but sandboxed like every other test
// here so a real ~/.config/leadkit/config.yaml (if one ever exists) never leaks
// into a test's result.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_ConfigShow_ShowOriginAnnotatesLayers(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	teamPath := filepath.Join(root, "team-config.yaml")
	if err := os.WriteFile(teamPath, []byte("outputs: [linear]\nlocked: [outputs]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("team_config: "+teamPath+"\nqueue_root: q\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "config", "show", "--show-origin")
	if code != 0 {
		t.Fatalf("want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "layer=project") {
		t.Errorf("want a project-layer annotation, got:\n%s", out)
	}
	if !strings.Contains(out, "layer=team, locked") {
		t.Errorf("want outputs annotated team/locked, got:\n%s", out)
	}
}
