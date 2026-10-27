package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A committed leadkit.yaml is repo content: anyone who can land a commit can land one. It may
// DESCRIBE a project, but what it GRANTS (push_whitelist, authority, enforced_remotes,
// owners_file) comes from the operator's external config when there is one, so a branch cannot
// authorise its own push.

const committedPermissive = `queue_root: ./committed-queue
store_root: ./committed-tasks
push_whitelist: ['*']
enforced_remotes: [nothing-at-all]
owners_file: .leadkit/committed-owners.yaml
authority:
  acme/web-client:
    can_push: true
    can_merge: true
    merge_grant: SELF-GRANTED
`

const operatorConfig = `queue_root: ./ext-queue
push_whitelist: ['feat/*']
enforced_remotes: [acme/web-client]
owners_file: ext-owners.yaml
authority:
  acme/web-client:
    can_push: false
`

// operatorFixture: a repo with a COMMITTED permissive config and the operator's external config.
func operatorFixture(t *testing.T, external string) (repo, cfgDir string) {
	t.Helper()
	_, cfgDir = externalTestHome(t)
	repo = mkRepo(t, "web-client")
	if err := os.WriteFile(filepath.Join(repo, DefaultProfileName), []byte(committedPermissive), 0o644); err != nil {
		t.Fatal(err)
	}
	if external != "" {
		writeExternalConfig(t, cfgDir, "web-client", external)
	}
	return repo, cfgDir
}

func TestLoadFrom_TheOperatorsGrantKeysBeatACommittedConfig(t *testing.T) {
	repo, cfgDir := operatorFixture(t, operatorConfig)
	prof, found, err := LoadFrom(repo)
	if err != nil || !found {
		t.Fatalf("LoadFrom: found=%v err=%v", found, err)
	}

	// The grant keys are the operator's.
	if want := []string{"feat/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the operator's %v (the committed ['*'] must not widen it)", prof.PushWhitelist, want)
	}
	if want := []string{"acme/web-client"}; !reflect.DeepEqual(prof.EnforcedRemotes, want) {
		t.Errorf("enforced_remotes = %v, want the operator's %v", prof.EnforcedRemotes, want)
	}
	a := prof.Authority["acme/web-client"]
	if a.CanPush || a.CanMerge.Allow || a.MergeGrant != "" {
		t.Errorf("authority = %+v: the committed self-grant must not apply", a)
	}
	if want := filepath.Join(cfgDir, "ext-owners.yaml"); prof.OwnersFile != want {
		t.Errorf("owners_file = %q, want %q (anchored to the external config's directory)", prof.OwnersFile, want)
	}

	// What describes the project still comes from the committed file.
	if prof.QueueRoot != "./committed-queue" || prof.StoreRoot != "./committed-tasks" {
		t.Errorf("the committed file still describes the project: queue_root=%q store_root=%q", prof.QueueRoot, prof.StoreRoot)
	}
	if prof.ProjectRoot() != repo {
		t.Errorf("ProjectRoot = %q, want the repo %q", prof.ProjectRoot(), repo)
	}
}

// When the operator's config says nothing about a grant key, the committed value still does not
// stand: the operator's (default) value does.
func TestLoadFrom_AnOperatorConfigSilentOnAGrantKeyStillBeatsTheCommittedValue(t *testing.T) {
	repo, _ := operatorFixture(t, "queue_root: ./ext-queue\n")
	prof, _, err := LoadFrom(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prof.PushWhitelist, DefaultPushWhitelist) {
		t.Errorf("push_whitelist = %v, want the default %v", prof.PushWhitelist, DefaultPushWhitelist)
	}
	if len(prof.Authority) != 0 || len(prof.EnforcedRemotes) != 0 || prof.OwnersFile != "" {
		t.Errorf("committed authority/enforced_remotes/owners_file leaked through: %+v %v %q", prof.Authority, prof.EnforcedRemotes, prof.OwnersFile)
	}
}

// With no operator config, a committed config governs itself, as before.
func TestLoadFrom_WithoutAnOperatorConfigTheCommittedFileApplies(t *testing.T) {
	repo, _ := operatorFixture(t, "")
	prof, _, err := LoadFrom(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the committed %v: no operator config, nothing to override it", prof.PushWhitelist, want)
	}
	if prof.Authority["acme/web-client"].MergeGrant != "SELF-GRANTED" {
		t.Errorf("the committed authority should apply with no operator config: %+v", prof.Authority)
	}
}

// Load() is the same, from the cwd.
func TestLoad_TheOperatorsGrantKeysBeatACommittedConfig(t *testing.T) {
	repo, _ := operatorFixture(t, operatorConfig)
	chdir(t, repo)
	prof, path, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("web-client", DefaultProfileName)) {
		t.Errorf("the reported profile is still the committed file, got %q", path)
	}
	if want := []string{"feat/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want %v", prof.PushWhitelist, want)
	}
}

// $LEADKIT_PROFILE is the operator naming a file themselves: it is not repo content, and not overlaid.
func TestLoad_AnExplicitProfileIsTheOperatorsOwnChoiceAndIsNotOverlaid(t *testing.T) {
	repo, _ := operatorFixture(t, operatorConfig)
	chdir(t, repo)
	explicit := filepath.Join(t.TempDir(), "mine.yaml")
	if err := os.WriteFile(explicit, []byte("push_whitelist: ['mine/*']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", explicit)
	prof, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mine/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the explicit file's %v", prof.PushWhitelist, want)
	}
}

// --show-origin tells the truth about where a grant key came from.
func TestLoadWithOrigin_GrantKeysAreAttributedToTheExternalConfig(t *testing.T) {
	repo, _ := operatorFixture(t, operatorConfig)
	chdir(t, repo)
	_, _, origins, err := LoadWithOrigin()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range grantKeys {
		if origins[k].Layer != "external" {
			t.Errorf("%s attributed to %q, want external", k, origins[k].Layer)
		}
	}
	if origins["queue_root"].Layer == "external" {
		t.Errorf("queue_root describes the project and comes from the committed file, got %q", origins["queue_root"].Layer)
	}
}

// An external config declared for ANOTHER repo (a same-named clone with a different origin) is not
// this repo's operator config, but that is no licence for the clone's own committed file to grant
// itself: the grant keys are WITHHELD (built-in defaults, so default-deny), the ordinary keys still
// come from the committed file, and the load is not refused.
func TestLoadFrom_ForeignCloneWithAnOperatorConfigPresentCannotGrantItself(t *testing.T) {
	repo, _ := operatorFixture(t, "project_origin: acme/web-client\n"+operatorConfig)
	stubOrigins(t, map[string]string{repo: "git@github.com:someone-else/web-client.git"})
	prof, _, err := LoadFrom(repo)
	if err != nil {
		t.Fatalf("a foreign clone must still load (ordinary keys apply), got %v", err)
	}
	def := Defaults()
	if !reflect.DeepEqual(prof.PushWhitelist, def.PushWhitelist) {
		t.Errorf("push_whitelist = %v, want the built-in default %v: neither the committed ['*'] nor the other owner's list applies", prof.PushWhitelist, def.PushWhitelist)
	}
	if len(prof.Authority) != 0 {
		t.Errorf("authority = %+v, want none (default-deny): the committed self-grant must not apply", prof.Authority)
	}
	if len(prof.EnforcedRemotes) != 0 || prof.OwnersFile != "" {
		t.Errorf("enforced_remotes = %v owners_file = %q, want the defaults (empty)", prof.EnforcedRemotes, prof.OwnersFile)
	}
	// Ordinary keys are kept: the clone is a different project and uses its own settings.
	if prof.QueueRoot != "./committed-queue" || prof.StoreRoot != "./committed-tasks" {
		t.Errorf("the committed file still describes the clone: queue_root=%q store_root=%q", prof.QueueRoot, prof.StoreRoot)
	}

	// ...while the repo the operator config IS for gets the operator's grants.
	stubOrigins(t, map[string]string{repo: "git@github.com:acme/web-client.git"})
	prof, _, err = LoadFrom(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"feat/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the operator's %v", prof.PushWhitelist, want)
	}
}

// Where the grant keys come from is visible: withheld ones are attributed to the defaults, not to
// the committed file.
func TestLoadWithOrigin_WithheldGrantKeysAreAttributedToTheDefaults(t *testing.T) {
	repo, _ := operatorFixture(t, "project_origin: acme/web-client\n"+operatorConfig)
	stubOrigins(t, map[string]string{repo: "git@github.com:someone-else/web-client.git"})
	chdir(t, repo)
	_, _, origins, err := LoadWithOrigin()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range grantKeys {
		if origins[k].Layer != "default" {
			t.Errorf("%s attributed to %q, want default (withheld)", k, origins[k].Layer)
		}
	}
	if origins["queue_root"].Layer != "project" {
		t.Errorf("queue_root describes the clone and stays attributed to the committed file, got %q", origins["queue_root"].Layer)
	}
}

// The adopter path is the one case where a committed file supplies the grant keys: no external
// config for the repo anywhere, so there is no operator to defer to.
func TestLoadFrom_WithNoOperatorConfigAtAllTheCommittedFileGoverns(t *testing.T) {
	repo, _ := operatorFixture(t, "")
	prof, _, err := LoadFrom(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the committed %v (no operator config exists)", prof.PushWhitelist, want)
	}
}

// An operator config that cannot be read is not a licence to fall back to the committed grants.
func TestLoadFrom_AnUnreadableOperatorConfigRefusesRatherThanFallingBackToTheCommittedGrants(t *testing.T) {
	repo, _ := operatorFixture(t, "push_whitelist: [unclosed\n")
	prof, _, err := LoadFrom(repo)
	if err == nil {
		t.Fatalf("want an error, got a profile with push_whitelist %v", prof.PushWhitelist)
	}
	if prof != nil || !strings.Contains(err.Error(), "config") {
		t.Errorf("a refused load returns no profile, got prof=%v err=%v", prof, err)
	}
}
