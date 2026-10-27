package queue

import (
	"time"

	"golang.org/x/sys/unix"
)

// chtimesNoDeref bumps path's own mtime WITHOUT following it if it is a symlink —
// the `touch -h` semantics queue-sweep.sh's --poke relies on. Poking a pending/
// entry must touch the symlink itself so a watcher on the pending/ dir sees the
// event; dereferencing would instead touch the canonical store file (outside any
// watched dir) and fire nothing. Best-effort: the shell version redirects errors
// to /dev/null, so a failure here (e.g. a dangling link) is swallowed too.
func chtimesNoDeref(path string) {
	now := time.Now()
	tv := unix.NsecToTimeval(now.UnixNano())
	_ = unix.Lutimes(path, []unix.Timeval{tv, tv})
}
