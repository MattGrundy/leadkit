package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func prePushRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir) // macOS: /var -> /private/var, so paths compare
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	return dir
}

// isolate keeps the developer's own ~/.gitconfig (which may set core.hooksPath) out of a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func TestPrePush_FreshCloneIsMissingThenInstalled(t *testing.T) {
	isolate(t)
	repo := prePushRepo(t)
	st := InspectPrePush(repo, "/opt/leadkit")
	if st.State != PrePushMissing {
		t.Fatalf("fresh clone: state = %s (%s), want missing", st.State, st.Detail)
	}
	if st.HookPath != filepath.Join(repo, ".git", "hooks", "pre-push") {
		t.Errorf("HookPath = %s", st.HookPath)
	}
	wrote, err := ApplyPrePush(st)
	if err != nil || !wrote {
		t.Fatalf("ApplyPrePush = %v, %v", wrote, err)
	}
	fi, err := os.Stat(st.HookPath)
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("the hook must be executable (git skips one that is not): %v %v", fi, err)
	}
	if got := InspectPrePush(repo, "/opt/leadkit"); got.State != PrePushInstalled || got.Binary != "/opt/leadkit" {
		t.Errorf("after apply: %s (%s) binary=%q, want installed", got.State, got.Detail, got.Binary)
	}
	if wrote, _ := ApplyPrePush(InspectPrePush(repo, "/opt/leadkit")); wrote {
		t.Error("applying again must be a no-op")
	}
}

func TestPrePush_ForeignHookIsNeverOverwritten(t *testing.T) {
	isolate(t)
	repo := prePushRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho theirs\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := InspectPrePush(repo, "/opt/leadkit")
	if st.State != PrePushForeign {
		t.Fatalf("state = %s, want foreign", st.State)
	}
	if wrote, err := ApplyPrePush(st); wrote || err != nil {
		t.Fatalf("ApplyPrePush on a foreign hook = %v, %v; it must not write", wrote, err)
	}
	if b, _ := os.ReadFile(hook); !strings.Contains(string(b), "theirs") {
		t.Errorf("the foreign hook was changed:\n%s", b)
	}
}

func TestPrePush_StaleAndNotExecutableAreRepaired(t *testing.T) {
	isolate(t)
	repo := prePushRepo(t)
	st := InspectPrePush(repo, "/old/leadkit")
	if _, err := ApplyPrePush(st); err != nil {
		t.Fatal(err)
	}
	stale := InspectPrePush(repo, "/new/leadkit")
	if stale.State != PrePushStale || stale.Binary != "/old/leadkit" {
		t.Fatalf("state = %s binary=%q, want stale pointing at the old binary", stale.State, stale.Binary)
	}
	if wrote, err := ApplyPrePush(stale); !wrote || err != nil {
		t.Fatalf("repair = %v, %v", wrote, err)
	}
	if got := InspectPrePush(repo, "/new/leadkit"); got.State != PrePushInstalled || got.Binary != "/new/leadkit" {
		t.Fatalf("after repair: %s binary=%q", got.State, got.Binary)
	}
	if err := os.Chmod(st.HookPath, 0o644); err != nil {
		t.Fatal(err)
	}
	ne := InspectPrePush(repo, "/new/leadkit")
	if ne.State != PrePushNotExecutable {
		t.Fatalf("state = %s, want not-executable", ne.State)
	}
	if wrote, _ := ApplyPrePush(ne); !wrote {
		t.Fatal("a non-executable hook of ours must be repaired")
	}
	if got := InspectPrePush(repo, "/new/leadkit"); got.State != PrePushInstalled {
		t.Errorf("after repair: %s", got.State)
	}
}

func TestPrePush_CoreHooksPathIsReportedNotWrittenThrough(t *testing.T) {
	isolate(t)
	repo := prePushRepo(t)
	redirect := t.TempDir()
	cmd := exec.Command("git", "config", "core.hooksPath", redirect)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	st := InspectPrePush(repo, "/opt/leadkit")
	if st.State != PrePushRedirected {
		t.Fatalf("state = %s (%s), want redirected", st.State, st.Detail)
	}
	for _, want := range []string{"core.hooksPath", redirect, "config"} {
		if !strings.Contains(st.Detail, want) {
			t.Errorf("the detail does not name %q: %s", want, st.Detail)
		}
	}
	if wrote, err := ApplyPrePush(st); wrote || err != nil {
		t.Fatalf("ApplyPrePush through a redirect = %v, %v; it must not write", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(redirect, "pre-push")); err == nil {
		t.Error("leadkit wrote into a redirected hooks directory")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "pre-push")); err == nil {
		t.Error("leadkit wrote a hook git will not read")
	}
	// A hook of ours placed in the redirected directory by hand is a hook git runs.
	st2 := InspectPrePush(repo, "/opt/leadkit")
	if err := os.WriteFile(filepath.Join(redirect, "pre-push"), st2.Want, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := InspectPrePush(repo, "/opt/leadkit"); got.State != PrePushInstalled {
		t.Errorf("our hook in the redirected directory: %s (%s), want installed", got.State, got.Detail)
	}
}

func TestPrePush_OutsideAGitRepo(t *testing.T) {
	isolate(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if st := InspectPrePush(t.TempDir(), "/opt/leadkit"); st.State != PrePushNoRepo {
		t.Errorf("state = %s, want no-repo", st.State)
	}
}

func TestPrePushScript_QuotesThePathAndRefusesWithoutTheBinary(t *testing.T) {
	const awkward = `/tmp/it's here/leadkit`
	script := PrePushScript(awkward)
	if got, ok := hookBinary(script); !ok || got != awkward {
		t.Fatalf("hookBinary = %q, %v; want the path back through the quoting", got, ok)
	}
	// With the binary absent the hook REFUSES, and says why, rather than opening.
	path := filepath.Join(t.TempDir(), "pre-push")
	if err := os.WriteFile(path, PrePushScript("/nonexistent/leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path, "origin", "url")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a hook whose binary is missing must refuse; output: %s", out)
	}
	if !strings.Contains(string(out), "cannot be gated") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}
}
