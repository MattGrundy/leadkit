package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hookwire"
)

// markerScript is a "hook binary" that records, by creating marker, that it was executed.
// Whether a binary ran is the property under test, so it must be observable independently of
// what Probe returns.
func markerScript(t *testing.T, dir, name string) (bin, marker string) {
	t.Helper()
	marker = filepath.Join(dir, name+".ran")
	bin = writeScript(t, dir, name, "#!/bin/sh\ntouch '"+marker+"'\n")
	return bin, marker
}

func ran(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	return err == nil
}

// The reason the bound exists: settings.json can be a cloned repo's own file, and doctor must
// not run a binary that file chose.
func TestRealHookProber_Probe_NeverExecutesAnUnknownBinary(t *testing.T) {
	dir := t.TempDir()
	bin, marker := markerScript(t, dir, "repo-chosen")

	err := realHookProber{install: &BinarySkewInput{
		EvalSymlinks: filepath.EvalSymlinks, Stat: os.Stat,
	}}.Probe(bin)

	if !errors.Is(err, errNotProbed) {
		t.Errorf("want errNotProbed for a binary outside the install set, got %v", err)
	}
	if ran(t, marker) {
		t.Fatal("doctor EXECUTED a binary that is not a known leadkit install")
	}
}

func TestRealHookProber_Probe_ExecutesAKnownInstall(t *testing.T) {
	dir := t.TempDir()
	bin, marker := markerScript(t, dir, "installed")
	if err := proberAllowing(bin).Probe(bin); err != nil {
		t.Fatal(err)
	}
	if !ran(t, marker) {
		t.Error("a known install was not executed, so the probe proves nothing")
	}
}

// Self is the fallback when the candidate enumeration finds nothing: the running binary is
// always safe to run, and nothing else becomes so.
func TestRealHookProber_Probe_FallsBackToSelfOnly(t *testing.T) {
	dir := t.TempDir()
	self, selfMarker := markerScript(t, dir, "self")
	other, otherMarker := markerScript(t, dir, "other")
	p := realHookProber{install: &BinarySkewInput{Self: self, EvalSymlinks: filepath.EvalSymlinks, Stat: os.Stat}}

	if err := p.Probe(self); err != nil || !ran(t, selfMarker) {
		t.Errorf("self must be probed: err=%v ran=%v", err, ran(t, selfMarker))
	}
	if err := p.Probe(other); !errors.Is(err, errNotProbed) || ran(t, otherMarker) {
		t.Errorf("anything but self must not be probed: err=%v ran=%v", err, ran(t, otherMarker))
	}
}

// Identity is the REAL file, so a symlink does not launder an untrusted binary, and a symlink
// to a trusted one is the same trusted binary.
func TestRealHookProber_Probe_BoundIsByRealPath(t *testing.T) {
	dir := t.TempDir()
	trusted, trustedMarker := markerScript(t, dir, "trusted")
	evil, evilMarker := markerScript(t, dir, "evil")
	repo := t.TempDir()
	toTrusted := filepath.Join(repo, "to-trusted")
	toEvil := filepath.Join(repo, "to-evil")
	if err := os.Symlink(trusted, toTrusted); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, toEvil); err != nil {
		t.Fatal(err)
	}
	p := proberAllowing(trusted)

	if err := p.Probe(toEvil); !errors.Is(err, errNotProbed) || ran(t, evilMarker) {
		t.Errorf("a symlink to an unknown binary must not be executed: err=%v ran=%v", err, ran(t, evilMarker))
	}
	if err := p.Probe(toTrusted); err != nil || !ran(t, trustedMarker) {
		t.Errorf("a symlink to a known install is that install: err=%v ran=%v", err, ran(t, trustedMarker))
	}
}

// A relative PATH entry joins to a path under the CURRENT directory — in a cloned repo, the
// repo's. Even when that candidate is a symlink resolving to an absolute path, it must not
// widen the set.
func TestRealHookProber_Probe_RelativePathEntryIsNotAnInstall(t *testing.T) {
	dir := t.TempDir()
	evil, evilMarker := markerScript(t, dir, "evil")
	cwd := t.TempDir()
	if err := os.Symlink(evil, filepath.Join(cwd, "leadkit")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	p := realHookProber{install: &BinarySkewInput{
		PathEnv: ".", EvalSymlinks: filepath.EvalSymlinks, Stat: os.Stat,
	}}
	if err := p.Probe(evil); !errors.Is(err, errNotProbed) || ran(t, evilMarker) {
		t.Errorf("a cwd-relative PATH entry must not make a binary an install: err=%v ran=%v", err, ran(t, evilMarker))
	}
}

// A candidate found by walking PATH is as good as an extra one: this is the shared
// enumeration, not a second list.
func TestRealHookProber_Probe_PathCandidatesCount(t *testing.T) {
	dir := t.TempDir()
	bin, marker := markerScript(t, dir, "leadkit")
	p := realHookProber{install: &BinarySkewInput{PathEnv: dir, EvalSymlinks: filepath.EvalSymlinks, Stat: os.Stat}}
	if err := p.Probe(bin); err != nil || !ran(t, marker) {
		t.Errorf("a leadkit on PATH is a known install: err=%v ran=%v", err, ran(t, marker))
	}
}

// Execution must be runtime-faithful, not just resolution: the child gets the hook runtime's
// PATH, not the caller's.
func TestRealHookProber_Probe_RunsUnderTheHookRuntimePath(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "path.out")
	bin := writeScript(t, dir, "leadkit", "#!/bin/sh\nprintf '%s' \"$PATH\" > '"+out+"'\n")
	t.Setenv("PATH", "/caller/only/bin")

	if err := proberAllowing(bin).Probe(bin); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != HookRuntimePath {
		t.Errorf("child PATH = %q, want the hook runtime's %q", got, HookRuntimePath)
	}
}

func TestRealHookProber_Probe_RelativeBinaryIsNotProbed(t *testing.T) {
	if err := proberAllowing("leadkit").Probe("leadkit"); !errors.Is(err, errNotProbed) {
		t.Errorf("want errNotProbed, got %v", err)
	}
}

func TestRealHookProber_Resolve_RelativePathIsUnverifiableNotBroken(t *testing.T) {
	_, err := realHookProber{}.Resolve("./bin/leadkit")
	if !errors.Is(err, errUnverifiable) {
		t.Errorf("a relative path resolves against a directory doctor cannot know; want errUnverifiable, got %v", err)
	}
}

// Resolution must not touch the process environment.
func TestRealHookProber_Resolve_LeavesPathAlone(t *testing.T) {
	t.Setenv("PATH", "/sentinel")
	_, _ = realHookProber{}.Resolve("sh")
	if got := os.Getenv("PATH"); got != "/sentinel" {
		t.Errorf("PATH = %q after Resolve", got)
	}
	if p, err := (realHookProber{}).Resolve("sh"); err != nil || !strings.HasPrefix(p, "/bin/") && !strings.HasPrefix(p, "/usr/bin/") {
		t.Errorf("sh should resolve on the hook runtime PATH, got %q err=%v", p, err)
	}
}

// The command shapes that work in production. Each must reduce to its binary word, not be
// read as a literal path.
func TestHookCommandWord_ProductionShapes(t *testing.T) {
	const home = "/home/u"
	for _, tc := range []struct{ prefix, want string }{
		{`/opt/leadkit`, `/opt/leadkit`},
		{`leadkit`, `leadkit`},
		{`"/opt/my dir/leadkit"`, `/opt/my dir/leadkit`},
		{`'/opt/my dir/leadkit'`, `/opt/my dir/leadkit`},
		{`$HOME/leadkit`, `/home/u/leadkit`},
		{`${HOME}/bin/leadkit`, `/home/u/bin/leadkit`},
		{`"$HOME/leadkit"`, `/home/u/leadkit`},
		{`~/leadkit`, `/home/u/leadkit`},
		{`exec /opt/leadkit`, `/opt/leadkit`},
		{`LEADKIT_DEBUG=1 /opt/leadkit`, `/opt/leadkit`},
		{`A=1 B="x y" exec /opt/leadkit`, `/opt/leadkit`},
		{`LEADKIT_DEBUG=1 "$HOME/leadkit"`, `/home/u/leadkit`},
		{`/opt/a\ b/leadkit`, `/opt/a b/leadkit`},
	} {
		got, err := hookCommandWord(tc.prefix, home)
		if err != nil || got != tc.want {
			t.Errorf("hookCommandWord(%q) = %q, %v; want %q", tc.prefix, got, err, tc.want)
		}
	}
}

// Anything not understood is "cannot tell", never a guess.
func TestHookCommandWord_UnderstoodNothingElse(t *testing.T) {
	for _, prefix := range []string{
		`$(which leadkit)`, "`which leadkit`", `$OTHER/leadkit`, `~bob/leadkit`,
		`cd /x && /opt/leadkit`, `/opt/leadkit |`, `sudo /opt/leadkit`, `/opt/*/leadkit`,
		`"/opt/leadkit`, `'/opt/leadkit`, `FOO=1`, `exec`, `"$HOMEDIR/leadkit"`, `leadkit --flag`,
	} {
		if got, err := hookCommandWord(prefix, "/home/u"); !errors.Is(err, errUnverifiable) {
			t.Errorf("hookCommandWord(%q) = %q, %v; want errUnverifiable", prefix, got, err)
		}
	}
}

// Through the checks: a healthy install wired in each production shape produces no CRITICAL
// and hands the prober the EXPANDED path.
func TestHookProbe_ProductionShapesAreNotCritical(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct{ command, wantToken string }{
		{`"/opt/leadkit" hook push-guard`, "/opt/leadkit"},
		{`$HOME/leadkit hook push-guard`, "/home/u/leadkit"},
		{`~/leadkit hook push-guard`, "/home/u/leadkit"},
		{`exec /opt/leadkit hook push-guard`, "/opt/leadkit"},
		{`LEADKIT_DEBUG=1 /opt/leadkit hook push-guard`, "/opt/leadkit"},
	} {
		f := &fakeProber{}
		checks := hookProbeChecks([]hookwire.WireStatus{wired("push-guard", tc.command)}, f)
		if len(f.resolved) != 1 || f.resolved[0] != tc.wantToken {
			t.Errorf("%q: resolved %v, want [%s]", tc.command, f.resolved, tc.wantToken)
		}
		for _, c := range checks {
			if c.Severity == SeverityCritical {
				t.Errorf("%q reported CRITICAL on a healthy install: %s", tc.command, c.Detail)
			}
		}
	}
}

func TestHookProbe_UninterpretableCommandWarnsRatherThanFails(t *testing.T) {
	f := &fakeProber{}
	checks := hookProbeChecks([]hookwire.WireStatus{wired("push-guard", `$(which leadkit) hook push-guard`)}, f)
	if len(checks) != 1 || checks[0].Severity != SeverityWarn || checks[0].OK {
		t.Fatalf("want one warn, got %+v", checks)
	}
	if len(f.resolved) != 0 || len(f.probed) != 0 {
		t.Errorf("nothing should be resolved or run for a command doctor cannot parse: %v %v", f.resolved, f.probed)
	}
}

// A binary the prober declines to run is not a failure: it resolved, and doctor chose not to
// execute it.
func TestHookProbe_NotProbedIsInfoNotCritical(t *testing.T) {
	f := &fakeProber{probeErr: map[string]error{"/resolved/lk": errNotProbed}}
	checks := hookProbeChecks([]hookwire.WireStatus{wired("push-guard", "/opt/lk hook push-guard")}, f)
	if len(checks) != 1 || !checks[0].OK || checks[0].Severity != SeverityInfo {
		t.Fatalf("want one info/OK verdict, got %+v", checks)
	}
	if !strings.Contains(checks[0].Detail, "not executed") {
		t.Errorf("must say it was not executed, got %q", checks[0].Detail)
	}
}

func TestHookProbe_UnverifiableResolveWarns(t *testing.T) {
	f := &fakeProber{resolveErr: map[string]error{"./lk": errUnverifiable}}
	checks := hookProbeChecks([]hookwire.WireStatus{wired("push-guard", "./lk hook push-guard")}, f)
	if len(checks) != 1 || checks[0].Severity != SeverityWarn {
		t.Fatalf("want one warn, got %+v", checks)
	}
}

// One verdict per binary, but it must name every hook on it, so no hook sharing the failed
// binary is left without a line.
func TestHookProbe_SharedBinaryFailureNamesEveryHook(t *testing.T) {
	f := &fakeProber{probeErr: map[string]error{"/resolved/lk": errors.New("did not respond")}}
	checks := hookProbeChecks([]hookwire.WireStatus{
		wired("brief-length", "/opt/lk hook brief-length"),
		wired("pr-guard", "/opt/lk hook pr-guard"),
		wired("push-guard", "/opt/lk hook push-guard"),
	}, f)
	if len(checks) != 1 {
		t.Fatalf("want one verdict, got %d", len(checks))
	}
	for _, name := range []string{"brief-length", "pr-guard", "push-guard"} {
		if !strings.Contains(checks[0].Detail, name) {
			t.Errorf("verdict does not name %s: %q", name, checks[0].Detail)
		}
	}
}

// Different command shapes for one binary still dedupe to one probe.
func TestHookProbe_ShapesOfOneBinaryShareAProbe(t *testing.T) {
	f := &fakeProber{}
	hookProbeChecks([]hookwire.WireStatus{
		wired("push-guard", `"/opt/lk" hook push-guard`),
		wired("pr-guard", `LEADKIT_DEBUG=1 /opt/lk hook pr-guard`),
	}, f)
	if len(f.probed) != 1 {
		t.Errorf("probed %d times, want 1: %v", len(f.probed), f.probed)
	}
}

// End to end, with the production prober: a settings.json that names a binary in the repo
// (the cloned-repo case) must be reported without running it.
func TestHookWiringChecks_DoesNotExecuteARepoChosenBinary(t *testing.T) {
	repo := t.TempDir()
	bin, marker := markerScript(t, repo, "leadkit")
	settings := filepath.Join(repo, "settings.json")
	cfg := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"` + bin + ` hook push-guard"}]}]}}`
	if err := os.WriteFile(settings, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// PATH does not include repo, so nothing enumerates it as an install.
	t.Setenv("PATH", "/usr/bin:/bin")

	checks := hookWiringChecks([]string{settings})

	if ran(t, marker) {
		t.Fatal("`doctor` executed a binary named by a settings file")
	}
	var probe *Check
	for i := range checks {
		if checks[i].Name == "hook-probe" {
			probe = &checks[i]
		}
	}
	if probe == nil || !probe.OK || !strings.Contains(probe.Detail, "not executed") {
		t.Errorf("want an info verdict saying it was not executed, got %+v", probe)
	}
}
