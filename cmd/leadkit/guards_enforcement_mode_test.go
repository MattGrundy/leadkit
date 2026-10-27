package main

// Enforcement-mode resolution FROM THE TARGET profile (docs/build-tasks/
// configurable-guard-enforcement.md): a guard's mode comes from the governing profile of
// the repo/file it acts on — reusing the same guards-resolve-config-from-target resolution
// the whitelist/gate/owners policy use — NOT the session-ambient profile. These whitebox
// tests drive the real per-target mode resolvers against t.TempDir profiles at DISTINCT
// paths, proving: the mode follows the target, default_mode is the fallback, a per-guard
// override wins, and an unprofiled/unset target is advisory (backward-compatible fail-open).

import (
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hook"
)

func TestGuardMode_ResolvesFromTargetProfile(t *testing.T) {
	sandboxHome(t)

	// Target A: default_mode enforce → every guard enforces.
	enforceAll := writeProfileDir(t, "queue_root: queue\nguards:\n  default_mode: enforce\n")
	// Target B: only push_guard overridden to enforce; the rest inherit the (unset)
	// default_mode → advisory.
	pushOnly := writeProfileDir(t, "queue_root: queue\nguards:\n  push_guard:\n    mode: enforce\n")
	// Target C: no guards block at all → advisory everywhere.
	plain := writeProfileDir(t, "queue_root: queue\n")

	for _, g := range config.SecurityGuards {
		if got := resolveTargetGuardModeDir(enforceAll, g); got != hook.ModeEnforce {
			t.Errorf("target A default_mode=enforce: %s resolved %s, want enforce", g, got)
		}
		if got := resolveTargetGuardModeDir(plain, g); got != hook.ModeAdvisory {
			t.Errorf("target C no guards block: %s resolved %s, want advisory (backward-compat)", g, got)
		}
	}

	// Per-guard override: push_guard enforces, the others stay advisory — proving a
	// guard's mode is independent and not smeared from another guard's setting.
	if got := resolveTargetGuardModeDir(pushOnly, config.GuardPushGuard); got != hook.ModeEnforce {
		t.Errorf("target B push_guard override: resolved %s, want enforce", got)
	}
	if got := resolveTargetGuardModeDir(pushOnly, config.GuardPushGate); got != hook.ModeAdvisory {
		t.Errorf("target B push_gate (unset, no default): resolved %s, want advisory", got)
	}

	// The MODE FOLLOWS THE TARGET: resolving the SAME guard for two different targets
	// yields each target's own mode, never one session-ambient answer.
	if a, b := resolveTargetGuardModeDir(enforceAll, config.GuardPushGate), resolveTargetGuardModeDir(plain, config.GuardPushGate); a != hook.ModeEnforce || b != hook.ModeAdvisory {
		t.Errorf("mode-from-target: enforceAll push_gate=%s (want enforce), plain push_gate=%s (want advisory)", a, b)
	}
}

func TestGuardMode_UnprofiledTarget_IsAdvisory(t *testing.T) {
	sandboxHome(t)
	unprofiled := t.TempDir() // no leadkit.yaml → built-in default → guards.default_mode "" → advisory
	for _, g := range config.SecurityGuards {
		if got := resolveTargetGuardModeDir(unprofiled, g); got != hook.ModeAdvisory {
			t.Errorf("unprofiled target: %s resolved %s, want advisory (built-in default fail-open)", g, got)
		}
	}
}

func TestOwnerEnforcement_ResolvesModeAndConfiguredFromTarget(t *testing.T) {
	sandboxHome(t)

	// owners_file configured AND owner_guard enforce → (enforce, configured=true).
	enf := writeProfileDir(t, "queue_root: queue\nowners_file: .leadkit/owners.yaml\nguards:\n  owner_guard:\n    mode: enforce\n")
	mode, configured := resolveTargetOwnersEnforcement(enf + "/some/file.go")
	if mode != hook.ModeEnforce || !configured {
		t.Errorf("configured+enforce target: (mode=%s, configured=%v), want (enforce, true)", mode, configured)
	}

	// No owners_file → configured=false, so enforce mode never fails-closed (no boundary).
	noOwners := writeProfileDir(t, "queue_root: queue\nguards:\n  default_mode: enforce\n")
	mode2, configured2 := resolveTargetOwnersEnforcement(noOwners + "/some/file.go")
	if mode2 != hook.ModeEnforce || configured2 {
		t.Errorf("enforce-but-no-owners_file: (mode=%s, configured=%v), want (enforce, false)", mode2, configured2)
	}
}
