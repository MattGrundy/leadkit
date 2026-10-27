package team

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/starter"
)

// These pin which source `team install` draws definitions from. The fallback to the
// embedded set is what lets a machine with no checkout staff a review gate at all, so a
// regression that silently installs nothing is invisible until someone needs a reviewer.

// sourceCfg is a destination config whose TeamDir the caller controls. Everything is under
// t.TempDir, so nothing touches the real ~/.claude.
func sourceCfg(t *testing.T) (Config, string) {
	t.Helper()
	root := t.TempDir()
	teamDir := filepath.Join(root, "team")
	return Config{
		AgentsDir:    filepath.Join(root, "dest", "agents"),
		ProtocolFile: filepath.Join(root, "dest", "agents", "_team-protocol.md"),
		SopFile:      filepath.Join(root, "dest", "SOP.md"),
		TeamDir:      teamDir,
	}, teamDir
}

func writeCaptured(t *testing.T, teamDir, name, body string) {
	t.Helper()
	mustWrite(t, filepath.Join(teamDir, "agents", name), body)
}

func planNames(p InstallPlan) []string {
	var out []string
	for _, f := range p.AgentDefs {
		out = append(out, f.Name)
	}
	return out
}

func TestComputeInstallPlan_NoSnapshot_FallsBackToEmbedded(t *testing.T) {
	cfg, _ := sourceCfg(t) // TeamDir does not exist at all
	embedded, err := starter.Agents()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{})
	if err != nil {
		t.Fatalf("a missing snapshot is the ordinary fresh-machine case, not an error: %v", err)
	}
	if !plan.AgentDefsFromEmbedded {
		t.Error("with no snapshot the plan must say its definitions came from the binary")
	}
	if len(plan.AgentDefs) != len(embedded) || len(embedded) == 0 {
		t.Fatalf("want all %d embedded definitions planned, got %v", len(embedded), planNames(plan))
	}
	for _, f := range plan.AgentDefs {
		if !bytes.Equal(f.Content, embedded[f.Name]) {
			t.Errorf("%s: planned content is not the embedded definition", f.Name)
		}
	}
	if !plan.ProtocolFromEmbedded || !bytes.Equal(plan.Protocol.Content, starter.TeamProtocol()) {
		t.Error("with no captured protocol the plan must carry the embedded one, and say so")
	}
}

func TestComputeInstallPlan_CapturedSnapshotWinsByDefault(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	writeCaptured(t, teamDir, "mine.md", "# mine\n")
	mustWrite(t, filepath.Join(teamDir, "_team-protocol.md"), "# our protocol\n")

	plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.AgentDefsFromEmbedded || plan.ProtocolFromEmbedded {
		t.Errorf("a captured snapshot is the default source, got embedded flags %v/%v",
			plan.AgentDefsFromEmbedded, plan.ProtocolFromEmbedded)
	}
	if got := planNames(plan); len(got) != 1 || got[0] != "mine.md" {
		t.Errorf("want exactly the captured definition, got %v", got)
	}
	if string(plan.Protocol.Content) != "# our protocol\n" {
		t.Errorf("protocol = %q, want the captured one", plan.Protocol.Content)
	}
}

func TestComputeInstallPlan_PreferEmbedded_IgnoresTheSnapshot(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	writeCaptured(t, teamDir, "mine.md", "# mine\n")
	writeCaptured(t, teamDir, "old.md", "---\nname: old\n---\nThis role is RETIRED.\n")
	mustWrite(t, filepath.Join(teamDir, "_team-protocol.md"), "# our protocol\n")
	embedded, err := starter.Agents()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{PreferEmbedded: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.AgentDefsFromEmbedded || !plan.ProtocolFromEmbedded {
		t.Error("--starters must select the embedded definitions and protocol")
	}
	if len(plan.AgentDefs) != len(embedded) {
		t.Errorf("want the %d embedded definitions, got %v", len(embedded), planNames(plan))
	}
	for _, f := range plan.AgentDefs {
		if f.Name == "mine.md" || f.Name == "old.md" {
			t.Errorf("captured definition %s leaked into a --starters plan", f.Name)
		}
	}
	if !bytes.Equal(plan.Protocol.Content, starter.TeamProtocol()) {
		t.Error("--starters must install the embedded protocol, not the captured one")
	}
	if len(plan.DeprecatedCaptured) != 0 {
		t.Errorf("nothing captured is being installed, so nothing captured can be deprecated: %v", plan.DeprecatedCaptured)
	}
}

func TestComputeInstallPlan_CapturedAgentsButNoProtocol_UsesEmbeddedProtocol(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	writeCaptured(t, teamDir, "mine.md", "# mine\n") // no _team-protocol.md beside it

	plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.AgentDefsFromEmbedded {
		t.Error("the agents are captured; only the protocol should fall back")
	}
	if !plan.ProtocolFromEmbedded || !bytes.Equal(plan.Protocol.Content, starter.TeamProtocol()) {
		t.Error("a snapshot with no protocol must fall back to the embedded protocol")
	}
}

func TestComputeInstallPlan_OnlyMarkdownFilesAreDefinitions(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	writeCaptured(t, teamDir, "mine.md", "# mine\n")
	writeCaptured(t, teamDir, "notes.txt", "scratch\n")
	if err := os.MkdirAll(filepath.Join(teamDir, "agents", "archive.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	plan, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{})
	if err != nil {
		t.Fatalf("a directory named like a definition must be skipped, not read: %v", err)
	}
	if got := planNames(plan); len(got) != 1 || got[0] != "mine.md" {
		t.Errorf("want only mine.md, got %v", got)
	}
}

// A missing snapshot dir falls back; one that exists but cannot be read must not. Treating
// every read failure as "no snapshot" would install generic starters over a team's own
// definitions because of a permissions or path problem nobody was told about.
func TestComputeInstallPlan_UnreadableSnapshotDir_IsAnErrorNotAFallback(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	// agents is a regular file, so reading it as a directory fails with something other
	// than not-exist.
	mustWrite(t, filepath.Join(teamDir, "agents"), "not a directory\n")

	_, err := ComputeInstallPlanWithOptions(cfg, filepath.Join(t.TempDir(), "queue"), InstallOptions{})
	if err == nil {
		t.Fatal("a snapshot path that cannot be read as a directory must fail, not silently fall back")
	}
	if !strings.Contains(err.Error(), "team install") {
		t.Errorf("error should say which command failed: %v", err)
	}
}

func TestComputeInstallPlan_DeprecatedCaptured_NamedInStableOrder(t *testing.T) {
	cfg, teamDir := sourceCfg(t)
	for _, n := range []string{"zeta.md", "alpha.md", "mid.md", "beta.md", "omega.md"} {
		writeCaptured(t, teamDir, n, "---\nname: x\n---\nDEPRECATED — use something else.\n")
	}
	writeCaptured(t, teamDir, "gone.md", "---\nname: gone\n---\nThis layer is retired.\n") // "retired" alone, no "deprecated"
	writeCaptured(t, teamDir, "fine.md", "---\nname: fine\n---\nStill in use.\n")
	// A definition that discusses retirement only well past the leading portion is not
	// declaring itself retired.
	writeCaptured(t, teamDir, "late.md", "---\nname: late\n---\n"+strings.Repeat("filler line\n", 300)+"this is retired\n")

	// The plan is built from a map and the directory lists these names already sorted, so a single
	// computation can match the expected order without any sort. Repeat it so that removing the sort
	// fails every run.
	queueDir := filepath.Join(t.TempDir(), "queue")
	want := "alpha.md,beta.md,gone.md,mid.md,omega.md,zeta.md"
	for i := 0; i < 64; i++ {
		plan, err := ComputeInstallPlanWithOptions(cfg, queueDir, InstallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(plan.DeprecatedCaptured, ","); got != want {
			t.Fatalf("run %d: DeprecatedCaptured = %q, want %q (sorted, and only the self-declared ones)", i, got, want)
		}
	}
}

func TestFormatInstallPlan_NothingToDo_DoesNotClaimTheSnapshotWhenEmbedded(t *testing.T) {
	cfg, _ := sourceCfg(t)
	queue := filepath.Join(t.TempDir(), "queue")
	plan, err := ComputeInstallPlanWithOptions(cfg, queue, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyInstallPlan(plan); err != nil {
		t.Fatal(err)
	}
	again, err := ComputeInstallPlanWithOptions(cfg, queue, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := FormatInstallPlan(again)
	if !strings.Contains(out, "nothing to do") {
		t.Fatalf("a second plan over an installed tree should find nothing to do: %q", out)
	}
	if strings.Contains(out, "captured snapshot") {
		t.Errorf("the source here was the binary, not a snapshot: %q", out)
	}
}
