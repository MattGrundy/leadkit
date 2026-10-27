package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain force-enables daemonQueueRootExists for the whole install test package: the
// pre-Fix-3 daemon tests pass synthetic queue paths ("/q", "/Users/x/queue", ...) purely
// to exercise label/plist PLANNING and predate the missing-queue guard, so they assume
// the queue exists. The dedicated Fix-3 test restores the real stat for its own scope.
func TestMain(m *testing.M) {
	daemonQueueRootExists = func(string) bool { return true }
	os.Exit(m.Run())
}

func TestCronToLaunchd_EveryNMinutes(t *testing.T) {
	sec, cal, note := cronToLaunchd("*/20 * * * *")
	if sec != 1200 || cal != nil || note != "" {
		t.Errorf("cronToLaunchd(*/20 * * * *) = (%d, %+v, %q), want (1200, nil, \"\")", sec, cal, note)
	}
}

func TestCronToLaunchd_DailyAtFixedTime(t *testing.T) {
	sec, cal, note := cronToLaunchd("30 4 * * *")
	if sec != 0 || cal == nil || *cal != (CalendarTime{Hour: 4, Minute: 30}) || note != "" {
		t.Errorf("cronToLaunchd(30 4 * * *) = (%d, %+v, %q), want (0, {4 30}, \"\")", sec, cal, note)
	}
}

func TestCronToLaunchd_UnsupportedShapeFallsBackWithNote(t *testing.T) {
	sec, cal, note := cronToLaunchd("0 9 * * 1-5") // weekday constraint — not handled
	if sec != 1200 || cal != nil || note == "" {
		t.Errorf("cronToLaunchd(weekday cron) = (%d, %+v, %q), want a 1200s fallback with a non-empty note", sec, cal, note)
	}
}

func TestCronToLaunchd_MalformedFallsBack(t *testing.T) {
	sec, cal, note := cronToLaunchd("not a cron expr")
	if sec != 1200 || cal != nil || note == "" {
		t.Errorf("cronToLaunchd(garbage) = (%d, %+v, %q), want a 1200s fallback with a note", sec, cal, note)
	}
}

func TestRenderPlist_ContainsExpectedFields(t *testing.T) {
	plist := RenderPlist(PlistSpec{
		Label:             "dev.leadkit.waker",
		ProgramArguments:  []string{"/usr/local/bin/leadkit", "queue", "watch", "--queue", "/Users/x/queue"},
		KeepAlive:         true,
		RunAtLoad:         true,
		StandardOutPath:   "/Users/x/.leadkit/waker.log",
		StandardErrorPath: "/Users/x/.leadkit/waker.err.log",
	})
	s := string(plist)
	for _, want := range []string{
		"<key>Label</key>", "<string>dev.leadkit.waker</string>",
		"<string>/usr/local/bin/leadkit</string>", "<string>queue</string>", "<string>watch</string>",
		"<string>/Users/x/queue</string>",
		"<key>KeepAlive</key>", "<true/>",
		"<key>RunAtLoad</key>",
		"<key>StandardOutPath</key>", "/Users/x/.leadkit/waker.log",
		"<key>StandardErrorPath</key>", "/Users/x/.leadkit/waker.err.log",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, s)
		}
	}
	if !strings.HasPrefix(s, "<?xml") {
		t.Errorf("rendered plist should start with the XML declaration, got:\n%s", s)
	}
}

func TestRenderPlist_StartCalendarInterval(t *testing.T) {
	plist := RenderPlist(PlistSpec{
		Label:            "dev.leadkit.sweep",
		ProgramArguments: []string{"/bin/leadkit", "queue", "sweep"},
		StartCalendar:    &CalendarTime{Hour: 4, Minute: 30},
	})
	s := string(plist)
	for _, want := range []string{"<key>StartCalendarInterval</key>", "<key>Hour</key>", "<integer>4</integer>", "<key>Minute</key>", "<integer>30</integer>"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plist missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "StartInterval<") {
		t.Errorf("StartCalendar set — plain StartInterval must not also appear:\n%s", s)
	}
}

func TestComputeDaemonPlan_NonDarwin_ReportsUnsupportedAndSkips(t *testing.T) {
	plan := ComputeDaemonPlan("linux", "/bin/leadkit", "/q", "/log", "*/20 * * * *", t.TempDir(), "")
	if plan.Supported {
		t.Fatal("want Supported=false on linux")
	}
	if !strings.Contains(plan.SkipNote, "linux") || !strings.Contains(plan.SkipNote, "systemd") {
		t.Errorf("SkipNote = %q, want it to mention linux + systemd TODO", plan.SkipNote)
	}
	if len(plan.Services) != 0 {
		t.Errorf("want no services planned on an unsupported OS, got %+v", plan.Services)
	}
}

func TestComputeDaemonPlan_Darwin_PlansBothServicesWithExpectedContent(t *testing.T) {
	dir := t.TempDir()
	// queueRoot's project-root basename is "x" — ComputeDaemonPlan namespaces both
	// services' labels/filenames with that slug (no project_id set).
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/Users/x/queue", "/Users/x/.leadkit", "*/20 * * * *", dir, "")
	if !plan.Supported {
		t.Fatal("want Supported=true on darwin")
	}
	if plan.Slug != "x" {
		t.Fatalf("want Slug=%q (queue_root's project-root basename), got %q", "x", plan.Slug)
	}
	if plan.Warning != "" {
		t.Errorf("a resolvable slug should carry no warning, got %q", plan.Warning)
	}
	if len(plan.Services) != 2 {
		t.Fatalf("want 2 services (waker, sweep), got %d: %+v", len(plan.Services), plan.Services)
	}
	byLabel := map[string]ServiceStatus{}
	for _, s := range plan.Services {
		byLabel[s.Spec.Label] = s
	}
	waker, ok := byLabel["dev.leadkit.waker.x"]
	if !ok || !waker.Changed {
		t.Fatalf("want a fresh, Changed=true dev.leadkit.waker.x plan, got %+v", waker)
	}
	if !strings.HasSuffix(waker.Spec.PlistPath, "dev.leadkit.waker.x.plist") {
		t.Errorf("waker plist path should carry the slug in its filename, got %s", waker.Spec.PlistPath)
	}
	if !strings.Contains(string(waker.Spec.Plist), "<string>dev.leadkit.waker.x</string>") {
		t.Errorf("waker plist's Label should be namespaced, got:\n%s", waker.Spec.Plist)
	}
	if !strings.Contains(string(waker.Spec.Plist), "queue watch") && !strings.Contains(string(waker.Spec.Plist), "<string>watch</string>") {
		t.Errorf("waker plist missing the `queue watch` invocation:\n%s", waker.Spec.Plist)
	}
	if !strings.Contains(string(waker.Spec.Plist), "<key>KeepAlive</key>\n\t<true/>") {
		t.Errorf("waker plist should have KeepAlive=true:\n%s", waker.Spec.Plist)
	}

	sweep, ok := byLabel["dev.leadkit.sweep.x"]
	if !ok || !sweep.Changed {
		t.Fatalf("want a fresh, Changed=true dev.leadkit.sweep.x plan, got %+v", sweep)
	}
	if !strings.HasSuffix(sweep.Spec.PlistPath, "dev.leadkit.sweep.x.plist") {
		t.Errorf("sweep plist path should carry the slug in its filename, got %s", sweep.Spec.PlistPath)
	}
	if !strings.Contains(string(sweep.Spec.Plist), "<string>sweep</string>") {
		t.Errorf("sweep plist missing the `queue sweep` invocation:\n%s", sweep.Spec.Plist)
	}
	if !strings.Contains(string(sweep.Spec.Plist), "StartInterval") {
		t.Errorf("sweep plist for */20 * * * * should use StartInterval:\n%s", sweep.Spec.Plist)
	}

	if plan.AnyChanged() != true {
		t.Error("AnyChanged() should be true — both services are fresh")
	}
}

func TestComputeDaemonPlan_AlreadyInstalledIdenticalContent_NotChanged(t *testing.T) {
	dir := t.TempDir()
	first := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	for _, s := range first.Services {
		if err := os.WriteFile(s.Spec.PlistPath, s.Spec.Plist, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	second := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	if second.AnyChanged() {
		t.Errorf("re-computing over already-installed, byte-identical plists should report no changes, got %+v", second.Services)
	}
	for _, s := range second.Services {
		if s.Changed {
			t.Errorf("service %s: Changed=true, want false (already installed)", s.Spec.Label)
		}
	}
}

func TestComputeDaemonPlan_DifferentQueueRootMarksChanged(t *testing.T) {
	dir := t.TempDir()
	first := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q-old", "/log", "*/20 * * * *", dir, "")
	for _, s := range first.Services {
		if err := os.WriteFile(s.Spec.PlistPath, s.Spec.Plist, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	second := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q-new", "/log", "*/20 * * * *", dir, "")
	if !second.AnyChanged() {
		t.Error("a different queue root should mark both services Changed=true (plist content differs)")
	}
}

func TestApplyDaemonPlan_DryRunNeverCalled_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	// The dry-run path: compute + format the plan, print it, and simply never call Apply.
	_ = FormatDaemonPlan(plan)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run must write nothing, found: %v", entries)
	}
}

func TestApplyDaemonPlan_WritesPlistsAndCallsStubbedLaunchctl(t *testing.T) {
	orig := runLaunchctl
	var calls [][]string
	runLaunchctl = func(args ...string) (string, error) {
		calls = append(calls, args)
		if len(args) > 0 && args[0] == "bootstrap" {
			return "", nil // pretend bootstrap always succeeds
		}
		return "", nil
	}
	t.Cleanup(func() { runLaunchctl = orig })

	dir := t.TempDir()
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results, err := ApplyDaemonPlan(plan, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %+v", results)
	}
	for _, r := range results {
		if r.Skipped {
			t.Errorf("fresh service %s should not be Skipped", r.Label)
		}
		if r.LoadMethod != "bootstrap" {
			t.Errorf("service %s LoadMethod = %q, want bootstrap (stub succeeds on bootstrap)", r.Label, r.LoadMethod)
		}
		if r.LoadErr != nil {
			t.Errorf("service %s LoadErr = %v, want nil", r.Label, r.LoadErr)
		}
	}
	if len(calls) != 2 {
		t.Errorf("want exactly 2 launchctl calls (one bootstrap per service), got %d: %v", len(calls), calls)
	}

	wakerPlist := filepath.Join(dir, "dev.leadkit.waker.plist")
	if _, err := os.Stat(wakerPlist); err != nil {
		t.Errorf("waker plist not written: %v", err)
	}
	sweepPlist := filepath.Join(dir, "dev.leadkit.sweep.plist")
	if _, err := os.Stat(sweepPlist); err != nil {
		t.Errorf("sweep plist not written: %v", err)
	}
}

func TestApplyDaemonPlan_FallsBackToLoadWhenBootstrapFails(t *testing.T) {
	orig := runLaunchctl
	var methodsTried []string
	runLaunchctl = func(args ...string) (string, error) {
		methodsTried = append(methodsTried, args[0])
		if args[0] == "bootstrap" {
			return "already bootstrapped", &fakeExitError{}
		}
		return "", nil // load succeeds
	}
	t.Cleanup(func() { runLaunchctl = orig })

	dir := t.TempDir()
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results, err := ApplyDaemonPlan(plan, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.LoadMethod != "load" {
			t.Errorf("service %s LoadMethod = %q, want load (bootstrap stubbed to fail)", r.Label, r.LoadMethod)
		}
	}
	if len(methodsTried) == 0 || methodsTried[0] != "bootstrap" {
		t.Errorf("want bootstrap tried before load, got %v", methodsTried)
	}
}

func TestApplyDaemonPlan_BackupsExistingPlistBeforeOverwriting(t *testing.T) {
	orig := runLaunchctl
	runLaunchctl = func(args ...string) (string, error) { return "", nil }
	t.Cleanup(func() { runLaunchctl = orig })

	dir := t.TempDir()
	wakerPath := filepath.Join(dir, "dev.leadkit.waker.plist")
	oldContent := []byte("old plist content")
	if err := os.WriteFile(wakerPath, oldContent, 0o644); err != nil {
		t.Fatal(err)
	}

	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results, err := ApplyDaemonPlan(plan, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	var wakerResult *DaemonApplyResult
	for i := range results {
		if results[i].Label == "dev.leadkit.waker" {
			wakerResult = &results[i]
		}
	}
	if wakerResult == nil || wakerResult.BackupPath == "" {
		t.Fatalf("want a BackupPath for the waker service (a plist already existed), got %+v", wakerResult)
	}
	backupData, err := os.ReadFile(wakerResult.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupData) != string(oldContent) {
		t.Errorf("backup content = %q, want %q", backupData, oldContent)
	}
}

func TestApplyDaemonPlan_IdempotentSecondRunSkipsAndNeverCallsLaunchctl(t *testing.T) {
	orig := runLaunchctl
	calls := 0
	runLaunchctl = func(args ...string) (string, error) { calls++; return "", nil }
	t.Cleanup(func() { runLaunchctl = orig })

	dir := t.TempDir()
	plan1 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	if _, err := ApplyDaemonPlan(plan1, dir, true); err != nil {
		t.Fatal(err)
	}
	firstCallCount := calls

	plan2 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results2, err := ApplyDaemonPlan(plan2, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results2 {
		if !r.Skipped {
			t.Errorf("second run: service %s should be Skipped (already installed, unchanged), got %+v", r.Label, r)
		}
	}
	if calls != firstCallCount {
		t.Errorf("second (idempotent) run must not call launchctl at all: calls went from %d to %d", firstCallCount, calls)
	}
}

func TestComputeDaemonPlan_UnsupportedOS_ApplyIsANoOp(t *testing.T) {
	dir := t.TempDir()
	plan := ComputeDaemonPlan("linux", "/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results, err := ApplyDaemonPlan(plan, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("want no results on an unsupported OS, got %+v", results)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("want nothing written on an unsupported OS, found: %v", entries)
	}
}

// TestApplyDaemonPlan_LoadServicesFalse_WritesPlistsButNeverCallsLaunchctl is the
// escape hatch a subprocess test (in cmd/leadkit) relies on: it cannot inject the
// runLaunchctl stub into a separately-exec'd binary, so ApplyDaemonPlan itself must
// be able to skip the launchctl call entirely while still writing the plist content.
func TestApplyDaemonPlan_LoadServicesFalse_WritesPlistsButNeverCallsLaunchctl(t *testing.T) {
	orig := runLaunchctl
	calls := 0
	runLaunchctl = func(args ...string) (string, error) { calls++; return "", nil }
	t.Cleanup(func() { runLaunchctl = orig })

	dir := t.TempDir()
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "")
	results, err := ApplyDaemonPlan(plan, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("loadServices=false must never call launchctl, got %d calls", calls)
	}
	for _, r := range results {
		if r.LoadMethod != "skipped" {
			t.Errorf("service %s LoadMethod = %q, want \"skipped\"", r.Label, r.LoadMethod)
		}
		if r.Skipped {
			t.Errorf("service %s: Skipped should mean 'already installed', not 'load was skipped' — got true unexpectedly", r.Label)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "dev.leadkit.waker.plist")); err != nil {
		t.Error("the plist itself must still be written even with loadServices=false")
	}
}

// fakeExitError is a minimal error stand-in for a failed launchctl call in tests —
// its only job is to be a non-nil error.
type fakeExitError struct{}

func (fakeExitError) Error() string { return "fake exit error" }

// --- Per-project daemon namespacing (docs/build-tasks/per-project-daemons.md) ---

func TestResolveProjectSlug(t *testing.T) {
	cases := []struct {
		name, projectID, queueRoot, want string
	}{
		{"queue_root project-root basename", "", "/Users/x/legacy-migration/queue", "legacy-migration"},
		{"explicit project_id wins over queue_root", "Widget Factory!", "/Users/x/other-project/queue", "widget-factory"},
		{"slugify lowercases + strips spaces/symbols", "", "/Users/x/My Project/queue", "my-project"},
		{"slugify collapses runs of symbols to one dash", "", "/Users/x/widget_factory!!/queue", "widget-factory"},
		{"empty project_id and empty queue_root -> unresolvable", "", "", ""},
		{"queue_root with no meaningful parent basename -> unresolvable", "", "/q", ""},
		{"trailing slash on queue_root doesn't change the basename", "", "/Users/x/another-project/queue/", "another-project"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveProjectSlug(c.projectID, c.queueRoot)
			if got != c.want {
				t.Errorf("ResolveProjectSlug(%q, %q) = %q, want %q", c.projectID, c.queueRoot, got, c.want)
			}
		})
	}
}

// TestComputeDaemonPlan_TwoProjects_DisjointLabelsAndFilenames_NoCollision is the core
// regression this task exists for: two projects sharing ONE LaunchAgents dir (the
// real-world scenario) must get fully disjoint labels + plist paths, and applying one
// must never mark the other's (not-yet-installed) services as already-installed —
// i.e. init of a NEW project never boots out, nor gets mistaken for, another's agents.
func TestComputeDaemonPlan_TwoProjects_DisjointLabelsAndFilenames_NoCollision(t *testing.T) {
	dir := t.TempDir() // shared LaunchAgents dir — the collision scenario
	planA := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/Users/x/legacy-migration/queue", "/log-a", "*/20 * * * *", dir, "")
	planB := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/Users/x/another-project/queue", "/log-b", "*/20 * * * *", dir, "")

	if planA.Slug == "" || planB.Slug == "" || planA.Slug == planB.Slug {
		t.Fatalf("want distinct, non-empty slugs, got A=%q B=%q", planA.Slug, planB.Slug)
	}

	labelsA, pathsA := map[string]bool{}, map[string]bool{}
	for _, s := range planA.Services {
		labelsA[s.Spec.Label] = true
		pathsA[s.Spec.PlistPath] = true
	}
	for _, s := range planB.Services {
		if labelsA[s.Spec.Label] {
			t.Errorf("label collision: %s used by both project A and B", s.Spec.Label)
		}
		if pathsA[s.Spec.PlistPath] {
			t.Errorf("plist path collision: %s used by both project A and B", s.Spec.PlistPath)
		}
	}

	orig := runLaunchctl
	runLaunchctl = func(args ...string) (string, error) { return "", nil }
	t.Cleanup(func() { runLaunchctl = orig })

	// Install project A only.
	if _, err := ApplyDaemonPlan(planA, dir, false); err != nil {
		t.Fatal(err)
	}

	// Re-init of the SAME project (A) is idempotent — touches only its own slug.
	planA2 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/Users/x/legacy-migration/queue", "/log-a", "*/20 * * * *", dir, "")
	if planA2.AnyChanged() {
		t.Errorf("re-init of the same project should be idempotent (unchanged), got %+v", planA2.Services)
	}

	// Init of the NEW project (B) must still see its own services as completely
	// fresh — A's apply must not have written, nor be mistaken for, B's plists.
	planB2 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/Users/x/another-project/queue", "/log-b", "*/20 * * * *", dir, "")
	if !planB2.AnyChanged() {
		t.Errorf("project B's services should still be fresh/unwritten after only A was applied, got %+v", planB2.Services)
	}
	for _, s := range planB2.Services {
		if !s.Changed {
			t.Errorf("service %s should be Changed=true (never written) — A's apply must not have clobbered or pre-satisfied it", s.Spec.Label)
		}
		if _, err := os.Stat(s.Spec.PlistPath); err == nil {
			t.Errorf("service %s: plist should not exist yet — B was never applied", s.Spec.Label)
		}
	}
}

func TestComputeDaemonPlan_SameProjectIDTwice_IdenticalLabelsAndFilenames(t *testing.T) {
	dir := t.TempDir()
	p1 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "another-project")
	p2 := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", "/q", "/log", "*/20 * * * *", dir, "another-project")
	if p1.Slug != "another-project" || p2.Slug != "another-project" {
		t.Fatalf("want Slug=another-project both times, got %q and %q", p1.Slug, p2.Slug)
	}
	for i := range p1.Services {
		if p1.Services[i].Spec.Label != p2.Services[i].Spec.Label {
			t.Errorf("label %q != %q for the same project_id", p1.Services[i].Spec.Label, p2.Services[i].Spec.Label)
		}
		if p1.Services[i].Spec.PlistPath != p2.Services[i].Spec.PlistPath {
			t.Errorf("plist path %q != %q for the same project_id", p1.Services[i].Spec.PlistPath, p2.Services[i].Spec.PlistPath)
		}
	}
}

// TestComputeDaemonPlan_PlistUsesPassedBinaryPath_NotHardcoded asserts the "already
// OK, just assert it" part of the spec: ProgramArguments[0] is whatever binaryPath the
// caller passed in (main.go wires it from os.Executable()/ResolveBinaryPath) — never a
// hardcoded string — so a stable installed binary yields a stable plist.
func TestComputeDaemonPlan_PlistUsesPassedBinaryPath_NotHardcoded(t *testing.T) {
	dir := t.TempDir()
	const binaryPath = "/opt/leadkit/bin/leadkit-installed"
	plan := ComputeDaemonPlan("darwin", binaryPath, "/q", "/log", "*/20 * * * *", dir, "")
	for _, s := range plan.Services {
		if !strings.Contains(string(s.Spec.Plist), "<string>"+binaryPath+"</string>") {
			t.Errorf("service %s plist should embed the passed binary path %q, got:\n%s", s.Spec.Label, binaryPath, s.Spec.Plist)
		}
	}
}

// --- Fix 3: guard a daemon plist against a non-existent queue dir ---

// TestComputeDaemonPlan_MissingQueueRoot_SkipsWithNote exercises the REAL stat check
// (restored from TestMain's force-true for this test only): a resolved queue root that
// doesn't exist must skip the whole daemon plan with a clear note, while an existing
// one plans daemons exactly as today.
func TestComputeDaemonPlan_MissingQueueRoot_SkipsWithNote(t *testing.T) {
	orig := daemonQueueRootExists
	daemonQueueRootExists = realDaemonQueueRootExists
	t.Cleanup(func() { daemonQueueRootExists = orig })

	launchDir := t.TempDir()

	// A queue root that does not exist -> skip, no services, a note that names it.
	missing := filepath.Join(t.TempDir(), "legacy-migration", "queue")
	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", missing, "/log", "*/20 * * * *", launchDir, "")
	if plan.Supported {
		t.Error("want Supported=false (skipped) when the queue root does not exist")
	}
	if len(plan.Services) != 0 {
		t.Errorf("want no daemon services planned for a missing queue root, got %+v", plan.Services)
	}
	if !strings.Contains(plan.SkipNote, missing) || !strings.Contains(plan.SkipNote, "does not exist") {
		t.Errorf("SkipNote = %q, want it to name the missing queue root and say it does not exist", plan.SkipNote)
	}
	if !strings.Contains(FormatDaemonPlan(plan), "does not exist") {
		t.Error("FormatDaemonPlan should surface the missing-queue skip note")
	}
	// The skip is a no-op for Apply — nothing written, never an error.
	results, err := ApplyDaemonPlan(plan, launchDir, false)
	if err != nil {
		t.Fatalf("ApplyDaemonPlan on a skipped (missing-queue) plan must not error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("want no apply results for a skipped plan, got %+v", results)
	}
	if entries, _ := os.ReadDir(launchDir); len(entries) != 0 {
		t.Errorf("a skipped daemon plan must write nothing, found: %v", entries)
	}

	// An EXISTING queue root -> daemons planned as today (the parent basename drives
	// the slug, so both services are namespaced and Changed=true on a fresh dir).
	projectRoot := filepath.Join(t.TempDir(), "legacy-migration")
	existingQueue := filepath.Join(projectRoot, "queue")
	if err := os.MkdirAll(existingQueue, 0o755); err != nil {
		t.Fatal(err)
	}
	ok := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", existingQueue, "/log", "*/20 * * * *", launchDir, "")
	if !ok.Supported {
		t.Fatalf("want Supported=true for an existing queue root, got SkipNote=%q", ok.SkipNote)
	}
	if ok.Slug != "legacy-migration" {
		t.Errorf("want Slug=legacy-migration (queue's project-root basename), got %q", ok.Slug)
	}
	if len(ok.Services) != 2 || !ok.AnyChanged() {
		t.Errorf("want 2 fresh services for an existing queue root, got %+v", ok.Services)
	}
}

func TestComputeDaemonPlan_EmptySlug_BareLabelPlusWarning(t *testing.T) {
	dir := t.TempDir()
	plan := ComputeDaemonPlan("darwin", "/bin/leadkit", "", "/log", "*/20 * * * *", dir, "")
	if plan.Slug != "" {
		t.Fatalf("want empty Slug when neither project_id nor queue_root resolve, got %q", plan.Slug)
	}
	if plan.Warning == "" {
		t.Error("want a non-empty Warning surfaced when the slug is unresolvable — collisions must never be silent")
	}
	for _, s := range plan.Services {
		if s.Spec.Label != WakerLabelBase && s.Spec.Label != SweepLabelBase {
			t.Errorf("want a bare legacy label when the slug is unresolvable, got %s", s.Spec.Label)
		}
	}
	formatted := FormatDaemonPlan(plan)
	if !strings.Contains(formatted, plan.Warning) {
		t.Error("FormatDaemonPlan should surface the Warning so it's never silent")
	}
}

// TestComputeDaemonPlan_SetsWorkingDirectoryToProjectRoot covers the defense-in-depth
// half of the project-scoped-sweep fix: the generated waker/sweep plists must pin
// launchd's cwd (which defaults to `/`) to the QUEUE's project root, so any cwd-relative
// resolution in the daemon-run sweep resolves against the project, not `/`.
func TestComputeDaemonPlan_SetsWorkingDirectoryToProjectRoot(t *testing.T) {
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "leadkit.yaml"), []byte("queue_root: queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	queueRoot := filepath.Join(proj, "queue")
	if err := os.MkdirAll(queueRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	launchDir := t.TempDir()

	plan := ComputeDaemonPlan("darwin", "/opt/bin/leadkit", queueRoot, "/log", "*/20 * * * *", launchDir, "")
	if !plan.Supported {
		t.Fatalf("plan should be supported for an existing queue: %s", plan.SkipNote)
	}
	if len(plan.Services) != 2 {
		t.Fatalf("want waker+sweep services, got %d", len(plan.Services))
	}
	want := "<key>WorkingDirectory</key>\n\t<string>" + proj + "</string>"
	for _, s := range plan.Services {
		if !strings.Contains(string(s.Spec.Plist), want) {
			t.Errorf("%s plist must set WorkingDirectory to the project root %q:\n%s", s.Spec.Label, proj, s.Spec.Plist)
		}
	}
}

// The skip an adopter can fix by running the steps the other way round must be loud: the plan output
// is long, and a single SkipNote line in it goes unseen.
func TestDaemonPlan_AMissingQueueIsAWarningNotAQuietSkip(t *testing.T) {
	orig := daemonQueueRootExists
	daemonQueueRootExists = func(string) bool { return false }
	t.Cleanup(func() { daemonQueueRootExists = orig })

	plan := ComputeDaemonPlan("darwin", "/bin/leadkit", "/proj/queue", "/log", "*/20 * * * *", t.TempDir(), "widget")
	if plan.Supported || !plan.QueueMissing {
		t.Fatalf("a missing queue is a skip flagged as such: %+v", plan)
	}
	w := plan.SkippedWarning()
	for _, want := range []string{"WARNING", "NOT installed", "/proj/queue", "team scaffold", "init --daemons-only --confirm", "nothing wakes the owners"} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning must say %q: %s", want, w)
		}
	}

	daemonQueueRootExists = func(string) bool { return true }
	ok := ComputeDaemonPlan("darwin", "/bin/leadkit", "/proj/queue", "/log", "*/20 * * * *", t.TempDir(), "widget")
	if ok.QueueMissing || ok.SkippedWarning() != "" {
		t.Errorf("an existing queue must not warn: %+v", ok)
	}
	linux := ComputeDaemonPlan("linux", "/bin/leadkit", "/proj/queue", "/log", "*/20 * * * *", t.TempDir(), "widget")
	if linux.SkippedWarning() != "" {
		t.Errorf("a non-darwin skip is not fixable by ordering, so it is not this warning: %s", linux.SkippedWarning())
	}
}
