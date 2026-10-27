package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFixture builds a captured team_dir (via a real Capture) plus a SEPARATE,
// initially-empty set of destination paths (a fresh-machine simulation) — install's
// job is to reconstruct the live paths FROM the captured snapshot.
func installFixture(t *testing.T) (capturedCfg Config, destAgentsDir, destProtocol, destSop, queueRoot string) {
	t.Helper()
	_, capturedCfg = buildFixture(t)
	if _, err := Capture(capturedCfg); err != nil {
		t.Fatal(err)
	}

	fresh := t.TempDir()
	destAgentsDir = filepath.Join(fresh, "agents")
	destProtocol = filepath.Join(destAgentsDir, "_team-protocol.md")
	destSop = filepath.Join(fresh, "SOP.md")
	queueRoot = filepath.Join(fresh, "queue")
	return capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot
}

func destConfig(cfg Config, destAgentsDir, destProtocol, destSop string) Config {
	return Config{
		AgentsDir: destAgentsDir, ProtocolFile: destProtocol, SopFile: destSop,
		TeamDir: cfg.TeamDir, RolesPath: cfg.RolesPath,
	}
}

func TestComputeInstallPlan_FreshMachine_EverythingChanged(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.AgentDefs) != 2 {
		t.Fatalf("want 2 agent defs planned, got %+v", plan.AgentDefs)
	}
	for _, f := range plan.AgentDefs {
		if !f.Changed {
			t.Errorf("agent def %s should be Changed=true on a fresh machine", f.Name)
		}
	}
	if plan.Protocol == nil || !plan.Protocol.Changed {
		t.Errorf("want Protocol Changed=true, got %+v", plan.Protocol)
	}
	if plan.Sop == nil || !plan.Sop.Changed {
		t.Errorf("want Sop Changed=true, got %+v", plan.Sop)
	}
	totalMissing := 0
	for _, l := range plan.Lanes {
		totalMissing += len(l.Missing)
	}
	if totalMissing != 9 { // 3 lanes * 3 subdirs, all missing
		t.Errorf("want 9 missing lane dirs (3 lanes x 3 subdirs), got %d: %+v", totalMissing, plan.Lanes)
	}
	if !plan.AnyChanged() {
		t.Error("AnyChanged() should be true on a fresh machine")
	}
}

func TestApplyInstallPlan_DryRunNeverCalled_WritesNothing(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	_ = FormatInstallPlan(plan) // the dry-run path: compute + print, never call Apply
	if _, err := os.Stat(destAgentsDir); !os.IsNotExist(err) {
		t.Error("dry-run must not create the dest agents dir")
	}
}

func TestApplyInstallPlan_WritesDefsAndCreatesLanes(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ApplyInstallPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.WrittenFiles) != 4 { // 2 agent defs + protocol + sop
		t.Errorf("WrittenFiles = %v, want 4 entries", res.WrittenFiles)
	}
	if len(res.CreatedDirs) != 9 {
		t.Errorf("CreatedDirs = %v, want 9", res.CreatedDirs)
	}
	if len(res.BackedUpTo) != 0 {
		t.Errorf("nothing pre-existed — want no backups, got %+v", res.BackedUpTo)
	}

	for _, want := range []string{
		filepath.Join(destAgentsDir, "lead.md"),
		filepath.Join(destAgentsDir, "devops-agent.md"),
		destProtocol, destSop,
		filepath.Join(queueRoot, "devops", "pending"),
		filepath.Join(queueRoot, "cr-1", "done"),
		filepath.Join(queueRoot, "crit-2", "in-progress"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected to exist after install: %s (%v)", want, err)
		}
	}
}

// seedCustomised writes an adopter's own version of the lead definition, the protocol
// and the SOP at the destination, and returns their paths and contents.
func seedCustomised(t *testing.T, destAgentsDir, destProtocol, destSop string) map[string]string {
	t.Helper()
	files := map[string]string{
		filepath.Join(destAgentsDir, "lead.md"): "# lead\nMY OWN rules, not the shipped ones\n",
		destProtocol:                            "my protocol\n",
		destSop:                                 "my sop\n",
	}
	for path, body := range files {
		mustWrite(t, path, body)
	}
	return files
}

// The contract of ticket 135 S0: install is create-only. A definition (and the protocol and
// SOP, which go through the same writer) that already exists and differs is KEPT, byte for
// byte, with no backup made and nothing reported as written.
func TestApplyInstallPlan_KeepsADifferingExistingFile(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	mine := seedCustomised(t, destAgentsDir, destProtocol, destSop)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ApplyInstallPlan(plan)
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range mine {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s was replaced; install must keep a file that exists and differs.\ngot:  %q\nwant: %q", path, got, want)
		}
	}
	if len(res.BackedUpTo) != 0 {
		t.Errorf("nothing was overwritten, so no backup should exist; got %+v", res.BackedUpTo)
	}
	if matches, _ := filepath.Glob(filepath.Join(destAgentsDir, "*.bak-*")); len(matches) != 0 {
		t.Errorf("no .bak files should be written when nothing is replaced, got %v", matches)
	}
	for _, name := range res.WrittenFiles {
		if name == "lead.md" || name == "_team-protocol.md" || name == "AUTONOMOUS-TEAM-SOP.md" {
			t.Errorf("%s reported as written, but it was kept", name)
		}
	}
	for _, want := range []string{"lead.md", "_team-protocol.md", "AUTONOMOUS-TEAM-SOP.md"} {
		found := false
		for _, k := range res.KeptFiles {
			found = found || k == want
		}
		if !found {
			t.Errorf("KeptFiles = %v, want it to name %s", res.KeptFiles, want)
		}
	}
	// A file that does not exist yet is still created: create-only is not do-nothing.
	if _, err := os.Stat(filepath.Join(destAgentsDir, "devops-agent.md")); err != nil {
		t.Errorf("the absent definition should still be created: %v", err)
	}
}

// The kept decision must hold at write time too, not only at plan time: a file that shows
// up between planning and applying is the adopter's and must not be replaced.
func TestApplyInstallPlan_KeepsAFileThatAppearedAfterPlanning(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlan(dc, queueRoot) // lead.md absent now, so planned as a create
	if err != nil {
		t.Fatal(err)
	}
	lead := filepath.Join(destAgentsDir, "lead.md")
	mustWrite(t, lead, "written after the plan was computed\n")
	res, err := ApplyInstallPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(lead)
	if string(got) != "written after the plan was computed\n" {
		t.Errorf("a file that appeared after planning was replaced: %q", got)
	}
	if len(res.BackedUpTo) != 0 {
		t.Errorf("unexpected backup: %+v", res.BackedUpTo)
	}
}

// The explicit escape hatch: ReplaceExisting overwrites a differing file and still takes a
// backup first. This is also the test that the backup path works for what IS overwritten.
func TestApplyInstallPlan_ReplaceExisting_BacksUpThenOverwrites(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	mine := seedCustomised(t, destAgentsDir, destProtocol, destSop)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	plan, err := ComputeInstallPlanWithOptions(dc, queueRoot, InstallOptions{ReplaceExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ApplyInstallPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.KeptFiles) != 0 {
		t.Errorf("nothing should be kept under ReplaceExisting, got %v", res.KeptFiles)
	}
	for name, path := range map[string]string{
		"lead.md": filepath.Join(destAgentsDir, "lead.md"), "_team-protocol.md": destProtocol, "AUTONOMOUS-TEAM-SOP.md": destSop,
	} {
		backupPath, ok := res.BackedUpTo[name]
		if !ok {
			t.Fatalf("want a backup recorded for %s, got %+v", name, res.BackedUpTo)
		}
		backupData, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(backupData) != mine[path] {
			t.Errorf("%s backup = %q, want the pre-install content %q", name, backupData, mine[path])
		}
		newData, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(newData) == mine[path] {
			t.Errorf("%s should hold the source content under ReplaceExisting, not the old local edit", name)
		}
	}
}

// Plan-level contract: Kept and Changed are mutually exclusive, and only a kept file is
// excluded from AnyChanged.
func TestComputeInstallPlan_ExistingDifferingFile_IsKeptNotChanged(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	seedCustomised(t, destAgentsDir, destProtocol, destSop)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.AgentDefs {
		switch f.Name {
		case "lead.md":
			if !f.Kept || f.Changed || !f.Exists {
				t.Errorf("lead.md: want Kept && !Changed && Exists, got %+v", f)
			}
		default:
			if f.Kept || !f.Changed {
				t.Errorf("%s is absent, want Changed && !Kept, got %+v", f.Name, f)
			}
		}
	}
	if !plan.Protocol.Kept || plan.Protocol.Changed || !plan.Sop.Kept || plan.Sop.Changed {
		t.Errorf("protocol and SOP must be kept too: %+v / %+v", plan.Protocol, plan.Sop)
	}
	if got := strings.Join(plan.KeptFiles(), ","); got != "agents/lead.md,protocol,sop" {
		t.Errorf("KeptFiles = %q", got)
	}
}

func TestApplyInstallPlan_CreateOnly_NeverTouchesAnExistingLane(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	// Pre-create one lane with a "ticket" already in it.
	existingLaneFile := filepath.Join(queueRoot, "devops", "pending", "42-some-ticket.md")
	mustWrite(t, existingLaneFile, "pre-existing ticket, must survive install")

	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyInstallPlan(plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(existingLaneFile)
	if err != nil {
		t.Fatalf("pre-existing ticket must survive install untouched: %v", err)
	}
	if string(data) != "pre-existing ticket, must survive install" {
		t.Error("pre-existing ticket content changed — install must be create-only")
	}
}

func TestComputeInstallPlan_AlreadyInstalled_NothingChanged(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	plan1, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyInstallPlan(plan1); err != nil {
		t.Fatal(err)
	}

	plan2, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.AnyChanged() {
		t.Errorf("second ComputeInstallPlan over an already-installed tree should find nothing to do, got %+v", plan2)
	}
	res2, err := ApplyInstallPlan(plan2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.WrittenFiles) != 0 || len(res2.CreatedDirs) != 0 || len(res2.BackedUpTo) != 0 {
		t.Errorf("idempotent second apply should do nothing, got %+v", res2)
	}
}

// A customised definition already on disk must not be planned under the same "+" line a
// brand-new file gets, and by default it is not replaced at all: the plan says it is kept.
func TestFormatInstallPlan_ExistingDifferingFile_IsMarkedKeptYours(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	mustWrite(t, filepath.Join(destAgentsDir, "lead.md"), "hand-written lead\n")
	mustWrite(t, destProtocol, "hand-written protocol\n")
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)

	for _, tc := range []struct {
		name string
		opts InstallOptions
		want string
	}{
		{"captured", InstallOptions{}, "captured copy"},
		{"embedded", InstallOptions{PreferEmbedded: true}, "embedded copy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := ComputeInstallPlanWithOptions(dc, queueRoot, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			out := FormatInstallPlan(plan)
			var leadLine, protoLine string
			addedOther := false
			for _, ln := range strings.Split(out, "\n") {
				switch {
				case strings.Contains(ln, "agents/lead.md"):
					leadLine = ln
				case strings.Contains(ln, "protocol"):
					protoLine = ln
				case strings.HasPrefix(ln, "+ agents/"):
					addedOther = true
				}
			}
			for _, ln := range []string{leadLine, protoLine} {
				if !strings.HasPrefix(ln, "= kept yours ") || !strings.Contains(ln, "differs from the "+tc.want) {
					t.Errorf("an existing differing file must print as kept yours naming the %s; got %q\nfull plan:\n%s", tc.want, ln, out)
				}
			}
			if strings.Contains(out, "REPLACE") {
				t.Errorf("create-only install must not claim to replace anything:\n%s", out)
			}
			if !strings.Contains(out, "2 existing file(s) differ from their source and were kept as yours") || !strings.Contains(out, "--replace-existing") {
				t.Errorf("want a summary counting the 2 kept files and naming the escape hatch, got:\n%s", out)
			}
			if tc.name == "captured" && !addedOther {
				t.Errorf("the other (new) agent def should still print as '+', got:\n%s", out)
			}
		})
	}
}

// Under the explicit escape hatch the same files print as REPLACES, with the .bak promise.
func TestFormatInstallPlan_ReplaceExisting_IsMarkedReplace(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	mustWrite(t, filepath.Join(destAgentsDir, "lead.md"), "hand-written lead\n")
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlanWithOptions(dc, queueRoot, InstallOptions{ReplaceExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	out := FormatInstallPlan(plan)
	if !strings.Contains(out, "~ REPLACES agents/lead.md") || !strings.Contains(out, ".bak") || strings.Contains(out, "kept yours") {
		t.Errorf("want a REPLACES line with the .bak promise and nothing kept, got:\n%s", out)
	}
}

// A plan whose only differences are kept files has nothing to write, and must not say the
// files "already match".
func TestFormatInstallPlan_OnlyKeptFiles_DoesNotClaimTheyMatch(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	first, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyInstallPlan(first); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(destAgentsDir, "lead.md"), "edited after install\n")
	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AnyChanged() {
		t.Errorf("a kept file is not a change: %+v", plan)
	}
	out := FormatInstallPlan(plan)
	if strings.Contains(out, "already matches") || !strings.Contains(out, "kept as yours") {
		t.Errorf("want 'kept as yours' and no 'already matches' claim, got:\n%s", out)
	}
}

// A destination that already matches is neither added nor replaced; and a plan with only
// creates carries no replace wording.
func TestFormatInstallPlan_FreshMachine_NoReplaceWording(t *testing.T) {
	capturedCfg, destAgentsDir, destProtocol, destSop, queueRoot := installFixture(t)
	dc := destConfig(capturedCfg, destAgentsDir, destProtocol, destSop)
	plan, err := ComputeInstallPlan(dc, queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatInstallPlan(plan); strings.Contains(out, "REPLACE") {
		t.Errorf("a fresh machine replaces nothing, yet the plan says so:\n%s", out)
	}
}
