package main

// profile-store-resolution: end-to-end proof that `review send` and `gate
// verdict` resolve against the PROJECT's own queue_root/store_root (from its
// leadkit.yaml), not the hardcoded legacy-migration default — the adopter
// regression three separate teams hit. Runs the actual binary; every fixture
// lives under t.TempDir(), and runCLI's mandatory HOME sandboxing (see
// apply_guard_test.go's package doc) keeps the real ~/.claude/review-gate
// records dir out of reach even though gate verdict writes a real record here.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/review"
)

// gitRepoWithOneCommit creates a throwaway git repo with a single commit (so
// HEAD resolves to a real full sha) and, if originURL != "", an origin remote.
func gitRepoWithOneCommit(t *testing.T, originURL string) (repo, headSHA string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if originURL != "" {
		run("remote", "add", "origin", originURL)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")

	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return repo, strings.TrimSpace(string(out))
}

// TestCLI_ReviewSend_ResolvesAgainstProjectsOwnQueueAndStore is the `review
// send` half of the adopter regression: a project's leadkit.yaml declares its
// OWN queue_root/store_root — send-to-review must resolve the ticket there,
// not against the built-in legacy-migration default.
func TestCLI_ReviewSend_ResolvesAgainstProjectsOwnQueueAndStore(t *testing.T) {
	bin := testBinary(t)
	projRoot := t.TempDir()

	profilePath := filepath.Join(projRoot, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(projRoot, "tasks")
	pendingDir := filepath.Join(projRoot, "queue", "code-review", "pending")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(tasksDir, "9-example.md")
	if err := os.WriteFile(ticketPath, []byte("---\ntitle: t\nstatus: backlog\n---\n\n# body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, _ := gitRepoWithOneCommit(t, "")

	env := []string{"LEADKIT_PROFILE=" + profilePath, "LEADKIT_QUEUE="}
	out, code := runCLI(t, bin, env, "review", "send", "--task", "9-example", "--repo", repo)
	if code != 0 {
		t.Fatalf("`review send` should exit 0, got %d. output:\n%s", code, out)
	}

	link := filepath.Join(pendingDir, "9-example.md")
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve queue link: %v", err)
	}
	wantReal, _ := filepath.EvalSymlinks(ticketPath)
	if real != wantReal {
		t.Errorf("queue link resolves to %s, want the project's own ticket %s — resolved against the wrong store", real, wantReal)
	}
}

// TestCLI_GateVerdict_ResolvesAgainstProjectsOwnQueueAndStore is the `gate
// verdict` half: it must resolve + reconcile the ticket against the project's
// own store, and the gate record against the project's own scope
// (enforced_remotes) — not the built-in defaults.
func TestCLI_GateVerdict_ResolvesAgainstProjectsOwnQueueAndStore(t *testing.T) {
	bin := testBinary(t)
	projRoot := t.TempDir()

	profileYAML := "queue_root: queue\nstore_root: tasks\nenforced_remotes: [custom-remote-name]\n"
	profilePath := filepath.Join(projRoot, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(projRoot, "tasks")
	crPending := filepath.Join(projRoot, "queue", "code-review", "pending")
	critPending := filepath.Join(projRoot, "queue", "critique", "pending")
	for _, d := range []string{tasksDir, crPending, critPending} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	repo, headSHA := gitRepoWithOneCommit(t, "git@example.com:org/custom-remote-name.git")

	ticketPath := filepath.Join(tasksDir, "9-example.md")
	ticket := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + repo + "\nsha: " + headSHA + "\nstage: code-review\ngate: true\n---\n\n# body\n"
	if err := os.WriteFile(ticketPath, []byte(ticket), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(crPending, "9-example.md")
	if err := os.Symlink(ticketPath, link); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	env := []string{"LEADKIT_PROFILE=" + profilePath, "LEADKIT_QUEUE=", "HOME=" + home}
	out, code := runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1")
	if code != 0 {
		t.Fatalf("`gate verdict` should exit 0, got %d. output:\n%s", code, out)
	}

	fm, err := review.ReadFrontmatter(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if fm["stage"] != "critique" || fm["status"] != "pending" {
		t.Errorf("ticket frontmatter = %+v, want stage=critique status=pending (routed to the project's own critique lane)", fm)
	}
	if _, err := os.Lstat(filepath.Join(critPending, "9-example.md")); err != nil {
		t.Errorf("ticket should have moved into the project's own critique role lane: %v", err)
	}

	recordPath := filepath.Join(home, ".claude", "review-gate", "records", headSHA+".json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("gate record not written at %s: %v", recordPath, err)
	}
	if !strings.Contains(string(data), "code-review") || !strings.Contains(string(data), "ready") {
		t.Errorf("record content = %s, want a code-review=ready entry", data)
	}
}
