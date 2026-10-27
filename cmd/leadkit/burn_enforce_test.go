package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/lock"
	"mgpp.dev/leadkit/internal/queue"
)

var enforceNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
var enforceReset = enforceNow.Add(48 * time.Hour)

// fixedUsage returns a fetch func that always yields the given weekly %.
func fixedUsage(weekPct float64) func() (burn.Usage, error) {
	return func() (burn.Usage, error) {
		return burn.Usage{WeekPct: weekPct, WeekResetsAt: enforceReset}, nil
	}
}

// TestEnforceOnce_WritesPauseAtPausePct is the CORE spec proof: the daemon tick — with
// NO session having run `leadkit burn` — writes the account pause lock the moment real
// account weekly usage crosses pause_pct.
func TestEnforceOnce_WritesPauseAtPausePct(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")

	res := enforceOnce(lockPath, fixedUsage(98.0), 98.0, 90.0, enforceNow)
	if !res.Fetched {
		t.Fatal("Fetched=false on a successful fetch")
	}
	if res.Decision.Action != "LOCK" || !res.Locked {
		t.Fatalf("want LOCK + Locked, got action=%q locked=%v", res.Decision.Action, res.Locked)
	}
	locked, meta, err := lock.Status(lockPath)
	if err != nil || !locked {
		t.Fatalf("account lock not written: locked=%v err=%v", locked, err)
	}
	if meta.SetBy != "burn-enforce" {
		t.Errorf("lock set_by = %q, want burn-enforce", meta.SetBy)
	}
	if meta.Reason == "" {
		t.Error("lock reason must be recorded")
	}
}

// TestEnforceOnce_ClearsBelowResumePct: hysteresis low edge — an existing pause clears
// once weekly usage drops below resume_pct.
func TestEnforceOnce_ClearsBelowResumePct(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	if err := lock.Set(lockPath, lock.Meta{Reason: "pre-existing", SetBy: "burn-enforce", PausedAt: enforceNow}); err != nil {
		t.Fatal(err)
	}
	res := enforceOnce(lockPath, fixedUsage(89.9), 98.0, 90.0, enforceNow)
	if res.Decision.Action != "RESUME" || res.Locked {
		t.Fatalf("want RESUME + unlocked, got action=%q locked=%v", res.Decision.Action, res.Locked)
	}
	if locked, _, _ := lock.Status(lockPath); locked {
		t.Error("account lock should have been cleared")
	}
}

// TestEnforceOnce_HoldsBetween: hysteresis middle band — a lock between resume_pct and
// pause_pct is HELD (stays locked), and an unlocked account in that band is untouched.
func TestEnforceOnce_HoldsBetween(t *testing.T) {
	// Locked + in-band → HELD, stays locked.
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	if err := lock.Set(lockPath, lock.Meta{Reason: "held", SetBy: "burn-enforce", PausedAt: enforceNow}); err != nil {
		t.Fatal(err)
	}
	res := enforceOnce(lockPath, fixedUsage(94.0), 98.0, 90.0, enforceNow)
	if res.Decision.Action != "HELD" || !res.Locked {
		t.Fatalf("want HELD + locked, got action=%q locked=%v", res.Decision.Action, res.Locked)
	}
	if locked, _, _ := lock.Status(lockPath); !locked {
		t.Error("HELD must keep the lock in place")
	}

	// Unlocked + in-band → no change, stays unlocked.
	lockPath2 := filepath.Join(t.TempDir(), "PAUSE")
	res2 := enforceOnce(lockPath2, fixedUsage(94.0), 98.0, 90.0, enforceNow)
	if res2.Decision.Action != "" || res2.Locked {
		t.Fatalf("want no-change + unlocked, got action=%q locked=%v", res2.Decision.Action, res2.Locked)
	}
	if locked, _, _ := lock.Status(lockPath2); locked {
		t.Error("an unlocked in-band account must not be paused")
	}
}

// TestEnforceOnce_ReadFailureNeverWritesPause is the CRITICAL fail-safe: an unreadable
// account usage signal must NEVER write a pause (fail toward not-locking), only surface
// the error for logging. This is the account-wide-pause safety invariant.
func TestEnforceOnce_ReadFailureNeverWritesPause(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	failing := func() (burn.Usage, error) { return burn.Usage{}, errors.New("keychain unavailable") }

	res := enforceOnce(lockPath, failing, 98.0, 90.0, enforceNow)
	if res.Fetched {
		t.Fatal("Fetched must be false on a read failure")
	}
	if res.FetchErr == nil {
		t.Fatal("FetchErr must be set so the caller can log it")
	}
	if res.Decision.Action != "" {
		t.Errorf("no throttle decision may be taken on a read failure, got %q", res.Decision.Action)
	}
	if _, err := os.Lstat(lockPath); err == nil {
		t.Fatal("FAIL-SAFE VIOLATION: a usage-read failure wrote a pause lock")
	}
}

// TestEnforceOnce_ReadFailureDoesNotClearExistingPause: fail-safe also means a read
// failure must not RESUME a legitimate existing pause (leave state EXACTLY as-is).
func TestEnforceOnce_ReadFailureDoesNotClearExistingPause(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "PAUSE")
	if err := lock.Set(lockPath, lock.Meta{Reason: "real pause", SetBy: "burn-enforce", PausedAt: enforceNow}); err != nil {
		t.Fatal(err)
	}
	failing := func() (burn.Usage, error) { return burn.Usage{}, errors.New("network down") }
	_ = enforceOnce(lockPath, failing, 98.0, 90.0, enforceNow)
	if locked, _, _ := lock.Status(lockPath); !locked {
		t.Error("FAIL-SAFE VIOLATION: a read failure cleared an existing pause")
	}
}

// TestQueueWait_LockedOnDaemonWrittenAccountLock is the integration proof: an agent
// idling on `queue wait` returns LOCKED off the account lock the daemon wrote — even
// though this project's own lock is absent — so the whole account pauses without the
// session opting in.
func TestQueueWait_LockedOnDaemonWrittenAccountLock(t *testing.T) {
	roleDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(roleDir, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	accountLock := filepath.Join(t.TempDir(), "PAUSE")

	// The daemon tick writes the account lock (no session involved).
	res := enforceOnce(accountLock, fixedUsage(99.0), 98.0, 90.0, enforceNow)
	if res.Decision.Action != "LOCK" {
		t.Fatalf("setup: expected the tick to LOCK, got %q", res.Decision.Action)
	}

	projectLock := "" // this project itself is NOT locked
	wr, err := queue.Wait(roleDir, 500*time.Millisecond, 50*time.Millisecond, projectLock, accountLock)
	if err != nil {
		t.Fatal(err)
	}
	if wr.Kind != "LOCKED" {
		t.Fatalf("queue wait Kind = %q, want LOCKED (off the account lock)", wr.Kind)
	}
	if wr.Msg != accountLock {
		t.Errorf("LOCKED Msg = %q, want the account lock path %q", wr.Msg, accountLock)
	}
}
