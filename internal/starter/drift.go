package starter

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DriftStatus describes one definition's relationship to the embedded starter of the same
// name.
type DriftStatus string

const (
	// DriftCustomised: the team has edited or replaced the starter. This is the EXPECTED
	// state and is reported as information, never as a problem. A drift report that
	// complains about every edited definition is a report people stop reading.
	DriftCustomised DriftStatus = "customised"
	// DriftPristine: still byte-identical to the shipped starter, marker included. Worth
	// a gentle nudge, because a starter is a placeholder: it says "replace me", and a
	// pristine critique definition means the gate is running on generic text.
	DriftPristine DriftStatus = "pristine"
	// DriftRemoved: a starter the working set does not have. Reported, NEVER restored —
	// deleting a role is a decision, and an upgrade that quietly reinstated it would
	// override a human on the strength of a default.
	DriftRemoved DriftStatus = "removed"
	// DriftLocal: a working definition with no starter counterpart. Entirely normal; the
	// starter set is a floor, not an inventory.
	DriftLocal DriftStatus = "local"
)

// DriftFinding is one definition's status against the embedded starters.
type DriftFinding struct {
	Def    string      `json:"def"`
	Status DriftStatus `json:"status"`
	Detail string      `json:"detail"`
}

// CompareToStarters reports how the definitions in agentsDir relate to the embedded
// starters. Read-only and advisory: it writes nothing and restores nothing.
//
// A missing agentsDir is not an error — every starter simply reads as removed, which is the
// honest description of a machine that has not installed them yet.
func CompareToStarters(agentsDir string) ([]DriftFinding, error) {
	embedded, err := Agents()
	if err != nil {
		return nil, err
	}
	local, err := readAgentDir(agentsDir)
	if err != nil {
		return nil, err
	}

	var out []DriftFinding
	for name, starterData := range embedded {
		path, present := local[name]
		if !present {
			out = append(out, DriftFinding{
				Def: name, Status: DriftRemoved,
				Detail: "not in the working set — left alone; removing a role is a decision, not drift to repair",
			})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		switch {
		case bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(starterData)):
			out = append(out, DriftFinding{
				Def: name, Status: DriftPristine,
				Detail: "unchanged from the shipped starter — starters are placeholders, so consider whether this role should say something specific to your project",
			})
		case IsGeneratedAgent(data):
			out = append(out, DriftFinding{
				Def: name, Status: DriftCustomised,
				Detail: "edited, but still carries the generated-starter marker — delete that line once you own this definition, so upgrades can tell yours from ours",
			})
		default:
			out = append(out, DriftFinding{
				Def: name, Status: DriftCustomised,
				Detail: "yours — marker removed",
			})
		}
	}
	for name := range local {
		if _, isStarter := embedded[name]; !isStarter {
			out = append(out, DriftFinding{
				Def: name, Status: DriftLocal,
				Detail: "no starter counterpart — the starter set is a floor, not an inventory",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Def < out[j].Def })
	return out, nil
}

// CountByStatus tallies findings, for a one-line summary.
func CountByStatus(findings []DriftFinding) map[DriftStatus]int {
	out := map[DriftStatus]int{}
	for _, f := range findings {
		out[f.Status]++
	}
	return out
}

// DefaultUserAgentsDir is the conventional per-user definition directory.
func DefaultUserAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "agents")
}

// readAgentDir lists *.md files directly in dir. A missing dir is not an error.
func readAgentDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		// A dotfile or an editor backup is not a definition.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out[e.Name()] = filepath.Join(dir, e.Name())
	}
	return out, nil
}
