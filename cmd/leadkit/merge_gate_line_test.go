package main

// pr-merge-gate-verdict-visible: the review-gate verdict on `leadkit pr merge` must be
// UNMISSABLE in BOTH output paths (the dry-run plan AND the actual merge). The plan/verdict
// formatting is factored into the PURE formatMergeGateLine so both paths share ONE tested
// formatter (the CLI print path itself needs a forge). These assert the three cases the
// spec enumerates: a live 2/2-ready gate, a --user-approved substitution, and a not-ready
// gate (the refused case — its line is still rendered but the real-merge path never prints
// it since the decision is refused). Print-only hardening: NO governance/logic change to
// DecideMerge, whose tests stay untouched and green.

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/prcreate"
)

// case 1: 2/2-ready — merging on a grant with the auto-gate green (no --user-approved).
// The line must name the review-gate and show ok=true, NOT the old bare `gate:`.
func TestFormatMergeGateLine_ReadyShowsOKTrue(t *testing.T) {
	line := formatMergeGateLine(prcreate.DecideMergeInput{
		UserApproved: false,
		GateOK:       true,
		GateReason:   "all layers ready (code-review critique) for abc1234",
	})
	if !strings.Contains(line, "ok=true") {
		t.Fatalf("ready gate line must show ok=true, got %q", line)
	}
	if strings.Contains(line, "SUBSTITUTED") {
		t.Fatalf("a live 2/2 gate must NOT read as SUBSTITUTED, got %q", line)
	}
	if !strings.Contains(line, "all layers ready (code-review critique) for abc1234") {
		t.Fatalf("ready gate line must carry the gate reason, got %q", line)
	}
}

// case 2: --user-approved substitution — the line must say SUBSTITUTED and STILL show the
// underlying auto-gate ok=false, so a human substitution is never mistaken for a live 2/2.
func TestFormatMergeGateLine_UserApprovedSubstitution(t *testing.T) {
	line := formatMergeGateLine(prcreate.DecideMergeInput{
		UserApproved: true,
		GateOK:       false,
		GateReason:   "no review-gate 2/2 record",
	})
	if !strings.Contains(line, "SUBSTITUTED by --user-approved") {
		t.Fatalf("substitution line must say SUBSTITUTED by --user-approved, got %q", line)
	}
	if !strings.Contains(line, "ok=false") {
		t.Fatalf("substitution line must still show the underlying auto-gate ok=false, got %q", line)
	}
}

// case 3: not-ready (no --user-approved, auto-gate red) — this is the refused case, so the
// real-merge path never prints this line; the formatter still renders ok=false with the
// reason for the dry-run plan.
func TestFormatMergeGateLine_NotReadyShowsOKFalse(t *testing.T) {
	line := formatMergeGateLine(prcreate.DecideMergeInput{
		UserApproved: false,
		GateOK:       false,
		GateReason:   "layers not ready: critique=MISSING",
	})
	if !strings.Contains(line, "ok=false") {
		t.Fatalf("not-ready gate line must show ok=false, got %q", line)
	}
	if strings.Contains(line, "SUBSTITUTED") {
		t.Fatalf("a not-ready gate without --user-approved must NOT read as SUBSTITUTED, got %q", line)
	}
	if !strings.Contains(line, "layers not ready: critique=MISSING") {
		t.Fatalf("not-ready gate line must carry the gate reason, got %q", line)
	}
}

// an empty gate reason must never leave the review-gate line with a dangling empty clause.
func TestFormatMergeGateLine_EmptyReasonFallback(t *testing.T) {
	line := formatMergeGateLine(prcreate.DecideMergeInput{GateOK: true})
	if strings.Contains(line, "()") {
		t.Fatalf("empty gate reason must fall back to a descriptive clause, got %q", line)
	}
}
