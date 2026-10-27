package queue

import (
	"strings"
	"testing"
	"time"
)

// wait and sweep must give the same answer about a finished ticket sitting in pending: wait does not wake
// for it (the work is over) and sweep does not call the lane STALLED (nothing is stuck), but sweep still
// REPORTS it, as a distinct condition, because once wait ignores a ticket a wrong status is otherwise
// silent.
func TestSweepAndWait_AgreeAboutAFinishedTicketInPending(t *testing.T) {
	root := t.TempDir()
	lane := root + "/developer"
	putTicket(t, root, "developer", "pending", "1-old.md", "done", "")

	res, err := Wait(lane, 60*time.Millisecond, 10*time.Millisecond, "")
	if err != nil || res.Kind != "TIMEOUT" {
		t.Fatalf("wait must idle on a finished ticket: %+v %v", res, err)
	}
	sw, err := Sweep(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Stalled != 0 {
		t.Errorf("a finished ticket is not stalled work: %v", sw.Lines)
	}
	if len(sw.Finished) != 1 || sw.Finished[0] != (FinishedTicket{Lane: "developer", Ticket: "1-old.md", Status: "done"}) {
		t.Fatalf("Finished = %+v, want the one ticket with its status", sw.Finished)
	}
	var line string
	for _, l := range sw.Lines {
		if strings.Contains(l, "1-old.md") {
			line = l
		}
	}
	if !strings.Contains(line, "FINISHED") || strings.Contains(line, "STALLED") || !strings.Contains(line, "move it to done") {
		t.Errorf("the report line must name it as finished and say what to do: %q", line)
	}

	// A live ticket beside it is the only thing that wakes the owner and the only stalled work.
	putTicket(t, root, "developer", "pending", "2-live.md", "pending", "")
	if res, _ := Wait(lane, time.Second, 10*time.Millisecond, ""); res.Msg != "2-live.md" {
		t.Errorf("wait names only the live ticket: %+v", res)
	}
	sw, _ = Sweep(root, false)
	if sw.Stalled != 1 || len(sw.Finished) != 1 {
		t.Errorf("the live ticket is stalled and the finished one is still reported: stalled=%d finished=%+v", sw.Stalled, sw.Finished)
	}
}

// One terminal set, read from one place. A status added to it is followed by BOTH verbs; a copy in either
// would not be.
func TestWaitAndSweepReadOneTerminalSet(t *testing.T) {
	const status = "shelved_by_this_test"
	terminalStatuses[status] = true
	t.Cleanup(func() { delete(terminalStatuses, status) })

	root := t.TempDir()
	putTicket(t, root, "developer", "pending", "1-x.md", status, "")
	if res, _ := Wait(root+"/developer", 60*time.Millisecond, 10*time.Millisecond, ""); res.Kind != "TIMEOUT" {
		t.Errorf("wait did not follow the shared set: %+v", res)
	}
	sw, _ := Sweep(root, false)
	if sw.Stalled != 0 || len(sw.Finished) != 1 || sw.Finished[0].Status != status {
		t.Errorf("sweep did not follow the shared set: %+v", sw)
	}

	delete(terminalStatuses, status)
	if res, _ := Wait(root+"/developer", time.Second, 10*time.Millisecond, ""); res.Kind != "WORK-PENDING" {
		t.Errorf("with the status out of the set wait must wake again: %+v", res)
	}
	if sw, _ := Sweep(root, false); sw.Stalled != 1 || len(sw.Finished) != 0 {
		t.Errorf("with the status out of the set sweep must call it stalled again: %+v", sw)
	}
}

func TestFormatStatusSurface_ListsAFinishedTicket(t *testing.T) {
	s := SweepStatus{Finished: []FinishedTicket{{Lane: "developer", Ticket: "1-old.md", Status: "done"}}}
	out := FormatStatusSurface("/s.json", s, true, "", 0, time.Now())
	if !strings.Contains(out, "queue healthy") || !strings.Contains(out, "FINISHED developer — 1-old.md (status: done)") {
		t.Errorf("a finished ticket does not make the queue unhealthy but must still be listed:\n%s", out)
	}
}

// A finished ticket beside a stall is the common live case, and it takes the OTHER branch of the surface
// (the one that reports problems), which has its own loop.
func TestFormatStatusSurface_ListsAFinishedTicketBesideAStall(t *testing.T) {
	s := SweepStatus{
		Stalled:      1,
		StalledLanes: []StalledLane{{Lane: "critique", Pending: 2}},
		Finished:     []FinishedTicket{{Lane: "developer", Ticket: "1-old.md", Status: "done"}},
	}
	out := FormatStatusSurface("/s.json", s, true, "", 0, time.Now())
	if strings.Contains(out, "queue healthy") {
		t.Fatalf("a stall makes the queue unhealthy:\n%s", out)
	}
	if !strings.Contains(out, "STALLED critique") || !strings.Contains(out, "FINISHED developer — 1-old.md (status: done) left in pending") {
		t.Errorf("the finished ticket must be listed beside the stall:\n%s", out)
	}
}

// The findings artifact is what status, the board and the status surface read; a finished ticket the
// report names but the artifact drops is invisible to all of them.
func TestBuildSweepStatus_CarriesTheFinishedTickets(t *testing.T) {
	res := SweepResult{Finished: []FinishedTicket{{Lane: "developer", Ticket: "1-old.md", Status: "done"}}}
	s := BuildSweepStatus(res, "", time.Now())
	if len(s.Finished) != 1 || s.Finished[0].Ticket != "1-old.md" {
		t.Fatalf("the artifact dropped the finished ticket: %+v", s)
	}
}
