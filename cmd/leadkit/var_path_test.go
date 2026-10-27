package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_SendToReviewThenVerdict_UnderTheUnresolvedTempDir is the incident, end to end. On macOS
// t.TempDir() is under /var, a symlink to /private/var, and the profile-resolved queue and the
// store came out spelled differently, so `review send` wrote a relative link that dangled and
// `gate verdict` then said "ticket not found". The root is deliberately NOT passed through
// EvalSymlinks. On a platform whose temp dir is not behind a symlink this passes trivially; the
// internal/queue and internal/review tests build the aliasing themselves and run everywhere.
func TestCLI_SendToReviewThenVerdict_UnderTheUnresolvedTempDir(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	proj := t.TempDir() // unresolved on purpose
	for _, d := range []string{"tasks", "queue/developer/pending", "queue/code-review/pending", "queue/code-review/in-progress", "queue/critique/pending"} {
		if err := os.MkdirAll(filepath.Join(proj, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(proj, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("project_id: r\nqueue_root: ./queue\nroster:\n  - type: developer\n    count: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = proj, sandboxedEnv(home, nil)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "git@example.com:o/r.git")
	git("config", "user.email", "a@b.c")
	git("config", "user.name", "n")
	git("add", "-A")
	git("commit", "-qm", "init")
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env = proj, sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("leadkit %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	run("task", "create", "--type", "developer", "--unparented", "--title", "x", "--assign", "developer")
	run("review", "send", "--task", "1-x", "--repo", proj)
	link := filepath.Join(proj, "queue", "code-review", "pending", "1-x.md")
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("the link review send made dangles (%v)", err)
	}
	run("queue", "move", "-queue", filepath.Join(proj, "queue"), link, filepath.Join(proj, "queue/code-review/in-progress")+"/")
	out := run("gate", "verdict", "--ticket", filepath.Join(proj, "queue/code-review/in-progress/1-x.md"), "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1")
	if !strings.Contains(out, "routed to critique") {
		t.Errorf("the verdict did not route the ticket:\n%s", out)
	}
}
