package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runWhoamiIn execs `leadkit whoami <args>` with cmd.Dir = workdir (so the binary's own
// cwd IS workdir — how a real session runs), under runCLI's mandatory sandbox. workdir is
// deliberately a plain temp dir (not a git repo): gitToplevel then reports no toplevel, so
// identity roots at the cwd itself and reads workdir/leadkit.yaml directly — no real git
// binary dependency, mirroring the in-package injection tests.
func runWhoamiIn(t *testing.T, workdir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	bin := testBinary(t)
	home := t.TempDir()
	cmd := exec.Command(bin, append([]string{"whoami"}, args...)...)
	cmd.Dir = workdir
	cmd.Env = sandboxedEnv(home, extraEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec whoami: %v\n%s", err, out)
	return "", -1
}

// TestCLI_Whoami_Mismatch: a session whose OWN leadkit.yaml says project_id=alpha, but
// with $LEADKIT_PROFILE pointing at a DIFFERENT project (beta), prints the MISMATCH
// warning naming BOTH slugs; its inbox/addressing uses the identity (alpha).
func TestCLI_Whoami_Mismatch(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte("project_id: alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT project's profile in $LEADKIT_PROFILE — the POLICY path honors it.
	betaProfile := filepath.Join(t.TempDir(), "leadkit.yaml")
	if err := os.WriteFile(betaProfile, []byte("project_id: beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runWhoamiIn(t, repo, []string{"LEADKIT_PROFILE=" + betaProfile})
	if code != 0 {
		t.Fatalf("whoami should exit 0, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "MISMATCH") {
		t.Fatalf("mismatch session must print MISMATCH, got:\n%s", out)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("MISMATCH must name BOTH identity=alpha and policy=beta, got:\n%s", out)
	}
}

// TestCLI_Whoami_JSONStable: --json emits the resolved identity with the expected fields
// and a mismatch flag set when identity ≠ policy.
func TestCLI_Whoami_JSONStable(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte("project_id: alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	betaProfile := filepath.Join(t.TempDir(), "leadkit.yaml")
	if err := os.WriteFile(betaProfile, []byte("project_id: beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runWhoamiIn(t, repo, []string{"LEADKIT_PROFILE=" + betaProfile, "CLAUDE_CODE_SESSION_ID=uuid-xyz"}, "--json")
	if code != 0 {
		t.Fatalf("whoami --json should exit 0, got %d:\n%s", code, out)
	}
	var info whoamiInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &info); err != nil {
		t.Fatalf("whoami --json must emit valid JSON: %v\n%s", err, out)
	}
	if info.IdentitySlug != "alpha" {
		t.Errorf("identity_slug = %q, want alpha", info.IdentitySlug)
	}
	if info.IdentitySource != "own leadkit.yaml project_id" {
		t.Errorf("identity_source = %q", info.IdentitySource)
	}
	if info.PolicySlug != "beta" {
		t.Errorf("policy_slug = %q, want beta (the ambient $LEADKIT_PROFILE)", info.PolicySlug)
	}
	if !info.Mismatch {
		t.Errorf("mismatch should be true when identity != policy")
	}
	if info.SessionID != "uuid-xyz" || info.SessionIDSource != "CLAUDE_CODE_SESSION_ID" {
		t.Errorf("session id = %q (source %q), want uuid-xyz/CLAUDE_CODE_SESSION_ID", info.SessionID, info.SessionIDSource)
	}
}

// TestCLI_Whoami_OwnConfigNoMismatch: when the ambient profile IS the session's own
// config (no stray env), identity and policy agree — no MISMATCH.
func TestCLI_Whoami_OwnConfigNoMismatch(t *testing.T) {
	repo := t.TempDir()
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("project_id: alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runWhoamiIn(t, repo, []string{"LEADKIT_PROFILE=" + profile}, "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var info whoamiInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &info); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if info.Mismatch {
		t.Errorf("own-config session must not report a mismatch: %+v", info)
	}
	if info.IdentitySlug != "alpha" || info.PolicySlug != "alpha" {
		t.Errorf("identity and policy should both be alpha, got %+v", info)
	}
}
