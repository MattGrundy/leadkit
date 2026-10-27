package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// externalTestHome sets HOME to a fresh temp dir, isolates the user layer (XDG +
// legacy), clears $LEADKIT_PROFILE, and stubs gitToplevelDir to "not a git repo" so
// repoIdentityRoot(startDir) == startDir — deterministic slugs without a git binary.
// Returns (home, externalConfigDirPath).
func externalTestHome(t *testing.T) (home, cfgDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "noxdg")) // isolate the user layer
	t.Setenv("LEADKIT_PROFILE", "")
	orig := gitToplevelDir
	gitToplevelDir = func(string) (string, bool) { return "", false }
	t.Cleanup(func() { gitToplevelDir = orig })
	cfgDir = filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, cfgDir
}

// writeExternalConfig writes ~/.claude/leadkit/config/<slug>.yaml.
func writeExternalConfig(t *testing.T, cfgDir, slug, content string) string {
	t.Helper()
	path := filepath.Join(cfgDir, slug+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// mkRepo creates a repo dir with a known basename (the slug) under a fresh temp root.
func mkRepo(t *testing.T, slug string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), slug)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestExternalResolution_Order is the full resolution-order proof: in-repo WINS over an
// external config (back-compat), removing the in-repo one falls to external, removing
// both falls to Defaults(), and $LEADKIT_PROFILE wins over everything.
func TestExternalResolution_Order(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	repo := mkRepo(t, "web-client")
	chdir(t, repo)

	extPath := writeExternalConfig(t, cfgDir, "web-client", "queue_root: ext-queue\nproject_id: ext-id\n")
	inRepoPath := filepath.Join(repo, DefaultProfileName)

	// (a) in-repo present alongside external → the IN-REPO one wins (back-compat).
	if err := os.WriteFile(inRepoPath, []byte("queue_root: repo-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.QueueRoot != "repo-queue" {
		t.Errorf("in-repo must win: QueueRoot = %q, want repo-queue", prof.QueueRoot)
	}
	if kind, _, _ := ConfigSource(prof, path); kind != "in-repo" {
		t.Errorf("source kind = %q, want in-repo", kind)
	}
	if prof.externalRepoRoot != "" {
		t.Errorf("an in-repo profile must not carry an externalRepoRoot, got %q", prof.externalRepoRoot)
	}

	// (b) remove the in-repo config → the EXTERNAL one loads.
	if err := os.Remove(inRepoPath); err != nil {
		t.Fatal(err)
	}
	prof, path, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.QueueRoot != "ext-queue" {
		t.Errorf("external fallback: QueueRoot = %q, want ext-queue", prof.QueueRoot)
	}
	if path != extPath {
		t.Errorf("path = %q, want the external config %q", path, extPath)
	}
	if kind, loc, _ := ConfigSource(prof, path); kind != "external" || loc != extPath {
		t.Errorf("source = (%q,%q), want (external,%q)", kind, loc, extPath)
	}

	// (c) remove the external config too → Defaults().
	if err := os.Remove(extPath); err != nil {
		t.Fatal(err)
	}
	prof, path, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty (defaults)", path)
	}
	if prof.QueueRoot != Defaults().QueueRoot {
		t.Errorf("QueueRoot = %q, want the Defaults() value %q", prof.QueueRoot, Defaults().QueueRoot)
	}

	// (d) $LEADKIT_PROFILE wins over an in-repo AND an external config.
	writeExternalConfig(t, cfgDir, "web-client", "queue_root: ext-queue\n")
	if err := os.WriteFile(inRepoPath, []byte("queue_root: repo-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envDir := t.TempDir()
	envPath := filepath.Join(envDir, "explicit.yaml")
	if err := os.WriteFile(envPath, []byte("queue_root: env-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", envPath)
	prof, path, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.QueueRoot != "env-queue" || path != envPath {
		t.Errorf("$LEADKIT_PROFILE must win: QueueRoot=%q path=%q, want env-queue / %q", prof.QueueRoot, path, envPath)
	}
}

// TestExternalResolution_KeyedBySlugNoLeak: the external config is keyed by the repo's
// cwd-honest slug ONLY — a DIFFERENT slug's external config never leaks into an
// unrelated repo.
func TestExternalResolution_KeyedBySlugNoLeak(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	repo := mkRepo(t, "web-client")
	chdir(t, repo)

	// Only a DIFFERENT slug's external config exists → this repo gets Defaults(), no leak.
	writeExternalConfig(t, cfgDir, "some-other-project", "queue_root: other-queue\n")
	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != "" || prof.QueueRoot == "other-queue" {
		t.Fatalf("a different slug's external config leaked: path=%q QueueRoot=%q", path, prof.QueueRoot)
	}

	// Now add THIS repo's own external config → it governs.
	own := writeExternalConfig(t, cfgDir, "web-client", "queue_root: mine-queue\n")
	prof, path, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.QueueRoot != "mine-queue" || path != own {
		t.Errorf("own external config must govern: QueueRoot=%q path=%q, want mine-queue / %q", prof.QueueRoot, path, own)
	}
}

// TestExternalResolution_ProjectRootIsRepoAndRelativeAnchorsToConfigDir is the
// load-bearing detail: an external config's ProjectRoot() is the REAL repo (so gate
// own-origin + identity derive from it), while its RELATIVE paths anchor to the external
// config's OWN dir (keeping queue/store out of the repo); an ABSOLUTE path is honored.
func TestExternalResolution_ProjectRootIsRepoAndRelativeAnchorsToConfigDir(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	repo := mkRepo(t, "web-client")

	extPath := writeExternalConfig(t, cfgDir, "web-client",
		"queue_root: ./queue\nrecords_dir: /abs/records\n")

	prof, found, err := LoadFrom(repo)
	if err != nil || !found || prof == nil {
		t.Fatalf("LoadFrom external: found=%v err=%v prof=%v", found, err, prof)
	}

	// ProjectRoot is the REAL repo, NOT the external config file's dir.
	if prof.ProjectRoot() != repo {
		t.Errorf("ProjectRoot() = %q, want the real repo %q", prof.ProjectRoot(), repo)
	}
	if prof.ProjectRoot() == filepath.Dir(extPath) {
		t.Error("ProjectRoot() must NOT be the external config dir")
	}

	// A RELATIVE queue_root anchors to the external config's OWN dir (out of the repo).
	wantQueue := filepath.Join(cfgDir, "queue")
	if got := prof.Resolve(prof.QueueRoot); got != wantQueue {
		t.Errorf("Resolve(queue_root) = %q, want it anchored to the config dir %q", got, wantQueue)
	}
	if strings.HasPrefix(prof.Resolve(prof.QueueRoot), repo) {
		t.Error("a relative external queue_root must NOT land inside the repo")
	}

	// An ABSOLUTE path is honored verbatim.
	if got := prof.Resolve(prof.RecordsDir); got != "/abs/records" {
		t.Errorf("Resolve(records_dir abs) = %q, want /abs/records", got)
	}
}

// TestExternalResolution_LoadFromFallsBackToDefaultsWithNoExternal: LoadFrom still
// reports found=false (→ caller's Defaults()) when neither an in-repo nor an external
// config exists — the additive step never fabricates a profile.
func TestExternalResolution_LoadFromFallsBackToDefaultsWithNoExternal(t *testing.T) {
	externalTestHome(t)
	repo := mkRepo(t, "unconfigured-repo")
	_, found, err := LoadFrom(repo)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if found {
		t.Error("LoadFrom must report found=false when no in-repo and no external config exist")
	}
}

// TestConfigSource_ClassifiesAllThree pins the source classifier used by whoami /
// config show: defaults (no path), in-repo (a path, no externalRepoRoot), external (a
// path + externalRepoRoot = the governed repo).
func TestConfigSource_ClassifiesAllThree(t *testing.T) {
	if kind, _, _ := ConfigSource(&Profile{}, ""); kind != "defaults" {
		t.Errorf("no path → kind %q, want defaults", kind)
	}
	if kind, loc, _ := ConfigSource(&Profile{root: "/r"}, "/r/leadkit.yaml"); kind != "in-repo" || loc != "/r/leadkit.yaml" {
		t.Errorf("in-repo classify = (%q,%q)", kind, loc)
	}
	ext := &Profile{root: "/home/.claude/leadkit/config", externalRepoRoot: "/repo"}
	kind, loc, repoRoot := ConfigSource(ext, "/home/.claude/leadkit/config/slug.yaml")
	if kind != "external" || repoRoot != "/repo" || loc == "" {
		t.Errorf("external classify = (%q,%q,%q)", kind, loc, repoRoot)
	}
}

// TestFormatShow_NamesExternalSource: config show's header names an external config AND
// the repo it governs; an in-repo config keeps the bare `# <path>` (back-compat).
func TestFormatShow_NamesExternalSource(t *testing.T) {
	ext := &Profile{root: "/cfg", externalRepoRoot: "/real/repo"}
	out, err := FormatShow(ext, "/cfg/web-client.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "external config: /cfg/web-client.yaml") || !strings.Contains(out, "/real/repo") {
		t.Errorf("external header missing config path or repo:\n%s", out)
	}

	in, err := FormatShow(&Profile{root: "/r"}, "/r/leadkit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in, "# /r/leadkit.yaml\n") {
		t.Errorf("in-repo header changed (back-compat break):\n%s", in)
	}
}

// TestStarterConfig_MirrorsTemplateAndNotesAnchoring: the init starter carries the
// identity + own-queue + gate-scope + roster stub, and the EXTERNAL variant documents
// that relative paths anchor to the config dir.
func TestStarterConfig_MirrorsTemplateAndNotesAnchoring(t *testing.T) {
	ext := StarterConfig("web-client", true)
	for _, want := range []string{"project_id: web-client", "queue_root: ./queue", "enforced_remotes:", "push_whitelist:", "roster:"} {
		if !strings.Contains(ext, want) {
			t.Errorf("external starter missing %q:\n%s", want, ext)
		}
	}
	if !strings.Contains(ext, "EXTERNAL") || !strings.Contains(ext, "config file's own dir") {
		t.Errorf("external starter must note relative-path anchoring:\n%s", ext)
	}
	inRepo := StarterConfig("web-client", false)
	if strings.Contains(inRepo, "EXTERNAL") {
		t.Errorf("in-repo starter must not carry the external anchoring note:\n%s", inRepo)
	}
}

// gitInitRepo makes a real git repo at dir.
func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

// useRealGitToplevel replaces externalTestHome's "not a git repo" stub with real git.
func useRealGitToplevel(t *testing.T) {
	t.Helper()
	orig := gitToplevelDir
	gitToplevelDir = realGitToplevelDir
	t.Cleanup(func() { gitToplevelDir = orig })
}

// writeProfile writes a leadkit.yaml declaring projectID into dir.
func writeProfile(t *testing.T, dir, projectID string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, DefaultProfileName)
	if err := os.WriteFile(path, []byte("project_id: "+projectID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscover_AncestorAboveGitToplevelIsIgnored(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	useRealGitToplevel(t)

	parent := t.TempDir()
	stray := writeProfile(t, parent, "stray-ancestor")
	repo := filepath.Join(parent, "guarded-repo")
	gitInitRepo(t, repo)
	cwd := filepath.Join(repo, "cmd", "deep")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	extPath := writeExternalConfig(t, cfgDir, "guarded-repo", "project_id: external-wins\n")
	chdir(t, cwd)

	if got, found := discover(cwd); found {
		t.Fatalf("discover crossed the git toplevel and found %q", got)
	}
	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if samePath(t, path, stray) {
		t.Fatalf("Load resolved the ancestor config %q above the git toplevel", path)
	}
	if !samePath(t, path, extPath) {
		t.Errorf("Load path = %q, want the external config %q", path, extPath)
	}
	if prof.ProjectID != "external-wins" {
		t.Errorf("ProjectID = %q, want external-wins", prof.ProjectID)
	}
	lp, found, err := LoadFrom(cwd)
	if err != nil || !found || lp.ProjectID != "external-wins" {
		t.Errorf("LoadFrom = (%+v, %v, %v), want the external config", lp, found, err)
	}
}

func TestDiscover_InRepoSubdirectoryStillFound(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	useRealGitToplevel(t)

	repo := filepath.Join(t.TempDir(), "guarded-repo")
	gitInitRepo(t, repo)
	sub := filepath.Join(repo, "services", "api")
	cwd := filepath.Join(sub, "internal", "deep")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inRepo := writeProfile(t, sub, "in-repo-subdir")
	writeExternalConfig(t, cfgDir, "guarded-repo", "project_id: external-loses\n")
	chdir(t, cwd)

	got, found := discover(cwd)
	if !found || !samePath(t, got, inRepo) {
		t.Fatalf("discover = (%q, %v), want the in-repo subdirectory config %q", got, found, inRepo)
	}
	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !samePath(t, path, inRepo) || prof.ProjectID != "in-repo-subdir" {
		t.Errorf("Load = (%q, %q), want the in-repo subdirectory config", path, prof.ProjectID)
	}
}

func TestDiscover_ConfigAtGitToplevelItselfIsFound(t *testing.T) {
	externalTestHome(t)

	parent := t.TempDir()
	writeProfile(t, parent, "stray-ancestor")
	repo := filepath.Join(parent, "guarded-repo")
	gitInitRepo(t, repo)
	atTop := writeProfile(t, repo, "at-toplevel")
	deep := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{repo, deep} {
		got, found := discover(start)
		if !found || !samePath(t, got, atTop) {
			t.Errorf("discover(%q) = (%q, %v), want the toplevel config %q", start, got, found, atTop)
		}
	}
}

func TestDiscover_OutsideAnyRepoWalkIsUnbounded(t *testing.T) {
	externalTestHome(t)

	parent := t.TempDir()
	if _, inRepo := repoRootOf(parent); inRepo {
		t.Skip("temp dir sits inside a git repo")
	}
	ancestor := writeProfile(t, parent, "ancestor")
	cwd := filepath.Join(parent, "x", "y")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, found := discover(cwd); !found || got != filepath.Join(parent, DefaultProfileName) {
		t.Errorf("discover = (%q, %v), want %q", got, found, ancestor)
	}
}

// TestDiscover_BoundaryHoldsWhenGitCannotAnswer makes git fail in each known way and
// requires an ancestor config above the toplevel to stay ignored, with a config at the
// toplevel still found.
func TestDiscover_BoundaryHoldsWhenGitCannotAnswer(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the start dir inside the checkout and the dir holding the checkout's
		// own leadkit.yaml (empty when the layout has no working tree).
		setup func(t *testing.T, repo string) (start, top string)
		// breakGit puts the environment in a state where git refuses to answer.
		breakGit func(t *testing.T)
	}{
		{
			name:     "git missing from PATH",
			setup:    func(t *testing.T, repo string) (string, string) { return filepath.Join(repo, "sub"), repo },
			breakGit: func(t *testing.T) { t.Setenv("PATH", "") },
		},
		{
			name:  "dubious ownership",
			setup: func(t *testing.T, repo string) (string, string) { return filepath.Join(repo, "sub"), repo },
			breakGit: func(t *testing.T) {
				t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
				t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
			},
		},
		{
			name:     "cwd inside .git",
			setup:    func(t *testing.T, repo string) (string, string) { return filepath.Join(repo, ".git", "hooks"), repo },
			breakGit: func(t *testing.T) {},
		},
		{
			name: "bare repository",
			setup: func(t *testing.T, repo string) (string, string) {
				bare := filepath.Join(filepath.Dir(repo), "bare.git")
				if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
					t.Fatalf("git init --bare: %v\n%s", err, out)
				}
				return filepath.Join(bare, "hooks"), ""
			},
			breakGit: func(t *testing.T) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cfgDir := externalTestHome(t)
			useRealGitToplevel(t)

			parent := t.TempDir()
			stray := writeProfile(t, parent, "stray-ancestor")
			repo := filepath.Join(parent, "guarded-repo")
			gitInitRepo(t, repo)
			if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			start, top := tc.setup(t, repo)
			writeExternalConfig(t, cfgDir, "guarded-repo", "project_id: external-wins\n")

			tc.breakGit(t)
			if got, ok := realGitToplevelDir(start); ok {
				if tc.name == "dubious ownership" {
					t.Skipf("this git does not honour GIT_TEST_ASSUME_DIFFERENT_OWNER (answered %q)", got)
				}
				t.Fatalf("precondition: git answered %q, expected it to fail", got)
			}

			if got, found := discover(start); found {
				t.Fatalf("discover from %q crossed the checkout boundary and found %q (stray %q)", start, got, stray)
			}
			if top != "" {
				atTop := writeProfile(t, top, "at-toplevel")
				if got, found := discover(start); !found || !samePath(t, got, atTop) {
					t.Errorf("discover from %q = (%q, %v), want the toplevel config %q", start, got, found, atTop)
				}
			}
		})
	}
}

// TestDiscover_LinkedWorktreeDotGitFileIsTheBoundary uses a real linked worktree, whose
// .git is a file, with git unable to run: the boundary must still be the worktree root.
func TestDiscover_LinkedWorktreeDotGitFileIsTheBoundary(t *testing.T) {
	externalTestHome(t)
	useRealGitToplevel(t)

	parent := t.TempDir()
	stray := writeProfile(t, parent, "stray-ancestor")
	mainRepo := filepath.Join(parent, "main")
	gitInitRepo(t, mainRepo)
	if out, err := exec.Command("git", "-C", mainRepo, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"commit", "-q", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	wt := filepath.Join(parent, "wt")
	if out, err := exec.Command("git", "-C", mainRepo, "worktree", "add", "-q", "--detach", wt).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("precondition: a linked worktree's .git should be a file, got %v (%v)", fi, err)
	}
	deep := filepath.Join(wt, "pkg", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	if top, ok := repoRootOf(deep); !ok || !samePath(t, top, wt) {
		t.Errorf("repoRootOf(%q) = (%q, %v), want the worktree root %q", deep, top, ok, wt)
	}
	if got, found := discover(deep); found {
		t.Fatalf("discover from %q crossed the worktree boundary and found %q (stray %q)", deep, got, stray)
	}
	atTop := writeProfile(t, wt, "at-toplevel")
	if got, found := discover(deep); !found || !samePath(t, got, atTop) {
		t.Errorf("discover(%q) = (%q, %v), want the worktree's own config %q", deep, got, found, atTop)
	}
}

// TestIsRepoRoot_BareLayoutNeedsHeadObjectsAndRefs pins the bare-repo heuristic: a
// directory that merely has a HEAD file, or lacks refs/, is not a repository.
func TestIsRepoRoot_BareLayoutNeedsHeadObjectsAndRefs(t *testing.T) {
	mk := func(t *testing.T, files []string, dirs []string) string {
		t.Helper()
		d := t.TempDir()
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(d, f), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, sub := range dirs {
			if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	cases := []struct {
		name  string
		files []string
		dirs  []string
		want  bool
	}{
		{"HEAD only", []string{"HEAD"}, nil, false},
		{"HEAD and objects", []string{"HEAD"}, []string{"objects"}, false},
		{"HEAD and refs", []string{"HEAD"}, []string{"refs"}, false},
		{"objects and refs without HEAD", nil, []string{"objects", "refs"}, false},
		{"HEAD as a directory", nil, []string{"HEAD", "objects", "refs"}, false},
		{"full bare layout", []string{"HEAD"}, []string{"objects", "refs"}, true},
	}
	for _, tc := range cases {
		if got := isRepoRoot(mk(t, tc.files, tc.dirs)); got != tc.want {
			t.Errorf("%s: isRepoRoot = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDiscover_SymlinkedStartIsWalkedByItsRealParents builds a symlink that sits under a
// directory holding a stray config but points into a checkout elsewhere. The walk must
// follow the real parents of the target, which is also what makes the boundary match on
// platforms whose temp dir is not itself a symlink.
func TestDiscover_SymlinkedStartIsWalkedByItsRealParents(t *testing.T) {
	externalTestHome(t)

	base := t.TempDir()
	stray := writeProfile(t, base, "stray-ancestor")
	repo := filepath.Join(base, "checkouts", "repo")
	gitInitRepo(t, repo)
	deep := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	links := filepath.Join(base, "links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(links, "lnk")
	if err := os.Symlink(deep, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	atTop := writeProfile(t, repo, "at-toplevel")

	if got, found := discover(link); !found || !samePath(t, got, atTop) {
		t.Errorf("discover(%q) = (%q, %v), want the checkout's own config %q, not the stray %q", link, got, found, atTop, stray)
	}
	if err := os.Remove(atTop); err != nil {
		t.Fatal(err)
	}
	if got, found := discover(link); found {
		t.Errorf("discover(%q) = %q, want none: the stray %q is not above the checkout", link, got, stray)
	}
	if top, ok := repoRootOf(link); !ok || !samePath(t, top, repo) {
		t.Errorf("repoRootOf(%q) = (%q, %v), want %q", link, top, ok, repo)
	}
}

func TestDiscover_DanglingSymlinkStartDoesNotWalkUnbounded(t *testing.T) {
	externalTestHome(t)

	base := t.TempDir()
	writeProfile(t, base, "stray-ancestor")
	link := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, found := discover(link); found {
		t.Errorf("discover(%q) = %q, want none: its real parent cannot be resolved", link, got)
	}
}

func TestRepoIdentityRoot_ResolvesToplevelWhenGitCannotAnswer(t *testing.T) {
	_, cfgDir := externalTestHome(t)
	useRealGitToplevel(t)

	repo := filepath.Join(t.TempDir(), "guarded-repo")
	gitInitRepo(t, repo)
	deep := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	if got := repoIdentityRoot(deep); !samePath(t, got, repo) {
		t.Errorf("repoIdentityRoot(%q) = %q, want the toplevel %q", deep, got, repo)
	}
	path, root, slug := ExternalConfigTargetForDir(deep)
	if slug != "guarded-repo" || !samePath(t, root, repo) || path != filepath.Join(cfgDir, "guarded-repo.yaml") {
		t.Errorf("ExternalConfigTargetForDir = (%q, %q, %q), want slug guarded-repo rooted at %q", path, root, slug, repo)
	}
}

// samePath compares two paths after symlink resolution.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	return canonicalDir(a) == canonicalDir(b)
}
