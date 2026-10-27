package main

// guards-resolve-config-from-target: the three PreToolUse guards (push-gate, owner-guard,
// task-status-guard) resolve their governing config from the TARGET of the action (the
// repo/file the tool call operates on), NOT the session-ambient profile — the ambient-profile
// bug class, generalized. These whitebox unit tests exercise the shared governingProfile
// helper and each guard's per-target resolver against t.TempDir profiles at DISTINCT paths
// (so a "session" and a "target" never coincide), proving: resolution follows the target,
// the fallback is the BUILT-IN default (never ambient, never allow-all), a relative path
// re-anchors on the TARGET root, and same-project resolution is byte-identical to the
// old session-baked config.

import (
	"os"
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
)

// sandboxHome redirects HOME to an empty temp dir so config's always-consulted USER layer
// can't leak the real machine's ~/.claude settings into a resolver under test.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// LoadFrom never consults LEADKIT_PROFILE, but a stray one in the env would still be
	// irrelevant here; clear it so nothing keys off the session's ambient scope.
	t.Setenv("LEADKIT_PROFILE", "")
	return home
}

// writeProfileDir writes a leadkit.yaml with body into a fresh dir and returns the dir.
func writeProfileDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- governingProfile ---------------------------------------------------------------

func TestGoverningProfile_ResolvesTargetProfile_ElseBuiltInDefault(t *testing.T) {
	sandboxHome(t)
	// A target project with a DISTINCT, non-default enforced_remotes.
	target := writeProfileDir(t, "queue_root: queue\nstore_root: tasks\nenforced_remotes: [only-target-remote]\n")

	prof := governingProfile(target)
	if len(prof.EnforcedRemotes) != 1 || prof.EnforcedRemotes[0] != "only-target-remote" {
		t.Errorf("governingProfile(target).EnforcedRemotes = %v, want [only-target-remote] (from the TARGET profile)", prof.EnforcedRemotes)
	}
	// records_dir is relative-to-target: prof.Resolve joins against the discovered root.
	if got, want := prof.Resolve("tasks"), filepath.Join(target, "tasks"); got != want {
		t.Errorf("target profile Resolve(tasks) = %q, want %q (anchored on the TARGET root)", got, want)
	}

	// An UNPROFILED path → the BUILT-IN default (config.Defaults()), never the ambient
	// profile: enforced_remotes is EMPTY (the own-origin default), and its ProjectRoot
	// is "" (no file on disk) — so an unprofiled target derives no own origin and is
	// out of scope.
	unprofiled := t.TempDir()
	def := governingProfile(unprofiled)
	if len(def.EnforcedRemotes) != 0 {
		t.Errorf("governingProfile(unprofiled).EnforcedRemotes = %v, want EMPTY (own-origin default)", def.EnforcedRemotes)
	}
	if def.ProjectRoot() != "" {
		t.Errorf("governingProfile(unprofiled).ProjectRoot() = %q, want \"\" (no discovered profile)", def.ProjectRoot())
	}
}

// --- push-gate: resolveTargetGate ---------------------------------------------------

// TestResolveTargetGate_UnprofiledRepo_OutOfScope is the bypass-closed property at the
// wiring layer under the new own-origin default: an unprofiled target repo's gate
// carries EMPTY enforced_remotes AND an empty ProjectRoot, so it derives no own origin
// and is OUT of scope (its own profile, if any, governs it). The records dir is still
// the central default.
func TestResolveTargetGate_UnprofiledRepo_OutOfScope(t *testing.T) {
	sandboxHome(t)
	repo := t.TempDir()
	g := resolveTargetGate(repo)
	if len(g.EnforcedRemotes) != 0 {
		t.Errorf("resolveTargetGate(unprofiled).EnforcedRemotes = %v, want EMPTY (own-origin default)", g.EnforcedRemotes)
	}
	if g.ProjectRoot != "" {
		t.Errorf("resolveTargetGate(unprofiled).ProjectRoot = %q, want \"\" (no discovered profile → out of scope)", g.ProjectRoot)
	}
	if g.RecordsDir != gate.DefaultRecordsDir() {
		t.Errorf("resolveTargetGate(unprofiled).RecordsDir = %q, want the central default %q", g.RecordsDir, gate.DefaultRecordsDir())
	}
}

// TestResolveTargetGate_RelativeRecordsDir_AnchorsOnTargetRoot proves the records_dir
// side: a RELATIVE records_dir in the TARGET profile resolves against the TARGET repo root,
// not the session cwd — so a cross-project gate record is found (the false-deny fix). An
// ABSOLUTE records_dir passes through unchanged.
func TestResolveTargetGate_RelativeRecordsDir_AnchorsOnTargetRoot(t *testing.T) {
	sandboxHome(t)
	target := writeProfileDir(t, "queue_root: queue\nrecords_dir: .leadkit/records\n")
	g := resolveTargetGate(target)
	if want := filepath.Join(target, ".leadkit", "records"); g.RecordsDir != want {
		t.Errorf("resolveTargetGate.RecordsDir = %q, want %q (relative records_dir anchored on the TARGET root)", g.RecordsDir, want)
	}
}

// TestResolveTargetGate_ScopeFollowsTarget proves scope is the TARGET's own governance:
// a target profile with a narrow enforced_remotes yields exactly that list, independent of
// any session.
func TestResolveTargetGate_ScopeFollowsTarget(t *testing.T) {
	sandboxHome(t)
	target := writeProfileDir(t, "queue_root: queue\nenforced_remotes: [target-only-remote]\n")
	g := resolveTargetGate(target)
	if len(g.EnforcedRemotes) != 1 || g.EnforcedRemotes[0] != "target-only-remote" {
		t.Errorf("resolveTargetGate.EnforcedRemotes = %v, want [target-only-remote] (the TARGET's own scope)", g.EnforcedRemotes)
	}
}

// TestResolveTargetGate_ProfiledNoEnforcedRemotes_WiresOwnOrigin: a PROFILED target
// that declares NO enforced_remotes gets an empty list AND its ProjectRoot set to the
// discovered project root — so the gate defaults to gating that project's OWN origin
// (zero config), not "enforce nothing".
func TestResolveTargetGate_ProfiledNoEnforcedRemotes_WiresOwnOrigin(t *testing.T) {
	sandboxHome(t)
	target := writeProfileDir(t, "queue_root: queue\nstore_root: tasks\n")
	g := resolveTargetGate(target)
	if len(g.EnforcedRemotes) != 0 {
		t.Errorf("resolveTargetGate(profiled, no enforced_remotes).EnforcedRemotes = %v, want EMPTY", g.EnforcedRemotes)
	}
	if g.ProjectRoot != target {
		t.Errorf("resolveTargetGate.ProjectRoot = %q, want the discovered project root %q (own-origin scope anchor)", g.ProjectRoot, target)
	}
}

// TestResolveTargetGate_SameProjectRegression: when the target IS the session's own
// project, resolveTargetGate yields the SAME scope + records_dir the old session-baked
// buildPushGateConfig(prof) did — byte-identical verdict inputs.
func TestResolveTargetGate_SameProjectRegression(t *testing.T) {
	home := sandboxHome(t)
	_ = home
	root := writeProfileDir(t, "queue_root: queue\nrecords_dir: .leadkit/records\nenforced_remotes: [acmecorp-product, acmecorp-services]\n")

	// The OLD behavior: build the gate from the profile loaded AT the project (LoadFrom is
	// exactly what Load would resolve when the session cwd is the project root).
	prof, found, err := config.LoadFrom(root)
	if err != nil || !found {
		t.Fatalf("LoadFrom(root) found=%v err=%v", found, err)
	}
	wantRecords := prof.Resolve(prof.RecordsDir)
	wantRemotes := prof.EnforcedRemotes

	g := resolveTargetGate(root)
	if g.RecordsDir != wantRecords {
		t.Errorf("same-project RecordsDir = %q, want %q (identical to the session-baked value)", g.RecordsDir, wantRecords)
	}
	if len(g.EnforcedRemotes) != len(wantRemotes) {
		t.Fatalf("same-project EnforcedRemotes = %v, want %v", g.EnforcedRemotes, wantRemotes)
	}
	for i := range wantRemotes {
		if g.EnforcedRemotes[i] != wantRemotes[i] {
			t.Errorf("same-project EnforcedRemotes[%d] = %q, want %q", i, g.EnforcedRemotes[i], wantRemotes[i])
		}
	}
}

// TestBuildPushGateConfig_UnionsSessionEnforcedRemotes is the coverage-regression fix's
// wiring proof: buildPushGateConfig loads the ACTING SESSION's own enforced_remotes
// (from the ambient policy profile, LEADKIT_PROFILE here) and unions them onto EVERY
// target gate as SessionEnforcedRemotes — distinct from the TARGET's own list. So a
// session that declares a cross-project repo gates its push to an UNPROFILED clone.
func TestBuildPushGateConfig_UnionsSessionEnforcedRemotes(t *testing.T) {
	sandboxHome(t)
	// The SESSION's ambient profile declares its cross-project list.
	sessionProfile := writeProfileDir(t, "queue_root: queue\nenforced_remotes: [session-cross-repo]\n")
	t.Setenv("LEADKIT_PROFILE", filepath.Join(sessionProfile, "leadkit.yaml"))

	cfg := buildPushGateConfig()
	// The TARGET is a DIFFERENT, unprofiled repo (no leadkit.yaml, no own list).
	unprofiledTarget := t.TempDir()
	g := cfg.ResolveGate(unprofiledTarget)

	if len(g.EnforcedRemotes) != 0 {
		t.Errorf("target-own EnforcedRemotes = %v, want EMPTY (the unprofiled target has no list)", g.EnforcedRemotes)
	}
	if len(g.SessionEnforcedRemotes) != 1 || g.SessionEnforcedRemotes[0] != "session-cross-repo" {
		t.Errorf("SessionEnforcedRemotes = %v, want [session-cross-repo] (the ACTING SESSION's list, unioned in)", g.SessionEnforcedRemotes)
	}
}

// --- owner-guard: resolveTargetOwnersPolicy -----------------------------------------

func TestResolveTargetOwnersPolicy_FromTargetProject(t *testing.T) {
	sandboxHome(t)
	// A target project whose owners_file defines ownership + its own clone_families.
	target := writeProfileDir(t, "queue_root: queue\nowners_file: owners.yaml\n")
	ownersYAML := "hard_block:\n  - secrets/**\npaths:\n  - glob: Projects/target-proj/frontend/**\n    owner: fe-agent\nclone_families:\n  target-proj:\n    - target-proj-fe\n"
	if err := os.WriteFile(filepath.Join(target, "owners.yaml"), []byte(ownersYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	policy, loaded := resolveTargetOwnersPolicy(target)
	if !loaded || policy == nil {
		t.Fatalf("resolveTargetOwnersPolicy(target) loaded=%v policy=%v, want a loaded target policy", loaded, policy)
	}
	if len(policy.Paths) != 1 || policy.Paths[0].Owner != "fe-agent" {
		t.Errorf("policy.Paths = %v, want the TARGET project's fe-agent ownership", policy.Paths)
	}
	if fam := policy.CloneFamilies["target-proj"]; len(fam) != 1 || fam[0] != "target-proj-fe" {
		t.Errorf("policy.CloneFamilies = %v, want the TARGET project's clone_families", policy.CloneFamilies)
	}

	// A project with NO owners_file (or no profile at all) → disabled (allow-unowned).
	noOwners := writeProfileDir(t, "queue_root: queue\n")
	if _, loaded := resolveTargetOwnersPolicy(noOwners); loaded {
		t.Error("a target project with no owners_file must resolve loaded=false (boundary disabled)")
	}
	if _, loaded := resolveTargetOwnersPolicy(t.TempDir()); loaded {
		t.Error("an unprofiled target must resolve loaded=false (boundary disabled), NEVER the ambient policy")
	}
}

// --- task-status-guard: resolveTargetTaskStatus -------------------------------------

func TestResolveTargetTaskStatus_FromTargetProject(t *testing.T) {
	sandboxHome(t)
	target := writeProfileDir(t, "queue_root: queue\nstore_root: board\n")
	targetFile := filepath.Join(target, "board", "t.md")

	scope := resolveTargetTaskStatus(targetFile)
	if want := filepath.Join(target, "board"); scope.StoreRoot != want {
		t.Errorf("resolveTargetTaskStatus.StoreRoot = %q, want %q (the TARGET project's store, anchored on its root)", scope.StoreRoot, want)
	}
	if len(scope.TriggerStatuses) == 0 {
		t.Error("scope.TriggerStatuses must never be empty — the built-in evidence set applies when the vocabulary is unset")
	}

	// An unprofiled target → the built-in default store; a file not under it is out of
	// scope (the no-op-allow fallback).
	def := resolveTargetTaskStatus(filepath.Join(t.TempDir(), "x.md"))
	if def.StoreRoot != config.Defaults().StoreRoot {
		t.Errorf("unprofiled target StoreRoot = %q, want the built-in default %q", def.StoreRoot, config.Defaults().StoreRoot)
	}
}

// TestResolveTargetGate_ListNamingOthers_StillGatesTargetsOwnOrigin: the push path. A
// TARGET repo whose own profile lists only OTHER projects in enforced_remotes still gates
// its own pushes — scope follows the target's own origin, resolved from the target (the
// test's cwd is elsewhere), and an all-zeros sha with no record is BLOCKED rather than
// "out of scope". The unprofiled control proves it is the own-origin rule doing it.
func TestResolveTargetGate_ListNamingOthers_StillGatesTargetsOwnOrigin(t *testing.T) {
	sandboxHome(t)
	const zeros = "0000000000000000000000000000000000000000"

	target := gitRepoNoCommits(t, "git@example.com:org/target-self.git")
	if err := os.WriteFile(filepath.Join(target, "leadkit.yaml"),
		[]byte("queue_root: queue\nstore_root: tasks\nenforced_remotes: [named-upstream]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := resolveTargetGate(target)
	g.RecordsDir = t.TempDir()
	ok, outcome, reason := g.CheckWithOutcome(zeros, target)
	if ok || outcome != gate.OutcomeUngated {
		t.Errorf("target's own origin with a list naming only others: want BLOCK/OutcomeUngated, got ok=%v outcome=%v (%s)", ok, outcome, reason)
	}

	unprofiled := gitRepoNoCommits(t, "git@example.com:org/not-governed.git")
	g2 := resolveTargetGate(unprofiled)
	g2.RecordsDir = t.TempDir()
	if ok, outcome, reason := g2.CheckWithOutcome(zeros, unprofiled); !ok || outcome != gate.OutcomeOutOfScope {
		t.Errorf("an unprofiled target must stay out of scope: got ok=%v outcome=%v (%s)", ok, outcome, reason)
	}
}
