package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hookwire"
)

// fakeProber records what it was asked and answers as told.
type fakeProber struct {
	resolveErr map[string]error
	probeErr   map[string]error
	resolved   []string
	probed     []string
}

func (f *fakeProber) Resolve(token string) (string, error) {
	f.resolved = append(f.resolved, token)
	if err, ok := f.resolveErr[token]; ok {
		return "", err
	}
	return "/resolved/" + filepath.Base(token), nil
}

func (f *fakeProber) Probe(bin string) error {
	f.probed = append(f.probed, bin)
	return f.probeErr[bin]
}

func wired(name, command string) hookwire.WireStatus {
	return hookwire.WireStatus{Name: name, Wired: true, Found: true, Command: command}
}

// The failure this exists for: a hook wired to a bare name that the hook runtime's PATH
// cannot resolve. It must be CRITICAL and it must say the guard is inert, because the
// operator's mental model is "it's in settings.json, so it's on".
func TestHookProbe_UnresolvableBinaryIsCritical(t *testing.T) {
	f := &fakeProber{resolveErr: map[string]error{"leadkit": errors.New("bare name does not resolve")}}
	checks := hookProbeChecks([]hookwire.WireStatus{wired("push-guard", "leadkit hook push-guard")}, f)

	if len(checks) != 1 {
		t.Fatalf("want one verdict, got %d", len(checks))
	}
	c := checks[0]
	if c.OK {
		t.Error("an unresolvable hook binary must not report OK")
	}
	if c.Severity != SeverityCritical {
		t.Errorf("severity = %q, want critical: a silently inert guard is the worst case", c.Severity)
	}
	if !strings.Contains(c.Detail, "unguarded") && !strings.Contains(c.Detail, "inert") {
		t.Errorf("detail must say the guard does not enforce, got %q", c.Detail)
	}
}

// A resolvable binary that cannot be executed is the same class of problem.
func TestHookProbe_UnrunnableBinaryIsCritical(t *testing.T) {
	f := &fakeProber{probeErr: map[string]error{"/resolved/leadkit-bin": errors.New("did not respond")}}
	checks := hookProbeChecks([]hookwire.WireStatus{
		wired("push-gate", "/opt/leadkit-bin hook push-gate"),
	}, f)
	if len(checks) != 1 || checks[0].OK || checks[0].Severity != SeverityCritical {
		t.Fatalf("want one critical verdict, got %+v", checks)
	}
}

// Several hooks normally share one binary. Probing it once per hook would exec the binary
// once per hook for no extra signal.
func TestHookProbe_ProbesEachBinaryOnce(t *testing.T) {
	f := &fakeProber{}
	checks := hookProbeChecks([]hookwire.WireStatus{
		wired("push-guard", "/opt/lk hook push-guard"),
		wired("push-gate", "/opt/lk hook push-gate"),
		wired("owner-guard", "/opt/lk hook owner-guard"),
	}, f)
	if len(f.probed) != 1 {
		t.Errorf("probed %d times, want 1 for a shared binary: %v", len(f.probed), f.probed)
	}
	if len(checks) != 1 {
		t.Errorf("want one verdict for one binary, got %d", len(checks))
	}
}

// An unwired hook is already reported by the wiring check. Probing it would restate that
// and, worse, would report a second failure for one problem.
func TestHookProbe_SkipsUnwiredHooks(t *testing.T) {
	f := &fakeProber{}
	checks := hookProbeChecks([]hookwire.WireStatus{
		{Name: "push-guard", Wired: false, Found: false},
		{Name: "pr-guard", Wired: false, Found: true, Command: "/opt/lk hook pr-guard"}, // misconfigured matcher
	}, f)
	if len(checks) != 0 {
		t.Errorf("unwired hooks must not be probed, got %+v", checks)
	}
	if len(f.resolved) != 0 {
		t.Errorf("must not resolve anything for unwired hooks, got %v", f.resolved)
	}
}

// The real prober's contract, exercised against the filesystem rather than a fake: an
// absolute path must exist AND carry an exec bit. A present-but-non-executable file is
// the case a plain existence check would wave through.
func TestRealHookProber_Resolve_AbsolutePathRules(t *testing.T) {
	dir := t.TempDir()
	noExec := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(noExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yesExec := filepath.Join(dir, "is-exec")
	if err := os.WriteFile(yesExec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := realHookProber{}
	if _, err := p.Resolve(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing absolute path must be an error")
	}
	if _, err := p.Resolve(dir); err == nil {
		t.Error("a directory must be an error")
	}
	if _, err := p.Resolve(noExec); err == nil {
		t.Error("a non-executable file must be an error, not a pass")
	}
	if got, err := p.Resolve(yesExec); err != nil || got != yesExec {
		t.Errorf("an executable file must resolve to itself, got %q err=%v", got, err)
	}
}

// A bare name must be resolved against the HOOK RUNTIME's PATH, never the caller's.
// Resolving against the caller's PATH succeeds in the shell and fails in the hook.
func TestRealHookProber_Resolve_BareNameIgnoresCallerPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "leadkit-probe-fixture")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Put the fixture on the CALLER's PATH only. The hook runtime PATH does not include it.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var rp realHookProber
	if _, err := rp.Resolve("leadkit-probe-fixture"); err == nil {
		t.Error("a bare name on the caller's PATH but not the hook runtime's must fail: " +
			"that difference is the whole bug")
	}

	// Sanity: the same name IS findable via the caller's PATH, so the assertion above is
	// about the PATH used and not about a missing file.
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("fixture vanished: %v", err)
	}
}
