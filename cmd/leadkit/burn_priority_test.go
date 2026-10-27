package main

// session-priority-burn.md: runPreemptionCheck is directly testable against a
// t.TempDir() registry, no live usage fetch needed — the same "split out for
// direct testing" pattern applyThrottle already uses.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/usage"
)

var burnPriorityNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// TestRunPreemptionCheck_AllNormalIsByteIdenticalNeverShedsNeverLogs is the
// #123 safety regression: an all-normal (or single-session) registry, even
// wildly over the afforded target, must be a total no-op — no shed, and
// critically, NO LOG FILE WRITTEN AT ALL (proving nothing new happened).
func TestRunPreemptionCheck_AllNormalIsByteIdenticalNeverShedsNeverLogs(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	if err := usage.WriteSessionEntry(dir, "proj-a", "sess1", 4, "normal", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "proj-b", "sess1", 4, "normal", burnPriorityNow); err != nil {
		t.Fatal(err)
	}

	got := runPreemptionCheck(dir, logPath, "proj-a/sess1", "proj-a", "sess1", "normal", burn.PriorityCritical, 2, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if got.Shed {
		t.Errorf("all-normal registry must never shed, got %+v", got)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("all-normal registry must never even create the preemption log, err=%v", err)
	}
}

func TestRunPreemptionCheck_SingleSessionIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	if err := usage.WriteSessionEntry(dir, "solo", "sess1", 5, "low", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	got := runPreemptionCheck(dir, logPath, "solo/sess1", "solo", "sess1", "low", burn.PriorityCritical, 1, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if got.Shed {
		t.Errorf("a single live session must never shed, got %+v", got)
	}
}

func TestRunPreemptionCheck_EmptyRegistryIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	got := runPreemptionCheck(dir, logPath, "solo/sess1", "solo", "sess1", "normal", burn.PriorityCritical, 1, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if got.Shed {
		t.Errorf("an empty/absent registry must never shed, got %+v", got)
	}
}

// TestRunPreemptionCheck_LowSheddedAndLogged is the preemptive-shed
// integration test: a low-priority session sharing the account with a
// critical one, over budget, gets shed to zero AND the shed is logged with
// an actionable YIELD reason — never silent.
func TestRunPreemptionCheck_LowSheddedAndLogged(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	if err := usage.WriteSessionEntry(dir, "low-proj", "sess1", 3, "low", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "crit-proj", "sess1", 4, "critical", burnPriorityNow); err != nil {
		t.Fatal(err)
	}

	got := runPreemptionCheck(dir, logPath, "low-proj/sess1", "low-proj", "sess1", "low", burn.PriorityCritical, 4, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if !got.Shed || got.N != 0 {
		t.Fatalf("low = %+v, want Shed=true N=0", got)
	}
	if !strings.Contains(got.Reason, "YIELD") {
		t.Errorf("Reason = %q, want it to say YIELD", got.Reason)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("preemption log not written: %v", err)
	}
	if !strings.Contains(string(data), "low-proj") || !strings.Contains(string(data), "YIELD") {
		t.Errorf("log content = %s, want the slug + an actionable reason recorded", data)
	}

	// The critical session, checked separately, must be untouched AND must
	// never itself be logged (nothing was shed for it).
	logSizeBefore, _ := os.Stat(logPath)
	critGot := runPreemptionCheck(dir, logPath, "crit-proj/sess1", "crit-proj", "sess1", "critical", burn.PriorityCritical, 4, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if critGot.Shed {
		t.Errorf("critical session was shed, want untouched: %+v", critGot)
	}
	logSizeAfter, _ := os.Stat(logPath)
	if logSizeBefore.Size() != logSizeAfter.Size() {
		t.Error("checking the untouched critical session must not append another log line")
	}
}

func TestRunPreemptionCheck_UnreadableRegistryFailsOpen(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root — permission bits don't block reads")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	got := runPreemptionCheck(locked, filepath.Join(t.TempDir(), "preemption.jsonl"), "x/y", "x", "y", "critical", burn.PriorityCritical, 1, burnPriorityNow, burnPriorityNow)
	if got.Shed {
		t.Errorf("an unreadable registry must fail OPEN (never shed), got %+v", got)
	}
}

// --- account-level priority ceiling (priority-ceiling.md) ---

// TestRunPreemptionCheck_UnsetCeilingByteIdenticalToNoClamp is the safety
// test: an absent ceiling file (resolved via usage.ReadPriorityCeiling the
// same way runBurn does) must produce the EXACT shed decision session-
// priority-burn's own tests already established with no ceiling concept at
// all — proving the ceiling mechanism changes nothing when unset.
func TestRunPreemptionCheck_UnsetCeilingByteIdenticalToNoClamp(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	if err := usage.WriteSessionEntry(dir, "low-proj", "sess1", 3, "low", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "crit-proj", "sess1", 4, "critical", burnPriorityNow); err != nil {
		t.Fatal(err)
	}

	ceiling := usage.ReadPriorityCeiling(filepath.Join(t.TempDir(), "does-not-exist"))
	if ceiling != burn.PriorityCritical {
		t.Fatalf("setup check: resolved ceiling = %q, want %q (no clamp)", ceiling, burn.PriorityCritical)
	}

	got := runPreemptionCheck(dir, logPath, "low-proj/sess1", "low-proj", "sess1", "low", ceiling, 4, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if !got.Shed || got.N != 0 {
		t.Fatalf("got = %+v, want Shed=true N=0 — the SAME result as pre-ceiling session-priority-burn; an absent ceiling must never change a preemption outcome", got)
	}
	if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), `"declared"`) {
		t.Errorf("no clamp should ever be logged for an absent (no-clamp) ceiling, got log: %s", data)
	}
}

// TestRunPreemptionCheck_SessionCannotExceedCeiling is the "can't-exceed-
// ceiling" test: it FIRST proves the self-promotion hole exists without a
// ceiling (a session that fakes `critical` stays fully protected), THEN
// proves an account-scoped ceiling closes it — the same session, same
// registry, now shares in the shedding it previously escaped, and the
// clamp is logged.
func TestRunPreemptionCheck_SessionCannotExceedCeiling(t *testing.T) {
	dir := t.TempDir()
	if err := usage.WriteSessionEntry(dir, "a-low-proj", "sess1", 1, "low", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	// "b-over-proj" sorts alphabetically BEFORE z-normal-proj, which is what
	// makes the clamped case land on it specifically within their tied tier.
	if err := usage.WriteSessionEntry(dir, "b-over-proj", "sess1", 2, "critical", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(dir, "z-normal-proj", "sess1", 3, "normal", burnPriorityNow); err != nil {
		t.Fatal(err)
	}
	// total=6, target=4, toShed=2: low absorbs 1, leaving 1 to spend inside
	// whichever tier b-over-proj ends up in.

	unclamped := runPreemptionCheck(dir, filepath.Join(t.TempDir(), "unclamped.jsonl"),
		"b-over-proj/sess1", "b-over-proj", "sess1", "critical", burn.PriorityCritical, 4, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if unclamped.Shed {
		t.Fatalf("setup check failed: without a ceiling, the self-promoted critical session should stay fully protected/untouched (low+normal alone absorb toShed=2), got %+v", unclamped)
	}

	logPath := filepath.Join(t.TempDir(), "preemption.jsonl")
	got := runPreemptionCheck(dir, logPath, "b-over-proj/sess1", "b-over-proj", "sess1", "critical", burn.PriorityNormal, 4, burnPriorityNow, burnPriorityNow.Add(-30*time.Minute))
	if !got.Shed || got.N != 1 {
		t.Fatalf("got = %+v, want Shed=true N=1 — clamped to normal (tied with, and sorting before, z-normal-proj), a %q ceiling closes the self-promotion hole the setup check just proved exists without it", got, burn.PriorityNormal)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("clamp not logged: %v", err)
	}
	for _, want := range []string{`"declared":"critical"`, `"ceiling":"normal"`, `"effective":"normal"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log content = %s, want it to contain %s", data, want)
		}
	}
}

func TestClampPriority_CaseAndWhitespaceTricksCannotEscapeTheCeiling(t *testing.T) {
	for _, declared := range []string{"CRITICAL", " critical ", "CrItIcAl", "critical\n"} {
		if got := burn.ClampPriority(burn.PriorityTier(declared), burn.PriorityLow); got != burn.PriorityLow {
			t.Errorf("ClampPriority(%q, low) = %q, want low — no case/whitespace trick escapes the ceiling", declared, got)
		}
	}
}

// --- burn priorities (session-priority-burn.md) ---

func TestBuildPriorityListing_SortedByShedOrderAndFlagsMultipleCritical(t *testing.T) {
	entries := []usage.SessionEntry{
		{Slug: "b", SessionID: "s1", N: 2, Priority: "critical", UpdatedAt: burnPriorityNow},
		{Slug: "a", SessionID: "s1", N: 1, Priority: "low", UpdatedAt: burnPriorityNow},
		{Slug: "c", SessionID: "s1", N: 3, Priority: "critical", UpdatedAt: burnPriorityNow},
	}
	listing := buildPriorityListing(entries, burn.PriorityCritical, burnPriorityNow)
	if listing.AccountN != 6 {
		t.Errorf("AccountN = %d, want 6", listing.AccountN)
	}
	if len(listing.Rows) != 3 || listing.Rows[0].Slug != "a" {
		t.Fatalf("Rows = %+v, want the low-priority session first (shed order)", listing.Rows)
	}
	if !listing.MultipleCritical {
		t.Error("want MultipleCritical=true — two sessions claim critical")
	}
}

func TestBuildPriorityListing_EmptyIsCleanNotMultipleCritical(t *testing.T) {
	listing := buildPriorityListing(nil, burn.PriorityCritical, burnPriorityNow)
	if listing.AccountN != 0 || len(listing.Rows) != 0 || listing.MultipleCritical {
		t.Errorf("empty listing = %+v, want a clean zero value", listing)
	}
}

func TestFormatPriorityListing_WarnsOnMultipleCritical(t *testing.T) {
	listing := buildPriorityListing([]usage.SessionEntry{
		{Slug: "a", SessionID: "s1", N: 1, Priority: "critical", UpdatedAt: burnPriorityNow},
		{Slug: "b", SessionID: "s1", N: 1, Priority: "critical", UpdatedAt: burnPriorityNow},
	}, burn.PriorityCritical, burnPriorityNow)
	out := formatPriorityListing(listing)
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "critical") {
		t.Errorf("output = %q, want a WARNING about multiple critical sessions", out)
	}
}

// TestBuildPriorityListing_ShowsDeclaredVsEffectiveAndFlagsClamped is the
// "burn priorities shows declared vs effective" test from priority-
// ceiling.md's Visibility section.
func TestBuildPriorityListing_ShowsDeclaredVsEffectiveAndFlagsClamped(t *testing.T) {
	entries := []usage.SessionEntry{
		{Slug: "over-proj", SessionID: "s1", N: 2, Priority: "critical", UpdatedAt: burnPriorityNow},
		{Slug: "honest-proj", SessionID: "s1", N: 1, Priority: "low", UpdatedAt: burnPriorityNow},
	}
	listing := buildPriorityListing(entries, burn.PriorityHigh, burnPriorityNow)
	if listing.Ceiling != "high" {
		t.Errorf("Ceiling = %q, want high", listing.Ceiling)
	}
	if !listing.AnyClamped {
		t.Error("want AnyClamped=true — over-proj declared above the ceiling")
	}

	var overRow, honestRow priorityRow
	for _, r := range listing.Rows {
		switch r.Slug {
		case "over-proj":
			overRow = r
		case "honest-proj":
			honestRow = r
		}
	}
	if overRow.DeclaredPriority != "critical" || overRow.Priority != "high" || !overRow.Clamped {
		t.Errorf("over-proj row = %+v, want declared=critical effective=high clamped=true", overRow)
	}
	if honestRow.DeclaredPriority != "low" || honestRow.Priority != "low" || honestRow.Clamped {
		t.Errorf("honest-proj row = %+v, want declared=effective=low clamped=false", honestRow)
	}

	out := formatPriorityListing(listing)
	if !strings.Contains(out, "NOTE") {
		t.Errorf("output = %q, want a NOTE about the clamped session", out)
	}
}

// TestCLI_BurnPriorities_JSONListsSortedAndReadOnly is the CLI-level proof:
// `leadkit burn priorities --json` lists live sessions sorted by shed order,
// prunes stale entries, and never writes anything.
func TestCLI_BurnPriorities_JSONListsSortedAndReadOnly(t *testing.T) {
	bin := testBinary(t)
	workstreamsDir := t.TempDir()
	// This is a CLI-level test: the real binary computes its own staleness
	// cutoff from time.Now() (no fake-clock injection across a subprocess
	// boundary), so entries MUST be timestamped against the real clock too —
	// burnPriorityNow (a fixed 2026-09-11 literal, used everywhere else in
	// this file against an explicitly-passed `now`/`cutoff`) drifts stale
	// relative to burn.DefaultWorkstreamTTL (30m) the moment real wall-clock
	// time moves past it, which is exactly what broke this test after it was
	// merged (main went red on a `go test` run made hours after the entries
	// were seeded — see the bounce that led to this fix).
	realNow := time.Now().UTC()
	if err := usage.WriteSessionEntry(workstreamsDir, "low-proj", "s1", 2, "low", realNow); err != nil {
		t.Fatal(err)
	}
	if err := usage.WriteSessionEntry(workstreamsDir, "crit-proj", "s1", 3, "critical", realNow); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, workstreamsDir)

	out, code := runCLI(t, bin, nil, "burn", "priorities", "--json", "--workstreams-dir", workstreamsDir)
	if code != 0 {
		t.Fatalf("`burn priorities --json` should exit 0, got %d. output:\n%s", code, out)
	}

	var listing priorityListing
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &listing); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if listing.AccountN != 5 || len(listing.Rows) != 2 {
		t.Fatalf("listing = %+v, want account_n=5, 2 rows", listing)
	}
	if listing.Rows[0].Slug != "low-proj" {
		t.Errorf("Rows[0] = %+v, want low-proj first (lowest priority sheds first)", listing.Rows[0])
	}

	after := snapshotFiles(t, workstreamsDir)
	if len(before) != len(after) {
		t.Errorf("`burn priorities` must never write to the registry it reads: before=%v after=%v", before, after)
	}
}

// TestCLI_Burn_LEADKIT_SESSION_PRIORITYEnvOverridesProfile proves the env
// override precedence: `leadkit burn` writes the session registry entry
// BEFORE the live usage fetch (which is stubbed to fail here, since only the
// registry write matters for this test), so $LEADKIT_SESSION_PRIORITY's
// effect is directly observable even though the tick itself errors out on
// the fetch.
func TestCLI_Burn_LEADKIT_SESSION_PRIORITYEnvOverridesProfile(t *testing.T) {
	bin := testBinary(t)
	workstreamsDir := t.TempDir()
	profileRoot := t.TempDir()
	profilePath := filepath.Join(profileRoot, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("burn:\n  session_priority: low\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	queueRoot := filepath.Join(t.TempDir(), "my-project", "queue")
	env := []string{
		"LEADKIT_PROFILE=" + profilePath,
		"LEADKIT_SESSION_ID=envtest",
		"LEADKIT_SESSION_PRIORITY=critical",
		"LEADKIT_QUEUE=" + queueRoot, // pin the slug deterministically (no baked-in default path)
	}
	fakeScript := filepath.Join(t.TempDir(), "does-not-exist.sh")

	_, _ = runCLI(t, bin, env, "burn", "--workstreams-dir", workstreamsDir, "--native=false", "--script", fakeScript)

	// install.ResolveProjectSlug derives "my-project" from the pinned queue_root's
	// project-root basename (this profile sets no queue_root of its own).
	data, err := os.ReadFile(filepath.Join(workstreamsDir, "my-project", "envtest.json"))
	if err != nil {
		t.Fatalf("session entry not written: %v", err)
	}
	if !strings.Contains(string(data), `"priority":"critical"`) {
		t.Errorf("session entry = %s, want priority=critical (env override wins over the profile's \"low\")", data)
	}
}
