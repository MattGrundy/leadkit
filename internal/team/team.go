// Package team is `leadkit team capture|status|install` — versioning the
// operating-model artifacts that live OUTSIDE the repo (~/.claude/agents' 20 agent
// defs + the team protocol, plus this project's SOP) into the repo itself, and
// reconstructing them on a fresh machine.
//
// capture is the PRIMARY, zero-impact command: it only ever READS from the
// configured source paths (agents_dir, protocol_file, sop_file) and WRITES into the
// repo's own team_dir — never touches ~/.claude or the live queue. status is
// read-only (a drift check). install is the only mutating command, guarded the same
// way init/audit are (dry-run default, --apply --confirm required — see
// cmd/leadkit's confirmGuard).
package team

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/roles"
)

// RosterSpec mirrors config.RosterSpec — kept as its own type here (not an import of
// internal/config) so this package doesn't depend on it, matching every other
// internal package's "plain struct, caller wires the profile" convention.
type RosterSpec struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`
	// Lane mirrors config.RosterSpec.Lane — see its doc comment. Consumed by
	// ComputeScaffoldPlan.
	Lane string `yaml:"lane,omitempty"`
	// ReviewLayer mirrors config.RosterSpec.ReviewLayer: the role is a review layer of the gate,
	// so standup lists it with the review consumers and scaffold keeps it off every other
	// review layer's lane.
	ReviewLayer bool `yaml:"review_layer,omitempty"`
}

// Config bundles team capture/status/install's profile inputs.
type Config struct {
	AgentsDir    string // source: live agent-def dir (e.g. ~/.claude/agents)
	ProtocolFile string // source: the team protocol doc
	SopFile      string // source: this project's SOP doc
	TeamDir      string // dest: the repo's versioned snapshot dir
	RolesPath    string // the queue/ROLES routing file — inbox-manifest's lane source
	Roster       []RosterSpec
	// SkillsDir is the LIVE leadkit-adopt skill's directory (e.g.
	// ~/.claude/skills/leadkit-adopt) — `leadkit init --apply` writes it there
	// from the embedded skill content; Capture reads whatever's actually live
	// there, the same "read the live artifact, write the snapshot" pattern
	// AgentsDir already follows. Best-effort: a project that hasn't run `init
	// --apply` yet simply has nothing to capture here (unlike AgentsDir, a
	// missing/absent SkillsDir is never an error — see captureSkill).
	SkillsDir string
}

// LaneEntry is one durable role -> lane mapping in inbox-manifest.yaml.
type LaneEntry struct {
	Role    string   `yaml:"role"`
	Lane    string   `yaml:"lane"`
	Subdirs []string `yaml:"subdirs"`
}

// canonicalSubdirs is what every durable lane is expected to have — NOT the full
// viewer status set (blocked/needs-rework are per-ticket states within these, not
// separate top-level lane dirs for this purpose).
var canonicalSubdirs = []string{"pending", "in-progress", "done"}

// InboxManifest is inbox-manifest.yaml's shape: the durable lane structure, built
// from queue/ROLES (the authoritative role->lane routing table) — NOT a scan of the
// live queue tree, which would also pick up transient/ephemeral lanes (cr-b-drain,
// fix-*, owner-479a, _parked*, ...) that don't belong in a versioned snapshot.
type InboxManifest struct {
	Lanes []LaneEntry `yaml:"lanes"`
}

// BuildInboxManifest reads rolesPath via internal/roles (which already skips
// comments/prose and anything that isn't a clean two-field row — see its own doc
// comment) and turns each row into a LaneEntry. A missing/unreadable rolesPath
// yields an empty manifest, not an error — capture is best-effort by design.
func BuildInboxManifest(rolesPath string) InboxManifest {
	entries := roles.List(rolesPath)
	lanes := make([]LaneEntry, 0, len(entries))
	for _, e := range entries {
		lanes = append(lanes, LaneEntry{
			Role:    e.Role,
			Lane:    e.Live,
			Subdirs: append([]string(nil), canonicalSubdirs...),
		})
	}
	return InboxManifest{Lanes: lanes}
}

// CapturedFile is one file's name + size, as captured.yaml records it.
type CapturedFile struct {
	Name string `yaml:"name"`
	Size int64  `yaml:"size"`
}

// CapturedSource records exactly where each captured artifact came from — so a
// future `team status`/install (possibly on a different machine) knows what it's
// comparing against.
type CapturedSource struct {
	AgentsDir    string `yaml:"agents_dir"`
	ProtocolFile string `yaml:"protocol_file"`
	SopFile      string `yaml:"sop_file"`
	RolesPath    string `yaml:"roles_path"`
}

// CapturedManifest is captured.yaml's shape: capture's own manifest-of-the-manifest —
// when, from where, what (with sizes), and the roster/workstream definition, so the
// whole team is described from one file.
type CapturedManifest struct {
	CapturedAt string         `yaml:"captured_at"` // RFC3339
	Source     CapturedSource `yaml:"source"`
	AgentDefs  []CapturedFile `yaml:"agent_defs"`
	Protocol   *CapturedFile  `yaml:"protocol,omitempty"`
	Sop        *CapturedFile  `yaml:"sop,omitempty"`
	// SopGenerated marks Sop as a leadkit-generated starter (queue-mode-enforcement
	// item 6/GAP-5) rather than a captured, hand-written artifact — so `team status`'s
	// drift check (and a human skimming captured.yaml) can tell the difference at a
	// glance. Absent/false = a real captured SOP (today's ordinary meaning).
	SopGenerated bool `yaml:"sop_generated,omitempty"`
	// SkillDir/SkillFiles record the leadkit-adopt skill snapshot (adoption-onboard
	// deliverable 3) — SkillDir is "" when nothing was captured (no live SkillsDir
	// yet, see captureSkill).
	SkillDir   string         `yaml:"skill_dir,omitempty"`
	SkillFiles []CapturedFile `yaml:"skill_files,omitempty"`
	Roster     []RosterSpec   `yaml:"roster,omitempty"`
	InboxLanes int            `yaml:"inbox_lanes"`
}

// CaptureResult summarizes what Capture did/wrote.
type CaptureResult struct {
	TeamDir        string
	AgentDefs      []string // filenames copied, sorted
	ProtocolCopied bool
	SopCopied      bool
	// SopGenerated: the captured SOP is a leadkit-generated starter, not a real
	// hand-written one — see captureSop.
	SopGenerated bool
	// SkillFiles: filenames captured from the live leadkit-adopt skill dir, sorted
	// — empty when there was nothing live to capture (see captureSkill).
	SkillFiles []string
	InboxLanes int
}

// nowFunc is the clock CapturedManifest's timestamp uses — injectable so tests can
// assert an exact captured_at value.
var nowFunc = time.Now

// Capture snapshots cfg's source artifacts into cfg.TeamDir: every *.md in AgentsDir
// (except a leading-underscore file, e.g. the protocol doc itself, which is copied
// separately below) into <TeamDir>/agents/, ProtocolFile to
// <TeamDir>/_team-protocol.md, SopFile to <TeamDir>/AUTONOMOUS-TEAM-SOP.md, plus
// inbox-manifest.yaml (from RolesPath) and captured.yaml (the manifest-of-everything).
// Read-only on every source path; every write stays inside cfg.TeamDir. Idempotent —
// re-running overwrites the prior snapshot with the current state.
func Capture(cfg Config) (CaptureResult, error) {
	var res CaptureResult
	res.TeamDir = cfg.TeamDir

	agentsOutDir := filepath.Join(cfg.TeamDir, "agents")
	if err := os.MkdirAll(agentsOutDir, 0o755); err != nil {
		return res, fmt.Errorf("team: mkdir %s: %w", agentsOutDir, err)
	}

	entries, err := os.ReadDir(cfg.AgentsDir)
	if err != nil {
		return res, fmt.Errorf("team: read agents_dir %s: %w", cfg.AgentsDir, err)
	}
	var capturedFiles []CapturedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, "_") {
			continue // "_"-prefixed: the protocol doc (captured separately) or similar meta files
		}
		src := filepath.Join(cfg.AgentsDir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			return res, fmt.Errorf("team: read %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(agentsOutDir, name), data, 0o644); err != nil {
			return res, fmt.Errorf("team: write %s: %w", name, err)
		}
		res.AgentDefs = append(res.AgentDefs, name)
		capturedFiles = append(capturedFiles, CapturedFile{Name: name, Size: int64(len(data))})
	}
	sort.Strings(res.AgentDefs)
	sort.Slice(capturedFiles, func(i, j int) bool { return capturedFiles[i].Name < capturedFiles[j].Name })

	var protocolFile *CapturedFile
	if cfg.ProtocolFile != "" {
		data, err := os.ReadFile(cfg.ProtocolFile)
		if err != nil {
			return res, fmt.Errorf("team: read protocol_file %s: %w", cfg.ProtocolFile, err)
		}
		if err := os.WriteFile(filepath.Join(cfg.TeamDir, "_team-protocol.md"), data, 0o644); err != nil {
			return res, fmt.Errorf("team: write _team-protocol.md: %w", err)
		}
		res.ProtocolCopied = true
		protocolFile = &CapturedFile{Name: "_team-protocol.md", Size: int64(len(data))}
	}

	sopFile, sopGenerated, sopWritten, err := captureSop(cfg)
	if err != nil {
		return res, err
	}
	res.SopCopied = sopWritten
	res.SopGenerated = sopGenerated

	skillFiles, skillDirName, err := captureSkill(cfg)
	if err != nil {
		return res, err
	}
	res.SkillFiles = make([]string, 0, len(skillFiles))
	for _, f := range skillFiles {
		res.SkillFiles = append(res.SkillFiles, f.Name)
	}

	manifest := BuildInboxManifest(cfg.RolesPath)
	res.InboxLanes = len(manifest.Lanes)
	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		return res, fmt.Errorf("team: marshal inbox-manifest.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.TeamDir, "inbox-manifest.yaml"), manifestData, 0o644); err != nil {
		return res, fmt.Errorf("team: write inbox-manifest.yaml: %w", err)
	}

	captured := CapturedManifest{
		CapturedAt: nowFunc().UTC().Format(time.RFC3339),
		Source: CapturedSource{
			AgentsDir: cfg.AgentsDir, ProtocolFile: cfg.ProtocolFile,
			SopFile: cfg.SopFile, RolesPath: cfg.RolesPath,
		},
		AgentDefs:    capturedFiles,
		Protocol:     protocolFile,
		Sop:          sopFile,
		SopGenerated: sopGenerated,
		SkillDir:     skillDirName,
		SkillFiles:   skillFiles,
		Roster:       cfg.Roster,
		InboxLanes:   res.InboxLanes,
	}
	capturedData, err := yaml.Marshal(captured)
	if err != nil {
		return res, fmt.Errorf("team: marshal captured.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.TeamDir, "captured.yaml"), capturedData, 0o644); err != nil {
		return res, fmt.Errorf("team: write captured.yaml: %w", err)
	}

	return res, nil
}
