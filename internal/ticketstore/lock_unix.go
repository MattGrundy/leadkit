//go:build unix

package ticketstore

import (
	"os"
	"syscall"
)

// withFileLock runs fn holding an exclusive advisory lock on the file at path, the same lock
// internal/review's ticket writers take.
func withFileLock(path string, fn func() error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
