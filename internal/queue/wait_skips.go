package queue

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// waitSkipLog is where Wait reports what it is watching and which entries it declined to wake for.
// stderr, so it goes wherever the session that ran `queue wait` captures it (the only caller is the
// `queue wait` verb; the launchd daemons run `queue watch` and `queue sweep`, not wait), and a lane
// that looks stranded explains itself.
var waitSkipLog io.Writer = os.Stderr

const (
	// skipLogBuffer is how many lines may wait on a slow log before further ones are dropped.
	skipLogBuffer = 64
	// skipLogFlushWait bounds how long a finishing wait lingers for the log to drain.
	skipLogFlushWait = 250 * time.Millisecond
)

// skipReporter logs each (entry, reason) once per wait, so the interval re-check does not repeat it.
//
// The lines are written by a goroutine of their own, through a bounded buffer that drops when full: a
// write to a stderr pipe whose reader has stalled blocks, and if that write ran in the wait loop the
// waiter would stay alive and never re-check, which is the stall this log exists to explain. A
// diagnostic must not be able to cause it. Only the wait loop's goroutine calls into the reporter.
type skipReporter struct {
	who  Claimant
	seen map[string]bool
	out  io.Writer
	ch   chan string
	done chan struct{}
}

func newSkipReporter(who Claimant) *skipReporter {
	return &skipReporter{who: who, seen: map[string]bool{}, out: waitSkipLog}
}

// emit queues one line for the log and reports whether it was accepted; it never blocks.
func (r *skipReporter) emit(format string, args ...any) bool {
	if r.ch == nil {
		r.ch = make(chan string, skipLogBuffer)
		r.done = make(chan struct{})
		go func() {
			defer close(r.done)
			for line := range r.ch {
				fmt.Fprint(r.out, line)
			}
		}()
	}
	select {
	case r.ch <- fmt.Sprintf(format, args...):
		return true
	default:
		return false
	}
}

// close lets queued lines drain, for at most skipLogFlushWait: a log that will not drain is abandoned
// rather than holding the wait's result back.
func (r *skipReporter) close() {
	if r.ch == nil {
		return
	}
	close(r.ch)
	select {
	case <-r.done:
	case <-time.After(skipLogFlushWait):
	}
}

// watching logs, once, the absolute directory this wait is looking at. The path is printed as passed
// to the verb, usually relative, so without this a log cannot tell "nothing to do" from "looking at the
// wrong place".
func (r *skipReporter) watching(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	r.emit("queue wait: watching %s\n", abs)
}

// report logs every entry of pending/ and in-progress/ under dir that looks like work but that wait
// does not count, with the reason. Dotfiles are ignored, as wait ignores them.
func (r *skipReporter) report(dir string) {
	for _, state := range workStates {
		sd := filepath.Join(dir, state)
		for _, name := range listEntries(sd) {
			p := filepath.Join(sd, name)
			reason := r.skipReason(p, name, state)
			if reason == "" {
				continue
			}
			key := p + "\x00" + reason
			if r.seen[key] {
				continue
			}
			// Remembered only once queued: a line dropped for a full buffer is tried again next pass.
			if r.emit("queue wait: skipping %s: %s\n", p, reason) {
				r.seen[key] = true
			}
		}
	}
}

// skipReason is why wait does not wake for the entry, or "" when it is counted (or not worth saying).
func (r *skipReporter) skipReason(p, name, state string) string {
	if !strings.HasSuffix(name, ".md") {
		return "stray, not a .md ticket"
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "dangling or unreadable link"
	}
	if !fi.Mode().IsRegular() {
		return "not a regular file"
	}
	if blocked, on := ticketBlocked(p); blocked {
		return "status blocked (blocked_on: " + on + ")"
	}
	if terminal, st := ticketTerminal(p); terminal {
		return "terminal status " + st
	}
	if state == "in-progress" && r.who.By != "" {
		if c, held := ClaimedByOther(p, r.who.By, time.Now()); held {
			return "held by live claim of " + c.By
		}
	}
	return ""
}
