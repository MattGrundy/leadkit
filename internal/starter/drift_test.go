package starter

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeDef(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Editor backups and dotfiles are not definitions; picking them up would report a
// half-finished copy as a real one.
func TestReadAgentDir_IgnoresNonDefinitions(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "real.md", "x")
	writeDef(t, dir, ".hidden.md", "x")
	writeDef(t, dir, "notes.txt", "x")
	if err := os.MkdirAll(filepath.Join(dir, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := readAgentDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["real.md"] == "" {
		t.Errorf("only real.md is a definition, got %v", got)
	}
	// A missing directory is the ordinary state of a fresh adopter, not an error.
	if got, err := readAgentDir(filepath.Join(dir, "absent")); err != nil || len(got) != 0 {
		t.Errorf("missing dir = (%v, %v), want empty and no error", got, err)
	}
}

// Each status must be distinguished correctly, because the only actionable one is pristine
// and the common ones must stay quiet.
func TestCompareToStarters_ClassifiesEachCase(t *testing.T) {
	dir := t.TempDir()
	embedded, err := Agents()
	if err != nil {
		t.Fatal(err)
	}

	// pristine: byte-identical to what shipped
	writeDef(t, dir, "lead.md", string(embedded["lead.md"]))
	// customised, marker removed: the team owns it
	writeDef(t, dir, "critique-agent.md", "---\nname: critique-agent\n---\nours entirely\n")
	// customised but marker left in place
	writeDef(t, dir, "developer.md", "---\nname: developer\n---\n"+AgentMarker+"\nedited but still marked\n")
	// local: no starter counterpart
	writeDef(t, dir, "db-agent.md", "---\nname: db-agent\n---\nlocal only\n")
	// everything else in the embedded set is absent => removed

	findings, err := CompareToStarters(dir)
	if err != nil {
		t.Fatal(err)
	}
	byDef := map[string]DriftFinding{}
	for _, f := range findings {
		byDef[f.Def] = f
	}

	cases := map[string]DriftStatus{
		"lead.md":           DriftPristine,
		"critique-agent.md": DriftCustomised,
		"developer.md":      DriftCustomised,
		"db-agent.md":       DriftLocal,
		"test-runner.md":    DriftRemoved,
	}
	for def, want := range cases {
		if got := byDef[def].Status; got != want {
			t.Errorf("%s: status %q, want %q", def, got, want)
		}
	}

	// The two customised cases share a status and differ only in what they tell the reader
	// to do, so the wording is the only place a swapped branch shows. The still-marked case
	// must say to delete the marker, since leaving it in place is what makes a future upgrade
	// unable to tell whose file it is; the marker-removed case must not say that, because
	// there is nothing left to delete.
	if d := byDef["developer.md"].Detail; !strings.Contains(d, "delete that line") || strings.Contains(d, "marker removed") {
		t.Errorf("a still-marked definition must say to delete the marker, got %q", d)
	}
	if d := byDef["critique-agent.md"].Detail; !strings.Contains(d, "marker removed") || strings.Contains(d, "delete that line") {
		t.Errorf("a definition whose marker is gone must say so and ask for nothing, got %q", d)
	}
	// A removed starter must be described as left alone, never as something to restore.
	if d := byDef["test-runner.md"].Detail; !strings.Contains(d, "decision") {
		t.Errorf("a removed starter must read as a decision, got %q", d)
	}
}

// Editors add and strip trailing newlines. A definition that differs from the starter only
// in surrounding whitespace is still the starter, and calling it customised would hide the
// one finding the report exists to make.
func TestCompareToStarters_PristineIgnoresSurroundingWhitespace(t *testing.T) {
	dir := t.TempDir()
	embedded, err := Agents()
	if err != nil {
		t.Fatal(err)
	}
	writeDef(t, dir, "lead.md", "\n\n"+string(embedded["lead.md"])+"\n\n  \n")

	findings, err := CompareToStarters(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Def == "lead.md" && f.Status != DriftPristine {
			t.Errorf("whitespace-only difference read as %q, want pristine", f.Status)
		}
	}
}

// Findings are consumed in order by a printed report, so the order must not depend on map
// iteration.
func TestCompareToStarters_FindingsAreSortedByDefinition(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"zz-agent.md", "aa-agent.md", "mm-agent.md"} {
		writeDef(t, dir, n, "---\nname: x\n---\nlocal\n")
	}
	findings, err := CompareToStarters(dir)
	if err != nil {
		t.Fatal(err)
	}
	var defs []string
	for _, f := range findings {
		defs = append(defs, f.Def)
	}
	if !sort.StringsAreSorted(defs) {
		t.Errorf("findings must be sorted by definition name, got %v", defs)
	}
	// Locals interleave with starters, so a sort applied to only one group would fail here.
	if len(defs) < 4 {
		t.Fatalf("expected starters plus the locals, got %v", defs)
	}
}

// A machine with no definitions installed must produce a report, not an error: every
// starter reads as not-installed, which is accurate.
func TestCompareToStarters_MissingDirIsReportedNotFatal(t *testing.T) {
	findings, err := CompareToStarters(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("missing dir must not be an error: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected every starter reported as not installed")
	}
	for _, f := range findings {
		if f.Status != DriftRemoved {
			t.Errorf("%s: status %q, want removed", f.Def, f.Status)
		}
	}
}
