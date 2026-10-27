package queue

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedClock is the fake clock the artifact tests use — no wall-clock dependency.
var fixedClock = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func sampleResult() SweepResult {
	return SweepResult{
		Stalled: 2,
		Broken:  1,
		StalledLanes: []StalledLane{
			{Lane: "cr-1", Pending: 3, Entries: []string{"a.md", "b.md", "c.md"}},
			{Lane: "finalise", Pending: 1, Entries: []string{"d.md"}},
		},
		BrokenLanes: []BrokenLane{
			{Lane: "gate-1", Links: []string{"x.md"}},
		},
	}
}

// TestFrozenClaimSurface asserts the findings artifact carries frozen claims through a
// round-trip and that FormatStatusSurface renders the FROZEN-CLAIM line + the
// probe→TaskStop→reassign remediation — the self-surfacing status/board readers' view.
func TestFrozenClaimSurface(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".leadkit", "sweep-status.json")

	s := BuildSweepStatus(SweepResult{}, "", fixedClock) // clean stalls/broken
	s.FrozenClaims = []FrozenClaim{{Lane: "devops", Ticket: "t42", Age: "2h0m0s"}}
	if err := WriteSweepStatus(path, s); err != nil {
		t.Fatalf("WriteSweepStatus: %v", err)
	}
	got, present, err := ReadSweepStatus(path)
	if err != nil || !present {
		t.Fatalf("ReadSweepStatus: present=%v err=%v", present, err)
	}
	if len(got.FrozenClaims) != 1 || got.FrozenClaims[0].Ticket != "t42" {
		t.Fatalf("round-tripped frozen claims = %+v, want one for devops/t42", got.FrozenClaims)
	}

	// Frozen claims do NOT flip Clean (stalls/broken are the daemon-diagnostics surface).
	if !got.Clean() {
		t.Error("frozen claims must not make Clean() false (dedicated surface)")
	}

	surface := FormatStatusSurface(path, got, present, "report", time.Hour, fixedClock)
	for _, want := range []string{"FROZEN-CLAIM devops t42", "claimed 2h0m0s, no advancement", "PROBE the owner", "TaskStop", "queue move <queue_root>/<lane>/in-progress/"} {
		if !strings.Contains(surface, want) {
			t.Errorf("status surface missing %q\n---\n%s", want, surface)
		}
	}
}

func TestBuildWriteReadSweepStatus_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".leadkit", "sweep-status.json")

	s := BuildSweepStatus(sampleResult(), "milestone m1: 2/3 DoD met", fixedClock)
	if s.TS != "2026-09-13T12:00:00Z" {
		t.Errorf("TS = %q, want the fixed clock's RFC3339", s.TS)
	}
	if err := WriteSweepStatus(path, s); err != nil {
		t.Fatalf("WriteSweepStatus: %v", err)
	}

	got, present, err := ReadSweepStatus(path)
	if err != nil {
		t.Fatalf("ReadSweepStatus: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true for a written artifact")
	}
	if got.Stalled != 2 || got.Broken != 1 {
		t.Errorf("counts = %d stalled / %d broken, want 2/1", got.Stalled, got.Broken)
	}
	if len(got.StalledLanes) != 2 || got.StalledLanes[0].Lane != "cr-1" {
		t.Errorf("stalled lanes not round-tripped: %+v", got.StalledLanes)
	}
	if len(got.BrokenLanes) != 1 || got.BrokenLanes[0].Links[0] != "x.md" {
		t.Errorf("broken lanes not round-tripped: %+v", got.BrokenLanes)
	}
	if got.Milestone != "milestone m1: 2/3 DoD met" {
		t.Errorf("milestone = %q, want the re-anchor line", got.Milestone)
	}
}

func TestReadSweepStatus_AbsentIsNotAnError(t *testing.T) {
	got, present, err := ReadSweepStatus(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("absent artifact must not error: %v", err)
	}
	if present {
		t.Error("present = true for an absent artifact")
	}
	if !got.Clean() {
		t.Error("a zero SweepStatus must read as Clean")
	}
}

func TestReadSweepStatus_EmptyPathIsNoOp(t *testing.T) {
	_, present, err := ReadSweepStatus("")
	if err != nil || present {
		t.Errorf(`ReadSweepStatus("") = present %v, err %v; want false, nil`, present, err)
	}
	if err := WriteSweepStatus("", BuildSweepStatus(sampleResult(), "", fixedClock)); err != nil {
		t.Errorf(`WriteSweepStatus("") must be a no-op, got %v`, err)
	}
}

func TestSweepStatus_Age(t *testing.T) {
	s := BuildSweepStatus(SweepResult{}, "", fixedClock)
	age, ok := s.Age(fixedClock.Add(90 * time.Minute))
	if !ok {
		t.Fatal("Age ok = false for a parseable TS")
	}
	if age != 90*time.Minute {
		t.Errorf("age = %s, want 90m", age)
	}
	if _, ok := (SweepStatus{TS: "not-a-time"}).Age(fixedClock); ok {
		t.Error("Age ok = true for an unparseable TS")
	}
}

func TestRemediation_NamesRepairPerSelfRepairMode(t *testing.T) {
	s := BuildSweepStatus(sampleResult(), "", fixedClock)

	command := s.Remediation("command")
	if !strings.Contains(command, "leadkit queue repair") {
		t.Errorf("command mode must name `leadkit queue repair`: %q", command)
	}
	if !strings.Contains(command, "cr-1") || !strings.Contains(command, "finalise") {
		t.Errorf("remediation must name the stalled lanes: %q", command)
	}

	auto := s.Remediation("auto")
	if !strings.Contains(auto, "automatically") {
		t.Errorf("auto mode must say it reaps automatically: %q", auto)
	}

	report := s.Remediation("report")
	if !strings.Contains(report, "surfacing only") {
		t.Errorf("report mode must say surfacing only: %q", report)
	}

	if clean := (BuildSweepStatus(SweepResult{}, "", fixedClock)).Remediation("command"); clean != "" {
		t.Errorf("a clean sweep has no remediation, got %q", clean)
	}
}

func TestFormatStatusSurface_AbsentStaleAndFindings(t *testing.T) {
	// Absent → the degrade line, never a false alarm.
	absent := FormatStatusSurface("/x/sweep.json", SweepStatus{}, false, "command", time.Hour, fixedClock)
	if !strings.Contains(absent, "may not have run") {
		t.Errorf("absent surface must degrade cleanly: %q", absent)
	}

	// Fresh with findings → the findings + remediation.
	s := BuildSweepStatus(sampleResult(), "", fixedClock)
	fresh := FormatStatusSurface("/x/sweep.json", s, true, "command", time.Hour, fixedClock)
	if !strings.Contains(fresh, "STALLED cr-1") || !strings.Contains(fresh, "BROKEN  gate-1") {
		t.Errorf("fresh surface must list findings: %q", fresh)
	}
	if !strings.Contains(fresh, "remediation:") || !strings.Contains(fresh, "leadkit queue repair") {
		t.Errorf("fresh surface must carry the remediation: %q", fresh)
	}

	// Stale → the stale note.
	stale := FormatStatusSurface("/x/sweep.json", s, true, "command", time.Hour, fixedClock.Add(2*time.Hour))
	if !strings.Contains(stale, "STALE") {
		t.Errorf("a 2h-old artifact past a 1h window must be flagged stale: %q", stale)
	}
}
