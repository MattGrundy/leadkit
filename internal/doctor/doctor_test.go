package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/usage"
)

// cleanProfile builds a Profile whose every critical/warn path actually exists —
// t.TempDir() fixtures only, never the real project.
func cleanProfile(t *testing.T) *config.Profile {
	t.Helper()
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	recordsDir := filepath.Join(root, "records")
	rolesPath := filepath.Join(queueRoot, "ROLES")
	sessionTasksRoot := filepath.Join(root, "session-tasks")

	for _, d := range []string{storeRoot, recordsDir, sessionTasksRoot, filepath.Join(queueRoot, "cr-1"), filepath.Join(queueRoot, "crit-1")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(rolesPath, []byte("code-review\tcr-1\ncritique\tcrit-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return &config.Profile{
		QueueRoot: queueRoot, StoreRoot: storeRoot, RecordsDir: recordsDir, RolesPath: rolesPath,
		TelemetryLog: filepath.Join(root, "tool-usage.jsonl"), SessionTasksRoot: sessionTasksRoot,
		GoalFile: filepath.Join(root, "goal.md"), HandoverFile: filepath.Join(root, "handover.md"),
		// A concrete, definitely-nonexistent name — an EMPTY KeychainService can hit
		// an ambiguous-match GUI auth prompt in the real `security` CLI and hang the
		// test forever (see keychainCheck's guard for the same reasoning).
		KeychainService: "leadkit-doctor-test-nonexistent-entry",
	}
}

func TestRun_CleanSetupPasses(t *testing.T) {
	prof := cleanProfile(t)
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("want all critical checks to pass, got: %+v", report.Checks)
	}
}

func TestRun_MissingCriticalPathFails(t *testing.T) {
	prof := cleanProfile(t)
	prof.StoreRoot = filepath.Join(t.TempDir(), "does-not-exist")

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if report.AllCriticalPass() {
		t.Fatal("want AllCriticalPass=false when store_root is missing")
	}
	found := false
	for _, c := range report.Checks {
		if c.Name == "store_root" {
			found = true
			if c.OK || c.Severity != SeverityCritical {
				t.Errorf("store_root check = %+v, want OK=false Severity=critical", c)
			}
		}
	}
	if !found {
		t.Fatal("no store_root check in report")
	}
}

func TestRun_MissingWarnOnlyPathStillPasses(t *testing.T) {
	prof := cleanProfile(t)
	prof.GoalFile = filepath.Join(t.TempDir(), "does-not-exist", "goal.md")

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("a missing goal_file must not fail critical checks, got: %+v", report.Checks)
	}
	for _, c := range report.Checks {
		if c.Name == "goal_file" && (c.OK || c.Severity != SeverityWarn) {
			t.Errorf("goal_file check = %+v, want OK=false Severity=warn", c)
		}
	}
}

func TestRun_ROLESProseRowIsFlaggedCritical(t *testing.T) {
	prof := cleanProfile(t)
	// Append a prose paragraph — the live-pollution bug this check exists to catch.
	f, err := os.OpenFile(prof.RolesPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("A clone is held only for work that is blocking mainline.\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if report.AllCriticalPass() {
		t.Fatal("want AllCriticalPass=false with a prose row in ROLES")
	}
	for _, c := range report.Checks {
		if c.Name == "roles-sanity" {
			if c.OK || c.Severity != SeverityCritical {
				t.Errorf("roles-sanity = %+v, want OK=false Severity=critical", c)
			}
			if !strings.Contains(c.Detail, "blocking mainline") {
				t.Errorf("roles-sanity detail = %q, want it to name the offending row", c.Detail)
			}
		}
	}
}

func TestRun_ROLESRowPointingAtMissingDirIsFlagged(t *testing.T) {
	prof := cleanProfile(t)
	if err := os.WriteFile(prof.RolesPath, []byte("code-review\tcr-1\nghost-role\tcr-does-not-exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if report.AllCriticalPass() {
		t.Fatal("want AllCriticalPass=false — ghost-role points at a non-existent queue dir")
	}
}

// TestRun_ROLESTwoRowsSharingOneLaneIsAccepted is the doctor half of queue-mode-
// enforcement item 4 (GAP-4): roles-sanity must ACCEPT (not flag) two canonical
// roles bound to the same live lane dir — the multi-role binding
// internal/team.ComputeScaffoldPlan can now produce. No code change was needed for
// this (rolesCheck already validates each row independently, with no
// duplicate-lane check at all) — this test is the regression that keeps it that
// way.
func TestRun_ROLESTwoRowsSharingOneLaneIsAccepted(t *testing.T) {
	prof := cleanProfile(t)
	if err := os.MkdirAll(filepath.Join(prof.QueueRoot, "crit-ov"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prof.RolesPath, []byte("code-review\tcr-1\ncritique\tcrit-ov\noutcome-verifier\tcrit-ov\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("two roles sharing one lane must not fail roles-sanity, got: %+v", report.Checks)
	}
	c := checksByName(report.Checks)["roles-sanity"]
	if !c.OK {
		t.Errorf("roles-sanity = %+v, want OK=true — sharing a lane is not malformed", c)
	}
}

// --- gate-only / queue-less project (docs/build-tasks/doctor-gate-only-project.md) ---
//
// A project can legitimately have NO queue (a gate-only external config: push-gate/
// whitelist scope only). A MISSING queue_root is not corruption, so doctor must not go
// RED on it. These tests pin the new behaviour AND guard the regression that a PRESENT-
// but-broken queue still flags at its real severity.

// TestRun_GateOnlyAbsentQueuePassesDoctor is the #123 web-client case: queue_root
// resolves to its DEFAULT (nobody declared a queue) and the path doesn't exist — doctor
// FAILed before (3 CRITICALs), must pass (exit 0) after, with the queue/roles checks
// degraded to INFO.
func TestRun_GateOnlyAbsentQueuePassesDoctor(t *testing.T) {
	prof := cleanProfile(t) // store_root/records_dir/session_tasks_root all exist
	// Gate-only / queue-less: queue_root left at its DEFAULT resolution, pointing at a
	// path that does not exist. Set LEADKIT_QUEUE so config.Defaults()'s queue_root (what
	// queueRootExplicit compares against) matches — i.e. queue_root is UNSET/default.
	absent := filepath.Join(t.TempDir(), "queue")
	t.Setenv("LEADKIT_QUEUE", absent)
	prof.QueueRoot = config.Defaults().QueueRoot // == absent, the default (unset) resolution
	prof.RolesPath = filepath.Join(prof.QueueRoot, "ROLES")

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("gate-only project (absent, default queue) must PASS doctor (exit 0), got: %+v", report.Checks)
	}
	byName := checksByName(report.Checks)
	for _, name := range []string{"queue_root", "roles_path", "roles-sanity"} {
		c := byName[name]
		if c.Severity == SeverityCritical {
			t.Errorf("%s = %+v, want INFO (never Critical) for an absent default queue", name, c)
		}
		if c.Severity != SeverityInfo {
			t.Errorf("%s = %+v, want Severity=info for a default (unset) absent queue_root", name, c)
		}
	}
}

// TestRun_ExplicitMissingQueueRootIsWarnNotCritical: an adopter EXPLICITLY declared a
// queue_root but the path is missing → WARN (named), not Critical/FAIL. A missing dir is
// not corruption; the queue tooling errors clearly if used.
func TestRun_ExplicitMissingQueueRootIsWarnNotCritical(t *testing.T) {
	prof := cleanProfile(t)
	prof.QueueRoot = filepath.Join(t.TempDir(), "declared-but-missing", "queue")
	prof.RolesPath = filepath.Join(prof.QueueRoot, "ROLES")

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("an explicitly-declared-but-missing queue_root must not FAIL doctor, got: %+v", report.Checks)
	}
	byName := checksByName(report.Checks)
	for _, name := range []string{"queue_root", "roles_path", "roles-sanity"} {
		c := byName[name]
		if c.Severity == SeverityCritical {
			t.Errorf("%s = %+v, want WARN (never Critical) for an explicit-but-missing queue_root", name, c)
		}
		if c.Severity != SeverityWarn {
			t.Errorf("%s = %+v, want Severity=warn for an explicitly-declared missing queue_root", name, c)
		}
	}
	if qr := byName["queue_root"]; !strings.Contains(qr.Detail, "queue_root") {
		t.Errorf("queue_root detail = %q, want it to name the configured queue_root", qr.Detail)
	}
}

// TestRun_PresentButBrokenROLESStillFlagsCritical is the regression guard: the absent-
// queue degrade must NOT mask a genuinely broken queue. The queue dir EXISTS but ROLES
// is unreadable → real Critical, as today.
func TestRun_PresentButBrokenROLESStillFlagsCritical(t *testing.T) {
	prof := cleanProfile(t) // QueueRoot dir exists (present)
	if err := os.Remove(prof.RolesPath); err != nil {
		t.Fatal(err)
	}

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if report.AllCriticalPass() {
		t.Fatal("a PRESENT queue with an unreadable ROLES must still FAIL doctor (not masked as absent)")
	}
	c := checksByName(report.Checks)["roles-sanity"]
	if c.OK || c.Severity != SeverityCritical {
		t.Errorf("roles-sanity = %+v, want OK=false Severity=critical (present-but-broken queue)", c)
	}
	rp := checksByName(report.Checks)["roles_path"]
	if rp.OK || rp.Severity != SeverityCritical {
		t.Errorf("roles_path = %+v, want OK=false Severity=critical (present-but-broken queue)", rp)
	}
}

// TestRun_HealthyQueueUnchanged pins that a healthy present queue is unaffected: the
// queue/roles checks stay OK Critical exactly as before the gate-only change.
func TestRun_HealthyQueueUnchanged(t *testing.T) {
	prof := cleanProfile(t)
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("a healthy present queue must pass, got: %+v", report.Checks)
	}
	byName := checksByName(report.Checks)
	for _, name := range []string{"queue_root", "roles_path", "roles-sanity"} {
		c := byName[name]
		if !c.OK || c.Severity != SeverityCritical {
			t.Errorf("%s = %+v, want OK=true Severity=critical (healthy present queue unchanged)", name, c)
		}
	}
}

// TestRun_ProfileWarnDoesNotForceFail: the `profile` roles_path/goal/handover warn stays
// a WARN and must not compound into FAIL on its own (here: an absent queue makes
// roles_path missing → prof.Validate() warns → the profile check is Warn, exit still 0).
func TestRun_ProfileWarnDoesNotForceFail(t *testing.T) {
	prof := cleanProfile(t)
	absent := filepath.Join(t.TempDir(), "queue")
	t.Setenv("LEADKIT_QUEUE", absent)
	prof.QueueRoot = config.Defaults().QueueRoot
	prof.RolesPath = filepath.Join(prof.QueueRoot, "ROLES")

	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("a profile validation warn must not force FAIL, got: %+v", report.Checks)
	}
	c := checksByName(report.Checks)["profile"]
	if c.Severity != SeverityWarn {
		t.Errorf("profile = %+v, want Severity=warn (never Critical)", c)
	}
}

func TestFsnotifyCheck_Passes(t *testing.T) {
	// fsnotify create+close should always succeed in a normal test environment —
	// this just guards against the check itself being broken, not the real machine.
	c := fsnotifyCheck()
	if !c.OK || c.Severity != SeverityCritical {
		t.Errorf("fsnotifyCheck() = %+v, want OK=true Severity=critical", c)
	}
}

func TestKeychainCheck_NeverPanicsAndStaysNonCritical(t *testing.T) {
	// Guarded rather than stubbed: this genuinely runs against the machine (per the
	// spec's "attempt + report ok/err"), but must never be Critical and must never
	// panic regardless of outcome — that's what's actually asserted here.
	prof := cleanProfile(t)
	prof.KeychainService = "leadkit-doctor-test-nonexistent-entry"
	c := keychainCheck(prof)
	if c.Severity == SeverityCritical {
		t.Errorf("keychainCheck severity = %v, want never critical", c.Severity)
	}
}

func TestWakeCommandCheck_UnsetIsInfoNoCheck(t *testing.T) {
	prof := cleanProfile(t)
	prof.WakeCommand = ""
	c := wakeCommandCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("wakeCommandCheck (unset) = %+v, want OK=true Severity=info (no check when unset)", c)
	}
}

func TestWakeCommandCheck_SetButUnresolvableWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.WakeCommand = "leadkit-doctor-nonexistent-wake-binary-xyz {agent} {msg}"
	c := wakeCommandCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("wakeCommandCheck (unresolvable) = %+v, want OK=false Severity=warn (never critical)", c)
	}
}

func TestWakeCommandCheck_SetAndResolvableIsInfoOK(t *testing.T) {
	prof := cleanProfile(t)
	// `sh` is resolvable on any POSIX test machine.
	prof.WakeCommand = "sh -c {msg}"
	c := wakeCommandCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("wakeCommandCheck (resolvable) = %+v, want OK=true Severity=info", c)
	}
}

func TestMilestoneAnchorCheck_AbsentAndUnverified(t *testing.T) {
	// Absent artifact → WARN (never critical).
	absent := &config.Profile{MilestonesFile: filepath.Join(t.TempDir(), "nope.yaml")}
	c := milestoneAnchorCheck(absent)
	if c.OK || c.Severity != SeverityWarn || !strings.Contains(c.Detail, "no milestone DoD artifact declared") {
		t.Errorf("absent artifact = %+v, want warn + no-artifact detail", c)
	}

	// Declared with a manual/unverified condition → WARN, naming it.
	dir := t.TempDir()
	path := filepath.Join(dir, "milestones.yaml")
	if err := os.WriteFile(path, []byte("current_milestone: m1\ndod:\n  - name: tasks-closed\n    type: tasks\n  - name: login-smoke\n    type: manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = milestoneAnchorCheck(&config.Profile{MilestonesFile: path})
	if c.OK || c.Severity != SeverityWarn || !strings.Contains(c.Detail, "login-smoke") {
		t.Errorf("manual condition = %+v, want warn naming login-smoke", c)
	}

	// Declared, all auto-verifiable → INFO, OK (never fails doctor).
	path2 := filepath.Join(dir, "clean.yaml")
	if err := os.WriteFile(path2, []byte("current_milestone: m1\ndod:\n  - name: tasks-closed\n    type: tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = milestoneAnchorCheck(&config.Profile{MilestonesFile: path2})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("clean artifact = %+v, want OK info", c)
	}
}

// checksByName indexes a Report's Checks by Name for easy lookup in the tests below.
func checksByName(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

var allWiredSettingsJSON = `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "SendMessage", "hooks": [{"type":"command","command":"leadkit hook brief-length"}]},
      {"matcher": "Bash", "hooks": [
        {"type":"command","command":"leadkit hook pr-guard"},
        {"type":"command","command":"leadkit hook queue-guard"},
        {"type":"command","command":"leadkit hook usage-logger"},
        {"type":"command","command":"leadkit hook push-guard"}
      ]},
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type":"command","command":"leadkit hook task-mechanism"}]}
    ]
  }
}`

func TestHookWiringChecks_AllWired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(allWiredSettingsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checksByName(hookWiringChecks([]string{path}))
	for _, name := range []string{"hook:brief-length", "hook:pr-guard", "hook:queue-guard", "hook:usage-logger", "hook:push-guard", "hook:task-mechanism"} {
		c, ok := checks[name]
		if !ok || !c.OK || c.Severity != SeverityWarn {
			t.Errorf("checks[%q] = %+v (ok=%v), want OK=true Severity=warn", name, c, ok)
		}
	}
}

func TestHookWiringChecks_SomeMissingIncludingPushGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	only := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"leadkit hook pr-guard"}]}]}}`
	if err := os.WriteFile(path, []byte(only), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checksByName(hookWiringChecks([]string{path}))
	if checks["hook:pr-guard"].OK != true {
		t.Errorf("hook:pr-guard = %+v, want OK=true", checks["hook:pr-guard"])
	}
	pg := checks["hook:push-guard"]
	if pg.OK {
		t.Errorf("hook:push-guard = %+v, want OK=false (missing)", pg)
	}
	if pg.Severity != SeverityWarn {
		t.Errorf("hook:push-guard severity = %v, want warn (missing hooks never fail doctor's exit code)", pg.Severity)
	}
	if !strings.Contains(pg.Detail, "branch-protection") {
		t.Errorf("hook:push-guard detail = %q, want the louder security-specific wording", pg.Detail)
	}
}

func TestHookWiringChecks_NoSettingsFileIsGraceful(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "settings.json")
	checks := hookWiringChecks([]string{missing})
	// Every hook still reports (as missing) — and nothing panics/errors over the
	// absent file, matching doctor's "no settings found, not a crash" contract.
	found := false
	for _, c := range checks {
		if c.Name == "hooks" {
			found = true
			if !c.OK {
				t.Errorf("the 'no settings.json at all' summary check should still be OK (informational), got %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("want a 'hooks' summary check when no settings.json was found anywhere")
	}
	for _, name := range []string{"hook:push-guard", "hook:pr-guard"} {
		if checksByName(checks)[name].OK {
			t.Errorf("checks[%q] should report missing when no settings.json exists", name)
		}
	}
}

func TestHookWiringChecks_WrongMatcherIsFlaggedMisconfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	wrong := `{"hooks":{"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"leadkit hook push-guard"}]}]}}`
	if err := os.WriteFile(path, []byte(wrong), 0o644); err != nil {
		t.Fatal(err)
	}
	pg := checksByName(hookWiringChecks([]string{path}))["hook:push-guard"]
	if pg.OK {
		t.Errorf("hook:push-guard under the wrong matcher = %+v, want OK=false", pg)
	}
	if !strings.Contains(pg.Detail, "MISCONFIGURED") {
		t.Errorf("hook:push-guard detail = %q, want it flagged as misconfigured (not just missing)", pg.Detail)
	}
}

func TestRun_IncludesHookWiringChecksAndStaysAllPassingWithoutSettings(t *testing.T) {
	prof := cleanProfile(t)
	// nil settingsPaths: no settings.json anywhere leadkit looked — every hook
	// reports missing, but since hook checks are Warn-only, AllCriticalPass must
	// still hold (leadkit is legitimately usable pre-install).
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if !report.AllCriticalPass() {
		t.Fatalf("missing hook wiring must not fail critical checks, got: %+v", report.Checks)
	}
	if _, ok := checksByName(report.Checks)["hook:push-guard"]; !ok {
		t.Fatal("Run's report should include a hook:push-guard check")
	}
}

func TestAgentTeamsCheck_EnvVarOn(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "1")
	c := agentTeamsCheck(nil)
	if !c.OK || c.Severity != SeverityCritical {
		t.Errorf("check = %+v, want OK=true Severity=critical", c)
	}
}

func TestAgentTeamsCheck_EnvVarOff(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "0")
	c := agentTeamsCheck(nil)
	if c.OK || c.Severity != SeverityCritical {
		t.Errorf("check = %+v, want OK=false Severity=critical (explicitly off)", c)
	}
}

func TestAgentTeamsCheck_SettingsJSONOn(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := agentTeamsCheck([]string{path})
	if !c.OK || c.Severity != SeverityCritical {
		t.Errorf("check = %+v, want OK=true Severity=critical", c)
	}
}

func TestAgentTeamsCheck_SettingsJSONKeyAbsentIsCriticalOff(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"SOME_OTHER_VAR":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := agentTeamsCheck([]string{path})
	if c.OK || c.Severity != SeverityCritical {
		t.Errorf("check = %+v, want OK=false Severity=critical (readable settings.json, key just absent = off)", c)
	}
}

func TestAgentTeamsCheck_NothingReadableIsInfoNotFalseFail(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	c := agentTeamsCheck([]string{path})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (couldn't verify, never a false Critical fail)", c)
	}
}

func TestRun_AgentTeamsCheckIsFirst(t *testing.T) {
	prof := cleanProfile(t)
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if len(report.Checks) == 0 || report.Checks[0].Name != "agent-teams" {
		t.Fatalf("want agent-teams as the FIRST check, got %+v", report.Checks)
	}
}

// --- subagent-model (doctor-subagent-model.md) ---

func TestSubagentModelCheck_OpusEnvWarns(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "claude-opus-4-8")
	c := subagentModelCheck(nil, "", time.Time{})
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn", c)
	}
	if !strings.Contains(c.Detail, "claude-opus-4-8") || !strings.Contains(c.Detail, "opus") {
		t.Errorf("detail = %q, want it to name the opus model id", c.Detail)
	}
}

func TestSubagentModelCheck_OpusSettingsJSONWarns(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"opus"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := subagentModelCheck([]string{path}, "", time.Time{})
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn", c)
	}
	if !strings.Contains(c.Detail, "opus") {
		t.Errorf("detail = %q, want it to name opus", c.Detail)
	}
}

func TestSubagentModelCheck_SonnetIsInfoOK(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "claude-sonnet-5")
	c := subagentModelCheck(nil, "", time.Time{})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
	if !strings.Contains(c.Detail, "claude-sonnet-5") {
		t.Errorf("detail = %q, want it to name the resolved model", c.Detail)
	}
}

func TestSubagentModelCheck_HaikuIsInfoOK(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"haiku"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := subagentModelCheck([]string{path}, "", time.Time{})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

func TestSubagentModelCheck_UnsetButSettingsReadableWarns(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"env":{"SOME_OTHER_VAR":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := subagentModelCheck([]string{path}, "", time.Time{})
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn (readable settings.json, key just absent = real 'not set')", c)
	}
	if !strings.Contains(c.Detail, "not set") {
		t.Errorf("detail = %q, want it to say the model is not set", c.Detail)
	}
}

func TestSubagentModelCheck_NothingReadableIsInfoNotFalseWarn(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	c := subagentModelCheck([]string{path}, "", time.Time{})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (couldn't verify, never a false Warn)", c)
	}
}

// TestSubagentModelCheck_ReusesSameSettingsPathsHelper is the "no second
// settings-parse path" regression: subagentModelCheck must read the key via the
// SAME readSettingsEnvVar helper agentTeamsCheck uses, against the SAME
// settingsPaths — proven by checking both a shared settings.json file for their
// own distinct keys and getting the expected, independent verdicts.
func TestSubagentModelCheck_ReusesSameSettingsPathsHelper(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "")
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	path := filepath.Join(t.TempDir(), "settings.json")
	shared := `{"env":{"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS":"1","CLAUDE_CODE_SUBAGENT_MODEL":"sonnet"}}`
	if err := os.WriteFile(path, []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	settingsPaths := []string{path}

	at := agentTeamsCheck(settingsPaths)
	if !at.OK || at.Severity != SeverityCritical {
		t.Errorf("agent-teams = %+v, want OK=true Severity=critical (unaffected by this check's addition)", at)
	}
	sm := subagentModelCheck(settingsPaths, "", time.Time{})
	if !sm.OK || sm.Severity != SeverityInfo || !strings.Contains(sm.Detail, "sonnet") {
		t.Errorf("subagent-model = %+v, want OK=true Severity=info naming sonnet — reading the SAME file settingsPaths already pointed at", sm)
	}
}

func TestRun_IncludesSubagentModelCheck(t *testing.T) {
	t.Setenv(subagentModelEnvVar, "claude-sonnet-5")
	prof := cleanProfile(t)
	report := Run(prof, "/tmp/fixture/leadkit.yaml", "leadkit test", nil, "", "", nil, "", "", "", "", "", "", "", "")
	if _, ok := checksByName(report.Checks)["subagent-model"]; !ok {
		t.Fatal("Run's report should include a subagent-model check")
	}
}

func TestLinearOutputCheck_OutputsWithoutLinearIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	c := linearOutputCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (outputs doesn't include linear)", c)
	}
}

func TestLinearOutputCheck_LinearWithoutTeamIDWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.Outputs = []string{"linear"}
	c := linearOutputCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn (linear output but no team_id)", c)
	}
}

func TestLinearOutputCheck_LinearWithTeamIDPasses(t *testing.T) {
	prof := cleanProfile(t)
	prof.Outputs = []string{"linear"}
	prof.Linear.TeamID = "CI"
	c := linearOutputCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (correctly configured — a pass must never warn)", c)
	}
}

func TestPRGuardRawVerbsCheck_NoMisconfigIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{
		"another-project": {RawPRVerbs: []string{"create", "edit"}},
	}
	c := prGuardRawVerbsCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

func TestPRGuardRawVerbsCheck_MergeInRawVerbsWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{
		"misconfigured": {RawPRVerbs: []string{"merge"}},
	}
	c := prGuardRawVerbsCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn", c)
	}
}

func TestDaemonAgentsCheck_EmptyDirArg_NotChecked(t *testing.T) {
	prof := cleanProfile(t)
	c := daemonAgentsCheck(prof, "")
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (caller declined the check)", c)
	}
}

func TestDaemonAgentsCheck_DirDoesNotExist_InfoNotFailure(t *testing.T) {
	prof := cleanProfile(t)
	c := daemonAgentsCheck(prof, filepath.Join(t.TempDir(), "LaunchAgents"))
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (nothing installed yet is not a failure)", c)
	}
}

func TestDaemonAgentsCheck_PerProjectAgentsListedBySlug(t *testing.T) {
	prof := cleanProfile(t)
	prof.QueueRoot = "/Users/x/another-project/queue"
	dir := t.TempDir()
	for _, name := range []string{
		install.WakerLabelBase + ".another-project.plist",
		install.SweepLabelBase + ".another-project.plist",
		install.WakerLabelBase + ".legacy-migration.plist",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := daemonAgentsCheck(prof, dir)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no bare legacy agent present)", c)
	}
	for _, want := range []string{"another-project", "legacy-migration"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to list slug %q", c.Detail, want)
		}
	}
}

func TestDaemonAgentsCheck_BareLegacyAgentFlaggedAsCollisionRisk(t *testing.T) {
	prof := cleanProfile(t)
	prof.QueueRoot = "/Users/x/another-project/queue"
	dir := t.TempDir()
	for _, name := range []string{
		install.WakerLabelBase + ".plist", // bare — no slug
		install.SweepLabelBase + ".another-project.plist",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := daemonAgentsCheck(prof, dir)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn (a bare legacy agent is a collision risk)", c)
	}
	if !strings.Contains(c.Detail, "COLLISION") {
		t.Errorf("detail = %q, want it to call out the collision risk", c.Detail)
	}
}

// TestWakerInstalledCheck_MissingWakerWarns is the poller-gap detection: a scaffolded
// queue with rostered owners but NO waker plist for its slug → WARN naming the slug and
// the `init --daemons-only` remediation.
func TestWakerInstalledCheck_MissingWakerWarns(t *testing.T) {
	prof := cleanProfile(t)     // queue_root exists
	prof.ProjectID = "acmecorp" // pin the resolved slug so the assertion is deterministic
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}}
	dir := t.TempDir() // launch-agents dir with NO waker plist for "acmecorp"

	c := wakerInstalledCheck(prof, dir)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (queue+roster but no waker installed)", c)
	}
	if !strings.Contains(c.Detail, "acmecorp") {
		t.Errorf("detail = %q, want it to name the project slug", c.Detail)
	}
	if !strings.Contains(c.Detail, "init --daemons-only --confirm") {
		t.Errorf("detail = %q, want the --daemons-only remediation", c.Detail)
	}
}

// TestWakerInstalledCheck_PresentIsOK: the SAME project WITH its waker plist installed →
// OK. Detection is by plist PRESENCE (not a running PID — a KeepAlive daemon may be
// mid-restart).
func TestWakerInstalledCheck_PresentIsOK(t *testing.T) {
	prof := cleanProfile(t)
	prof.ProjectID = "acmecorp"
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}}
	dir := t.TempDir()
	// The slug-namespaced waker label ComputeDaemonPlan would install for project_id "acmecorp".
	if err := os.WriteFile(filepath.Join(dir, install.WakerLabelBase+".acmecorp.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := wakerInstalledCheck(prof, dir)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (waker plist present)", c)
	}
}

// TestWakerInstalledCheck_GateOnlyQueueLessNotFlagged is the gate-only regression: a
// project whose queue_root does NOT exist has no lanes to watch, so no waker is expected —
// it must NOT be flagged (INFO), consistent with the absent-queue degradation.
func TestWakerInstalledCheck_GateOnlyQueueLessNotFlagged(t *testing.T) {
	prof := cleanProfile(t)
	prof.QueueRoot = filepath.Join(t.TempDir(), "does-not-exist") // gate-only: no queue
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}} // even with a roster
	dir := t.TempDir()                                            // empty launch-agents dir

	c := wakerInstalledCheck(prof, dir)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (a gate-only/queue-less project needs no waker)", c)
	}
}

// TestWakerInstalledCheck_NoRosterNotFlagged: a queue with no rostered work-roles has no
// owners to wake — not the gap this check exists for; INFO, not WARN.
func TestWakerInstalledCheck_NoRosterNotFlagged(t *testing.T) {
	prof := cleanProfile(t) // queue exists, but no Roster set
	dir := t.TempDir()      // no waker plist
	c := wakerInstalledCheck(prof, dir)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (no roster work-roles → nobody to wake)", c)
	}
}

// TestWakerInstalledCheck_EmptyDirArg_NotChecked: no launch-agents dir given (non-macOS /
// caller declined) → Info, never a failure.
func TestWakerInstalledCheck_EmptyDirArg_NotChecked(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}}
	c := wakerInstalledCheck(prof, "")
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (caller declined the check)", c)
	}
}

func TestWorkstreamRegistryCheck_EmptyDirArg_NotChecked(t *testing.T) {
	prof := cleanProfile(t)
	c := workstreamRegistryCheck(prof, "")
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (caller declined the check)", c)
	}
}

func TestWorkstreamRegistryCheck_EmptyRegistry_Info(t *testing.T) {
	prof := cleanProfile(t)
	c := workstreamRegistryCheck(prof, t.TempDir())
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no live entries is not a failure)", c)
	}
}

func TestWorkstreamRegistryCheck_ListsBySlugAndFlagsStale(t *testing.T) {
	prof := cleanProfile(t)
	dir := t.TempDir()
	now := time.Now()
	if err := usage.WriteWorkstreamEntry(dir, "legacy-migration", 3, now); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteWorkstreamEntry(dir, "another-project", 2, now.Add(-time.Hour)); err != nil {
		t.Fatal(err) // older than the default 30m TTL -> stale
	}
	c := workstreamRegistryCheck(prof, dir)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want OK=false Severity=warn (a stale entry is present)", c)
	}
	for _, want := range []string{"legacy-migration", "another-project", "STALE"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
}

// --- queue-bypass (queue-mode-enforcement item 3) ---

// writeWorkishSessionTask writes one non-gate session task whose description is long
// enough to read as workish under cleanProfile's default threshold (280 chars).
func writeWorkishSessionTask(t *testing.T, sessionTasksRoot string) {
	t.Helper()
	dir := filepath.Join(sessionTasksRoot, "session-abc123")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	desc := strings.Repeat("this reads like real task content, not gate coordination. ", 6)
	body := fmt.Sprintf(`{"id":"1","subject":"do the thing","status":"in_progress","description":%q}`, desc)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- owner-liveness (team-standup-teach-check-scaffold) ---

// writeLaneTicket drops a ticket into queueRoot/<lane>/<status>/ — a t.TempDir queue
// fixture the sweep-backed owner-liveness check censuses.
func writeLaneTicket(t *testing.T, queueRoot, lane, status, name string) {
	t.Helper()
	dir := filepath.Join(queueRoot, lane, status)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("ticket"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerLivenessCheck_PendingNoConsumerWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "ci-pipeline", Count: 1}, {Type: "devops", Count: 1}}
	// Both roster lanes hold pending tickets; nothing is in-progress anywhere — the
	// exact #123 shape: a full queue with no persistent owners consuming it.
	writeLaneTicket(t, prof.QueueRoot, "ci-pipeline", "pending", "t1.md")
	writeLaneTicket(t, prof.QueueRoot, "devops", "pending", "t2.md")

	c := ownerLivenessCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (pending in queue, no consumer)", c)
	}
	for _, want := range []string{"leadkit team standup", "0 in-progress", "no persistent owners"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
	if !strings.Contains(c.Detail, "ci-pipeline") && !strings.Contains(c.Detail, "devops") {
		t.Errorf("detail = %q, want it to name a starved role", c.Detail)
	}
}

func TestOwnerLivenessCheck_RoleWithInProgressNotFlagged(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "devops", Count: 1}}
	// devops has claimed a ticket (in-progress) — an owner IS consuming; never flag.
	writeLaneTicket(t, prof.QueueRoot, "devops", "in-progress", "t1.md")

	c := ownerLivenessCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (a role with in-progress is consuming)", c)
	}
}

// TestOwnerLivenessCheck_StarvedRoleAmidBusyPeerWarns is the Fix-1 regression: a
// single starved lane (ci-infra: 4 pending, 0 in-progress) must WARN even while a
// PEER lane is busy (ci-pipeline: 0 pending, 2 in-progress). Fails before the fix
// (the global-in-progress early-return masked ci-infra); passes after (per-role).
func TestOwnerLivenessCheck_StarvedRoleAmidBusyPeerWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "ci-infra", Count: 1}, {Type: "ci-pipeline", Count: 1}}
	// ci-infra is STARVED: 4 pending, nobody consuming.
	for _, n := range []string{"t1.md", "t2.md", "t3.md", "t4.md"} {
		writeLaneTicket(t, prof.QueueRoot, "ci-infra", "pending", n)
	}
	// ci-pipeline is BUSY: 2 in-progress. Under the old global mask this hid ci-infra.
	writeLaneTicket(t, prof.QueueRoot, "ci-pipeline", "in-progress", "p1.md")
	writeLaneTicket(t, prof.QueueRoot, "ci-pipeline", "in-progress", "p2.md")

	c := ownerLivenessCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (ci-infra starved while ci-pipeline is busy)", c)
	}
	if !strings.Contains(c.Detail, "ci-infra") {
		t.Errorf("detail = %q, want it to name the starved role ci-infra", c.Detail)
	}
	if !strings.Contains(c.Detail, "leadkit team standup") {
		t.Errorf("detail = %q, want the standup remedy", c.Detail)
	}
}

// TestOwnerLivenessCheck_AllRolesHaveConsumerIsOK is the OK counterpart: when EVERY
// role with pending work also has an in-progress consumer, no role is starved → OK.
func TestOwnerLivenessCheck_AllRolesHaveConsumerIsOK(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "ci-infra", Count: 1}, {Type: "ci-pipeline", Count: 1}}
	// ci-infra: pending AND in-progress (a consumer is pulling). ci-pipeline: busy.
	writeLaneTicket(t, prof.QueueRoot, "ci-infra", "pending", "t1.md")
	writeLaneTicket(t, prof.QueueRoot, "ci-infra", "in-progress", "t2.md")
	writeLaneTicket(t, prof.QueueRoot, "ci-pipeline", "in-progress", "p1.md")

	c := ownerLivenessCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (every pending lane has a consumer)", c)
	}
}

func TestOwnerLivenessCheck_NoRosterIsInfo(t *testing.T) {
	prof := cleanProfile(t) // no roster
	c := ownerLivenessCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no roster)", c)
	}
}

func TestOwnerLivenessCheck_UnreachableQueueIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "ci-pipeline", Count: 1}}
	prof.QueueRoot = filepath.Join(t.TempDir(), "does-not-exist")

	c := ownerLivenessCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (queue unreachable → no false WARN)", c)
	}
}

// TestOwnerLivenessCheck_DistinctFromQueueBypass proves the two checks catch
// DIFFERENT states and neither subsumes the other: work SITTING IN the queue with no
// consumer (owner-liveness) vs work running OFF the queue with an empty queue
// (queue-bypass). Each fixture WARNs its own check and passes the other's.
func TestOwnerLivenessCheck_DistinctFromQueueBypass(t *testing.T) {
	// Fixture 1: pending in queue, no consumer, plus work-ish session activity.
	// owner-liveness WARNs; queue-bypass does NOT (the queue holds tickets).
	t.Run("pending-in-queue-no-consumer", func(t *testing.T) {
		prof := cleanProfile(t)
		prof.Roster = []config.RosterSpec{{Type: "ci-pipeline", Count: 1}}
		writeLaneTicket(t, prof.QueueRoot, "ci-pipeline", "pending", "t1.md")
		writeWorkishSessionTask(t, prof.SessionTasksRoot)

		if c := ownerLivenessCheck(prof); c.OK || c.Severity != SeverityWarn {
			t.Errorf("owner-liveness = %+v, want WARN (pending, no consumer)", c)
		}
		if c := queueBypassCheck(prof); !c.OK || c.Severity != SeverityInfo {
			t.Errorf("queue-bypass = %+v, want OK/Info (the queue holds a ticket → not a bypass)", c)
		}
	})

	// Fixture 2: empty queue + work-ish session activity. queue-bypass WARNs; there
	// is no pending work at all, so owner-liveness has nothing to consume → Info.
	t.Run("work-off-queue-empty", func(t *testing.T) {
		prof := cleanProfile(t)
		prof.Roster = []config.RosterSpec{{Type: "ci-pipeline", Count: 1}}
		writeWorkishSessionTask(t, prof.SessionTasksRoot)

		if c := queueBypassCheck(prof); c.OK || c.Severity != SeverityWarn {
			t.Errorf("queue-bypass = %+v, want WARN (work off an empty queue)", c)
		}
		if c := ownerLivenessCheck(prof); !c.OK || c.Severity != SeverityInfo {
			t.Errorf("owner-liveness = %+v, want OK/Info (no pending work in the queue)", c)
		}
	})
}

func TestQueueBypassCheck_NoRosterIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	c := queueBypassCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no roster declared)", c)
	}
}

func TestQueueBypassCheck_EmptyQueueWithWorkishSessionTaskWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "code-review", Count: 1}}
	writeWorkishSessionTask(t, prof.SessionTasksRoot)
	// cleanProfile's queue lanes exist but have no pending/in-progress/done entries
	// at all — zero tickets in the queue.

	c := queueBypassCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (work-ish session task, empty queue)", c)
	}
	for _, want := range []string{"OFF the queue", "waker daemon", "team scaffold"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
}

func TestQueueBypassCheck_LiveTicketsNeverFalsePositive(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "code-review", Count: 1}}
	writeWorkishSessionTask(t, prof.SessionTasksRoot)

	// Give the queue a real, live ticket — a healthy queue must never be flagged,
	// regardless of gate-record recency.
	pending := filepath.Join(prof.QueueRoot, "cr-1", "pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "some-task.md"), []byte("ticket"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := queueBypassCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info — a queue holding a live ticket is never a bypass", c)
	}
}

func TestQueueBypassCheck_NoWorkishActivityIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "code-review", Count: 1}}
	// No session tasks at all: nothing work-ish, so an empty queue is not a signal.
	c := queueBypassCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no work-ish session-task activity)", c)
	}
}

// --- branch-whitelist (queue-mode-enforcement item 5) ---

// fakeGit is a doctor.Git stub — tests never shell out to a real git repo.
type fakeGit struct {
	branch string
	ok     bool
	remote string
}

func (f fakeGit) CurrentBranch(string) (string, bool) { return f.branch, f.ok }
func (f fakeGit) RemoteURL(string) string             { return f.remote }

func TestBranchWhitelistCheck_MatchingBranchIsClean(t *testing.T) {
	prof := cleanProfile(t)
	prof.PushWhitelist = []string{"feat/*", "fix/*"}
	c := branchWhitelistCheck(prof, fakeGit{branch: "feat/queue-mode-enforcement", ok: true})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

func TestBranchWhitelistCheck_NonMatchingBranchWarns(t *testing.T) {
	prof := cleanProfile(t)
	prof.PushWhitelist = []string{"feat/*", "fix/*"}
	c := branchWhitelistCheck(prof, fakeGit{branch: "dev-access", ok: true})
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn", c)
	}
	if !strings.Contains(c.Detail, "dev-access") || !strings.Contains(c.Detail, "push-guard") {
		t.Errorf("detail = %q, want it to name the branch and push-guard", c.Detail)
	}
}

func TestBranchWhitelistCheck_PerRepoAuthorityOverridesTopLevel(t *testing.T) {
	prof := cleanProfile(t)
	prof.PushWhitelist = []string{"feat/*"}
	prof.Authority = map[string]config.AuthoritySpec{
		"platform-infra": {PushWhitelist: []string{"develop"}},
	}
	c := branchWhitelistCheck(prof, fakeGit{branch: "develop", ok: true, remote: "git@github.com:org/platform-infra.git"})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (per-repo override allows develop)", c)
	}
	if !strings.Contains(c.Detail, "authority.platform-infra") {
		t.Errorf("detail = %q, want it to name the winning authority entry", c.Detail)
	}
}

func TestBranchWhitelistCheck_DetachedHeadIsInfoNotWarn(t *testing.T) {
	prof := cleanProfile(t)
	c := branchWhitelistCheck(prof, fakeGit{branch: "HEAD", ok: true})
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (detached HEAD — nothing to check)", c)
	}
}

func TestBranchWhitelistCheck_NilGitIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	c := branchWhitelistCheck(prof, nil)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no git seam given)", c)
	}
}

// TestBranchWhitelistCheck_AgreesWithRealPushGuard is the shared-matcher regression:
// doctor's check and hook.PushGuard's own matchesWhitelist must reach the SAME
// verdict for the same (branch, patterns) inputs, since doctor calls
// hook.MatchWhitelist directly rather than a second implementation.
func TestBranchWhitelistCheck_AgreesWithRealPushGuard(t *testing.T) {
	patterns := []string{"feat/*", "release/*"}
	cases := []struct {
		branch string
		want   bool
	}{
		{"feat/x", true},
		{"feat/x/y", true}, // "*" crosses "/"
		{"release/1.0", true},
		{"dev-access", false},
		{"main", false},
	}
	for _, c := range cases {
		got := hook.MatchWhitelist(c.branch, patterns)
		if got != c.want {
			t.Errorf("hook.MatchWhitelist(%q, %v) = %v, want %v", c.branch, patterns, got, c.want)
		}
		// branchWhitelistCheck must reach the identical verdict via the same call.
		prof := cleanProfile(t)
		prof.PushWhitelist = patterns
		gotCheck := branchWhitelistCheck(prof, fakeGit{branch: c.branch, ok: true}).OK
		if gotCheck != c.want {
			t.Errorf("branchWhitelistCheck(%q).OK = %v, want %v (disagrees with hook.MatchWhitelist)", c.branch, gotCheck, c.want)
		}
	}
}

// --- authority-key-collision (authority-key-matching-hardening.md) ---

// TestAuthorityKeyCollisionCheck_AmbientProfile: the exact report — a `someinfra` key that
// loosely (mid-word) substring-matches a shared-infra origin must WARN, naming
// the key + origin + remedy.
func TestAuthorityKeyCollisionCheck_AmbientProfile(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{
		// `infra` collides mid-word inside `shared-infra` — the ambient-profile shape.
		"infra": {PushWhitelist: []string{"chore/*"}},
	}
	git := fakeGit{ok: true, remote: "git@github.com:acmecorp/shared-infra.git"}
	c := authorityKeyCollisionCheck(prof, git)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (loose collision)", c)
	}
	for _, want := range []string{"infra", "shared-infra", "LOOSE", "scope the authority key"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", c.Detail, want)
		}
	}
}

func TestAuthorityKeyCollisionCheck_BoundaryAlignedIsOK(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{
		"platform-infra": {PushWhitelist: []string{"develop"}},
	}
	git := fakeGit{ok: true, remote: "git@github.com:acmecorp/platform-infra.git"}
	c := authorityKeyCollisionCheck(prof, git)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (boundary-aligned single match)", c)
	}
}

func TestAuthorityKeyCollisionCheck_MultipleMatchesWarn(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{
		"acmecorp/shared-infra": {PushWhitelist: []string{"feat/*"}},
		"acmecorp":             {PushWhitelist: []string{"chore/*"}},
	}
	git := fakeGit{ok: true, remote: "git@github.com:acmecorp/shared-infra.git"}
	c := authorityKeyCollisionCheck(prof, git)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (ambiguity)", c)
	}
	for _, want := range []string{"multiple authority keys", "longest key wins", "acmecorp/shared-infra"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", c.Detail, want)
		}
	}
}

func TestAuthorityKeyCollisionCheck_NoAuthorityIsOK(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{"acmecorp/widgets": {}}
	git := fakeGit{ok: true, remote: "git@github.com:other/unrelated.git"}
	c := authorityKeyCollisionCheck(prof, git)
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (no matching key)", c)
	}
}

func TestAuthorityKeyCollisionCheck_FailsOpenNoGitOrOrigin(t *testing.T) {
	prof := cleanProfile(t)
	prof.Authority = map[string]config.AuthoritySpec{"someinfra": {PushWhitelist: []string{"chore/*"}}}
	if c := authorityKeyCollisionCheck(prof, nil); !c.OK || c.Severity != SeverityInfo {
		t.Errorf("nil git: check = %+v, want OK=true Severity=info (fail-open)", c)
	}
	if c := authorityKeyCollisionCheck(prof, fakeGit{ok: true, remote: ""}); !c.OK || c.Severity != SeverityInfo {
		t.Errorf("no origin: check = %+v, want OK=true Severity=info (fail-open)", c)
	}
}

// --- session-priority (session-priority-burn.md) ---

func TestSessionPriorityCheck_EmptyDirArg_NotChecked(t *testing.T) {
	c := sessionPriorityCheck(cleanProfile(t), "", burn.PriorityCritical)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

func TestSessionPriorityCheck_EmptyRegistry_Info(t *testing.T) {
	c := sessionPriorityCheck(cleanProfile(t), t.TempDir(), burn.PriorityCritical)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info (no live sessions is not a failure)", c)
	}
}

func TestSessionPriorityCheck_OneCriticalIsFine(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "proj-a", "s1", 1, "critical", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "proj-b", "s1", 1, "low", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := sessionPriorityCheck(cleanProfile(t), dir, burn.PriorityCritical)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info — exactly one critical session is fine", c)
	}
}

func TestSessionPriorityCheck_MultipleCriticalWarns(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "proj-a", "s1", 1, "critical", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "proj-b", "s1", 1, "critical", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := sessionPriorityCheck(cleanProfile(t), dir, burn.PriorityCritical)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn", c)
	}
	for _, want := range []string{"proj-a", "proj-b", "self-declared"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
}

// --- priority ceiling visibility (priority-ceiling.md) ---

func TestSessionPriorityCheck_SurfacesEffectiveCeiling(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "proj-a", "s1", 1, "low", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := sessionPriorityCheck(cleanProfile(t), dir, burn.PriorityHigh)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
	if !strings.Contains(c.Detail, "high") {
		t.Errorf("detail = %q, want it to surface the effective ceiling %q", c.Detail, "high")
	}
}

func TestSessionPriorityCheck_OverCeilingSessionWarns(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "over-proj", "s1", 2, "critical", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := sessionPriorityCheck(cleanProfile(t), dir, burn.PriorityHigh)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn — one session declared above the ceiling", c)
	}
	for _, want := range []string{"over-proj", "clamped", "high"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
}

func TestSessionPriorityCheck_UnsetCeilingNeverWarnsOnDeclaredCriticalAlone(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "proj-a", "s1", 1, "critical", time.Now()); err != nil {
		t.Fatal(err)
	}
	// An unset ceiling (burn.PriorityCritical, the ReadPriorityCeiling
	// fallback) must never flag a single critical declaration as "clamped" —
	// nothing was clamped, byte-identical to session-priority-burn's own
	// single-critical-is-fine behavior.
	c := sessionPriorityCheck(cleanProfile(t), dir, burn.PriorityCritical)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

// --- role-keyed-lanes.md: doctor's queue-lanes check. ---

// TestQueueLanesCheck_AllRoleLanesIsClean is the happy path: every lane dir under
// queue_root resolves to a canonical role (either via the fixed short-form fold —
// cr-1/crit-1, as cleanProfile's own fixture already has — or a declared roster
// role), so the check is clean, Info-severity, no sprawl reported.
func TestQueueLanesCheck_AllRoleLanesIsClean(t *testing.T) {
	prof := cleanProfile(t) // already has queue/cr-1 and queue/crit-1
	c := queueLanesCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info — cr-1/crit-1 fold to code-review/critique", c)
	}
}

// TestQueueLanesCheck_FlagsNonRoleLanes is the warn path: an ephemeral/per-task
// lane with no canonical-role home is flagged, listed by name, and pointed at the
// consolidation tool — while a declared roster role's instance lane (ci-infra-1)
// is NOT flagged, because it resolves to prof.Roster's declared "ci-infra" role.
func TestQueueLanesCheck_FlagsNonRoleLanes(t *testing.T) {
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "ci-infra", Count: 1}}
	for _, d := range []string{"ci-infra-1", "fix-1161", "owner-479a"} {
		if err := os.MkdirAll(filepath.Join(prof.QueueRoot, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := queueLanesCheck(prof)
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn", c)
	}
	for _, want := range []string{"fix-1161", "owner-479a", "queue consolidate"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "ci-infra-1") {
		t.Errorf("detail = %q, must NOT flag ci-infra-1 — it resolves to the declared roster role ci-infra", c.Detail)
	}
}

// TestQueueLanesCheck_UnreachableQueueRootIsInfo mirrors the other checks'
// "can't check, don't fail" convention for an unreachable queue_root — pathChecks
// above already reports the critical failure; this check just has nothing to add.
func TestQueueLanesCheck_UnreachableQueueRootIsInfo(t *testing.T) {
	prof := cleanProfile(t)
	prof.QueueRoot = filepath.Join(t.TempDir(), "does-not-exist")
	c := queueLanesCheck(prof)
	if !c.OK || c.Severity != SeverityInfo {
		t.Errorf("check = %+v, want OK=true Severity=info", c)
	}
}

// TestSessionIdentityCheck covers the fail-open session-identity check: a WARN naming
// both slugs on mismatch, Info/OK on agreement, Info when identity is unresolved.
func TestSessionIdentityCheck(t *testing.T) {
	// Mismatch: env/ancestor shadowing the session's real project → WARN, both named.
	c := SessionIdentityCheck("container-base", "git-repo basename", "legacy-migration")
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("mismatch must be a non-OK WARN, got OK=%v sev=%s", c.OK, c.Severity)
	}
	if !strings.Contains(c.Detail, "container-base") || !strings.Contains(c.Detail, "legacy-migration") {
		t.Errorf("WARN must name BOTH slugs, got %q", c.Detail)
	}
	if c.Severity == SeverityCritical {
		t.Error("session-identity must be fail-open, never critical")
	}

	// Match: identity == policy → OK/Info.
	ok := SessionIdentityCheck("alpha", "own leadkit.yaml project_id", "alpha")
	if !ok.OK || ok.Severity == SeverityCritical {
		t.Errorf("agreement must be OK and non-critical, got %+v", ok)
	}

	// Unresolved identity → OK/Info, never a false WARN.
	empty := SessionIdentityCheck("", "cwd basename", "alpha")
	if !empty.OK {
		t.Errorf("unresolved identity must not fail, got %+v", empty)
	}
}

// A CRITICAL with no way out is a dead end: the two directories that are reported CRITICAL when
// absent must name the command that creates each.
func TestPathChecks_AbsentStoreAndRecordsNameTheCommandThatCreatesThem(t *testing.T) {
	root := t.TempDir()
	prof := config.Defaults()
	prof.StoreRoot = filepath.Join(root, "tasks")
	prof.RecordsDir = filepath.Join(root, "records")
	prof.QueueRoot = filepath.Join(root, "queue")
	var store, records Check
	for _, c := range pathChecks(&prof) {
		switch c.Name {
		case "store_root":
			store = c
		case "records_dir":
			records = c
		}
	}
	if store.OK || store.Severity != SeverityCritical || !strings.Contains(store.Detail, "leadkit team scaffold --apply --confirm") {
		t.Errorf("store_root: %+v", store)
	}
	if records.OK || records.Severity != SeverityCritical || !strings.Contains(records.Detail, "leadkit init --apply --confirm") {
		t.Errorf("records_dir: %+v", records)
	}
	if err := os.MkdirAll(prof.StoreRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range pathChecks(&prof) {
		if c.Name == "store_root" && (!c.OK || strings.Contains(c.Detail, "creates it")) {
			t.Errorf("a present store must read plainly: %+v", c)
		}
	}
}

func TestLinearOutputCheck_DirectModeChecksKeyReachableNeverPrintsIt(t *testing.T) {
	const key = "lin_api_DOCTOR_SECRET"
	prof := cleanProfile(t)
	prof.Outputs = []string{"linear"}
	prof.Linear.TeamID = "CI"
	noKC := func(string, string) (string, error) { return "", errors.New("no") }

	// env key present -> direct mode, reachable, value never printed.
	c := linearOutputCheckFor(prof, func(k string) string {
		if k == "LINEAR_API_KEY" {
			return key
		}
		return ""
	}, noKC)
	if !c.OK || !strings.Contains(c.Detail, "direct") || strings.Contains(c.Detail, key) {
		t.Errorf("check = %+v, want OK direct-mode detail without the key", c)
	}

	// forced direct, key unreachable -> warn.
	prof.Linear.SyncMode = "direct"
	c = linearOutputCheckFor(prof, func(string) string { return "" }, noKC)
	if c.OK || c.Severity != SeverityWarn {
		t.Errorf("check = %+v, want a warning when direct mode has no reachable key", c)
	}

	// forced direct, keychain supplies it -> OK, still never printed.
	prof.KeychainService = "svc"
	c = linearOutputCheckFor(prof, func(string) string { return "" }, func(string, string) (string, error) { return key, nil })
	if !c.OK || strings.Contains(c.Detail, key) {
		t.Errorf("check = %+v", c)
	}

	// mcp mode (no key, no force) -> the original team_id check, unchanged.
	prof.Linear.SyncMode = ""
	prof.Linear.TeamID = ""
	c = linearOutputCheckFor(prof, func(string) string { return "" }, noKC)
	if c.OK || !strings.Contains(c.Detail, "team_id is unset") {
		t.Errorf("mcp mode must keep the original check, got %+v", c)
	}
}

// Under defaults the profile check names the exact external file it would read (keyed by
// the ORIGIN repo-name, not the dir basename) and the init command, and is not a failure.
func TestProfileCheck_DefaultsNamesExternalPathAndInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LEADKIT_PROFILE", "")
	repo := filepath.Join(t.TempDir(), "ci")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/widget-ci.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	d := config.Defaults()
	c := profileCheckAt(&d, "", repo)
	want := filepath.Join(home, ".claude", "leadkit", "config", "widget-ci.yaml")
	if c.Severity == SeverityCritical || !strings.Contains(c.Detail, want) || !strings.Contains(c.Detail, "leadkit config init --external") {
		t.Errorf("want non-critical check naming %s and the init command, got %+v", want, c)
	}
}
