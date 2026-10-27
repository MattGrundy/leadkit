package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultSurfaceDedupeWindow is how long a session stays silent after a routing-inbox
// surface, so the self-surfacing hook nudges at most once per window per session rather
// than nagging on every tool call. Coarse on purpose — this is a gentle reminder, not a
// gate.
const DefaultSurfaceDedupeWindow = 30 * time.Minute

// surfaceStateDirName is the hidden subdir under the routing store where per-session
// dedupe markers live. Kept INSIDE the routing dir (not a project path) so it travels
// with the store and a test's temp routing dir is self-contained. The leading dot keeps
// it out of the kind scan (scan only descends real <kind> dirs, and a dotfile dir isn't
// a valid kind anyway).
const surfaceStateDirName = ".surfaced"

// surfaceMarker is one session's last-surfaced timestamp.
type surfaceMarker struct {
	LastSurfaced time.Time `json:"last_surfaced"`
}

// SurfaceOnce is the self-surfacing hook's core: it reports whether THIS session should
// emit a one-line routing-inbox surface right now, and the line to emit. It is
// FAIL-OPEN throughout — any trouble (unreadable store, unresolvable identity, a marker
// read/write error) yields ("", false): the surface is a convenience, never allowed to
// disturb the tool call it piggy-backs on.
//
// It surfaces when the session's project (slug) has at least one OPEN (non-terminal)
// routing item AND this session hasn't already surfaced within window (the dedupe). When
// it decides to surface, it records the marker so the next call inside the window stays
// silent. slug or sessionID being empty → silent (an unresolvable identity has nothing
// to key a dedupe marker on, and nothing to address an inbox for).
func SurfaceOnce(dir, slug, sessionID string, window time.Duration, now time.Time) (line string, surface bool) {
	if dir == "" || slug == "" || sessionID == "" {
		return "", false
	}
	s := Open(dir)

	// Count OPEN (non-terminal) items for this slug. Inbox already skips malformed files
	// and treats a missing store as empty; a genuine read failure fails open (silent).
	items, err := s.Inbox(slug, InboxFilter{})
	if err != nil {
		return "", false
	}
	open := 0
	for _, e := range items {
		if !e.Terminal() {
			open++
		}
	}
	if open == 0 {
		return "", false
	}

	// Dedupe: silent if this session surfaced within the window.
	markerPath := filepath.Join(dir, surfaceStateDirName, slug+"__"+sessionID+".json")
	if data, rerr := os.ReadFile(markerPath); rerr == nil {
		var m surfaceMarker
		if json.Unmarshal(data, &m) == nil && now.Sub(m.LastSurfaced) < window {
			return "", false
		}
	}

	// Record the marker (best-effort — a write failure must NOT suppress this surface;
	// worst case the next call also surfaces, which is harmless).
	if data, merr := json.Marshal(surfaceMarker{LastSurfaced: now}); merr == nil {
		if mkerr := os.MkdirAll(filepath.Dir(markerPath), 0o755); mkerr == nil {
			_ = writeFileAtomic(markerPath, data)
		}
	}

	return fmt.Sprintf("you have %d open routing item(s): leadkit routing inbox", open), true
}
