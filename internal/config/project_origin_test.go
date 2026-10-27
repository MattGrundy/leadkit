package config

import (
	"strings"
	"testing"
)

// Ticket 139. An external config is found by the origin's repo NAME, so a clone of a different
// owner's repo of the same name loaded it and took its absolute store_root and queue_root.

func TestOriginMatchesProject(t *testing.T) {
	cases := []struct {
		origin, key string
		want        bool
	}{
		{"git@github.com:acme/leadkit.git", "acme/leadkit", true},
		{"https://github.com/acme/leadkit.git", "acme/leadkit", true},
		{"https://github.com/acme/leadkit", "acme/leadkit", true},
		{"ssh://git@github.com/acme/leadkit.git", "acme/leadkit", true},
		{"https://user:token@github.com/acme/leadkit.git", "acme/leadkit", true},
		{"git@github.com:acme/leadkit.git", "github.com/acme/leadkit", true},
		{"git@gitlab.com:acme/leadkit.git", "github.com/acme/leadkit", false}, // another host
		{"git@github.com:evil/leadkit.git", "acme/leadkit", false},            // another owner
		{"git@github.com:acme/leadkit-fork.git", "acme/leadkit", false},       // a longer repo name
		{"git@github.com:me/acme/leadkit.git", "acme/leadkit", true},          // nested group: the tail is the key
		{"git@github.com:xacme/leadkit.git", "acme/leadkit", false},           // not at a path boundary
		{"git@github.com:ACME/leadkit.git", "acme/leadkit", false},            // case differs: fails closed
		{"git@github.com:acme/leadkit.git", "leadkit", false},                 // no owner would match any clone
		{"git@github.com:acme/leadkit.git", "", false},
		{"", "acme/leadkit", false}, // no origin cannot be shown to match
	}
	for _, c := range cases {
		if got := OriginMatchesProject(c.origin, c.key); got != c.want {
			t.Errorf("OriginMatchesProject(%q, %q) = %v, want %v", c.origin, c.key, got, c.want)
		}
	}
}

// stubOrigins makes gitOriginURLDir answer from a map of repo dir -> origin URL.
func stubOrigins(t *testing.T, m map[string]string) {
	t.Helper()
	orig := gitOriginURLDir
	gitOriginURLDir = func(dir string) string { return m[dir] }
	t.Cleanup(func() { gitOriginURLDir = orig })
}

func TestExternalConfig_ARepoWhoseOriginDoesNotMatchProjectOriginIsRefused(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	mine, foreign := mkRepo(t, "leadkit"), mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{
		mine:    "git@github.com:acme/leadkit.git",
		foreign: "git@github.com:evil/leadkit.git",
	})
	// Both resolve to the same file: the slug is the repo name.
	writeExternalConfig(t, cfgDir, "leadkit", "project_origin: acme/leadkit\nstore_root: /owners/real/tasks\nqueue_root: /owners/real/queue\n")

	prof, found, err := LoadFrom(mine)
	if err != nil || !found || prof.StoreRoot != "/owners/real/tasks" {
		t.Fatalf("the owner's own checkout must load its config: found=%v err=%v prof=%+v", found, err, prof)
	}

	prof, _, err = LoadFrom(foreign)
	if err == nil || !strings.Contains(err.Error(), "project_origin") {
		t.Fatalf("a different owner's clone of the same name must be refused, got err=%v", err)
	}
	if prof != nil {
		t.Errorf("a refused load must not hand back a profile (and its store_root): %+v", prof)
	}

	chdir(t, foreign)
	if prof, _, err := Load(); err == nil || prof != nil {
		t.Errorf("Load from the foreign clone must refuse too: prof=%v err=%v", prof, err)
	}
}

// With no origin at all, a config that declares one cannot be shown to apply: refused.
func TestExternalConfig_ARepoWithNoOriginIsRefusedWhenTheConfigDeclaresOne(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	repo := mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{})
	writeExternalConfig(t, cfgDir, "leadkit", "project_origin: acme/leadkit\nstore_root: /owners/real/tasks\n")
	if _, _, err := LoadFrom(repo); err == nil {
		t.Error("a repo with no origin must not load a config that names one")
	}
}

// Honest about the limit: a config WITHOUT project_origin is not checked (that is the exposure
// `config init --external` now closes by writing the key). Pinned so a change to it is deliberate.
func TestExternalConfig_WithoutProjectOriginIsNotChecked(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	foreign := mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{foreign: "git@github.com:evil/leadkit.git"})
	writeExternalConfig(t, cfgDir, "leadkit", "store_root: /owners/real/tasks\n")
	if prof, found, err := LoadFrom(foreign); err != nil || !found || prof.StoreRoot != "/owners/real/tasks" {
		t.Errorf("an unmarked config loads as before: found=%v err=%v", found, err)
	}
}

// The starter `config init --external` writes names the repo it was written for, so what it
// writes passes its own check for that repo and fails for another.
func TestStarterConfigFor_WritesAProjectOriginThatMatchesItsRepo(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	mine, foreign := mkRepo(t, "leadkit"), mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{
		mine:    "git@github.com:acme/leadkit.git",
		foreign: "git@github.com:evil/leadkit.git",
	})
	po := ProjectOriginForDir(mine)
	if po != "acme/leadkit" {
		t.Fatalf("ProjectOriginForDir = %q, want acme/leadkit", po)
	}
	starter := StarterConfigFor("leadkit", true, po)
	if !strings.Contains(starter, "\nproject_origin: acme/leadkit\n") {
		t.Fatalf("the external starter must carry project_origin:\n%s", starter)
	}
	if strings.Contains(StarterConfigFor("leadkit", false, po), "project_origin") {
		t.Error("an in-repo starter has no use for project_origin")
	}
	writeExternalConfig(t, cfgDir, "leadkit", starter)
	if _, _, err := LoadFrom(mine); err != nil {
		t.Errorf("the repo the starter was written for must load it: %v", err)
	}
	if _, _, err := LoadFrom(foreign); err == nil {
		t.Error("another owner's clone must be refused by the starter's own project_origin")
	}
}

// A remote can carry a credential (`https://user:token@host/...`), and the refusal fires on
// exactly the clone whose origin does not match: the failure case must not print it.
func TestExternalConfig_TheRefusalNeverPrintsACredentialInTheRemote(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	for _, origin := range []string{
		"https://deploy:ghp_SECRETTOKEN123@github.com/evil/leadkit.git",
		"https://ghp_SECRETTOKEN123@github.com/evil/leadkit.git",
		"https://deploy:ghp_SECRETTOKEN123@github.com:8443/evil/leadkit.git",
		"ssh://ghp_SECRETTOKEN123@github.com/evil/leadkit.git",
		"ghp_SECRETTOKEN123@github.com:evil/leadkit.git",
		"https://deploy:p@sSECRETTOKEN123@github.com/evil/leadkit.git", // an unencoded @ in the password
	} {
		foreign := mkRepo(t, "leadkit")
		stubOrigins(t, map[string]string{foreign: origin})
		writeExternalConfig(t, cfgDir, "leadkit", "project_origin: acme/leadkit\nstore_root: /owners/real/tasks\n")
		_, _, err := LoadFrom(foreign)
		if err == nil {
			t.Fatalf("%s: a mismatched clone must be refused", origin)
		}
		if strings.Contains(err.Error(), "SECRETTOKEN") || strings.Contains(err.Error(), "deploy") {
			t.Errorf("%s: the refusal leaks the credential: %v", origin, err)
		}
		// ...and still names both origins, so the operator can see what did not match.
		if !strings.Contains(err.Error(), "acme/leadkit") || !strings.Contains(err.Error(), "github.com") || !strings.Contains(err.Error(), "evil/leadkit") {
			t.Errorf("%s: the refusal must still name the declared origin and the host and path found: %v", origin, err)
		}
	}
}

func TestRedactOrigin(t *testing.T) {
	cases := map[string]string{
		"https://user:tok@github.com/acme/leadkit.git": "github.com/acme/leadkit",
		"https://tok@github.com/acme/leadkit":          "github.com/acme/leadkit",
		"https://user:tok@host:8443/acme/leadkit.git":  "host/8443/acme/leadkit", // a port reads as scp's colon: harmless for display and matching
		"ssh://git@github.com/acme/leadkit.git":        "github.com/acme/leadkit",
		"git@github.com:acme/leadkit.git":              "github.com/acme/leadkit",
		"https://github.com/acme/leadkit.git":          "github.com/acme/leadkit",
		"https://u:p%40ss@github.com/acme/leadkit.git": "github.com/acme/leadkit",
		"": "",
	}
	for in, want := range cases {
		if got := RedactOrigin(in); got != want {
			t.Errorf("RedactOrigin(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(RedactOrigin(in), "tok") || strings.Contains(RedactOrigin(in), "p%40ss") {
			t.Errorf("RedactOrigin(%q) kept a credential: %q", in, RedactOrigin(in))
		}
	}
}

// The DECLARED key is printed too, and a key pasted from `git remote get-url origin` is accepted
// (matching strips userinfo on both sides), so a credentialed key must be redacted like the origin.
func TestExternalConfig_TheRefusalNeverPrintsACredentialInTheDeclaredKey(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	foreign := mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{foreign: "git@github.com:evil/leadkit.git"})
	for _, key := range []string{
		"https://deploy:ghp_SECRETKEY123@github.com/acme/leadkit.git",
		"https://ghp_SECRETKEY123@github.com/acme/leadkit",
		"ghp_SECRETKEY123@github.com:acme/leadkit.git",
		"https://deploy:p@sSECRETKEY123@github.com/acme/leadkit.git",
	} {
		writeExternalConfig(t, cfgDir, "leadkit", "project_origin: '"+key+"'\nstore_root: /owners/real/tasks\n")
		_, _, err := LoadFrom(foreign)
		if err == nil {
			t.Fatalf("%s: a mismatched clone must be refused", key)
		}
		if strings.Contains(err.Error(), "SECRETKEY") || strings.Contains(err.Error(), "deploy") {
			t.Errorf("%s: the refusal leaks the credential in the declared key: %v", key, err)
		}
		if !strings.Contains(err.Error(), "github.com/acme/leadkit") {
			t.Errorf("%s: the refusal should still name the declared key (redacted): %v", key, err)
		}
	}
	// ...and the same credentialed key is accepted by its own repo, so this is reachable.
	own := mkRepo(t, "leadkit")
	stubOrigins(t, map[string]string{own: "git@github.com:acme/leadkit.git"})
	writeExternalConfig(t, cfgDir, "leadkit", "project_origin: 'https://deploy:ghp_SECRETKEY123@github.com/acme/leadkit.git'\nstore_root: /owners/real/tasks\n")
	if _, _, err := LoadFrom(own); err != nil {
		t.Errorf("a key pasted from the remote URL matches that repo, which is how it reaches the message: %v", err)
	}
}

// `config show` prints the resolved profile; a credentialed project_origin must not appear in it.
func TestFormatShow_NeverPrintsACredentialInProjectOrigin(t *testing.T) {
	p := Defaults()
	p.ProjectOrigin = "https://deploy:ghp_SECRETKEY123@github.com/acme/leadkit.git"
	for name, render := range map[string]func() (string, error){
		"show":        func() (string, error) { return FormatShow(&p, "/c.yaml") },
		"show-origin": func() (string, error) { return FormatShowOrigin(&p, "/c.yaml", nil) },
	} {
		out, err := render()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "SECRETKEY") || strings.Contains(out, "deploy") {
			t.Errorf("%s leaks the credential:\n%s", name, out)
		}
		if !strings.Contains(out, "project_origin: github.com/acme/leadkit") {
			t.Errorf("%s should still show the key, redacted:\n%s", name, out)
		}
	}
	if p.ProjectOrigin == "github.com/acme/leadkit" {
		t.Error("showing must not rewrite the profile it was given")
	}
}
