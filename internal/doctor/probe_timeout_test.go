package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript makes the executable fixtures that drive the real realHookProber.Probe.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// proberAllowing is a realHookProber whose install set is exactly paths — the bound on what
// Probe may execute — so fixtures in a TempDir count as "known leadkit installs".
func proberAllowing(paths ...string) realHookProber {
	return realHookProber{install: &BinarySkewInput{
		ExtraCandidates: paths,
		EvalSymlinks:    filepath.EvalSymlinks,
		Stat:            os.Stat,
	}}
}

// The case the code comment claims to catch: "a wired binary that hangs is as useless as one
// that is missing". It must be a FINDING, not a pass.
func TestRealHookProber_Probe_HangIsAFinding(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "hang", "#!/bin/sh\nsleep 30\n")

	start := time.Now()
	err := proberAllowing(bin).Probe(bin)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a binary that hangs reported healthy after %s — the timeout is being swallowed", elapsed)
	}
	if !strings.Contains(err.Error(), "did not respond") {
		t.Errorf("want a timeout finding, got %q", err)
	}
	if elapsed > hookProbeTimeout*2 {
		t.Errorf("took %s, timeout is %s", elapsed, hookProbeTimeout)
	}
}

// A non-zero exit still proves the binary started, which is what the probe tests.
func TestRealHookProber_Probe_NonZeroExitIsNotAFinding(t *testing.T) {
	dir := t.TempDir()
	bin := writeScript(t, dir, "fails", "#!/bin/sh\nexit 3\n")
	if err := proberAllowing(bin).Probe(bin); err != nil {
		t.Errorf("a non-zero exit should pass (the binary started): %v", err)
	}
}

func TestRealHookProber_Probe_StartFailureIsAFinding(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := proberAllowing(notExec).Probe(notExec)
	if err == nil || errors.Is(err, errNotProbed) || !strings.Contains(err.Error(), "could not be executed") {
		t.Errorf("a non-executable known install should be a start-failure finding, got %v", err)
	}
}
