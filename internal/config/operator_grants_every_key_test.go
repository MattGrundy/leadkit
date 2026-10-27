package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Ticket 234. The overlay exists so a branch cannot authorise its own push, and what authorises a push
// is more than the keys that name branches and repos: records_dir is where the review gate reads its BYPASS file
// from, and guards is the enforcement mode of every guard. A committed value for either could switch the
// gate off, and a hand-kept list of keys is how a later key slips out. These tests are driven by
// grantKeys itself: an entry with no probe, or one setGrantKeys does not copy, fails.

// grantProbe is one grant key: a committed value, a different operator value, and how to read the value
// the loaded profile ends up with.
type grantProbe struct {
	committed, external string
	read                func(*Profile, string) any // the profile and the external config dir
	wantExternal        func(cfgDir string) any
	wantDefault         func(*Profile) any // what the key is with nothing from the operator
	// adopter is the committed value used when there is NO operator config, and wantAdopter what the
	// profile then holds. Empty adopter means committed is used and wantAdopter is not consulted: a key
	// whose committed value is also its default (guards: advisory) needs a different one to be seen.
	adopter     string
	wantAdopter func(cfgDir string) any
}

var grantProbes = map[string]grantProbe{
	"push_whitelist": {
		committed: "push_whitelist: ['*']\n", external: "push_whitelist: ['feat/*']\n",
		read:         func(p *Profile, _ string) any { return p.PushWhitelist },
		wantExternal: func(string) any { return []string{"feat/*"} },
		wantDefault:  func(*Profile) any { return DefaultPushWhitelist },
	},
	"authority": {
		committed: "authority:\n  acme/web-client:\n    can_push: true\n", external: "authority:\n  acme/web-client:\n    can_push: false\n",
		read:         func(p *Profile, _ string) any { return p.Authority["acme/web-client"].CanPush },
		wantExternal: func(string) any { return false },
		wantDefault:  func(*Profile) any { return false },
	},
	"enforced_remotes": {
		committed: "enforced_remotes: [nothing-at-all]\n", external: "enforced_remotes: [acme/web-client]\n",
		read:         func(p *Profile, _ string) any { return p.EnforcedRemotes },
		wantExternal: func(string) any { return []string{"acme/web-client"} },
		wantDefault:  func(*Profile) any { return []string(nil) },
	},
	"owners_file": {
		committed: "owners_file: .leadkit/committed-owners.yaml\n", external: "owners_file: ext-owners.yaml\n",
		read:         func(p *Profile, _ string) any { return p.OwnersFile },
		wantExternal: func(cfgDir string) any { return filepath.Join(cfgDir, "ext-owners.yaml") },
		wantDefault:  func(*Profile) any { return "" },
	},
	"records_dir": {
		committed: "records_dir: ./committed-recs\n", external: "records_dir: ./ext-recs\n",
		read:         func(p *Profile, _ string) any { return p.Resolve(p.RecordsDir) },
		wantExternal: func(cfgDir string) any { return filepath.Join(cfgDir, "ext-recs") },
		wantDefault:  func(p *Profile) any { d := Defaults(); return p.Resolve(d.RecordsDir) },
	},
	"guards": {
		committed: "guards:\n  default_mode: advisory\n", external: "guards:\n  default_mode: enforce\n",
		adopter:      "guards:\n  default_mode: advisory\n  push_gate:\n    mode: enforce\n",
		wantAdopter:  func(string) any { return "enforce" },
		read:         func(p *Profile, _ string) any { return p.EffectiveGuardMode(GuardPushGate) },
		wantExternal: func(string) any { return "enforce" },
		wantDefault:  func(p *Profile) any { d := Defaults(); return d.EffectiveGuardMode(GuardPushGate) },
	},
}

// probeFixture is a repo with a COMMITTED config carrying probe.committed and, when external is not
// empty, the operator's external config carrying it.
func probeFixture(t *testing.T, committed, external string) (repo, cfgDir string) {
	t.Helper()
	_, cfgDir = externalTestHome(t)
	repo = mkRepo(t, "web-client")
	if err := os.WriteFile(filepath.Join(repo, DefaultProfileName), []byte("queue_root: ./committed-queue\n"+committed), 0o644); err != nil {
		t.Fatal(err)
	}
	if external != "" {
		writeExternalConfig(t, cfgDir, "web-client", "queue_root: ./ext-queue\n"+external)
	}
	return repo, cfgDir
}

// Every entry of grantKeys has a probe, and every probe is an entry: a key added to the slice without a
// way to prove it is the operator's fails here, and so does a probe for a key that is not a grant.
func TestGrantKeys_EveryEntryHasAProbeAndEveryProbeIsAnEntry(t *testing.T) {
	inSlice := map[string]bool{}
	for _, k := range grantKeys {
		inSlice[k] = true
		if _, ok := grantProbes[k]; !ok {
			t.Errorf("grant key %q has no probe in grantProbes: add one that shows the operator's value winning", k)
		}
	}
	for k := range grantProbes {
		if !inSlice[k] {
			t.Errorf("grantProbes has %q, which is not in grantKeys", k)
		}
	}
}

// For EACH grant key: a committed value, an operator value that differs, and the operator's wins.
func TestGrantKeys_EveryEntryIsTakenFromTheOperatorConfig(t *testing.T) {
	for _, k := range grantKeys {
		probe, ok := grantProbes[k]
		if !ok {
			continue // reported by the test above
		}
		t.Run(k, func(t *testing.T) {
			repo, cfgDir := probeFixture(t, probe.committed, probe.external)
			prof, _, err := LoadFrom(repo)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := probe.read(prof, cfgDir), probe.wantExternal(cfgDir); !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %v, want the operator's %v: the committed value must not stand", k, got, want)
			}
		})
	}
}

// With no external config the committed value applies: the adopter path is unchanged.
func TestGrantKeys_WithNoOperatorConfigTheCommittedValueApplies(t *testing.T) {
	for _, k := range grantKeys {
		probe, ok := grantProbes[k]
		if !ok {
			continue
		}
		t.Run(k, func(t *testing.T) {
			committed, want := probe.committed, probe.wantAdopter
			if probe.adopter != "" {
				committed = probe.adopter
			} else {
				want = nil
			}
			repo, cfgDir := probeFixture(t, committed, "")
			prof, _, err := LoadFrom(repo)
			if err != nil {
				t.Fatal(err)
			}
			got := probe.read(prof, cfgDir)
			if want != nil {
				if !reflect.DeepEqual(got, want(cfgDir)) {
					t.Errorf("%s = %v, want the committed value's %v (no operator config)", k, got, want(cfgDir))
				}
				return
			}
			if reflect.DeepEqual(got, probe.wantExternal(cfgDir)) || reflect.DeepEqual(got, probe.wantDefault(prof)) {
				t.Errorf("%s = %v: with no operator config this should be the committed value, not the operator's or the default", k, got)
			}
		})
	}
}

// An operator config that says nothing about a key still beats the committed value: the default stands.
func TestGrantKeys_AnOperatorConfigSilentOnAKeyStillBeatsTheCommittedValue(t *testing.T) {
	for _, k := range grantKeys {
		probe, ok := grantProbes[k]
		if !ok {
			continue
		}
		t.Run(k, func(t *testing.T) {
			repo, cfgDir := probeFixture(t, probe.committed, "project_id: only\n")
			prof, _, err := LoadFrom(repo)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := probe.read(prof, cfgDir), probe.wantDefault(prof); !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %v, want the default %v", k, got, want)
			}
		})
	}
}

// An external config declared for ANOTHER repo withholds every grant key, the two new ones included.
func TestGrantKeys_AForeignCloneGetsTheDefaultsForEveryEntry(t *testing.T) {
	for _, k := range grantKeys {
		probe, ok := grantProbes[k]
		if !ok {
			continue
		}
		t.Run(k, func(t *testing.T) {
			repo, cfgDir := probeFixture(t, probe.committed, "project_origin: acme/web-client\n"+probe.external)
			stubOrigins(t, map[string]string{repo: "git@github.com:someone-else/web-client.git"})
			prof, _, err := LoadFrom(repo)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := probe.read(prof, cfgDir), probe.wantDefault(prof); !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %v, want the built-in default %v: neither the committed nor the other owner's value applies", k, got, want)
			}
		})
	}
}

// --show-origin attributes every grant key to the operator, the records_dir and guards keys included.
func TestGrantKeys_ShowOriginAttributesEveryEntryToTheExternalConfig(t *testing.T) {
	repo, _ := probeFixture(t, "records_dir: ./committed-recs\nguards:\n  default_mode: advisory\n", "records_dir: ./ext-recs\nguards:\n  default_mode: enforce\n")
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
}
