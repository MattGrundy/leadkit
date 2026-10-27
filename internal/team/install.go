package team

// `leadkit team install` reconstructs the team from a captured snapshot onto a
// (possibly fresh) machine — the only mutating command in this package, guarded the
// same way `init`/`audit --apply` are: dry-run default, --apply --confirm required
// (cmd/leadkit's shared confirmGuard).
//
// Install is CREATE-ONLY. A file that does not exist is created; one that already exists
// and differs is KEPT, reported as "kept yours", and never replaced. That covers agent
// definitions, the team protocol and the SOP alike: all are text the adopter is expected
// to edit, and none of them can be told apart from an edit by comparing bytes with the
// source (a stale-but-pristine copy and a customised one both just "differ"), so the only
// safe default is to leave an existing file alone. A lane dir is likewise create-only.
//
// The one way to overwrite is the explicit InstallOptions.ReplaceExisting
// (`--replace-existing`), which is never a default and still copies the original to a
// .bak-<timestamp> first. It is the upgrade and reset path: without it, taking a newer
// shipped definition (or discarding a bad edit) would mean deleting files by hand.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/starter"
)

// InstallFileItem is one file install would (or did) write.
type InstallFileItem struct {
	Name     string
	DestPath string
	Content  []byte
	Changed  bool // Apply will write DestPath: it is absent, or present and being replaced
	// Exists is true when DestPath is already on disk. With Changed it means Apply will
	// REPLACE a file whose content differs, not create one; that only happens under
	// ReplaceExisting, and the plan says so because the file may be the adopter's own.
	Exists bool
	// Kept is true when DestPath exists, differs from Content, and will be left alone
	// because install is create-only. Never set together with Changed.
	Kept bool
}

// InstallLaneItem is one manifest lane's dir-creation plan.
type InstallLaneItem struct {
	Lane    string
	Subdirs []string // every expected subdir name (pending/in-progress/done)
	Missing []string // full paths (under the live queue root) that don't exist yet
}

// InstallPlan is `team install`'s full plan — computed by ComputeInstallPlan,
// executed by ApplyInstallPlan.
type InstallPlan struct {
	AgentDefs []InstallFileItem
	Protocol  *InstallFileItem
	Sop       *InstallFileItem
	Lanes     []InstallLaneItem
	// Provenance, so the plan can say where the content came from rather than presenting
	// the binary's own starters as though they were the team's captured definitions.
	AgentDefsFromEmbedded bool
	ProtocolFromEmbedded  bool
	// DeprecatedCaptured names captured definitions that declare themselves deprecated or
	// retired. Installing one onto a fresh machine teaches a pattern the authoring team
	// abandoned, and the notice saying so rarely survives being copied.
	DeprecatedCaptured []string
	// ReplaceExisting records that the plan was computed under --replace-existing, so Apply
	// knows it may overwrite (with a backup). False is the default and means create-only.
	ReplaceExisting bool
}

// declaresDeprecated reports whether a definition says, in its own words, that it is
// retired. Checked on the leading portion so a passing mention further down does not count.
func declaresDeprecated(data []byte) bool {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	low := strings.ToLower(string(head))
	return strings.Contains(low, "deprecated") || strings.Contains(low, "retired")
}

// KeptFiles returns the labels of the files install is leaving alone because they exist
// and differ, in plan order.
func (p InstallPlan) KeptFiles() []string {
	var out []string
	for _, f := range p.AgentDefs {
		if f.Kept {
			out = append(out, "agents/"+f.Name)
		}
	}
	if p.Protocol != nil && p.Protocol.Kept {
		out = append(out, "protocol")
	}
	if p.Sop != nil && p.Sop.Kept {
		out = append(out, "sop")
	}
	return out
}

// AnyChanged reports whether Apply would write or create anything.
func (p InstallPlan) AnyChanged() bool {
	for _, f := range p.AgentDefs {
		if f.Changed {
			return true
		}
	}
	if p.Protocol != nil && p.Protocol.Changed {
		return true
	}
	if p.Sop != nil && p.Sop.Changed {
		return true
	}
	for _, l := range p.Lanes {
		if len(l.Missing) > 0 {
			return true
		}
	}
	return false
}

// ComputeInstallPlan is pure except for reads (the captured snapshot in cfg.TeamDir,
// and the live destination paths, to detect what already matches) — it writes
// nothing. queueRoot is where manifest lanes get created under.
// InstallOptions tunes where definitions come from.
type InstallOptions struct {
	// PreferEmbedded ignores any captured snapshot and installs the definitions embedded in
	// the binary. Needed because a snapshot is a point-in-time copy of one team's machine:
	// it can carry that project's specifics, and it can carry roles that have since been
	// retired. A captured set is the right default — it is the team's own — but it is the
	// wrong thing to hand a different machine.
	PreferEmbedded bool
	// ReplaceExisting lets install overwrite an existing file whose content differs (an
	// original is still copied to a .bak-<timestamp> first). Off by default: install is
	// create-only, and an adopter's customised definition is kept.
	ReplaceExisting bool
}

// ComputeInstallPlan keeps the original signature: captured snapshot preferred, embedded
// definitions as the fallback.
func ComputeInstallPlan(cfg Config, queueRoot string) (InstallPlan, error) {
	return ComputeInstallPlanWithOptions(cfg, queueRoot, InstallOptions{})
}

// ComputeInstallPlanWithOptions is ComputeInstallPlan with source selection.
//
// Install is the only place leadkit resolves which agent definition a machine has, and the rule
// is the destination's: a definition already in cfg.AgentsDir that differs from its source is
// kept (opts.ReplaceExisting overrides, with a backup), so the installed file wins. What a
// session then uses is Claude Code's decision (user and project agents, a session --agents
// override); leadkit applies no precedence of its own, and does not model one.
func ComputeInstallPlanWithOptions(cfg Config, queueRoot string, opts InstallOptions) (InstallPlan, error) {
	plan := InstallPlan{ReplaceExisting: opts.ReplaceExisting}

	// Prefer a captured snapshot; fall back to the definitions embedded in the binary.
	//
	// Without the fallback, installing a team requires a checkout of THIS repo, which makes
	// a clone a prerequisite for a tool shipped as a single binary. It also makes the
	// reviewer roles unavailable on a fresh machine: the harness can only offer an agent
	// type whose definition is installed, so with no definitions present there is no
	// code-review or critique agent to spawn, and the two-layer gate cannot be staffed at
	// all. The embedded set is what makes a fresh machine able to review its own work.
	agentsSrcDir := filepath.Join(cfg.TeamDir, "agents")
	captured := map[string][]byte{}
	entries, readErr := os.ReadDir(agentsSrcDir)
	if readErr == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(agentsSrcDir, name))
			if err != nil {
				return plan, fmt.Errorf("team install: read %s: %w", name, err)
			}
			captured[name] = data
		}
	} else if !os.IsNotExist(readErr) {
		return plan, fmt.Errorf("team install: read %s: %w", agentsSrcDir, readErr)
	}

	source := captured
	plan.AgentDefsFromEmbedded = len(captured) == 0 || opts.PreferEmbedded
	if plan.AgentDefsFromEmbedded {
		embedded, err := starter.Agents()
		if err != nil {
			return plan, fmt.Errorf("team install: %w", err)
		}
		source = embedded
	}
	for name, data := range source {
		dest := filepath.Join(cfg.AgentsDir, name)
		plan.AgentDefs = append(plan.AgentDefs, newInstallItem(name, dest, data, opts.ReplaceExisting))
	}
	sort.Slice(plan.AgentDefs, func(i, j int) bool { return plan.AgentDefs[i].Name < plan.AgentDefs[j].Name })
	// Built from the sorted slice, not the map, so the warning names them in a stable order.
	if !plan.AgentDefsFromEmbedded {
		for _, f := range plan.AgentDefs {
			if declaresDeprecated(f.Content) {
				plan.DeprecatedCaptured = append(plan.DeprecatedCaptured, f.Name)
			}
		}
	}

	protocolData, protoErr := os.ReadFile(filepath.Join(cfg.TeamDir, "_team-protocol.md"))
	if protoErr != nil || opts.PreferEmbedded {
		// Same reasoning as the agent defs: a fresh machine has no captured protocol, and
		// a team with no protocol has nothing telling it how to coordinate.
		protocolData = starter.TeamProtocol()
		plan.ProtocolFromEmbedded = true
	}
	proto := newInstallItem("_team-protocol.md", cfg.ProtocolFile, protocolData, opts.ReplaceExisting)
	plan.Protocol = &proto
	if data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "AUTONOMOUS-TEAM-SOP.md")); err == nil {
		sop := newInstallItem("AUTONOMOUS-TEAM-SOP.md", cfg.SopFile, data, opts.ReplaceExisting)
		plan.Sop = &sop
	}

	if data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "inbox-manifest.yaml")); err == nil {
		var m InboxManifest
		if yaml.Unmarshal(data, &m) == nil {
			for _, lane := range m.Lanes {
				item := InstallLaneItem{Lane: lane.Lane, Subdirs: lane.Subdirs}
				for _, sub := range lane.Subdirs {
					dir := filepath.Join(queueRoot, lane.Lane, sub)
					if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
						item.Missing = append(item.Missing, dir)
					}
				}
				plan.Lanes = append(plan.Lanes, item)
			}
		}
	}

	return plan, nil
}

// newInstallItem decides what install does with one file. This is where install is
// create-only: an existing file that differs is Kept, not Changed, unless the caller
// explicitly asked to replace.
func newInstallItem(name, dest string, content []byte, replaceExisting bool) InstallFileItem {
	item := InstallFileItem{Name: name, DestPath: dest, Content: content, Exists: fileExists(dest)}
	differs := !sameContent(dest, content)
	switch {
	case !differs:
		// already has this exact content: nothing to do
	case item.Exists && !replaceExisting:
		item.Kept = true
	default:
		item.Changed = true
	}
	return item
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sameContent(path string, want []byte) bool {
	existing, err := os.ReadFile(path)
	return err == nil && bytes.Equal(existing, want)
}

// InstallApplyResult is what ApplyInstallPlan actually did.
type InstallApplyResult struct {
	WrittenFiles []string          // names (agent defs + protocol + sop) actually written
	BackedUpTo   map[string]string // name -> backup path, for anything that was overwritten (only under ReplaceExisting)
	KeptFiles    []string          // names left untouched because they exist and differ (create-only)
	CreatedDirs  []string          // full paths created (lane subdirs)
}

// ApplyInstallPlan performs plan's writes: every Changed file is written atomically
// (temp file + rename), backing up whatever it replaces first; every Missing lane
// dir is created (os.MkdirAll — create-only, an existing lane is never touched).
//
// Without plan.ReplaceExisting a file that has appeared at the destination since the plan
// was computed is kept too, so create-only holds at the moment of writing and not only at
// the moment of planning.
func ApplyInstallPlan(plan InstallPlan) (InstallApplyResult, error) {
	res := InstallApplyResult{BackedUpTo: map[string]string{}}

	writeItem := func(item InstallFileItem) error {
		if item.Kept || (item.Changed && !plan.ReplaceExisting && fileExists(item.DestPath)) {
			res.KeptFiles = append(res.KeptFiles, item.Name)
			return nil
		}
		if !item.Changed {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(item.DestPath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(item.DestPath), err)
		}
		if existing, err := os.ReadFile(item.DestPath); err == nil {
			backup := fmt.Sprintf("%s.bak-%s", item.DestPath, time.Now().UTC().Format("20060102T150405Z"))
			if err := os.WriteFile(backup, existing, 0o644); err != nil {
				return fmt.Errorf("backup %s: %w", backup, err)
			}
			res.BackedUpTo[item.Name] = backup
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", item.DestPath, err)
		}

		dir := filepath.Dir(item.DestPath)
		tmp, err := os.CreateTemp(dir, ".leadkit-team-*.tmp")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(item.Content); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("write temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("close temp file: %w", err)
		}
		if err := os.Rename(tmpPath, item.DestPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("rename into place: %w", err)
		}
		res.WrittenFiles = append(res.WrittenFiles, item.Name)
		return nil
	}

	for _, item := range plan.AgentDefs {
		if err := writeItem(item); err != nil {
			return res, fmt.Errorf("team install: %s: %w", item.Name, err)
		}
	}
	if plan.Protocol != nil {
		if err := writeItem(*plan.Protocol); err != nil {
			return res, fmt.Errorf("team install: protocol: %w", err)
		}
	}
	if plan.Sop != nil {
		if err := writeItem(*plan.Sop); err != nil {
			return res, fmt.Errorf("team install: sop: %w", err)
		}
	}

	for _, lane := range plan.Lanes {
		for _, dir := range lane.Missing {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, fmt.Errorf("team install: mkdir %s: %w", dir, err)
			}
			res.CreatedDirs = append(res.CreatedDirs, dir)
		}
	}

	return res, nil
}

// FormatInstallPlan renders plan for `team install`'s output — printed for BOTH
// dry-run and a real apply (the plan IS what a real run acts on). A file that will be
// created prints "+"; an existing file that differs prints "= kept yours" and is left
// alone; under --replace-existing it prints "~ REPLACES" instead, so a customised
// definition is never overwritten under the same line a new file gets.
func FormatInstallPlan(plan InstallPlan) string {
	var b strings.Builder
	replaces, kept := 0, 0
	line := func(f InstallFileItem, label string) {
		if f.Kept {
			kept++
			fmt.Fprintf(&b, "= kept yours %s -> %s (differs from the %s; left untouched)\n", label, f.DestPath, plan.sourceLabel(label))
			return
		}
		if !f.Changed {
			return
		}
		if f.Exists {
			replaces++
			fmt.Fprintf(&b, "~ REPLACES %s -> %s (differs from the %s; a .bak is kept)\n", label, f.DestPath, plan.sourceLabel(label))
			return
		}
		fmt.Fprintf(&b, "+ %s -> %s\n", label, f.DestPath)
	}
	for _, f := range plan.AgentDefs {
		line(f, "agents/"+f.Name)
	}
	if plan.Protocol != nil {
		line(*plan.Protocol, "protocol")
	}
	if plan.Sop != nil {
		line(*plan.Sop, "sop")
	}
	for _, lane := range plan.Lanes {
		for _, dir := range lane.Missing {
			fmt.Fprintf(&b, "+ mkdir %s (lane %s)\n", dir, lane.Lane)
		}
	}
	if replaces > 0 {
		fmt.Fprintf(&b, "%d existing file(s) will be REPLACED; each original is copied to a .bak-<timestamp> first\n", replaces)
	}
	if kept > 0 {
		fmt.Fprintf(&b, "%d existing file(s) differ from their source and were kept as yours; install never replaces a file that exists. To overwrite them deliberately, re-run with --replace-existing (each original is copied to a .bak-<timestamp> first)\n", kept)
	}
	if !plan.AnyChanged() {
		if kept > 0 {
			b.WriteString("nothing to write — only files you already have, kept as yours\n")
		} else {
			b.WriteString("nothing to do — every file and lane already matches its source\n")
		}
	}
	return b.String()
}

// sourceLabel names where the replacing content comes from, for the REPLACES line.
func (p InstallPlan) sourceLabel(label string) string {
	embedded := p.AgentDefsFromEmbedded
	switch label {
	case "protocol":
		embedded = p.ProtocolFromEmbedded
	case "sop":
		embedded = false // the SOP has no embedded fallback: it only ever comes from a snapshot
	}
	if embedded {
		return "embedded copy"
	}
	return "captured copy"
}
