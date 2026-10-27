package lock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSet_WritesParseableMetaAndCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "PAUSE")
	meta := Meta{
		Reason: "session 98.2% ≥ throttle", SetBy: "burn",
		PausedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		PausePct: 98, ResumePct: 90,
	}
	if err := Set(path, meta); err != nil {
		t.Fatalf("Set: %v", err)
	}

	locked, got, err := Status(path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !locked {
		t.Fatal("want locked=true")
	}
	if got.Reason != meta.Reason || got.SetBy != meta.SetBy || got.PausePct != meta.PausePct || got.ResumePct != meta.ResumePct {
		t.Errorf("Status round-trip = %+v, want %+v", got, meta)
	}
	if !got.PausedAt.Equal(meta.PausedAt) {
		t.Errorf("PausedAt = %v, want %v", got.PausedAt, meta.PausedAt)
	}
}

func TestSet_KeepsEarliestPausedAtOnRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PAUSE")
	first := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if err := Set(path, Meta{Reason: "first", PausedAt: first}); err != nil {
		t.Fatalf("Set (first): %v", err)
	}

	later := first.Add(time.Hour)
	if err := Set(path, Meta{Reason: "refreshed reason", PausedAt: later}); err != nil {
		t.Fatalf("Set (refresh): %v", err)
	}

	_, got, err := Status(path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.PausedAt.Equal(first) {
		t.Errorf("PausedAt = %v, want the original %v to survive the refresh", got.PausedAt, first)
	}
}

func TestStatus_UnlockedWhenAbsent(t *testing.T) {
	locked, meta, err := Status(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Status: want nil error for an absent lock, got %v", err)
	}
	if locked {
		t.Error("want locked=false")
	}
	if meta != (Meta{}) {
		t.Errorf("want a zero-value Meta, got %+v", meta)
	}
}

func TestClear_IdempotentWhenAlreadyAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PAUSE")
	if err := Clear(path); err != nil {
		t.Fatalf("Clear on an absent lock: want nil error, got %v", err)
	}
}

func TestClear_RemovesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PAUSE")
	if err := Set(path, Meta{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := Clear(path); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	locked, _, err := Status(path)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if locked {
		t.Error("want locked=false after Clear")
	}
}

func TestWait_ReturnsImmediatelyWhenAlreadyAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	done := make(chan bool, 1)
	go func() { done <- Wait(path, 5*time.Second) }()
	select {
	case cleared := <-done:
		if !cleared {
			t.Error("want cleared=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return promptly for an already-absent lock")
	}
}

func TestWait_ReturnsClearedWhenFileIsRemovedDuringWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PAUSE")
	if err := Set(path, Meta{Reason: "x"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() { done <- Wait(path, 5*time.Second) }()

	// Give Wait a moment to arm its watch before removing the lock.
	time.Sleep(100 * time.Millisecond)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	select {
	case cleared := <-done:
		if !cleared {
			t.Error("want cleared=true after the lock is removed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not notice the lock being removed")
	}
}

func TestWait_TimeoutReturnsFalseWhileStillLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PAUSE")
	if err := Set(path, Meta{Reason: "x"}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	cleared := Wait(path, 100*time.Millisecond)
	elapsed := time.Since(start)

	if cleared {
		t.Error("want cleared=false — the lock was never removed")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Wait took %v, want it to return promptly after its ~100ms timeout", elapsed)
	}
}
