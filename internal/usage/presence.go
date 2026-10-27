package usage

// Active-session presence registry (docs/build-tasks/burn-auto-enforce.md).
//
// The workstream registry (workstreams.go) counts DECLARED workstreams — its entries
// carry an N that feeds burn's account_n. Presence is deliberately SEPARATE and carries
// NO N: it is a lightweight "this session is alive" heartbeat, refreshed on every
// session activity by the usage-logger PreToolUse hook (machine-wide, on every
// Bash/Agent), so a live session is visible WITHOUT it having to run `leadkit burn` —
// and WITHOUT inflating account_n, because a reviewer/watcher/poller session is free
// infra, not a workstream. The governor's workstream-vs-infra accounting still decides
// n from the declared workstream entries alone; presence only makes liveness observable
// and lets a crashed session age out of the "active" set on the stale window.
//
// One file per (slug, session) pair — dir/<slug>/<sessionID>.json — so concurrent
// sessions never contend on a shared file (the same no-cross-writer-contention property
// WriteSessionEntry gives the session registry).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultPresenceDir is the account-wide directory every session's presence heartbeat
// lives under.
var DefaultPresenceDir = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "presence")

// PresenceEntry is one live SESSION's presence heartbeat — identity + freshness only,
// deliberately NO N (presence never contributes to burn's account_n; see the package
// doc above).
//
// MessagingSocket + HarnessSession are the HARNESS ADDRESSING HANDLE
// (docs/build-tasks/session-identity-cwd-honest.md): the Claude Code cross-session
// SendMessage layer addresses by socket/uuid, not by leadkit slug/session_id, so
// recording them here lets DeliveryHint bridge a leadkit recipient to a real
// SendMessage target instead of the sender guessing. Both are read from env at write
// time and OMITTED (omitempty) when absent — an older writer's entry simply lacks them
// and DeliveryHint degrades to a slug/uuid hint (backward-safe).
type PresenceEntry struct {
	Slug            string    `json:"slug"`
	SessionID       string    `json:"session_id"`
	MessagingSocket string    `json:"messaging_socket,omitempty"`
	HarnessSession  string    `json:"harness_session,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// WritePresenceEntry atomically refreshes this session's presence heartbeat at
// dir/<slug>/<sessionID>.json with a fresh updated_at. slug=="" or sessionID=="" is a
// no-op (success, nothing written) — an unresolvable identity has nothing stable to key
// a file on, mirroring WriteWorkstreamEntry/WriteSessionEntry's fallback. Best-effort by
// contract: callers (the hook) log and continue on error — a presence write must never
// block or fail the tool call it is piggy-backing on.
func WritePresenceEntry(dir, slug, sessionID string, now time.Time) error {
	return WritePresenceEntryWithHarness(dir, slug, sessionID, "", "", now)
}

// WritePresenceEntryWithHarness is WritePresenceEntry plus the HARNESS ADDRESSING
// HANDLE (messagingSocket = $CLAUDE_CODE_MESSAGING_SOCKET, harnessSession =
// $CLAUDE_CODE_SESSION_ID), so DeliveryHint can name a real SendMessage target for a
// live recipient. Empty handle fields are simply omitted from the persisted entry
// (backward-safe); everything else — the atomic temp+rename, the empty-identity no-op —
// is identical to WritePresenceEntry.
func WritePresenceEntryWithHarness(dir, slug, sessionID, messagingSocket, harnessSession string, now time.Time) error {
	if slug == "" || sessionID == "" {
		return nil
	}
	sessionDir := filepath.Join(dir, slug)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return fmt.Errorf("usage: mkdir presence dir %s: %w", sessionDir, err)
	}
	data, err := json.Marshal(PresenceEntry{
		Slug:            slug,
		SessionID:       sessionID,
		MessagingSocket: messagingSocket,
		HarnessSession:  harnessSession,
		UpdatedAt:       now,
	})
	if err != nil {
		return fmt.Errorf("usage: marshal presence entry: %w", err)
	}

	path := filepath.Join(sessionDir, sessionID+".json")
	tmp, err := os.CreateTemp(sessionDir, ".leadkit-presence-*.tmp")
	if err != nil {
		return fmt.Errorf("usage: create temp presence file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("usage: write temp presence file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: close temp presence file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: rename presence file into place: %w", err)
	}
	return nil
}

// ReadPresence reads every live presence heartbeat under dir, DROPPING any entry whose
// UpdatedAt is older than cutoff (pass now.Add(-staleWindow)) — a crashed/exited session
// ages out of the active set on the stale window. ok=false only on a real read failure
// (permission denied, ...); a missing dir is the ordinary "nothing active yet" state and
// reports ok=true, empty. A malformed individual file is skipped (one bad entry never
// blinds the whole read), same rule as ReadWorkstreamRegistry.
func ReadPresence(dir string, cutoff time.Time) (entries []PresenceEntry, ok bool) {
	de, err := os.ReadDir(dir)
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	for _, e := range de {
		if !e.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, se := range sub {
			if se.IsDir() || filepath.Ext(se.Name()) != ".json" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name(), se.Name()))
			if err != nil {
				continue
			}
			var entry PresenceEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				continue
			}
			if entry.UpdatedAt.Before(cutoff) {
				continue
			}
			entries = append(entries, entry)
		}
	}
	return entries, true
}
