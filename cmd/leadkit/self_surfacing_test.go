package main

// End-to-end coverage of self-surfacing daemon diagnostics (docs/build-tasks/
// self-surfacing-diagnostics.md) through the built binary via the mandatory runCLI
// sandbox — the exit-code health semantics, the findings artifact, the self_repair
// mode matrix, `queue repair`, `leadkit status`, and the config-driven notifier
// (which only ever `touch`es a marker file inside the test's own tmp dir).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sweepFixture builds a throwaway queue (never real data) under root: a STALLED lane
// (live pending ticket, empty in-progress) and a BROKEN lane (a dangling symlink). It
// writes a leadkit.yaml carrying the given diagnostics block and returns the profile
// path, the queue root, and the resolved artifact path.
func sweepFixture(t *testing.T, diagnosticsYAML string) (profilePath, queueRoot, artifactPath string) {
	t.Helper()
	root := t.TempDir()
	queueRoot = filepath.Join(root, "queue")
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}

	// STALLED: cr-1 has a live pending ticket, nothing in-progress.
	live := filepath.Join(store, "live.md")
	if err := os.WriteFile(live, []byte("---\ntitle: t\nstatus: pending\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLink(t, live, filepath.Join(queueRoot, "cr-1", "pending", "live.md"))
	if err := os.MkdirAll(filepath.Join(queueRoot, "cr-1", "in-progress"), 0o755); err != nil {
		t.Fatal(err)
	}

	// BROKEN: gate-1 has a dangling symlink (target removed).
	dead := filepath.Join(store, "dead.md")
	if err := os.WriteFile(dead, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLink(t, dead, filepath.Join(queueRoot, "gate-1", "pending", "dead.md"))
	if err := os.Remove(dead); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(queueRoot, "gate-1", "in-progress"), 0o755); err != nil {
		t.Fatal(err)
	}

	profilePath = filepath.Join(root, "leadkit.yaml")
	yaml := "queue_root: queue\nstore_root: tasks\nsweep_status_file: .leadkit/sweep-status.json\n" + diagnosticsYAML
	if err := os.WriteFile(profilePath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath, queueRoot, filepath.Join(root, ".leadkit", "sweep-status.json")
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// readArtifact parses the findings artifact into the fields the tests assert on.
func readArtifact(t *testing.T, path string) struct {
	Stalled int `json:"stalled"`
	Broken  int `json:"broken"`
} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("artifact not written: %v", err)
	}
	var s struct {
		Stalled int `json:"stalled"`
		Broken  int `json:"broken"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("artifact not valid JSON: %v\n%s", err, data)
	}
	return s
}

func TestCLI_Sweep_StallsExitZeroAndWriteArtifact(t *testing.T) {
	bin := testBinary(t)
	profilePath, queueRoot, artifact := sweepFixture(t, "")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueRoot)
	if code != 0 {
		t.Fatalf("sweep with stalls/broken must exit 0 (findings are health-reportable), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "queue-sweep:") {
		t.Errorf("missing the sweep summary line:\n%s", out)
	}
	// command mode (the default) names the repair as the remediation.
	if !strings.Contains(out, "leadkit queue repair") {
		t.Errorf("sweep output must surface the remediation:\n%s", out)
	}
	got := readArtifact(t, artifact)
	if got.Stalled < 1 || got.Broken < 1 {
		t.Errorf("artifact = %+v, want at least 1 stalled and 1 broken", got)
	}
}

func TestCLI_Sweep_UnreadableQueueExitsNonZero(t *testing.T) {
	bin := testBinary(t)
	profilePath, _, _ := sweepFixture(t, "")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	missing := filepath.Join(t.TempDir(), "no-such-queue")
	out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", missing)
	if code == 0 {
		t.Fatalf("a sweep that can't read the queue root must exit non-zero, got 0:\n%s", out)
	}
}

func TestCLI_Sweep_StrictExitsNonZeroOnStalls(t *testing.T) {
	bin := testBinary(t)
	profilePath, queueRoot, artifact := sweepFixture(t, "")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueRoot, "--strict")
	if code == 0 {
		t.Fatalf("--strict must exit non-zero on stalls/broken, got 0:\n%s", out)
	}
	// The artifact is still written even in strict mode (findings are recorded).
	if _, err := os.Stat(artifact); err != nil {
		t.Errorf("--strict must still write the findings artifact: %v", err)
	}
}

func TestCLI_Sweep_SelfRepairMatrix(t *testing.T) {
	bin := testBinary(t)
	danglingRel := filepath.FromSlash("gate-1/pending/dead.md")
	liveRel := filepath.FromSlash("cr-1/pending/live.md")

	cases := []struct {
		mode         string
		wantReaped   bool
		wantInOutput string
	}{
		{"report", false, "surfacing only"},
		{"command", false, "leadkit queue repair"},
		{"auto", true, "self-repair (auto)"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			profilePath, queueRoot, _ := sweepFixture(t, "diagnostics:\n  self_repair: "+tc.mode+"\n")
			env := []string{"LEADKIT_PROFILE=" + profilePath}

			out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueRoot)
			if code != 0 {
				t.Fatalf("sweep exit = %d, want 0:\n%s", code, out)
			}
			if !strings.Contains(out, tc.wantInOutput) {
				t.Errorf("%s mode output missing %q:\n%s", tc.mode, tc.wantInOutput, out)
			}
			_, danglingErr := os.Lstat(filepath.Join(queueRoot, danglingRel))
			if tc.wantReaped {
				if !os.IsNotExist(danglingErr) {
					t.Errorf("auto mode must reap the dangling link (err=%v)", danglingErr)
				}
			} else {
				if danglingErr != nil {
					t.Errorf("%s mode must NOT mutate the queue; dangling link gone (err=%v)", tc.mode, danglingErr)
				}
			}
			// The LIVE ticket must survive in every mode.
			if _, err := os.Lstat(filepath.Join(queueRoot, liveRel)); err != nil {
				t.Errorf("%s mode reaped a LIVE ticket: %v", tc.mode, err)
			}
		})
	}
}

func TestCLI_QueueRepair_DryRunListsThenApplyReapsOnlyDangling(t *testing.T) {
	bin := testBinary(t)
	profilePath, queueRoot, _ := sweepFixture(t, "")
	env := []string{"LEADKIT_PROFILE=" + profilePath}
	dangling := filepath.Join(queueRoot, filepath.FromSlash("gate-1/pending/dead.md"))
	live := filepath.Join(queueRoot, filepath.FromSlash("cr-1/pending/live.md"))

	// Dry-run: lists the dangling link, mutates nothing.
	out, code := runCLI(t, bin, env, "queue", "repair", "--queue", queueRoot)
	if code != 0 {
		t.Fatalf("repair dry-run exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "REAP") || !strings.Contains(out, "dead.md") {
		t.Errorf("dry-run must list the dangling link:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run must say so:\n%s", out)
	}
	if _, err := os.Lstat(dangling); err != nil {
		t.Errorf("dry-run must not reap: %v", err)
	}

	// Apply: reaps the dangling link, logs it, leaves the live ticket.
	out, code = runCLI(t, bin, env, "queue", "repair", "--queue", queueRoot, "--apply")
	if code != 0 {
		t.Fatalf("repair --apply exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "reaped") || !strings.Contains(out, "dead.md") {
		t.Errorf("--apply must log the reap:\n%s", out)
	}
	if _, err := os.Lstat(dangling); !os.IsNotExist(err) {
		t.Errorf("--apply must reap the dangling link (err=%v)", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Errorf("--apply reaped a LIVE ticket: %v", err)
	}
}

func TestCLI_Status_SurfacesArtifactWithRemediationAndDegradesWhenAbsent(t *testing.T) {
	bin := testBinary(t)
	profilePath, queueRoot, _ := sweepFixture(t, "")
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	// Absent artifact (no sweep yet) → clean degrade, never a false alarm.
	out, code := runCLI(t, bin, env, "status")
	if code != 0 {
		t.Fatalf("status exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "may not have run") {
		t.Errorf("status with no artifact must degrade cleanly:\n%s", out)
	}

	// Run the sweep to write the artifact, then status surfaces the findings + fix.
	if _, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueRoot); code != 0 {
		t.Fatalf("seed sweep exit = %d, want 0", code)
	}
	out, code = runCLI(t, bin, env, "status")
	if code != 0 {
		t.Fatalf("status exit = %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"daemon diagnostics", "STALLED cr-1", "BROKEN  gate-1", "leadkit queue repair"} {
		if !strings.Contains(out, want) {
			t.Errorf("status must surface %q:\n%s", want, out)
		}
	}
}

func TestCLI_Sweep_NotifierIsConfigDriven(t *testing.T) {
	bin := testBinary(t)

	// surface_notify ON + a notifier that touches a marker INSIDE tmp → marker appears.
	on := t.TempDir()
	marker := filepath.Join(on, "notified")
	diagOn := "diagnostics:\n  surface_notify: true\n  notify_threshold: 1\n  notify_command: \"touch " + marker + "\"\n"
	profileOn, queueOn, _ := sweepFixture(t, diagOn)
	if _, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profileOn}, "queue", "sweep", "--queue", queueOn); code != 0 {
		t.Fatalf("sweep (notify on) exit = %d, want 0", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("surface_notify=true + threshold crossed must exec the notifier (marker missing): %v", err)
	}

	// surface_notify OFF (default) → the same command is never exec'd.
	off := t.TempDir()
	markerOff := filepath.Join(off, "notified")
	diagOff := "diagnostics:\n  surface_notify: false\n  notify_command: \"touch " + markerOff + "\"\n"
	profileOff, queueOff, _ := sweepFixture(t, diagOff)
	if _, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profileOff}, "queue", "sweep", "--queue", queueOff); code != 0 {
		t.Fatalf("sweep (notify off) exit = %d, want 0", code)
	}
	if _, err := os.Stat(markerOff); !os.IsNotExist(err) {
		t.Errorf("surface_notify=false must never exec the notifier (marker present, err=%v)", err)
	}
}

// stalledQueue builds a throwaway STALLED queue lane under root (a live pending ticket,
// nothing in-progress) so a sweep has findings to write, and returns the queue root.
func stalledQueue(t *testing.T, root string) (queueRoot string) {
	t.Helper()
	queueRoot = filepath.Join(root, "queue")
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(store, "live.md")
	if err := os.WriteFile(live, []byte("---\ntitle: t\nstatus: pending\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLink(t, live, filepath.Join(queueRoot, "cr-1", "pending", "live.md"))
	if err := os.MkdirAll(filepath.Join(queueRoot, "cr-1", "in-progress"), 0o755); err != nil {
		t.Fatal(err)
	}
	return queueRoot
}

// writeProject writes a leadkit.yaml + a milestones.yaml (current_milestone: milestone)
// under root — a minimal project the sweep's artifact/re-anchor can resolve against.
func writeProject(t *testing.T, root, milestone string) {
	t.Helper()
	yaml := "queue_root: queue\nstore_root: tasks\nsweep_status_file: .leadkit/sweep-status.json\nmilestones_file: .leadkit/milestones.yaml\n"
	if err := os.WriteFile(filepath.Join(root, "leadkit.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".leadkit", "milestones.yaml"), []byte("current_milestone: "+milestone+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCLI_Sweep_ArtifactAndReanchorResolveToQueuesProjectNotCwd is the exact bug: a
// sweep of project A's queue INVOKED from project B must write A's findings artifact
// into A (not B) and re-anchor on A's milestones (not B's) — the outputs follow the
// swept --queue's project, independent of the invoking cwd/profile.
func TestCLI_Sweep_ArtifactAndReanchorResolveToQueuesProjectNotCwd(t *testing.T) {
	bin := testBinary(t)

	// Project A owns the queue; its artifact + milestones must be the ones used.
	projA := t.TempDir()
	queueA := stalledQueue(t, projA)
	writeProject(t, projA, "A-ships")

	// Project B is the INVOKING project (LEADKIT_PROFILE=B) — cwd-relative resolution
	// would (buggily) write A's findings here and read B's milestones.
	projB := t.TempDir()
	writeProject(t, projB, "B-ships")

	env := []string{"LEADKIT_PROFILE=" + filepath.Join(projB, "leadkit.yaml")}
	out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueA)
	if code != 0 {
		t.Fatalf("sweep must exit 0, got %d:\n%s", code, out)
	}

	// THE BUG: the findings artifact lands in the QUEUE's project A, never the invoking B.
	if _, err := os.Stat(filepath.Join(projA, ".leadkit", "sweep-status.json")); err != nil {
		t.Errorf("findings artifact must be written to the swept queue's project A: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projB, ".leadkit", "sweep-status.json")); err == nil {
		t.Errorf("findings artifact must NOT be written to the invoking project B")
	}

	// The milestone re-anchor reads A's milestones.yaml, not B's.
	if !strings.Contains(out, "milestone: A-ships") {
		t.Errorf("milestone re-anchor must read the swept queue's project A milestones (A-ships):\n%s", out)
	}
	if strings.Contains(out, "milestone: B-ships") {
		t.Errorf("milestone re-anchor must NOT read the invoking project B milestones (B-ships):\n%s", out)
	}
}

// TestCLI_Sweep_UnresolvableQueueProjectDegradesToCwdRelative covers the degrade-safe
// fallback: a bare/orphan queue with no leadkit.yaml above it must NOT crash — the sweep
// falls back to today's cwd-relative artifact (best-effort) and still exits cleanly.
func TestCLI_Sweep_UnresolvableQueueProjectDegradesToCwdRelative(t *testing.T) {
	bin := testBinary(t)

	// A bare queue: an empty dir with no leadkit.yaml anywhere above it.
	bare := t.TempDir()
	queueBare := filepath.Join(bare, "queue")
	if err := os.MkdirAll(queueBare, 0o755); err != nil {
		t.Fatal(err)
	}

	// The invoking project B (LEADKIT_PROFILE=B) is the cwd-relative fallback target.
	projB := t.TempDir()
	writeProject(t, projB, "B-ships")

	env := []string{"LEADKIT_PROFILE=" + filepath.Join(projB, "leadkit.yaml")}
	out, code := runCLI(t, bin, env, "queue", "sweep", "--queue", queueBare)
	if code != 0 {
		t.Fatalf("an unresolvable-project queue must still sweep cleanly (best-effort), got %d:\n%s", code, out)
	}
	// Degraded to cwd-relative: the artifact lands in the invoking profile's project B.
	if _, err := os.Stat(filepath.Join(projB, ".leadkit", "sweep-status.json")); err != nil {
		t.Errorf("unresolvable queue project must degrade to the cwd-relative artifact in B: %v", err)
	}
}

// `self_repair: auto` is the daemon acting unattended, so it does what its name promises (repair dangling
// links) and nothing else. A stray directory is reported by the sweep and left for a person to clear.
func TestCLI_Sweep_AutoRepairReapsTheDanglingLinkButNeverClearsAStray(t *testing.T) {
	bin := testBinary(t)
	profilePath, queueRoot, _ := sweepFixture(t, "diagnostics:\n  self_repair: auto\n")
	stray := filepath.Join(queueRoot, "gate-1", "pending", "in-progress")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, bin, []string{"LEADKIT_PROFILE=" + profilePath}, "queue", "sweep", "--queue", queueRoot)
	if code != 0 {
		t.Fatalf("sweep exit = %d:\n%s", code, out)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "gate-1", "pending", "dead.md")); !os.IsNotExist(err) {
		t.Errorf("auto mode still reaps the dangling link: %v", err)
	}
	if fi, err := os.Stat(stray); err != nil || !fi.IsDir() {
		t.Errorf("auto mode must not delete a stray directory, even an empty one: %v", err)
	}
	if !strings.Contains(out, "STRAY   gate-1/pending/in-progress") {
		t.Errorf("the sweep reports the stray:\n%s", out)
	}
}
