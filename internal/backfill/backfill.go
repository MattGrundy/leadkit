// Package backfill is `leadkit queue backfill-type` — the migration that infers a
// canonical `type:` per store task and writes it into frontmatter. It shares its
// inference rule with internal/orchestrate's planner via internal/typeinfer.Resolve,
// so the migration fixes exactly what the planner sees (same owner-string
// fragmentation, same canonical set, same fallback order).
//
// SAFETY: ComputePlan never writes anything — it's a pure read + typeinfer.Resolve
// pass. Only Apply (called for --apply --confirm) writes, and only the `type` key,
// via review.UpdateFrontmatter (preserves field order/every other field exactly).
// dry-run is the default the CLI wires; see cmd/leadkit's confirmGuard for the same
// bare---apply-refuses guard audit/init/team install/orchestrate apply all share.
package backfill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/review"
	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/typeinfer"
)

// Item is one task backfill resolved a type for.
type Item struct {
	ID     string
	Type   string
	Source string // "type" | "owner" | "lane" — "type" means already-canonical, a no-op
	path   string // the store file's full path, for Apply — unexported, CLI/tests don't need it
}

// Manual is one task backfill could NOT resolve — never guessed at, reported for a
// human.
type Manual struct {
	ID     string
	Reason string
}

// Plan is ComputePlan's result — read-only until Apply.
type Plan struct {
	Resolved []Item
	Manual   []Manual
}

// AlreadyTyped reports how many of Resolved are already-canonical (a no-op — Apply
// skips these) vs would actually change something.
func (p Plan) AlreadyTyped() int {
	n := 0
	for _, it := range p.Resolved {
		if it.Source == "type" {
			n++
		}
	}
	return n
}

// ComputePlan reads storeRoot's frontmatter, queueRoot's overlay placements, and
// rolesPath's lane->role table + agentsDir's canonical type set, and resolves each
// task's type via typeinfer.Resolve. Pure — writes nothing.
func ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir string) (Plan, error) {
	var plan Plan

	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		return plan, fmt.Errorf("backfill: read store root %s: %w", storeRoot, err)
	}

	placedLane, err := readOverlayPlacements(queueRoot)
	if err != nil {
		return plan, fmt.Errorf("backfill: read queue root %s: %w", queueRoot, err)
	}
	laneToRole := map[string]string{}
	for _, e := range roles.List(rolesPath) {
		laneToRole[e.Live] = e.Role
	}
	canonical := typeinfer.LoadCanonical(agentsDir)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(storeRoot, e.Name())
		fm, err := review.ReadFrontmatter(path)
		if err != nil {
			continue // best-effort — a file with no/malformed frontmatter is out of scope
		}
		id := strings.TrimSpace(fm["id"])
		if id == "" {
			id = idFromFilename(e.Name())
		}
		if id == "" {
			continue
		}

		lane := placedLane[id]
		typ, source, ok := typeinfer.Resolve(fm["type"], fm["owner"], lane, laneToRole, canonical)
		if !ok {
			plan.Manual = append(plan.Manual, Manual{ID: id, Reason: manualReason(fm["owner"], lane)})
			continue
		}
		plan.Resolved = append(plan.Resolved, Item{ID: id, Type: typ, Source: source, path: path})
	}

	return plan, nil
}

// manualReason gives a human a starting point without guessing at the actual type.
func manualReason(owner, lane string) string {
	o := strings.TrimSpace(owner)
	switch {
	case o == "" || strings.EqualFold(o, "unassigned") || strings.EqualFold(o, "null"):
		if lane == "" {
			return fmt.Sprintf("owner=%q and no queue placement to infer from", owner)
		}
		return fmt.Sprintf("owner=%q and lane %q didn't resolve via ROLES either", owner, lane)
	default:
		return fmt.Sprintf("owner=%q doesn't match any known agent type (looks like a branch/spike name, not a role)", owner)
	}
}

// backfillLogEntry is one backfill_log JSONL record.
type backfillLogEntry struct {
	TS     string `json:"ts"`
	ID     string `json:"id"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

// Apply writes `type:` for every Resolved item whose Source isn't already "type"
// (an already-canonical type is a no-op, by design — see Plan.AlreadyTyped). dryRun
// skips every write (and every log entry) but still returns the same count of items
// that WOULD be written, for a dry-run summary that matches the shape of a real run.
func Apply(plan Plan, dryRun bool, logPath string) (writtenCount int, err error) {
	var logged []backfillLogEntry
	now := time.Now().UTC().Format(time.RFC3339)

	for _, it := range plan.Resolved {
		if it.Source == "type" {
			continue // already canonical — idempotent no-op
		}
		if !dryRun {
			if err := review.UpdateFrontmatter(it.path, []review.KV{{Key: "type", Value: it.Type}}); err != nil {
				return writtenCount, fmt.Errorf("backfill: %s: %w", it.ID, err)
			}
			logged = append(logged, backfillLogEntry{TS: now, ID: it.ID, Type: it.Type, Source: it.Source})
		}
		writtenCount++
	}

	if !dryRun && len(logged) > 0 && logPath != "" {
		if err := writeBackfillLog(logPath, logged); err != nil {
			return writtenCount, fmt.Errorf("backfill: change log: %w", err)
		}
	}
	return writtenCount, nil
}

// FormatPlan renders plan for `queue backfill-type`'s output — per-task source,
// the manual list, and a one-line summary. applied is the count Apply actually
// wrote (0 for a pure dry-run preview render, since nothing was written yet — pass
// the DRY-RUN count from Apply(plan, true, "") to preview what WOULD be written).
func FormatPlan(plan Plan, applied int, dryRun bool) string {
	var b strings.Builder
	for _, it := range plan.Resolved {
		verb := "would set"
		if !dryRun {
			verb = "set"
		}
		if it.Source == "type" {
			fmt.Fprintf(&b, "%s: already %s (no-op)\n", it.ID, it.Type)
			continue
		}
		fmt.Fprintf(&b, "%s: %s type=%s (source: %s)\n", it.ID, verb, it.Type, it.Source)
	}
	for _, m := range plan.Manual {
		fmt.Fprintf(&b, "%s: manual — %s\n", m.ID, m.Reason)
	}
	verb := "would write"
	if !dryRun {
		verb = "wrote"
	}
	fmt.Fprintf(&b, "summary: %d resolved (%d already typed, %d %s), %d manual\n",
		len(plan.Resolved), plan.AlreadyTyped(), applied, verb, len(plan.Manual))
	return b.String()
}

func writeBackfillLog(path string, entries []backfillLogEntry) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// idFromFilename extracts a store task's leading "NNN-" id from its filename — used
// only when the frontmatter itself carries no explicit `id:` field. Mirrors
// internal/orchestrate's own (unexported, duplicated on purpose — see that
// package's doc comment on staying decoupled).
func idFromFilename(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(name) || name[i] != '-' {
		return ""
	}
	return name[:i]
}

// readOverlayPlacements walks queueRoot for symlinks and returns id -> lane for
// every resolvable entry — the same shape internal/orchestrate's own reader
// produces (duplicated rather than imported, matching this codebase's small-package
// decoupling convention). A dangling symlink is skipped; a task with no overlay
// placement at all (never routed, or already in done/) simply has lane == "".
func readOverlayPlacements(queueRoot string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(queueRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != queueRoot && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return nil // dangling
		}
		id := idFromFilename(d.Name())
		if id == "" {
			return nil
		}
		lane := filepath.Base(filepath.Dir(filepath.Dir(path)))
		out[id] = lane
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
