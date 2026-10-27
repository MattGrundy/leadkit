package team

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// StatusResult is `leadkit team status`'s drift report — everything here is a
// read-only comparison, nothing is written.
type StatusResult struct {
	Added        []string // agent-def filename present live, not in the captured snapshot
	Changed      []string // present in both, content differs
	Removed      []string // present in the captured snapshot, gone from the live dir
	MissingLanes []string // a manifest lane whose dir doesn't exist under the live queue root
}

// Clean reports whether nothing has drifted.
func (r StatusResult) Clean() bool {
	return len(r.Added) == 0 && len(r.Changed) == 0 && len(r.Removed) == 0 && len(r.MissingLanes) == 0
}

// Status compares the live agents_dir + queue lanes against cfg.TeamDir's captured
// snapshot. Read-only throughout: a missing agents_dir, team_dir, or manifest just
// means fewer/more findings (e.g. everything in the live dir reads as "Added"),
// never an error — `team status` is a report, not a strict validator.
func Status(cfg Config, queueRoot string) (StatusResult, error) {
	var res StatusResult

	live := readMDFiles(cfg.AgentsDir, true)
	captured := readMDFiles(filepath.Join(cfg.TeamDir, "agents"), false)

	for name, data := range live {
		capData, ok := captured[name]
		switch {
		case !ok:
			res.Added = append(res.Added, name)
		case !bytes.Equal(data, capData):
			res.Changed = append(res.Changed, name)
		}
	}
	for name := range captured {
		if _, ok := live[name]; !ok {
			res.Removed = append(res.Removed, name)
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Changed)
	sort.Strings(res.Removed)

	manifestPath := filepath.Join(cfg.TeamDir, "inbox-manifest.yaml")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var m InboxManifest
		if yaml.Unmarshal(data, &m) == nil {
			for _, lane := range m.Lanes {
				dir := filepath.Join(queueRoot, lane.Lane)
				if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
					res.MissingLanes = append(res.MissingLanes, lane.Lane)
				}
			}
		}
	}
	sort.Strings(res.MissingLanes)

	return res, nil
}

// readMDFiles reads every *.md in dir into name -> content. skipUnderscore excludes
// a leading-underscore file (the protocol doc, captured separately) — only relevant
// for the LIVE agents_dir; the captured snapshot's agents/ subdir never contains one
// in the first place (Capture already excludes it), so skipUnderscore is false there.
// A missing/unreadable dir yields an empty map, never an error.
func readMDFiles(dir string, skipUnderscore bool) map[string][]byte {
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if skipUnderscore && strings.HasPrefix(name, "_") {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			out[name] = data
		}
	}
	return out
}
