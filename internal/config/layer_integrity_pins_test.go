package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Layer-resolution integrity pins (ticket 331, found by the ticket-156 mutation sweep). Every
// property here held before the test existed; each test names the mutant it defends and was seen
// to fail under it.

// Mutant CL1: in loadLayerFile, widen `os.IsNotExist(err)` to `err != nil`. A layer file that
// EXISTS but cannot be read is then reported as an absent layer, so a team or user layer holding
// restrictions (a lock, a narrower push_whitelist) is silently dropped from the merge instead of
// failing the load.
//
// A directory in the file's place is read-fails-for-a-reason-other-than-absence on every platform
// and for root; a mode-0 file is the realistic permission failure but root reads it anyway.
func TestLoadLayerFile_ReadFailureIsAnErrorNotAnAbsentLayer(t *testing.T) {
	t.Run("a directory where the file should be", func(t *testing.T) {
		data, err := loadLayerFile(t.TempDir())
		if err == nil {
			t.Errorf("a layer path that exists and cannot be read as a file must surface, got data=%q err=nil", data)
		}
	})
	t.Run("a file with no read permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-0 file")
		}
		p := filepath.Join(t.TempDir(), "team.yaml")
		if err := os.WriteFile(p, []byte("push_whitelist: [x/*]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		if data, err := loadLayerFile(p); err == nil {
			t.Errorf("a layer that exists and cannot be read must surface, got data=%q err=nil", data)
		}
	})
}

// The other side of CL1, so the pin cannot be met by erroring on everything: an absent layer (and
// the "no layer configured" empty path) is still data=nil, err=nil.
func TestLoadLayerFile_AnAbsentLayerIsNilNil(t *testing.T) {
	for name, path := range map[string]string{
		"no path":      "",
		"missing file": filepath.Join(t.TempDir(), "nope.yaml"),
	} {
		data, err := loadLayerFile(path)
		if data != nil || err != nil {
			t.Errorf("%s: got data=%q err=%v, want nil, nil", name, data, err)
		}
	}
}

// Mutant CL1, end to end: the same widening, observed through Load() for each of the two layers
// that go through loadLayerFile. The unit test above would not notice a mutant that swallowed the
// error one frame up, in loadLayered.
func TestLoad_AnUnreadableTeamLayerFailsTheLoad(t *testing.T) {
	sandboxUserLayer(t)
	root := t.TempDir()
	// A directory as the team config: unreadable as a file, on every platform and for root.
	teamDir := filepath.Join(root, "team-config.yaml")
	if err := os.Mkdir(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProject(t, root, "team_config: "+teamDir+"\nqueue_root: q\n")

	prof, _, err := Load()
	if err == nil {
		t.Fatalf("a team layer that exists and cannot be read must fail the load, not be ignored; got profile with Outputs=%v", prof.Outputs)
	}
	if !strings.Contains(err.Error(), "team config") {
		t.Errorf("the error must say which layer, got %q", err)
	}
}

func TestLoad_AnUnreadableUserLayerFailsTheLoad(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "workspace_model: worktrees\n")
	userFile := filepath.Join(xdg, "leadkit", "config.yaml")
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	if err := os.Chmod(userFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(userFile, 0o644) })
	root := t.TempDir()
	writeProject(t, root, "queue_root: q\n")

	if _, _, err := Load(); err == nil {
		t.Fatal("a user layer that exists and cannot be read must fail the load, not be ignored")
	} else if !strings.Contains(err.Error(), "user config") {
		t.Errorf("the error must say which layer, got %q", err)
	}
}

// Mutant CL2: in resolveLayers, overlay the user layer AFTER the team layer (user over team). For a
// key that is neither a preference nor locked, the precedence is project > team > user, so where
// only user and team set it the team's value must stand. The existing precedence tests cover
// user-vs-project (a preference key, settled in pass 2) and team-vs-project, but none puts a
// plain key in both user and team, so swapping those two overlays passed everything.
func TestLoad_ATeamValueBeatsTheUsersForAPlainKey(t *testing.T) {
	xdg := sandboxUserLayer(t)
	writeUserConfig(t, xdg, "outputs: [jira]\n")
	root := t.TempDir()
	teamPath := writeTeamConfig(t, root, "outputs: [linear]\n")
	writeProject(t, root, "team_config: "+teamPath+"\nqueue_root: q\n") // the project sets no outputs

	prof, _, origins, err := LoadWithOrigin()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prof.Outputs, []string{"linear"}) {
		t.Errorf("Outputs = %v, want [linear]: for a plain key the team layer beats the user layer", prof.Outputs)
	}
	if got := origins["outputs"].Layer; got != "team" {
		t.Errorf("origin of outputs = %q, want team", got)
	}
}

// Mutant CG11: ApplyOperatorGrants reports applied=true when the repo has NO external config.
// The caller (the stand-in profile for a committed file that failed to load) tells the user "the
// operator's external config governs the grant keys" on applied, so a true here makes it claim a
// governor that does not exist, while the profile's own values are what actually stand.
func TestApplyOperatorGrants_NoExternalConfigAppliesNothing(t *testing.T) {
	repo, _ := operatorFixture(t, "") // a committed config, and NO external one
	prof := Defaults()
	prof.PushWhitelist = []string{"mine/*"}
	before := prof.PushWhitelist

	applied, err := ApplyOperatorGrants(&prof, repo)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("applied = true with no external config: nothing was applied")
	}
	if !reflect.DeepEqual(prof.PushWhitelist, before) {
		t.Errorf("push_whitelist changed to %v with no external config; the profile's own values must stand", prof.PushWhitelist)
	}
}

// The other side of CG11: with an external config, applied is true and the operator's grants
// replace the profile's.
func TestApplyOperatorGrants_AnExternalConfigIsAppliedAndSaidSo(t *testing.T) {
	repo, _ := operatorFixture(t, operatorConfig)
	prof := Defaults()
	prof.PushWhitelist = []string{"*"}

	applied, err := ApplyOperatorGrants(&prof, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Error("applied = false with an external config present")
	}
	if want := []string{"feat/*"}; !reflect.DeepEqual(prof.PushWhitelist, want) {
		t.Errorf("push_whitelist = %v, want the operator's %v", prof.PushWhitelist, want)
	}
}
