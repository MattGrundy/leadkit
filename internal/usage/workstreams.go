package usage

// The account-wide workstream registry (docs/build-tasks/cross-project-burn.md):
// burn's usage numbers (session/weekly %) are already account-wide, but its
// workstream count N was per-project — two teams on one account each scaling
// independently into the same shared headroom. This file is the small, per-project
// registry burn.Input.AccountN is summed from: one JSON file per project under a
// shared, account-wide directory, keyed by that project's daemon slug (see
// internal/install.ResolveProjectSlug — REUSE that exact function for the key,
// computed by the caller and passed in here; this package does not import
// internal/install, so a project's daemon key and its burn key can never drift apart
// by having two different slugging implementations).
//
// One writer per file (the project that owns that slug) avoids cross-project write
// contention entirely — every project only ever touches its own <slug>.json.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultWorkstreamsDir is the account-wide directory every project's workstream-count
// file lives under.
var DefaultWorkstreamsDir = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "workstreams")

// WorkstreamEntry is one project's live workstream-count declaration.
type WorkstreamEntry struct {
	Slug      string    `json:"slug"`
	N         int       `json:"n"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WriteWorkstreamEntry atomically writes this project's <slug>.json into dir. slug==""
// is a no-op (success, nothing written) — an unresolvable slug has no stable identity
// to key a shared file on, and writing under a bare/ambiguous name would risk exactly
// the cross-project collision this registry exists to prevent (mirrors
// internal/install's daemon-namespacing fallback: unresolvable -> stay out of the
// shared namespace entirely, don't guess).
func WriteWorkstreamEntry(dir, slug string, n int, now time.Time) error {
	if slug == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("usage: mkdir workstreams dir %s: %w", dir, err)
	}
	data, err := json.Marshal(WorkstreamEntry{Slug: slug, N: n, UpdatedAt: now})
	if err != nil {
		return fmt.Errorf("usage: marshal workstream entry: %w", err)
	}

	path := filepath.Join(dir, slug+".json")
	tmp, err := os.CreateTemp(dir, ".leadkit-workstream-*.tmp")
	if err != nil {
		return fmt.Errorf("usage: create temp workstream file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("usage: write temp workstream file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: close temp workstream file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: rename workstream file into place: %w", err)
	}
	return nil
}

// ReadWorkstreamRegistry reads every <slug>.json in dir, silently DROPPING any entry
// whose UpdatedAt is older than cutoff (pass now.Add(-ttl)) — a crashed/idle project's
// last-written count can't hold phantom account headroom forever. ok=false means the
// read itself failed for a reason a caller MUST fail open on (never block work over an
// unreadable registry — this is a budget optimiser, not a gate): a missing directory is
// NOT such a failure (an absent/never-created registry is the ordinary single-project
// state) and reports ok=true with a nil/empty slice; any other read error (permission
// denied, ...) reports ok=false. A malformed individual file is skipped, same
// "one bad entry shouldn't blind the whole read" rule as LoadHistory.
func ReadWorkstreamRegistry(dir string, cutoff time.Time) (entries []WorkstreamEntry, ok bool) {
	de, err := os.ReadDir(dir)
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	for _, e := range de {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var entry WorkstreamEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}
		if entry.UpdatedAt.Before(cutoff) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, true
}

// SessionEntry is one live SESSION's workstream-count + priority declaration —
// finer-grained than WorkstreamEntry (which is per PROJECT slug only): two
// concurrent sessions of the same project each get their own entry.
// (docs/build-tasks/session-priority-burn.md)
type SessionEntry struct {
	Slug string `json:"slug"`
	// SessionID identifies the session within Slug — "" only ever appears on an
	// entry ReadSessionRegistry SYNTHESIZED from a legacy flat <slug>.json (a
	// not-yet-upgraded writer); WriteSessionEntry itself refuses "" (see below).
	SessionID string `json:"session_id"`
	N         int    `json:"n"`
	// Priority is the raw declared string ("critical"|"high"|"normal"|"low") —
	// "" (unset, or synthesized from a legacy entry) means normal; callers use
	// burn.NormalizePriority rather than comparing this raw field directly.
	Priority  string    `json:"priority,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WriteSessionEntry atomically writes this session's live entry into
// dir/<slug>/<sessionID>.json — one file per (slug, session) pair, so
// concurrent sessions of the SAME project never contend on a shared file (the
// same no-cross-writer-contention property WriteWorkstreamEntry already gives
// at the per-project level, now refined to per-session). slug==""
// or sessionID=="" is a no-op, mirroring WriteWorkstreamEntry's unresolvable-
// slug fallback — no stable identity to key a shared file on.
func WriteSessionEntry(dir, slug, sessionID string, n int, priority string, now time.Time) error {
	if slug == "" || sessionID == "" {
		return nil
	}
	sessionDir := filepath.Join(dir, slug)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return fmt.Errorf("usage: mkdir session dir %s: %w", sessionDir, err)
	}
	data, err := json.Marshal(SessionEntry{Slug: slug, SessionID: sessionID, N: n, Priority: priority, UpdatedAt: now})
	if err != nil {
		return fmt.Errorf("usage: marshal session entry: %w", err)
	}

	path := filepath.Join(sessionDir, sessionID+".json")
	tmp, err := os.CreateTemp(sessionDir, ".leadkit-session-*.tmp")
	if err != nil {
		return fmt.Errorf("usage: create temp session file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("usage: write temp session file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: close temp session file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: rename session file into place: %w", err)
	}
	return nil
}

// ReadSessionRegistry reads every live SESSION entry under dir: the per-session
// shape (dir/<slug>/<session_id>.json, written by WriteSessionEntry) for every
// slug that has one, else falling back — PER SLUG — to a legacy flat
// dir/<slug>.json (WriteWorkstreamEntry's shape, a not-yet-upgraded writer) read
// back as ONE synthesized session entry (SessionID="", Priority="" == normal).
// A slug with BOTH (mid-upgrade) counts ONLY its session-dir entries — the
// legacy file for that slug is ignored, never double-counted. Stale entries
// (older than cutoff) are dropped, same rule as ReadWorkstreamRegistry. ok=false
// only on a real read failure (permission denied, ...); a missing dir is the
// ordinary "nothing registered yet" state and reports ok=true, empty.
func ReadSessionRegistry(dir string, cutoff time.Time) (entries []SessionEntry, ok bool) {
	de, err := os.ReadDir(dir)
	if err != nil {
		return nil, os.IsNotExist(err)
	}

	slugDirs := map[string]bool{}
	for _, e := range de {
		if e.IsDir() {
			slugDirs[e.Name()] = true
		}
	}

	for _, e := range de {
		name := e.Name()
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			for _, se := range sub {
				if se.IsDir() || filepath.Ext(se.Name()) != ".json" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, name, se.Name()))
				if err != nil {
					continue
				}
				var entry SessionEntry
				if err := json.Unmarshal(data, &entry); err != nil {
					continue
				}
				if entry.UpdatedAt.Before(cutoff) {
					continue
				}
				entries = append(entries, entry)
			}
			continue
		}
		if filepath.Ext(name) != ".json" {
			continue
		}
		slug := strings.TrimSuffix(name, ".json")
		if slugDirs[slug] {
			continue // superseded by that slug's own session-dir entries above
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var legacy WorkstreamEntry
		if err := json.Unmarshal(data, &legacy); err != nil {
			continue
		}
		if legacy.UpdatedAt.Before(cutoff) {
			continue
		}
		entries = append(entries, SessionEntry{Slug: legacy.Slug, N: legacy.N, UpdatedAt: legacy.UpdatedAt})
	}
	return entries, true
}

// OtherProjectsTotal sums every live entry's N except thisSlug's own — the "other
// projects' claimed workstreams" figure burn.Input.AccountN is built from:
// AccountN = thisProjectN + OtherProjectsTotal(entries, thisSlug). Excluding thisSlug
// by name (rather than relying on write-then-read ordering to have already recorded
// this tick's own entry) is what makes "single project / empty registry / only this
// slug present" reduce to exactly 0 regardless of whether this project's own write
// happened yet this tick.
func OtherProjectsTotal(entries []WorkstreamEntry, thisSlug string) int {
	sum := 0
	for _, e := range entries {
		if e.Slug == thisSlug {
			continue
		}
		sum += e.N
	}
	return sum
}
