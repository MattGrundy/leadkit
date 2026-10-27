package repoguard

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// checkDependabot returns one problem per reason the config would not keep pinned actions
// current. Pinning without an update path freezes the code the workflows run: the pin is
// auditable, but nothing moves it, and the decay is silent until an action is deprecated or
// has a CVE. The update path is part of the pin, so it is guarded with it.
func checkDependabot(content string) []string {
	var cfg struct {
		Version int `yaml:"version"`
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Directory string `yaml:"directory"`
			Schedule  struct {
				Interval string `yaml:"interval"`
			} `yaml:"schedule"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return []string{"not valid YAML: " + err.Error()}
	}
	var out []string
	if cfg.Version != 2 {
		out = append(out, "version must be 2")
	}
	for _, u := range cfg.Updates {
		if u.Ecosystem != "github-actions" {
			continue
		}
		// Workflows live under /.github/workflows; for this ecosystem Dependabot's
		// directory "/" covers them.
		if u.Directory != "/" {
			out = append(out, "github-actions directory is "+u.Directory+", want / (which covers .github/workflows)")
		}
		if u.Schedule.Interval == "" {
			out = append(out, "github-actions has no schedule.interval, so it would never run")
		}
		return out
	}
	return append(out, "no github-actions entry in updates")
}

func TestPinnedActionsHaveAnUpdatePath(t *testing.T) {
	b, err := os.ReadFile("../../.github/dependabot.yml")
	if err != nil {
		t.Fatalf("pinned actions need an update path, and .github/dependabot.yml is missing: %v", err)
	}
	for _, p := range checkDependabot(string(b)) {
		t.Error(p)
	}
}

func TestCheckDependabotRecognisesWhatItShouldReject(t *testing.T) {
	const ok = "version: 2\nupdates:\n  - package-ecosystem: github-actions\n    directory: /\n    schedule:\n      interval: weekly\n"
	cases := []struct {
		name, content string
		problems      int
	}{
		{"valid", ok, 0},
		{"valid beside another ecosystem", ok + "  - package-ecosystem: gomod\n    directory: /\n    schedule:\n      interval: weekly\n", 0},
		{"empty", "", 2},
		{"only another ecosystem", strings.Replace(ok, "github-actions", "gomod", 1), 1},
		{"wrong directory", strings.Replace(ok, "directory: /", "directory: /sub", 1), 1},
		{"no schedule", strings.Replace(ok, "    schedule:\n      interval: weekly\n", "", 1), 1},
		{"wrong version", strings.Replace(ok, "version: 2", "version: 1", 1), 1},
		{"not yaml", "updates: [", 1},
	}
	for _, c := range cases {
		if got := checkDependabot(c.content); len(got) != c.problems {
			t.Errorf("%s: %d problems %v, want %d", c.name, len(got), got, c.problems)
		}
	}
}
