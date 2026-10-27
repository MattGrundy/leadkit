//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ptyDriver runs argv with a pseudo-terminal as its controlling terminal and types a line into it
// when the prompt appears. It is a stand-in for the OWNER at a terminal, and it is also exactly what
// an agent could do with any pty tool: the confirmation stops a process with no terminal, not one
// that allocates its own.
const ptyDriver = `import os, pty, sys, time, select
typed, argv = sys.argv[1], sys.argv[2:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(argv[0], argv)
out, sent, start = b"", False, time.time()
while time.time() - start < 20:
    r, _, _ = select.select([fd], [], [], 0.2)
    if not r:
        continue
    try:
        d = os.read(fd, 4096)
    except OSError:
        break
    if not d:
        break
    out += d
    if not sent and b"to confirm" in out:
        os.write(fd, typed.encode())
        sent = True
else:
    os.kill(pid, 9)
_, st = os.waitpid(pid, 0)
sys.stdout.write(out.decode("utf-8", "replace"))
sys.exit(os.waitstatus_to_exitcode(st))
`

// TestCLI_PREditBase drives `leadkit pr edit --base` through the real binary with a fake `gh` that
// reports the PR's current base and records any edit it is asked to perform. The refusal is the
// part that proves the guard: a retarget whose current base is not merged into the new one must not
// reach the forge. The fake gh is never given a way to succeed at one.
func TestCLI_PREditBase(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to give the child a terminal to confirm on")
	}
	bin := testBinary(t)
	home := t.TempDir()
	repo := t.TempDir()
	driver := filepath.Join(t.TempDir(), "pty_driver.py")
	if err := os.WriteFile(driver, []byte(ptyDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(sandboxedEnv(home, nil), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("remote", "add", "origin", "git@github.com:acme/widget.git")
	commit := func(msg string) string {
		if err := os.WriteFile(filepath.Join(repo, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", msg)
		return git("rev-parse", "HEAD")
	}
	root := commit("root")
	below := commit("stack base, merged into main")
	main := commit("main")
	git("checkout", "-q", "-b", "diverged", root)
	diverged := commit("not merged")
	// The forge is a local bare repo, so `git fetch origin` (the refresh the retarget check performs)
	// works offline. The authority entry matches it by name and says it is github.
	bare := filepath.Join(t.TempDir(), "forge.git")
	git("init", "-q", "--bare", bare)
	git("remote", "set-url", "origin", bare)
	git("push", "-q", "origin", main+":refs/heads/main", below+":refs/heads/below", diverged+":refs/heads/diverged")

	fakeAuthor := "me"
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fake gh: `pr view ... baseRefName` answers $FAKE_BASE, `pr view ... author` answers
	// $FAKE_AUTHOR, `api user` answers $FAKE_SELF; `pr edit` records its argv and succeeds.
	stubs := t.TempDir()
	log := filepath.Join(stubs, "edits.log")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in *author*) echo \"$FAKE_AUTHOR\" ;; *) echo \"$FAKE_BASE\" ;; esac; exit 0 ;;\n" +
		"  \"api user\") echo \"$FAKE_SELF\"; exit 0 ;;\n" +
		"  \"pr edit\") echo \"$@\" >> " + log + "; exit 0 ;;\nesac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	profileUsed := profile
	cmdEnv := func(fakeBase string) []string {
		return sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profileUsed, "FAKE_BASE=" + fakeBase, "FAKE_AUTHOR=" + fakeAuthor, "FAKE_SELF=me", "LEADKIT_SESSION_ID=sess-1", "PATH=" + stubs + ":" + os.Getenv("PATH")})
	}
	finish := func(c *exec.Cmd, args []string) (string, int) {
		t.Helper()
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
	// run has NO controlling terminal (its own session), like an agent's shell. It must not inherit
	// the terminal `go test` runs under, or the confirmation would wait on a developer's keyboard.
	run := func(fakeBase string, args ...string) (string, int) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir = repo
		c.Env = cmdEnv(fakeBase)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return finish(c, args)
	}
	// runTyped runs with a pty as the controlling terminal and types `typed` at the prompt.
	runTyped := func(fakeBase, typed string, args ...string) (string, int) {
		t.Helper()
		c := exec.Command(python, append([]string{driver, typed, bin}, args...)...)
		c.Dir = repo
		c.Env = cmdEnv(fakeBase)
		return finish(c, args)
	}
	edits := func() string { b, _ := os.ReadFile(log); return string(b) }

	// This session is a registered lead. The refusals in this first stretch are about ancestry and the
	// passthrough, which are judged before the owner is ever asked.
	if out, code := run("", "register", "--role", "lead"); code != 0 {
		t.Fatalf("register: exit %d\n%s", code, out)
	}

	// REFUSED: the PR is based on a branch that is NOT merged into main.
	out, code := run("diverged", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, `"diverged" is NOT merged into "main"`) {
		t.Fatalf("a retarget off an unmerged base must be refused (exit %d):\n%s", code, out)
	}
	if edits() != "" {
		t.Fatalf("the forge was asked to edit a refused retarget: %s", edits())
	}

	// REFUSED: a base that cannot be verified is not guessed at.
	out, code = run("origin-missing", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code == 0 || !strings.Contains(out, "Fail-closed") {
		t.Fatalf("an unverifiable base must refuse fail-closed (exit %d):\n%s", code, out)
	}

	// REFUSED: the passthrough cannot be used to dodge the check.
	if out, code = run("diverged", "pr", "edit", "4", "--title", "t", "--repo", repo, "--", "--base", "main"); code == 0 {
		t.Fatalf("--base through the passthrough must be refused:\n%s", out)
	}
	if edits() != "" {
		t.Fatalf("the forge was asked to edit: %s", edits())
	}

	// DRY RUN of the allowed case shows the plan and still does not call the forge.
	out, code = run("below", "pr", "edit", "4", "--base", "main", "--repo", repo, "--dry-run")
	if code != 0 || !strings.Contains(out, "below -> main") || !strings.Contains(out, "--base main") {
		t.Fatalf("dry run of a narrowing retarget (exit %d):\n%s", code, out)
	}
	if edits() != "" {
		t.Fatalf("a dry run reached the forge: %s", edits())
	}

	// NO TERMINAL: everything else is satisfied (lead, ancestry), and it is still refused, because
	// the owner cannot be asked. This is what an agent's shell gets.
	out, code = run("below", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code == 0 || !strings.Contains(out, "no controlling terminal") || edits() != "" {
		t.Fatalf("a retarget with no terminal must be refused and never reach the forge (exit %d, gh saw %q):\n%s", code, edits(), out)
	}

	// WRONG ANSWER on a real terminal: refused, nothing sent.
	for _, typed := range []string{"5\n", "yes\n", "\n"} {
		out, code = runTyped("below", typed, "pr", "edit", "4", "--base", "main", "--repo", repo)
		if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "confirmation was not given") || edits() != "" {
			t.Fatalf("typing %q must not confirm (exit %d, gh saw %q):\n%s", typed, code, edits(), out)
		}
	}

	// CONFIRMED on a real terminal: the stacked-PR case, where the current base is already merged into main.
	out, code = runTyped("below", "4\n", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code != 0 || !strings.Contains(out, "below -> main") {
		t.Fatalf("a strictly narrowing retarget the owner confirmed must be allowed (exit %d):\n%s", code, out)
	}
	if got := edits(); !strings.Contains(got, "pr edit 4") || !strings.Contains(got, "--base main") {
		t.Errorf("gh was not asked to retarget: %q", got)
	}
	// It is per use: the next retarget asks again, and a wrong answer to it is refused.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if out, code = runTyped("below", "no\n", "pr", "edit", "4", "--base", "main", "--repo", repo); code == 0 || edits() != "" {
		t.Fatalf("a second retarget must ask again (exit %d, gh saw %q):\n%s", code, edits(), out)
	}

	// The role, on its own: an ic who types the right number is refused before being asked.
	if out, code := run("", "register", "--role", "ic"); code != 0 {
		t.Fatalf("register ic: exit %d\n%s", code, out)
	}
	out, code = runTyped("below", "4\n", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code == 0 || !strings.Contains(out, "only a lead may retarget") || !strings.Contains(out, `"ic"`) || edits() != "" {
		t.Fatalf("an ic must be refused even with the right confirmation (exit %d, gh saw %q):\n%s", code, edits(), out)
	}
	if strings.Contains(out, "Type the PR number") {
		t.Errorf("a refused role must be refused BEFORE the owner is asked:\n%s", out)
	}
	// A title-only edit needs neither a role nor a terminal.
	if out, code = run("below", "pr", "edit", "4", "--title", "x", "--repo", repo); code != 0 {
		t.Fatalf("a title edit is not gated on role or terminal (exit %d):\n%s", code, out)
	}
	if out, code := run("", "register", "--role", "lead"); code != 0 {
		t.Fatalf("register lead: exit %d\n%s", code, out)
	}

	// owner_prs_only applies to a base-only edit too: retargeting someone else's PR is refused even
	// when the retarget itself would be strictly narrowing and the owner would confirm it.
	ownerProfile := filepath.Join(repo, "owner.yaml")
	if err := os.WriteFile(ownerProfile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n    owner_prs_only: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profileUsed = ownerProfile
	if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	fakeAuthor = "someone-else"
	out, code = runTyped("below", "4\n", "pr", "edit", "4", "--base", "main", "--repo", repo)
	if code == 0 || !strings.Contains(out, "owner_prs_only") {
		t.Fatalf("a base-only retarget of another author's PR must be refused by owner_prs_only (exit %d):\n%s", code, out)
	}
	if edits() != "" {
		t.Fatalf("the forge was asked to retarget a PR the caller does not own: %q", edits())
	}
	fakeAuthor = "me"
	if out, code = runTyped("below", "4\n", "pr", "edit", "4", "--base", "main", "--repo", repo); code != 0 {
		t.Fatalf("control: the same retarget of the caller's own PR must be allowed (exit %d):\n%s", code, out)
	}
}
