package main

// push-guard target-repo whitelist resolution: proof that the push-guard hook judges a
// push by the whitelist that GOVERNS THE TARGET REPO — the repo actually being pushed —
// not the session-ambient profile the hook loaded at startup (the cross-repo ambient-profile
// bug). resolveTargetWhitelist is the real load-from-path resolver injected into the
// PURE internal/hook guard; these exercise it directly against t.TempDir profiles at
// distinct paths, plus one end-to-end pass through hook.Dispatch with the real resolver.
//
// HOME/XDG are sandboxed so a real ~/.config/leadkit/config.yaml (the USER layer) can
// never leak a push_whitelist into a "no project profile → built-in default" assertion.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hook"
)

// sandboxUserLayer points HOME/XDG_CONFIG_HOME at empty temp dirs so config.LoadFrom's
// USER layer contributes nothing — every whitelist here comes purely from the target
// project layer (or the built-in default), never the running machine's user config.
func sandboxUserLayer(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func writeTargetProfile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wlHas(wl []string, target string) bool { return hook.MatchWhitelist(target, wl) }

// TestResolveTargetWhitelist_AmbientProfileCrossRepo_NowAllowed is the ambient-profile case: the
// SESSION's own profile sets push_whitelist [chore/*], but the push targets a DIFFERENT
// project's clone (shared-infra) that has NO leadkit profile. The whitelist must
// resolve from the TARGET (→ built-in default, which includes mg/*), NOT the ambient
// [chore/*]. Denied before this fix, allowed after. resolveTargetWhitelist doesn't even
// take the ambient profile as an argument — the leak is structurally impossible now.
func TestResolveTargetWhitelist_AmbientProfileCrossRepo_NowAllowed(t *testing.T) {
	sandboxUserLayer(t)
	targetRepo := t.TempDir() // a clone with NO leadkit.yaml up-tree (under temp, not the project tree)

	wl := resolveTargetWhitelist(targetRepo, "git@github.com:acmecorp/shared-infra.git")

	if !wlHas(wl, "mg/example-proxy") {
		t.Errorf("mg/example-proxy not on the resolved whitelist %v — the ambient [chore/*] must NOT govern a cross-repo push", wl)
	}
	// And it is genuinely the built-in default, not some empty/ambient list.
	for _, want := range config.DefaultPushWhitelist {
		if !wlHas(wl, strings.Replace(want, "*", "x", 1)) {
			t.Errorf("resolved whitelist %v missing built-in pattern %q", wl, want)
		}
	}
	// Protected branches still blocked even on the fallback default.
	if wlHas(wl, "master") || wlHas(wl, "release/1.2") {
		t.Errorf("built-in default must still block protected branches, got %v", wl)
	}
}

// TestResolveTargetWhitelist_SameRepoStillDenied is the regression guard: a session
// pushing its OWN repo whose profile sets [chore/*] must STILL deny mg/* — the target IS
// that repo, so its own policy correctly applies. No behavior change for same-repo.
func TestResolveTargetWhitelist_SameRepoStillDenied(t *testing.T) {
	sandboxUserLayer(t)
	repo := t.TempDir()
	writeTargetProfile(t, repo, "queue_root: q\npush_whitelist:\n  - chore/*\n")

	wl := resolveTargetWhitelist(repo, "git@github.com:acmecorp/project-i.git")

	if wlHas(wl, "mg/anything") {
		t.Errorf("same-repo [chore/*] profile must still DENY mg/* — got whitelist %v", wl)
	}
	if !wlHas(wl, "chore/thing") {
		t.Errorf("same-repo [chore/*] profile must still ALLOW chore/* — got whitelist %v", wl)
	}
}

// TestResolveTargetWhitelist_TargetRepoAuthorityOverride proves the TARGET repo's own
// authority.<key>.push_whitelist override applies (resolved from the target profile via
// the shared deterministic MatchAuthorityKey), and a non-matching origin falls to the
// target's top-level default.
func TestResolveTargetWhitelist_TargetRepoAuthorityOverride(t *testing.T) {
	sandboxUserLayer(t)
	repo := t.TempDir()
	writeTargetProfile(t, repo, strings.Join([]string{
		"queue_root: q",
		"push_whitelist:",
		"  - chore/*",
		"authority:",
		"  acmecorp/widget:",
		"    push_whitelist:",
		"      - feat/*",
		"      - mg/*",
		"",
	}, "\n"))

	// Origin matches the authority key → the override governs (feat/*, mg/*).
	wl := resolveTargetWhitelist(repo, "git@github.com:acmecorp/widget.git")
	if !wlHas(wl, "mg/x") || !wlHas(wl, "feat/y") {
		t.Errorf("authority override should allow feat/* + mg/*, got %v", wl)
	}
	if wlHas(wl, "chore/z") {
		t.Errorf("authority override REPLACES the top-level list, so chore/* should be denied, got %v", wl)
	}

	// A non-matching origin → the target's top-level default [chore/*].
	wl2 := resolveTargetWhitelist(repo, "git@github.com:acmecorp/other.git")
	if !wlHas(wl2, "chore/z") || wlHas(wl2, "mg/x") {
		t.Errorf("non-matching origin should use the target top-level [chore/*], got %v", wl2)
	}
}

// TestResolveTargetWhitelist_FallbackIsBuiltinDefault proves the fail-safe fallback is
// the BUILT-IN default (never the ambient profile, never empty/allow-all): both a target
// with no leadkit.yaml up-tree AND a target whose discovered profile is malformed resolve
// to config.DefaultPushWhitelist — protected branches still blocked, feat/fix/chore/mg/user
// allowed.
func TestResolveTargetWhitelist_FallbackIsBuiltinDefault(t *testing.T) {
	sandboxUserLayer(t)

	t.Run("no profile", func(t *testing.T) {
		wl := resolveTargetWhitelist(t.TempDir(), "git@github.com:acmecorp/no-profile.git")
		assertBuiltinDefault(t, wl)
	})

	t.Run("malformed profile", func(t *testing.T) {
		repo := t.TempDir()
		// Unbalanced flow-sequence bracket → a hard YAML parse error, so LoadFrom
		// returns found=true with a non-nil error → the safe built-in default.
		writeTargetProfile(t, repo, "push_whitelist: [feat/*, mg/*\nqueue_root: q\n")
		wl := resolveTargetWhitelist(repo, "git@github.com:acmecorp/malformed.git")
		assertBuiltinDefault(t, wl)
	})

	t.Run("empty repoPath falls back to cwd walk-up, still safe", func(t *testing.T) {
		// From an arbitrary cwd with no project profile up-tree the result is still the
		// built-in default — never empty/allow-all. (cwd here is the test binary's dir,
		// which may or may not sit under a project; we only assert protected stays blocked
		// and the list is non-empty — the fail-closed invariant.)
		wl := resolveTargetWhitelist("", "")
		if len(wl) == 0 {
			t.Fatalf("empty resolver result — must never be empty/allow-all")
		}
		if wlHas(wl, "master") {
			t.Errorf("resolved whitelist must never allow a protected branch, got %v", wl)
		}
	})
}

func assertBuiltinDefault(t *testing.T, wl []string) {
	t.Helper()
	if wlHas(wl, "master") || wlHas(wl, "develop") || wlHas(wl, "release/9") {
		t.Errorf("fallback default must still block protected branches, got %v", wl)
	}
	for _, allowed := range []string{"feat/x", "fix/x", "chore/x", "mg/x", "user/x"} {
		if !wlHas(wl, allowed) {
			t.Errorf("fallback default should allow %q (built-in default), got %v", allowed, wl)
		}
	}
}

// TestPushGuardHook_CrossRepoEndToEnd wires the WHOLE thing: buildPushGuardConfig off a
// session-ambient profile whose push_whitelist is ONLY [chore/*], then dispatches a real
// `git -C <clone> push origin mg/example-proxy` where <clone> is a real git repo with a
// shared-infra origin and NO leadkit.yaml. The guard resolves the whitelist from
// the TARGET (→ built-in default) and ALLOWS mg/*, matching what config show/doctor would
// report from that clone — proving the ambient [chore/*] never leaks through the wiring.
func TestPushGuardHook_CrossRepoEndToEnd(t *testing.T) {
	sandboxUserLayer(t)
	clone, _ := gitRepoWithOneCommit(t, "git@github.com:acmecorp/shared-infra.git")

	ambient := config.Defaults()
	ambient.PushWhitelist = []string{"chore/*"} // the project-i session's own (legit) choice
	cfg := hook.Config{PushGuard: buildPushGuardConfig(&ambient)}

	payload := []byte(`{"tool_name":"Bash","tool_input":{"command":"git -C ` + clone + ` push origin mg/example-proxy"}}`)
	res := hook.Dispatch(hook.NamePushGuard, payload, cfg)
	if res.Stdout != "" || res.ExitCode != 0 {
		t.Fatalf("cross-repo mg/example-proxy push = %+v, want ALLOW (target repo → built-in default, not ambient [chore/*])", res)
	}

	// Sanity mirror: pushing to master (protected) is still DENIED under the same wiring.
	deny := []byte(`{"tool_name":"Bash","tool_input":{"command":"git -C ` + clone + ` push origin master"}}`)
	if r := hook.Dispatch(hook.NamePushGuard, deny, cfg); r.Stdout == "" {
		t.Errorf("push to master = %+v, want DENY (protected branch still blocked)", r)
	}
}
