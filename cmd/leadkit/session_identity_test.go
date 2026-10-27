package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/usage"
)

// mkdir is a tiny test helper: create dir (and parents) or fail.
func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withGitToplevel installs a deterministic gitToplevel that maps want→top and restores
// the original on cleanup — the same injection guard-target-path-normalization's tests
// use, so identity resolution needs no real git binary.
func withGitToplevel(t *testing.T, m map[string]string) {
	t.Helper()
	orig := gitToplevel
	gitToplevel = func(dir string) (string, bool) {
		if top, ok := m[dir]; ok {
			return top, true
		}
		return "", false
	}
	t.Cleanup(func() { gitToplevel = orig })
}

// TestSessionIdentitySlug_ConfigLessRepo_UsesOwnBasename is THE core-bug test: a
// config-less git repo at .../container-base — with $LEADKIT_PROFILE pointing at a
// DIFFERENT project AND an ancestor leadkit.yaml above it — resolves its identity from
// its OWN git toplevel basename, never the env's or the ancestor's slug.
func TestSessionIdentitySlug_ConfigLessRepo_UsesOwnBasename(t *testing.T) {
	parent := t.TempDir()
	// An ancestor leadkit.yaml ABOVE the repo — identity must NOT walk up to it.
	if err := os.WriteFile(filepath.Join(parent, config.DefaultProfileName), []byte("project_id: an-ancestor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := mkdir(t, filepath.Join(parent, "container-base")) // NO own leadkit.yaml
	cwd := mkdir(t, filepath.Join(repo, "cmd", "deep"))       // resolve from a nested dir
	withGitToplevel(t, map[string]string{cwd: repo})

	// A stray $LEADKIT_PROFILE for a DIFFERENT project — identity must ignore it entirely.
	otherProfile := filepath.Join(t.TempDir(), config.DefaultProfileName)
	if err := os.WriteFile(otherProfile, []byte("project_id: legacy-migration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", otherProfile)
	t.Setenv("LEADKIT_QUEUE", filepath.Join(t.TempDir(), "queue"))

	slug, source := sessionIdentitySlug(cwd)
	if slug != "container-base" {
		t.Fatalf("config-less repo must resolve its OWN basename slug; got %q (source %q) — env/ancestor collapse still present", slug, source)
	}
	if source != "git-repo basename" {
		t.Errorf("source = %q, want %q", source, "git-repo basename")
	}
}

// TestSessionIdentitySlug_OwnProjectID_Unchanged is the regression guard: a project WITH
// its own leadkit.yaml (project_id set) resolves EXACTLY as install.ResolveProjectSlug
// does today — identity is a no-op for adopted projects.
func TestSessionIdentitySlug_OwnProjectID_Unchanged(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, config.DefaultProfileName), []byte("project_id: Mixed-Case-Example\nqueue_root: queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withGitToplevel(t, map[string]string{repo: repo})

	slug, source := sessionIdentitySlug(repo)
	want := install.ResolveProjectSlug("Mixed-Case-Example", "")
	if slug != want {
		t.Fatalf("own project_id must resolve identically to ResolveProjectSlug; got %q want %q", slug, want)
	}
	if source != "own leadkit.yaml project_id" {
		t.Errorf("source = %q, want own-project_id", source)
	}
}

// TestSessionIdentitySlug_OwnQueueRoot_Unchanged: an own config with only queue_root (no
// project_id) resolves to the own-config queue-root basename slug — unchanged.
func TestSessionIdentitySlug_OwnQueueRoot_Unchanged(t *testing.T) {
	parent := t.TempDir()
	repo := mkdir(t, filepath.Join(parent, "acmecorp-web"))
	if err := os.WriteFile(filepath.Join(repo, config.DefaultProfileName), []byte("queue_root: queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withGitToplevel(t, map[string]string{repo: repo})

	slug, source := sessionIdentitySlug(repo)
	want := install.ResolveProjectSlug("", filepath.Join(repo, "queue"))
	if slug != want || slug != "acmecorp-web" {
		t.Fatalf("own queue_root slug = %q, want %q (acmecorp-web)", slug, want)
	}
	if source != "own leadkit.yaml queue_root" {
		t.Errorf("source = %q, want own-queue_root", source)
	}
}

// TestSessionIdentitySlug_LeadkitRepoIsLeadkitNotProjects reproduces the observed
// self-mis-ID: the leadkit repo (no own config here) under an ancestor Projects/
// leadkit.yaml resolves to "leadkit" (its own toplevel), never "projects" (the ancestor).
func TestSessionIdentitySlug_LeadkitRepoIsLeadkitNotProjects(t *testing.T) {
	projects := mkdir(t, filepath.Join(t.TempDir(), "Projects"))
	if err := os.WriteFile(filepath.Join(projects, config.DefaultProfileName), []byte("project_id: projects\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := mkdir(t, filepath.Join(projects, "leadkit")) // the leadkit clone, no own config
	withGitToplevel(t, map[string]string{repo: repo})

	slug, _ := sessionIdentitySlug(repo)
	if slug != "leadkit" {
		t.Fatalf("leadkit repo must resolve to %q, got %q (ancestor Projects/leadkit.yaml leaked)", "leadkit", slug)
	}
}

// TestSessionIdentitySlug_NonGitCwd_UsesCwdBasename: a non-git cwd derives from the cwd
// basename; env/ancestor are still ignored.
func TestSessionIdentitySlug_NonGitCwd_UsesCwdBasename(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, config.DefaultProfileName), []byte("project_id: an-ancestor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := mkdir(t, filepath.Join(parent, "loose-scripts"))
	withGitToplevel(t, map[string]string{}) // not a git repo → gitToplevel returns false
	t.Setenv("LEADKIT_PROFILE", filepath.Join(t.TempDir(), config.DefaultProfileName))

	slug, source := sessionIdentitySlug(cwd)
	if slug != "loose-scripts" {
		t.Fatalf("non-git cwd slug = %q, want loose-scripts", slug)
	}
	if source != "cwd basename" {
		t.Errorf("source = %q, want cwd-basename", source)
	}
}

// TestResolveSessionID_FallbackChain: $LEADKIT_SESSION_ID → $CLAUDE_CODE_SESSION_ID →
// slug, in that order.
func TestResolveSessionID_FallbackChain(t *testing.T) {
	t.Run("explicit LEADKIT_SESSION_ID wins", func(t *testing.T) {
		t.Setenv("LEADKIT_SESSION_ID", "explicit-42")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "harness-uuid")
		id, src := resolveSessionID("proj")
		if id != "explicit-42" || src != "LEADKIT_SESSION_ID" {
			t.Fatalf("got %q/%q, want explicit-42/LEADKIT_SESSION_ID", id, src)
		}
	})
	t.Run("harness uuid when LEADKIT_SESSION_ID unset", func(t *testing.T) {
		t.Setenv("LEADKIT_SESSION_ID", "")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "b7e19500-uuid")
		id, src := resolveSessionID("proj")
		if id != "b7e19500-uuid" || src != "CLAUDE_CODE_SESSION_ID" {
			t.Fatalf("got %q/%q, want the harness uuid (NOT the slug)", id, src)
		}
		if id == "proj" {
			t.Fatal("session id fell back to the slug even though CLAUDE_CODE_SESSION_ID was set — the clobber bug")
		}
	})
	t.Run("slug last resort", func(t *testing.T) {
		t.Setenv("LEADKIT_SESSION_ID", "")
		t.Setenv("CLAUDE_CODE_SESSION_ID", "")
		id, src := resolveSessionID("proj")
		if id != "proj" || src != "slug-fallback" {
			t.Fatalf("got %q/%q, want proj/slug-fallback", id, src)
		}
	})
}

// TestUniqueSessionID_TwoInstancesDistinctPresenceFiles: with the harness UUID present,
// two instances of ONE project write DISTINCT presence files (no clobber); with neither
// env set they collapse onto the single slug-keyed file (documented last-resort).
func TestUniqueSessionID_TwoInstancesDistinctPresenceFiles(t *testing.T) {
	presenceDir := t.TempDir()
	slug := "container-base"
	now := time.Now().UTC()

	t.Setenv("LEADKIT_SESSION_ID", "")
	for _, uuid := range []string{"uuid-8b", "uuid-84"} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", uuid)
		id, _ := resolveSessionID(slug)
		if err := usage.WritePresenceEntryWithHarness(presenceDir, slug, id, "", uuid, now); err != nil {
			t.Fatal(err)
		}
	}
	entries, ok := usage.ReadPresence(presenceDir, time.Time{})
	if !ok {
		t.Fatal("ReadPresence ok=false")
	}
	if len(entries) != 2 {
		t.Fatalf("two instances must register 2 distinct presence files, got %d: %+v", len(entries), entries)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.SessionID] = true
	}
	if !seen["uuid-8b"] || !seen["uuid-84"] {
		t.Fatalf("distinct session ids not both present: %+v", entries)
	}
}

// TestIdentityVsPolicy_Boundary is the deliberate-divergence test: for a config-less
// session with $LEADKIT_PROFILE pointing elsewhere, the POLICY resolution (config.Load →
// ResolveProjectSlug) still honors the env — UNCHANGED — while IDENTITY resolves the
// session's own cwd/git-repo. The two intentionally diverge; whoami/doctor surface it.
func TestIdentityVsPolicy_Boundary(t *testing.T) {
	// The env profile the POLICY path must keep honoring.
	polDir := t.TempDir()
	polProfile := filepath.Join(polDir, config.DefaultProfileName)
	if err := os.WriteFile(polProfile, []byte("project_id: policy-project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", polProfile)

	// POLICY resolution is UNCHANGED — config.Load honors $LEADKIT_PROFILE.
	prof, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	policySlug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	if policySlug != "policy-project" {
		t.Fatalf("POLICY resolution must still honor $LEADKIT_PROFILE; got %q, want policy-project (regression in the untouched policy path)", policySlug)
	}

	// IDENTITY resolves the session's OWN config-less repo, diverging on purpose.
	repo := mkdir(t, filepath.Join(t.TempDir(), "identity-project"))
	withGitToplevel(t, map[string]string{repo: repo})
	idSlug, _ := sessionIdentitySlug(repo)
	if idSlug != "identity-project" {
		t.Fatalf("IDENTITY must resolve the own repo; got %q", idSlug)
	}
	if idSlug == policySlug {
		t.Fatal("identity and policy should diverge for a config-less session under a stray $LEADKIT_PROFILE — that divergence is exactly what whoami surfaces")
	}
}
