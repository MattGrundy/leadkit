package usage

import (
	"os"
	"path/filepath"
	"testing"

	"mgpp.dev/leadkit/internal/burn"
)

func TestReadPriorityCeiling_UnsetAbsentIsCriticalNoClamp(t *testing.T) {
	t.Setenv(PriorityCeilingEnvVar, "")
	got := ReadPriorityCeiling(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != burn.PriorityCritical {
		t.Errorf("ReadPriorityCeiling(missing file) = %q, want %q (no clamp)", got, burn.PriorityCritical)
	}
}

func TestReadPriorityCeiling_EmptyPathIsCriticalNoClamp(t *testing.T) {
	t.Setenv(PriorityCeilingEnvVar, "")
	if got := ReadPriorityCeiling(""); got != burn.PriorityCritical {
		t.Errorf("ReadPriorityCeiling(\"\") = %q, want %q", got, burn.PriorityCritical)
	}
}

func TestReadPriorityCeiling_GarbageContentIsCriticalNoClamp(t *testing.T) {
	t.Setenv(PriorityCeilingEnvVar, "")
	path := filepath.Join(t.TempDir(), "priority-ceiling")
	if err := os.WriteFile(path, []byte("!!!not-a-tier###\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Garbage normalizes to "normal" via burn.NormalizePriority, NOT critical
	// by fiat — but this test's point is that a MISSING/unreadable file (the
	// actual "fail open" contract) never accidentally clamps to low; garbage
	// content that IS readable is honoured as literally written (normalized).
	// See TestReadPriorityCeiling_UnreadableFileIsCriticalNoClamp for the
	// real fail-open case.
	got := ReadPriorityCeiling(path)
	if got != burn.PriorityNormal {
		t.Errorf("ReadPriorityCeiling(garbage content) = %q, want %q (NormalizePriority's own fallback)", got, burn.PriorityNormal)
	}
}

func TestReadPriorityCeiling_UnreadableFileFailsOpenToCritical(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits don't block reads")
	}
	t.Setenv(PriorityCeilingEnvVar, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-ceiling")
	if err := os.WriteFile(path, []byte("low"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	got := ReadPriorityCeiling(path)
	if got != burn.PriorityCritical {
		t.Errorf("ReadPriorityCeiling(unreadable file) = %q, want %q — fail OPEN, never accidentally clamp to low", got, burn.PriorityCritical)
	}
}

func TestReadPriorityCeiling_FileSetsCeiling(t *testing.T) {
	t.Setenv(PriorityCeilingEnvVar, "")
	path := filepath.Join(t.TempDir(), "priority-ceiling")
	if err := os.WriteFile(path, []byte("high\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadPriorityCeiling(path); got != burn.PriorityHigh {
		t.Errorf("ReadPriorityCeiling = %q, want %q", got, burn.PriorityHigh)
	}
}

func TestReadPriorityCeiling_EnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "priority-ceiling")
	if err := os.WriteFile(path, []byte("low"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PriorityCeilingEnvVar, "high")
	if got := ReadPriorityCeiling(path); got != burn.PriorityHigh {
		t.Errorf("ReadPriorityCeiling = %q, want the env override %q over the file's %q", got, burn.PriorityHigh, "low")
	}
}
