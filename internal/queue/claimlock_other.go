//go:build !unix

package queue

// withClaimLock has no advisory lock to take on this platform; fn runs unguarded.
func withClaimLock(_ string, fn func() error) error { return fn() }
