//go:build !unix

package review

// withTicketLock has no advisory lock to take on this platform; fn runs unguarded.
func withTicketLock(_ string, fn func() error) error { return fn() }
