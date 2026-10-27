package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// InFlightItem is one in-progress piece of work in a Handover.
type InFlightItem struct {
	ID    string `yaml:"id"`
	Owner string `yaml:"owner"`
	State string `yaml:"state"`
	Note  string `yaml:"note,omitempty"`
}

// RosterEntry is one live agent in a Handover's roster snapshot.
type RosterEntry struct {
	Name     string    `yaml:"name"`
	Type     string    `yaml:"type"`
	LastSeen time.Time `yaml:"last_seen"`
}

// Handover is the durable cross-compaction state: in-flight work, open decisions, and
// the roster as of the last write. Supersedes the old ad-hoc SESSION-STATE.md
// auto-snapshot. Written by a PreCompact hook (a later kit step); this step only
// defines the schema + Load/Emit/show.
type Handover struct {
	Updated        time.Time      `yaml:"updated"`
	InFlight       []InFlightItem `yaml:"in_flight,omitempty"`
	OpenDecisions  []string       `yaml:"open_decisions,omitempty"`
	RosterSnapshot []RosterEntry  `yaml:"roster_snapshot,omitempty"`
	Notes          string         `yaml:"notes,omitempty"`
}

// LoadHandover reads and parses path. The error is errors.Is(err,
// os.ErrNotExist)-checkable when the file is simply absent.
func LoadHandover(path string) (Handover, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Handover{}, err
	}
	yamlBlock, _, err := splitFrontmatter(data)
	if err != nil {
		return Handover{}, fmt.Errorf("state: handover %s: %w", path, err)
	}
	var h Handover
	if err := yaml.Unmarshal(yamlBlock, &h); err != nil {
		return Handover{}, fmt.Errorf("state: parse handover %s: %w", path, err)
	}
	return h, nil
}

// EmitHandover writes h to path as frontmatter + a rendered body, creating path's
// parent dir if needed. A full regenerate, like EmitGoal — the PreCompact hook that
// eventually writes this owns the whole file each time.
func EmitHandover(path string, h Handover) error {
	yamlBlock, err := yaml.Marshal(h)
	if err != nil {
		return fmt.Errorf("state: marshal handover: %w", err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBlock)
	b.WriteString("---\n\n# Handover\n")

	if len(h.InFlight) > 0 {
		b.WriteString("\n## In Flight\n")
		for _, it := range h.InFlight {
			fmt.Fprintf(&b, "- %s (owner=%s, state=%s): %s\n", it.ID, it.Owner, it.State, it.Note)
		}
	}
	if len(h.OpenDecisions) > 0 {
		b.WriteString("\n## Open Decisions\n")
		for _, d := range h.OpenDecisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(h.RosterSnapshot) > 0 {
		b.WriteString("\n## Roster Snapshot\n")
		for _, r := range h.RosterSnapshot {
			fmt.Fprintf(&b, "- %s (%s), last seen %s\n", r.Name, r.Type, r.LastSeen.Format(time.RFC3339))
		}
	}
	if h.Notes != "" {
		fmt.Fprintf(&b, "\n## Notes\n%s\n", h.Notes)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("state: mkdir for handover %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("state: write handover %s: %w", path, err)
	}
	return nil
}

// FormatHandoverSummary renders a compact summary of h for `state show`.
func FormatHandoverSummary(h Handover) string {
	var b strings.Builder
	fmt.Fprintf(&b, "updated: %s\n", h.Updated.Format(time.RFC3339))
	fmt.Fprintf(&b, "in-flight: %d, open decisions: %d, roster: %d\n",
		len(h.InFlight), len(h.OpenDecisions), len(h.RosterSnapshot))
	for _, it := range h.InFlight {
		fmt.Fprintf(&b, "  - %s [%s] owner=%s: %s\n", it.ID, it.State, it.Owner, it.Note)
	}
	for _, d := range h.OpenDecisions {
		fmt.Fprintf(&b, "  ? %s\n", d)
	}
	if h.Notes != "" {
		fmt.Fprintf(&b, "notes: %s\n", h.Notes)
	}
	return b.String()
}
