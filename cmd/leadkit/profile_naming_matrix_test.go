package main

// Ticket 319. The external profile is found by the ORIGIN's repository name. Rename the remote and
// the lookup misses a profile that is sitting in the config directory, the repo then reads as
// governed by nothing, and an out-of-scope verdict allows. The naming check
// (config.ExternalConfigsNamingRepoByRemote) must run whether or not the repo has a .leadkit/
// directory, and the three enforcers (the PreToolUse hook, the git pre-push check and `leadkit
// push`) must agree. This pins the 16 cells of the measurement that found it:
//
//	mode      enforce | advisory
//	.leadkit  present | absent
//	remote    renamed (origin -> x) | removed (origin gone, a different remote added)
//	clone     named like the profile | named otherwise
//
// The profile (proj.yaml) has no project_origin. A cell holds what each enforcer DOES: "refuse",
// "allow", or "allow+report" (an allow that says the repo is unconfigured).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type namingCell struct {
	mode            string // enforce | advisory
	leadkit         bool
	op              string // rename | remove
	sameNamed       bool   // the clone directory is called "proj", as the profile is
	hook, pre, verb string
	why             string
}

// The cells. Where two enforcers differ the cell says why; the rows marked residual are the cases
// this check cannot fix and are pinned as they are so that a fix flips them on purpose.
var namingCells = []namingCell{
	// Differently-named clone, remote renamed: the remote keeps the URL, so the profile is named
	// by it. Refused by all three, in either mode, with or without .leadkit/. (The first two cells,
	// where .leadkit/ is absent, were the bug: the hook and the pre-push check allowed them.)
	{"enforce", false, "rename", false, "refuse", "refuse", "refuse", "profile found by the remote's URL"},
	{"advisory", false, "rename", false, "refuse", "refuse", "refuse", "profile found by the remote's URL"},
	{"enforce", true, "rename", false, "refuse", "refuse", "refuse", "profile found by the remote's URL"},
	{"advisory", true, "rename", false, "refuse", "refuse", "refuse", "profile found by the remote's URL"},

	// Same-named clone, remote renamed or removed: the lookup falls back to the directory name and
	// finds the profile, whose scope cannot be confirmed without an origin (ticket 228). Enforce
	// refuses everywhere. Advisory is that ticket's documented fail-open for an unresolvable scope:
	// the hook and the pre-push check allow, `leadkit push` refuses. NOT changed here.
	{"enforce", false, "rename", true, "refuse", "refuse", "refuse", "228: scope unresolved"},
	{"advisory", false, "rename", true, "allow", "allow", "refuse", "228 advisory fail-open"},
	{"enforce", true, "rename", true, "refuse", "refuse", "refuse", "228: scope unresolved"},
	{"advisory", true, "rename", true, "allow", "allow", "refuse", "228 advisory fail-open"},
	{"enforce", false, "remove", true, "refuse", "refuse", "refuse", "228: scope unresolved"},
	{"advisory", false, "remove", true, "allow", "allow", "refuse", "228 advisory fail-open"},
	{"enforce", true, "remove", true, "refuse", "refuse", "refuse", "228: scope unresolved"},
	{"advisory", true, "remove", true, "allow", "allow", "refuse", "228 advisory fail-open"},

	// RESIDUAL: the origin removed, the clone differently named, no project_origin. The repo shares
	// no handle with its profile, so nothing client-side can find it. Without .leadkit/ nothing says
	// it is a leadkit project at all. With it, the repo is unconfigured and says so.
	{"enforce", false, "remove", false, "allow", "allow", "refuse", "residual: no handle"},
	{"advisory", false, "remove", false, "allow", "allow", "refuse", "residual: no handle"},
	{"enforce", true, "remove", false, "allow+report", "allow+report", "allow+report", "residual: no handle"},
	{"advisory", true, "remove", false, "allow+report", "allow+report", "allow+report", "residual: no handle"},
}

type namingWorld struct {
	home, work, repo, bare, sha string
}

func newNamingWorld(t *testing.T, c namingCell) namingWorld {
	t.Helper()
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := namingWorld{home: filepath.Join(root, "home"), work: root, bare: filepath.Join(root, "bare.git")}
	mustWrite(t, filepath.Join(w.home, ".claude", "leadkit", "config", "proj.yaml"),
		fmt.Sprintf("project_id: proj\nrecords_dir: %s\nguards:\n  default_mode: %s\n", filepath.Join(root, "records"), c.mode))
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", w.bare)
	dir := "clone"
	if c.sameNamed {
		dir = "proj"
	}
	w.repo = filepath.Join(root, dir)
	if err := os.MkdirAll(w.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, w.repo, "init", "-q", "-b", "main")
	gitRun(t, w.repo, "config", "user.email", "a@b")
	gitRun(t, w.repo, "config", "user.name", "n")
	gitRun(t, w.repo, "remote", "add", "origin", "https://github.com/acme/proj.git")
	gitRun(t, w.repo, "config", "remote.origin.pushurl", w.bare)
	mustWrite(t, filepath.Join(w.repo, "seed"), "x")
	gitRun(t, w.repo, "add", "-A")
	gitRun(t, w.repo, "commit", "-q", "-m", "init")
	if c.leadkit {
		mustWrite(t, filepath.Join(w.repo, ".leadkit", "decisions.jsonl"), "{}\n")
	}
	switch c.op {
	case "rename":
		gitRun(t, w.repo, "remote", "rename", "origin", "x")
	case "remove":
		gitRun(t, w.repo, "remote", "remove", "origin")
		gitRun(t, w.repo, "remote", "add", "x", w.bare)
	}
	out, err := exec.Command("git", "-C", w.repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	w.sha = strings.TrimSpace(string(out))
	return w
}

func (w namingWorld) env(extra ...string) []string {
	return append(sandboxedEnv(w.home, []string{"LEADKIT_PROFILE="}), append([]string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS="}, extra...)...)
}

// run execs bin in the clone and returns stdout, stderr and the exit code.
func (w namingWorld) run(t *testing.T, bin, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = w.repo
	cmd.Env = w.env()
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("exec %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return o.String(), e.String(), code
}

// outcome reduces an enforcer's result to refuse | allow | allow+report.
func outcomeOf(refused, reported bool) string {
	switch {
	case refused:
		return "refuse"
	case reported:
		return "allow+report"
	}
	return "allow"
}

func TestNamingMatrix_SixteenCellsAcrossTheThreeEnforcers(t *testing.T) {
	bin := testBinary(t)
	for _, c := range namingCells {
		name := fmt.Sprintf("%s/leadkit=%v/%s/sameNamed=%v", c.mode, c.leadkit, c.op, c.sameNamed)
		t.Run(name, func(t *testing.T) {
			w := newNamingWorld(t, c)

			payload, err := json.Marshal(map[string]any{"cwd": w.repo, "tool_name": "Bash", "tool_input": map[string]string{"command": "git push x main"}})
			if err != nil {
				t.Fatal(err)
			}
			out, errOut, code := w.run(t, bin, string(payload), "hook", "push-gate")
			if code != 0 {
				t.Fatalf("hook exited %d: %s %s", code, out, errOut)
			}
			hook := outcomeOf(strings.Contains(out, `"permissionDecision":"deny"`), strings.Contains(out, "UNCONFIGURED"))

			stdin := "refs/heads/main " + w.sha + " refs/heads/main 0000000000000000000000000000000000000000\n"
			out, errOut, code = w.run(t, bin, stdin, "gate", "check-push")
			pre := outcomeOf(code != 0, strings.Contains(out+errOut, "UNCONFIGURED"))

			out, errOut, code = w.run(t, bin, "", "push", "--repo", w.repo, "--branch", "main", "--remote", "x", "--dry-run")
			verb := outcomeOf(code != 0, strings.Contains(out+errOut, "UNCONFIGURED"))

			if hook != c.hook || pre != c.pre || verb != c.verb {
				t.Errorf("%s\n  hook: got %s, want %s\n  pre-push: got %s, want %s\n  leadkit push: got %s, want %s", c.why, hook, c.hook, pre, c.pre, verb, c.verb)
			}
		})
	}
}
