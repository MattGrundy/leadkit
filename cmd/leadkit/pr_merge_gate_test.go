package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_PRMerge_AFailingGateBlocksTheMerge pins the review gate on the MERGE path, the one place the
// verdict of prcreate.GateReady reaches DecideMerge. Nothing else did: forcing the gate result true in
// runPRMerge left every test green, because every other merge test records both layers first, so a gate
// that always said yes was indistinguishable from the real one.
//
// The refusals come first and carry the weight. A fake gh describes an open, clean, green, single-commit
// PR, so every other rung of the ladder (including the lineage check, which a lone commit satisfies with
// no record at all) passes, and the gate is the ONLY thing that can refuse. The forge must never be reached.
// The final case, a satisfied gate that merges, is the control that shows the refusals were the gate's.
func TestCLI_PRMerge_AFailingGateBlocksTheMerge(t *testing.T) {
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
	git("push", "-q", "origin", "main")
	git("fetch", "-q", "origin")
	git("checkout", "-q", "-b", "feature")
	head := commit("the only commit beyond main")
	git("push", "-q", "origin", "feature")
	git("fetch", "-q", "origin")

	stubs := t.TempDir()
	mergeLog := filepath.Join(stubs, "merges.log")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in *commits*) echo '{\"title\":\"t\",\"number\":9,\"commits\":[{\"messageHeadline\":\"h\",\"messageBody\":\"\"}]}' ;; " +
		"*) echo '{\"state\":\"OPEN\",\"isDraft\":false,\"mergeable\":\"MERGEABLE\",\"mergeStateStatus\":\"CLEAN\",\"headRefOid\":\"" + head + "\",\"headRefName\":\"feature\",\"title\":\"t\",\"baseRefName\":\"main\"}' ;; esac; exit 0 ;;\n" +
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
	merges := func() string { b, _ := os.ReadFile(mergeLog); return string(b) }
	merge := func(extra ...string) (string, int) {
		t.Helper()
		_ = os.Remove(mergeLog)
		return runLK(append([]string{"pr", "merge", "--pr", "9", "--repo", repo}, extra...)...)
	}
	record := func(layer, by string) {
		t.Helper()
		if out, code := runLK("gate", "record", "--repo", repo, "--sha", head, "--layer", layer, "--verdict", "ready", "--no-mutant", "test fixture", "--by", by); code != 0 {
			t.Fatalf("gate record %s: exit %d\n%s", layer, code, out)
		}
	}
	refused := func(what, mustName string) {
		t.Helper()
		out, code := merge()
		if code == 0 || merges() != "" || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "not gated") || !strings.Contains(out, mustName) {
			t.Fatalf("%s: the merge must be refused BY THE GATE (naming %q) and never reach the forge (exit %d, gh saw %q):\n%s", what, mustName, code, merges(), out)
		}
		plan, _ := merge("--dry-run")
		if !strings.Contains(plan, "review-gate:  ok=false") {
			t.Fatalf("%s: the plan must report the gate as not ok, never ok=true:\n%s", what, plan)
		}
	}

	// no record at all
	refused("no review record", "no review record")
	// one layer ready is not 2/2: critique is still missing
	record("code-review", "cr-1")
	refused("code-review alone", "critique=MISSING")

	// control: the second layer lands, and the same PR merges, saying it was the gate that let it
	record("critique", "crit-1")
	out, code := merge()
	if code != 0 || !strings.Contains(merges(), "--squash") || !strings.Contains(out, "review-gate: ok=true") {
		t.Fatalf("with both layers ready the same PR must merge (exit %d, gh saw %q):\n%s", code, merges(), out)
	}
	// ...and what reaches the forge is pinned to the head the gate judged (ticket 241): a push to the
	// branch after the ladder ran makes gh refuse instead of landing commits nobody gated.
	if !strings.Contains(merges(), "--match-head-commit "+head) {
		t.Fatalf("the merge gh was run with is not pinned to the gated head %s: %q", head, merges())
	}
}
