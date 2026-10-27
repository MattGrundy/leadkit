package registry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRaw runs a git command in dir, failing the test on error — the test's own git seam
// (separate from the Store's, so the test can set up remotes/identity directly).
func gitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// configIdentity sets a per-repo commit identity so the Store's own defaultGit commits
// succeed without depending on the machine's global git config.
func configIdentity(t *testing.T, repo string) {
	gitRaw(t, repo, "config", "user.name", "test")
	gitRaw(t, repo, "config", "user.email", "test@example.com")
}

// TestCrossClone_DisjointMerge is THE per-file design proof (spec's headline test): two
// clones of a shared remote each register a session; a `git pull --rebase` across them
// MERGES both entries with NO conflict — including the non-fast-forward case where one
// clone pushes second and its disjoint file rebases cleanly onto the other's.
func TestCrossClone_DisjointMerge(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitRaw(t, "", "init", "--bare", "-b", "main", remote)

	cloneA := filepath.Join(base, "A")
	gitRaw(t, "", "clone", remote, cloneA)
	configIdentity(t, cloneA)

	now := time.Now().UTC()
	// A registers first — establishes main on the remote with A's disjoint file.
	sA := Open(cloneA)
	_, syncA, err := sA.Register(RegisterParams{SessionID: "sessA", Name: "alice", Host: "mac1", Role: RoleLead, Project: "leadkit", Leads: []string{"leadkit"}, Now: now})
	if err != nil {
		t.Fatalf("A.Register: %v", err)
	}
	if !syncA.Pushed {
		t.Fatalf("A's first register should push cleanly, note=%q", syncA.Note)
	}

	// B clones AFTER A pushed (so it shares history), then registers a disjoint session.
	cloneB := filepath.Join(base, "B")
	gitRaw(t, "", "clone", remote, cloneB)
	configIdentity(t, cloneB)
	sB := Open(cloneB)

	// Create a concurrent-race: A commits+pushes a SECOND session while B is behind, so
	// B's push is a non-fast-forward that must pull-rebase-retry.
	_, syncA2, err := sA.Register(RegisterParams{SessionID: "sessA2", Name: "alice2", Host: "mac1", Role: RoleIC, Project: "leadkit", Now: now})
	if err != nil {
		t.Fatalf("A.Register(2): %v", err)
	}
	if !syncA2.Pushed {
		t.Fatalf("A's second register should push, note=%q", syncA2.Note)
	}

	_, syncB, err := sB.Register(RegisterParams{SessionID: "sessB", Name: "bob", Host: "mac2", Role: RoleLead, Project: "leadkit", Leads: []string{"svc"}, Now: now})
	if err != nil {
		t.Fatalf("B.Register: %v", err)
	}
	if !syncB.Pushed {
		t.Fatalf("B's register should succeed via pull-rebase-retry (disjoint files), note=%q", syncB.Note)
	}

	// A fresh clone of the remote must carry ALL THREE disjoint entries, no conflict.
	cloneC := filepath.Join(base, "C")
	gitRaw(t, "", "clone", remote, cloneC)
	entries, ok := Open(cloneC).ReadAll()
	if !ok {
		t.Fatal("ReadAll on fresh clone failed")
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.SessionID] = true
	}
	for _, want := range []string{"sessA", "sessA2", "sessB"} {
		if !got[want] {
			t.Errorf("merged registry missing %s; have %v", want, keys(got))
		}
	}
	// No conflict markers anywhere in the merged tree.
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(cloneC, "sessions", EntryFileName(e.Host, e.SessionID)))
		if strings.Contains(string(data), "<<<<<<<") {
			t.Errorf("conflict marker in %s@%s entry", e.SessionID, e.Host)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestInit_Idempotent: cloning attaches a fresh repo; a second Init on an already-git repo
// with an origin is a no-op that reports the existing remote.
func TestInit_Idempotent(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitRaw(t, "", "init", "--bare", "-b", "main", remote)

	repo := filepath.Join(base, "reg")
	s := Open(repo)

	res, err := s.Init(remote)
	if err != nil {
		t.Fatalf("Init clone: %v", err)
	}
	if res.Action != "cloned" {
		t.Fatalf("first Init should clone, got %q", res.Action)
	}
	if !s.Initialized() {
		t.Fatal("registry should be Initialized after clone")
	}

	res2, err := s.Init(remote)
	if err != nil {
		t.Fatalf("idempotent Init: %v", err)
	}
	if res2.Action != "already-initialized" {
		t.Fatalf("second Init should be a no-op, got %q", res2.Action)
	}
}

// TestInit_NoRemoteNoRepo errors (nothing to clone), not panics.
func TestInit_NoRemoteNoRepo(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "reg"))
	if _, err := s.Init(""); err == nil {
		t.Fatal("Init with no remote and no repo should error")
	}
}

// fakeGit drives the sync retry loop without a real remote: push fails until failPushes
// reaches 0, everything else succeeds. It records the push count.
type fakeGit struct {
	failPushes int
	pushCalls  int
}

func (f *fakeGit) run(dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "config": // remote.origin.url — pretend an origin exists
		return "https://example.com/registry.git", nil
	case "rev-parse":
		return "main\n", nil
	case "push":
		f.pushCalls++
		if f.failPushes > 0 {
			f.failPushes--
			return "! [rejected] main -> main (non-fast-forward)", errNonFastForward
		}
		return "", nil
	default: // add, commit, pull, fetch
		return "", nil
	}
}

var errNonFastForward = &gitErr{"non-fast-forward"}

type gitErr struct{ s string }

func (e *gitErr) Error() string { return e.s }

// TestSync_NonFastForwardRetry: a push rejected as non-fast-forward pull-rebase-retries and
// then succeeds (disjoint files rebase cleanly).
func TestSync_NonFastForwardRetry(t *testing.T) {
	repo := t.TempDir()
	mustMkGitDir(t, repo)
	f := &fakeGit{failPushes: 2} // fail twice, succeed on the third attempt
	s := OpenWithGit(repo, f.run)

	_, sync, err := s.Register(RegisterParams{SessionID: "s1", Host: "mac1", Role: RoleLead, Project: "p", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !sync.Pushed {
		t.Fatalf("expected push to succeed after retries, note=%q", sync.Note)
	}
	if f.pushCalls != 3 {
		t.Errorf("expected 3 push attempts, got %d", f.pushCalls)
	}
}

// TestSync_ExhaustedRetries: a push that never succeeds gives up best-effort — no error,
// the local entry stays intact, and the outcome carries a note.
func TestSync_ExhaustedRetries(t *testing.T) {
	repo := t.TempDir()
	mustMkGitDir(t, repo)
	f := &fakeGit{failPushes: 100} // always fail
	s := OpenWithGit(repo, f.run)

	_, sync, err := s.Register(RegisterParams{SessionID: "s1", Host: "mac1", Role: RoleLead, Project: "p", Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("exhausted retries must NOT surface an error: %v", err)
	}
	if sync.Pushed {
		t.Fatal("push should not report success")
	}
	if sync.Note == "" {
		t.Error("expected a give-up note")
	}
	if f.pushCalls != maxSyncRetries {
		t.Errorf("expected %d bounded push attempts, got %d", maxSyncRetries, f.pushCalls)
	}
	// Local entry intact.
	if _, err := os.Stat(filepath.Join(repo, "sessions", EntryFileName("mac1", "s1"))); err != nil {
		t.Errorf("local entry must survive a failed sync: %v", err)
	}
}

// mustMkGitDir makes repo look like a git repo (a .git dir) so isGitRepo() is true while
// the fake GitFunc serves the actual git operations.
func mustMkGitDir(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}
