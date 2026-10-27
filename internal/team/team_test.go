package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// buildFixture lays out a throwaway agents_dir + protocol + sop + ROLES, mirroring
// the real shape (20-ish agent defs, an underscore-prefixed protocol file living
// alongside them, prose/comments in ROLES that must be skipped).
func buildFixture(t *testing.T) (root string, cfg Config) {
	t.Helper()
	root = t.TempDir()
	agentsDir := filepath.Join(root, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(agentsDir, "lead.md"), "# lead\nthe lead agent\n")
	mustWrite(t, filepath.Join(agentsDir, "devops-agent.md"), "# devops\n")
	mustWrite(t, filepath.Join(agentsDir, "_team-protocol.md"), "# protocol\nstanding pool rules\n")

	sopPath := filepath.Join(root, "SOP.md")
	mustWrite(t, sopPath, "# SOP\nhow the team works\n")

	rolesPath := filepath.Join(root, "ROLES")
	mustWrite(t, rolesPath, `# role -> live agent name, comments/prose must be skipped
# this line has no second field and must not become a lane:
dev-devops      devops
code-review     cr-1
critique        crit-2

# a stray transient-looking note, not a real row:
# cr-b-drain fix-something owner-479a should never appear as a lane
`)

	teamDir := filepath.Join(root, "team")
	cfg = Config{
		AgentsDir: agentsDir, ProtocolFile: filepath.Join(agentsDir, "_team-protocol.md"),
		SopFile: sopPath, TeamDir: teamDir, RolesPath: rolesPath,
		Roster: []RosterSpec{{Type: "code-review", Count: 1}, {Type: "critique", Count: 1}},
	}
	return root, cfg
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCapture_CopiesAgentDefsAndProtocolAndSop(t *testing.T) {
	_, cfg := buildFixture(t)
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AgentDefs) != 2 || res.AgentDefs[0] != "devops-agent.md" || res.AgentDefs[1] != "lead.md" {
		t.Errorf("AgentDefs = %v, want [devops-agent.md lead.md] (the protocol file must be excluded)", res.AgentDefs)
	}
	if !res.ProtocolCopied || !res.SopCopied {
		t.Errorf("want both ProtocolCopied and SopCopied, got %+v", res)
	}

	for _, want := range []string{
		filepath.Join(cfg.TeamDir, "agents", "lead.md"),
		filepath.Join(cfg.TeamDir, "agents", "devops-agent.md"),
		filepath.Join(cfg.TeamDir, "_team-protocol.md"),
		filepath.Join(cfg.TeamDir, "AUTONOMOUS-TEAM-SOP.md"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected file not written: %s (%v)", want, err)
		}
	}
	// The protocol file must NOT also land in agents/ (it's captured separately).
	if _, err := os.Stat(filepath.Join(cfg.TeamDir, "agents", "_team-protocol.md")); !os.IsNotExist(err) {
		t.Error("_team-protocol.md must not be duplicated into agents/")
	}
}

func TestCapture_InboxManifestIsCanonicalLanesOnly(t *testing.T) {
	_, cfg := buildFixture(t)
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.InboxLanes != 3 {
		t.Fatalf("InboxLanes = %d, want 3 (dev-devops, code-review, critique — prose/comments skipped)", res.InboxLanes)
	}

	data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "inbox-manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m InboxManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("inbox-manifest.yaml is not parseable YAML: %v\n%s", err, data)
	}
	if len(m.Lanes) != 3 {
		t.Fatalf("parsed %d lanes, want 3: %+v", len(m.Lanes), m.Lanes)
	}
	byRole := map[string]LaneEntry{}
	for _, l := range m.Lanes {
		byRole[l.Role] = l
	}
	if byRole["code-review"].Lane != "cr-1" {
		t.Errorf("code-review lane = %q, want cr-1", byRole["code-review"].Lane)
	}
	if got := byRole["code-review"].Subdirs; len(got) != 3 || got[0] != "pending" || got[2] != "done" {
		t.Errorf("subdirs = %v, want [pending in-progress done]", got)
	}
	// Nothing from the "transient-looking note" comment leaked in as a lane.
	for _, forbidden := range []string{"cr-b-drain", "fix-something", "owner-479a"} {
		if _, ok := byRole[forbidden]; ok {
			t.Errorf("transient-looking name %q must never appear as a captured lane", forbidden)
		}
	}
}

func TestCapture_WritesParseableCapturedYAML(t *testing.T) {
	_, cfg := buildFixture(t)
	fixedTime := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return fixedTime }
	defer func() { nowFunc = orig }()

	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "captured.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m CapturedManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("captured.yaml is not parseable YAML: %v\n%s", err, data)
	}
	if m.CapturedAt != "2026-09-05T12:00:00Z" {
		t.Errorf("captured_at = %q, want 2026-09-05T12:00:00Z", m.CapturedAt)
	}
	if len(m.AgentDefs) != 2 {
		t.Errorf("AgentDefs = %+v, want 2 entries with sizes", m.AgentDefs)
	}
	for _, f := range m.AgentDefs {
		if f.Size <= 0 {
			t.Errorf("captured file %q has non-positive size %d", f.Name, f.Size)
		}
	}
	if m.Protocol == nil || m.Sop == nil {
		t.Errorf("want both Protocol and Sop captured, got %+v", m)
	}
	if len(m.Roster) != 2 {
		t.Errorf("Roster = %+v, want the 2 entries from cfg.Roster", m.Roster)
	}
	if m.InboxLanes != 3 {
		t.Errorf("InboxLanes = %d, want 3", m.InboxLanes)
	}
	if m.Source.AgentsDir != cfg.AgentsDir || m.Source.RolesPath != cfg.RolesPath {
		t.Errorf("Source = %+v, want it to record cfg's paths", m.Source)
	}
}

func TestCapture_IsIdempotent(t *testing.T) {
	_, cfg := buildFixture(t)
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(cfg.TeamDir, "agents", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(cfg.TeamDir, "agents", "lead.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("re-capturing should overwrite with identical content, not diverge")
	}
}

func TestCapture_NeverTouchesSourcePaths(t *testing.T) {
	_, cfg := buildFixture(t)
	beforeAgents := snapshotDir(t, cfg.AgentsDir)
	beforeSop, _ := os.ReadFile(cfg.SopFile)

	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}

	afterAgents := snapshotDir(t, cfg.AgentsDir)
	afterSop, _ := os.ReadFile(cfg.SopFile)
	if len(beforeAgents) != len(afterAgents) {
		t.Fatalf("agents_dir file count changed: before=%d after=%d", len(beforeAgents), len(afterAgents))
	}
	for k, v := range beforeAgents {
		if afterAgents[k] != v {
			t.Errorf("source file %q changed — capture must be read-only on sources", k)
		}
	}
	if string(beforeSop) != string(afterSop) {
		t.Error("sop_file changed — capture must be read-only on sources")
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func TestBuildInboxManifest_MissingRolesFileYieldsEmptyNotError(t *testing.T) {
	m := BuildInboxManifest(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(m.Lanes) != 0 {
		t.Errorf("want 0 lanes for a missing ROLES file, got %+v", m.Lanes)
	}
}

func TestCapture_MissingSourceIsAnError(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.AgentsDir = filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Capture(cfg); err == nil {
		t.Error("want an error when agents_dir doesn't exist — capture should not silently produce an empty snapshot")
	}
}

// --- starter SOP (queue-mode-enforcement item 6 / GAP-5) ---

func TestCapture_EmptySopFileGeneratesStarter(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.SopFile = ""

	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SopCopied || !res.SopGenerated {
		t.Errorf("want SopCopied=true SopGenerated=true for an unset sop_file, got %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "AUTONOMOUS-TEAM-SOP.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), starterSOPMarker) {
		t.Errorf("generated SOP missing the starter marker:\n%s", data)
	}
	if !strings.Contains(string(data), "replace with your project's own") {
		t.Error("generated SOP should be clearly marked as a starter to replace")
	}

	capData, err := os.ReadFile(filepath.Join(cfg.TeamDir, "captured.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m CapturedManifest
	if err := yaml.Unmarshal(capData, &m); err != nil {
		t.Fatal(err)
	}
	if !m.SopGenerated {
		t.Error("captured.yaml must record sop_generated: true so `team status` can tell a starter from a captured SOP")
	}
}

func TestCapture_NonexistentSopFilePathAlsoGeneratesStarter(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.SopFile = filepath.Join(t.TempDir(), "does-not-exist-SOP.md")

	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SopGenerated {
		t.Errorf("a configured but missing sop_file should fall back to the starter, got %+v", res)
	}
}

func TestCapture_NeverOverwritesAnExistingHandWrittenSOP(t *testing.T) {
	_, cfg := buildFixture(t)
	// First capture with a real SopFile — a genuine hand-written SOP lands in
	// team_dir.
	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(cfg.TeamDir, "AUTONOMOUS-TEAM-SOP.md")
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}

	// Now sop_file goes missing (e.g. a stale/misconfigured path) — capture must
	// NEVER replace the real SOP already captured with a generic starter.
	cfg.SopFile = filepath.Join(t.TempDir(), "gone.md")
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.SopGenerated {
		t.Error("must not report SopGenerated=true — an existing hand-written SOP must never be treated as replaceable")
	}
	if res.SopCopied {
		t.Error("must not report SopCopied=true — nothing was (or should be) written this run")
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("existing hand-written SOP must be byte-for-byte unchanged:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestCapture_RegeneratingAnExistingStarterIsIdempotentNotClobbering(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.SopFile = ""

	if _, err := Capture(cfg); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(cfg.TeamDir, "AUTONOMOUS-TEAM-SOP.md")
	first, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run with sop_file still unset: a PRIOR generated starter is fine to
	// regenerate (it's still marked as a starter, not hand-written) — this must not
	// be treated as "an existing hand-written SOP".
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SopGenerated {
		t.Error("regenerating a prior starter should still report SopGenerated=true")
	}
	second, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("regenerating the starter from the same template must be byte-identical")
	}
}

// --- skill versioning (adoption-onboard deliverable 3) ---

func TestCapture_NoSkillsDirIsNotAnError(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.SkillsDir = "" // unset — nothing captured, still not an error.
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SkillFiles) != 0 {
		t.Errorf("SkillFiles = %v, want none", res.SkillFiles)
	}
}

func TestCapture_SkillsDirConfiguredButAbsentIsNotAnError(t *testing.T) {
	_, cfg := buildFixture(t)
	cfg.SkillsDir = filepath.Join(t.TempDir(), "leadkit-adopt") // configured, never created
	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SkillFiles) != 0 {
		t.Errorf("SkillFiles = %v, want none — init --apply simply hasn't run yet", res.SkillFiles)
	}
}

func TestCapture_SnapshotsLiveSkillIntoTeamDirAndCapturedYAML(t *testing.T) {
	root, cfg := buildFixture(t)
	skillsDir := filepath.Join(root, "skills", "leadkit-adopt")
	mustWrite(t, filepath.Join(skillsDir, "SKILL.md"), "---\nname: leadkit-adopt\n---\nbody\n")
	cfg.SkillsDir = skillsDir

	res, err := Capture(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SkillFiles) != 1 || res.SkillFiles[0] != "SKILL.md" {
		t.Errorf("SkillFiles = %v, want [SKILL.md]", res.SkillFiles)
	}
	captured, err := os.ReadFile(filepath.Join(cfg.TeamDir, "skills", "leadkit-adopt", "SKILL.md"))
	if err != nil {
		t.Fatalf("captured skill file not written: %v", err)
	}
	if string(captured) != "---\nname: leadkit-adopt\n---\nbody\n" {
		t.Errorf("captured content = %q, want the live skill's content unchanged", captured)
	}

	data, err := os.ReadFile(filepath.Join(cfg.TeamDir, "captured.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m CapturedManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.SkillDir != "leadkit-adopt" {
		t.Errorf("captured.yaml skill_dir = %q, want leadkit-adopt", m.SkillDir)
	}
	if len(m.SkillFiles) != 1 || m.SkillFiles[0].Name != "SKILL.md" || m.SkillFiles[0].Size <= 0 {
		t.Errorf("captured.yaml skill_files = %+v, want one SKILL.md entry with a positive size", m.SkillFiles)
	}
}

// The starter SOP is adopter-facing and teaches the gate chain; the gate has two layers, so it
// must not teach a third that nothing records.
func TestStarterSOP_TeachesTheTwoLayerGate(t *testing.T) {
	sop := starterSOP()
	if strings.Contains(sop, "outcome-verif") {
		t.Error("starter SOP teaches a third gate layer (outcome-verification) that nothing requires")
	}
	if !strings.Contains(sop, "code-review") || !strings.Contains(sop, "critique") {
		t.Error("starter SOP must still name the two gate layers")
	}
}
