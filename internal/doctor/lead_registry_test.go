package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/registry"
)

// writeRegEntry writes one registry entry file into repo/sessions/ (the on-disk form other
// machines' clones carry), so the fail-open lead-registry check can be exercised without a
// real git remote.
func writeRegEntry(t *testing.T, repo string, e registry.Entry) {
	t.Helper()
	dir := filepath.Join(repo, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, registry.EntryFileName(e.Host, e.SessionID)), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func findCheck(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in report", name)
	return Check{}
}

func regProfile() *config.Profile {
	d := config.Defaults()
	d.RegistryTTL = "30m"
	return &d
}

// TestLeadRegistry_AmbiguousWarns: 2+ live leads for one slug → WARN, never a hard error.
func TestLeadRegistry_AmbiguousWarns(t *testing.T) {
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "lead1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})
	writeRegEntry(t, repo, registry.Entry{SessionID: "b", Name: "lead2", Host: "mac2", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	c := leadRegistryCheck(regProfile(), repo, "mac1", "a", now)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("want a WARN, got %+v", c)
	}
	if !strings.Contains(c.Detail, "AMBIGUOUS-LEADERSHIP") {
		t.Errorf("detail should name the ambiguity: %q", c.Detail)
	}
}

// TestLeadRegistry_UnregisteredWarns: this session absent from the registry → WARN.
func TestLeadRegistry_UnregisteredWarns(t *testing.T) {
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "lead1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	c := leadRegistryCheck(regProfile(), repo, "mac9", "not-registered", now)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("want a WARN, got %+v", c)
	}
	if !strings.Contains(c.Detail, "UNREGISTERED") {
		t.Errorf("detail should flag the unregistered session: %q", c.Detail)
	}
}

// TestLeadRegistry_CleanIsInfo: one lead per slug + this session registered → OK/Info.
func TestLeadRegistry_CleanIsInfo(t *testing.T) {
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "lead1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	c := leadRegistryCheck(regProfile(), repo, "mac1", "a", now)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("clean registry should be OK/Info, got %+v", c)
	}
}

// TestLeadRegistry_UninitializedIsInfo: no repo / empty registry is Info, never an error.
func TestLeadRegistry_UninitializedIsInfo(t *testing.T) {
	if c := leadRegistryCheck(regProfile(), "", "", "", time.Now()); !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("empty registry-repo should be Info, got %+v", c)
	}
	empty := t.TempDir() // exists but no sessions/ dir
	if c := leadRegistryCheck(regProfile(), empty, "mac1", "a", time.Now()); !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("uninitialized registry should be Info, got %+v", c)
	}
}

// TestLeadRegistry_LocalModeQuiet: in LOCAL mode (no remote, no .git) doctor never nags
// about stale-sync/uninitialization — but ambiguous-leadership STILL fires.
func TestLeadRegistry_LocalModeQuiet(t *testing.T) {
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "lead1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	c := leadRegistryCheck(regProfile(), repo, "mac1", "a", now)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("LOCAL clean registry must be OK/Info, got %+v", c)
	}
	if strings.Contains(c.Detail, "stale sync") {
		t.Errorf("LOCAL mode must never warn about stale sync: %q", c.Detail)
	}

	// Ambiguous leadership STILL fires in LOCAL mode (and still carries no stale-sync noise).
	writeRegEntry(t, repo, registry.Entry{SessionID: "b", Name: "lead2", Host: "mac2", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})
	c2 := leadRegistryCheck(regProfile(), repo, "mac1", "a", now)
	if c2.OK || !strings.Contains(c2.Detail, "AMBIGUOUS-LEADERSHIP") {
		t.Errorf("ambiguous leadership must still warn in LOCAL mode: %+v", c2)
	}
	if strings.Contains(c2.Detail, "stale sync") {
		t.Errorf("LOCAL mode must not add stale-sync alongside ambiguity: %q", c2.Detail)
	}
}

// TestLeadRegistry_EmptyLocalMentionsLocal: an empty LOCAL registry's advisory frames it as
// a local registry and does NOT nag about `registry init --remote`.
func TestLeadRegistry_EmptyLocalMentionsLocal(t *testing.T) {
	empty := t.TempDir()
	c := leadRegistryCheck(regProfile(), empty, "mac1", "a", time.Now())
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("empty local registry should be Info, got %+v", c)
	}
	if !strings.Contains(c.Detail, "local registry") || strings.Contains(c.Detail, "registry init --remote") {
		t.Errorf("empty LOCAL registry should not nag about init --remote: %q", c.Detail)
	}
}

// TestLeadRegistry_SyncModeStaleSyncWarns: in SYNC mode (dir is a git repo) an OLD
// FETCH_HEAD still surfaces the stale-sync warning (remote-mode behaviour unchanged).
func TestLeadRegistry_SyncModeStaleSyncWarns(t *testing.T) {
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "lead1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	gitDir := filepath.Join(repo, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fh := filepath.Join(gitDir, "FETCH_HEAD")
	if err := os.WriteFile(fh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour) // > leadRegistryStaleSyncWindow (2h)
	if err := os.Chtimes(fh, old, old); err != nil {
		t.Fatal(err)
	}

	c := leadRegistryCheck(regProfile(), repo, "mac1", "a", now)
	if c.OK || !strings.Contains(c.Detail, "stale sync") {
		t.Fatalf("SYNC mode must keep the stale-sync warning, got %+v", c)
	}
}

// TestLeadRegistry_WiredIntoRun confirms the check is part of a full doctor Run and is
// never critical (fail-open).
func TestLeadRegistry_WiredIntoRun(t *testing.T) {
	prof := cleanProfile(t)
	repo := t.TempDir()
	now := time.Now().UTC()
	writeRegEntry(t, repo, registry.Entry{SessionID: "a", Name: "l1", Host: "mac1", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})
	writeRegEntry(t, repo, registry.Entry{SessionID: "b", Name: "l2", Host: "mac2", Role: registry.RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now})

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", repo, "mac1", "a", "")
	c := findCheck(t, report, "lead-registry")
	if c.Severity == SeverityCritical {
		t.Fatalf("lead-registry must never be critical (fail-open), got %+v", c)
	}
	if c.OK {
		t.Errorf("expected the ambiguous-leadership WARN to surface, got %+v", c)
	}
	// Ambiguous leadership is a WARN, so it must NOT flip doctor's critical gate.
	if !report.AllCriticalPass() {
		t.Error("lead-registry WARN must not fail the critical gate")
	}
}
