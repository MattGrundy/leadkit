package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_PREdit_PassthroughIsAnAllowlist runs the smuggling cases through the real binary with a
// fake `gh` that records every edit it is asked to perform. The refusals are the point: a trailing
// -R would otherwise be the LAST --repo on the composed command line, so the edit would land in a
// repository the authority entry never covered. The fake gh has no way to succeed at anything but
// the control case.
func TestCLI_PREdit_PassthroughIsAnAllowlist(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = repo, sandboxedEnv(home, nil)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "git@github.com:acme/widget.git")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  acme/widget:\n    can_open_pr: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubs := t.TempDir()
	log := filepath.Join(stubs, "edits.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir = repo
		c.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile, "PATH=" + stubs + ":" + os.Getenv("PATH")})
		out, err := c.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("exec %v: %v", args, err)
		return "", -1
	}
	edits := func() string { b, _ := os.ReadFile(log); return string(b) }

	// Control: the same edit with no passthrough goes through, so the refusals below are about the passthrough.
	out, code := run("pr", "edit", "4", "--title", "t", "--repo", repo)
	if code != 0 || !strings.Contains(edits(), "--repo acme/widget") || !strings.Contains(edits(), "--title t") {
		t.Fatalf("control edit failed (exit %d, gh saw %q):\n%s", code, edits(), out)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}

	for _, extra := range [][]string{
		{"-R", "victim/other"},
		{"--repo=victim/other"},
		{"--attach", "/etc/passwd"},
		{"--add-reviewer", "someone"},
		{"--body", "replaced"},
		{"--base", "main"},
		{"-Bmain"}, // attached shorthand: pflag parses it as base=main
		{"-Rvictim/other"},
		{"99"},
	} {
		args := append([]string{"pr", "edit", "4", "--title", "t", "--repo", repo, "--"}, extra...)
		out, code := run(args...)
		if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "allowlist") {
			t.Errorf("passthrough %v must be refused (exit %d):\n%s", extra, code, out)
		}
		if got := edits(); got != "" {
			t.Fatalf("the forge was asked to run a refused edit (%v): %q", extra, got)
		}
	}

	// A dry run of a refused passthrough also says so and still runs nothing.
	out, code = run("pr", "edit", "4", "--title", "t", "--repo", repo, "--dry-run", "--", "-R", "victim/other")
	if !strings.Contains(out, "refused") || edits() != "" {
		t.Errorf("dry run (exit %d) must show the refusal and run nothing: %q\n%s", code, edits(), out)
	}
}
