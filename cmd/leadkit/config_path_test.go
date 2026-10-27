package main

// `leadkit config path` / whoami / config show naming the config files for a repo. Each
// test runs the real binary in a real git repo whose DIR basename ("ci") deliberately
// differs from its origin repo-name ("widget-ci"), so a slug derived from the dir would
// name the wrong file.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// cfgPathRepo makes <tmp>/ci with origin acme/widget-ci.git, returning (home, repo).
func cfgPathRepo(t *testing.T) (home, repo string) {
	t.Helper()
	home = t.TempDir()
	repo = mkdir(t, filepath.Join(t.TempDir(), "ci"))
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "git@github.com:acme/widget-ci.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// macOS: /var -> /private/var; use the resolved path the binary will see.
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		repo = r
	}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	return home, repo
}

func runIn(t *testing.T, dir, home string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(testBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = sandboxedEnv(home, env)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec: %v\n%s", err, out)
	return "", -1
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	mkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCLI_ConfigPath_Defaults(t *testing.T) {
	home, repo := cfgPathRepo(t)
	ext := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	out, code := runIn(t, repo, home, []string{"LEADKIT_PROFILE="}, "config", "path")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		ext + " [not created]",
		filepath.Join(repo, "leadkit.yaml") + " [not created]",
		"ACTIVE: defaults (no config file)",
		"leadkit config init --external",
		"wins over external if both exist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "config/ci.yaml") {
		t.Errorf("must key on origin repo-name, not dir basename:\n%s", out)
	}
}

func TestCLI_ConfigPath_ExternalActive(t *testing.T) {
	home, repo := cfgPathRepo(t)
	ext := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	writeFile(t, ext, "project_id: widget-ci\n")
	out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE="}, "config", "path")
	if !strings.Contains(out, ext+" [exists]") || !strings.Contains(out, "ACTIVE: "+ext) {
		t.Errorf("external should be [exists] and ACTIVE:\n%s", out)
	}
	if strings.Contains(out, "leadkit config init --external") {
		t.Errorf("no init hint when a config is active:\n%s", out)
	}
}

func TestCLI_ConfigPath_InRepoWinsOverExternal(t *testing.T) {
	home, repo := cfgPathRepo(t)
	ext := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	writeFile(t, ext, "project_id: widget-ci\n")
	inRepo := filepath.Join(repo, "leadkit.yaml")
	writeFile(t, inRepo, "project_id: x\n")
	out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE="}, "config", "path")
	if !strings.Contains(out, ext+" [exists]") || !strings.Contains(out, inRepo+" [exists]") {
		t.Errorf("both should be [exists]:\n%s", out)
	}
	if !strings.Contains(out, "ACTIVE: "+inRepo) {
		t.Errorf("in-repo must be ACTIVE:\n%s", out)
	}
}

func TestCLI_ConfigPath_EnvOverrideWins(t *testing.T) {
	home, repo := cfgPathRepo(t)
	writeFile(t, filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml"), "project_id: a\n")
	writeFile(t, filepath.Join(repo, "leadkit.yaml"), "project_id: b\n")
	envProf := filepath.Join(t.TempDir(), "other.yaml")
	writeFile(t, envProf, "project_id: c\n")
	out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE=" + envProf}, "config", "path")
	if !strings.Contains(out, "$LEADKIT_PROFILE: "+envProf) || !strings.Contains(out, "ACTIVE: "+envProf) {
		t.Errorf("$LEADKIT_PROFILE must be shown and ACTIVE:\n%s", out)
	}
}

// The printed external path must be exactly what the shared resolver computes (no fork).
func TestConfigPath_ExternalMatchesResolver(t *testing.T) {
	home, repo := cfgPathRepo(t)
	t.Setenv("HOME", home)
	t.Setenv("LEADKIT_PROFILE", "")
	want := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	writeFile(t, want, "project_id: widget-ci\n")
	got, _, ok := config.ExternalConfigPathForDir(repo)
	if !ok || got != want {
		t.Fatalf("ExternalConfigPathForDir = %q ok=%v, want %q", got, ok, want)
	}
	if loc := config.ResolveLocations(repo); loc.ExternalPath != got || loc.Active != got {
		t.Errorf("ResolveLocations external=%q active=%q, want %q", loc.ExternalPath, loc.Active, got)
	}
}

func TestCLI_WhoamiAndConfigShow_DefaultsNameWouldBePath(t *testing.T) {
	home, repo := cfgPathRepo(t)
	ext := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	for _, args := range [][]string{{"whoami"}, {"config", "show"}, {"config", "show", "--show-origin"}} {
		out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE="}, args...)
		if !strings.Contains(out, "would read "+ext+" if created") ||
			!strings.Contains(out, "dir basename") || !strings.Contains(out, "leadkit config path") {
			t.Errorf("%v: missing would-be path/basename note:\n%s", args, out)
		}
	}
	// With the external file present the hint disappears.
	writeFile(t, ext, "project_id: widget-ci\n")
	out, _ := runIn(t, repo, home, []string{"LEADKIT_PROFILE="}, "whoami")
	if strings.Contains(out, "would read") {
		t.Errorf("no hint once the config exists:\n%s", out)
	}
}
