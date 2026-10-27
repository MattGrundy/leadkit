package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/install"
)

// TestSessionIdentitySlug_ExternalConfigProjectID: a repo with NO in-repo leadkit.yaml
// but an external ~/.claude/leadkit/config/<slug>.yaml carrying a project_id resolves its
// identity from that external project_id — consistent with the external POLICY path — and
// still ignores a stray $LEADKIT_PROFILE (keyed purely by the repo's cwd-honest slug).
func TestSessionIdentitySlug_ExternalConfigProjectID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))

	repo := mkdir(t, filepath.Join(t.TempDir(), "acmecorp-ext")) // basename = the slug; no own config
	if err := os.WriteFile(filepath.Join(cfgDir, "acmecorp-ext.yaml"), []byte("project_id: acmecorp-external-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withGitToplevel(t, map[string]string{repo: repo})
	// A stray env profile for a DIFFERENT project — identity must ignore it entirely.
	t.Setenv("LEADKIT_PROFILE", filepath.Join(t.TempDir(), config.DefaultProfileName))

	slug, source := sessionIdentitySlug(repo)
	want := install.ResolveProjectSlug("acmecorp-external-id", "")
	if slug != want {
		t.Fatalf("external project_id identity slug = %q, want %q", slug, want)
	}
	if source != "external <slug>.yaml project_id" {
		t.Errorf("source = %q, want external-project_id", source)
	}
}

// TestSessionIdentitySlug_InRepoWinsOverExternal: when BOTH an in-repo leadkit.yaml and
// an external <slug>.yaml exist, identity uses the IN-REPO project_id (back-compat wins),
// never the external one.
func TestSessionIdentitySlug_InRepoWinsOverExternal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))

	repo := mkdir(t, filepath.Join(t.TempDir(), "acmecorp-ext"))
	if err := os.WriteFile(filepath.Join(repo, config.DefaultProfileName), []byte("project_id: in-repo-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "acmecorp-ext.yaml"), []byte("project_id: external-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withGitToplevel(t, map[string]string{repo: repo})

	slug, source := sessionIdentitySlug(repo)
	if slug != install.ResolveProjectSlug("in-repo-id", "") {
		t.Fatalf("in-repo project_id must win for identity, got slug %q (source %q)", slug, source)
	}
	if source != "own leadkit.yaml project_id" {
		t.Errorf("source = %q, want own-project_id (in-repo wins)", source)
	}
}

// TestCLI_Whoami_ExternalConfigSource: `leadkit whoami --json`, run in a repo governed
// ONLY by an external config, names the resolved source as "external" with the config's
// path, and resolves both identity and policy from it (no mismatch).
func TestCLI_Whoami_ExternalConfigSource(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	repo := mkdir(t, filepath.Join(t.TempDir(), "acmecorp-ext")) // basename = slug; no in-repo config
	if err := os.WriteFile(filepath.Join(cfgDir, "acmecorp-ext.yaml"), []byte("project_id: acmecorp-ext\nqueue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "whoami", "--json")
	cmd.Dir = repo
	// LEADKIT_PROFILE explicitly EMPTY so the external resolution step runs (an absent-file
	// env path would otherwise be the resolved project path). HOME carries the external config.
	cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE="})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out)
	}

	var info whoamiInfo
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &info); uerr != nil {
		t.Fatalf("bad json: %v\n%s", uerr, out)
	}
	if info.ConfigSource != "external" {
		t.Errorf("config_source = %q, want external\n%s", info.ConfigSource, out)
	}
	if !strings.Contains(info.ConfigLocation, filepath.Join("config", "acmecorp-ext.yaml")) {
		t.Errorf("config_location = %q, want the external config path", info.ConfigLocation)
	}
	if info.IdentitySlug != "acmecorp-ext" {
		t.Errorf("identity_slug = %q, want acmecorp-ext (from the external project_id)", info.IdentitySlug)
	}
	if info.IdentitySource != "external <slug>.yaml project_id" {
		t.Errorf("identity_source = %q, want external-project_id", info.IdentitySource)
	}
	if info.Mismatch {
		t.Errorf("external-governed session should not report a mismatch: %+v", info)
	}
}
