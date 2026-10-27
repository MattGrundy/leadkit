package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 228. The review gate decides whether a push is its business from the remote named `origin`.
// A repo with a profile whose only remote is named something else cannot have its scope confirmed, and
// that must not read as "not governed", which is allow in every mode: renaming one remote would turn the
// gate off in push-gate, gate check, leadkit push and the verdict, while doctor reported full enforcement.
func TestCLI_AProfiledRepoWhoseRemoteIsNotCalledOriginIsNotOutOfScope(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	home := t.TempDir()

	mk := func(profile, remoteName string) string {
		repo := filepath.Join(t.TempDir(), "noorigin")
		initRepoWithOrigin(t, repo, "")
		gitRun(t, repo, "remote", "add", remoteName, "https://github.com/acme/noorigin.git")
		if profile != "" {
			if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte(profile), 0o644); err != nil {
				t.Fatal(err)
			}
			gitRun(t, repo, "add", "-A")
			gitRun(t, repo, "commit", "-q", "-m", "profile")
		}
		return repo
	}
	enforce := "guards:\n  default_mode: enforce\n"
	advisory := "project_id: x\n"

	// ENFORCE: the gate cannot confirm scope, so the push is denied and the reason is readable.
	repo := mk(enforce, "github")
	deny := pushGateVerdictCmd(t, bin, home, repo, "git push github main")
	if deny == "" {
		t.Fatal("enforce mode allowed a push of a profiled repo whose origin the gate cannot read")
	}
	// ...and the same repo with the remote named origin is judged (denied for the missing record), so the
	// refusal above is about scope and not about something else in the fixture.
	repo2 := mk(enforce, "origin")
	if d := pushGateVerdictCmd(t, bin, home, repo2, "git push origin main"); !strings.Contains(d, "no review record") {
		t.Fatalf("control: with origin named, the push is denied for the missing record, got %q", d)
	}

	// ADVISORY: cannot-confirm allows, as it does everywhere in advisory mode.
	repo3 := mk(advisory, "github")
	if d := pushGateVerdictCmd(t, bin, home, repo3, "git push github main"); d != "" {
		t.Errorf("advisory mode must allow an unconfirmable scope, got deny: %s", d)
	}

	// CONTROL: a repo with NO profile and no origin is genuinely not governed, in either mode.
	bare := mk("", "github")
	if d := pushGateVerdictCmd(t, bin, home, bare, "git push github main"); d != "" {
		t.Errorf("an unprofiled repo must stay allowed, got: %s", d)
	}

	// leadkit push refuses a scope it cannot confirm, whatever the mode: an explicit push of a named sha.
	out, code := runCLIDir(t, bin, repo3, nil, "push", "--repo", repo3, "--branch", "main", "--remote", "github", "--dry-run")
	if code == 0 || strings.Contains(out, "out of scope of the profile in effect") {
		t.Errorf("leadkit push must not call an unreadable origin out of scope (exit %d):\n%s", code, out)
	}
}
