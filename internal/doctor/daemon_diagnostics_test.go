package doctor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/queue"
)

var ddClock = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

// ddProfile builds a profile rooted at a tmp dir with the sweep artifact path set and
// the diagnostics defaults (surface_doctor on, self_repair command).
func ddProfile(t *testing.T) (*config.Profile, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "sweep-status.json")
	prof := &config.Profile{
		SweepStatusFile: path, // absolute — Resolve returns it unchanged
		Diagnostics:     config.DefaultDiagnostics(),
	}
	return prof, path
}

func writeArtifact(t *testing.T, path string, res queue.SweepResult, ts time.Time) {
	t.Helper()
	if err := queue.WriteSweepStatus(path, queue.BuildSweepStatus(res, "", ts)); err != nil {
		t.Fatal(err)
	}
}

func findingsResult() queue.SweepResult {
	return queue.SweepResult{
		Stalled:      1,
		Broken:       1,
		StalledLanes: []queue.StalledLane{{Lane: "cr-1", Pending: 2}},
		BrokenLanes:  []queue.BrokenLane{{Lane: "gate-1", Links: []string{"x.md"}}},
	}
}

func TestDaemonDiagnostics_FreshFindingsWarnWithRemediation(t *testing.T) {
	prof, path := ddProfile(t)
	writeArtifact(t, path, findingsResult(), ddClock)

	c := daemonDiagnosticsCheck(prof, ddClock)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("fresh findings must Warn, got OK=%v sev=%s", c.OK, c.Severity)
	}
	if !strings.Contains(c.Detail, "leadkit queue repair") {
		t.Errorf("Warn must name the repair remediation: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "cr-1") {
		t.Errorf("Warn must name the stalled lane: %q", c.Detail)
	}
}

func TestDaemonDiagnostics_AbsentIsInfoNotWarn(t *testing.T) {
	prof, _ := ddProfile(t)
	c := daemonDiagnosticsCheck(prof, ddClock)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("an absent artifact must Info-degrade, got OK=%v sev=%s (%s)", c.OK, c.Severity, c.Detail)
	}
	if !strings.Contains(c.Detail, "may not have run") {
		t.Errorf("absent detail should point at the daemon not running: %q", c.Detail)
	}
}

func TestDaemonDiagnostics_StaleIsInfoNotWarn(t *testing.T) {
	prof, path := ddProfile(t)
	writeArtifact(t, path, findingsResult(), ddClock) // fresh at ddClock...
	// ...but doctor runs 2h later, past the 1h stale window.
	c := daemonDiagnosticsCheck(prof, ddClock.Add(2*time.Hour))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("stale findings must Info-degrade (never a false Warn), got OK=%v sev=%s", c.OK, c.Severity)
	}
	if !strings.Contains(c.Detail, "stale") {
		t.Errorf("stale detail should say so: %q", c.Detail)
	}
}

func TestDaemonDiagnostics_CleanSweepIsInfo(t *testing.T) {
	prof, path := ddProfile(t)
	writeArtifact(t, path, queue.SweepResult{}, ddClock)
	c := daemonDiagnosticsCheck(prof, ddClock)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("a clean sweep must Info, got OK=%v sev=%s", c.OK, c.Severity)
	}
	if !strings.Contains(c.Detail, "healthy") {
		t.Errorf("clean detail should read healthy: %q", c.Detail)
	}
}

func TestDaemonDiagnostics_SurfaceDoctorOffIsDisabled(t *testing.T) {
	prof, path := ddProfile(t)
	writeArtifact(t, path, findingsResult(), ddClock) // findings present...
	prof.Diagnostics.SurfaceDoctor = false            // ...but the toggle is off

	c := daemonDiagnosticsCheck(prof, ddClock)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("surface_doctor=false must not Warn, got OK=%v sev=%s", c.OK, c.Severity)
	}
	if !strings.Contains(c.Detail, "surface_doctor=false") {
		t.Errorf("disabled detail should name the toggle: %q", c.Detail)
	}
}
