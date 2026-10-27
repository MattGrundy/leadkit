package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// putTicket writes tasks/<name> with the given status (and blocked_on when non-empty) and links it
// into <root>/<lane>/<state>/.
func putTicket(t *testing.T, root, lane, state, name, status, blockedOn string) string {
	t.Helper()
	fm := "---\ntitle: t\nstatus: " + status + "\n"
	if blockedOn != "" {
		fm += "blocked_on: " + blockedOn + "\n"
	}
	fm += "---\n\nbody\n"
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, name), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, lane, state, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../tasks/"+name, link); err != nil {
		t.Fatal(err)
	}
	for _, s := range bucketDirs {
		if err := os.MkdirAll(filepath.Join(root, lane, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return link
}

func TestMove_RefusesToClaimABlockedTicketAndSaysWhyAndWhatToDo(t *testing.T) {
	root := t.TempDir()
	link := putTicket(t, root, "developer", "pending", "87-x.md", "blocked", `"owner decision; and PR #13 merging"`)
	err := Move(root, link, filepath.Join(root, "developer", "in-progress")+"/", false)
	if err == nil {
		t.Fatal("claiming a blocked ticket must be refused")
	}
	for _, want := range []string{"status: blocked", "blocked_on: owner decision; and PR #13 merging", "not claimable", "queue unlink"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if _, serr := os.Lstat(link); serr != nil {
		t.Error("the refusal moved the ticket anyway")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "developer", "in-progress")); len(entries) != 0 {
		t.Errorf("in-progress holds %v after a refused claim", entries)
	}
}

func TestMove_BlockedIsRecognisedInItsWrittenVariants(t *testing.T) {
	for _, status := range []string{"Blocked", "BLOCKED", "blocked   ", `"blocked"`} {
		root := t.TempDir()
		link := putTicket(t, root, "developer", "pending", "1-x.md", status, "")
		err := Move(root, link, filepath.Join(root, "developer", "in-progress")+"/", false)
		if err == nil {
			t.Errorf("status %q must be treated as blocked", status)
			continue
		}
		if !strings.Contains(err.Error(), "no blocked_on recorded") {
			t.Errorf("status %q with no blocked_on must say so: %v", status, err)
		}
	}
}

func TestMove_OnlyTheClaimIsRefusedAndOnlyForBlocked(t *testing.T) {
	// a blocked ticket can still be moved anywhere that is not a claim
	root := t.TempDir()
	link := putTicket(t, root, "developer", "pending", "1-x.md", "blocked", "x")
	if err := Move(root, link, filepath.Join(root, "developer", "done")+"/", false); err != nil {
		t.Errorf("moving a blocked ticket to done is not a claim and must work: %v", err)
	}

	// every non-blocked status can be claimed
	for _, status := range []string{"pending", "in_progress", "needs_rework", "gated", "backlog", ""} {
		root := t.TempDir()
		link := putTicket(t, root, "developer", "pending", "1-x.md", status, "")
		if err := Move(root, link, filepath.Join(root, "developer", "in-progress")+"/", false); err != nil {
			t.Errorf("status %q must be claimable: %v", status, err)
		}
	}

	// changing the status is the deliberate act that releases it
	root = t.TempDir()
	link = putTicket(t, root, "developer", "pending", "1-x.md", "blocked", "x")
	if err := Move(root, link, filepath.Join(root, "developer", "in-progress")+"/", false); err == nil {
		t.Fatal("precondition: blocked refuses")
	}
	ticket := filepath.Join(root, "tasks", "1-x.md")
	if err := os.WriteFile(ticket, []byte("---\ntitle: t\nstatus: pending\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Move(root, link, filepath.Join(root, "developer", "in-progress")+"/", false); err != nil {
		t.Errorf("once its status is changed the ticket must be claimable: %v", err)
	}
}

func TestMove_ADanglingEntryIsNotMistakenForBlocked(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "developer", "pending")
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../tasks/gone.md", filepath.Join(lane, "gone.md")); err != nil {
		t.Fatal(err)
	}
	err := Move(root, filepath.Join(lane, "gone.md"), filepath.Join(root, "developer", "in-progress")+"/", false)
	if err != nil && strings.Contains(err.Error(), "blocked") {
		t.Errorf("a dangling link has no status to be blocked: %v", err)
	}
}

func TestWait_ALaneHoldingOnlyBlockedTicketsHasNoWork(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "developer")
	putTicket(t, root, "developer", "pending", "87-x.md", "blocked", "owner")
	res, err := Wait(lane, 60*time.Millisecond, 10*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "TIMEOUT" {
		t.Errorf("Kind = %s (%q), want TIMEOUT: an owner woken for a ticket it may not claim has nothing to do", res.Kind, res.Msg)
	}

	putTicket(t, root, "developer", "pending", "88-y.md", "pending", "")
	res, err = Wait(lane, time.Second, 10*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "WORK-PENDING" || res.Msg != "88-y.md" {
		t.Errorf("got %s %q, want WORK-PENDING naming only the claimable ticket", res.Kind, res.Msg)
	}
}

func TestSweep_BlockedTicketsAreNotStalledWork(t *testing.T) {
	root := t.TempDir()
	putTicket(t, root, "developer", "pending", "87-x.md", "blocked", "owner")
	res, err := Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stalled != 0 {
		t.Errorf("a lane holding only a blocked ticket is idle, not stalled: %+v", res.StalledLanes)
	}
	// ...but it is REPORTED, by id and blocked_on: excluding it from the stalled count must not make
	// it invisible, or a forgotten one (or one whose blocker has cleared) is never surfaced again.
	if len(res.Blocked) != 1 || res.Blocked[0] != (BlockedTicket{Lane: "developer", Ticket: "87-x.md", BlockedOn: "owner"}) {
		t.Errorf("Blocked = %+v, want the one ticket with its blocked_on", res.Blocked)
	}
	if !strings.Contains(strings.Join(res.Lines, "\n"), "BLOCKED developer — 87-x.md (blocked_on: owner)") {
		t.Errorf("the human report must name it:\n%s", strings.Join(res.Lines, "\n"))
	}
	st := BuildSweepStatus(res, "", time.Now())
	if len(st.Blocked) != 1 {
		t.Errorf("the findings artifact must carry it too: %+v", st.Blocked)
	}
	if !st.Clean() {
		t.Error("a blocked ticket is expected, not broken: it must not make the sweep unclean (nor fail -strict)")
	}
	if !strings.Contains(FormatStatusSurface("p", st, true, "report", 0, time.Now()), "BLOCKED developer") {
		t.Error("`leadkit status` must show it")
	}
	for _, l := range res.Lanes {
		if l.Lane == "developer" && l.Pending != 0 {
			t.Errorf("the lane census must not count a blocked ticket as pending work: %+v", res.Lanes)
		}
	}

	putTicket(t, root, "developer", "pending", "88-y.md", "pending", "")
	res, err = Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stalled != 1 || len(res.StalledLanes[0].Entries) != 1 || res.StalledLanes[0].Entries[0] != "88-y.md" {
		t.Errorf("the stalled entry list must name only the claimable ticket: %+v", res.StalledLanes)
	}
}

// `queue rm` decides whether a ticket is terminal from the same status reader the blocked check
// uses. Making that reader tolerant of quotes for the blocked check must not make a quoted
// `"done"` terminal for rm: an unrecognised value is guarded, the conservative default, and a
// destructive verb must not get more permissive as a side effect of a claim check.
func TestRemoveTask_AQuotedTerminalStatusIsStillGuarded(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	storeFile := filepath.Join(store, "q.md")
	if err := os.WriteFile(storeFile, []byte("---\ntitle: t\nstatus: \"done\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RemoveTask(root, store, "q.md", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Terminal || !res.Guarded || res.RemovedStore {
		t.Fatalf("a quoted status is not recognised as terminal, so rm --apply must be guarded: terminal=%v guarded=%v removed=%v", res.Terminal, res.Guarded, res.RemovedStore)
	}
	if _, err := os.Stat(storeFile); err != nil {
		t.Error("the ticket was deleted")
	}
	if got := readStoreStatus(storeFile); got != `"done"` {
		t.Errorf("readStoreStatus must return the value exactly as written, got %q", got)
	}
}

func TestSweepPoke_TouchesOnlyClaimableEntries(t *testing.T) {
	root := t.TempDir()
	blocked := putTicket(t, root, "developer", "pending", "87-x.md", "blocked", "owner")
	live := putTicket(t, root, "developer", "pending", "88-y.md", "pending", "")
	old := time.Now().Add(-48 * time.Hour)
	for _, l := range []string{blocked, live} {
		if err := lchtimesForTest(l, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sweep(root, true); err != nil {
		t.Fatal(err)
	}
	mt := func(p string) time.Time {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	if !mt(live).After(old.Add(time.Hour)) {
		t.Error("the claimable stalled entry must be poked")
	}
	if !mt(blocked).Before(old.Add(time.Hour)) {
		t.Error("a blocked ticket must not be poked: it is not work to wake an owner for")
	}
}

// A blocked ticket is listed on the surface that renders when something else is wrong too: those are
// the moments an operator reads it, and a separate branch is where a listing can silently go missing.
func TestFormatStatusSurface_BlockedIsListedBesideOtherFindings(t *testing.T) {
	st := SweepStatus{
		Blocked:      []BlockedTicket{{Lane: "developer", Ticket: "87-x.md", BlockedOn: "owner decision"}},
		Stalled:      1,
		StalledLanes: []StalledLane{{Lane: "code-review", Pending: 2}},
	}
	if st.Clean() {
		t.Fatal("control: a stalled lane makes the status unclean")
	}
	out := FormatStatusSurface("p", st, true, "report", 0, time.Now())
	for _, want := range []string{"STALLED code-review", "BLOCKED developer — 87-x.md (blocked_on: owner decision)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the unhealthy surface must show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "queue healthy") {
		t.Errorf("an unclean queue must not read healthy:\n%s", out)
	}
}

// A leftover claim on a ticket that is already done must not wake the lane's owners: the work is over,
// and an owner woken for it has nothing to do and nothing it may change. Ticket 200's sibling case.
func TestWait_AStaleInProgressClaimOnAFinishedOrBlockedTicketDoesNotWake(t *testing.T) {
	for _, status := range []string{"done", "completed", "dropped", "superseded", "blocked", `"done"`, "Done"} {
		root := t.TempDir()
		lane := filepath.Join(root, "finalise")
		putTicket(t, root, "finalise", "in-progress", "1-stale.md", status, "")
		res, err := Wait(lane, 60*time.Millisecond, 10*time.Millisecond, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Kind != "TIMEOUT" {
			t.Errorf("status %q: Kind = %s (%q), want TIMEOUT: a finished or blocked claim is not an orphan", status, res.Kind, res.Msg)
		}
	}

	// ...while a live claim is still an orphan, and sits beside the stale one without being hidden by it.
	root := t.TempDir()
	lane := filepath.Join(root, "finalise")
	putTicket(t, root, "finalise", "in-progress", "1-stale.md", "done", "")
	putTicket(t, root, "finalise", "in-progress", "2-live.md", "in_progress", "")
	res, err := Wait(lane, time.Second, 10*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "ORPHAN-INPROGRESS" || res.Msg != "2-live.md" {
		t.Errorf("got %s %q, want ORPHAN-INPROGRESS naming only the live claim", res.Kind, res.Msg)
	}
}

// A finished ticket left in pending is not work either, and an actionable arrival still wakes a wait
// that was already blocked on a lane holding only blocked and finished tickets.
func TestWait_ABlockedWaitWakesWhenAnActionableTicketArrives(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "developer")
	putTicket(t, root, "developer", "pending", "1-blocked.md", "blocked", "owner")
	putTicket(t, root, "developer", "pending", "2-done.md", "done", "")

	type out struct {
		res WaitResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, e := Wait(lane, 5*time.Second, 20*time.Millisecond, "")
		ch <- out{r, e}
	}()
	select {
	case o := <-ch:
		t.Fatalf("Wait returned with only blocked and finished tickets: %+v %v", o.res, o.err)
	case <-time.After(300 * time.Millisecond):
	}
	putTicket(t, root, "developer", "pending", "3-real.md", "pending", "")
	select {
	case o := <-ch:
		if o.err != nil || o.res.Kind != "WORK-PENDING" || o.res.Msg != "3-real.md" {
			t.Errorf("got %+v %v, want WORK-PENDING naming only 3-real.md", o.res, o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an actionable arrival did not wake the blocked wait")
	}
}
