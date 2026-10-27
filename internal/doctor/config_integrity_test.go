package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/config"
)

// externalProfile writes an external config under a temp HOME and returns its path and the
// profile it parses to, with the history directory pointed at a temp dir too.
func externalProfile(t *testing.T, body string) (path string, history string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg"))
	history = filepath.Join(t.TempDir(), "config-history")
	orig := configHistoryDir
	configHistoryDir = func() string { return history }
	t.Cleanup(func() { configHistoryDir = orig })
	dir := config.ExternalConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "proj.yaml")
	writeCfg(t, path, body)
	return path, history
}

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func profileOf(t *testing.T, path string) *config.Profile {
	t.Helper()
	p, _, err := config.LoadProjectFile(path)
	if err != nil || p == nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return p
}

const grantA = "authority:\n  acme/proj:\n    can_merge: true\n    merge_grant: INC-1\n"

// Ticket 138. The file that grants merge authority is outside the repo and the review gate; the
// only trace an edit leaves is the one doctor keeps.
func TestConfigIntegrity_ReportsTheGrantAndWarnsOnceWhenTheFileChanges(t *testing.T) {
	path, history := externalProfile(t, grantA)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	first := externalConfigIntegrityCheck(profileOf(t, path), path, at)
	if !first.OK || first.Severity != SeverityInfo || !strings.Contains(first.Detail, "authority.acme/proj: can_merge=true merge_grant=INC-1") || !strings.Contains(first.Detail, "baseline") {
		t.Fatalf("first run: want an info check naming the grant and saving a baseline, got %+v", first)
	}

	again := externalConfigIntegrityCheck(profileOf(t, path), path, at.Add(time.Hour))
	if !again.OK || !strings.Contains(again.Detail, "unchanged since 2026-10-09T12:00:00Z") {
		t.Fatalf("unchanged file: want a quiet check dated from the baseline, got %+v", again)
	}

	// An edit that widens the grant: warned, with both hashes and where the copies are.
	writeCfg(t, path, grantA+"    push_whitelist: ['*']\n")
	changed := externalConfigIntegrityCheck(profileOf(t, path), path, at.Add(2*time.Hour))
	if changed.OK || changed.Severity != SeverityWarn || !strings.Contains(changed.Detail, "CHANGED") || !strings.Contains(changed.Detail, history) {
		t.Fatalf("an edited file must warn and name the history, got %+v", changed)
	}
	// ...once: the next look is quiet again, dated from the change.
	quiet := externalConfigIntegrityCheck(profileOf(t, path), path, at.Add(3*time.Hour))
	if !quiet.OK || !strings.Contains(quiet.Detail, "unchanged since 2026-10-09T14:00:00Z") {
		t.Fatalf("after the warning the file is the new baseline, got %+v", quiet)
	}

	// Both versions are kept, outside the config directory, so the change can be diffed.
	entries, err := os.ReadDir(filepath.Join(history, "proj"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("want two snapshots, got %v %v", entries, err)
	}
	if strings.HasPrefix(history, config.ExternalConfigDir()) {
		t.Errorf("history %s must not live inside the config dir", history)
	}
	oldCopy, _ := os.ReadFile(filepath.Join(history, "proj", entries[0].Name()))
	if string(oldCopy) != grantA {
		t.Errorf("the first snapshot must hold the original text, got %q", oldCopy)
	}
}

func TestConfigIntegrity_OnlyAnExternalConfigIsInScope(t *testing.T) {
	path, history := externalProfile(t, grantA)
	inRepo := filepath.Join(t.TempDir(), "leadkit.yaml")
	writeCfg(t, inRepo, grantA)
	for _, p := range []string{"", inRepo} {
		c := externalConfigIntegrityCheck(profileOf(t, path), p, time.Now())
		if !c.OK || !strings.Contains(c.Detail, "nothing to check") {
			t.Errorf("profilePath %q: want nothing to check, got %+v", p, c)
		}
	}
	if _, err := os.Stat(history); err == nil {
		t.Error("a profile that is not the external config must record nothing")
	}
}

func TestMergeGrantSummary(t *testing.T) {
	cases := map[string]string{
		"queue_root: q\n": "no authority entry grants merge",
		"authority:\n  a/b:\n    can_merge: false\n":                             "no authority entry grants merge",
		"authority:\n  a/b:\n    can_merge: human\n":                             "authority.a/b: can_merge=human",
		"authority:\n  a/b:\n    can_merge: false\n    merge_grant: X-1\n":       "authority.a/b: can_merge=false merge_grant=X-1",
		"authority:\n  a/b:\n    can_merge: true\n  c/d:\n    can_merge: true\n": "authority.a/b: can_merge=true; authority.c/d: can_merge=true",
	}
	dir := t.TempDir()
	for body, want := range cases {
		p := filepath.Join(dir, "c.yaml")
		writeCfg(t, p, body)
		if got := mergeGrantSummary(profileOf(t, p)); !strings.Contains(got, want) {
			t.Errorf("%q: summary %q does not contain %q", body, got, want)
		}
	}
}

// The default history location is beside the config directory, not inside it.
func TestConfigHistoryDir_DefaultIsOutsideTheConfigDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, dir := configHistoryDir(), config.ExternalConfigDir()
	if strings.HasPrefix(got+string(filepath.Separator), dir+string(filepath.Separator)) {
		t.Errorf("history %s is inside the config dir %s", got, dir)
	}
	if filepath.Dir(got) != filepath.Dir(dir) {
		t.Errorf("history %s should sit beside the config dir %s", got, dir)
	}
}
