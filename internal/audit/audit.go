// Package audit is leadkit's read-only audit engine: two lenses over the operating
// model's task machinery, each returning findings — nothing here mutates anything.
// See docs/PORTABLE-OPERATING-KIT.md ("Audit") and OPERATING-MODEL.md (the task
// model). --apply (reaping, status rewrite) is a later, separate step.
package audit

import (
	"fmt"
	"strings"
)

// Finding categories.
const (
	CategoryOrphanedActive     = "orphaned-active"
	CategoryDanglingSymlink    = "dangling-symlink"
	CategoryStatusDrift        = "status-drift"
	CategoryDuplicateID        = "duplicate-id"
	CategoryNoncanonicalStatus = "noncanonical-status"
	CategorySessionMisuse      = "session-misuse"
)

// Finding severities.
const (
	SeverityWarn = "warn"
	SeverityInfo = "info"
)

// Categories lists every category, in the report's canonical display order.
var Categories = []string{
	CategoryOrphanedActive, CategoryDanglingSymlink, CategoryStatusDrift,
	CategoryDuplicateID, CategoryNoncanonicalStatus, CategorySessionMisuse,
}

// Finding is one audit observation.
type Finding struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	ID       string `json:"id"`
	Detail   string `json:"detail"`

	// RefSrc/RefPath, when RefPath is non-empty, resolve this finding to a single
	// browsable file: RefSrc selects which root ("" = the queue overlay, "store" =
	// the canonical store) RefPath is relative to — the same src/p pair
	// internal/viewer's /task route already takes. Left empty when a finding names
	// more than one file (duplicate-id) or names no file at all (session-misuse).
	RefSrc  string `json:"ref_src,omitempty"`
	RefPath string `json:"ref_path,omitempty"`
}

func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s: %s", f.Severity, f.ID, f.Detail)
}

// normalizeStatus lowercases and folds "-" to "_" so "in-progress" and "in_progress"
// compare equal — separator normalization is for COMPARISON only; findings report the
// original, unnormalized value.
func normalizeStatus(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}
