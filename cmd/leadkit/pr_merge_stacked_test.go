package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_PRMerge_StackedBranchesDecideTheMethod pins the wiring of the stack check through the real
// binary, which the pure decision tests cannot: that the probe's answer reaches DecideMerge, that the PR's
// own base (not "main") is what the range is measured against, and that the head the forge reports is the
// PR's own branch. A fake gh describes an open, clean, green PR and records every merge it is asked for.
func TestCLI_PRMerge_StackedBranchesDecideTheMethod(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := t.TempDir()
	gitEnv := append(sandboxedEnv(home, nil), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = repo, gitEnv
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", msg)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	bare := filepath.Join(t.TempDir(), "forge.git")
	git("init", "-q", "--bare", bare)
	git("remote", "add", "origin", bare)
	commit("root")
	git("branch", "-M", "main")

	// main <- k1 <- k2 <- k3 (the PR head, "keystone"); "on-k1" forks from the OLDEST commit and does not
	// contain the head, so a head-anchored check would call keystone a leaf. "release" sits at k1.
	git("checkout", "-q", "-b", "keystone")
	k1 := commit("k1")
	commit("k2")
	k3 := commit("k3")
	git("branch", "release", k1)
	git("checkout", "-q", "-b", "on-k1", k1)
	commit("d1")
	git("checkout", "-q", "main")
	git("push", "-q", "origin", "main", "keystone", "on-k1", "release")
	git("fetch", "-q", "origin")
	// the PR's own local branch sits one commit PAST the head the forge reports, so it is excluded by name
	// (it contains the oldest commit and is not at the head sha)
	git("checkout", "-q", "keystone")
	commit("k4")
	git("checkout", "-q", "main")

	stubs := t.TempDir()
	mergeLog := filepath.Join(stubs, "merges.log")
	baseFile := filepath.Join(stubs, "base")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in *commits*) echo '{\"title\":\"t\",\"number\":9,\"commits\":[{\"messageHeadline\":\"h\",\"messageBody\":\"\"}]}' ;; " +
		"*) echo '{\"state\":\"OPEN\",\"isDraft\":false,\"mergeable\":\"MERGEABLE\",\"mergeStateStatus\":\"CLEAN\",\"headRefOid\":\"" + k3 + "\",\"headRefName\":\"keystone\",\"title\":\"t\",\"baseRefName\":\"'\"$(cat " + baseFile + ")\"'\"}' ;; esac; exit 0 ;;\n" +
		"  \"pr checks\") exit 0 ;;\n" +
		"  \"pr merge\") echo \"$@\" >> " + mergeLog + "; exit 0 ;;\n" +
		"  api\\ repos/*/commits/*) echo '{\"commit\":{\"message\":\"plain\"}}'; exit 0 ;;\nesac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(repo, "merge.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n    can_merge: true\n    merge_grant: test grant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runLK := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env = repo, sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile, "PATH=" + stubs + ":" + os.Getenv("PATH")})
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	// the gate is not what is under test: record both layers on the head and on release (k1, a branch beneath
	// it) so the ladder reaches the stack check
	for _, sha := range []string{k3, k1} {
		for _, lb := range [][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}} {
			if out, code := runLK("gate", "record", "--repo", repo, "--sha", sha, "--layer", lb[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", lb[1]); code != 0 {
				t.Fatalf("gate record %s: exit %d\n%s", lb[0], code, out)
			}
		}
	}
	setBase := func(b string) {
		t.Helper()
		if err := os.WriteFile(baseFile, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	merges := func() string { b, _ := os.ReadFile(mergeLog); return string(b) }
	reset := func() { _ = os.Remove(mergeLog) }
	merge := func(extra ...string) (string, int) {
		t.Helper()
		reset()
		return runLK(append([]string{"pr", "merge", "--pr", "9", "--repo", repo}, extra...)...)
	}

	setBase("main")

	// (a) no method asked for: a stacked branch turns the default into a merge commit, and the plan says so
	out, code := merge("--dry-run")
	for _, s := range []string{"method:       merge", "method note:  defaulted to merge", "stacked:      2 branch(es)", "on-k1, release"} {
		if code != 0 || !strings.Contains(out, s) {
			t.Fatalf("dry-run with no --method over a stacked branch must plan a merge commit and name the branch (%q missing, exit %d):\n%s", s, code, out)
		}
	}
	if strings.Contains(out, "keystone") {
		t.Fatalf("the PR's own branch is not its dependent, even when it has moved past the head:\n%s", out)
	}
	if out, code = merge(); code != 0 || !strings.Contains(merges(), "--merge") || strings.Contains(merges(), "--squash") {
		t.Fatalf("a real run with no --method over a stacked branch must merge, not squash (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	// (b) an explicit squash or rebase over the stack is refused, names the branch, and never reaches the forge
	for _, m := range []string{"squash", "rebase"} {
		out, code = merge("--method", m, "--user-approved")
		if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "on-k1") || merges() != "" ||
			!strings.Contains(out, "delete them on the remote") || !strings.Contains(out, "git fetch --prune") || strings.Contains(out, "delete/prune") {
			t.Fatalf("--method %s over a stacked branch must be refused and never reach the forge (exit %d, gh saw %q):\n%s", m, code, merges(), out)
		}
	}
	if out, code = merge("--method", "merge"); code != 0 || !strings.Contains(merges(), "--merge") {
		t.Fatalf("an explicit merge commit is allowed over a stack (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	// (c) the range is measured against the PR's own base: against release (at k1) the range is k2..k3, on-k1
	// does not carry k2, so nothing is stacked. Measured against a hardcoded main it would be refused.
	setBase("release")
	out, code = merge("--method", "squash")
	if code != 0 || !strings.Contains(merges(), "--squash") {
		t.Fatalf("a PR based on release is measured against release, where nothing is stacked (exit %d, gh saw %q):\n%s", code, merges(), out)
	}
	if out, code = merge("--dry-run"); !strings.Contains(out, "stacked:      none") || !strings.Contains(out, "method:       squash") {
		t.Fatalf("the plan for a leaf range shows none stacked and the squash default (exit %d):\n%s", code, out)
	}
	// ...and with no --method at all, a real run over the leaf range squashes: the default path, not an explicit ask
	if out, code = merge(); code != 0 || !strings.Contains(merges(), "--squash") {
		t.Fatalf("a real run with no --method over a leaf range must squash (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	// (c2) a base this clone does not have cannot be measured: the plan says UNKNOWN (not "none"), and a squash
	// does not reach the forge. The real run passes --user-approved so the gate and the lineage check are
	// substituted and ONLY the stack check can refuse it; without that the lineage check refuses first and
	// the stack check's fail-closed path on a real run is never reached.
	setBase("ghost")
	if out, code = merge("--dry-run"); !strings.Contains(out, "stacked:      UNKNOWN") {
		t.Fatalf("an unmeasurable stack is UNKNOWN in the plan, never none (exit %d):\n%s", code, out)
	}
	if out, code = merge("--method", "squash", "--user-approved"); code == 0 || merges() != "" || !strings.Contains(out, "cannot determine whether branches are stacked") {
		t.Fatalf("a squash with an unmeasurable stack must be refused BY THE STACK CHECK and not reach the forge, --user-approved or not (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	// (c3) the dependents survive only as fetched remote-tracking refs, never checked out: still seen, still refused
	setBase("main")
	git("branch", "-q", "-D", "on-k1", "release")
	if out, code = merge("--method", "squash"); code == 0 || merges() != "" || !strings.Contains(out, "on-k1") {
		t.Fatalf("dependents held only as remote-tracking refs must still refuse a squash (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	// (d) remove every branch carrying the range (on-k1, and release which also sits on k1) from the remote, and a squash of the same PR over main is allowed
	git("push", "-q", "origin", "--delete", "on-k1", "release")
	git("fetch", "-q", "--prune", "origin")
	out, code = merge("--method", "squash")
	if code != 0 || !strings.Contains(merges(), "--squash") {
		t.Fatalf("with the stacked branch gone a squash is allowed (exit %d, gh saw %q):\n%s", code, merges(), out)
	}
}
