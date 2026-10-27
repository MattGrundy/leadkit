//go:build unix

package ticketstore

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestCommitWaitsForAHeldFileLock(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Update(p, set("priority", "P1"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Update finished while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := ReadFile(p).Doc.Value("priority"); v != "P1" {
		t.Errorf("priority = %q after the lock was released", v)
	}
}
