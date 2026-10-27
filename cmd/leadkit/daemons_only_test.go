package main

// End-to-end tests for `leadkit init --daemons-only` (docs/build-tasks/
// adopt-installs-daemons.md): the narrow, idempotent per-project daemon install adoption
// runs. These exec the ACTUAL built binary through runCLI's mandatory sandbox (HOME +
// cwd + LEADKIT_PROFILE pinned into temp dirs — see apply_guard_test.go's package doc),
// and pass --skip-launchctl so the plist CONTENT is written but no real launchd
// registration ever happens (a separately-exec'd binary can't take the in-process
// runLaunchctl stub — see ApplyDaemonPlan's doc comment).

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// daemonsOnlyProfile writes a profile fixture whose queue_root EXISTS under a named
// project dir, so the resolved daemon slug is that dir's name (asserted on the plist
// filenames). Returns the profile path and the resolved slug.
func daemonsOnlyProfile(t *testing.T, slug string) (profilePath string) {
	t.Helper()
	profileDir := t.TempDir()
	queueRoot := filepath.Join(profileDir, slug, "queue")
	if err := os.MkdirAll(queueRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	profilePath = filepath.Join(profileDir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: "+slug+"/queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath
}

// TestCLI_InitDaemonsOnly_DoesNotTouchSettingsOrSkills_AndIsIdempotent covers the core
// contract: --daemons-only --confirm installs ONLY the project's waker+sweep plists,
// leaves settings.json byte-identical, installs no skills, does NOT write the account
// burn plist, and a second run is a no-op/refresh (no duplicate, no error).
func TestCLI_InitDaemonsOnly_DoesNotTouchSettingsOrSkills_AndIsIdempotent(t *testing.T) {
	bin := testBinary(t)

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"hooks":{"PreToolUse":[]}}`)
	if err := os.WriteFile(settingsPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	launchAgentsDir := t.TempDir()
	skillsDir := t.TempDir()
	profilePath := daemonsOnlyProfile(t, "widgets")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env,
		"init", "--daemons-only", "--confirm",
		"--settings", settingsPath, "--launch-agents-dir", launchAgentsDir,
		"--skills-dir", skillsDir, "--skip-launchctl")
	if code != 0 {
		t.Fatalf("`init --daemons-only --confirm` should exit 0, got %d. output:\n%s", code, out)
	}

	// settings.json is byte-for-byte untouched — the WHOLE point of the narrow flag.
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("--daemons-only must leave settings.json byte-identical\nbefore=%s\nafter=%s", original, after)
	}
	// No skill installed.
	if _, err := os.Stat(filepath.Join(skillsDir, "leadkit-adopt")); !os.IsNotExist(err) {
		t.Errorf("--daemons-only must not install the leadkit-adopt skill (err=%v)", err)
	}

	if runtime.GOOS == "darwin" {
		entries, err := os.ReadDir(launchAgentsDir)
		if err != nil {
			t.Fatal(err)
		}
		got := plistNames(entries)
		want := map[string]bool{
			"dev.leadkit.waker.widgets.plist": true,
			"dev.leadkit.sweep.widgets.plist": true,
		}
		if len(got) != len(want) {
			t.Fatalf("want exactly the waker+sweep plists for slug widgets, got %v", got)
		}
		for _, n := range got {
			if !want[n] {
				t.Errorf("unexpected plist written by --daemons-only: %q (should be waker/sweep only, no burn/hooks)", n)
			}
		}
		if _, err := os.Stat(filepath.Join(launchAgentsDir, "dev.leadkit.burn.plist")); !os.IsNotExist(err) {
			t.Errorf("--daemons-only must NOT install the account-level burn plist (err=%v)", err)
		}
	}

	// Idempotent re-run: no error, no duplicate — the same set of plists, settings still
	// untouched.
	before := snapshotFiles(t, launchAgentsDir)
	out2, code2 := runCLI(t, bin, env,
		"init", "--daemons-only", "--confirm",
		"--settings", settingsPath, "--launch-agents-dir", launchAgentsDir,
		"--skills-dir", skillsDir, "--skip-launchctl")
	if code2 != 0 {
		t.Fatalf("idempotent re-run should exit 0, got %d. output:\n%s", code2, out2)
	}
	if runtime.GOOS == "darwin" {
		if !strings.Contains(out2, "already installed, unchanged") {
			t.Errorf("re-run should report the plists already installed/unchanged, got:\n%s", out2)
		}
	}
	after2 := snapshotFiles(t, launchAgentsDir)
	if len(after2) != len(before) {
		t.Errorf("re-run must not duplicate plists: before=%d files, after=%d files", len(before), len(after2))
	}
	if s, _ := os.ReadFile(settingsPath); string(s) != string(original) {
		t.Error("re-run must still leave settings.json untouched")
	}
}

// TestCLI_InitDaemonsOnly_QueueLessInstallsNothing: a gate-only project (queue_root does
// not exist) installs NOTHING, exits 0, and says so plainly.
func TestCLI_InitDaemonsOnly_QueueLessInstallsNothing(t *testing.T) {
	bin := testBinary(t)

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(settingsPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	launchAgentsDir := t.TempDir()

	// A profile whose queue_root points at a dir that does NOT exist — the gate-only case.
	profileDir := t.TempDir()
	profilePath := filepath.Join(profileDir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: nonexistent/queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env,
		"init", "--daemons-only", "--confirm",
		"--settings", settingsPath, "--launch-agents-dir", launchAgentsDir, "--skip-launchctl")
	if code != 0 {
		t.Fatalf("queue-less --daemons-only should exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "no queue") || !strings.Contains(out, "no waker needed") {
		t.Errorf("want the 'no queue — no waker needed' message, got:\n%s", out)
	}
	if entries, _ := os.ReadDir(launchAgentsDir); len(entries) != 0 {
		t.Errorf("queue-less --daemons-only must install NOTHING, found: %v", entries)
	}
	if s, _ := os.ReadFile(settingsPath); string(s) != string(original) {
		t.Error("queue-less --daemons-only must not touch settings.json")
	}
}

// TestCLI_InitDaemonsOnly_ParityWithApply: --daemons-only produces byte-identical waker
// and sweep plists to the full --apply path for the same project (same generator, not a
// forked template).
func TestCLI_InitDaemonsOnly_ParityWithApply(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("daemon plists are only written on darwin")
	}
	bin := testBinary(t)
	profilePath := daemonsOnlyProfile(t, "widgets")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	applyDir := t.TempDir()
	if _, code := runCLI(t, bin, env,
		"init", "--apply", "--confirm",
		"--settings", filepath.Join(t.TempDir(), "settings.json"),
		"--launch-agents-dir", applyDir,
		"--skills-dir", t.TempDir(), "--skip-launchctl"); code != 0 {
		t.Fatalf("`init --apply --confirm` should exit 0, got %d", code)
	}

	daemonsDir := t.TempDir()
	if _, code := runCLI(t, bin, env,
		"init", "--daemons-only", "--confirm",
		"--settings", filepath.Join(t.TempDir(), "settings.json"),
		"--launch-agents-dir", daemonsDir,
		"--skills-dir", t.TempDir(), "--skip-launchctl"); code != 0 {
		t.Fatalf("`init --daemons-only --confirm` should exit 0, got %d", code)
	}

	for _, name := range []string{"dev.leadkit.waker.widgets.plist", "dev.leadkit.sweep.widgets.plist"} {
		a, err := os.ReadFile(filepath.Join(applyDir, name))
		if err != nil {
			t.Fatalf("--apply did not write %s: %v", name, err)
		}
		d, err := os.ReadFile(filepath.Join(daemonsDir, name))
		if err != nil {
			t.Fatalf("--daemons-only did not write %s: %v", name, err)
		}
		if string(a) != string(d) {
			t.Errorf("%s differs between --apply and --daemons-only (must be the same generator)\napply=%s\ndaemons-only=%s", name, a, d)
		}
	}
}

// TestCLI_InitDaemonsOnly_SkipLaunchctlHonored: --skip-launchctl writes the plists but the
// output never claims a launchctl load happened (the escape hatch is honored under the new
// flag). runCLI already runs every invocation with --skip-launchctl in the other tests;
// this asserts the flag's observable effect explicitly.
func TestCLI_InitDaemonsOnly_SkipLaunchctlHonored(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("daemon plists are only written on darwin")
	}
	bin := testBinary(t)
	launchAgentsDir := t.TempDir()
	profilePath := daemonsOnlyProfile(t, "widgets")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env,
		"init", "--daemons-only", "--confirm",
		"--settings", filepath.Join(t.TempDir(), "settings.json"),
		"--launch-agents-dir", launchAgentsDir,
		"--skills-dir", t.TempDir(), "--skip-launchctl")
	if code != 0 {
		t.Fatalf("exit %d. output:\n%s", code, out)
	}
	// Plists written...
	if _, err := os.Stat(filepath.Join(launchAgentsDir, "dev.leadkit.waker.widgets.plist")); err != nil {
		t.Errorf("--skip-launchctl should still WRITE the waker plist: %v", err)
	}
	// ...but no launchctl load is claimed.
	if strings.Contains(out, "loaded via `launchctl") {
		t.Errorf("--skip-launchctl must not call launchctl, but output claims a load:\n%s", out)
	}
}

// plistNames returns the *.plist filenames among dir entries.
func plistNames(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".plist") {
			out = append(out, e.Name())
		}
	}
	return out
}
