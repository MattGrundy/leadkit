// Package lock implements the throttle-triggered, token-free team pause: a lockfile
// at the profile's throttle.lock_file. Agents idle on an fsnotify watch of the
// lockfile's parent dir (zero tokens spent polling) and resume the moment it's
// removed. internal/burn sets the lock automatically at the usage throttle; the lead
// can also Set/Clear it by hand via `leadkit lock`/`leadkit unlock`.
//
// Agent + lead protocol (documented here, NOT wired anywhere by this package):
//   - Agent end-of-turn: `leadkit queue wait <inbox>` → if it returns LOCKED, run
//     `leadkit lock wait` → on UNLOCKED, loop back to `queue wait`. Fully
//     token-free while paused (see internal/queue.Wait's lockPath param).
//   - Lead: when burn emits a LOCK verdict, post a one-line notice to the team
//     (belt-and-suspenders + transcript visibility — the passive lock-aware wait
//     already pauses un-notified idle agents on its own). On RESUME, no action is
//     needed: agents wake from `lock wait` the moment the file is removed.
//
// See docs/PORTABLE-OPERATING-KIT.md.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// DefaultAccountLockPath is the ACCOUNT-LEVEL pause lock the burn-enforce daemon
// (docs/build-tasks/burn-auto-enforce.md) writes/clears from real account weekly usage.
// The throttle is account-scoped (the weekly budget is account-wide), so a single
// account-level lock — honored by `queue wait`/`lock wait` ALONGSIDE the per-project
// lock — pauses every session, including one whose project has no `.leadkit/` of its
// own. Kept in the same account-wide leadkit namespace as the workstream/project
// registries (~/.claude/leadkit/).
var DefaultAccountLockPath = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "PAUSE")

// Meta is the lockfile's content.
type Meta struct {
	Reason    string    `yaml:"reason"`
	SetBy     string    `yaml:"set_by"`
	PausedAt  time.Time `yaml:"paused_at"`
	PausePct  float64   `yaml:"pause_pct"`
	ResumePct float64   `yaml:"resume_pct"`
	// ResumeAt is informational only — the five_hour.resets_at from the usage
	// reading that triggered the lock, i.e. when the window resets regardless of
	// the resume_pct hysteresis.
	ResumeAt time.Time `yaml:"resume_at,omitempty"`
}

// Status reports whether path exists and, if so, its parsed Meta. A missing file is
// locked=false, err=nil — not finding a lock is the normal, expected case, not a
// failure. locked is still true on a parse error (the file's existence is the actual
// lock signal; a corrupt file is still a lock, just one whose metadata is unreadable)
// so callers can't mistake "can't parse" for "not locked".
func Status(path string) (locked bool, meta Meta, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return false, Meta{}, nil
		}
		return false, Meta{}, fmt.Errorf("lock: read %s: %w", path, rerr)
	}
	if uerr := yaml.Unmarshal(data, &meta); uerr != nil {
		return true, Meta{}, fmt.Errorf("lock: parse %s: %w", path, uerr)
	}
	return true, meta, nil
}

// Set writes meta to path atomically (temp file + rename), creating path's parent dir
// if needed. Idempotent-ish: if path is already locked, the EARLIEST paused_at is
// kept — the pause started when it first triggered, not when it was last refreshed —
// but the reason is updated to note the refresh, so a re-Set (e.g. burn's HELD tick
// re-confirming the lock) doesn't reset the clock or lose the original reason.
func Set(path string, meta Meta) error {
	if existed, existing, serr := Status(path); serr == nil && existed {
		meta.PausedAt = existing.PausedAt
		meta.Reason = fmt.Sprintf("%s (refreshed %s; was: %q)", meta.Reason, time.Now().UTC().Format(time.RFC3339), existing.Reason)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("lock: mkdir for %s: %w", path, err)
	}
	data, err := yaml.Marshal(meta)
	if err != nil {
		return fmt.Errorf("lock: marshal meta: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("lock: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("lock: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Clear removes path. Not finding it is not an error — clearing an already-cleared
// lock is a normal, idempotent no-op.
func Clear(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lock: remove %s: %w", path, err)
	}
	return nil
}

// Wait blocks at ZERO TOKEN COST — an fsnotify watch on path's parent dir, filtered
// to path itself, never a poll loop — until path is absent, then returns
// cleared=true. If already absent, returns immediately. On timeout, returns
// cleared=false (re-arm), the same contract as internal/queue.Wait's TIMEOUT.
func Wait(path string, timeout time.Duration) (cleared bool) {
	return WaitAll([]string{path}, timeout)
}

// AnyPresent reports whether any of paths currently exists (empty entries skipped) —
// the "am I paused right now?" check `queue wait`/`lock wait` run against BOTH the
// per-project lock and the account-level lock (docs/build-tasks/burn-auto-enforce.md).
func AnyPresent(paths ...string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

// WaitAll is Wait generalized to several lock paths: it blocks (zero token cost, an
// fsnotify watch on every distinct parent dir) until NONE of paths exists, then returns
// cleared=true. A session can be paused by its per-project lock OR the account-level
// burn-enforce lock (docs/build-tasks/burn-auto-enforce.md); it must stay paused until
// EVERY lock holding it is cleared, so returning only when all are absent is the correct
// resume condition. Empty path entries are ignored. If all are already absent, returns
// immediately; on timeout, cleared=false (re-arm), same contract as Wait.
func WaitAll(paths []string, timeout time.Duration) (cleared bool) {
	watch := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "" {
			watch = append(watch, p)
		}
	}
	if len(watch) == 0 || !AnyPresent(watch...) {
		return true
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return pollUntilAbsent(watch, timeout)
	}
	defer w.Close()

	for _, p := range watch {
		if err := w.Add(filepath.Dir(p)); err != nil {
			return pollUntilAbsent(watch, timeout)
		}
	}

	// A lock may have been removed in the gap between the initial check and the watch
	// being armed — re-check now that we're actually watching.
	if !AnyPresent(watch...) {
		return true
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		select {
		case _, ok := <-w.Events:
			if !ok {
				return pollUntilAbsent(watch, time.Until(deadline))
			}
			// Never trust the triggering event alone — re-run the full absence check
			// across every watched lock (a Remove of one lock while another still holds
			// must NOT resume).
			if !AnyPresent(watch...) {
				return true
			}
		case _, ok := <-w.Errors:
			if !ok {
				return pollUntilAbsent(watch, time.Until(deadline))
			}
		case <-time.After(remaining):
			return false
		}
	}
}

// pollUntilAbsent is WaitAll's fallback when fsnotify itself can't be set up (a watcher
// or Add failure) — not zero-cost, but still correct, and such failures are rare.
func pollUntilAbsent(paths []string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !AnyPresent(paths...) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}
