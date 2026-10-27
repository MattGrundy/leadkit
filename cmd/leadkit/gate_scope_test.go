package main

// gate-scope-config: `gate check` and `gate check-push` must agree on scope for
// the SAME repo+profile — the "no missed call site" regression the spec calls
// out. Both commands are exercised against the ACTUAL binary, a real (offline,
// commit-less) throwaway git repo, and a custom enforced_remotes profile — the
// exact end-to-end path that would have silently drifted had only one of the two
// CLI call sites been wired to newGate().

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// TestNewGate_WiresProfileScope is the whitebox unit proof behind the CLI-level
// test below: newGate (the ONE helper every scope-sensitive gate call site in this
// file goes through) actually carries prof.EnforcedRemotes AND prof.ProjectRoot()
// onto the constructed Gate — for both a project override and the untouched
// config.Defaults() case (which now ships EMPTY enforced_remotes: an unset list
// gates the project's own origin, not a baked-in named list).
func TestNewGate_WiresProfileScope(t *testing.T) {
	// A profile LOADED from a real dir, so its ProjectRoot() is non-empty: a hand-built
	// &config.Profile{} has root "" and would make the ProjectRoot assertion below equal
	// "" == "" whether or not newGate wired it.
	root := writeProfileDir(t, "queue_root: queue\nstore_root: tasks\nenforced_remotes: [another-project]\n")
	custom, _, err := config.LoadFrom(root)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if custom.ProjectRoot() == "" {
		t.Fatal("fixture error: the loaded profile must have a non-empty ProjectRoot() for this test to mean anything")
	}
	g := newGate(custom)
	if len(g.EnforcedRemotes) != 1 || g.EnforcedRemotes[0] != "another-project" {
		t.Errorf("newGate(custom).EnforcedRemotes = %v, want [another-project]", g.EnforcedRemotes)
	}
	if g.ProjectRoot != custom.ProjectRoot() {
		t.Errorf("newGate(custom).ProjectRoot = %q, want %q (mirrors prof.ProjectRoot())", g.ProjectRoot, custom.ProjectRoot())
	}

	def := config.Defaults()
	g2 := newGate(&def)
	if len(g2.EnforcedRemotes) != 0 {
		t.Fatalf("newGate(Defaults()).EnforcedRemotes = %v, want EMPTY (own-origin default)", g2.EnforcedRemotes)
	}
	if g2.ProjectRoot != def.ProjectRoot() {
		t.Errorf("newGate(Defaults()).ProjectRoot = %q, want %q", g2.ProjectRoot, def.ProjectRoot())
	}
}

func gitRepoNoCommits(t *testing.T, originURL string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", originURL)
	return repo
}

const scopeTestFakeSHA = "1111111111111111111111111111111111111111"

// runGateCheckPushCLI runs `leadkit gate check-push` with cmd.Dir = repo (the
// hook's own convention — CheckPush always checks the process cwd, never an
// explicit --repo) and a synthetic pre-push protocol line on stdin.
func runGateCheckPushCLI(t *testing.T, bin, repo string, extraEnv []string) (output string, exitCode int) {
	t.Helper()
	stdin := "refs/heads/main " + scopeTestFakeSHA + " refs/heads/main 0000000000000000000000000000000000000000\n"
	cmd := exec.Command(bin, "gate", "check-push")
	cmd.Dir = repo
	cmd.Env = sandboxedEnv(t.TempDir(), extraEnv)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %s gate check-push: %v\n%s", bin, err, out)
	return "", -1
}

// TestCLI_GateCheckAndCheckPush_AgreeOnScope_CustomEnforcedRemotes is the "no
// missed call site" regression: a repo NOT in the built-in default list, brought
// into scope by the project's own enforced_remotes, must be BLOCKED (no record)
// by BOTH `gate check` and `gate check-push` — a missed wiring at either site
// would show up here as the two commands disagreeing.
func TestCLI_GateCheckAndCheckPush_AgreeOnScope_CustomEnforcedRemotes(t *testing.T) {
	bin := testBinary(t)
	repo := gitRepoNoCommits(t, "git@example.com:org/custom-remote-name.git")

	profileRoot := t.TempDir()
	profilePath := filepath.Join(profileRoot, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nenforced_remotes: [custom-remote-name]\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	checkOut, checkCode := runCLI(t, bin, env, "gate", "check", "--repo", repo, "--sha", scopeTestFakeSHA)
	if checkCode != 1 {
		t.Errorf("`gate check` on an in-scope (via profile) repo with no record: want exit 1 (BLOCK), got %d. output:\n%s", checkCode, checkOut)
	}
	if !strings.Contains(checkOut, "BLOCK") {
		t.Errorf("`gate check` output = %q, want BLOCK", checkOut)
	}

	pushOut, pushCode := runGateCheckPushCLI(t, bin, repo, env)
	if pushCode != 1 {
		t.Errorf("`gate check-push` on the SAME in-scope repo with no record: want exit 1 (deny), got %d. output:\n%s", pushCode, pushOut)
	}
}

// TestCLI_GateCheckAndCheckPush_AgreeOnScope_DefaultProfile is the same
// consistency check for the UNPROFILED default case: a repo with NO leadkit.yaml at
// all (config.Defaults() — empty enforced_remotes AND no project root) has no
// derivable own origin, so it stays out-of-scope-allow for both commands.
func TestCLI_GateCheckAndCheckPush_AgreeOnScope_DefaultProfile(t *testing.T) {
	bin := testBinary(t)
	repo := gitRepoNoCommits(t, "git@example.com:org/custom-remote-name.git")

	checkOut, checkCode := runCLI(t, bin, nil, "gate", "check", "--repo", repo, "--sha", scopeTestFakeSHA)
	if checkCode != 0 {
		t.Errorf("`gate check` on an out-of-scope repo (no profile): want exit 0 (allow), got %d. output:\n%s", checkCode, checkOut)
	}

	pushOut, pushCode := runGateCheckPushCLI(t, bin, repo, nil)
	if pushCode != 0 {
		t.Errorf("`gate check-push` on the SAME out-of-scope repo: want exit 0 (allow), got %d. output:\n%s", pushCode, pushOut)
	}
}

// TestCLI_GateCheckPush_DefaultGatesOwnOrigin is the zero-config self-gating proof at
// the CLI: a repo whose OWN leadkit.yaml declares NO enforced_remotes still gates its
// OWN pushes — `gate check-push` BLOCKS (exit 1) when there's no review record, purely
// because the target repo IS the profile's own project (own-origin default). No named
// remote list anywhere.
func TestCLI_GateCheckPush_DefaultGatesOwnOrigin(t *testing.T) {
	bin := testBinary(t)
	repo := gitRepoNoCommits(t, "git@example.com:org/self-gated.git")

	// The profile lives IN the repo (so its project root == the repo being pushed),
	// and declares no enforced_remotes — the pure own-origin default.
	profilePath := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pushOut, pushCode := runGateCheckPushCLI(t, bin, repo, []string{"LEADKIT_PROFILE=" + profilePath})
	if pushCode != 1 {
		t.Errorf("`gate check-push` on the project's OWN repo (no enforced_remotes, no record): want exit 1 (BLOCK own origin), got %d. output:\n%s", pushCode, pushOut)
	}
}

// TestCLI_GateCheck_ListNamingOtherReposStillGatesOwnOrigin is the tickets 11/87
// reproduction at the CLI: a profile whose enforced_remotes names only OTHER projects
// must still gate the repo that owns the profile. An all-zeros sha with no record is
// BLOCKED for the project's own repo — and the same profile still leaves a DIFFERENT,
// unlisted repo out of scope, so the block is the own-origin rule and not a gate that
// refuses everything. BLOCK can only come from the record check (every earlier check in
// Check returns allow), so the "no review record" text pins the reason.
func TestCLI_GateCheck_ListNamingOtherReposStillGatesOwnOrigin(t *testing.T) {
	bin := testBinary(t)
	own := gitRepoNoCommits(t, "git@example.com:org/self-gated.git")
	other := gitRepoNoCommits(t, "git@example.com:org/someone-elses.git")

	profilePath := filepath.Join(own, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nenforced_remotes: [named-upstream-a, named-upstream-b]\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}
	const zeros = "0000000000000000000000000000000000000000"

	out, code := runCLI(t, bin, env, "gate", "check", "--repo", own, "--sha", zeros)
	if code != 1 || !strings.Contains(out, "BLOCK") || !strings.Contains(out, "no review record") {
		t.Errorf("own repo, list names only others, no record: want exit 1 BLOCK 'no review record', got %d:\n%s", code, out)
	}

	pushOut, pushCode := runGateCheckPushCLI(t, bin, own, env)
	if pushCode != 1 {
		t.Errorf("`gate check-push` on the project's own repo with a list naming only others: want exit 1, got %d:\n%s", pushCode, pushOut)
	}

	// Control: an unprofiled repo that is not in the list stays out of scope (scope follows
	// the TARGET's own governing profile, and this target has none). This pins that the
	// fix did not turn the gate into "refuse everything"; it cannot tell own-origin
	// equality from mere presence of an origin (the internal/gate tests do).
	out, code = runCLI(t, bin, env, "gate", "check", "--repo", other, "--sha", zeros)
	if code != 0 || !strings.Contains(out, "out of scope") {
		t.Errorf("an unlisted, different repo must stay out of scope: want exit 0 'out of scope', got %d:\n%s", code, out)
	}
}
