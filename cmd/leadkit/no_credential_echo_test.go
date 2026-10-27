package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A remote can carry a credential (`https://user:TOKEN@host/...`). The diagnostic commands people
// run and paste (`config show`, `doctor`) and the commands that take a remote (`registry init`,
// `team fetch-reference`) must never print it, on the success path or the failure path. The check
// scans the ENTIRE output of each for the token, not the line a redaction was expected on, so a
// second format string on a line already fixed cannot hide.
func TestCLI_DiagnosticsAndRemoteTakingVerbsNeverPrintACredentialFromARemote(t *testing.T) {
	bin := testBinary(t)
	const token = "SECRETTOKEN4567"
	remote := "https://deploy:" + token + "@github.com/org/leaky.git"
	// The profile names the repo as an authority with a push_whitelist, so `config show` and `doctor`
	// both reach their origin-bearing lines; a mid-word key and a second key make the collision check fire too.
	repo := cliProject(t, t.TempDir(), "leaky", remote,
		"queue_root: ./queue\nstore_root: ./tasks\nauthority:\n  org/leaky:\n    raw_pr_verbs: [create]\n    push_whitelist: ['feat/*']\n  leaky:\n    raw_pr_verbs: [create]\n  eaky:\n    raw_pr_verbs: [create]\n")
	// The same remote under a profile with no push_whitelist override: `config show` takes its
	// other branch (the default whitelist), which prints the origin in a second format string.
	plain := cliProject(t, t.TempDir(), "leaky", remote, "queue_root: ./queue\nstore_root: ./tasks\n")
	// Exactly one authority key, at a boundary: doctor's all-clear line.
	single := cliProject(t, t.TempDir(), "leaky", remote, "queue_root: ./queue\nstore_root: ./tasks\nauthority:\n  org/leaky:\n    raw_pr_verbs: [create]\n")
	home := t.TempDir()
	// Unreachable on purpose: the clone fails at once, and git's own error quotes the URL.
	unreachable := "https://deploy:" + token + "@127.0.0.1:1/org/x.git"

	var shown bool
	check := func(dir string, args ...string) {
		out, _ := atCwd(t, bin, home, dir, args...)
		if strings.Contains(out, token) || strings.Contains(out, "deploy:") {
			t.Errorf("`leadkit %s` printed the remote's credential:\n%s", strings.Join(args, " "), out)
		}
		shown = shown || strings.Contains(out, "github.com/org/leaky") || strings.Contains(out, "127.0.0.1:1")
	}
	for _, dir := range []string{repo.dir, plain.dir, single.dir} {
		check(dir, "config", "show")
		check(dir, "config", "show", "--show-origin")
		check(dir, "doctor")
	}
	check(repo.dir, "registry", "init", "--remote", unreachable, "--registry-repo", filepath.Join(t.TempDir(), "reg"))
	check(repo.dir, "team", "fetch-reference", "--repo", unreachable)
	// Not vacuous: the remote itself (redacted) is shown by at least one of them.
	if !shown {
		t.Error("no command showed the remote at all; this test would pass for any behaviour")
	}
}

// The two success lines that no offline run reaches through the binary.
func TestFetchedSummary_NeverPrintsACredentialFromTheRepoURL(t *testing.T) {
	got := fetchedSummary(3, "https://deploy:SECRETTOKEN4567@git.example/org/refs.git", "main", "abc123", "/team/reference/refs")
	if strings.Contains(got, "SECRETTOKEN4567") || strings.Contains(got, "deploy") || !strings.Contains(got, "git.example/org/refs.git@main") {
		t.Errorf("fetchedSummary = %q", got)
	}
}

// `registry init` on a git repo with no origin attaches the remote without a network round trip
// that succeeds, and a second run reports the remote it finds: both lines name the remote.
func TestCLI_RegistryInitNeverPrintsACredentialFromTheRemote(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	cwd := t.TempDir()
	reg := filepath.Join(t.TempDir(), "reg")
	gitIn(t, t.TempDir(), "init", "-q", reg)
	remote := "https://deploy:SECRETTOKEN4567@127.0.0.1:1/org/reg.git"
	for i := 0; i < 2; i++ { // attached, then already-initialized
		out, _ := atCwd(t, bin, home, cwd, "registry", "init", "--remote", remote, "--registry-repo", reg)
		if strings.Contains(out, "SECRETTOKEN4567") || strings.Contains(out, "deploy:") {
			t.Errorf("run %d printed the remote's credential:\n%s", i+1, out)
		}
		if !strings.Contains(out, "127.0.0.1:1/org/reg.git") {
			t.Errorf("run %d should still name the remote (redacted), got:\n%s", i+1, out)
		}
	}
}

// The attach arm of `registry init` fails when git cannot lock the repo's config, and then both
// the error git returns and git's own message carry the remote. The whole output is scanned.
func TestCLI_RegistryInitAttachFailureNeverPrintsACredentialFromTheRemote(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	reg := filepath.Join(t.TempDir(), "reg")
	gitIn(t, t.TempDir(), "init", "-q", reg)
	if err := os.WriteFile(filepath.Join(reg, ".git", "config.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := atCwd(t, bin, home, t.TempDir(), "registry", "init",
		"--remote", "https://deploy:SECRETTOKEN4567@127.0.0.1:1/org/reg.git", "--registry-repo", reg)
	if code == 0 {
		t.Fatalf("control: the attach should fail with the config locked, got success:\n%s", out)
	}
	if strings.Contains(out, "SECRETTOKEN4567") || strings.Contains(out, "deploy:") {
		t.Errorf("the attach failure printed the remote's credential:\n%s", out)
	}
	if !strings.Contains(out, "attach origin") || !strings.Contains(out, "127.0.0.1:1/org/reg.git") {
		t.Errorf("the failure should still say what failed and name the remote (redacted), got:\n%s", out)
	}
}
