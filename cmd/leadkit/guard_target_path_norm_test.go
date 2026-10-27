package main

// guard-target-path-normalization (SECURITY): the shared path→profile resolution used by
// push-guard (resolveTargetWhitelist) and the push-gate/owner-guard/task-status resolvers
// (governingProfile) must never let a `~`/relative/nonexistent/bare TARGET path resolve to
// the session-ancestor profile — the ambient-profile leak. These whitebox tests drive the real
// resolvers with an explicit hookResolveBase (the payload cwd + $HOME the extracted path is
// normalized against) so a "session" tree and a "target" tree never coincide, and prove:
// a tilde-cd to a real profile-less clone is ALLOWED (built-in default, not the ambient
// [chore/*]); an absolute-cd still works; a non-resolving path falls to the built-in
// default (never the session-ancestor); git-toplevel re-roots the walk-up; the payload cwd
// (not os.Getwd() of the hook subprocess) is the bare-push base; same-project verdicts are
// unchanged; and protected branches stay denied through every path form.

import (
	"os"
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// choreProfileDir writes a leadkit.yaml whose push_whitelist is ONLY [chore/*] into a fresh
// dir and returns it — the stand-in for the project-i session-ancestor profile that must
// never leak onto a cross-repo push.
func choreProfileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte("queue_root: q\npush_whitelist:\n  - chore/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// realDir makes a real, profile-less directory (a "clone" with no leadkit.yaml up-tree
// because t.TempDir sits under the OS temp root, not any project tree).
func realDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestNorm_AmbientProfile_TildeCd_NowAllowed is the ambient-profile reproduction: the resolution base
// (payload cwd) is a session under a [chore/*] profile, and the push targets `cd
// ~/<clone> && git push mg/x` where ~/<clone> expands to a REAL profile-less clone OUTSIDE
// the session tree. Before this fix the literal `~` was absolutized under the session cwd
// and the walk-up found the session's [chore/*] (mg denied). After it, `~` expands, the
// clone resolves, and the whitelist is the BUILT-IN default (mg allowed) — never [chore/*].
func TestNorm_AmbientProfile_TildeCd_NowAllowed(t *testing.T) {
	home := t.TempDir()
	session := choreProfileDir(t) // the project-i session-ancestor [chore/*]
	clone := filepath.Join(home, "Projects", "shared-infra")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	base := hookResolveBase{cwd: session, home: home}

	wl := resolveTargetWhitelistFrom("~/Projects/shared-infra", "", base)
	if !hookMatch(wl, "mg/example-proxy") {
		t.Errorf("tilde-cd to a profile-less clone must resolve the BUILT-IN default (mg/* allowed), got %v — the ambient [chore/*] leaked", wl)
	}
	if hookMatch(wl, "chore/only") && !hookMatch(wl, "feat/x") {
		t.Errorf("resolved whitelist %v looks like the ambient [chore/*], not the built-in default", wl)
	}
	// Proof the [chore/*] IS reachable when the target genuinely is the session (a bare
	// push): same base, empty target → the session's [chore/*] → mg DENIED. The contrast
	// shows the tilde-cd correctly redirected the target away from the session profile.
	bareWL := resolveTargetWhitelistFrom("", "", base)
	if hookMatch(bareWL, "mg/example-proxy") {
		t.Errorf("control failed: a bare push resolves the session [chore/*], which must DENY mg/* — got %v", bareWL)
	}
	if !hookMatch(bareWL, "chore/thing") {
		t.Errorf("control: session profile should allow chore/*, got %v", bareWL)
	}
}

// TestNorm_AbsoluteCd_Regression: an ABSOLUTE profile-less target (the form that already
// worked) still resolves the built-in default → mg allowed.
func TestNorm_AbsoluteCd_Regression(t *testing.T) {
	session := choreProfileDir(t)
	clone := realDir(t)
	base := hookResolveBase{cwd: session, home: t.TempDir()}

	wl := resolveTargetWhitelistFrom(clone, "", base)
	if !hookMatch(wl, "mg/x") {
		t.Errorf("absolute-cd to a profile-less clone must allow mg/* (built-in default), got %v", wl)
	}
	if hookMatch(wl, "master") {
		t.Errorf("built-in default must still deny master, got %v", wl)
	}
}

// TestNorm_NonResolving_FallsToBuiltinDefault: a tilde/relative/nonexistent target that
// does NOT resolve to a real directory must fall back to the BUILT-IN default (mg allowed,
// master denied), NEVER walk up into the session-ancestor [chore/*].
func TestNorm_NonResolving_FallsToBuiltinDefault(t *testing.T) {
	home := t.TempDir()
	session := choreProfileDir(t)
	base := hookResolveBase{cwd: session, home: home}

	cases := map[string]string{
		"relative typo (absolutized under session, nonexistent)": "shared-infra-typo",
		"tilde nonexistent":         "~/no-such-clone",
		"tilde-subpath nonexistent": "~/Projects/ghost",
		"unresolvable ~user":        "~nosuchuser-xyz/repo",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			wl := resolveTargetWhitelistFrom(raw, "", base)
			if !hookMatch(wl, "mg/x") {
				t.Errorf("non-resolving %q must fall to the built-in default (mg/* allowed), got %v — session [chore/*] leaked", raw, wl)
			}
			if hookMatch(wl, "master") || hookMatch(wl, "develop") || hookMatch(wl, "release/9") {
				t.Errorf("built-in default fallback must still deny protected branches, got %v", wl)
			}
			// Must NOT be the ambient [chore/*] (which denies mg/*): the mg/* assertion
			// above already proves this, this is the explicit anti-leak restatement.
		})
	}
}

// TestNorm_GitToplevel_ResolvesFromToplevel (Fix 2): a normalized real directory inside a
// git repo resolves the profile from the repo's git TOPLEVEL. Driven with an injected
// gitToplevel so it is deterministic and unambiguously proves the toplevel — not a plain
// lexical walk-up from the (deeper) target — supplies the governing profile.
func TestNorm_GitToplevel_ResolvesFromToplevel(t *testing.T) {
	// A distinctive profile at the "git toplevel" the injected resolver reports.
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, "leadkit.yaml"), []byte("queue_root: q\npush_whitelist:\n  - toponly/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The actual (real) target dir carries NO leadkit.yaml, so only the injected toplevel
	// can supply toponly/*.
	target := realDir(t)

	orig := gitToplevel
	gitToplevel = func(dir string) (string, bool) {
		if dir == target {
			return top, true
		}
		return "", false
	}
	defer func() { gitToplevel = orig }()

	base := hookResolveBase{cwd: t.TempDir(), home: t.TempDir()}
	wl := resolveTargetWhitelistFrom(target, "", base)
	if !hookMatch(wl, "toponly/x") {
		t.Errorf("git-toplevel profile (toponly/*) not applied — got %v; the walk-up must re-root at the reported toplevel", wl)
	}
	if hookMatch(wl, "mg/x") {
		t.Errorf("the target's own dir has no profile; only the toplevel profile (toponly/*) should govern, got %v", wl)
	}
}

// TestNorm_NonGit_FallsToBuiltinDefault (Fix 2): a real, profile-less directory that is NOT
// a git repo (git-toplevel reports nothing) resolves to the built-in default, never an
// unrelated profile.
func TestNorm_NonGit_FallsToBuiltinDefault(t *testing.T) {
	orig := gitToplevel
	gitToplevel = func(string) (string, bool) { return "", false } // simulate a non-git path
	defer func() { gitToplevel = orig }()

	target := realDir(t) // real dir, no leadkit.yaml up-tree
	base := hookResolveBase{cwd: choreProfileDir(t), home: t.TempDir()}
	wl := resolveTargetWhitelistFrom(target, "", base)
	for _, allowed := range []string{"feat/x", "fix/x", "chore/x", "mg/x", "user/x"} {
		if !hookMatch(wl, allowed) {
			t.Errorf("non-git profile-less target should resolve the built-in default (allow %q), got %v", allowed, wl)
		}
	}
	if hookMatch(wl, "master") {
		t.Errorf("built-in default must still deny master, got %v", wl)
	}
}

// TestNorm_PayloadCwdBase_BarePush (Fix 3): a truly-bare `git push` (no cd/-C) resolves its
// target from the PAYLOAD cwd, not os.Getwd() of the hook subprocess. With the payload base
// pointing at a profile-less clone the bare push is ALLOWED (built-in default); with it
// pointing at a [chore/*] session it is judged by that profile (mg denied) — the base
// genuinely drives the bare-push target.
func TestNorm_PayloadCwdBase_BarePush(t *testing.T) {
	profileless := realDir(t)
	baseClone := hookResolveBase{cwd: profileless, home: t.TempDir()}
	if wl := resolveTargetWhitelistFrom("", "", baseClone); !hookMatch(wl, "mg/x") {
		t.Errorf("bare push with payload cwd = profile-less clone should allow mg/* (built-in default), got %v", wl)
	}

	session := choreProfileDir(t)
	baseSession := hookResolveBase{cwd: session, home: t.TempDir()}
	if wl := resolveTargetWhitelistFrom("", "", baseSession); hookMatch(wl, "mg/x") {
		t.Errorf("bare push with payload cwd = [chore/*] session should DENY mg/*, got %v", wl)
	}
}

// TestNorm_PayloadCwdOverride_WiresIntoResolveBase (Fix 3): resolveBaseOverride — what
// runHook installs from the payload's `cwd` — is what currentResolveBase() (and therefore
// the injected resolveTargetWhitelist) uses; a nil override falls back to os.Getwd()+$HOME
// with no regression.
func TestNorm_PayloadCwdOverride_WiresIntoResolveBase(t *testing.T) {
	// nil override → the process-cwd default.
	resolveBaseOverride = nil
	if got := currentResolveBase(); got.cwd != hookCwd() {
		t.Errorf("nil override: currentResolveBase().cwd = %q, want the process cwd %q", got.cwd, hookCwd())
	}

	session := choreProfileDir(t)
	override := hookResolveBase{cwd: session, home: t.TempDir()}
	resolveBaseOverride = &override
	defer func() { resolveBaseOverride = nil }()

	// The plain resolveTargetWhitelist (no explicit base) now reads the installed override:
	// a bare push resolves the [chore/*] session → mg denied.
	if wl := resolveTargetWhitelist("", ""); hookMatch(wl, "mg/x") {
		t.Errorf("with the payload override installed, a bare push should resolve the [chore/*] session (deny mg/*), got %v", wl)
	}
}

// TestParsePayloadCwd covers the one-field decode runHook uses to install the base.
func TestParsePayloadCwd(t *testing.T) {
	if got := parsePayloadCwd([]byte(`{"tool_name":"Bash","cwd":"/Users/x/Projects/p","tool_input":{}}`)); got != "/Users/x/Projects/p" {
		t.Errorf("parsePayloadCwd = %q, want the payload cwd", got)
	}
	if got := parsePayloadCwd([]byte(`{"tool_name":"Bash","tool_input":{}}`)); got != "" {
		t.Errorf("parsePayloadCwd on a payload with no cwd = %q, want empty", got)
	}
	if got := parsePayloadCwd([]byte(`not json`)); got != "" {
		t.Errorf("parsePayloadCwd on malformed input = %q, want empty (best-effort)", got)
	}
}

// TestNorm_SameProject_Regression: a session pushing its OWN repo (the target path resolves
// to its own [chore/*] profile) still DENIES mg/* and ALLOWS chore/* — identical to the
// pre-normalization same-project verdict.
func TestNorm_SameProject_Regression(t *testing.T) {
	own := choreProfileDir(t)
	base := hookResolveBase{cwd: own, home: t.TempDir()}

	// Explicit absolute self-target.
	wl := resolveTargetWhitelistFrom(own, "", base)
	if hookMatch(wl, "mg/anything") {
		t.Errorf("same-project [chore/*] must still DENY mg/*, got %v", wl)
	}
	if !hookMatch(wl, "chore/thing") {
		t.Errorf("same-project [chore/*] must still ALLOW chore/*, got %v", wl)
	}
	// Bare push (target == the session's own repo) — identical verdict.
	if bare := resolveTargetWhitelistFrom("", "", base); hookMatch(bare, "mg/x") {
		t.Errorf("same-project bare push must still DENY mg/*, got %v", bare)
	}
}

// TestNorm_ProtectedBranch_DeniedThroughAllPathForms: master is never on the resolved
// whitelist for ANY path form (tilde-cd, absolute-cd, relative, bare, nonexistent), so a
// protected-branch push is denied regardless of how the target was expressed — the
// built-in-default fallback never opens a protected branch either.
func TestNorm_ProtectedBranch_DeniedThroughAllPathForms(t *testing.T) {
	home := t.TempDir()
	session := choreProfileDir(t)
	clone := filepath.Join(home, "clone")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	base := hookResolveBase{cwd: session, home: home}

	forms := map[string]string{
		"tilde-cd real clone":  "~/clone",
		"absolute real clone":  clone,
		"relative nonexistent": "ghost-dir",
		"tilde nonexistent":    "~/ghost",
		"bare (session)":       "",
	}
	for name, raw := range forms {
		t.Run(name, func(t *testing.T) {
			wl := resolveTargetWhitelistFrom(raw, "", base)
			for _, protected := range []string{"master", "main", "develop", "release/1.2"} {
				// main is on the session profile? choreProfileDir sets only [chore/*], and
				// the built-in default is [feat/fix/chore/mg/user] — neither allows any of
				// these, so every form must deny every protected branch.
				if hookMatch(wl, protected) {
					t.Errorf("form %q: protected branch %q must be denied, got whitelist %v", name, protected, wl)
				}
			}
		})
	}
}

// hookMatch reuses the real guard glob (main-package view of hook.MatchWhitelist).
func hookMatch(wl []string, target string) bool { return wlHas(wl, target) }

// compile-time anchor so config stays imported if the assertions above change.
var _ = config.DefaultPushWhitelist
