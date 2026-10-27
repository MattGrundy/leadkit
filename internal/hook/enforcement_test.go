package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/owners"
)

// Enforcement-mode tests (docs/build-tasks/configurable-guard-enforcement.md): each
// security guard's advisory (fail-open, today) vs enforce (fail-closed) posture. The
// contract under test, per the spec: ONLY the ambiguous/UNVERIFIABLE path flips — a clear
// positive allow still allows and a clear positive violation still denies in BOTH modes;
// only an error/unverifiable condition differs (advisory allows, enforce denies). advisory
// must be byte-identical to today (the existing guard tests are the advisory baseline).

func modeResolver(m GuardMode) func(string) GuardMode {
	return func(string) GuardMode { return m }
}

// --- push-guard ---------------------------------------------------------------------
//
// push-guard ALREADY fail-CLOSES on its unverifiable paths (a bare push whose HEAD can't
// be resolved, an unparseable refspec, a nil/empty whitelist) in BOTH modes — so its
// verdict is identical advisory-vs-enforce; the mode only NAMES the posture in the deny.
// These tests pin (a) the unverifiable path denies + names the mode in each mode, and
// (b) a clear allow and a clear violation are unchanged in both modes.

func TestPushGuard_Enforce_FailsClosedOnUnverifiable(t *testing.T) {
	noRemoteStub(t) // every runGit fails → a bare push's HEAD is unresolvable → unverifiable
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		cfg := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), Mode: modeResolver(mode)}
		res := PushGuard(pushInput("git push"), cfg)
		denied := res.ExitCode == 0 && res.Stdout != ""
		if !denied {
			t.Fatalf("mode=%s: bare push with unresolvable HEAD = %+v, want DENY (fail-closed on unverifiable)", mode, res)
		}
		if !strings.Contains(res.Stdout, "mode: "+mode.String()) {
			t.Errorf("mode=%s: deny reason should name the mode, got:\n%s", mode, res.Stdout)
		}
	}
}

func TestPushGuard_ClearAllowAndViolation_UnchangedByMode(t *testing.T) {
	noRemoteStub(t)
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		cfg := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), Mode: modeResolver(mode)}
		// Clear positive allow: an explicit whitelisted refspec → allow in BOTH modes.
		if res := PushGuard(pushInput("git push origin feat/x"), cfg); res.ExitCode != 0 || res.Stdout != "" {
			t.Errorf("mode=%s: whitelisted feat/x = %+v, want ALLOW in both modes", mode, res)
		}
		// Clear positive violation: a protected branch → deny in BOTH modes.
		res := PushGuard(pushInput("git push origin master"), cfg)
		if !(res.ExitCode == 0 && res.Stdout != "") {
			t.Errorf("mode=%s: master push = %+v, want DENY in both modes", mode, res)
		}
	}
}

// --- push-gate ----------------------------------------------------------------------
//
// push-gate is a genuine advisory→enforce FLIP: a push it can't evaluate (bad repo path /
// unresolvable HEAD / unresolvable target scope) fails OPEN today, and fails CLOSED under
// enforce. A clear gated allow, a legitimate out-of-scope allow, and a clear ungated deny
// are identical in both.

// unresolvableHeadGate is in-scope-capable but HEAD won't resolve → OutcomeUnverifiable.
func unresolvableHeadGate(t *testing.T, repo string) *gate.Gate {
	t.Helper()
	return &gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{repo: enforcedRemote}, head: ""},
		EnforcedRemotes: []string{enforcedSubstr},
	}
}

func TestPushGate_Enforce_FailsClosedOnUnverifiable(t *testing.T) {
	repo := t.TempDir()

	// advisory: unverifiable push fails OPEN (allow) — byte-identical to today.
	adv := PushGateConfig{ResolveGate: staticGate(unresolvableHeadGate(t, repo)), Cwd: repo, Mode: modeResolver(ModeAdvisory)}
	assertAllow(t, PushGate(bashInput("git push"), adv), "advisory: unresolvable HEAD fails open")
	// A nil Mode resolver must behave exactly like advisory (backward-compat).
	nilMode := PushGateConfig{ResolveGate: staticGate(unresolvableHeadGate(t, repo)), Cwd: repo}
	assertAllow(t, PushGate(bashInput("git push"), nilMode), "nil mode == advisory fail-open")

	// enforce: the SAME unverifiable push fails CLOSED (deny) + names the mode.
	enf := PushGateConfig{ResolveGate: staticGate(unresolvableHeadGate(t, repo)), Cwd: repo, Mode: modeResolver(ModeEnforce)}
	res := PushGate(bashInput("git push"), enf)
	assertDeny(t, res, "enforce: unresolvable HEAD fails closed")
	if !strings.Contains(res.Stdout, "mode: enforce") {
		t.Errorf("enforce deny should name the mode, got:\n%s", res.Stdout)
	}
}

func TestPushGate_Enforce_FailsClosedOnUnresolvableScope(t *testing.T) {
	repo := t.TempDir()
	// HEAD resolves, but the target's origin URL is "" → scope can't be confirmed →
	// OutcomeUnverifiable (an EXPLICIT enforced_remotes list has nothing to match against).
	mkGate := func() *gate.Gate {
		return &gate.Gate{
			RecordsDir:      t.TempDir(),
			Git:             fakeGateGit{remotes: map[string]string{repo: ""}, head: gateTestSHA},
			EnforcedRemotes: []string{enforcedSubstr},
		}
	}
	assertAllow(t, PushGate(bashInput("git push"), PushGateConfig{ResolveGate: staticGate(mkGate()), Cwd: repo, Mode: modeResolver(ModeAdvisory)}),
		"advisory: unresolvable scope fails open")
	assertDeny(t, PushGate(bashInput("git push"), PushGateConfig{ResolveGate: staticGate(mkGate()), Cwd: repo, Mode: modeResolver(ModeEnforce)}),
		"enforce: unresolvable scope fails closed")
}

func TestPushGate_ClearVerdicts_UnchangedByMode(t *testing.T) {
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		// Clear ungated (in scope, no record) → DENY in both modes.
		repo := t.TempDir()
		cfg, _ := pushGateEnv(t, repo, enforcedRemote)
		cfg.Mode = modeResolver(mode)
		assertDeny(t, PushGate(bashInput("git push"), cfg), "ungated in-scope denies in "+mode.String())

		// Clear gated (full passing record) → ALLOW in both modes.
		repo2 := t.TempDir()
		cfg2, recordsDir := pushGateEnv(t, repo2, enforcedRemote)
		cfg2.Mode = modeResolver(mode)
		writeFullRecord(t, recordsDir, gateTestSHA)
		assertAllow(t, PushGate(bashInput("git push"), cfg2), "gated allows in "+mode.String())

		// Legitimate out-of-scope (origin resolved, not enforced) → ALLOW in both, even enforce.
		repo3 := t.TempDir()
		outGate := &gate.Gate{
			RecordsDir:      t.TempDir(),
			Git:             fakeGateGit{remotes: map[string]string{repo3: "git@github.com:other/unrelated.git"}, head: gateTestSHA},
			EnforcedRemotes: []string{enforcedSubstr},
		}
		outCfg := PushGateConfig{ResolveGate: staticGate(outGate), Cwd: repo3, Mode: modeResolver(mode)}
		assertAllow(t, PushGate(bashInput("git push"), outCfg), "out-of-scope allows in "+mode.String())
	}
}

// --- owner-guard --------------------------------------------------------------------
//
// owner-guard is a genuine advisory→enforce FLIP on the UNVERIFIABLE path only: an
// owners_file that is CONFIGURED but unreadable/unparseable (ResolvePolicy loaded=false,
// ownersConfigured=true) fails OPEN today, fails CLOSED under enforce. A project with NO
// owners_file has no boundary → allow in both. A clear violation and a clear allow are
// unchanged in both.

func ownerEnforcement(m GuardMode, configured bool) func(string) (GuardMode, bool) {
	return func(string) (GuardMode, bool) { return m, configured }
}

func TestOwnerGuard_Enforce_FailsClosedOnUnreadablePolicy(t *testing.T) {
	unreadable := func(string) (*owners.Policy, bool) { return nil, false } // configured but won't load

	// advisory: boundary disabled → allow (today), even for a would-be hard-blocked path.
	adv := OwnerGuardConfig{
		ResolvePolicy: unreadable, Home: "/home/me", Pwd: "/home/me",
		Enforcement: ownerEnforcement(ModeAdvisory, true),
	}
	if got := OwnerGuard(ownerEdit("secrets/key.pem"), adv); got != allow {
		t.Errorf("advisory: unreadable owners policy → allow (fail-open), got %+v", got)
	}
	// nil Enforcement resolver behaves as advisory (backward-compat).
	nilEnf := OwnerGuardConfig{ResolvePolicy: unreadable, Home: "/home/me", Pwd: "/home/me"}
	if got := OwnerGuard(ownerEdit("secrets/key.pem"), nilEnf); got != allow {
		t.Errorf("nil Enforcement == advisory fail-open, got %+v", got)
	}

	// enforce: the SAME unreadable-but-configured policy → DENY (exit 2) + names the mode.
	enf := OwnerGuardConfig{
		ResolvePolicy: unreadable, Home: "/home/me", Pwd: "/home/me",
		Enforcement: ownerEnforcement(ModeEnforce, true),
	}
	got := OwnerGuard(ownerEdit("secrets/key.pem"), enf)
	if got.ExitCode != 2 || got.Stderr == "" {
		t.Fatalf("enforce: unreadable owners policy = %+v, want DENY exit 2 (fail-closed)", got)
	}
	if !strings.Contains(got.Stderr, "mode: enforce") {
		t.Errorf("enforce deny should name the mode, got: %q", got.Stderr)
	}
}

func TestOwnerGuard_Enforce_NoOwnersFile_StillAllows(t *testing.T) {
	// owners_file NOT configured (ownersConfigured=false): no boundary exists → allow even
	// in enforce (a clear no-policy allow, NOT an unverifiable condition).
	cfg := OwnerGuardConfig{
		ResolvePolicy: func(string) (*owners.Policy, bool) { return nil, false },
		Home:          "/home/me", Pwd: "/home/me",
		Enforcement: ownerEnforcement(ModeEnforce, false),
	}
	if got := OwnerGuard(ownerEdit("secrets/key.pem"), cfg); got != allow {
		t.Errorf("enforce + no owners_file → allow (no boundary), got %+v", got)
	}
}

func TestOwnerGuard_ClearVerdicts_UnchangedByMode(t *testing.T) {
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		// Clear violation: a non-owner editing an owned path → DENY in both modes.
		deny := OwnerGuard(ownerEdit("Projects/repo/frontend/app.tsx"), OwnerGuardConfig{
			ResolvePolicy: staticPolicy(ownerPolicy()), Caller: "backend-agent", Lead: "lead",
			Home: "/home/me", Pwd: "/home/me", Enforcement: ownerEnforcement(mode, true),
		})
		if deny.ExitCode != 2 {
			t.Errorf("mode=%s: non-owner edit = %+v, want DENY", mode, deny)
		}
		if !strings.Contains(deny.Stderr, "mode: "+mode.String()) {
			t.Errorf("mode=%s: deny should name the mode, got %q", mode, deny.Stderr)
		}
		// Clear allow: the owner editing its own path → ALLOW in both modes.
		ok := OwnerGuard(ownerEdit("Projects/repo/frontend/app.tsx"), OwnerGuardConfig{
			ResolvePolicy: staticPolicy(ownerPolicy()), Caller: "fe-agent", Lead: "lead",
			Home: "/home/me", Pwd: "/home/me", Enforcement: ownerEnforcement(mode, true),
		})
		if ok != allow {
			t.Errorf("mode=%s: owner on own path = %+v, want ALLOW", mode, ok)
		}
	}
}
