package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/hookwire"
)

// hookMatcherTokens parses settings and returns the UNION of every matcher token that
// any entry invoking the named leadkit hook fires under — the set of tool events the
// hook would actually run on. Used to assert a consolidation never drops coverage.
func hookMatcherTokens(t *testing.T, settings []byte, name string) map[string]bool {
	t.Helper()
	var root struct {
		Hooks map[string][]hookwire.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &root); err != nil {
		t.Fatalf("settings not parseable: %v\n%s", err, settings)
	}
	tokens := map[string]bool{}
	for _, g := range root.Hooks["PreToolUse"] {
		for _, h := range g.Hooks {
			if !hookwire.Invokes(h.Command, name) {
				continue
			}
			for _, tok := range strings.Split(g.Matcher, "|") {
				tokens[strings.TrimSpace(tok)] = true
			}
		}
	}
	return tokens
}

func TestComputePlan_EmptyExisting_AddsEveryHook(t *testing.T) {
	plan, err := ComputePlan(nil, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed {
		t.Fatal("want Changed=true from an empty settings.json")
	}
	if len(plan.Additions) != 11 {
		t.Errorf("got %d additions, want 11 (one per hookwire.ExpectedHooks entry): %v", len(plan.Additions), plan.Additions)
	}
	if len(plan.AlreadyWired) != 0 {
		t.Errorf("want no AlreadyWired from an empty settings.json, got %v", plan.AlreadyWired)
	}

	var root map[string]any
	if err := json.Unmarshal(plan.NewSettings, &root); err != nil {
		t.Fatalf("NewSettings is not valid JSON: %v\n%s", err, plan.NewSettings)
	}
}

func TestComputePlan_AlreadyFullyWired_NoChange(t *testing.T) {
	existing := []byte(`{
  "hooks": {
    "PreToolUse": [
      {"matcher": "SendMessage", "hooks": [
        {"type":"command","command":"leadkit hook brief-length"},
        {"type":"command","command":"leadkit hook milestone-backstop"}
      ]},
      {"matcher": "Bash", "hooks": [
        {"type":"command","command":"leadkit hook pr-guard"},
        {"type":"command","command":"leadkit hook queue-guard"},
        {"type":"command","command":"leadkit hook usage-logger"},
        {"type":"command","command":"leadkit hook push-guard"},
        {"type":"command","command":"leadkit hook push-gate"}
      ]},
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type":"command","command":"leadkit hook task-mechanism"}]},
      {"matcher": "Edit|Write|MultiEdit", "hooks": [{"type":"command","command":"leadkit hook task-status-guard"}]},
      {"matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": [{"type":"command","command":"leadkit hook owner-guard"}]},
      {"matcher": "Bash|Agent|Task", "hooks": [{"type":"command","command":"leadkit hook routing-inbox"}]}
    ]
  }
}`)
	plan, err := ComputePlan(existing, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Changed {
		t.Errorf("want Changed=false when every hook is already wired, got additions=%v", plan.Additions)
	}
	if !bytes.Equal(plan.NewSettings, existing) {
		t.Error("NewSettings should equal the untouched input when nothing changed")
	}
	if len(plan.AlreadyWired) != 11 {
		t.Errorf("want all 11 hooks reported AlreadyWired, got %v", plan.AlreadyWired)
	}
}

func TestComputePlan_PreservesExistingNonLeadkitHooksAndOtherKeys(t *testing.T) {
	existing := []byte(`{
  "permissions": {"allow": ["Bash(git status)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type":"command","command":"bash $HOME/.claude/hooks/some-other-guard.sh"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit", "hooks": [{"type":"command","command":"bash $HOME/.claude/hooks/autocommit.sh"}]}
    ]
  },
  "statusLine": {"type": "command", "command": "echo hi"}
}`)
	plan, err := ComputePlan(existing, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed {
		t.Fatal("want Changed=true — no leadkit hooks were wired yet")
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(plan.NewSettings, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["permissions"]; !ok {
		t.Error("permissions key must be preserved")
	}
	if _, ok := root["statusLine"]; !ok {
		t.Error("statusLine key must be preserved")
	}

	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(root["hooks"], &hooks); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("hooks.PostToolUse must be preserved untouched")
	}
	if !bytes.Contains(plan.NewSettings, []byte("some-other-guard.sh")) {
		t.Error("the pre-existing non-leadkit PreToolUse hook must still be present")
	}
}

func TestComputePlan_LeavesAMisconfiguredExistingEntryAlone(t *testing.T) {
	// push-guard already has an entry, but under the wrong matcher (Edit, not Bash).
	// ComputePlan must NOT add a second push-guard entry, and must NOT rewrite the
	// existing one — that's doctor's job to flag, a human's job to fix.
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"leadkit hook push-guard"}]}]}}`)
	plan, err := ComputePlan(existing, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Additions {
		if bytes.Contains([]byte(a), []byte("push-guard")) {
			t.Errorf("must not add a duplicate push-guard entry when one already exists (even misconfigured): %v", plan.Additions)
		}
	}
	found := false
	for _, n := range plan.AlreadyWired {
		if n == "push-guard" {
			found = true
		}
	}
	if !found {
		t.Errorf("push-guard should be reported AlreadyWired (left alone), got %v", plan.AlreadyWired)
	}
}

func TestComputePlan_MalformedJSON_Refuses(t *testing.T) {
	_, err := ComputePlan([]byte(`{"hooks": not valid`), "leadkit")
	if err == nil {
		t.Fatal("want an error on malformed existing settings.json")
	}
}

func TestComputePlan_RedundantBashHooksReported(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"bash $HOME/.claude/hooks/pr-repo-guard.sh"}]}
	]}}`)
	plan, err := ComputePlan(existing, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range plan.Redundant {
		if bytes.Contains([]byte(r), []byte("pr-repo-guard.sh")) {
			found = true
		}
	}
	if !found {
		t.Errorf("want a redundancy note for pr-repo-guard.sh now that pr-guard is wired, got %v", plan.Redundant)
	}
	// The bash entry itself must still be present — never removed.
	if !bytes.Contains(plan.NewSettings, []byte("pr-repo-guard.sh")) {
		t.Error("the redundant bash hook must never be removed")
	}
}

func TestComputePlan_TaskMechanismAndPushGuard_NoRedundancyNoted(t *testing.T) {
	plan, err := ComputePlan(nil, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Redundant {
		if bytes.Contains([]byte(r), []byte("push-guard")) || bytes.Contains([]byte(r), []byte("task-mechanism")) {
			t.Errorf("push-guard/task-mechanism have no bash predecessor, must never appear as redundant: %v", plan.Redundant)
		}
	}
}

func TestApply_DryRunNeverCalled_LeavesFileByteForByteUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// The dry-run path: compute the plan, print it, and simply never call Apply.
	if _, err := ComputePlan(original, "leadkit"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Error("computing a plan must never touch the file on disk")
	}
}

func TestApply_OnEmptySettings_WritesAllHooksAndNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json") // does not exist yet
	plan, err := ComputePlan(nil, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Apply(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	if backup != "" {
		t.Errorf("want no backup when no prior settings.json existed, got %q", backup)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("settings.json was not written: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("written settings.json is not valid JSON: %v", err)
	}
}

func TestApply_OnExistingSettings_WritesTimestampedBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := ComputePlan(original, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Apply(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("want a backup path when a prior settings.json existed")
	}
	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("backup file not found: %v", err)
	}
	if !bytes.Equal(backupData, original) {
		t.Error("backup content must equal the pre-write file exactly")
	}
}

func TestApply_IsIdempotent_SecondRunIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	plan1, err := ComputePlan(nil, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(path, plan1); err != nil {
		t.Fatal(err)
	}
	firstWrite, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	plan2, err := ComputePlan(firstWrite, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Changed {
		t.Errorf("second ComputePlan over the just-written file should find nothing to add, got %v", plan2.Additions)
	}
	backup2, err := Apply(path, plan2)
	if err != nil {
		t.Fatal(err)
	}
	if backup2 != "" {
		t.Errorf("Apply on an unchanged plan must be a pure no-op (no backup, no write), got backup=%q", backup2)
	}
	secondRead, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstWrite, secondRead) {
		t.Error("the file must be byte-for-byte identical after the idempotent second run")
	}
}

// --- init-hook-idempotency (docs/build-tasks/init-hook-idempotency.md) ---

func TestComputePlan_WiresAbsoluteBinaryPath_NotBare(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	plan, err := ComputePlan(nil, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed {
		t.Fatal("want Changed=true from an empty settings.json")
	}
	for _, a := range plan.Additions {
		if !bytes.Contains([]byte(a), []byte(absBinary+" hook ")) {
			t.Errorf("addition %q does not wire the absolute binary path %q", a, absBinary)
		}
		if bytes.Contains([]byte(a), []byte(" leadkit hook ")) {
			t.Errorf("addition %q wires a bare `leadkit hook` instead of the absolute path", a)
		}
	}
	if !bytes.Contains(plan.NewSettings, []byte(absBinary+" hook push-guard")) {
		t.Errorf("NewSettings must contain the absolute-path push-guard command, got:\n%s", plan.NewSettings)
	}
}

func TestComputePlan_ReRunOnAbsoluteAlreadyWired_IsNoOp(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	plan1, err := ComputePlan(nil, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := ComputePlan(plan1.NewSettings, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Changed {
		t.Errorf("re-running init on already-absolute-wired settings.json must be a no-op, got additions=%v repairs=%v", plan2.Additions, plan2.Repairs)
	}
	if len(plan2.AlreadyWired) != 11 {
		t.Errorf("want all 11 hooks reported AlreadyWired on the re-run, got %v", plan2.AlreadyWired)
	}
	if !bytes.Equal(plan2.NewSettings, plan1.NewSettings) {
		t.Error("NewSettings must be byte-identical to the input when nothing changed")
	}
}

func TestComputePlan_BareLeadkitHooks_CanonicalizedAndRepairedToAbsolute(t *testing.T) {
	// A prior/other-session `init --apply` wired every hook bare (`leadkit hook
	// <name>`) — the exact shape project-i's/#123's sessions each left behind.
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher": "SendMessage", "hooks": [
			{"type":"command","command":"leadkit hook brief-length"},
			{"type":"command","command":"leadkit hook milestone-backstop"}
		]},
		{"matcher": "Bash", "hooks": [
			{"type":"command","command":"leadkit hook pr-guard"},
			{"type":"command","command":"leadkit hook queue-guard"},
			{"type":"command","command":"leadkit hook usage-logger"},
			{"type":"command","command":"leadkit hook push-guard"},
			{"type":"command","command":"leadkit hook push-gate"}
		]},
		{"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type":"command","command":"leadkit hook task-mechanism"}]},
		{"matcher": "Edit|Write|MultiEdit", "hooks": [{"type":"command","command":"leadkit hook task-status-guard"}]},
		{"matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": [{"type":"command","command":"leadkit hook owner-guard"}]},
		{"matcher": "Bash|Agent|Task", "hooks": [{"type":"command","command":"leadkit hook routing-inbox"}]}
	]}}`)
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed {
		t.Fatal("want Changed=true — the bare hooks need repairing to the absolute form")
	}
	if len(plan.Additions) != 0 {
		t.Errorf("must NOT add a duplicate absolute entry alongside a bare one — bare and absolute are the SAME hook (canonicalized); got additions=%v", plan.Additions)
	}
	if len(plan.Repairs) != 11 {
		t.Errorf("want all 11 bare hooks repaired, got %v", plan.Repairs)
	}
	if bytes.Contains(plan.NewSettings, []byte(`"leadkit hook`)) {
		t.Errorf("bare leadkit hook commands must be removed after repair, got:\n%s", plan.NewSettings)
	}
	for _, name := range []string{"brief-length", "pr-guard", "queue-guard", "usage-logger", "push-guard", "push-gate", "task-mechanism", "task-status-guard", "owner-guard", "milestone-backstop", "routing-inbox"} {
		want := absBinary + " hook " + name
		if bytes.Count(plan.NewSettings, []byte(want)) != 1 {
			t.Errorf("want exactly one %q entry after repair, got:\n%s", want, plan.NewSettings)
		}
	}
}

func TestComputePlan_BareAndAbsoluteDuplicates_ConsolidatedToOne(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	// The live multi-session incident this fix is for: project-i's `init --apply` wired
	// the bare set, then #123's wired the (fixed) absolute set again on top.
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher": "Bash", "hooks": [{"type":"command","command":"leadkit hook push-guard"}]},
		{"matcher": "Bash", "hooks": [{"type":"command","command":"` + absBinary + ` hook push-guard"}]}
	]}}`)
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range plan.Repairs {
		if bytes.Contains([]byte(r), []byte("push-guard")) {
			found = true
		}
	}
	if !found {
		t.Errorf("want push-guard reported as repaired (consolidated), got %v", plan.Repairs)
	}
	want := absBinary + " hook push-guard"
	if bytes.Count(plan.NewSettings, []byte(want)) != 1 {
		t.Errorf("want exactly one consolidated %q entry, got:\n%s", want, plan.NewSettings)
	}
	if bytes.Contains(plan.NewSettings, []byte(`"leadkit hook push-guard"`)) {
		t.Error("the bare duplicate must be removed by consolidation")
	}
}

func TestComputePlan_Repair_NeverTouchesNonLeadkitHooksSharingAGroup(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	// A bare leadkit hook shares a matcher GROUP with a non-leadkit bash guard — the
	// repair must strip only the leadkit entry, leaving the bash guard untouched.
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher": "Bash", "hooks": [
			{"type":"command","command":"bash $HOME/.claude/hooks/queue-mv-guard.sh"},
			{"type":"command","command":"leadkit hook queue-guard"}
		]}
	]}}`)
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plan.NewSettings, []byte("queue-mv-guard.sh")) {
		t.Error("a non-leadkit bash guard sharing a matcher group with a repaired hook must never be removed")
	}
	want := absBinary + " hook queue-guard"
	if bytes.Count(plan.NewSettings, []byte(want)) != 1 {
		t.Errorf("want exactly one consolidated %q entry, got:\n%s", want, plan.NewSettings)
	}
}

func TestApply_AtomicWrite_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	plan, err := ComputePlan(nil, "leadkit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(path, plan); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Errorf("unexpected leftover file %q — Apply must clean up its temp file", e.Name())
		}
	}
}

// --- Fix 1: matcher-aware consolidation (docs/build-tasks/init-consolidation-matcher-aware.md) ---

// TestComputePlan_UsageLoggerUnderTwoMatchers_KeepsBoth is THE regression this task
// exists for: usage-logger is legitimately wired under BOTH Bash (tool-command
// telemetry) and Agent|Task (agent-spawn telemetry). The old consolidation collapsed
// all entries invoking a hook into ONE entry with the single canonical matcher,
// silently dropping the Agent|Task spawn telemetry. After ComputePlan, usage-logger
// must STILL fire on Bash AND Agent AND Task. (Fails before Fix 1, passes after.)
func TestComputePlan_UsageLoggerUnderTwoMatchers_KeepsBoth(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher": "Bash", "hooks": [{"type":"command","command":"` + absBinary + ` hook usage-logger"}]},
		{"matcher": "Agent|Task", "hooks": [{"type":"command","command":"` + absBinary + ` hook usage-logger"}]}
	]}}`)
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	tokens := hookMatcherTokens(t, plan.NewSettings, "usage-logger")
	for _, want := range []string{"Bash", "Agent", "Task"} {
		if !tokens[want] {
			t.Errorf("after ComputePlan, usage-logger no longer fires on %q — consolidation dropped a matcher (coverage now %v)", want, tokens)
		}
	}
	// And it must not be left duplicated: exactly one canonical command entry survives.
	if got := bytes.Count(plan.NewSettings, []byte(absBinary+" hook usage-logger")); got != 1 {
		t.Errorf("want exactly one consolidated usage-logger entry, got %d:\n%s", got, plan.NewSettings)
	}
}

// TestComputePlan_UsageLoggerFreshWire_CoversBashAndSpawn asserts Fix 1 part (a): a
// FRESH wire (usage-logger not present at all) is added under its full intended matcher
// set — Bash|Agent|Task — not just Bash, so spawn telemetry works from a clean install.
func TestComputePlan_UsageLoggerFreshWire_CoversBashAndSpawn(t *testing.T) {
	plan, err := ComputePlan(nil, "/Users/matt/.claude/leadkit-bin")
	if err != nil {
		t.Fatal(err)
	}
	tokens := hookMatcherTokens(t, plan.NewSettings, "usage-logger")
	for _, want := range []string{"Bash", "Agent", "Task"} {
		if !tokens[want] {
			t.Errorf("a freshly-added usage-logger must fire on %q, coverage=%v", want, tokens)
		}
	}
}

// TestComputePlan_ConsolidationPreservesUnrecognizedMatcher asserts Fix 1 part (b): a
// repair must never REDUCE the tool events a hook fires on, even for a matcher token
// init doesn't itself intend — here usage-logger is wired under an extra "Foo" event
// alongside its normal ones; the consolidated entry must still carry Foo.
func TestComputePlan_ConsolidationPreservesUnrecognizedMatcher(t *testing.T) {
	const absBinary = "/Users/matt/.claude/leadkit-bin"
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher": "Bash", "hooks": [{"type":"command","command":"` + absBinary + ` hook usage-logger"}]},
		{"matcher": "Agent|Task|Foo", "hooks": [{"type":"command","command":"` + absBinary + ` hook usage-logger"}]}
	]}}`)
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	tokens := hookMatcherTokens(t, plan.NewSettings, "usage-logger")
	for _, want := range []string{"Bash", "Agent", "Task", "Foo"} {
		if !tokens[want] {
			t.Errorf("consolidation dropped the %q matcher token — a repair must never shrink coverage (got %v)", want, tokens)
		}
	}
}

// --- Fix 2: symlink-equivalent binary path is already-canonical (no churn) ---

// TestComputePlan_SymlinkEquivalentBinary_AlreadyWiredNoChurn wires every hook at a
// real binary file, then runs ComputePlan with a canonical binary that is a SYMLINK to
// that same file. Every entry differs from canonical only in the binary token but
// resolves to the identical real file, so nothing should be rewritten — the plan is a
// pure no-op (Changed=false, every hook AlreadyWired, settings byte-for-byte unchanged).
func TestComputePlan_SymlinkEquivalentBinary_AlreadyWiredNoChurn(t *testing.T) {
	realDir := t.TempDir()
	realBin := filepath.Join(realDir, "leadkit-bin")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	canonical := filepath.Join(linkDir, "leadkit")
	if err := os.Symlink(realBin, canonical); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	var b strings.Builder
	b.WriteString(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[`)
	for i, spec := range hookwire.ExpectedHooks {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"command","command":%q}`, realBin+" hook "+spec.Name)
	}
	b.WriteString(`]}]}}`)
	existing := []byte(b.String())

	plan, err := ComputePlan(existing, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Changed {
		t.Errorf("a symlink-equivalent binary must produce NO churn, got Changed=true (repairs=%v additions=%v)", plan.Repairs, plan.Additions)
	}
	if len(plan.AlreadyWired) != len(hookwire.ExpectedHooks) {
		t.Errorf("want all %d hooks AlreadyWired (symlink-equivalent), got %v", len(hookwire.ExpectedHooks), plan.AlreadyWired)
	}
	if len(plan.Repairs) != 0 {
		t.Errorf("a symlink-equivalent binary must never be repaired, got %v", plan.Repairs)
	}
	if !bytes.Equal(plan.NewSettings, existing) {
		t.Errorf("settings must be byte-for-byte unchanged (no rewrite to the symlink path), got:\n%s", plan.NewSettings)
	}
	// The original real-binary command must survive verbatim — never rewritten to the link.
	if !bytes.Contains(plan.NewSettings, []byte(realBin+" hook push-guard")) {
		t.Error("the real-binary command must be left untouched, not rewritten to the canonical symlink path")
	}
}

// TestComputePlan_GenuinelyDifferentRealPath_StillRepaired guards against Fix 2
// over-suppressing: an entry whose binary resolves to a DIFFERENT real file than
// canonical is a genuinely stale path and must still be repaired to canonical.
func TestComputePlan_GenuinelyDifferentRealPath_StillRepaired(t *testing.T) {
	staleDir := t.TempDir()
	staleBin := filepath.Join(staleDir, "leadkit-old")
	if err := os.WriteFile(staleBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonDir := t.TempDir()
	canonical := filepath.Join(canonDir, "leadkit")
	if err := os.WriteFile(canonical, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"` + staleBin + ` hook push-guard"}]}
	]}}`)
	plan, err := ComputePlan(existing, canonical)
	if err != nil {
		t.Fatal(err)
	}
	repaired := false
	for _, r := range plan.Repairs {
		if strings.Contains(r, "push-guard") {
			repaired = true
		}
	}
	if !repaired {
		t.Errorf("a genuinely different real binary path must still be repaired, got repairs=%v", plan.Repairs)
	}
	if bytes.Contains(plan.NewSettings, []byte(staleBin+" hook push-guard")) {
		t.Error("the stale-path command must be removed by the repair")
	}
	if bytes.Count(plan.NewSettings, []byte(canonical+" hook push-guard")) != 1 {
		t.Errorf("want exactly one canonical push-guard entry after repair, got:\n%s", plan.NewSettings)
	}
}

// TestComputePlan_EvalSymlinksError_DegradesToRepair asserts Fix 2's degrade-safe
// posture: when EvalSymlinks errors, the entry is treated exactly as today (repaired),
// never mistakenly suppressed.
func TestComputePlan_EvalSymlinksError_DegradesToRepair(t *testing.T) {
	orig := evalSymlinks
	evalSymlinks = func(string) (string, error) { return "", fmt.Errorf("boom") }
	t.Cleanup(func() { evalSymlinks = orig })

	const absBinary = "/Users/matt/.claude/leadkit-bin"
	existing := []byte(`{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"/some/other/leadkit-bin hook push-guard"}]}
	]}}`)
	plan, err := ComputePlan(existing, absBinary)
	if err != nil {
		t.Fatal(err)
	}
	repaired := false
	for _, r := range plan.Repairs {
		if strings.Contains(r, "push-guard") {
			repaired = true
		}
	}
	if !repaired {
		t.Errorf("an EvalSymlinks error must fall back to repair, got repairs=%v", plan.Repairs)
	}
}
