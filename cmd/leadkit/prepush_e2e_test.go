package main

// Ticket 171 through the real binary and real git: the pre-push hook `leadkit init` installs
// refuses a raw push whatever the shell spelled it as, because git runs the hook after the
// shell has finished expanding. These are the spellings the text parser documents as out of
// reach (ticket 165); none of them can be satisfied by reading the command line.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const pp = "pu" + "sh" // the verb, kept out of this file's own text

// prePushFixture builds a governed clone with a reachable bare remote and, when install is
// true, runs `leadkit init --apply --confirm` in it to install the hook.
type prePushFixture struct {
	t     *testing.T
	bin   string
	home  string
	repo  string
	bare  string
	extra []string
}

func newPrePushFixture(t *testing.T, install bool) *prePushFixture {
	t.Helper()
	requireGit(t)
	f := &prePushFixture{t: t, bin: testBinary(t), home: t.TempDir()}
	f.bare = filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(f.bare), "init", "-q", "--bare", f.bare)
	f.repo = filepath.Join(t.TempDir(), "gatedpp")
	initRepoWithOrigin(t, f.repo, "https://github.com/acme/gatedpp.git")
	ppWrite(t, filepath.Join(f.repo, "leadkit.yaml"), "project_id: gatedpp\n")
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-q", "-m", "profile")
	// The scope check reads remote.origin.url (a github.com URL, so the repo is governed); the
	// push itself goes to a local bare repo through pushurl.
	gitRun(t, f.repo, "config", "remote.origin.pushurl", f.bare)
	if install {
		out, code := f.run("init", "--apply", "--confirm",
			"--settings", filepath.Join(t.TempDir(), "settings.json"),
			"--launch-agents-dir", t.TempDir(), "--skills-dir", t.TempDir(),
			"--projects-dir", t.TempDir(), "--skip-launchctl")
		if code != 0 {
			t.Fatalf("leadkit init failed (%d):\n%s", code, out)
		}
		if !strings.Contains(out, "pre-push hook: wrote") {
			t.Fatalf("init did not report writing the hook:\n%s", out)
		}
	}
	return f
}

func (f *prePushFixture) env() []string {
	return append(sandboxedEnv(f.home, f.extra),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS=",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

// run execs the leadkit binary in the clone.
func (f *prePushFixture) run(args ...string) (string, int) {
	f.t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Dir = f.repo
	cmd.Env = f.env()
	return combined(f.t, cmd)
}

// shell runs a command line under bash in the clone, as an agent's Bash tool would.
func (f *prePushFixture) shell(line string) (string, int) {
	f.t.Helper()
	cmd := exec.Command("bash", "-c", line)
	cmd.Dir = f.repo
	cmd.Env = f.env()
	return combined(f.t, cmd)
}

func combined(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v", cmd.Args, err)
	return "", -1
}

func (f *prePushFixture) remoteHas(ref string) bool {
	cmd := exec.Command("git", "--git-dir", f.bare, "rev-parse", "--verify", "-q", ref)
	return cmd.Run() == nil
}

// The plain spelling as a firing control, then spellings of the verb that ticket 165 documents as
// out of the text parser's reach (ANSI-C quoting, brace and IFS expansion, a variable, a git alias)
// and a few more (a line continuation, eval, a command substitution).
func outOfReachSpellings() map[string]string {
	target := "origin HEAD:refs/heads/"
	return map[string]string{
		"plain (firing control)": "git " + pp + " " + target,
		"ANSI-C quoted verb":     "git $'" + pp + "' " + target,
		"brace expansion":        "git {" + pp + ",} " + target,
		"IFS expansion":          "git ${IFS}" + pp + " " + target,
		"verb in a variable":     "g=git; $g " + pp + " " + target,
		"git alias":              "git -c alias.p=" + pp + " p " + target,
		"line continuation":      "git pu\\\nsh " + target,
		"eval of a substitution": "eval \"$(echo git " + pp + " " + target + ")\"",
		"command substitution":   "$(echo git) " + pp + " " + target,
	}
}

func TestCLI_PrePushHook_RefusesWhateverTheShellSpelled(t *testing.T) {
	f := newPrePushFixture(t, true)
	i := 0
	for name, line := range outOfReachSpellings() {
		i++
		branch := "t" + string(rune('a'+i))
		t.Run(name, func(t *testing.T) {
			out, code := f.shell(line + branch)
			if code == 0 {
				t.Fatalf("the push went through; the hook did not run:\n%s", out)
			}
			if !strings.Contains(out, "leadkit pre-push: push refused by the review gate") {
				t.Errorf("refused, but not by the pre-push hook (the cause is not diagnosable):\n%s", out)
			}
			if !strings.Contains(out, "no review record") || !strings.Contains(out, "REVIEW_GATE_BYPASS") {
				t.Errorf("the refusal must name the cause and the override:\n%s", out)
			}
			if f.remoteHas("refs/heads/" + branch) {
				t.Errorf("the ref reached the remote")
			}
		})
	}

	// Allowed control: with both layers recorded for the head, the same unreadable spelling is let through.
	t.Run("a gated push passes, spelled the same way", func(t *testing.T) {
		for _, layer := range [][]string{{"code-review", "reviewer-a"}, {"critique", "critic-b"}} {
			if out, code := f.run("gate", "record", "--layer", layer[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", layer[1]); code != 0 {
				t.Fatalf("gate record %s: %d\n%s", layer[0], code, out)
			}
		}
		out, code := f.shell("git $'" + pp + "' origin HEAD:refs/heads/passes")
		if code != 0 {
			t.Fatalf("a gated head was refused:\n%s", out)
		}
		if !f.remoteHas("refs/heads/passes") {
			t.Error("the push reported success but the ref is absent")
		}
	})
}

func TestCLI_PrePushHook_WithoutTheHookTheSameSpellingsGetThrough(t *testing.T) {
	// The mutation, as a test: leave the installation out and the first group flips to allowed.
	f := newPrePushFixture(t, false)
	i := 0
	for name, line := range outOfReachSpellings() {
		i++
		branch := "u" + string(rune('a'+i))
		t.Run(name, func(t *testing.T) {
			if out, code := f.shell(line + branch); code != 0 {
				t.Fatalf("with no hook installed the push should succeed (it is the hook that refuses):\n%s", out)
			}
		})
	}
}

func TestCLI_PrePushHook_NoVerifyIsTheResidualTheTextLayerRefuses(t *testing.T) {
	f := newPrePushFixture(t, true)
	// The hook cannot stop its own bypass: git skips it, and the push goes through.
	if out, code := f.shell("git " + pp + " --no-verify origin HEAD:refs/heads/skipped"); code != 0 {
		t.Fatalf("expected --no-verify to skip the hook (that is the residual this test documents), got %d:\n%s", code, out)
	}
	// So the text layer refuses it, by name, before it runs.
	reason := ppPushGate(t, f.bin, f.home, f.repo, "git "+pp+" --no-verify origin HEAD:refs/heads/again")
	if !strings.Contains(reason, "skips the pre-push hook") {
		t.Errorf("push-gate did not refuse --no-verify by name:\n%s", reason)
	}
	// And the hooks path redirect, which would turn the hook off wholesale.
	reason = ppPushGate(t, f.bin, f.home, f.repo, "git config core.hooksPath /dev/null")
	if !strings.Contains(reason, "core.hooksPath") {
		t.Errorf("push-gate did not refuse a core.hooksPath assignment:\n%s", reason)
	}
}

func TestCLI_Init_PrePushHookIsOptInToTheCloneItRunsIn(t *testing.T) {
	f := newPrePushFixture(t, false)
	real, err := filepath.EvalSymlinks(f.repo) // git reports the resolved path (macOS /var -> /private/var)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(real, ".git", "hooks", "pre-push")

	// Dry run writes nothing but says what it would do.
	out, code := f.run("init", "--settings", filepath.Join(t.TempDir(), "s.json"), "--launch-agents-dir", t.TempDir(),
		"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir())
	if code != 0 || !strings.Contains(out, "pre-push hook: "+hook+" (will install)") {
		t.Fatalf("dry run (%d) does not plan the hook:\n%s", code, out)
	}
	if _, err := os.Stat(hook); err == nil {
		t.Fatal("a dry run wrote the hook")
	}

	// A foreign hook is left alone, and init says how to chain it.
	ppWrite(t, hook, "#!/bin/sh\necho theirs\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ = f.run("init", "--apply", "--confirm", "--settings", filepath.Join(t.TempDir(), "s.json"), "--launch-agents-dir", t.TempDir(),
		"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir(), "--skip-launchctl")
	if b, _ := os.ReadFile(hook); !strings.Contains(string(b), "theirs") {
		t.Fatalf("init overwrote a foreign hook:\n%s", b)
	}
	if !strings.Contains(out, "left alone") || !strings.Contains(out, "gate check-push") {
		t.Errorf("init does not say how to wire a foreign hook:\n%s", out)
	}

	// A core.hooksPath redirect is reported, and nothing is written into it.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	redirect := t.TempDir()
	gitRun(t, f.repo, "config", "core.hooksPath", redirect)
	out, _ = f.run("init", "--apply", "--confirm", "--settings", filepath.Join(t.TempDir(), "s.json"), "--launch-agents-dir", t.TempDir(),
		"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir(), "--skip-launchctl")
	if !strings.Contains(out, "NOT installed") || !strings.Contains(out, redirect) {
		t.Errorf("init does not report the redirect:\n%s", out)
	}
	if entries, _ := os.ReadDir(redirect); len(entries) != 0 {
		t.Errorf("init wrote into a redirected hooks directory: %v", entries)
	}
}

func TestCLI_Doctor_ReportsThePrePushHookInEachState(t *testing.T) {
	f := newPrePushFixture(t, false)
	check := func() string {
		t.Helper()
		out, _ := f.run("doctor")
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "pre-push-hook") {
				return line
			}
		}
		t.Fatalf("doctor has no pre-push-hook line:\n%s", out)
		return ""
	}
	if got := check(); !strings.Contains(got, "NOT installed") {
		t.Errorf("missing hook: %s", got)
	}
	f.run("init", "--apply", "--confirm", "--settings", filepath.Join(t.TempDir(), "s.json"), "--launch-agents-dir", t.TempDir(),
		"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir(), "--skip-launchctl")
	if got := check(); !strings.Contains(got, "installed:") {
		t.Errorf("installed hook: %s", got)
	}
	redirect := t.TempDir()
	gitRun(t, f.repo, "config", "core.hooksPath", redirect)
	if got := check(); !strings.Contains(got, "redirected") || !strings.Contains(got, redirect) {
		t.Errorf("redirected hook: %s", got)
	}
}

func ppWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ppPushGate runs `leadkit hook push-gate` for command from dir and returns the deny reason
// ("" for an allow).
func ppPushGate(t *testing.T, bin, home, dir, command string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"cwd": dir, "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "hook", "push-gate")
	cmd.Dir = dir
	cmd.Env = append(sandboxedEnv(home, nil), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS=")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook push-gate: %v\n%s", err, out)
	}
	var res struct {
		HookSpecificOutput struct {
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return ""
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	return res.HookSpecificOutput.PermissionDecisionReason
}

// record gates the clone's HEAD with both layers, by two different recorders.
func (f *prePushFixture) record() {
	f.t.Helper()
	for _, layer := range [][]string{{"code-review", "reviewer-a"}, {"critique", "critic-b"}} {
		if out, code := f.run("gate", "record", "--layer", layer[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", layer[1]); code != 0 {
			f.t.Fatalf("gate record %s: %d\n%s", layer[0], code, out)
		}
	}
}

func (f *prePushFixture) initHere() {
	f.t.Helper()
	if out, code := f.run("init", "--apply", "--confirm", "--settings", filepath.Join(f.t.TempDir(), "s.json"), "--launch-agents-dir", f.t.TempDir(),
		"--skills-dir", f.t.TempDir(), "--projects-dir", f.t.TempDir(), "--skip-launchctl"); code != 0 {
		f.t.Fatalf("init: %d\n%s", code, out)
	}
}

// An out-of-scope verdict exempts a repo only if a profile was READ to reach it (ticket 204). The
// hook is the enforcement layer, so it must not read "I could not find the profile" as "ungoverned".
func TestCLI_PrePushHook_AnUnreadableProfileIsNotAnExemption(t *testing.T) {
	push := func(f *prePushFixture, branch string) (string, int) {
		return f.shell("git " + pp + " origin HEAD:refs/heads/" + branch)
	}

	t.Run("an in-repo profile that stops parsing", func(t *testing.T) {
		f := newPrePushFixture(t, true)
		f.record()
		if out, code := push(f, "ok1"); code != 0 {
			t.Fatalf("control: a gated head with a readable profile must pass:\n%s", out)
		}
		ppWrite(t, filepath.Join(f.repo, "leadkit.yaml"), "project_id: [unclosed\n")
		out, code := push(f, "bad1")
		if code == 0 {
			t.Fatalf("the push went through with an unreadable profile; the default stood in as an exemption:\n%s", out)
		}
		for _, w := range []string{"leadkit pre-push", "cannot be known", "leadkit.yaml", "REVIEW_GATE_BYPASS"} {
			if !strings.Contains(out, w) {
				t.Errorf("the refusal does not say %q:\n%s", w, out)
			}
		}
		if f.remoteHas("refs/heads/bad1") {
			t.Error("the ref reached the remote")
		}
		ppWrite(t, filepath.Join(f.repo, "leadkit.yaml"), "project_id: gatedpp\n")
		if out, code := push(f, "ok2"); code != 0 {
			t.Fatalf("with the profile restored the same push must pass:\n%s", out)
		}
	})

	t.Run("an external profile hidden by a changed HOME", func(t *testing.T) {
		f := newPrePushFixture(t, false)
		gitRun(t, f.repo, "rm", "-q", "leadkit.yaml")
		gitRun(t, f.repo, "commit", "-q", "-m", "profile moves to the external config")
		ppWrite(t, filepath.Join(f.home, ".claude", "leadkit", "config", "gatedpp.yaml"), "project_id: gatedpp\n")
		ppWrite(t, filepath.Join(f.repo, ".leadkit", "owners.yaml"), "hard_block: []\n")
		f.initHere()
		f.record()
		if out, code := push(f, "ok1"); code != 0 {
			t.Fatalf("control: a gated head with the external profile must pass:\n%s", out)
		}
		f.extra = []string{"HOME=" + t.TempDir()}
		out, code := push(f, "bad1")
		if code == 0 {
			t.Fatalf("HOME=<elsewhere> hid the profile and the push went through:\n%s", out)
		}
		if !strings.Contains(out, "no profile was found") || !strings.Contains(out, "HOME=") {
			t.Errorf("the refusal must name what was searched and the HOME it used:\n%s", out)
		}
	})

	t.Run("a repo with no sign of leadkit stays out of scope and allowed", func(t *testing.T) {
		f := newPrePushFixture(t, false)
		gitRun(t, f.repo, "rm", "-q", "leadkit.yaml")
		gitRun(t, f.repo, "commit", "-q", "-m", "no profile")
		f.initHere()
		if _, err := os.Stat(filepath.Join(f.repo, ".leadkit")); err == nil {
			t.Fatal("fixture error: init created a .leadkit/ directory, so this repo now has a sign of leadkit")
		}
		if out, code := push(f, "plain"); code != 0 {
			t.Fatalf("an ungoverned repo must stay allowed and silent:\n%s", out)
		}
	})

	t.Run("the bypass still works", func(t *testing.T) {
		f := newPrePushFixture(t, true)
		ppWrite(t, filepath.Join(f.repo, "leadkit.yaml"), "project_id: [unclosed\n")
		if _, code := push(f, "bad"); code == 0 {
			t.Fatal("fixture error: the unreadable profile is not refused")
		}
		out, code := f.shell("REVIEW_GATE_BYPASS=deliberate git " + pp + " origin HEAD:refs/heads/bypassed")
		if code != 0 {
			t.Fatalf("a deliberate bypass must be honoured:\n%s", out)
		}
	})
}

func TestCLI_PrePushHook_AppliesToALinkedWorktree(t *testing.T) {
	// The hook lives in the common git dir, so a worktree of the clone runs it too.
	f := newPrePushFixture(t, true)
	wt := filepath.Join(t.TempDir(), "linked")
	gitRun(t, f.repo, "worktree", "add", "-q", "-b", "linked", wt)
	run := func(branch string) (string, int) {
		cmd := exec.Command("bash", "-c", "git "+pp+" origin HEAD:refs/heads/"+branch)
		cmd.Dir = wt
		cmd.Env = f.env()
		return combined(t, cmd)
	}
	out, code := run("fromwt1")
	if code == 0 || !strings.Contains(out, "leadkit pre-push: push refused by the review gate") {
		t.Fatalf("an ungated push from a linked worktree must be refused by the hook (exit %d):\n%s", code, out)
	}
	f.record() // the worktree's HEAD is the clone's HEAD
	if out, code := run("fromwt2"); code != 0 {
		t.Fatalf("a gated push from a linked worktree must pass:\n%s", out)
	}
}

// A release-tagging workflow publishes ANNOTATED tags. git gives the hook the tag object's sha, which
// nobody recorded anything against, so judging that sha refuses every annotated tag however fully its
// commit is reviewed (and with it --tags and --follow-tags). The hook must judge the commit the tag
// peels to: refused while that commit is unrecorded, naming the tag and the commit, allowed once it is.
func TestCLI_PrePushHook_AnAnnotatedTagIsJudgedByItsCommit(t *testing.T) {
	f := newPrePushFixture(t, true)
	gitRun(t, f.repo, "tag", "-a", "-m", "release", "v1")
	gitRun(t, f.repo, "tag", "light") // a lightweight tag at the same commit is the control

	commit := strings.TrimSpace(runGitOut(t, f.repo, "rev-parse", "HEAD"))
	tagObj := strings.TrimSpace(runGitOut(t, f.repo, "rev-parse", "v1"))
	if commit == tagObj {
		t.Fatal("fixture: an annotated tag must be its own object")
	}

	attempts := map[string]string{
		"the annotated tag":      "git " + pp + " origin v1",
		"--tags":                 "git " + pp + " --tags origin",
		"--follow-tags a branch": "git " + pp + " --follow-tags origin HEAD:refs/heads/ft",
	}
	for name, line := range attempts {
		out, code := f.shell(line)
		if code == 0 {
			t.Errorf("%s: an unrecorded commit's tag went through:\n%s", name, out)
			continue
		}
		if !strings.Contains(out, tagObj+" -> commit "+commit) {
			t.Errorf("%s: the refusal must name the tag object and the commit it peels to:\n%s", name, out)
		}
	}
	if f.remoteHas("refs/tags/v1") {
		t.Fatal("a refused tag reached the remote")
	}

	for _, layer := range [][]string{{"code-review", "reviewer-a"}, {"critique", "critic-b"}} {
		if out, code := f.run("gate", "record", "--layer", layer[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", layer[1]); code != 0 {
			t.Fatalf("gate record %s: %d\n%s", layer[0], code, out)
		}
	}
	for name, line := range map[string]string{
		"the lightweight tag":    "git " + pp + " origin light",
		"the annotated tag":      "git " + pp + " origin v1",
		"--follow-tags a branch": "git " + pp + " --follow-tags origin HEAD:refs/heads/ft",
	} {
		if out, code := f.shell(line); code != 0 {
			t.Errorf("%s: refused although its commit is fully recorded:\n%s", name, out)
		}
	}
	if !f.remoteHas("refs/tags/v1") || !f.remoteHas("refs/tags/light") {
		t.Error("the tags reported pushed but are absent from the remote")
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// The pre-push hook is one of the items init says it would change: with the hook missing the count is one
// higher than with a hook init leaves alone, everything else equal. Without this the count (the only thing
// that tells the operator how much `--apply --confirm` will do) could drop the hook silently.
func TestCLI_Init_TheDryCountIncludesThePrePushHook(t *testing.T) {
	f := newPrePushFixture(t, false)
	real, err := filepath.EvalSymlinks(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(real, ".git", "hooks", "pre-push")
	count := func() int {
		out, code := f.run("init", "--apply", "--settings", filepath.Join(t.TempDir(), "s.json"), "--launch-agents-dir", t.TempDir(),
			"--skills-dir", t.TempDir(), "--projects-dir", t.TempDir())
		m := regexp.MustCompile(`would change (\d+) item`).FindStringSubmatch(out)
		if code != 1 || m == nil {
			t.Fatalf("init --apply without --confirm must refuse and count (exit %d):\n%s", code, out)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	missing := count()
	ppWrite(t, hook, "#!/bin/sh\necho theirs\n") // a foreign hook is left alone, so it is not a change
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	if left := count(); missing != left+1 {
		t.Errorf("a missing hook is one item init would change: count %d with it missing, %d with a foreign hook", missing, left)
	}
}
