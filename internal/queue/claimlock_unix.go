//go:build unix

package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withClaimLock runs fn holding an exclusive advisory lock for entry's claim, so reading who holds a
// ticket and writing a new holder cannot interleave with another owner doing the same.
func withClaimLock(entry string, fn func() error) error {
	path := claimLockFile(entry)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("lock claim on %s: %w", filepath.Base(entry), err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock claim on %s: %w", filepath.Base(entry), err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
