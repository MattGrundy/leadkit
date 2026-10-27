//go:build !unix

package ticketstore

// withFileLock runs fn without a lock: this platform has no advisory file lock.
func withFileLock(_ string, fn func() error) error { return fn() }
