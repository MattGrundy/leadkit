package main

// Ticket 302 through the real binary and the real classifier: a repo with a .leadkit/ runtime
// directory and NO profile anywhere (none in the repo, none external) is UNCONFIGURED. Every
// enforcement point allows it AND says so; it does not refuse, because that blocked a live CVE
// patch, and it does not stay silent, because a gate that does not know whether it governs a repo
// has not decided the repo is exempt.
//
// The refusals that must survive are asserted next to it: a profile that exists and was not
// accepted, and a HOME that holds no leadkit config directory (which is what a profile hidden by a
// changed HOME looks like).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const unconfiguredMarker = "[push-gate state: unconfigured]"

// unconfiguredRepo is a clone with an origin, a .leadkit/ runtime directory, and no profile.
func unconfiguredRepo(t *testing.T, name string) string {
	t.Helper()
	requireGit(t)
	repo := filepath.Join(t.TempDir(), name)
	initRepoWithOrigin(t, repo, "https://github.com/acme/"+name+".git")
	mustWrite(t, filepath.Join(repo, ".leadkit", "decisions.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(repo, ".leadkit", "push-violations.jsonl"), "{}\n")
	return repo
}

// unconfiguredRepoAt is unconfiguredRepo with a clone directory that is not called what the origin is.
func unconfiguredRepoAt(t *testing.T, dirName, originName string) string {
	t.Helper()
	requireGit(t)
	repo := filepath.Join(t.TempDir(), dirName)
	initRepoWithOrigin(t, repo, "https://github.com/acme/"+originName+".git")
	mustWrite(t, filepath.Join(repo, ".leadkit", "decisions.jsonl"), "{}\n")
	return repo
}

// configuredHome is a HOME where leadkit keeps external configs for other projects.
func configuredHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "someone-else.yaml"), "project_id: someone-else\n")
	return home
}

// hookRun runs `leadkit hook push-gate` for command from dir and returns the deny reason ("" for
// an allow), the report the allow carries and the exit code. The report is read from where the
// harness reads it: the systemMessage on stdout, which must be matched by the additionalContext
// the model is given and must carry no permissionDecision. A hook that wrote only to stderr
// returns that text instead, so a test that expects silence still sees it.
func hookRun(t *testing.T, bin, home, dir, command string) (deny, stderr string, code int) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"cwd": dir, "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "hook", "push-gate")
	cmd.Dir = dir
	cmd.Env = append(sandboxedEnv(home, nil), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS=")
	cmd.Stdin = bytes.NewReader(payload)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(out)) > 0 {
		var res struct {
			SystemMessage      string `json:"systemMessage"`
			HookSpecificOutput struct {
				PermissionDecision       string `json:"permissionDecision"`
				PermissionDecisionReason string `json:"permissionDecisionReason"`
				AdditionalContext        string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("unexpected hook output (%v): %s", err, out)
		}
		switch {
		case res.HookSpecificOutput.PermissionDecision == "deny":
			deny = res.HookSpecificOutput.PermissionDecisionReason
		case res.HookSpecificOutput.PermissionDecision != "" || res.SystemMessage == "" || res.SystemMessage != res.HookSpecificOutput.AdditionalContext:
			t.Fatalf("an allow that reports must be systemMessage == additionalContext with no permissionDecision: %s", out)
		default:
			return "", res.SystemMessage, code
		}
	}
	return deny, errb.String(), code
}

func TestCLI_PushGate_UnconfiguredRepoIsAllowedAndSaysSo(t *testing.T) {
	bin := testBinary(t)
	home := configuredHome(t)
	repo := unconfiguredRepo(t, "cvepatch")

	for _, command := range []string{"git push", "git push origin main", "git push --all", "git -C " + repo + " push"} {
		deny, stderr, code := hookRun(t, bin, home, repo, command)
		if deny != "" || code != 0 {
			t.Fatalf("%q: an unconfigured repo must not block a push (exit %d): %s", command, code, deny)
		}
		for _, want := range []string{"UNCONFIGURED", unconfiguredMarker, "cvepatch", "leadkit config init", "no leadkit profile exists"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%q: the allow must state the state; stderr lacks %q:\n%s", command, want, stderr)
			}
		}
	}
}

// What stays refused. Each of these is a repo where "gated or not" is a question with an answer
// that this machine could not read, which is a different thing from there being no policy.
func TestCLI_PushGate_UnconfiguredDoesNotSoftenTheRefusals(t *testing.T) {
	bin := testBinary(t)

	t.Run("a HOME with no leadkit config directory is a profile that may be hidden", func(t *testing.T) {
		repo := unconfiguredRepo(t, "hidden")
		home := t.TempDir() // nothing under it: what a changed HOME looks like
		deny, stderr, _ := hookRun(t, bin, home, repo, "git push")
		if deny == "" || !strings.Contains(deny, "cannot be known") || !strings.Contains(deny, "holds no leadkit config directory") {
			t.Fatalf("must refuse, saying why; got deny=%q stderr=%q", deny, stderr)
		}
		if strings.Contains(stderr, "UNCONFIGURED") {
			t.Errorf("a refusal is not an unconfigured allow:\n%s", stderr)
		}
	})

	t.Run("a profile that exists and does not parse", func(t *testing.T) {
		repo := unconfiguredRepo(t, "badyaml")
		mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: [unclosed\n")
		deny, stderr, _ := hookRun(t, bin, configuredHome(t), repo, "git push")
		if deny == "" || !strings.Contains(deny, "was not accepted") || strings.Contains(stderr, "UNCONFIGURED") {
			t.Fatalf("an unreadable in-repo profile must refuse, not read as unconfigured; deny=%q stderr=%q", deny, stderr)
		}
	})

	t.Run("an external profile that does not parse", func(t *testing.T) {
		repo := unconfiguredRepo(t, "extbad")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "extbad.yaml"), "project_id: [unclosed\n")
		deny, stderr, _ := hookRun(t, bin, home, repo, "git push")
		if deny == "" || !strings.Contains(deny, "was not accepted") || strings.Contains(stderr, "UNCONFIGURED") {
			t.Fatalf("an unreadable external profile must refuse; deny=%q stderr=%q", deny, stderr)
		}
	})

	t.Run("a readable profile governs, and an ungated push is refused for its record", func(t *testing.T) {
		repo := unconfiguredRepo(t, "profiled")
		mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "project_id: profiled\n")
		deny, stderr, _ := hookRun(t, bin, configuredHome(t), repo, "git push")
		if !strings.Contains(deny, "no review record") || strings.Contains(stderr, "UNCONFIGURED") {
			t.Fatalf("a profiled repo is judged on its record; deny=%q stderr=%q", deny, stderr)
		}
	})

	t.Run("a repo with no sign of leadkit stays silent", func(t *testing.T) {
		requireGit(t)
		repo := filepath.Join(t.TempDir(), "plain")
		initRepoWithOrigin(t, repo, "https://github.com/acme/plain.git")
		deny, stderr, code := hookRun(t, bin, configuredHome(t), repo, "git push")
		if deny != "" || stderr != "" || code != 0 {
			t.Fatalf("no sign of leadkit is a silent allow; deny=%q stderr=%q exit=%d", deny, stderr, code)
		}
	})
}

// A command the push parser cannot read is judged on the repo it runs in. In an unconfigured
// repo that is the same report, and a refusal where the profile is unreadable.
func TestCLI_PushGate_UnconfiguredRepoUnreadableCommand(t *testing.T) {
	bin := testBinary(t)
	repo := unconfiguredRepo(t, "unsure")
	deny, stderr, _ := hookRun(t, bin, configuredHome(t), repo, `bash -c "git push"`)
	if deny != "" || !strings.Contains(stderr, unconfiguredMarker) {
		t.Fatalf("want an allow that reports the state; deny=%q stderr=%q", deny, stderr)
	}
	deny, _, _ = hookRun(t, bin, t.TempDir(), repo, `bash -c "git push"`)
	if deny == "" {
		t.Fatal("a HOME holding no leadkit config must still refuse an unreadable command")
	}
}

// classifyRepo is the one place the states are decided; the hook, the pre-push hook and
// `leadkit push` all ask it.
func TestClassifyRepo_FourStates(t *testing.T) {
	requireGit(t)
	homeIsTheAccount(t)
	t.Setenv("LEADKIT_PROFILE", "")
	t.Setenv("HOME", configuredHome(t))

	plain := filepath.Join(t.TempDir(), "plain")
	initRepoWithOrigin(t, plain, "https://github.com/acme/plain.git")
	if g, why := classifyRepo(plain); g != govNone || why != "" {
		t.Errorf("no sign of leadkit: got %v %q", g, why)
	}

	loose := unconfiguredRepo(t, "loose")
	g, why := classifyRepo(loose)
	if g != govUnconfigured || !strings.Contains(why, "no leadkit profile exists") || !strings.Contains(why, "loose.yaml") {
		t.Errorf("a .leadkit/ directory and no profile under a configured HOME: got %v %q", g, why)
	}
	if hookProfileProblem(loose) != "" || hookUnconfigured(loose) == "" {
		t.Error("the two questions must partition the states: unconfigured is not a problem")
	}

	profiled := unconfiguredRepo(t, "profiled")
	mustWrite(t, filepath.Join(profiled, "leadkit.yaml"), "project_id: profiled\n")
	if g, _ := classifyRepo(profiled); g != govProfiled {
		t.Errorf("a readable profile: got %v", g)
	}
	if hookProfileProblem(profiled) != "" || hookUnconfigured(profiled) != "" {
		t.Error("a profiled repo is neither a problem nor unconfigured")
	}

	broken := unconfiguredRepo(t, "broken")
	mustWrite(t, filepath.Join(broken, "leadkit.yaml"), "project_id: [unclosed\n")
	if g, why := classifyRepo(broken); g != govProblem || !strings.Contains(why, "was not accepted") {
		t.Errorf("a profile that was turned away: got %v %q", g, why)
	}
	if hookProfileProblem(broken) == "" || hookUnconfigured(broken) != "" {
		t.Error("a refused profile is a problem, never unconfigured")
	}

	t.Setenv("HOME", t.TempDir())
	if g, why := classifyRepo(loose); g != govProblem || !strings.Contains(why, "holds no leadkit config directory") {
		t.Errorf("the same repo under a HOME with no config directory: got %v %q", g, why)
	}
	if hookUnconfigured(loose) != "" {
		t.Error("a HOME that cannot have found a profile must not read as unconfigured")
	}

	t.Setenv("HOME", "")
	if g, _ := classifyRepo(loose); g == govUnconfigured {
		t.Error("an empty HOME must not read as unconfigured")
	}
}

// `leadkit push` (the governed verb) states the state and proceeds; it does not hide behind the
// session's profile, and it still refuses where the HOME could not have found a profile.
func TestCLIPush_UnconfiguredRepoProceedsAndSaysSo(t *testing.T) {
	bin := testBinary(t)
	repo := unconfiguredRepo(t, "cvepatch")
	run := func(home string) (string, int) {
		cmd := exec.Command(bin, "push", "--repo", repo, "--branch", "fix/cve", "--dry-run")
		cmd.Dir = home
		cmd.Env = sandboxedEnv(home, []string{"REVIEW_GATE_BYPASS="})
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}

	out, code := run(configuredHome(t))
	if code != 0 || !strings.Contains(out, unconfiguredMarker) || !strings.Contains(out, "would push") {
		t.Fatalf("an unconfigured repo must proceed, stating the state (exit %d):\n%s", code, out)
	}
	if strings.Contains(out, "out of scope of the profile in effect") {
		t.Errorf("there is no profile in effect to be out of scope of:\n%s", out)
	}

	out, code = run(t.TempDir())
	if code != 1 || !strings.Contains(out, "cannot be known") {
		t.Fatalf("a HOME holding no leadkit config must still refuse (exit %d):\n%s", code, out)
	}
}

// The git pre-push hook is the enforcement layer; a text hook that allowed while the git hook
// refused would not unblock anything.
func TestCLI_PrePushHook_UnconfiguredRepoIsAllowedAndSaysSo(t *testing.T) {
	push := func(f *prePushFixture, branch string) (string, int) {
		return f.shell("git " + pp + " origin HEAD:refs/heads/" + branch)
	}
	newRepo := func(t *testing.T) *prePushFixture {
		f := newPrePushFixture(t, false)
		gitRun(t, f.repo, "rm", "-q", "leadkit.yaml")
		gitRun(t, f.repo, "commit", "-q", "-m", "no profile")
		ppWrite(t, filepath.Join(f.repo, ".leadkit", "decisions.jsonl"), "{}\n")
		f.initHere()
		return f
	}

	t.Run("under a HOME that holds leadkit configs", func(t *testing.T) {
		f := newRepo(t)
		ppWrite(t, filepath.Join(f.home, ".claude", "leadkit", "config", "someone-else.yaml"), "project_id: x\n")
		out, code := push(f, "cve")
		if code != 0 || !f.remoteHas("refs/heads/cve") {
			t.Fatalf("the push must go through (exit %d):\n%s", code, out)
		}
		if !strings.Contains(out, "UNCONFIGURED") || !strings.Contains(out, unconfiguredMarker) {
			t.Errorf("and the hook must say why it let it through:\n%s", out)
		}
	})

	t.Run("under a HOME that cannot have found a profile", func(t *testing.T) {
		f := newRepo(t)
		if _, err := os.Stat(filepath.Join(f.home, ".claude", "leadkit", "config")); err == nil {
			t.Fatal("fixture error: this HOME already holds a leadkit config directory")
		}
		out, code := push(f, "bad")
		if code == 0 || f.remoteHas("refs/heads/bad") || !strings.Contains(out, "cannot be known") {
			t.Fatalf("must refuse (exit %d):\n%s", code, out)
		}
	})
}

// homeIsTheAccount stubs the system's answer for the account's home to be whatever $HOME is at the
// time of the call, so an in-process test can reach the unconfigured state.
func homeIsTheAccount(t *testing.T) {
	t.Helper()
	orig := accountHomes
	accountHomes = func() []string { return []string{os.Getenv("HOME")} }
	t.Cleanup(func() { accountHomes = orig })
}

// A profile that exists must not read as absent because the handle the lookup uses was changed.
// External configs are found by the ORIGIN's repository name, so `git remote rename origin x`
// makes the lookup miss a profile that is sitting in the config directory. The repo then looks
// like a .leadkit/ directory with no profile, and a gated repo published with no review record.
func TestCLI_PushGate_ARenamedRemoteDoesNotHideAProfile(t *testing.T) {
	bin := testBinary(t)

	t.Run("a config named for the origin", func(t *testing.T) {
		repo := unconfiguredRepoAt(t, "clone-a", "gatedx")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "gatedx.yaml"), "project_id: gatedx\n")
		deny, report, _ := hookRun(t, bin, home, repo, "git push")
		if !strings.Contains(deny, "no review record") {
			t.Fatalf("control: the external profile gates the repo; deny=%q report=%q", deny, report)
		}
		gitRun(t, repo, "remote", "rename", "origin", "elsewhere")
		deny, report, _ = hookRun(t, bin, home, repo, "git push")
		if deny == "" || strings.Contains(report, "UNCONFIGURED") {
			t.Fatalf("a renamed remote hid the profile and the push was allowed; report=%q", report)
		}
		if !strings.Contains(deny, "gatedx.yaml") || !strings.Contains(deny, "cannot be known") {
			t.Errorf("the refusal must name the config that was not found:\n%s", deny)
		}
	})

	t.Run("a config that declares project_origin under another name", func(t *testing.T) {
		repo := unconfiguredRepoAt(t, "clone-b", "gatedy")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "legacy-name.yaml"), "project_id: legacy-name\nproject_origin: acme/gatedy\n")
		for _, rename := range []bool{false, true} {
			if rename {
				gitRun(t, repo, "remote", "rename", "origin", "elsewhere")
			}
			deny, report, _ := hookRun(t, bin, home, repo, "git push")
			if deny == "" || strings.Contains(report, "UNCONFIGURED") || !strings.Contains(deny, "legacy-name.yaml") {
				t.Fatalf("rename=%v: a config that names this repo by project_origin must refuse it; deny=%q report=%q", rename, deny, report)
			}
		}
	})

	t.Run("a config named for the clone directory while the origin has another name", func(t *testing.T) {
		repo := unconfiguredRepoAt(t, "mydir", "other-name")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "mydir.yaml"), "project_id: something\n")
		deny, report, _ := hookRun(t, bin, home, repo, "git push")
		if deny == "" || strings.Contains(report, "UNCONFIGURED") || !strings.Contains(deny, "mydir.yaml") {
			t.Fatalf("a config named for the clone directory is this repo's; deny=%q report=%q", deny, report)
		}
	})

	t.Run("a config in the directory that cannot be read cannot be ruled out", func(t *testing.T) {
		repo := unconfiguredRepo(t, "unruled")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "broken.yaml"), "project_id: [unclosed\n")
		deny, report, _ := hookRun(t, bin, home, repo, "git push")
		if deny == "" || strings.Contains(report, "UNCONFIGURED") || !strings.Contains(deny, "broken.yaml") || !strings.Contains(deny, "may belong") || strings.Contains(deny, " belongs to this repo") {
			t.Fatalf("an unparseable config may be the one; deny=%q report=%q", deny, report)
		}
	})

	t.Run("configs for other repos do not stop the state", func(t *testing.T) {
		repo := unconfiguredRepo(t, "genuinely-new")
		home := configuredHome(t)
		mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "another.yaml"), "project_id: another\nproject_origin: acme/another\n")
		deny, report, _ := hookRun(t, bin, home, repo, "git push")
		if deny != "" || !strings.Contains(report, unconfiguredMarker) {
			t.Fatalf("a repo no config names is unconfigured; deny=%q report=%q", deny, report)
		}
	})
}

// The same hole through the two other enforcement points: `leadkit push` self-gates, and the git
// pre-push check reads the profile itself, so neither is covered by the text hook refusing.
func TestCLI_RenamedRemote_IsRefusedByPushVerbAndPrePush(t *testing.T) {
	bin := testBinary(t)
	repo := unconfiguredRepoAt(t, "clone-z", "gatedz")
	home := configuredHome(t)
	mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "gatedz.yaml"), "project_id: gatedz\n")
	gitRun(t, repo, "remote", "rename", "origin", "elsewhere")

	cmd := exec.Command(bin, "push", "--repo", repo, "--branch", "main", "--dry-run")
	cmd.Dir = home
	cmd.Env = sandboxedEnv(home, []string{"REVIEW_GATE_BYPASS="})
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "would push") || !strings.Contains(string(out), "cannot be known") {
		t.Errorf("leadkit push must refuse a repo whose profile was hidden by a renamed remote (err=%v):\n%s", err, out)
	}

	shaOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	pre := exec.Command(bin, "gate", "check-push")
	pre.Dir = repo
	pre.Env = append(sandboxedEnv(home, []string{"REVIEW_GATE_BYPASS="}), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	pre.Stdin = strings.NewReader("refs/heads/main " + sha + " refs/heads/main 0000000000000000000000000000000000000000\n")
	out, err = pre.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cannot be known") {
		t.Errorf("the pre-push check must refuse it too (err=%v):\n%s", err, out)
	}
}

// A HOME that is not the account's own is a place leadkit's configs are not, so a missing profile
// there is not evidence of anything. Forging it needs no remote rename and leaves no trace.
func TestClassifyRepo_AHomeThatIsNotTheAccountsIsNotUnconfigured(t *testing.T) {
	requireGit(t)
	t.Setenv("LEADKIT_PROFILE", "")
	home := configuredHome(t)
	t.Setenv("HOME", home)
	repo := unconfiguredRepo(t, "forged")

	orig := accountHomes
	t.Cleanup(func() { accountHomes = orig })

	accountHomes = func() []string { return []string{home} }
	if g, _ := classifyRepo(repo); g != govUnconfigured {
		t.Fatalf("control: HOME is the account's: got %v", g)
	}
	accountHomes = func() []string { return []string{t.TempDir()} }
	g, why := classifyRepo(repo)
	if g != govProblem || !strings.Contains(why, "not the home directory of the account") {
		t.Errorf("a HOME the account does not own: got %v %q", g, why)
	}
	accountHomes = func() []string { return []string{home, t.TempDir()} }
	if g, _ := classifyRepo(repo); g != govProblem {
		t.Errorf("one source agreeing is not enough when another disagrees: got %v", g)
	}
	accountHomes = func() []string { return nil }
	if g, _ := classifyRepo(repo); g != govProblem {
		t.Errorf("with no way to ask the system, HOME cannot be vouched for: got %v", g)
	}
}

// The real sources answer, and agree with the HOME the process was started with.
func TestAccountHomes_RealSourcesAgreeWithTheRealHome(t *testing.T) {
	homes := accountHomes()
	if len(homes) == 0 {
		t.Skip("no source for the account home in this environment")
	}
	real, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("HOME", real)
	if !homeIsTheAccountsOwn() {
		t.Skipf("this environment's HOME %q is not the account's (sources said %v); the checks below would pass for the wrong reason", real, homes)
	}
	t.Setenv("HOME", t.TempDir())
	if homeIsTheAccountsOwn() {
		t.Error("a temp directory is not the account's home")
	}
	t.Setenv("HOME", "")
	if homeIsTheAccountsOwn() {
		t.Error("an empty HOME is not the account's home")
	}
}
