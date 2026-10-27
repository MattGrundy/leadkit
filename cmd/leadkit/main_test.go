package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/lock"
	"mgpp.dev/leadkit/internal/queue"
)

func TestHumanOutputLines_IncludesGuardFooter(t *testing.T) {
	lines := humanOutputLines("HOLD at 3 workstreams", 3, 3, 0)
	if len(lines) != 3 {
		t.Fatalf("want verdict + guard footer + accounting line (3 lines), got %d: %v", len(lines), lines)
	}
	if lines[0] != "HOLD at 3 workstreams" {
		t.Fatalf("want verdict line first, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "GUARDS:") {
		t.Fatalf("want a GUARDS footer, got %q", lines[1])
	}
	for _, must := range []string{"reuse standing team", "TASK FILES", "queue-move", "not session tasks"} {
		if !strings.Contains(lines[1], must) {
			t.Errorf("footer missing expected phrase %q: %q", must, lines[1])
		}
	}
}

func TestHumanOutputLines_IncludesAccountingLine(t *testing.T) {
	lines := humanOutputLines("HOLD at 3 workstreams", 3, 3, 0)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[2], "ACCOUNTING:") {
		t.Fatalf("want an ACCOUNTING line third, got %q", lines[2])
	}
	for _, must := range []string{
		"WORKSTREAMS, not agents",
		"1 workstream = 1 owner actively working a task",
		"Reviewers, watchers, pollers, the orchestrator",
		"NOT counted",
		"never kill or idle them",
		"n workstreams ≠ n agents",
	} {
		if !strings.Contains(lines[2], must) {
			t.Errorf("accounting line missing expected phrase %q: %q", must, lines[2])
		}
	}
}

func TestHumanOutputLines_NoOtherProjects_ExactlyThreeLines(t *testing.T) {
	// Backward-compat: single-project (otherProjectsN==0) must print EXACTLY the
	// original three lines, no cross-project note appended.
	lines := humanOutputLines("HOLD at 3 workstreams", 3, 3, 0)
	if len(lines) != 3 {
		t.Fatalf("want exactly 3 lines with no cross-project data, got %d: %v", len(lines), lines)
	}
}

func TestHumanOutputLines_OtherProjectsPresent_AppendsAccountingNote(t *testing.T) {
	lines := humanOutputLines("HOLD at 1 workstreams — headroom claimed elsewhere", 8, 1, 7)
	if len(lines) != 4 {
		t.Fatalf("want a 4th cross-project accounting line, got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[3], "ACCOUNTING:") {
		t.Errorf("want the 4th line to be an ACCOUNTING note, got %q", lines[3])
	}
	for _, must := range []string{"+7", "account total 8", "own N: 1"} {
		if !strings.Contains(lines[3], must) {
			t.Errorf("accounting note missing %q: %q", must, lines[3])
		}
	}
}

func TestJSONOutput_DoesNotCarryGuardFooter(t *testing.T) {
	out := jsonOutput{N: 3, NPrev: 3, Verdict: "HOLD at 3 workstreams"}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "GUARDS") || strings.Contains(string(b), "ACCOUNTING") {
		t.Fatalf("JSON output must stay machine-only, got %s", b)
	}
}

func TestJSONOutput_CarriesWorkstreamsUnit(t *testing.T) {
	out := jsonOutput{N: 3, Unit: "workstreams", NPrev: 3, Verdict: "HOLD at 3 workstreams"}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"unit":"workstreams"`) {
		t.Fatalf("want unit:\"workstreams\" in the JSON, got %s", b)
	}
}

var resetsAtFixture = time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)

func TestApplyThrottle_SessionAtOrOverPausePctSetsLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	nowLocked, decision, statusErr, actionErr := applyThrottle(lockPath, 98.5, 98.0, 90.0, resetsAtFixture, now)
	if statusErr != nil || actionErr != nil {
		t.Fatalf("statusErr=%v actionErr=%v, want both nil", statusErr, actionErr)
	}
	if decision.Action != "LOCK" {
		t.Fatalf("Action = %q, want LOCK", decision.Action)
	}
	if !nowLocked {
		t.Error("want nowLocked=true")
	}

	locked, meta, err := lock.Status(lockPath)
	if err != nil {
		t.Fatalf("lock.Status: %v", err)
	}
	if !locked {
		t.Fatal("want the lockfile to actually exist on disk")
	}
	if meta.SetBy != "burn" || !meta.PausedAt.Equal(now) || meta.PausePct != 98.0 || meta.ResumePct != 90.0 {
		t.Errorf("meta = %+v, unexpected", meta)
	}
}

func TestApplyThrottle_LockedAndUnderResumePctClears(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if err := lock.Set(lockPath, lock.Meta{Reason: "was locked", SetBy: "burn", PausedAt: now}); err != nil {
		t.Fatal(err)
	}

	nowLocked, decision, statusErr, actionErr := applyThrottle(lockPath, 50.0, 98.0, 90.0, resetsAtFixture, now)
	if statusErr != nil || actionErr != nil {
		t.Fatalf("statusErr=%v actionErr=%v, want both nil", statusErr, actionErr)
	}
	if decision.Action != "RESUME" {
		t.Fatalf("Action = %q, want RESUME", decision.Action)
	}
	if nowLocked {
		t.Error("want nowLocked=false")
	}
	if locked, _, err := lock.Status(lockPath); err != nil || locked {
		t.Errorf("lock.Status: locked=%v err=%v, want unlocked", locked, err)
	}
}

func TestApplyThrottle_LockedInBetweenStaysLockedHeld(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	original := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	if err := lock.Set(lockPath, lock.Meta{Reason: "original reason", SetBy: "burn", PausedAt: original}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	nowLocked, decision, statusErr, actionErr := applyThrottle(lockPath, 93.0, 98.0, 90.0, resetsAtFixture, now)
	if statusErr != nil || actionErr != nil {
		t.Fatalf("statusErr=%v actionErr=%v, want both nil", statusErr, actionErr)
	}
	if decision.Action != "HELD" {
		t.Fatalf("Action = %q, want HELD", decision.Action)
	}
	if !nowLocked {
		t.Error("want nowLocked=true (still locked)")
	}

	// HELD must not have touched the lock file at all — same original PausedAt/reason.
	_, meta, err := lock.Status(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.PausedAt.Equal(original) || meta.Reason != "original reason" {
		t.Errorf("HELD mutated the lock: meta = %+v, want it untouched (PausedAt=%v, Reason=%q)", meta, original, "original reason")
	}
}

func TestApplyThrottle_NotLockedUnderPausePctNoChange(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	nowLocked, decision, statusErr, actionErr := applyThrottle(lockPath, 5.0, 98.0, 90.0, resetsAtFixture, now)
	if statusErr != nil || actionErr != nil {
		t.Fatalf("statusErr=%v actionErr=%v, want both nil", statusErr, actionErr)
	}
	if decision.Action != "" {
		t.Fatalf("Action = %q, want no action", decision.Action)
	}
	if nowLocked {
		t.Error("want nowLocked=false")
	}
	if locked, _, err := lock.Status(lockPath); err != nil || locked {
		t.Errorf("lock.Status: locked=%v err=%v, want unlocked (never created)", locked, err)
	}
}

func TestConfirmGuard_NeitherFlag_DryRun(t *testing.T) {
	if got := confirmGuard(false, false); got != guardDryRun {
		t.Errorf("confirmGuard(false, false) = %v, want guardDryRun", got)
	}
}

func TestConfirmGuard_ConfirmAloneWithoutApply_StillDryRun(t *testing.T) {
	// --confirm without --apply is meaningless, not an accidental mutate path.
	if got := confirmGuard(false, true); got != guardDryRun {
		t.Errorf("confirmGuard(false, true) = %v, want guardDryRun", got)
	}
}

func TestConfirmGuard_ApplyAlone_Refused(t *testing.T) {
	// This is the exact incident: a bare --apply (Go parses it true with no
	// value) must never mutate — it refuses instead.
	if got := confirmGuard(true, false); got != guardRefused {
		t.Errorf("confirmGuard(true, false) = %v, want guardRefused", got)
	}
}

func TestConfirmGuard_ApplyAndConfirm_Mutates(t *testing.T) {
	if got := confirmGuard(true, true); got != guardMutate {
		t.Errorf("confirmGuard(true, true) = %v, want guardMutate", got)
	}
}

func TestResolveDefaultQueueRoot_UsesProfileQueueRootWhenLEADKIT_QUEUEUnset(t *testing.T) {
	t.Setenv("LEADKIT_QUEUE", "")
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: my-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profilePath)

	want := filepath.Join(dir, "my-queue")
	got := resolveDefaultQueueRoot()
	if got != want {
		t.Errorf("resolveDefaultQueueRoot() = %q, want the profile's resolved queue_root %q", got, want)
	}
}

func TestResolveDefaultQueueRoot_LEADKIT_QUEUEStillOverridesTheProfile(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: my-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profilePath)

	override := filepath.Join(t.TempDir(), "env-queue")
	t.Setenv("LEADKIT_QUEUE", override)

	got := resolveDefaultQueueRoot()
	if got != override {
		t.Errorf("resolveDefaultQueueRoot() = %q, want $LEADKIT_QUEUE override %q", got, override)
	}
}

// --- resolveQueueAndStore (profile-store-resolution.md): the shared helper
// `review send`/`gate verdict`/`queue move` all resolve queue_root+store_root
// through. ---

func TestResolveQueueAndStore_UsesProfileQueueAndStoreRoot(t *testing.T) {
	t.Setenv("LEADKIT_QUEUE", "")
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: my-queue\nstore_root: my-tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profilePath)

	queueRoot, storeRoot := resolveQueueAndStore(loadProfileOrDefaults())
	wantQueue := filepath.Join(dir, "my-queue")
	wantStore := filepath.Join(dir, "my-tasks")
	if queueRoot != wantQueue || storeRoot != wantStore {
		t.Errorf("resolveQueueAndStore = (%q, %q), want (%q, %q) — the adopter regression", queueRoot, storeRoot, wantQueue, wantStore)
	}
}

// TestResolveQueueAndStore_NoProfileByteIdenticalToBuiltinDefault is the
// no-config safety test: with no leadkit.yaml found and no $LEADKIT_QUEUE,
// resolution MUST match the built-in default — queue.DefaultRoot() (now the
// cwd-relative ./queue) for the queue, its sibling "tasks" dir for the store.
func TestResolveQueueAndStore_NoProfileByteIdenticalToBuiltinDefault(t *testing.T) {
	t.Setenv("LEADKIT_QUEUE", "")
	t.Setenv("LEADKIT_PROFILE", filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	queueRoot, storeRoot := resolveQueueAndStore(loadProfileOrDefaults())

	wantQueue := queue.DefaultRoot()
	wantStore := filepath.Join(filepath.Dir(wantQueue), "tasks")
	if queueRoot != wantQueue {
		t.Errorf("queueRoot = %q, want queue.DefaultRoot() %q (byte-identical, #123 safety)", queueRoot, wantQueue)
	}
	if storeRoot != wantStore {
		t.Errorf("storeRoot = %q, want the sibling tasks dir %q (byte-identical, #123 safety)", storeRoot, wantStore)
	}
}

func TestResolveQueueAndStore_LEADKIT_QUEUEOverridesQueueRootNotProfileStoreRoot(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: my-queue\nstore_root: my-tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profilePath)
	override := filepath.Join(t.TempDir(), "env-queue")
	t.Setenv("LEADKIT_QUEUE", override)

	queueRoot, storeRoot := resolveQueueAndStore(loadProfileOrDefaults())
	if queueRoot != override {
		t.Errorf("queueRoot = %q, want the $LEADKIT_QUEUE override %q", queueRoot, override)
	}
	wantStore := filepath.Join(dir, "my-tasks")
	if storeRoot != wantStore {
		t.Errorf("storeRoot = %q, want the profile's own store_root %q — unaffected by $LEADKIT_QUEUE", storeRoot, wantStore)
	}
}

// TestResolveQueueAndStore_QueueMoveAgreesWithReviewSendAndGateVerdict is the
// shared-helper regression: `queue move`'s own flag default (resolveDefaultQueueRoot)
// must resolve the SAME queue root resolveQueueAndStore gives `review send`/`gate
// verdict`, for the same profile+env — no divergent resolution left behind.
func TestResolveQueueAndStore_QueueMoveAgreesWithReviewSendAndGateVerdict(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: my-queue\nstore_root: my-tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", profilePath)
	t.Setenv("LEADKIT_QUEUE", "")

	queueMoveRoot := resolveDefaultQueueRoot()
	queueRoot, _ := resolveQueueAndStore(loadProfileOrDefaults())
	if queueMoveRoot != queueRoot {
		t.Errorf("queue move resolves %q, review send/gate verdict resolve %q — must agree", queueMoveRoot, queueRoot)
	}
}
