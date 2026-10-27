package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnostics_Defaults(t *testing.T) {
	d := Defaults()
	if d.SweepStatusFile != DefaultSweepStatusFile {
		t.Errorf("SweepStatusFile = %q, want %q", d.SweepStatusFile, DefaultSweepStatusFile)
	}
	dg := d.Diagnostics
	if !dg.SurfaceDoctor || !dg.SurfaceBoard {
		t.Errorf("surface_doctor/surface_board should default ON, got %+v", dg)
	}
	if dg.SurfaceNotify {
		t.Error("surface_notify should default OFF")
	}
	if dg.NotifyCommand != "" {
		t.Errorf("notify_command should default empty, got %q", dg.NotifyCommand)
	}
	if dg.NotifyThreshold != DefaultNotifyThreshold {
		t.Errorf("notify_threshold = %d, want %d", dg.NotifyThreshold, DefaultNotifyThreshold)
	}
	if dg.SelfRepair != SelfRepairCommand {
		t.Errorf("self_repair should default %q, got %q", SelfRepairCommand, dg.SelfRepair)
	}
}

// TestDiagnostics_FileFlipsEachToggle proves every new key is config-driven: a
// non-default value in the file overrides its default, and unset sibling keys keep
// their defaults (the yaml-over-Defaults merge).
func TestDiagnostics_FileFlipsEachToggle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultProfileName)
	yaml := "" +
		"sweep_status_file: .leadkit/custom-sweep.json\n" +
		"diagnostics:\n" +
		"  surface_doctor: false\n" +
		"  surface_notify: true\n" +
		"  notify_command: \"notify-send {msg}\"\n" +
		"  notify_threshold: 5\n" +
		"  self_repair: auto\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)

	prof, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.SweepStatusFile != ".leadkit/custom-sweep.json" {
		t.Errorf("SweepStatusFile = %q, want the override", prof.SweepStatusFile)
	}
	dg := prof.Diagnostics
	if dg.SurfaceDoctor {
		t.Error("surface_doctor: false in the file must win over the true default")
	}
	if !dg.SurfaceNotify || dg.NotifyCommand != "notify-send {msg}" || dg.NotifyThreshold != 5 {
		t.Errorf("notify keys not applied from file: %+v", dg)
	}
	if dg.SelfRepair != SelfRepairAuto {
		t.Errorf("self_repair = %q, want auto", dg.SelfRepair)
	}
	// surface_board was NOT set in the file — its true default must survive the merge.
	if !dg.SurfaceBoard {
		t.Error("surface_board default (true) must survive an unset field in a partial diagnostics block")
	}
}
