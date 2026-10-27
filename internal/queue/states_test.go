package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A ticket moved to a directory nothing watches is stranded: Wait checks pending and in-progress
// only, so no owner is woken for it. These tests are the failing cases from the incident run
// against the unfixed code: Move accepted `active/`, and Sweep reported nothing.

func laneWithTicket(t *testing.T) (root, ticket string) {
	t.Helper()
	root = t.TempDir()
	ticket = filepath.Join(root, "developer", "pending", "77-x.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), ticket, "77-x.md")
	for _, s := range []string{"in-progress", "done"} {
		if err := os.MkdirAll(filepath.Join(root, "developer", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, ticket
}

func TestMove_RefusesANonCanonicalStateAndTouchesNothing(t *testing.T) {
	root, ticket := laneWithTicket(t)
	for _, state := range []string{"active", "in_progress", "wip", "in-progres"} {
		to := filepath.Join(root, "developer", state) + "/"
		err := Move(root, ticket, to, false)
		if err == nil {
			t.Fatalf("a move to %q must be refused", state)
		}
		for _, want := range []string{state, "pending, in-progress, done"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal for %q must name %q, got: %v", state, want, err)
			}
		}
		if _, serr := os.Lstat(filepath.Join(root, "developer", state)); !os.IsNotExist(serr) {
			t.Errorf("the refused move still created %s/ (stat err = %v)", state, serr)
		}
	}
	if _, err := os.Lstat(ticket); err != nil {
		t.Errorf("the source must be untouched after a refusal: %v", err)
	}
}

func TestMove_RefusesAWrongDepth(t *testing.T) {
	root, ticket := laneWithTicket(t)
	for _, to := range []string{
		filepath.Join(root, "developer", "77-x.md"),                      // no state
		filepath.Join(root, "developer", "pending", "deeper", "77-x.md"), // too deep
		filepath.Join(root, "77-x.md"),                                   // no lane
	} {
		if err := Move(root, ticket, to, false); err == nil {
			t.Errorf("a move to %s must be refused", to)
		}
	}
}

func TestMove_StillAcceptsEveryCanonicalState(t *testing.T) {
	root, ticket := laneWithTicket(t)
	for _, state := range []string{"in-progress", "done", "pending"} {
		to := filepath.Join(root, "developer", state) + "/"
		if err := Move(root, ticket, to, false); err != nil {
			t.Fatalf("a move to %s must still work: %v", state, err)
		}
		ticket = filepath.Join(root, "developer", state, "77-x.md")
	}
	// A move into a lane that does not exist yet still creates it, as before.
	if err := Move(root, ticket, filepath.Join(root, "code-review", "pending")+"/", false); err != nil {
		t.Fatalf("a move into a new lane must still work: %v", err)
	}
}

// Moving a ticket OUT of a stray directory into a valid state is the sanctioned cleanup.
func TestMove_CanRescueATicketFromAStrayState(t *testing.T) {
	root, _ := laneWithTicket(t)
	stray := filepath.Join(root, "developer", "active", "78-y.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), stray, "78-y.md")
	if err := Move(root, stray, filepath.Join(root, "developer", "pending")+"/", false); err != nil {
		t.Fatalf("rescuing a stranded ticket must work: %v", err)
	}
}

func TestSweep_ReportsATicketStrandedInANonCanonicalState(t *testing.T) {
	root, _ := laneWithTicket(t)
	stray := filepath.Join(root, "developer", "active", "77-x.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), stray, "77-x.md")

	res, err := Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stray != 1 || len(res.StrayStates) != 1 {
		t.Fatalf("Stray = %d, StrayStates = %+v; the sweep must name the stranded ticket (it said clean)", res.Stray, res.StrayStates)
	}
	got := res.StrayStates[0]
	if got.Lane != "developer" || got.Dir != "active" || len(got.Tickets) != 1 || got.Tickets[0] != "77-x.md" {
		t.Errorf("StrayState = %+v", got)
	}
	if !strings.Contains(strings.Join(res.Lines, "\n"), "STRAY   developer/active") {
		t.Errorf("the human report lacks the STRAY line:\n%s", strings.Join(res.Lines, "\n"))
	}

	st := BuildSweepStatus(res, "", time.Now())
	if st.Clean() {
		t.Error("a sweep with a stranded ticket must not be Clean")
	}
	if rem := st.Remediation("report"); !strings.Contains(rem, "developer/active/77-x.md") || !strings.Contains(rem, "queue move") {
		t.Errorf("the remediation must name the ticket and the fix, got: %q", rem)
	}
	// queue move resolves against the cwd, so the printed fix must carry <queue_root>/ on both paths.
	if rem := st.Remediation("report"); !strings.Contains(rem, "queue move <queue_root>/<lane>/<dir>/<t> <queue_root>/<lane>/pending/") {
		t.Errorf("the queue move fix must name <queue_root>/ on both paths, got: %q", rem)
	}
	if !NotifyThresholdCrossed(st, 1) {
		t.Error("a stranded ticket must cross the notify threshold")
	}
}

// A lane whose live ticket is IN PROGRESS (so it is not stalled) and which has no stray state dir:
// the only fixture on which a stray-related finding cannot be hidden by some other one.
func healthyLane(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), filepath.Join(root, "developer", "in-progress", "77-x.md"), "77-x.md")
	for _, s := range []string{"pending", "done"} {
		if err := os.MkdirAll(filepath.Join(root, "developer", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSweep_DoesNotFlagWhatIsNotStranded(t *testing.T) {
	root := healthyLane(t)
	// An empty stray dir strands nothing. (The sanctioned cleanup, `queue unlink` of the stray
	// link, leaves exactly this behind, so flagging it would print a permanent finding.)
	if err := os.MkdirAll(filepath.Join(root, "developer", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A separator-variant alias of the real in-progress dir, which HOLDS the ticket: it resolves to
	// tickets the lane already counts, so flagging it would count that ticket twice.
	if err := os.Symlink("in-progress", filepath.Join(root, "developer", "in_progress")); err != nil {
		t.Fatal(err)
	}
	// Dot entries are not state dirs.
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), filepath.Join(root, "developer", ".tmp", "z.md"), "z.md")

	res, err := Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stray != 0 || len(res.StrayStates) != 0 {
		t.Errorf("Stray = %d, StrayStates = %+v; want none (empty dir, alias and dot dir are not stranded work)", res.Stray, res.StrayStates)
	}
	for _, l := range res.Lines {
		if strings.Contains(l, "STRAY") {
			t.Errorf("the human report flags something that is not stranded: %q", l)
		}
	}
	if !BuildSweepStatus(res, "", time.Now()).Clean() {
		t.Error("a lane with nothing wrong must be Clean")
	}
}

// With the live ticket in progress the lane is not stalled, so the stray ticket is the ONLY
// finding and every surface must still react to it. On a fixture that is also stalled each of
// these passes whether or not the stray term exists.
func TestSweep_AStrandedTicketAloneDrivesEverySurface(t *testing.T) {
	root := healthyLane(t)
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), filepath.Join(root, "developer", "active", "78-y.md"), "78-y.md")

	res, err := Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stalled != 0 || res.Broken != 0 {
		t.Fatalf("fixture invariant: the stray must be the only finding, got stalled=%d broken=%d", res.Stalled, res.Broken)
	}
	if res.Stray != 1 {
		t.Fatalf("Stray = %d, want 1", res.Stray)
	}
	st := BuildSweepStatus(res, "", time.Now())
	if st.Clean() {
		t.Error("Clean() must be false when a ticket is stranded")
	}
	if !NotifyThresholdCrossed(st, 1) {
		t.Error("a stranded ticket alone must cross a threshold of 1")
	}
	if NotifyThresholdCrossed(st, 2) {
		t.Error("one stranded ticket must not cross a threshold of 2")
	}
	if args := substituteNotifyArgs("notify {total} {stalled} {broken}", st); strings.Join(args, " ") != "notify 1 0 0" {
		t.Errorf("{total} must count the stranded ticket, got %v", args)
	}
	if msg := NotifyMessage(st); !strings.Contains(msg, "stranded") {
		t.Errorf("the notification must name it: %q", msg)
	}
	if st.Remediation("report") == "" {
		t.Error("Remediation must not be empty for a stranded ticket")
	}
}

func TestUnlink_RemovesOneDuplicateLinkAndNothingElse(t *testing.T) {
	root, pending := laneWithTicket(t)
	stray := filepath.Join(root, "developer", "active", "77-x.md")
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../tasks/77-x.md", stray); err != nil {
		t.Fatal(err)
	}

	dry, err := Unlink(root, stray, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Removed {
		t.Errorf("default must be a dry run: %+v", dry)
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Fatal("the dry run removed the link")
	}
	if len(dry.Remaining) != 1 || !strings.HasSuffix(dry.Remaining[0], "developer/pending/77-x.md") {
		t.Errorf("Remaining = %v, want the pending link", dry.Remaining)
	}

	res, err := Unlink(root, stray, false, true)
	if err != nil || !res.Removed {
		t.Fatalf("apply: %+v, %v", res, err)
	}
	if _, err := os.Lstat(stray); !os.IsNotExist(err) {
		t.Error("the stray link is still there")
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Error("the legitimate link was removed")
	}
	if _, err := os.Stat(filepath.Join(store, "77-x.md")); err != nil {
		t.Error("the ticket file itself was removed")
	}
}

func TestUnlink_Refusals(t *testing.T) {
	root, pending := laneWithTicket(t)

	if _, err := Unlink(root, pending, false, true); err == nil || !strings.Contains(err.Error(), "ONLY link") {
		t.Errorf("a ticket's only link must be refused: %v", err)
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Error("the refused unlink removed the only link anyway")
	}
	if res, err := Unlink(root, pending, true, true); err != nil || !res.Removed {
		t.Errorf("-allow-last must permit it: %+v, %v", res, err)
	}

	real := filepath.Join(root, "developer", "done", "plain.md")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unlink(root, real, true, true); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Errorf("a plain file must never be removed: %v", err)
	}
	if _, err := os.Stat(real); err != nil {
		t.Error("the plain file was removed")
	}

	outside := filepath.Join(t.TempDir(), "a", "b", "c.md")
	setupSymlinkEntry(t, t.TempDir(), outside, "c.md")
	if _, err := Unlink(root, outside, true, true); err == nil {
		t.Error("a link outside the queue root must be refused")
	}
	if _, err := Unlink(root, filepath.Join(root, "developer", "pending", "nope.md"), true, true); err == nil {
		t.Error("a missing entry must be refused")
	}
	if _, err := Unlink(root, filepath.Join(root, "developer"), true, true); err == nil {
		t.Error("a lane directory must be refused")
	}
}

func TestUnlink_ReapsADanglingLink(t *testing.T) {
	root, _ := laneWithTicket(t)
	dangling := filepath.Join(root, "developer", "active", "gone.md")
	if err := os.MkdirAll(filepath.Dir(dangling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../tasks/gone.md", dangling); err != nil {
		t.Fatal(err)
	}
	if res, err := Unlink(root, dangling, false, true); err != nil || !res.Removed {
		t.Errorf("a dangling link has no ticket to orphan: %+v, %v", res, err)
	}
}

// A symlink that is a SIBLING of the queue root and shaped <d>/<t> has the root-relative path
// ../d/t: three parts, so only the in-root check stops it.
func TestUnlink_RefusesALinkOutsideTheRootEvenWhenShapedLikeOne(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "queue")
	if err := os.MkdirAll(filepath.Join(root, "developer", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "tasks")
	outside := filepath.Join(base, "elsewhere", "1-x.md") // base/elsewhere/1-x.md: sibling of queue/, not under it
	setupSymlinkEntry(t, store, outside, "1-x.md")
	// a second link, so the only-link refusal cannot be what stops it
	setupSymlinkEntry(t, store, filepath.Join(root, "developer", "pending", "1-x.md"), "1-x.md")

	_, err := Unlink(root, outside, true, true)
	if err == nil || !strings.Contains(err.Error(), "outside the queue root") {
		t.Fatalf("a link outside the root must be refused as such, got %v", err)
	}
	if _, serr := os.Lstat(outside); serr != nil {
		t.Error("the outside link was removed")
	}
}

func TestUnlink_RefusesAnEntryThatIsNotExactlyLaneDirTicket(t *testing.T) {
	root, _ := laneWithTicket(t)
	deep := filepath.Join(root, "developer", "pending", "nested", "deeper.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), deep, "deeper.md")
	if _, err := Unlink(root, deep, true, true); err == nil || !strings.Contains(err.Error(), "<lane>/<dir>/<ticket>") {
		t.Errorf("a four-part entry must be refused for its shape: %v", err)
	}
	if _, err := os.Lstat(deep); err != nil {
		t.Error("the deep link was removed")
	}
	shallow := filepath.Join(root, "toplevel.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), shallow, "toplevel.md")
	if _, err := Unlink(root, shallow, true, true); err == nil {
		t.Error("a two-part entry must be refused for its shape")
	}
}
