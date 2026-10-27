package audit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/review"
)

// activeStatuses are the store-task frontmatter status values orphaned-active checks
// against — a task in one of these but with no overlay placement anywhere has no lane
// watching it. Deliberately a fixed list here, NOT the profile's canonical_statuses:
// this is judging "should something be watching this", not "is this spelling
// sanctioned" — the two lists overlap but aren't the same set (e.g. awaiting_pr_review
// is active-relevant but isn't in the default canonical set at all).
var activeStatuses = map[string]bool{
	"pending": true, "in_progress": true, "in_review": true, "gated": true,
	"needs_rework": true, "blocked": true, "awaiting_pr_review": true, "awaiting_review": true,
}

// idPattern extracts a store task's leading "NNN-" id from its filename.
var idPattern = regexp.MustCompile(`^(\d+)-`)

func idFromFilename(name string) string {
	m := idPattern.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// idOrBase is a Finding's ID: the task's numeric id when its filename has one, else
// the filename itself.
func idOrBase(id, base string) string {
	if id != "" {
		return id
	}
	return base
}

// storeTask is what DiskLens needs from one store/*.md file.
type storeTask struct {
	ID     string
	Status string // raw frontmatter value, "" if absent
}

// overlayEntry is one symlink found anywhere under the queue root.
type overlayEntry struct {
	Base   string // symlink filename — the store task's filename it (should) point at
	Status string // the symlink's immediate parent dir name — whatever bucket it's filed under
	Path   string // full path to the symlink itself
}

// walkOverlay finds every symlink under queueRoot, skipping any directory (at any
// depth) whose name starts with "_" — the waker's own scratch/test-fixture rule
// (_gvtest-*, _held, _parked, ...). Best-effort: a path that errors mid-walk is
// skipped rather than aborting the whole audit.
func walkOverlay(queueRoot string) ([]overlayEntry, error) {
	var entries []overlayEntry
	err := filepath.WalkDir(queueRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != queueRoot && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil // a real file at this level (ROLES, ACTIVE-REVIEWERS.md, ...) — not a queue entry
		}
		entries = append(entries, overlayEntry{
			Base:   d.Name(),
			Status: filepath.Base(filepath.Dir(path)),
			Path:   path,
		})
		return nil
	})
	return entries, err
}

// DiskLens is audit Lens 1: orphaned-active, dangling-symlink, status-drift,
// duplicate-id, and noncanonical-status findings over the store (storeRoot) and its
// queue overlay (queueRoot). canonicalStatuses is the sanctioned status vocabulary
// (the profile's canonical_statuses) — a deviation is FLAGGED, never normalized.
func DiskLens(queueRoot, storeRoot string, canonicalStatuses []string) ([]Finding, error) {
	canonicalSet := map[string]bool{}
	for _, s := range canonicalStatuses {
		canonicalSet[normalizeStatus(s)] = true
	}

	storeEntries, err := os.ReadDir(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("audit: read store root %s: %w", storeRoot, err)
	}

	tasks := map[string]storeTask{} // filename -> task
	idFiles := map[string][]string{}
	for _, e := range storeEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm, err := review.ReadFrontmatter(filepath.Join(storeRoot, e.Name()))
		if err != nil {
			continue // best-effort — a file with no/malformed frontmatter is out of scope here
		}
		id := idFromFilename(e.Name())
		tasks[e.Name()] = storeTask{ID: id, Status: fm["status"]}
		if id != "" {
			idFiles[id] = append(idFiles[id], e.Name())
		}
	}

	var findings []Finding

	for base, t := range tasks {
		if t.Status != "" && !canonicalSet[normalizeStatus(t.Status)] {
			findings = append(findings, Finding{
				Category: CategoryNoncanonicalStatus, Severity: SeverityWarn,
				ID:      idOrBase(t.ID, base),
				Detail:  fmt.Sprintf("%s: status %q is not in the canonical set", base, t.Status),
				RefSrc:  "store",
				RefPath: base,
			})
		}
	}

	for id, files := range idFiles {
		if len(files) < 2 {
			continue
		}
		sorted := append([]string(nil), files...)
		sort.Strings(sorted)
		findings = append(findings, Finding{
			Category: CategoryDuplicateID, Severity: SeverityInfo,
			ID:     id,
			Detail: fmt.Sprintf("id %s used by: %s", id, strings.Join(sorted, ", ")),
		})
	}

	overlay, err := walkOverlay(queueRoot)
	if err != nil {
		return findings, fmt.Errorf("audit: walk queue root %s: %w", queueRoot, err)
	}

	placed := map[string]bool{}
	for _, oe := range overlay {
		if _, statErr := os.Stat(oe.Path); statErr != nil {
			overlayRel, relErr := filepath.Rel(queueRoot, oe.Path)
			if relErr != nil {
				overlayRel = "" // fall back to no link rather than a bad one
			}
			findings = append(findings, Finding{
				Category: CategoryDanglingSymlink, Severity: SeverityWarn,
				ID:      oe.Base,
				Detail:  fmt.Sprintf("%s: target missing", oe.Path),
				RefSrc:  "", // the overlay entry itself — /task shows it as an unreadable dangling link
				RefPath: overlayRel,
			})
			continue
		}
		placed[oe.Base] = true
		if t, ok := tasks[oe.Base]; ok && normalizeStatus(oe.Status) != normalizeStatus(t.Status) {
			findings = append(findings, Finding{
				Category: CategoryStatusDrift, Severity: SeverityWarn,
				ID:      idOrBase(t.ID, oe.Base),
				Detail:  fmt.Sprintf("%s: overlay dir=%q frontmatter status=%q", oe.Base, oe.Status, t.Status),
				RefSrc:  "store",
				RefPath: oe.Base,
			})
		}
	}

	for base, t := range tasks {
		if !activeStatuses[normalizeStatus(t.Status)] || placed[base] {
			continue
		}
		findings = append(findings, Finding{
			Category: CategoryOrphanedActive, Severity: SeverityWarn,
			ID:      idOrBase(t.ID, base),
			Detail:  fmt.Sprintf("%s: status=%q, no overlay placement found", base, t.Status),
			RefSrc:  "store",
			RefPath: base,
		})
	}

	return findings, nil
}
