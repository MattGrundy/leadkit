package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 234, through the real binary. A committed leadkit.yaml that sets records_dir, beside a
// committed BYPASS file in that directory, turned the review gate off for the checkout even though the
// operator's external config governs the repo; and a committed advisory mode replaced the operator's
// enforce. records_dir and guards are the operator's to set, like the other grant keys.
func TestCLI_ACommittedRecordsDirCannotBypassTheOperatorsGate(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	home := t.TempDir()
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(committed string, external bool) string {
		repo := filepath.Join(t.TempDir(), "gated") // basename and origin repo name = the external config's slug
		initRepoWithOrigin(t, repo, "https://github.com/acme/gated.git")
		write(filepath.Join(repo, "leadkit.yaml"), committed)
		write(filepath.Join(repo, "recs", "BYPASS"), "committed self-bypass\n")
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", "profile")
		if external {
			write(filepath.Join(cfgDir, "gated.yaml"), "project_id: gated\nguards:\n  default_mode: enforce\n")
		}
		return repo
	}
	const committed = "project_id: gated\nrecords_dir: ./recs\nguards:\n  default_mode: advisory\n"
	pushCmd := "git push origin main"

	// With the operator's config: the committed records_dir (and its BYPASS) is ignored, so the push is
	// judged by the operator's records dir and refused for the missing record.
	repo := mk(committed, true)
	if d := pushGateVerdictCmd(t, bin, home, repo, pushCmd, "LEADKIT_PROFILE="); !strings.Contains(d, "no review record") {
		t.Fatalf("the committed records_dir must not stand against the operator's config; want a refusal for the missing record, got %q", d)
	}
	out, code := runCLIDir(t, bin, repo, []string{"HOME=" + home, "LEADKIT_PROFILE="}, "config", "show", "--show-origin")
	if code != 0 {
		t.Fatalf("config show: exit %d\n%s", code, out)
	}
	for _, key := range []string{"records_dir", "guards"} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, key+":") || strings.Contains(line, key) && strings.Contains(line, "layer=") {
				found = found || strings.Contains(line, "layer=external")
			}
		}
		if !found {
			t.Errorf("config show --show-origin must attribute %s to the external config:\n%s", key, out)
		}
	}

	// CONTROL: the adopter path. With NO operator config the committed file governs itself, so its
	// BYPASS file in its records_dir is honoured exactly as before: the refusal above comes from the
	// overlay and not from something else in the fixture.
	if err := os.Remove(filepath.Join(cfgDir, "gated.yaml")); err != nil {
		t.Fatal(err)
	}
	adopter := mk(committed, false)
	if d := pushGateVerdictCmd(t, bin, home, adopter, pushCmd, "LEADKIT_PROFILE="); d != "" {
		t.Errorf("with no operator config the committed records_dir and its BYPASS apply, want allow, got %q", d)
	}
}
