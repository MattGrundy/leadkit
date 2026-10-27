package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustRegister(t *testing.T, s *Store, p RegisterParams) Entry {
	t.Helper()
	e, _, err := s.Register(p)
	if err != nil {
		t.Fatalf("Register(%s@%s): %v", p.SessionID, p.Host, err)
	}
	return e
}

// TestRegister_WritesOnlyOwnEntry proves the own-identity rule: a session's entry file is
// keyed by (host, session_id) resolved by the tool, and a second session writes a DISJOINT
// file — the per-file design that keeps cross-machine merges conflict-free.
func TestRegister_WritesOnlyOwnEntry(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo) // not a git repo — sync degrades, local write still happens

	now := time.Now().UTC()
	mustRegister(t, s, RegisterParams{SessionID: "sessA", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, Now: now})
	mustRegister(t, s, RegisterParams{SessionID: "sessB", Name: "bob", Host: "mac2", Role: RoleIC, Project: "leadkit", Now: now})

	fileA := filepath.Join(repo, "sessions", EntryFileName("mac1", "sessA"))
	fileB := filepath.Join(repo, "sessions", EntryFileName("mac2", "sessB"))
	if fileA == fileB {
		t.Fatal("two sessions must map to disjoint files")
	}
	for _, f := range []string{fileA, fileB} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected entry file %s: %v", f, err)
		}
	}

	entries, ok := s.ReadAll()
	if !ok || len(entries) != 2 {
		t.Fatalf("ReadAll: ok=%v len=%d, want 2 entries", ok, len(entries))
	}
}

// TestRegister_Touch refreshes only updated_at and preserves role/leads/started_at.
func TestRegister_Touch(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	mustRegister(t, s, RegisterParams{SessionID: "s1", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"#123"}, Now: start})

	later := start.Add(10 * time.Minute)
	e := mustRegister(t, s, RegisterParams{SessionID: "s1", Host: "mac1", Now: later, Touch: true})
	if e.Role != RoleLead {
		t.Errorf("touch lost role: %q", e.Role)
	}
	if len(e.Leads) != 1 || e.Leads[0] != "#123" {
		t.Errorf("touch lost leads: %v", e.Leads)
	}
	if !e.StartedAt.Equal(start) {
		t.Errorf("touch changed started_at: got %v want %v", e.StartedAt, start)
	}
	if !e.UpdatedAt.Equal(later) {
		t.Errorf("touch didn't refresh updated_at: got %v want %v", e.UpdatedAt, later)
	}
}

// TestRegister_Clear removes this session's entry.
func TestRegister_Clear(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo)
	now := time.Now().UTC()
	mustRegister(t, s, RegisterParams{SessionID: "s1", Host: "mac1", Role: RoleWatcher, Project: "leadkit", Now: now})
	file := filepath.Join(repo, "sessions", EntryFileName("mac1", "s1"))
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("precondition: entry should exist: %v", err)
	}
	mustRegister(t, s, RegisterParams{SessionID: "s1", Host: "mac1", Now: now, Clear: true})
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("clear should remove the entry file, stat err=%v", err)
	}
}

// TestLiveAndFilter covers the who staleness window and the project/role/host filters.
func TestLiveAndFilter(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{SessionID: "a", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "b", Name: "bob", Host: "mac2", Role: RoleIC, Project: "leadkit", UpdatedAt: now.Add(-5 * time.Minute)},
		{SessionID: "c", Name: "carol", Host: "mac1", Role: RoleReviewer, Project: "other", UpdatedAt: now.Add(-90 * time.Minute)}, // stale
	}
	ttl := 30 * time.Minute
	live := Live(entries, now.Add(-ttl))
	if len(live) != 2 {
		t.Fatalf("Live: got %d, want 2 (carol is stale)", len(live))
	}

	byProject := Filter{Project: "leadkit"}.Apply(live)
	if len(byProject) != 2 {
		t.Errorf("filter project=leadkit: got %d, want 2", len(byProject))
	}
	byRole := Filter{Role: "lead"}.Apply(live)
	if len(byRole) != 1 || byRole[0].Name != "alice" {
		t.Errorf("filter role=lead: got %v", byRole)
	}
	byHost := Filter{Host: "mac2"}.Apply(live)
	if len(byHost) != 1 || byHost[0].Name != "bob" {
		t.Errorf("filter host=mac2: got %v", byHost)
	}
}

// TestAmbiguousLeadership is the conflict signal: two live role:lead entries claiming the
// SAME leads slug flag AMBIGUOUS-LEADERSHIP (the two-leadkit-lead / duplicate-slug
// case). A single lead per slug, and a lead appearing once, never conflict.
func TestAmbiguousLeadership(t *testing.T) {
	now := time.Now().UTC()
	live := []Entry{
		{SessionID: "a", Name: "lead-mac1", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now},
		{SessionID: "b", Name: "lead-mac2", Host: "mac2", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now},
		{SessionID: "c", Name: "ic", Host: "mac1", Role: RoleIC, Project: "leadkit", UpdatedAt: now},
	}
	amb := AmbiguousLeadership(live)
	if len(amb) != 1 {
		t.Fatalf("expected 1 ambiguous slug, got %d: %+v", len(amb), amb)
	}
	if amb[0].Slug != "leadkit" || len(amb[0].Leaders) != 2 {
		t.Fatalf("ambiguous claim wrong: %+v", amb[0])
	}

	// One lead per slug is not ambiguous.
	if got := AmbiguousLeadership(live[:1]); len(got) != 0 {
		t.Errorf("single lead should not be ambiguous, got %+v", got)
	}

	// A lead with no explicit leads implicitly leads its project — still conflicts.
	implicit := []Entry{
		{SessionID: "x", Host: "mac1", Role: RoleLead, Project: "svc", UpdatedAt: now},
		{SessionID: "y", Host: "mac2", Role: RoleLead, Project: "svc", UpdatedAt: now},
	}
	if got := AmbiguousLeadership(implicit); len(got) != 1 || got[0].Slug != "svc" {
		t.Errorf("implicit-project ambiguity not detected: %+v", got)
	}
}

// TestFormatWho renders the table and, prominently, the ambiguous banner; stale rows only
// appear when passed (the --all path).
func TestFormatWho(t *testing.T) {
	now := time.Now().UTC()
	live := []Entry{
		{SessionID: "a", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now.Add(-time.Minute)},
		{SessionID: "b", Name: "bob", Host: "mac2", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, UpdatedAt: now.Add(-time.Minute)},
	}
	out := FormatWho(live, nil, "", now)
	if !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Errorf("table missing sessions:\n%s", out)
	}
	if !strings.Contains(out, "AMBIGUOUS-LEADERSHIP") {
		t.Errorf("ambiguous banner missing:\n%s", out)
	}

	stale := []Entry{{SessionID: "c", Name: "carol", Host: "mac1", Role: RoleWatcher, Project: "x", UpdatedAt: now.Add(-time.Hour)}}
	withStale := FormatWho(live[:1], stale, "", now)
	if !strings.Contains(withStale, "carol") || !strings.Contains(withStale, "stale") {
		t.Errorf("--all should show stale rows:\n%s", withStale)
	}
	noStale := FormatWho(live[:1], nil, "", now)
	if strings.Contains(noStale, "carol") {
		t.Errorf("default who should omit stale rows:\n%s", noStale)
	}
}

// TestDegrade_UninitializedRegistry proves the SYNC-mode (remote configured) degrade path:
// with a remote set but the dir NOT yet a git repo, register writes local and reports a
// sync-failed note; who reads the local cache and reports it can't sync. Neither errors.
// (LOCAL mode — no remote — is covered by TestLocalMode_NoGitNoWarning: no note at all.)
func TestDegrade_UninitializedRegistry(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo).WithRemote("https://example.invalid/registry.git") // SYNC intent, no repo yet
	now := time.Now().UTC()

	_, sync, err := s.Register(RegisterParams{SessionID: "s1", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, Now: now})
	if err != nil {
		t.Fatalf("register on uninitialized registry must not error: %v", err)
	}
	if sync.Pushed {
		t.Error("nothing should have been pushed on an uninitialized registry")
	}
	if sync.Note == "" || !strings.Contains(sync.Note, "not a git repo") {
		t.Errorf("expected a sync-failed note, got %q", sync.Note)
	}
	// The local entry is intact.
	if _, err := os.Stat(filepath.Join(repo, "sessions", EntryFileName("mac1", "s1"))); err != nil {
		t.Fatalf("local entry should be written despite no sync: %v", err)
	}

	ok, note := s.PullBestEffort()
	if ok {
		t.Error("PullBestEffort should report not-ok on an uninitialized registry")
	}
	if note == "" {
		t.Error("PullBestEffort should carry a degrade note")
	}
}

// TestPrune_DryRunAndApply: dry-run mutates nothing; apply removes ONLY stale entries.
func TestPrune_DryRunAndApply(t *testing.T) {
	repo := t.TempDir()
	s := Open(repo)
	now := time.Now().UTC()
	mustRegister(t, s, RegisterParams{SessionID: "live", Host: "mac1", Role: RoleLead, Project: "p", Now: now})
	mustRegister(t, s, RegisterParams{SessionID: "stale", Host: "mac2", Role: RoleIC, Project: "p", Now: now.Add(-2 * time.Hour)})

	cutoff := now.Add(-30 * time.Minute)

	dry, err := s.Prune(cutoff, false)
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}
	if len(dry.Candidates) != 1 || dry.Candidates[0].SessionID != "stale" {
		t.Fatalf("dry-run candidates wrong: %+v", dry.Candidates)
	}
	if _, err := os.Stat(filepath.Join(repo, "sessions", EntryFileName("mac2", "stale"))); err != nil {
		t.Fatalf("dry-run must not remove anything: %v", err)
	}

	res, err := s.Prune(cutoff, true)
	if err != nil {
		t.Fatalf("prune apply: %v", err)
	}
	if len(res.Removed) != 1 {
		t.Fatalf("apply should remove 1 stale entry, got %d", len(res.Removed))
	}
	if _, err := os.Stat(filepath.Join(repo, "sessions", EntryFileName("mac2", "stale"))); !os.IsNotExist(err) {
		t.Errorf("stale entry should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "sessions", EntryFileName("mac1", "live"))); err != nil {
		t.Errorf("live entry must survive prune: %v", err)
	}
}

// TestEntryFileName_Sanitizes ensures unsafe chars in host/session collapse to a single
// safe segment, and the "__" separator stays unambiguous (input "_" -> "-").
func TestEntryFileName_Sanitizes(t *testing.T) {
	name := EntryFileName("my/host name", "sess_id/../x")
	if strings.ContainsAny(name[:len(name)-len(".json")], "/ ") {
		t.Errorf("filename not sanitized: %q", name)
	}
	if strings.Count(name, "__") != 1 {
		t.Errorf("expected exactly one __ separator in %q", name)
	}
}

// TestEntry_JSONRoundTrip guards the on-disk schema (the field names other machines parse).
func TestEntry_JSONRoundTrip(t *testing.T) {
	e := Entry{SessionID: "s", Name: "n", Host: "h", Role: RoleLead, Project: "p", Leads: []string{"a", "b"}, StartedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_id", "name", "host", "role", "project", "leads", "started_at", "updated_at"} {
		if !strings.Contains(string(data), "\""+key+"\"") {
			t.Errorf("missing json key %q in %s", key, data)
		}
	}
	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Role != RoleLead || len(got.Leads) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
