package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// requireGit skips a real-git test when no git binary is available (CI without git).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

// gitRun runs `git <args>` in dir with a hermetic identity + no global/system config
// (so the author's ~/.gitconfig can't affect signing, etc.), failing the test on error.
// It does NOT make init.defaultBranch hermetic: GIT_CONFIG_SYSTEM=/dev/null does not stop
// Apple git reading its bundled gitconfig (which sets main), so a fixture that needs a
// particular initial branch must name it with `init -b` (ticket 314).
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
}

// initRepoWithOrigin creates a real git repo at dir with remote.origin.url = originURL
// and one commit (so `git worktree add` has a HEAD to branch from). originURL may be ""
// to create a repo with NO origin. The initial branch is always "main", named explicitly
// rather than inherited from the machine's init.defaultBranch (ticket 314).
func initRepoWithOrigin(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	if originURL != "" {
		gitRun(t, dir, "remote", "add", "origin", originURL)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
}

// TestInitRepoWithOrigin_BranchIsMainWhateverGitsDefaultIs pins ticket 314: the fixture's
// branch is "main" even where git's init.defaultBranch is "master" (a GitHub runner). The
// default is forced with GIT_CONFIG_COUNT/KEY/VALUE env config, which outranks every config
// file, so the test exercises the CI condition on a machine whose own default is main.
func TestInitRepoWithOrigin_BranchIsMainWhateverGitsDefaultIs(t *testing.T) {
	requireGit(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "init.defaultBranch")
	t.Setenv("GIT_CONFIG_VALUE_0", "master")

	// Precondition: the simulated default really is master, or the assertion below proves nothing.
	probe := t.TempDir()
	gitRun(t, probe, "init", "-q")
	if got := gitIn(t, probe, "symbolic-ref", "--short", "HEAD"); got != "master" {
		t.Fatalf("precondition: a plain `git init` under the simulated default gave %q, want master", got)
	}

	repo := filepath.Join(t.TempDir(), "fixture")
	initRepoWithOrigin(t, repo, "https://github.com/acme/fixture.git")
	if got := gitIn(t, repo, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("fixture branch = %q, want main", got)
	}
	// The commit lives on main, so a test that names the branch resolves it.
	if got := gitIn(t, repo, "rev-parse", "--verify", "-q", "refs/heads/main^{commit}"); got == "" {
		t.Error("refs/heads/main does not resolve to a commit")
	}
}

// addWorktree adds a git worktree of repo at wtPath on a new branch, returning wtPath.
func addWorktree(t *testing.T, repo, wtPath, branch string) string {
	t.Helper()
	gitRun(t, repo, "worktree", "add", "-q", "-b", branch, wtPath)
	return wtPath
}

// TestSessionIdentitySlug_WorktreeResolvesToRepoOrigin is the #123 driver case: a repo
// whose clone dir name differs from its origin, with a WORKTREE under
// .claude/worktrees/<branch>. Resolving identity from the WORKTREE cwd must yield the
// ORIGIN repo-name, NOT the worktree-dir basename (the branch/chore name) and NOT the
// clone-dir basename.
func TestSessionIdentitySlug_WorktreeResolvesToRepoOrigin(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	// Clone dir name (product-web-2) deliberately differs from the origin (product-web).
	repo := filepath.Join(t.TempDir(), "product-web-2")
	initRepoWithOrigin(t, repo, "git@github.com:acmecorp/product-web.git")
	wt := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "helm-chart-cache"), "helm-chart-cache")

	slug, source := sessionIdentitySlug(wt)
	if slug != "product-web" {
		t.Fatalf("worktree identity slug = %q (source %q), want product-web (origin repo-name), NOT the worktree basename helm-chart-cache", slug, source)
	}
	if source != "git origin repo-name" {
		t.Errorf("source = %q, want %q", source, "git origin repo-name")
	}
}

// TestSessionIdentitySlug_CloneDirNameDiffersFromOrigin is the ci case: a repo checked out
// under a SHORT dir name (api) whose origin is a longer repo-name (api-gateway) resolves to
// the origin name, not the checkout-dir basename.
func TestSessionIdentitySlug_CloneDirNameDiffersFromOrigin(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	repo := filepath.Join(t.TempDir(), "api") // dir basename "api"
	initRepoWithOrigin(t, repo, "git@github.com:acmecorp/api-gateway.git")

	slug, source := sessionIdentitySlug(repo)
	if slug != "api-gateway" {
		t.Fatalf("clone-dir slug = %q, want api-gateway (origin), not the dir basename api", slug)
	}
	if source != "git origin repo-name" {
		t.Errorf("source = %q, want git-origin", source)
	}
}

// TestSessionIdentitySlug_TwoWorktreesSameSlug: two worktrees of ONE repo resolve to the
// SAME origin slug (they are distinguished by session uuid in presence/registry, not slug).
func TestSessionIdentitySlug_TwoWorktreesSameSlug(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	repo := filepath.Join(t.TempDir(), "product-web-2")
	initRepoWithOrigin(t, repo, "https://github.com/acmecorp/product-web.git")
	wtA := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "chore-a"), "chore-a")
	wtB := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "chore-b"), "chore-b")

	slugA, _ := sessionIdentitySlug(wtA)
	slugB, _ := sessionIdentitySlug(wtB)
	if slugA != "product-web" || slugB != "product-web" {
		t.Fatalf("two worktrees must share the origin slug; got %q and %q", slugA, slugB)
	}
}

// TestSessionIdentitySlug_SSHHTTPSNoGitParseSame: the same repo reached via ssh, https,
// and a no-.git URL all resolve to the identical origin slug.
func TestSessionIdentitySlug_SSHHTTPSNoGitParseSame(t *testing.T) {
	requireGit(t)

	for _, url := range []string{
		"git@github.com:acmecorp/product-web.git",
		"https://github.com/acmecorp/product-web.git",
		"https://github.com/acmecorp/product-web",
	} {
		sandboxHome(t)
		repo := filepath.Join(t.TempDir(), "checkout")
		initRepoWithOrigin(t, repo, url)
		slug, source := sessionIdentitySlug(repo)
		if slug != "product-web" {
			t.Errorf("origin %q -> slug %q, want product-web", url, slug)
		}
		if source != "git origin repo-name" {
			t.Errorf("origin %q -> source %q, want git-origin", url, source)
		}
	}
}

// TestSessionIdentitySlug_NoOriginFallsBackToBasename: a local-only repo (no origin
// remote) resolves to its git-toplevel basename, exactly as before this change.
func TestSessionIdentitySlug_NoOriginFallsBackToBasename(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	repo := filepath.Join(t.TempDir(), "local-only-repo")
	initRepoWithOrigin(t, repo, "") // no origin
	slug, source := sessionIdentitySlug(repo)
	if slug != "local-only-repo" {
		t.Fatalf("no-origin repo slug = %q, want local-only-repo (git-toplevel basename)", slug)
	}
	if source != "git-repo basename" {
		t.Errorf("source = %q, want git-repo basename", source)
	}
}

// TestSessionIdentitySlug_NonOriginRemoteNotUsed: a repo with a NON-origin remote
// (upstream) but NO origin must fall back to the basename — never guess the non-origin
// remote for identity.
func TestSessionIdentitySlug_NonOriginRemoteNotUsed(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	repo := filepath.Join(t.TempDir(), "myrepo")
	initRepoWithOrigin(t, repo, "") // no origin
	gitRun(t, repo, "remote", "add", "upstream", "git@github.com:acmecorp/some-other-name.git")

	slug, source := sessionIdentitySlug(repo)
	if slug != "myrepo" {
		t.Fatalf("slug = %q, want myrepo — a non-origin remote must NOT be used for identity", slug)
	}
	if source != "git-repo basename" {
		t.Errorf("source = %q, want git-repo basename", source)
	}
}

// TestSessionIdentitySlug_MalformedOriginFallsBackToBasename: an origin URL whose last
// path segment strips to empty (unparseable) falls back to the basename — never crashes.
func TestSessionIdentitySlug_MalformedOriginFallsBackToBasename(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	repo := filepath.Join(t.TempDir(), "weird-origin-repo")
	// last segment ".git" strips to "" -> OriginRepoName returns "" -> basename fallback.
	initRepoWithOrigin(t, repo, "https://example.com/owner/.git")
	slug, source := sessionIdentitySlug(repo)
	if slug != "weird-origin-repo" {
		t.Fatalf("malformed-origin slug = %q, want weird-origin-repo (basename fallback)", slug)
	}
	if source != "git-repo basename" {
		t.Errorf("source = %q, want git-repo basename", source)
	}
}

// TestSessionIdentitySlug_NonGitCwdUsesCwdBasename: a non-git cwd resolves to its cwd
// basename with source "cwd basename" (real git, no repo).
func TestSessionIdentitySlug_NonGitCwdUsesCwdBasename(t *testing.T) {
	requireGit(t)
	sandboxHome(t)

	cwd := mkdir(t, filepath.Join(t.TempDir(), "loose-dir"))
	slug, source := sessionIdentitySlug(cwd)
	if slug != "loose-dir" {
		t.Fatalf("non-git cwd slug = %q, want loose-dir", slug)
	}
	if source != "cwd basename" {
		t.Errorf("source = %q, want cwd basename", source)
	}
}

// TestSessionIdentitySlug_ProjectIDOverridesOrigin: an external <origin-slug>.yaml with a
// project_id overrides the origin-derived slug (the origin name is only the KEY; project_id
// is the final label). Also proves the external config KEY is the origin name, not the
// clone-dir basename.
func TestSessionIdentitySlug_ProjectIDOverridesOrigin(t *testing.T) {
	requireGit(t)
	home := sandboxHome(t)
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	// Keyed by the ORIGIN name (product-web), even though the clone dir is product-web-2.
	if err := os.WriteFile(filepath.Join(cfgDir, "product-web.yaml"), []byte("project_id: custom-label\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(t.TempDir(), "product-web-2")
	initRepoWithOrigin(t, repo, "git@github.com:acmecorp/product-web.git")

	slug, source := sessionIdentitySlug(repo)
	if slug != "custom-label" {
		t.Fatalf("project_id must override origin name; got %q (source %q)", slug, source)
	}
	if source != "external <slug>.yaml project_id" {
		t.Errorf("source = %q, want external-project_id", source)
	}
}

// TestExternalConfigResolvesFromMainAndWorktree: an external config keyed by the ORIGIN
// name resolves from BOTH the main clone AND a worktree of the same repo — the origin-keying
// win (a basename key would only match the main clone dir, never the worktree).
func TestExternalConfigResolvesFromMainAndWorktree(t *testing.T) {
	requireGit(t)
	home := sandboxHome(t)
	cfgDir := mkdir(t, filepath.Join(home, ".claude", "leadkit", "config"))
	if err := os.WriteFile(filepath.Join(cfgDir, "product-web.yaml"), []byte("project_id: from-external\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(t.TempDir(), "product-web-2") // clone dir name != origin
	initRepoWithOrigin(t, repo, "git@github.com:acmecorp/product-web.git")
	wt := addWorktree(t, repo, filepath.Join(repo, ".claude", "worktrees", "helm-chart-cache"), "helm-chart-cache")

	for _, dir := range []struct{ name, cwd string }{{"main clone", repo}, {"worktree", wt}} {
		cand, _, ok := config.ExternalConfigPathForDir(dir.cwd)
		if !ok {
			t.Fatalf("%s: external config must resolve via origin-name key, got ok=false", dir.name)
		}
		if filepath.Base(cand) != "product-web.yaml" {
			t.Errorf("%s: external config path = %q, want product-web.yaml", dir.name, cand)
		}
		slug, source := sessionIdentitySlug(dir.cwd)
		if slug != "from-external" {
			t.Errorf("%s: identity slug = %q, want from-external (external project_id)", dir.name, slug)
		}
		if source != "external <slug>.yaml project_id" {
			t.Errorf("%s: source = %q, want external-project_id", dir.name, source)
		}
	}
}

// TestCLI_Whoami_OriginSourceLabel: `leadkit whoami --json` in a repo whose identity comes
// from the origin reports IdentitySource "git origin repo-name" and the origin slug.
func TestCLI_Whoami_OriginSourceLabel(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	home := t.TempDir()

	repo := filepath.Join(t.TempDir(), "api") // dir basename differs from origin
	initRepoWithOrigin(t, repo, "git@github.com:acmecorp/api-gateway.git")

	cmd := exec.Command(bin, "whoami", "--json")
	cmd.Dir = repo
	cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE="})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out)
	}
	var info whoamiInfo
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &info); uerr != nil {
		t.Fatalf("bad json: %v\n%s", uerr, out)
	}
	if info.IdentitySlug != "api-gateway" {
		t.Errorf("identity_slug = %q, want api-gateway (origin)\n%s", info.IdentitySlug, out)
	}
	if info.IdentitySource != "git origin repo-name" {
		t.Errorf("identity_source = %q, want git-origin\n%s", info.IdentitySource, out)
	}
}
