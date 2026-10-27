package hierarchy

import (
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/typeinfer"
)

// LoadSubtasksResolved is LoadSubtasks plus on-the-fly type inference (type -> owner
// -> lane, via internal/typeinfer.Resolve — the SAME rule `queue backfill-type`
// uses to WRITE the type field, see internal/backfill's own doc comment). `propose`
// needs a good type signal for every task right now, not only the tasks that have
// actually been backfilled — inferring on read decouples proposal quality from that
// migration ever having been --applied.
//
// queueRoot/rolesPath/agentsDir mirror exactly what `queue backfill-type` reads
// (internal/backfill.ComputePlan) — the queue-overlay walk is duplicated here rather
// than imported, this codebase's small-package decoupling convention; internal/
// typeinfer itself IS the deliberate shared source of truth these consumers don't
// duplicate.
func LoadSubtasksResolved(storeRoot, queueRoot, rolesPath, agentsDir string) ([]Subtask, error) {
	subtasks, err := LoadSubtasks(storeRoot)
	if err != nil {
		return nil, err
	}
	placedLane, err := readOverlayPlacements(queueRoot)
	if err != nil {
		return nil, err
	}
	laneToRole := map[string]string{}
	for _, e := range roles.List(rolesPath) {
		laneToRole[e.Live] = e.Role
	}
	canonical := typeinfer.LoadCanonical(agentsDir)

	for i, s := range subtasks {
		if typ, _, ok := typeinfer.Resolve(s.Type, s.Owner, placedLane[s.ID], laneToRole, canonical); ok {
			subtasks[i].Type = typ
		}
		// Unresolved (typeinfer.Resolve's own "manual" case): leave Type as-is
		// (usually ""). propose's own future-work fallback already handles "no
		// type signal" — this is never a hard failure.
	}
	return subtasks, nil
}

// readOverlayPlacements walks queueRoot for symlinks and returns id -> lane for
// every resolvable entry — mirrors internal/backfill's own reader exactly (see that
// package's doc comment on why it's duplicated, not imported, across consumers).
// A dangling symlink is skipped; a task with no overlay placement at all (never
// routed, or already in done/) simply has lane == "".
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
