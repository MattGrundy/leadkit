package lock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAnyPresent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "A")
	b := filepath.Join(dir, "B")
	if AnyPresent(a, b, "") {
		t.Fatal("nothing written yet — AnyPresent should be false")
	}
	if err := os.WriteFile(b, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !AnyPresent(a, b) {
		t.Fatal("B exists — AnyPresent should be true")
	}
	if !AnyPresent("", b) {
		t.Fatal("empty entries must be skipped, B still present")
	}
}

// TestWaitAll_ReturnsImmediatelyWhenAllAbsent: the resume condition is ALL locks gone.
func TestWaitAll_ReturnsImmediatelyWhenAllAbsent(t *testing.T) {
	dir := t.TempDir()
	if !WaitAll([]string{filepath.Join(dir, "A"), filepath.Join(dir, "B")}, time.Second) {
		t.Fatal("all absent — WaitAll should return cleared=true immediately")
	}
	// Empty / all-empty input is treated as "nothing to wait on".
	if !WaitAll([]string{"", ""}, time.Second) {
		t.Fatal("no real paths — WaitAll should return cleared=true")
	}
}

// TestWaitAll_TimesOutWhileAnyPresent: a still-held lock keeps the waiter paused; it
// re-arms (cleared=false) on timeout rather than falsely resuming.
func TestWaitAll_TimesOutWhileAnyPresent(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "HELD")
	if err := os.WriteFile(held, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if WaitAll([]string{filepath.Join(dir, "GONE"), held}, 150*time.Millisecond) {
		t.Fatal("one lock still held — WaitAll must NOT report cleared (no false resume)")
	}
}
