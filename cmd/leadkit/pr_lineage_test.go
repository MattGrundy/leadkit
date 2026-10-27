package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_PRCreate_AGatedTipOverUngatedCommitsIsRefused reproduces the incident through the real
// binary: a branch whose tip has a complete gate record, built on a branch with none. `pr create`
// must refuse although the tip alone reports `gate: ok=true`, because the commits beneath the tip
// are unreviewed. The refusal is the point, and gating the branch beneath is what clears it.
func TestCLI_PRCreate_AGatedTipOverUngatedCommitsIsRefused(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := t.TempDir()
	env := func(extra ...string) []string { return sandboxedEnv(home, extra) }
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir, c.Env = repo, append(env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(msg string) string {
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

	git("checkout", "-q", "-b", "keystone")
	commit("k1")
	commit("k2")
	keystone := commit("k3")
	git("checkout", "-q", "-b", "gated-tip")
	tip := commit("a1")

	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(repo, "body.md")
	if err := os.WriteFile(body, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir, c.Env = repo, env("LEADKIT_PROFILE="+profile)
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
	gateFully := func(sha string) {
		t.Helper()
		// in order: critique is chained behind code-review
		for _, lb := range [][2]string{{"code-review", "cr-1"}, {"critique", "crit-1"}} {
			if out, code := run("gate", "record", "--repo", repo, "--sha", sha, "--layer", lb[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", lb[1]); code != 0 {
				t.Fatalf("gate record %s: exit %d\n%s", lb[0], code, out)
			}
		}
	}
	create := func(extra ...string) (string, int) {
		return run(append([]string{"pr", "create", "--base", "main", "--head", "gated-tip", "--title", "t", "--body-file", body, "--repo", repo}, extra...)...)
	}

	gateFully(tip) // the tip IS fully gated: a check of the tip alone says yes

	out, code := create("--dry-run")
	if !strings.Contains(out, "gate:         ok=true") {
		t.Fatalf("precondition: the head's own gate is green:\n%s", out)
	}
	if !strings.Contains(out, "lineage:      ok=false") || !strings.Contains(out, "keystone@"+keystone[:8]) || !strings.Contains(out, "3 commit(s)") {
		t.Fatalf("the plan must show the ungated branch beneath (exit %d):\n%s", code, out)
	}
	out, code = create()
	if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "lineage is not") {
		t.Fatalf("pr create must refuse a gated tip over ungated commits (exit %d):\n%s", code, out)
	}

	// `pr merge` lands the same commits, so it must refuse the same PR. It is wired separately from
	// `pr create` (the head comes from the forge, the base from the PR), so it is pinned separately:
	// a fake gh describes an open, clean, green PR whose head is the gated tip, and records any merge.
	stubs := t.TempDir()
	mergeLog := filepath.Join(stubs, "merges.log")
	view := `{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","headRefOid":"` + tip + `","title":"t","baseRefName":"main"}`
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"  \"pr view\") case \"$*\" in *commits*) echo '{\"title\":\"t\",\"number\":9,\"commits\":[{\"messageHeadline\":\"h\",\"messageBody\":\"\"}]}' ;; *) echo '" + view + "' ;; esac; exit 0 ;;\n" +
		"  \"pr checks\") exit 0 ;;\n" +
		"  \"pr merge\") echo \"$@\" >> " + mergeLog + "; exit 0 ;;\n" +
		"  api\\ repos/*/commits/*) echo '{\"commit\":{\"message\":\"plain\"}}'; exit 0 ;;\nesac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mergeProfile := filepath.Join(repo, "merge.yaml")
	if err := os.WriteFile(mergeProfile, []byte("queue_root: queue\nstore_root: tasks\nauthority:\n  forge.git:\n    forge: github\n    can_open_pr: true\n    can_merge: true\n    merge_grant: test grant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merge := func() (string, int) {
		t.Helper()
		c := exec.Command(bin, "pr", "merge", "--pr", "9", "--repo", repo)
		c.Dir, c.Env = repo, env("LEADKIT_PROFILE="+mergeProfile, "PATH="+stubs+":"+os.Getenv("PATH"))
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

	out, code = merge()
	if code == 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "lineage is not") || !strings.Contains(out, "keystone@"+keystone[:8]) || merges() != "" {
		t.Fatalf("pr merge must refuse a gated tip over ungated commits and never reach the forge (exit %d, gh saw %q):\n%s", code, merges(), out)
	}

	gateFully(keystone) // the remedy the refusal names
	out, code = create("--dry-run")
	if code != 0 || !strings.Contains(out, "lineage:      ok=true") || !strings.Contains(out, "would run:") {
		t.Fatalf("with the branch beneath gated the PR is allowed (exit %d):\n%s", code, out)
	}
	out, code = merge()
	if code != 0 || !strings.Contains(merges(), "pr merge 9") {
		t.Fatalf("with the branch beneath gated the merge is allowed (exit %d, gh saw %q):\n%s", code, merges(), out)
	}
}
