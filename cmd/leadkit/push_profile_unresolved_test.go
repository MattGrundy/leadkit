package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 197. With no profile found, the built-in default governs nothing, so a push read as "out
// of scope" and went ungated under the label "by policy". Three cases must stay apart:
// a profile read that governs the repo (gated), a profile read that does not (allowed), and no
// profile read at all (refused).

func TestPushRun_NoProfileFound_IsRefusedNotCalledOutOfScope(t *testing.T) {
	repo := t.TempDir()
	// The same out-of-scope gate outcome as TestPushRun_OutOfScope_PushesWithNote...
	deps, runner, _, out, errb := pushHarness(t, "git@github.com:someorg/random.git", []string{pushEnforcedSubstr}, nil)
	// ...but no profile was read.
	deps.noProfile = func(string) string { return "no leadkit profile was found (looked for X); HOME=\"/tmp\"" }
	t.Setenv("REVIEW_GATE_BYPASS", "")

	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 1 {
		t.Fatalf("an unresolved profile must refuse (exit 1), got %d\n%s", code, errb.String())
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("a refused push must not run git push, got %+v", runner.calls)
	}
	msg := errb.String()
	for _, want := range []string{"cannot be known", "no leadkit profile was found", `HOME="/tmp"`, "REVIEW_GATE_BYPASS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must contain %q, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "by policy") || strings.Contains(msg, "authority") {
		t.Errorf("a resolution failure must not be attributed to policy or advise a grant, got: %s", msg)
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be reported pushed, got: %s", out.String())
	}

	// A deliberate bypass still works, loudly, and says it did not know.
	t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
	deps2, runner2, _, _, errb2 := pushHarness(t, "git@github.com:someorg/random.git", []string{pushEnforcedSubstr}, nil)
	deps2.noProfile = deps.noProfile
	if code := pushRun(deps2, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"}); code != 0 || len(runner2.pushCalls()) != 1 {
		t.Fatalf("a set REVIEW_GATE_BYPASS must allow the push, got exit %d, pushes %d", code, len(runner2.pushCalls()))
	}
	if !strings.Contains(errb2.String(), "not known to be gated") {
		t.Errorf("the bypass must be announced, got: %s", errb2.String())
	}
}

// A profile that WAS read and does not govern the repo is a decision, and stays allowed.
func TestPushRun_ProfileFoundButRepoOutOfScope_StillPushes(t *testing.T) {
	repo := t.TempDir()
	deps, runner, _, _, errb := pushHarness(t, "git@github.com:someorg/random.git", []string{pushEnforcedSubstr}, nil)
	deps.noProfile = func(string) string { return "" }
	if code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"}); code != 0 || len(runner.pushCalls()) != 1 {
		t.Fatalf("a genuinely out-of-scope repo must push, got exit %d, pushes %d\n%s", code, len(runner.pushCalls()), errb.String())
	}
	if !strings.Contains(errb.String(), "out of scope") {
		t.Errorf("want the out-of-scope note, got: %s", errb.String())
	}
}

// A gated push never asks the question: the profile check only judges "out of scope".
func TestPushRun_GatedPush_NeverConsultsTheProfileCheck(t *testing.T) {
	repo := t.TempDir()
	deps, _, recordsDir, _, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	writePushRecord(t, recordsDir, pushFullSHA)
	deps.noProfile = func(string) string { t.Error("noProfile consulted for a gated push"); return "boom" }
	if code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"}); code != 0 {
		t.Fatalf("gated push should succeed, got %d", code)
	}
}

// End to end through the real binary and the real discovery. The fixtures: a profiled session
// project, a profiled target, an unprofiled repo, and a directory under no project.
func TestCLIPush_ProfileDiscovery_KeepsTheThreeCasesApart(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\n")
	home := t.TempDir()
	push := func(cwd, repo string) (string, int) {
		return atCwd(t, bin, home, cwd, "push", "--repo", repo, "--branch", "feat/x", "--dry-run")
	}

	// profile resolvable + in scope: gated (and, with no record, blocked), from any cwd.
	for name, cwd := range map[string]string{"neutral dir": m.neutral, "session project": m.session.dir} {
		out, code := push(cwd, m.target.dir)
		if code != 1 || !strings.Contains(out, "BLOCKED") {
			t.Errorf("in-scope target from %s: want the gate to BLOCK, got exit %d:\n%s", name, code, out)
		}
	}

	// resolvable + genuinely out of scope: the session's profile was read, the unprofiled clone is
	// not governed by it: allowed, and said plainly.
	out, code := push(m.session.dir, m.unprofiled.dir)
	if code != 0 || !strings.Contains(out, "out of scope of the profile in effect") {
		t.Errorf("out-of-scope under a read profile must be allowed, got exit %d:\n%s", code, out)
	}

	// unresolvable: neither the target nor the session has a profile anywhere. REFUSED, naming
	// what was searched and the HOME, and not calling it a policy.
	out, code = push(m.neutral, m.unprofiled.dir)
	if code != 1 || !strings.Contains(out, "cannot be known") || !strings.Contains(out, "HOME=") || strings.Contains(out, "by policy") {
		t.Errorf("an unresolved profile must be refused with the search named, got exit %d:\n%s", code, out)
	}
}

// $LEADKIT_PROFILE names the SESSION's profile, never the target's: pointing it at nothing leaves a
// target with no profile of its own unresolved, and pointing nothing at the target's external
// config location keeps it so; once that external config exists the target IS governed.
func TestCLIPush_TargetProfileComesFromTheTargetNotFromTheEnvironment(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\n")
	home := t.TempDir()
	run := func(env ...string) (string, int) {
		cmd := exec.Command(bin, "push", "--repo", m.unprofiled.dir, "--branch", "feat/x", "--dry-run")
		cmd.Dir = m.neutral
		cmd.Env = sandboxedEnv(home, env)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out), 0
	}

	out, code := run("LEADKIT_PROFILE=/nonexistent/x.yaml")
	if code != 1 || !strings.Contains(out, "cannot be known") {
		t.Errorf("a $LEADKIT_PROFILE that names nothing is not a profile for the target, got exit %d:\n%s", code, out)
	}

	// The target's external config, found by its origin's repo name under HOME, governs it.
	cfgDir := filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "unprofiled.yaml"), []byte("queue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run("LEADKIT_PROFILE=")
	if code != 1 || strings.Contains(out, "cannot be known") || !strings.Contains(out, "BLOCKED") {
		t.Errorf("a target with an external config is governed by it (gated, no record: blocked), got exit %d:\n%s", code, out)
	}

	// A repo with no origin has nothing for the gate to scope on. With a profile read for it, that is
	// "cannot confirm" and the explicit push is refused (ticket 228); only a repo with no profile and no
	// origin is out of scope outright.
	bare := filepath.Join(m.neutral, "norigin")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, bare, "init", "-q")
	gitIn(t, bare, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "i")
	cmdNo := func() (string, int) {
		cmd := exec.Command(bin, "push", "--repo", bare, "--branch", "feat/x", "--dry-run")
		cmd.Dir = m.neutral
		cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE="})
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out), 0
	}
	if out, code := cmdNo(); code != 1 || !strings.Contains(out, "cannot be known") {
		t.Fatalf("control: with no profile for it, the origin-less repo is unresolved, got exit %d:\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "norigin.yaml"), []byte("queue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := cmdNo(); code != 1 || !strings.Contains(out, "no remote named `origin`") || !strings.Contains(out, "git remote rename <name> origin") || strings.Contains(out, "out of scope of the profile in effect") {
		t.Errorf("a profile that was read for a repo whose origin cannot be read cannot confirm scope, so the explicit push is refused, got exit %d:\n%s", code, out)
	}
}

// The third state: a profile that EXISTS and was not accepted (here it will not parse; the loader
// also turns one away on a project_origin mismatch). Built-in defaults govern in its place, so it
// read as "out of scope" exactly like a missing profile, even with a healthy session profile beside
// it. It is refused too, and the session's profile cannot vouch for it.
func TestCLIPush_AProfileThatExistsButWasNotAccepted_IsRefusedNotCalledOutOfScope(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\n")
	broken := cliProject(t, t.TempDir(), "brokenproj", "git@example.com:org/brokenproj.git", "queue_root: [unclosed\n")
	home := t.TempDir()

	out, code := atCwd(t, bin, home, m.session.dir, "push", "--repo", broken.dir, "--branch", "feat/x", "--dry-run")
	if code != 1 || !strings.Contains(out, "was not accepted") || !strings.Contains(out, "cannot be known") {
		t.Fatalf("a profile that exists but was refused must refuse the push, got exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "out of scope") {
		t.Errorf("it must not be reported as out of scope: %s", out)
	}
}

func TestRedactCredentials(t *testing.T) {
	cases := map[string]string{
		`origin "https://user:ghp_secret@github.com/acme/widgets.git" mismatch`: `origin "https://***@github.com/acme/widgets.git" mismatch`,
		`ssh://git:tok@host/x.git and https://a@h/y`:                            `ssh://***@host/x.git and https://***@h/y`,
		`git@github.com:acme/widgets.git`:                                       `git@github.com:acme/widgets.git`, // scp-style has no userinfo secret
		`no url here`:                                                           `no url here`,
	}
	for in, want := range cases {
		if got := redactCredentials(in); got != want {
			t.Errorf("redactCredentials(%q) = %q, want %q", in, got, want)
		}
	}
	// And it is applied to the reason a refused profile gives.
	st := refusedProfile("/p.yaml", errors.New(`project_origin mismatch: origin "https://u:tok@github.com/o/r.git"`))
	if strings.Contains(st.Why, "tok") || !st.Refused || st.Found {
		t.Errorf("refusedProfile must redact and be Refused, got %+v", st)
	}
}
