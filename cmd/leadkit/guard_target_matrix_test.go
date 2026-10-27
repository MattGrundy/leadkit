package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// A guard must resolve policy from what the command targets, not from where the session
// happens to be. Fixing one call site does not protect the others, so this tests the SHAPE
// of the class rather than any single site.
//
// It stands up two projects with deliberately CONFLICTING policy on every dimension a
// guard reads, then asserts that every way of naming the target resolves to exactly one
// of THREE outcomes:
//
//   - the TARGET's policy, when the path resolves to the target;
//   - the SESSION's policy, when the path genuinely names something inside the session;
//   - the BUILT-IN default, when the path resolves to nothing.
//
// The third outcome is the one the bug class lives in. A resolver that falls through to
// the session on an unresolvable path passes any matrix whose rows all name real
// directories, so the non-resolving rows are the controls that matter.

type matrixProject struct {
	dir  string
	name string
}

// builtin marks a row whose path resolves to nothing, so the BUILT-IN default must govern:
// neither project's policy.
const builtin = "<built-in default>"

// writeMatrixProject creates a project whose every guard-visible policy value is
// distinctive, so a wrong resolution is visible rather than plausible. queue_root and
// store_root differ per project because the ANCHOR they resolve against is the question.
func writeMatrixProject(t *testing.T, parent, name string) matrixProject {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "" +
		"project_id: " + name + "\n" +
		"queue_root: ./" + name + "-queue\n" +
		"store_root: ./" + name + "-tasks\n" +
		"push_whitelist: [" + name + "/*]\n" +
		"enforced_remotes: [" + name + "-remote]\n" +
		"canonical_statuses: [pending, in_review, " + name + "-only-status]\n"
	if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return matrixProject{dir: dir, name: name}
}

// matrixWorld is the two-project fixture plus the base a hook resolves against.
type matrixWorld struct {
	home    string
	session matrixProject
	target  matrixProject
	base    hookResolveBase
	orphan  string // a symlink INSIDE the session project to a real, profile-less repo elsewhere
}

// newMatrixWorld builds the fixture and pins the two environmental inputs the production
// hook gets from its minimal environment rather than from the test:
//
//   - HOME, so config's always-consulted user layer cannot leak the developer's real
//     ~/.claude into a row;
//   - gitToplevel, because in production every push target IS a git repo. Without the stub
//     the rows would depend on whether TMPDIR happens to sit inside a repo, which would
//     re-root the walk-up and fail every row on some machines.
func newMatrixWorld(t *testing.T) matrixWorld {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LEADKIT_PROFILE", "")
	// The rows that must resolve to NOTHING walk up lexically from under TMPDIR, so an
	// ancestor leadkit.yaml (TMPDIR inside a profiled repo) would govern them. gitToplevel is
	// stubbed below, but that walk-up is plain path arithmetic and cannot be. Say so plainly
	// rather than failing every such row with a profile that looks like a resolver bug.
	for d := filepath.Dir(home); d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "leadkit.yaml")); err == nil {
			t.Fatalf("%s/leadkit.yaml sits above TMPDIR (%s) and would govern every row that "+
				"should resolve to the built-in default; run these tests with TMPDIR outside any leadkit project", d, home)
		}
	}
	w := matrixWorld{home: home}
	w.session = writeMatrixProject(t, home, "sessionproj")
	w.target = writeMatrixProject(t, home, "targetproj")
	w.base = hookResolveBase{cwd: w.session.dir, home: home}

	// A clone with no leadkit.yaml of its own, reached through a symlink that sits INSIDE
	// the session project's tree. Walking up from the LINK path reaches the session's
	// profile; walking up from the clone's real path reaches nothing. git reports real
	// paths, which is exactly what the toplevel re-root exists to exploit.
	realOrphan := filepath.Join(home, "orphan-repo")
	if err := os.MkdirAll(realOrphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.session.dir, "clones"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.orphan = filepath.Join(w.session.dir, "clones", "orphan-link")
	if err := os.Symlink(realOrphan, w.orphan); err != nil {
		t.Fatal(err)
	}
	stubMatrixGit(t, w.session.dir, w.target.dir, realOrphan)
	return w
}

// stubMatrixGit makes each of repos a git repository root: gitToplevel reports the MOST
// SPECIFIC repo containing dir's REAL path (as `git rev-parse --show-toplevel` does, so a
// symlink into a repo is that repo), and nothing otherwise. The repo is returned as spelled
// in repos, so a row can still assert the project root exactly.
func stubMatrixGit(t *testing.T, repos ...string) {
	t.Helper()
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	orig := gitToplevel
	gitToplevel = func(dir string) (string, bool) {
		dir = real(dir)
		best, bestLen := "", 0
		for _, r := range repos {
			rr := real(r)
			if (dir == rr || strings.HasPrefix(dir, rr+string(os.PathSeparator))) && len(rr) > bestLen {
				best, bestLen = r, len(rr)
			}
		}
		return best, best != ""
	}
	t.Cleanup(func() { gitToplevel = orig })
}

// assertGoverned checks EVERY guard-visible dimension of prof against the expected
// governing project (or the built-in default). A resolver that got one dimension right and
// another wrong is the exact defect this guards, so nothing here short-circuits.
func (w matrixWorld) assertGoverned(t *testing.T, prof *config.Profile, want string) {
	t.Helper()
	if prof == nil {
		t.Fatal("resolved no profile at all")
	}
	if want == builtin {
		for _, p := range []matrixProject{w.session, w.target} {
			if prof.ProjectID == p.name || firstOr(prof.PushWhitelist) == p.name+"/*" ||
				firstOr(prof.EnforcedRemotes) == p.name+"-remote" || hasStatus(prof, p.name+"-only-status") {
				t.Errorf("a path resolving to nothing must get the built-in default, but %s's policy governed it (project_id=%q push_whitelist=%v)", p.name, prof.ProjectID, prof.PushWhitelist)
			}
		}
		if prof.ProjectRoot() != "" {
			t.Errorf("project root = %q, want none for the built-in default", prof.ProjectRoot())
		}
		if !reflect.DeepEqual(prof.PushWhitelist, config.DefaultPushWhitelist) {
			t.Errorf("push_whitelist = %v, want the built-in %v", prof.PushWhitelist, config.DefaultPushWhitelist)
		}
		return
	}
	p := w.session
	other := w.target
	if want == w.target.name {
		p, other = w.target, w.session
	}
	checkDimension(t, "project_id", prof.ProjectID, p.name)
	checkDimension(t, "push_whitelist", firstOr(prof.PushWhitelist), p.name+"/*")
	checkDimension(t, "enforced_remotes", firstOr(prof.EnforcedRemotes), p.name+"-remote")
	// The anchor is the whole question for the next three: Base() of a resolved path would
	// discard it, and ProjectRoot() is what resolveTargetGate actually consumes.
	checkDimension(t, "project root", prof.ProjectRoot(), p.dir)
	checkDimension(t, "store_root", prof.Resolve(prof.StoreRoot), filepath.Join(p.dir, p.name+"-tasks"))
	checkDimension(t, "queue_root", prof.Resolve(prof.QueueRoot), filepath.Join(p.dir, p.name+"-queue"))
	if !hasStatus(prof, p.name+"-only-status") || hasStatus(prof, other.name+"-only-status") {
		t.Errorf("canonical_statuses = %v, want %s's vocabulary and not %s's", prof.CanonicalStatuses, p.name, other.name)
	}
}

func hasStatus(prof *config.Profile, s string) bool {
	for _, c := range prof.CanonicalStatuses {
		if c == s {
			return true
		}
	}
	return false
}

// The two fixtures must actually conflict on every dimension asserted below, or a wrong
// resolution could be invisible. Checked against the LOADED profiles rather than against
// the project names, which differ by construction and would prove nothing.
func TestGuardTargetMatrix_FixturesConflictOnEveryDimension(t *testing.T) {
	w := newMatrixWorld(t)
	s, _, err := config.LoadFrom(w.session.dir)
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := config.LoadFrom(w.target.dir)
	if err != nil {
		t.Fatal(err)
	}
	for dim, differ := range map[string]bool{
		"project_id":         s.ProjectID != g.ProjectID,
		"push_whitelist":     !reflect.DeepEqual(s.PushWhitelist, g.PushWhitelist),
		"enforced_remotes":   !reflect.DeepEqual(s.EnforcedRemotes, g.EnforcedRemotes),
		"project root":       s.ProjectRoot() != g.ProjectRoot(),
		"store_root":         s.Resolve(s.StoreRoot) != g.Resolve(g.StoreRoot),
		"queue_root":         s.Resolve(s.QueueRoot) != g.Resolve(g.QueueRoot),
		"canonical_statuses": !reflect.DeepEqual(s.CanonicalStatuses, g.CanonicalStatuses),
	} {
		if !differ {
			t.Errorf("the two projects do not differ on %s, so a wrong resolution there would pass", dim)
		}
	}
}

// TestGuardTargetMatrix_EveryPathFormResolvesTheTarget is the regression matrix for the
// ambient-profile class: path form x policy dimension.
func TestGuardTargetMatrix_EveryPathFormResolvesTheTarget(t *testing.T) {
	w := newMatrixWorld(t)
	sep := string(os.PathSeparator)

	dirForms := map[string]struct{ raw, want string }{
		"absolute path":            {w.target.dir, w.target.name},
		"tilde path":               {"~/" + w.target.name, w.target.name},
		"relative from session":    {filepath.Join("..", w.target.name), w.target.name},
		"absolute with trailing /": {w.target.dir + sep, w.target.name},
		// No path named at all is the session itself.
		"bare (no path)": {"", w.session.name},

		// NON-RESOLVING: the mechanism the bug class lives in. Each names something that
		// does not exist as a directory, and must fall to the built-in default. Delete the
		// stat-as-directory gate in resolveGoverningStart and every one of these reports the
		// session's policy, because the lexical walk-up finds the session's leadkit.yaml.
		"relative, nonexistent, inside the session":              {"no-such-dir", builtin},
		"relative, nonexistent sibling":                          {filepath.Join("..", "no-such-clone"), builtin},
		"tilde, nonexistent":                                     {"~/no-such-clone", builtin},
		"tilde, nonexistent, under the session":                  {"~/" + w.session.name + "/no-such-dir", builtin},
		"~user that cannot be resolved":                          {"~nosuchuser-xyz/" + w.target.name, builtin},
		"a file, not a directory":                                {filepath.Join(w.target.dir, "leadkit.yaml"), builtin},
		"a profile-less clone symlinked from inside the session": {w.orphan, builtin},
	}
	for form, tc := range dirForms {
		t.Run("dir/"+form, func(t *testing.T) {
			w.assertGoverned(t, governingDirProfile(tc.raw, w.base), tc.want)
		})
	}

	// File targets go through the file-lenient resolver rather than the dir-strict one, so
	// they need their own rows: owner-guard and task-status-guard are handed a FILE.
	// A file may not exist yet (a Write creating it), so its path is walked up LEXICALLY.
	fileForms := map[string]struct{ raw, want string }{
		"absolute file in target":  {filepath.Join(w.target.dir, "tasks", "1.md"), w.target.name},
		"tilde file in target":     {filepath.Join("~/"+w.target.name, "tasks", "1.md"), w.target.name},
		"absolute file in session": {filepath.Join(w.session.dir, "tasks", "1.md"), w.session.name},
		// A relative file path is relative to the PAYLOAD cwd, so it is judged by whichever
		// project that path lands in. `../targetproj/...` reaches the target.
		"relative file reaching the target": {filepath.Join("..", w.target.name, "tasks", "1.md"), w.target.name},
		"relative file in session":          {filepath.Join("tasks", "1.md"), w.session.name},
		// A file under no project at all is governed by no project's policy.
		"tilde file under no project": {"~/no-such-project/tasks/1.md", builtin},
	}
	for form, tc := range fileForms {
		t.Run("file/"+form, func(t *testing.T) {
			w.assertGoverned(t, governingProfileFrom(tc.raw, w.base), tc.want)
		})
	}
}

// A relative FILE path that names the target's directory name but is not anchored at the
// target names a different file: `<session cwd>/targetproj/tasks/1.md`, which lies inside
// the session project, so the session's policy governs it. That is the path resolving as
// written, and owners.relFor — which decides the ownership match — joins a relative path
// onto the same cwd, so resolver and matcher agree. A future "fix" has to argue with this.
func TestGuardTargetMatrix_RelativeFileIsAnchoredOnThePayloadCwd(t *testing.T) {
	w := newMatrixWorld(t)
	raw := filepath.Join(w.target.name, "tasks", "1.md")

	if got, want := normalizeFileTarget(raw, w.base), filepath.Join(w.session.dir, raw); got != want {
		t.Fatalf("a relative file path must be absolutized against the payload cwd: got %q, want %q", got, want)
	}
	w.assertGoverned(t, governingProfileFrom(raw, w.base), w.session.name)

	// The same text with a DIFFERENT payload cwd is a different file, in the target.
	other := hookResolveBase{cwd: w.home, home: w.home}
	w.assertGoverned(t, governingProfileFrom(raw, other), w.target.name)
}

// Production reads HOME from the hook's minimal environment, which may not set it. The
// other matrices inject a home, so on their own they prove something the deployed guard may
// not do. With no home, a `~` cannot be expanded and must never be read as a literal path
// under the session.
func TestGuardTargetMatrix_HomeUnsetInTheHookEnvironment(t *testing.T) {
	w := newMatrixWorld(t)
	t.Setenv("HOME", "")
	base := hookResolveBase{cwd: w.session.dir, home: ""}

	for _, raw := range []string{"~/" + w.target.name, "~"} {
		t.Run("dir/"+raw, func(t *testing.T) {
			w.assertGoverned(t, governingDirProfile(raw, base), builtin)
		})
	}

	// A `~` FILE target with no home is anchored on the payload cwd rather than walked up as
	// a literal `~/...` path (normalizeFileTarget says so on purpose). That is the session's
	// policy, by design; it is pinned so a change to it is a decision rather than an accident.
	t.Run("file/~", func(t *testing.T) {
		raw := filepath.Join("~/"+w.target.name, "tasks", "1.md")
		w.assertGoverned(t, governingProfileFrom(raw, base), w.session.name)
	})
}

// The whitelist resolver gets its own pass through the same forms, asserting the effective branch decision rather than the
// profile behind it: the target's pattern is allowed and the session's is not.
func TestGuardTargetMatrix_WhitelistDecisionFollowsTarget(t *testing.T) {
	w := newMatrixWorld(t)

	forms := map[string]string{
		"absolute path":         w.target.dir,
		"tilde path":            "~/" + w.target.name,
		"relative from session": filepath.Join("..", w.target.name),
	}
	for form, raw := range forms {
		t.Run(form, func(t *testing.T) {
			wl := resolveTargetWhitelistFrom(raw, "", w.base)
			if !hookMatch(wl, w.target.name+"/x") {
				t.Errorf("target's own branch pattern must be allowed, got whitelist %v", wl)
			}
			if hookMatch(wl, w.session.name+"/x") {
				t.Errorf("the SESSION's branch pattern must not leak into a cross-project push, got %v", wl)
			}
		})
	}

	// A path that resolves to nothing gets the built-in default: not the session's list
	// and not the target's.
	for form, raw := range map[string]string{
		"nonexistent, inside the session": "no-such-dir",
		"tilde, nonexistent":              "~/no-such-clone",
		"unresolvable ~user":              "~nosuchuser-xyz/" + w.target.name,
		"symlinked profile-less clone":    w.orphan,
	} {
		t.Run("non-resolving/"+form, func(t *testing.T) {
			wl := resolveTargetWhitelistFrom(raw, "", w.base)
			if !reflect.DeepEqual(wl, config.DefaultPushWhitelist) {
				t.Errorf("whitelist = %v, want the built-in default %v", wl, config.DefaultPushWhitelist)
			}
			if hookMatch(wl, w.session.name+"/x") {
				t.Errorf("the SESSION's whitelist leaked into a target that resolves to nothing: %v", wl)
			}
		})
	}

	// And the control, stated as a decision rather than a profile.
	wl := resolveTargetWhitelistFrom("", "", w.base)
	if !hookMatch(wl, w.session.name+"/x") {
		t.Errorf("a bare push is governed by the session, got whitelist %v", wl)
	}
	if hookMatch(wl, w.target.name+"/x") {
		t.Errorf("a bare push must not inherit the target project's whitelist, got %v", wl)
	}
}

func checkDimension(t *testing.T, dim, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q (policy came from the wrong project)", dim, got, want)
	}
}

func firstOr(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}
