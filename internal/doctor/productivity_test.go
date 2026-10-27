package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/config"
)

// Productivity watcher (docs/build-tasks/productivity-watcher.md).
//
// These tests drive the pure productivityCheck with a FAKE CLOCK and injected
// activity/deliverable sources (ProductivityProbe), so the busy≠productive logic is
// exercised without touching fs/git. The fs-backed source helpers (session-lens
// reuse, gate records, queue advancement, done-tasks) are tested separately below.

func fakeSig(when time.Time, who, what string) *ProductivitySignal {
	return &ProductivitySignal{When: when, Who: who, What: what}
}

// fakeProbe builds a ProductivityProbe from optional activity/deliverable signals; a
// nil signal means "no signal of that kind readable" (ok=false).
func fakeProbe(act, deliv *ProductivitySignal) ProductivityProbe {
	return ProductivityProbe{
		Activity: func() (ProductivitySignal, bool) {
			if act == nil {
				return ProductivitySignal{}, false
			}
			return *act, true
		},
		Deliverable: func() (ProductivitySignal, bool) {
			if deliv == nil {
				return ProductivitySignal{}, false
			}
			return *deliv, true
		},
	}
}

// TestProductivityCheck_AliveChurningNoDeliverableWarns is the core signal: an agent
// that is active (recent churn) but whose last deliverable predates the window →
// Warn, naming the agent + the gap + the off-queue-invisibility cross-link.
func TestProductivityCheck_AliveChurningNoDeliverableWarns(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 3 * time.Hour
	act := fakeSig(now.Add(-10*time.Minute), "session s1", "4 transient subagent output file(s), 2 session task(s)")
	deliv := fakeSig(now.Add(-5*time.Hour), "", "new commit")

	c := productivityCheck(now, window, fakeProbe(act, deliv))
	if c.OK || c.Severity != SeverityWarn {
		t.Fatalf("check = %+v, want OK=false Severity=warn (active + no deliverable past the window)", c)
	}
	for _, want := range []string{"session s1", "NO deliverable", "queue-bypass", "Busy is not productive"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", c.Detail, want)
		}
	}
}

// TestProductivityCheck_RecentDeliverableIsOK: an agent that is active AND produced a
// deliverable within the window is productive → OK/Info, never a Warn.
func TestProductivityCheck_RecentDeliverableIsOK(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 3 * time.Hour
	act := fakeSig(now.Add(-10*time.Minute), "session s1", "churn")
	deliv := fakeSig(now.Add(-30*time.Minute), "", "queue advancement")

	c := productivityCheck(now, window, fakeProbe(act, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (productive — recent deliverable)", c)
	}
	if !strings.Contains(c.Detail, "productive") {
		t.Errorf("detail = %q, want it to read as productive", c.Detail)
	}
}

// TestProductivityCheck_NoActivitySignalIsInfoNeverWarn: with NO readable activity
// signal the check genuinely can't tell → Info, and must NEVER false-Warn.
func TestProductivityCheck_NoActivitySignalIsInfoNeverWarn(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// No activity, plus a stale deliverable — the tempting-but-wrong false-Warn shape.
	deliv := fakeSig(now.Add(-10*time.Hour), "", "new commit")
	c := productivityCheck(now, 3*time.Hour, fakeProbe(nil, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (no activity signal → can't tell)", c)
	}
	if !strings.Contains(c.Detail, "can't tell") {
		t.Errorf("detail = %q, want it to say it can't tell", c.Detail)
	}
}

// TestProductivityCheck_StaleActivityIsInfo: if the last activity itself predates the
// window, no agent is currently active — nothing to warn about → Info.
func TestProductivityCheck_StaleActivityIsInfo(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	act := fakeSig(now.Add(-6*time.Hour), "session s1", "churn")
	c := productivityCheck(now, 3*time.Hour, fakeProbe(act, nil))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK=true Severity=info (activity older than the window → not currently active)", c)
	}
}

// TestProductivityCheck_WindowIsConfigurable: the SAME activity/deliverable pair reads
// OK under a generous window but WARNs under a tight one — the window drives the call.
func TestProductivityCheck_WindowIsConfigurable(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	act := fakeSig(now.Add(-5*time.Minute), "session s1", "churn")
	deliv := fakeSig(now.Add(-90*time.Minute), "", "new commit")

	if c := productivityCheck(now, 3*time.Hour, fakeProbe(act, deliv)); !c.OK {
		t.Errorf("with a 3h window, a 90m-old deliverable is productive; got %+v", c)
	}
	if c := productivityCheck(now, time.Hour, fakeProbe(act, deliv)); c.OK || c.Severity != SeverityWarn {
		t.Errorf("with a 1h window, a 90m-old deliverable is a stall; got %+v", c)
	}
}

// TestProductivityCheck_OffQueueWithCommitsIsProductive: deliverables are NOT only
// queue advancement — an off-queue agent (no queue signal) whose only deliverable is a
// commit still reads productive.
func TestProductivityCheck_OffQueueWithCommitsIsProductive(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	act := fakeSig(now.Add(-2*time.Minute), "session s1", "churn")
	deliv := fakeSig(now.Add(-20*time.Minute), "", "new commit")
	c := productivityCheck(now, 3*time.Hour, fakeProbe(act, deliv))
	if !c.OK || c.Severity != SeverityInfo {
		t.Fatalf("check = %+v, want OK/Info (a commit is a deliverable even off-queue)", c)
	}
	if !strings.Contains(c.Detail, "new commit") {
		t.Errorf("detail = %q, want it to name the commit deliverable", c.Detail)
	}
}

// TestProductivityStallLine_SweepSignal: the sweep-daemon helper emits a report line
// for a stalled project and NOTHING for a productive one.
func TestProductivityStallLine_SweepSignal(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	window := 3 * time.Hour

	// Stalled: active but the last deliverable predates the window.
	stalledProbe := fakeProbe(fakeSig(now.Add(-5*time.Minute), "session s1", "churn"), fakeSig(now.Add(-5*time.Hour), "", "new commit"))
	line, stalled := ProductivityStallLine(now, window, stalledProbe)
	if !stalled {
		t.Fatalf("stalled project: want stalled=true, got a productive reading")
	}
	if !strings.Contains(line, "PRODUCTIVE-STALL") || !strings.Contains(line, "session s1") {
		t.Errorf("stall line = %q, want a PRODUCTIVE-STALL line naming the agent", line)
	}

	// Productive: a recent deliverable → no line at all.
	okProbe := fakeProbe(fakeSig(now.Add(-5*time.Minute), "session s1", "churn"), fakeSig(now.Add(-10*time.Minute), "", "queue advancement"))
	if line, stalled := ProductivityStallLine(now, window, okProbe); stalled || line != "" {
		t.Errorf("productive project: want no stall line, got (%q, %v)", line, stalled)
	}
}

// TestProductivityCheck_DistinctFromOwnerLivenessAndQueueBypass proves the watcher is
// a COMPLEMENTARY signal, not a dup: it fires on active+no-deliverable (a state where
// neither neighbour fires), and the neighbours stay quiet there.
func TestProductivityCheck_DistinctFromOwnerLivenessAndQueueBypass(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	prof := cleanProfile(t)
	prof.Roster = []config.RosterSpec{{Type: "code-review", Count: 1}}

	// A queue that is BOTH full and consumed (a ticket in-progress) — owner-liveness OK
	// (a consumer is pulling), queue-bypass OK (the queue holds tickets). Yet the agent
	// is active while its last deliverable predates the window → productivity WARNs.
	writeLaneTicket(t, prof.QueueRoot, "code-review", "in-progress", "t1.md")
	if c := ownerLivenessCheck(prof); !c.OK {
		t.Fatalf("owner-liveness should be OK here (a consumer is in-progress); got %+v", c)
	}
	if c := queueBypassCheck(prof); !c.OK {
		t.Fatalf("queue-bypass should be OK here (the queue holds a ticket); got %+v", c)
	}
	stall := fakeProbe(fakeSig(now.Add(-2*time.Minute), "session s1", "churn"), fakeSig(now.Add(-6*time.Hour), "", "new commit"))
	if c := productivityCheck(now, 3*time.Hour, stall); c.OK || c.Severity != SeverityWarn {
		t.Fatalf("productivity should WARN on active+no-deliverable where the neighbours are quiet; got %+v", c)
	}
}

// --- fs-backed source helpers (reuse of the session lens + gate-records + queue plumbing) ---

// touchFile writes path (creating parents) and stamps its mtime.
func touchFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestSessionActivitySignal_ReadsChurnAndLabelsSession: the ACTIVITY source sees
// transient subagent .output files + session-task writes, timestamps them, and labels
// the most-recently-touched session as the active agent.
func TestSessionActivitySignal_ReadsChurnAndLabelsSession(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	touchFile(t, filepath.Join(root, "session-abc", "child-1.output"), now.Add(-30*time.Minute))
	touchFile(t, filepath.Join(root, "session-abc", "child-2.output"), now.Add(-3*time.Minute))
	touchFile(t, filepath.Join(root, "session-abc", "1.json"), now.Add(-40*time.Minute))

	sig, ok := sessionActivitySignal(root)
	if !ok {
		t.Fatal("want an activity signal from a session dir with churn")
	}
	if !sig.When.Equal(now.Add(-3 * time.Minute)) {
		t.Errorf("activity When = %v, want the newest file mtime %v", sig.When, now.Add(-3*time.Minute))
	}
	if !strings.Contains(sig.Who, "abc") {
		t.Errorf("activity Who = %q, want it to name session abc", sig.Who)
	}
}

// TestSessionActivitySignal_EmptyRootIsNoSignal: an absent/empty tasks root yields no
// activity signal (which the check honours as "can't tell").
func TestSessionActivitySignal_EmptyRootIsNoSignal(t *testing.T) {
	if _, ok := sessionActivitySignal(filepath.Join(t.TempDir(), "nope")); ok {
		t.Error("absent tasks root should yield no activity signal")
	}
	if _, ok := sessionActivitySignal(t.TempDir()); ok {
		t.Error("empty tasks root should yield no activity signal")
	}
}

// TestDeliverableHelpers_NewestAcrossKinds: the fs deliverable sources (gate records,
// queue advancement, done session-tasks) each surface their newest timestamp.
func TestDeliverableHelpers_NewestAcrossKinds(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	// Gate records: newest .json mtime wins.
	records := t.TempDir()
	touchFile(t, filepath.Join(records, "aaa.json"), now.Add(-2*time.Hour))
	touchFile(t, filepath.Join(records, "bbb.json"), now.Add(-30*time.Minute))
	if ts, name, ok := newestJSONMtime(records); !ok || !ts.Equal(now.Add(-30*time.Minute)) || name != "bbb.json" {
		t.Errorf("newestJSONMtime = (%v,%q,%v), want the bbb.json mtime", ts, name, ok)
	}

	// Queue advancement: only in-progress/done count, pending is excluded.
	queueRoot := t.TempDir()
	touchFile(t, filepath.Join(queueRoot, "cr", "pending", "p.md"), now.Add(-1*time.Minute)) // ignored
	touchFile(t, filepath.Join(queueRoot, "cr", "in-progress", "a.md"), now.Add(-45*time.Minute))
	touchFile(t, filepath.Join(queueRoot, "cr", "done", "b.md"), now.Add(-20*time.Minute))
	if ts, ok := newestQueueAdvance(queueRoot); !ok || !ts.Equal(now.Add(-20*time.Minute)) {
		t.Errorf("newestQueueAdvance = (%v,%v), want the done/b.md mtime (pending excluded)", ts, ok)
	}
	// A queue with only pending tickets has NOT advanced.
	pendOnly := t.TempDir()
	touchFile(t, filepath.Join(pendOnly, "cr", "pending", "p.md"), now)
	if _, ok := newestQueueAdvance(pendOnly); ok {
		t.Error("a pending-only queue must not read as advanced")
	}

	// Done session-tasks: a completed-status task's file mtime is a deliverable.
	tasks := t.TempDir()
	dir := filepath.Join(tasks, "session-xyz")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(`{"id":"1","status":"in_progress"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	donePath := filepath.Join(dir, "2.json")
	if err := os.WriteFile(donePath, []byte(`{"id":"2","status":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(donePath, now.Add(-15*time.Minute), now.Add(-15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ts, ok := newestDoneSessionTask(tasks); !ok || !ts.Equal(now.Add(-15*time.Minute)) {
		t.Errorf("newestDoneSessionTask = (%v,%v), want the completed task's mtime", ts, ok)
	}
}

// TestProductivityStallWindowDur_Configurable pins the config parse + fallback the
// watcher reads its window through.
func TestProductivityStallWindowDur_Configurable(t *testing.T) {
	if got := (config.Burn{ProductivityStallWindow: "45m"}).ProductivityStallWindowDur(); got != 45*time.Minute {
		t.Errorf("parsed window = %v, want 45m", got)
	}
	if got := (config.Burn{}).ProductivityStallWindowDur(); got != burn.DefaultProductivityStallWindow {
		t.Errorf("empty window = %v, want the default %v", got, burn.DefaultProductivityStallWindow)
	}
	if got := (config.Burn{ProductivityStallWindow: "nonsense"}).ProductivityStallWindowDur(); got != burn.DefaultProductivityStallWindow {
		t.Errorf("unparseable window = %v, want the default %v", got, burn.DefaultProductivityStallWindow)
	}
}
