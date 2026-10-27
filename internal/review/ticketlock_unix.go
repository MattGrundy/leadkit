//go:build unix

package review

import (
	"fmt"
	"os"
	"syscall"
)

// withTicketLock runs fn holding an exclusive advisory lock on the ticket file itself. Every
// leadkit writer of a ticket (a frontmatter rewrite, a note) takes it, so the two cannot
// interleave their read-modify-write. It does NOT stop a writer that ignores it, such as a shell
// redirect, which is why notes go through `leadkit task note`.
func withTicketLock(path string, fn func() error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("review: lock %s: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("review: lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
