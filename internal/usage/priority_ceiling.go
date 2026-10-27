package usage

// Account-scoped priority ceiling (priority-ceiling.md): closes session-
// priority-burn's self-promotion hole by capping the effective priority any
// session can claim, at a level only the ACCOUNT OWNER controls — never a
// project's leadkit.yaml (a session could edit its own project config to
// self-promote; that's the whole hole this closes). This file lives
// alongside the workstreams registry, both under ~/.claude/leadkit, but is
// NOT part of it: no project ever writes here, and internal/config.Profile
// carries no field for it.

import (
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/burn"
)

// DefaultPriorityCeilingPath is the account-scoped ceiling file's default
// location — the account owner (whoever controls this machine's ~/.claude)
// edits it directly; no leadkit command writes it.
var DefaultPriorityCeilingPath = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "priority-ceiling")

// PriorityCeilingEnvVar overrides the file when set (env wins over the file,
// same "env wins" convention every other leadkit override uses).
const PriorityCeilingEnvVar = "LEADKIT_PRIORITY_CEILING"

// ReadPriorityCeiling resolves the effective ceiling: $LEADKIT_PRIORITY_CEILING
// if set, else path's trimmed content, else burn.PriorityCritical (the
// top tier — meaning NO CLAMP, byte-identical to pre-ceiling session-
// priority-burn). NEVER an error and NEVER anything but a valid PriorityTier:
// a missing file, an unreadable one, empty content, or path=="" all fail
// OPEN to PriorityCritical — this is a governor guardrail, and an
// unreadable/garbage ceiling must never accidentally clamp every session in
// the account down to `low`.
func ReadPriorityCeiling(path string) burn.PriorityTier {
	if v := os.Getenv(PriorityCeilingEnvVar); v != "" {
		return burn.NormalizePriority(v)
	}
	if path == "" {
		return burn.PriorityCritical
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return burn.PriorityCritical
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return burn.PriorityCritical
	}
	return burn.NormalizePriority(s)
}
