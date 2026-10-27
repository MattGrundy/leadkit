package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/owners"
)

// hookCwd is pr-guard's own working directory when the command it's checking
// carries no `cd <path> &&` prefix of its own — the leadkit hook process's cwd
// IS the session's own cwd (that's how PreToolUse hooks run), the same fallback
// push-guard's own repoPath="" -> process-cwd convention already relies on.
// "" (a Getwd failure, effectively never in practice) is passed through
// unchanged — PRGuard's own runGit call then just uses ITS process cwd too.
func hookCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// parsePayloadCwd best-effort extracts Claude Code's top-level `cwd` from the raw
// PreToolUse payload (Fix 3). It decodes only that one field so a malformed/oversized
// payload never disturbs the guard (Dispatch does the real, hook-specific parse). "" on
// any decode failure or an absent field — the caller then keeps the process-cwd default.
func parsePayloadCwd(stdin []byte) string {
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(stdin, &p); err != nil {
		return ""
	}
	return p.Cwd
}

// buildPushGuardConfig wires push-guard's inputs. The WHITELIST is resolved per-push
// from the TARGET repo (resolveTargetWhitelist), NOT baked from the session-ambient
// prof — the correctness fix: a session pushing a DIFFERENT project's clone is judged
// by that clone's own policy, matching `config show`/`doctor` run from that repo. prof
// is used ONLY for the ambient-scoped ViolationsLogPath (where THIS session records its
// denies), which is deliberately unchanged.
func buildPushGuardConfig(prof *config.Profile) hook.PushGuardConfig {
	return hook.PushGuardConfig{
		EffectiveWhitelist: resolveTargetWhitelist,
		ViolationsLogPath:  prof.Resolve(prof.PushViolationsLog),
		// Enforcement mode is resolved PER-TARGET-REPO (like the whitelist), from that
		// repo's own governing profile — never the session-ambient prof — so a push to a
		// DIFFERENT project's clone honors THAT clone's guards.push_guard.mode.
		Mode: func(repoPath string) hook.GuardMode {
			return resolveTargetGuardModeDir(repoPath, config.GuardPushGuard)
		},
	}
}

// toHookMode translates a config guard-mode string (config.GuardMode*) into the pure
// guards' hook.GuardMode. Only the exact "enforce" string enforces; anything else
// (advisory/empty/unknown) is advisory — the same safe normalization
// config.EffectiveGuardMode applies, kept in one place so the two can't diverge.
func toHookMode(mode string) hook.GuardMode {
	if mode == config.GuardModeEnforce {
		return hook.ModeEnforce
	}
	return hook.ModeAdvisory
}

// resolveTargetGuardModeDir resolves a guard's enforcement mode from the profile that
// GOVERNS a repo-DIRECTORY target (push-guard / push-gate) — the dir-strict resolver
// (expand ~, safe-absolutize, stat-as-dir, git-toplevel; non-resolving → built-in
// default), matching how the whitelist/gate are resolved for the same target. An
// unprofiled/unresolvable target → config.Defaults() → guards.default_mode "" → advisory
// (backward-compatible fail-open, never a surprise enforce).
func resolveTargetGuardModeDir(repoPath, guard string) hook.GuardMode {
	prof := governingDirProfile(repoPath, currentResolveBase())
	return toHookMode(prof.EffectiveGuardMode(guard))
}

// resolveTargetWhitelist resolves the push whitelist that GOVERNS the target repo — the
// repo actually being pushed — exactly as `config show`/`doctor` resolve from a
// directory. It deliberately ignores the session-ambient profile (that leak is the bug
// this fixes): it loads the profile discovered by walking up from repoPath (or the
// process cwd when repoPath == "", push-guard's own convention), then computes that
// profile's effective whitelist for origin — the target's matching
// authority.<key>.push_whitelist override (deterministic longest-key, via the shared
// config.MatchAuthorityKey inside EffectivePushWhitelist) else its top-level
// push_whitelist. FALLBACK IS FAIL-SAFE, never the ambient profile and never allow-all:
// when the target has no leadkit.yaml up-tree, or a discovered profile is
// unreadable/malformed, or the resolved list is somehow empty, it returns the BUILT-IN
// default whitelist (config.DefaultPushWhitelist — feat/fix/chore/mg/user; protected
// branches still blocked). Returned to internal/hook's PURE guard as its injected
// EffectiveWhitelist resolver.
func resolveTargetWhitelist(repoPath, origin string) []string {
	return resolveTargetWhitelistFrom(repoPath, origin, currentResolveBase())
}

// resolveTargetWhitelistFrom is resolveTargetWhitelist with an explicit resolution base
// (the payload cwd + $HOME the extracted path is normalized against — see
// governingProfileFrom / resolveGoverningStart). It resolves the whitelist from the
// profile that GOVERNS the NORMALIZED target (tilde-expanded, safely-absolutized,
// stat-verified), never the session-ambient one: a tilde/relative/nonexistent/bare path
// that does not resolve to a real directory falls back to the BUILT-IN default profile,
// NOT a walk-up into the session-ancestor tree (the ambient-profile leak). A resolved-but-empty
// list still falls back to the built-in default whitelist so the guard stays
// default-deny + fail-closed.
func resolveTargetWhitelistFrom(repoPath, origin string, b hookResolveBase) []string {
	prof := governingDirProfile(repoPath, b)
	whitelist, _ := config.EffectivePushWhitelist(prof, origin)
	if len(whitelist) == 0 {
		// A profile that resolves an empty whitelist must never mean allow-all — fall
		// back to the built-in default so the guard stays default-deny + fail-closed.
		return append([]string(nil), config.DefaultPushWhitelist...)
	}
	return whitelist
}

// governingProfile resolves the profile that GOVERNS targetPath — a FILE the tool call
// operates on (owner-guard / task-status-guard) — the leadkit.yaml discovered by walking
// up from the NORMALIZED target (config.LoadFrom), else the BUILT-IN default
// (config.Defaults()), NEVER the session-ambient profile. It is the ambient-profile fix
// generalized: every per-target FILE guard resolver builds on this so each judges a target
// by the TARGET's own governance, not the editing session's. Quiet + best-effort, matching
// resolveTargetWhitelist: a discovered-but-unreadable/malformed profile also falls back to
// Defaults() with no stderr noise (a PreToolUse guard must fall back safely and silently).
//
// FILE semantics (deliberately distinct from a repo-DIRECTORY target — see
// governingDirProfile): the target is a file that may not exist yet (a Write creating a new
// task file), so the walk-up runs from the NORMALIZED path lexically rather than
// stat-gating it to a real directory — otherwise the guard would go inert (fall to
// Defaults) for every not-yet-created file. The ambient-profile leak surface is the shell
// `cd`/`-C` DIRECTORY path (push), not a tool file_path (already absolute); the file
// resolver still hardens it defensively — expanding a leading `~`/`~user` and absolutizing
// a relative path against the PAYLOAD cwd (Fix 1/3) — so no mis-based path can form.
func governingProfile(targetPath string) *config.Profile {
	return governingProfileFrom(targetPath, currentResolveBase())
}

// governingProfileFrom is governingProfile with an explicit resolution base. It normalizes
// the raw FILE path (expand env + `~`/`~user`, absolutize a relative path against the
// payload cwd) BEFORE the lexical LoadFrom walk-up. No profile up-tree falls back to Defaults()
// — never the session-ambient one. A profile that is found and did not load is NOT the same as
// no profile: see governingFileProfile.
func governingProfileFrom(targetPath string, b hookResolveBase) *config.Profile {
	prof, _ := governingFileProfile(targetPath, b)
	return prof
}

// governingDirProfile resolves the profile that GOVERNS a repo-DIRECTORY target — the repo
// actually being pushed (push-guard's whitelist + push-gate's review-gate). This is the
// ambient-profile leak surface: the directory comes from a shell `cd <path> &&` / `-C <path>`
// token the binary never ran through a shell, so it may be `~`/`~user`-prefixed, relative,
// bogus, or absent. It runs that raw path through resolveGoverningStart (expand `~`,
// absolutize against the PAYLOAD cwd, STAT-as-directory, prefer the git toplevel) BEFORE
// LoadFrom: a path that does NOT resolve to a real directory → the BUILT-IN default
// profile, NEVER a walk-up into the session-ancestor tree (the leak). Only a real directory
// walks up (from its own git root where resolvable), so it finds the target repo's own
// profile or none → built-in default. A discovered-but-unreadable/malformed profile also
// falls back to Defaults().
func governingDirProfile(dirPath string, b hookResolveBase) *config.Profile {
	start, ok := resolveGoverningStart(dirPath, b)
	if !ok {
		d := config.Defaults()
		return &d
	}
	prof, found, err := config.LoadFrom(start)
	if !found || err != nil || prof == nil {
		d := config.Defaults()
		return &d
	}
	return prof
}

// normalizeFileTarget hardens a raw FILE target path (owner-guard / task-status-guard)
// without the stat-as-directory gate the repo-DIRECTORY resolver applies — the file may be
// about to be created, so its path is walked up LEXICALLY (config.LoadFrom stats only the
// candidate leadkit.yaml at each level, never the path itself). It still expands env vars
// and a leading `~`/`~user`, and absolutizes a relative path against the PAYLOAD cwd (Fix
// 1/3), so a mis-based path can't form. An unresolvable `~user`, or an empty path, falls
// back to the payload cwd rather than a bare/relative string.
func normalizeFileTarget(rawPath string, b hookResolveBase) string {
	p := rawPath
	if p == "" {
		return b.cwd
	}
	p = os.ExpandEnv(p)
	if exp := expandTilde(p, b.home); exp != "" {
		p = exp
	} else if p != "" && p[0] == '~' {
		// A `~`/`~user` we could not expand (no home / unknown user): don't walk up a
		// literal `~`-path (its lexical walk-up would fall through to the process cwd = the
		// session tree) — anchor on the payload cwd instead.
		return b.cwd
	}
	if !filepath.IsAbs(p) {
		if b.cwd == "" {
			return p
		}
		p = filepath.Join(b.cwd, p)
	}
	return filepath.Clean(p)
}

// hookResolveBase carries the payload-derived base a raw target path is normalized
// against (Fix 3 of docs/build-tasks/guard-target-path-normalization.md): cwd is the
// PreToolUse payload's own working directory — the base for absolutizing a RELATIVE
// `cd`/`-C` target and the fallback target for a truly-bare `git push` — and home is the
// $HOME a leading `~` expands against (never a shell; the binary execs without one).
type hookResolveBase struct {
	cwd  string
	home string
}

// defaultResolveBase is the base used by every direct caller (and every test) that does
// not thread a payload cwd: the hook process cwd (which IS the session cwd for a real
// PreToolUse hook, the pre-existing convention) and $HOME.
func defaultResolveBase() hookResolveBase {
	return hookResolveBase{cwd: hookCwd(), home: os.Getenv("HOME")}
}

// resolveBaseOverride, when non-nil, is the payload-derived resolution base runHook
// installs for THIS single hook invocation from the PreToolUse payload's `cwd` (Fix 3).
// nil — every direct caller and unit test — means currentResolveBase() yields
// defaultResolveBase(), so behavior is unchanged outside the wired hook path. Safe as a
// process-global: a `leadkit hook` process handles exactly one payload and exits.
var resolveBaseOverride *hookResolveBase

// currentResolveBase returns the payload base runHook installed for this invocation, else
// the process-cwd default.
func currentResolveBase() hookResolveBase {
	if resolveBaseOverride != nil {
		return *resolveBaseOverride
	}
	return defaultResolveBase()
}

// resolveGoverningStart turns a RAW target path — an extracted `-C`/`cd` value the binary
// never ran through a shell (so it may be `~`/`~user`-prefixed, relative, bogus, or empty
// for a truly-bare push) — into the ABSOLUTE, STAT-VERIFIED directory to start the
// leadkit.yaml walk-up from. ok=false means "no target we can resolve" → the caller uses
// the BUILT-IN default profile and NEVER walks up into the session-ancestor tree (the
// ambient-profile leak). Steps, in order:
//   - empty path → the payload cwd (Fix 3: a truly-bare `git push` resolves from where the
//     tool call runs, not the hook subprocess's own cwd);
//   - expand env vars, then a leading `~`/`~user` against HOME (Fix 1 — the exec-without-a-
//     shell never does this for us: the unexpanded `~` is exactly what leaked to project-i);
//   - absolutize a still-relative path against the payload cwd (Fix 3), not the hook cwd;
//   - STAT (Fix 1): a path that is not a real directory → ok=false → built-in default;
//   - prefer the target's GIT TOPLEVEL (Fix 2, best-effort): re-root the walk-up at the
//     clone's own git root so a sibling project's leadkit.yaml can't govern it. A non-git
//     real directory keeps its own path as the start, so its own at/above leadkit.yaml
//     still resolves (no regression, and never fail-closed on the git call).
func resolveGoverningStart(rawPath string, b hookResolveBase) (string, bool) {
	p := rawPath
	if p == "" {
		p = b.cwd // truly-bare push → the payload cwd (Fix 3)
	}
	if p == "" {
		return "", false
	}
	p = os.ExpandEnv(p)
	p = expandTilde(p, b.home)
	if p == "" {
		return "", false // an unexpandable ~ / ~user is non-resolving → built-in default
	}
	if !filepath.IsAbs(p) {
		if b.cwd == "" {
			return "", false // no reliable base → non-resolving → built-in default
		}
		p = filepath.Join(b.cwd, p)
	}
	p = filepath.Clean(p)
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return "", false // Fix 1: not a real directory → built-in default, never a walk-up
	}
	if top, ok := gitToplevel(p); ok && top != "" {
		return top, true // Fix 2: root the walk-up at the target's own git toplevel
	}
	return p, true
}

// expandTilde expands a leading `~`/`~/x` (to home) and `~user`/`~user/x` (to that user's
// home) using HOME / an os user lookup — NOT a shell. An empty home falls back to
// os.Getenv("HOME"). A `~` with no home, or a `~user` that can't be resolved, returns ""
// so the caller treats it as non-resolving (→ built-in default) rather than absolutizing a
// literal `~`-path into an unrelated tree (the ambient-profile leak mechanism).
func expandTilde(p, home string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if home == "" {
		home = os.Getenv("HOME")
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		if home == "" {
			return ""
		}
		return filepath.Join(home, p[len("~/"):])
	}
	// ~user or ~user/rest
	name := p[1:]
	rest := ""
	if i := strings.IndexByte(name, '/'); i >= 0 {
		rest = name[i+1:]
		name = name[:i]
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return "" // unresolvable ~user → non-resolving (built-in default), never a literal path
	}
	if rest == "" {
		return u.HomeDir
	}
	return filepath.Join(u.HomeDir, rest)
}

// gitToplevel resolves the git working-tree root that CONTAINS dir (Fix 2, defense-in-
// depth): rooting the leadkit.yaml walk-up at the target clone's OWN git root structurally
// stops it wandering into a sibling project's tree. ok=false when dir is not inside a git
// repo, or git can't run at all — the caller then keeps the plain normalized directory,
// never fail-closing a legitimate push on this best-effort step. An injectable var so
// tests are deterministic without a real git binary.
var gitToplevel = func(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", false
	}
	return top, true
}

// sessionIdentitySlug computes THIS session's project IDENTITY slug — WHO the session
// is — anchored strictly at its OWN working tree. It is deliberately SEPARATE from the
// POLICY profile resolution (config.Load → install.ResolveProjectSlug, which may honor
// an inherited $LEADKIT_PROFILE or walk UP to an ancestor leadkit.yaml): identity must
// NEVER inherit an env profile or an ancestor's config, or a config-less collaborating
// project collapses onto whatever shared slug the ambient profile resolves and its
// briefs/inbox are mis-addressed (docs/build-tasks/session-identity-cwd-honest.md).
//
// THE BOUNDARY: policy = where the config lives (may walk up / honor env — guards,
// gate scope, queue ops, daemon install); identity = who the session is (its own
// cwd/git-repo). This resolver is identity only.
//
// Resolution, in order:
//  1. root = the git toplevel of cwd (`git rev-parse --show-toplevel`), else cwd itself
//     when cwd is not a git repo. NOTE: for a git WORKTREE the toplevel is the worktree
//     dir (a branch/chore name), NOT the main repo — which is exactly why the derivation
//     below prefers the git ORIGIN's repo-name over this basename.
//  2. <root>/leadkit.yaml — the OWN-tree config ONLY, never an ancestor above root
//     (config.LoadProjectFile reads exactly that path, no walk-up, no env): a non-empty
//     project_id → slugify(project_id); else a queue_root → its project-root basename
//     slug. So a project WITH its own config resolves EXACTLY as ResolveProjectSlug does
//     today (no regression). This explicit adopter override wins outright.
//  3. EXTERNAL config project_id — an external ~/.claude/leadkit/config/<slug>.yaml
//     (keyed by the ORIGIN-based slug, steps 4→5, so it resolves from ANY clone/worktree)
//     whose project_id overrides the derived slug.
//  4. git ORIGIN repo-name (the PRIMARY derivation): parse remote.origin.url of the repo
//     at root → last path segment minus ".git" → slugify. A worktree shares its main
//     repo's origin, so this is STABLE across worktrees AND differently-named clone dirs.
//     Source: "git origin repo-name".
//  5. git-toplevel basename (fallback: no resolvable origin, a local-only repo).
//  6. else slugify(basename(cwd)) — the non-git last resort.
//
// Chicken-and-egg: step 3 needs the external config, which is KEYED by the slug. The
// KEY is derived by steps 4→6 (origin-name/basename, NO config, via
// config.ExternalConfigPathForDir which now keys the same origin-based way); THEN a
// project_id in the keyed config overrides the final reported slug.
//
// Best-effort throughout: an unreadable/malformed own config or a malformed origin URL
// degrades to the basename derivation rather than failing. REUSES gitToplevel +
// gitOriginRemote + install.Slugify/OriginRepoName/ResolveProjectSlug.
func sessionIdentitySlug(cwd string) (slug, source string) {
	root := cwd
	fromGit := false
	if cwd != "" {
		if top, ok := gitToplevel(cwd); ok && top != "" {
			root = top
			fromGit = true
		}
	}
	// EXPLICIT project_id wins over any derived slug. Capture the in-repo profile once so
	// its queue_root can serve as a LATE fallback (below origin) without a reload.
	var inRepoProf *config.Profile
	if root != "" {
		if prof, ok, err := config.LoadProjectFile(filepath.Join(root, config.DefaultProfileName)); ok && err == nil && prof != nil {
			inRepoProf = prof
			if s := install.ResolveProjectSlug(prof.ProjectID, ""); s != "" {
				return s, "own leadkit.yaml project_id"
			}
		}
	}
	// EXTERNAL config project_id: a repo with NO in-repo leadkit.yaml may still be
	// governed by ~/.claude/leadkit/config/<slug>.yaml (keyed by THIS repo's ORIGIN-based
	// slug — never env/ancestor — so it resolves from any clone/worktree). Consult its
	// project_id so identity matches policy (both resolve to the external config's declared
	// project). Only project_id is used — an external queue_root anchors to the config dir,
	// so it is NOT a meaningful identity signal; a config with no project_id falls through
	// to the origin/basename derivation below.
	if root != "" {
		if extPath, _, ok := config.ExternalConfigPathForDir(root); ok {
			if prof, ok, err := config.LoadProjectFile(extPath); ok && err == nil && prof != nil {
				if s := install.ResolveProjectSlug(prof.ProjectID, ""); s != "" {
					return s, "external <slug>.yaml project_id"
				}
			}
		}
	}
	// git ORIGIN repo-name — the PRIMARY DERIVED identity (after an explicit project_id).
	// The origin is what the repo IS, stable across every worktree (shared origin) and
	// every differently-named clone dir. It BEATS the in-repo queue_root basename below,
	// which — like the dir basename — is only a weak checkout-dir signal (a queue_root of
	// `./queue` yields the repo's own DIR name, which differs from the origin when a repo is
	// cloned under a different directory name).
	if root != "" {
		if name := install.OriginRepoName(gitOriginRemote(root)); name != "" {
			return name, "git origin repo-name"
		}
	}
	// FALLBACK (no resolvable origin — a local-only repo): the in-repo queue_root basename,
	// if the in-repo config set one. This keeps origin-less projects (a local repo with no
	// remote) resolving to their queue_root-derived slug as before.
	if inRepoProf != nil {
		if qr := inRepoProf.Resolve(inRepoProf.QueueRoot); qr != "" {
			if s := install.ResolveProjectSlug("", qr); s != "" {
				return s, "own leadkit.yaml queue_root"
			}
		}
	}
	// Last resort: git-toplevel basename; non-git → cwd basename.
	slug = install.Slugify(filepath.Base(root))
	if fromGit {
		source = "git-repo basename"
	} else {
		source = "cwd basename"
	}
	return slug, source
}

// sessionCwd is the session's OWN working directory for identity resolution: the
// payload-derived cwd runHook installs for a hook invocation (which IS the session's
// cwd), else the process cwd. Mirrors currentResolveBase's cwd, so identity resolves
// from the same directory the guard-target resolution already trusts.
func sessionCwd() string {
	return currentResolveBase().cwd
}

// buildPushGateConfig wires push-gate. The review-gate a push is judged against is
// resolved PER-TARGET-REPO (resolveTargetGate), NOT baked from the session-ambient prof —
// the correctness fix mirroring buildPushGuardConfig: a session pushing a DIFFERENT
// project's clone enforces THAT clone's own gate scope + records_dir, so a narrow-list
// session can no longer make a governed repo out-of-scope. Cwd is the no-`git -C`/`cd`
// repo fallback (the hook's own cwd, which IS the session's cwd) and $HOME the ~-expansion
// the shell botched.
func buildPushGateConfig() hook.PushGateConfig {
	// Cwd/Home come from the current resolution base so a truly-bare push and a `~`-path
	// resolve against the PAYLOAD cwd + $HOME (Fix 3), not the hook subprocess's own cwd.
	b := currentResolveBase()
	// ResolveGate is the shared per-target resolver (targetGateResolver): the TARGET's own
	// gate with the acting SESSION's enforced_remotes unioned in ADD-ONLY, so a session's
	// declared CROSS-project pushes are gated even to an UNPROFILED clone of one of those
	// repos, which target-own gating alone (an unprofiled target -> Defaults -> out of scope)
	// would miss. It can only widen scope.
	return hook.PushGateConfig{
		ResolveGate: targetGateResolver(),
		Cwd:         b.cwd,
		Home:        b.home,
		// Enforcement mode is resolved PER-TARGET-REPO (like the gate), from that repo's
		// own governing profile — so a push to another project's clone honors THAT clone's
		// guards.push_gate.mode.
		Mode: func(repoPath string) hook.GuardMode {
			return resolveTargetGuardModeDir(repoPath, config.GuardPushGate)
		},
		// Whether the target's profile could be READ: the difference between a repo the
		// profile exempts and a repo whose profile was lost (ticket 204).
		ProfileProblem: hookProfileProblem,
		// A .leadkit/ directory and no profile anywhere, under a HOME that was searched: allowed, with
		// the state reported on the decision (ticket 302), never refused and never silent.
		Unconfigured: hookUnconfigured,
	}
}

// resolveTargetGate builds the review-gate that GOVERNS the target repo — from the repo's
// own governing profile (governingProfile), exactly as `config show`/`doctor` resolve
// from a directory. The TARGET-OWN scope decision here deliberately ignores the
// session-ambient profile; the acting session's enforced_remotes are unioned in ADD-ONLY
// by buildPushGateConfig's wrapper (SessionEnforcedRemotes), never here. Scope
// (enforced_remotes + the profile's own project root) and records_dir come from the
// TARGET profile; the FALLBACK for an unprofiled target is config.Defaults(), whose
// EnforcedRemotes is empty AND whose ProjectRoot is "" — so an unprofiled target is
// OUT of scope (its own profile, if any, governs it), while a PROFILED repo with no
// enforced_remotes — or with a list that omits itself — gates its OWN origin
// (ProjectRoot's origin) with zero config. An explicit enforced_remotes list adds the
// OTHER repos it names; it never removes the target's own origin. A RELATIVE records_dir
// in the target profile resolves against the TARGET repo root (via prof.Resolve), not
// the session root, so a cross-project gate record is FOUND; gate.New() seeds the real
// git + central default records dir, overridden only when the profile resolves a
// non-empty path (config.Defaults() always seeds one). Returned to the PURE guard as
// its injected ResolveGate resolver.
func resolveTargetGate(repoPath string) *gate.Gate {
	// A repo-DIRECTORY target → the dir-strict resolver (expand ~, safe-absolutize,
	// stat-as-dir, git-toplevel; non-resolving → built-in default), never the file-lenient
	// walk-up, so a tilde/relative/nonexistent push target can't leak a session-ancestor gate.
	prof := governingDirProfile(repoPath, currentResolveBase())
	g := gate.New()
	if rd := prof.Resolve(prof.RecordsDir); rd != "" {
		g.RecordsDir = rd
	}
	g.EnforcedRemotes = prof.EnforcedRemotes
	g.ProjectRoot = prof.ProjectRoot()
	return g
}

// evidenceRequiringStatuses are the terminal-ish statuses task-status-guard treats as
// "asserts work exists" — the shell's awaiting_pr_review|in_review|completed set.
// buildTaskStatusConfig intersects these with the profile's canonical_statuses so the
// trigger set is profile-driven (a project whose vocabulary omits one won't trigger on it).
var evidenceRequiringStatuses = []string{"completed", "in_review", "awaiting_pr_review"}

// buildTaskStatusConfig wires task-status-guard with a PER-TARGET scope resolver
// (resolveTargetTaskStatus): the store_root + status vocabulary that govern a task file
// are resolved from the file's OWN project, not the session — so a task file is guarded by
// THAT project's evidence rule even from a foreign session, and a file under no known
// store falls out of scope and fails open exactly as today.
func buildTaskStatusConfig() hook.TaskStatusConfig {
	return hook.TaskStatusConfig{Resolve: resolveTargetTaskStatus}
}

// resolveTargetTaskStatus resolves the TaskStatusScope governing targetFile from the
// project that owns it (governingProfile), never the session-ambient profile. An
// unprofiled target falls back to config.Defaults() — its default store_root (the file is
// then in scope only if it lives under that built-in store) and the built-in status
// vocabulary — so the guard no-ops (allow) for a file under no known project, unchanged.
func resolveTargetTaskStatus(targetFile string) hook.TaskStatusScope {
	return taskStatusScope(governingProfile(targetFile))
}

// taskStatusScope derives the store_root scope, the trigger statuses (evidence-requiring
// ∩ canonical_statuses), the status aliases (so `done`→completed triggers) and the full
// canonical legend from one profile.
func taskStatusScope(prof *config.Profile) hook.TaskStatusScope {
	canon := map[string]bool{}
	for _, s := range prof.CanonicalStatuses {
		canon[s] = true
	}
	var trigger []string
	for _, s := range evidenceRequiringStatuses {
		if canon[s] {
			trigger = append(trigger, s)
		}
	}
	// A profile with no canonical_statuses set at all still enforces the built-in set,
	// so the guard is never silently inert just because the vocabulary key is empty.
	if len(trigger) == 0 {
		trigger = append([]string(nil), evidenceRequiringStatuses...)
	}
	return hook.TaskStatusScope{
		StoreRoot:       prof.Resolve(prof.StoreRoot),
		TriggerStatuses: trigger,
		StatusAliases:   prof.StatusAliases,
		CanonicalLegend: prof.CanonicalStatuses,
	}
}

// buildOwnerGuardConfig wires owner-guard from prof: the owners policy (owners_file,
// project-relative like goal_file — absent/missing/unparseable → boundary DISABLED,
// fail open), the caller identity (CLAUDE_AGENT_NAME → .leadkit/active-agent → lead),
// the .leadkit/bypass sentinel, and $HOME/cwd for the home-relative REL. The sentinels
// (bypass, active-agent) and the audit log all resolve from the project's `.leadkit/`
// folder — the same leadkit-owned namespace as goal_file/handover_file/the throttle
// lock — so they don't litter the repo root. (The shell original kept these out of the
// working tree in ~/.claude/projects/<slug>/ for the same reason; .leadkit/ is the
// portable equivalent.) No dual-location fallback: a hard move, as this shipped this
// session with no adopter relying on the old root location.
func buildOwnerGuardConfig(prof *config.Profile) hook.OwnerGuardConfig {
	cfg := hook.OwnerGuardConfig{
		Lead:          owners.DefaultLead,
		Home:          os.Getenv("HOME"),
		Pwd:           hookCwd(),
		ResolvePolicy: resolveTargetOwnersPolicy,
		// Enforcement mode + whether an owners policy is CONFIGURED are resolved PER-TARGET
		// (from the target FILE's own project, like the policy), so owner-guard fails CLOSED
		// on a configured-but-unreadable owners policy only when THAT project's
		// guards.owner_guard.mode is enforce — never the session-ambient prof.
		Enforcement: resolveTargetOwnersEnforcement,
		// A governing profile that did not load: why, and the verdict when nothing of it can be read.
		ProfileProblem: resolveTargetOwnersProfileProblem,
	}
	cfg.AuditLogPath = prof.Resolve(".leadkit/owner-guard.log")

	if _, err := os.Stat(prof.Resolve(".leadkit/bypass")); err == nil {
		cfg.Bypass = true
	}
	activeAgent, _ := os.ReadFile(prof.Resolve(".leadkit/active-agent"))
	cfg.Caller = owners.ResolveCaller(os.Getenv("CLAUDE_AGENT_NAME"), string(activeAgent), owners.DefaultLead)
	return cfg
}

// resolveTargetOwnersPolicy resolves the ownership Policy that GOVERNS targetPath from the
// project that OWNS it (governingProfile → its owners_file), never the session-ambient
// profile — so a project that defines an owners_file (and its own clone_families) has its
// boundary enforced even from a foreign session. A target project with no profile or no
// owners_file (or an absent/unparseable owners file) → loaded=false → boundary DISABLED
// (allow-unowned), exactly today's semantics for an unprofiled project. The FALLBACK is
// never the ambient profile and never allow-when-should-deny.
func resolveTargetOwnersPolicy(targetPath string) (*owners.Policy, bool) {
	return loadOwnersPolicy(governingProfile(targetPath))
}

// resolveTargetOwnersEnforcement resolves, from the project that GOVERNS targetPath (its
// own profile, never the session-ambient one), owner-guard's enforcement mode AND whether
// an owners policy is CONFIGURED there (owners_file set). ownersConfigured distinguishes
// "no boundary exists" (owners_file unset → allow, both modes) from "boundary declared but
// unreadable" (owners_file set but its file won't load → enforce fails CLOSED). It reads
// owners_file != "" only — the actual load/parse (and its loaded=false) is
// resolveTargetOwnersPolicy's job; the guard combines the two signals.
func resolveTargetOwnersEnforcement(targetPath string) (hook.GuardMode, bool) {
	prof := governingProfile(targetPath)
	mode := toHookMode(prof.EffectiveGuardMode(config.GuardOwnerGuard))
	return mode, prof.OwnersFile != ""
}

// resolveTargetOwnersProfileProblem is owner-guard's view of fileProfileProblem for targetPath.
func resolveTargetOwnersProfileProblem(targetPath string) hook.OwnerProfileProblem {
	_, p := governingFileProfile(targetPath, currentResolveBase())
	if p == nil {
		return hook.OwnerProfileProblem{}
	}
	return hook.OwnerProfileProblem{Mismatch: p.Mismatch, Unreadable: p.Unreadable, File: p.Path, Why: p.Why}
}

// loadOwnersPolicy loads and parses a profile's owners_file into a Policy. owners_file
// unset, its file absent, or an unparseable file all yield loaded=false (boundary
// disabled / fail open) — a policy we can't read must never block.
func loadOwnersPolicy(prof *config.Profile) (*owners.Policy, bool) {
	if prof.OwnersFile == "" {
		return nil, false
	}
	data, err := os.ReadFile(prof.Resolve(prof.OwnersFile))
	if err != nil {
		return nil, false // missing file → disabled, exactly like the shell's absent owners.yaml
	}
	policy, err := owners.ParsePolicy(data)
	if err != nil {
		return nil, false // unparseable → fail open (a policy we can't read must not block)
	}
	return &policy, true
}
