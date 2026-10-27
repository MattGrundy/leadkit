package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The reverse of the documented order, on the real binary: init before the queue exists installs
// no waker and no sweep, and has to say so where it cannot be missed (stderr, after the plan, and
// again as the last thing it prints), with the way out.
func TestCLI_InitBeforeTheQueueExistsWarnsThatNoDaemonsWereInstalled(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("daemon install is darwin-only")
	}
	bin := testBinary(t)
	dir := t.TempDir()
	profile := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("project_id: widget\nqueue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := t.TempDir()
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profile}, "init", "--apply", "--confirm",
		"--settings", filepath.Join(t.TempDir(), "settings.json"), "--launch-agents-dir", agents,
		"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir(), "--skip-launchctl")
	if code != 0 {
		t.Fatalf("init itself still succeeds (hooks and skill are fine): %d\n%s", code, out)
	}
	if n := strings.Count(out, "WARNING: the waker and sweep daemons were NOT installed"); n != 2 {
		t.Errorf("want the warning after the plan AND at the end, got %d:\n%s", n, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "Until then nothing wakes the owners.") {
		t.Errorf("the warning must be the last thing printed:\n%s", out)
	}
	for _, label := range []string{"dev.leadkit.waker.widget", "dev.leadkit.sweep.widget"} {
		if _, err := os.Stat(filepath.Join(agents, label+".plist")); err == nil {
			t.Errorf("%s was installed for a queue that does not exist", label)
		}
	}

	// After the queue exists, the way out the warning names works.
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code = runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profile}, "init", "--daemons-only", "--confirm", "--launch-agents-dir", agents, "--skip-launchctl"); code != 0 {
		t.Fatalf("daemons-only: %d\n%s", code, out)
	}
	for _, label := range []string{"dev.leadkit.waker.widget", "dev.leadkit.sweep.widget"} {
		if _, err := os.Stat(filepath.Join(agents, label+".plist")); err != nil {
			t.Errorf("%s missing after the recovery command: %v", label, err)
		}
	}
}
