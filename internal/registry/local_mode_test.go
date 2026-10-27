package registry

// Coverage for the LOCAL-by-default behaviour (docs/build-tasks/registry-local-default.md):
// with no remote configured and no .git, the registry is a plain local dir — register/who
// work with zero setup, NO git shell-out, and NO degrade warning. SYNC mode (remote
// configured OR dir-is-git-with-origin) is unchanged and still exercised by git_test.go.

import (
	"strings"
	"testing"
	"time"
)

// recordingGit is an injectable GitFunc that records every call — the LOCAL-mode tests
// assert it is NEVER invoked.
type recordingGit struct{ calls [][]string }

func (r *recordingGit) run(dir string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{dir}, args...))
	return "", nil
}

// TestLocalMode_NoGitNoWarning is the headline local test: register (and prune, and the
// read-path sync) in LOCAL mode writes/updates the local files, reports NO degrade note,
// and NEVER shells git. Two sessions produce two disjoint local files.
func TestLocalMode_NoGitNoWarning(t *testing.T) {
	repo := t.TempDir()
	rec := &recordingGit{}
	s := OpenWithGit(repo, rec.run) // no remote, no .git → LOCAL mode

	if s.SyncMode() {
		t.Fatal("a plain dir with no remote and no .git must be LOCAL mode")
	}

	now := time.Now().UTC()
	_, sync, err := s.Register(RegisterParams{SessionID: "s1", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, Now: now})
	if err != nil {
		t.Fatalf("register in local mode must not error: %v", err)
	}
	// The local write is complete — nothing attempted/committed/pushed, no degrade note.
	if sync.Note != "" || sync.Attempted || sync.Pushed || sync.Committed {
		t.Errorf("local register must be a clean no-sync, got %+v", sync)
	}

	// A second session → a second, disjoint local file.
	if _, _, err := s.Register(RegisterParams{SessionID: "s2", Host: "mac2", Role: RoleIC, Project: "leadkit", Now: now}); err != nil {
		t.Fatalf("second register: %v", err)
	}

	// who's read-path sync is a clean no-op in LOCAL mode (ok, no note).
	if ok, note := s.PullBestEffort(); !ok || note != "" {
		t.Errorf("PullBestEffort in local mode must be clean, got ok=%v note=%q", ok, note)
	}

	entries, ok := s.ReadAll()
	if !ok || len(entries) != 2 {
		t.Fatalf("expected 2 local entries, got ok=%v len=%d", ok, len(entries))
	}

	// THE ASSERTION: git was NEVER shelled anywhere on the LOCAL path.
	if len(rec.calls) != 0 {
		t.Fatalf("LOCAL mode must never shell git; recorded calls: %v", rec.calls)
	}

	// Prune is git-free in LOCAL mode too.
	if _, err := s.Prune(now.Add(time.Hour), true); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("LOCAL prune must never shell git; recorded calls: %v", rec.calls)
	}
}

// TestLocalMode_WhoCleanTable proves who prints a clean table in LOCAL mode: PullBestEffort
// returns ok (so main builds no note), and the rendered table carries no "not initialized"/
// "possibly stale" framing — and still shells no git.
func TestLocalMode_WhoCleanTable(t *testing.T) {
	repo := t.TempDir()
	rec := &recordingGit{}
	s := OpenWithGit(repo, rec.run)
	now := time.Now().UTC()

	if _, _, err := s.Register(RegisterParams{SessionID: "s1", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, Now: now}); err != nil {
		t.Fatalf("register: %v", err)
	}

	ok, note := s.PullBestEffort()
	entries, _ := s.ReadAll()
	live := Live(entries, now.Add(-time.Hour))

	// Mirror main.runWho's note logic: a note is built only when the sync is not ok.
	whoNote := ""
	if !ok {
		whoNote = "(possibly stale — sync failed: " + note + ")"
	}
	out := FormatWho(live, nil, whoNote, now)

	if strings.Contains(out, "possibly stale") || strings.Contains(out, "not initialized") {
		t.Errorf("LOCAL who table must be clean (no degrade framing):\n%s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("table should list the session:\n%s", out)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("LOCAL who must never shell git; recorded calls: %v", rec.calls)
	}
}

// TestSyncMode_Detection pins the mode rule: remote configured → SYNC (short-circuits before
// any git call); dir-is-git-with-origin → SYNC; git repo WITHOUT an origin → LOCAL.
func TestSyncMode_Detection(t *testing.T) {
	// A configured remote is SYNC — decided WITHOUT shelling git.
	rec := &recordingGit{}
	if s := OpenWithGit(t.TempDir(), rec.run).WithRemote("https://x/registry.git"); !s.SyncMode() {
		t.Error("a configured remote must be SYNC mode")
	}
	if len(rec.calls) != 0 {
		t.Errorf("remote-configured SyncMode must short-circuit before git: %v", rec.calls)
	}

	// A git repo whose origin resolves (fakeGit's `config` returns an origin url) is SYNC.
	repo := t.TempDir()
	mustMkGitDir(t, repo)
	if s := OpenWithGit(repo, (&fakeGit{}).run); !s.SyncMode() {
		t.Error("a git repo with an origin must be SYNC mode")
	}

	// A git repo whose origin can't be read (no origin) stays LOCAL.
	repo2 := t.TempDir()
	mustMkGitDir(t, repo2)
	noOrigin := func(dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "config" {
			return "", &gitErr{"no origin"}
		}
		return "", nil
	}
	if OpenWithGit(repo2, noOrigin).SyncMode() {
		t.Error("a git repo without an origin must be LOCAL mode")
	}
}
