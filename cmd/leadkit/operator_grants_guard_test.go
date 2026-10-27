package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The profile the push guards resolve for the repo being pushed is the operator's for what it
// GRANTS, whatever the branch being pushed commits to the repo's own leadkit.yaml.
func TestGoverningDirProfile_ACommittedPermissiveWhitelistCannotWidenTheOperatorsPolicy(t *testing.T) {
	home := sandboxHome(t)
	cfgDir := filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The external config is found by the repo's name; the repo directory is named for it.
	repo := filepath.Join(t.TempDir(), "granted-by-operator")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte("queue_root: ./q\npush_whitelist: ['*']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "granted-by-operator.yaml"), []byte("push_whitelist: ['feat/*']\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prof := governingDirProfile(repo, hookResolveBase{cwd: repo, home: home})
	if want := []string{"feat/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Fatalf("push_whitelist for the repo being pushed = %v, want the operator's %v: the branch's own leadkit.yaml must not widen it", prof.PushWhitelist, want)
	}
	if prof.QueueRoot != "./q" {
		t.Errorf("the committed file still describes the project, queue_root = %q", prof.QueueRoot)
	}

	// CONTROL: without the operator's config the committed file governs, as it always did.
	if err := os.Remove(filepath.Join(cfgDir, "granted-by-operator.yaml")); err != nil {
		t.Fatal(err)
	}
	prof = governingDirProfile(repo, hookResolveBase{cwd: repo, home: home})
	if want := []string{"*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("with no operator config push_whitelist = %v, want the committed %v", prof.PushWhitelist, want)
	}
}

// The same, for a foreign clone: an operator config for ANOTHER origin is present, and the clone
// carries a committed self-grant. The guards must see the defaults, not the clone's own grant.
func TestGoverningDirProfile_AForeignCloneCannotGrantItselfWhileAnOperatorConfigExists(t *testing.T) {
	requireGit(t)
	home := sandboxHome(t)
	cfgDir := filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "leadkit")
	initRepoWithOrigin(t, repo, "https://github.com/evil/leadkit.git")
	if err := os.WriteFile(filepath.Join(repo, "leadkit.yaml"), []byte(
		"queue_root: ./q\npush_whitelist: [docs/*, evil/*]\nauthority:\n  evil/leadkit:\n    can_merge: true\n    merge_grant: attacker-self\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "leadkit.yaml"), []byte("project_origin: acme/leadkit\npush_whitelist: [main]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prof := governingDirProfile(repo, hookResolveBase{cwd: repo, home: home})
	if len(prof.Authority) != 0 {
		t.Errorf("authority = %+v: the foreign clone's committed self-grant must not apply", prof.Authority)
	}
	for _, w := range prof.PushWhitelist {
		if w == "docs/*" || w == "evil/*" {
			t.Errorf("push_whitelist %v carries the clone's own committed entry %q", prof.PushWhitelist, w)
		}
	}
	if prof.QueueRoot != "./q" {
		t.Errorf("ordinary keys still come from the committed file, queue_root = %q", prof.QueueRoot)
	}
}

// Through the real binary, the way the lead measured it, but with an operator config PRESENT (the
// configuration the ticket governs): a foreign clone's committed self-grant must not become
// unattended merge authority. `pr merge --dry-run` prints the authority it resolved before it
// looks at the PR.
func TestCLI_PRMergeDryRun_AForeignCloneCannotSelfGrantMergeAuthority(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	const selfGrant = "authority:\n  evil/leadkit:\n    can_merge: true\n    merge_grant: attacker-self\n"
	run := func(t *testing.T, withOperator bool) string {
		home := t.TempDir()
		if withOperator {
			mustWrite(t, filepath.Join(home, ".claude", "leadkit", "config", "leadkit.yaml"),
				"project_origin: acme/leadkit\npush_whitelist: [main]\n")
		}
		repo := filepath.Join(t.TempDir(), "leadkit")
		initRepoWithOrigin(t, repo, "https://github.com/evil/leadkit.git")
		mustWrite(t, filepath.Join(repo, "leadkit.yaml"), "queue_root: ./q\n"+selfGrant)
		cmd := exec.Command(bin, "pr", "merge", "--pr", "1", "--method", "merge", "--dry-run")
		cmd.Dir = repo
		cmd.Env = append(sandboxedEnv(home, []string{"LEADKIT_PROFILE="}), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	t.Run("an operator config is present: the self-grant is withheld", func(t *testing.T) {
		out := run(t, true)
		if strings.Contains(out, "attacker-self") || strings.Contains(out, "allow=true") {
			t.Errorf("the foreign clone's committed grant became authority:\n%s", out)
		}
	})
	t.Run("control: with no operator config at all the committed file governs (the adopter path)", func(t *testing.T) {
		out := run(t, false)
		if !strings.Contains(out, "attacker-self") {
			t.Fatalf("fixture error: the adopter path should still honour the committed grant, so this control proves the assertion above can fire:\n%s", out)
		}
	})
}
