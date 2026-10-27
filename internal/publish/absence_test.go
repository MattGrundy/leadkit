package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcluded(t *testing.T) {
	manifest := []string{"docs/build-tasks", "ONBOARDING.md"}
	for rel, want := range map[string]bool{
		"ONBOARDING.md":             true,
		"docs/build-tasks":          true,
		"docs/build-tasks/":         true,
		"docs/build-tasks/spec.md":  true,
		"docs/build-tasks-extra.md": false,
		"docs/ONBOARDING.md":        false,
		"docs/other.md":             false,
		"ONBOARDING.md.bak":         false,
	} {
		if got := Excluded(manifest, rel); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadManifest(dir); err == nil {
		t.Error("a tree with no manifest must be an error: nothing then explains an absence")
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(DefaultManifest)), []byte("// c\na.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(dir)
	if err != nil || strings.Join(got, ",") != "a.md" {
		t.Errorf("LoadManifest = %v, %v; want [a.md]", got, err)
	}
}
