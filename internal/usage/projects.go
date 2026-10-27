package usage

// The account-level PROJECT registry (docs/build-tasks/multi-project-board.md):
// the workstream registry above (WorkstreamEntry) already enumerates active
// project SLUGS account-wide, but only carries {slug, n, updated_at} — not the
// paths a multi-project board needs to actually RENDER each project. This file
// is a SIBLING registry, not an extension of WorkstreamEntry's shape (that shape
// is a stable file contract burn's account total already reads; this is a
// separate, richer record other things — this project's queue/store/issues
// roots + a display name — that only the board consumes). Same directory
// pattern (one JSON file per slug, atomic write, one writer per slug — the
// project that owns that slug), keyed by the SAME install.ResolveProjectSlug
// the workstream registry and the daemon namespacing use, so a project can
// never appear under two different names across the three registries.
//
// IMPORTANT: single-project `leadkit serve -queue` must NEVER read (or write)
// this registry — see docs/build-tasks/multi-project-board.md's hard
// constraint and internal/viewer's account.go (the only reader) vs. viewer.go
// (the single-project Server, which does not import this package at all).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultProjectsDir is the account-wide directory every project's registered-
// roots file lives under.
var DefaultProjectsDir = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "projects")

// ProjectEntry is one project's registered roots — everything the account board
// needs to point the existing viewer.Scan/tree.BuildTree machinery at that
// project without the project itself being open.
type ProjectEntry struct {
	Slug string `json:"slug"`
	// Name is a display name for the board's index/switcher — the project's
	// project_id when set, else the caller's own best-effort fallback (e.g. the
	// queue root's parent directory basename). Never used as a lookup key —
	// Slug is, always.
	Name       string    `json:"name"`
	QueueRoot  string    `json:"queue_root"`
	StoreRoot  string    `json:"store_root,omitempty"`
	IssuesRoot string    `json:"issues_root,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// WriteProjectEntry atomically writes this project's <slug>.json into dir —
// same shape of atomicity and one-writer-per-slug contract as
// WriteWorkstreamEntry. slug=="" is a no-op (success, nothing written) for the
// same reason WriteWorkstreamEntry's is: an unresolvable slug has no stable
// identity to key a shared file on.
func WriteProjectEntry(dir, slug, name, queueRoot, storeRoot, issuesRoot string, now time.Time) error {
	if slug == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("usage: mkdir projects dir %s: %w", dir, err)
	}
	data, err := json.Marshal(ProjectEntry{
		Slug: slug, Name: name,
		QueueRoot: queueRoot, StoreRoot: storeRoot, IssuesRoot: issuesRoot,
		UpdatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("usage: marshal project entry: %w", err)
	}

	path := filepath.Join(dir, slug+".json")
	tmp, err := os.CreateTemp(dir, ".leadkit-project-*.tmp")
	if err != nil {
		return fmt.Errorf("usage: create temp project file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("usage: write temp project file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: close temp project file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("usage: rename project file into place: %w", err)
	}
	return nil
}

// ReadProjectRegistry reads every <slug>.json in dir, sorted by slug. Unlike
// ReadWorkstreamRegistry, stale entries are NOT dropped here — the board wants
// to SHOW a stale project (greyed / flagged), never silently omit it
// (multi-project-board.md: "Stale entries ... shown greyed / omitted, not
// errored" — the board decides that, comparing UpdatedAt against its own TTL).
// ok=false only on a genuine read failure (permission denied, ...); a missing
// directory is the ordinary "nothing registered yet" state and reports
// ok=true with a nil/empty slice. A malformed individual file is skipped, same
// "one bad entry shouldn't blind the whole read" rule as the workstream/session
// registries.
func ReadProjectRegistry(dir string) (entries []ProjectEntry, ok bool) {
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
		var entry ProjectEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries, true
}
