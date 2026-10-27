package queue

import (
	"strings"
	"testing"
	"time"
)

// A pending/ can hold a blocked ticket and an entry that is not a ticket at once. Each is excluded from
// the work count by its own check, and the two checks must compose: neither wakes the owner, and a ready
// ticket beside them is still the only thing reported.
func TestWait_ABlockedTicketAndAStrayBesideAReadyOneWakeOnlyForTheReadyOne(t *testing.T) {
	l := newStrayLane(t)
	putTicket(t, l.root, l.lane, "pending", "1-blocked.md", "blocked", "an owner decision")
	l.mkdir(t, "pending", "in-progress")

	res, err := Wait(l.root+"/"+l.lane, 300*time.Millisecond, 20*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "TIMEOUT" {
		t.Fatalf("a blocked ticket and a stray directory are not work: %s %q", res.Kind, res.Msg)
	}

	putTicket(t, l.root, l.lane, "pending", "2-ready.md", "ready", "")
	res, err = Wait(l.root+"/"+l.lane, 300*time.Millisecond, 20*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "WORK-PENDING" || res.Msg != "2-ready.md" {
		t.Fatalf("only the ready ticket is work: %s %q", res.Kind, res.Msg)
	}
}

func TestSweep_ABlockedTicketAndAStrayAreEachReportedAndNeitherIsStalledWork(t *testing.T) {
	l := newStrayLane(t)
	putTicket(t, l.root, l.lane, "pending", "1-blocked.md", "blocked", "an owner decision")
	l.mkdir(t, "pending", "in-progress")

	res, err := Sweep(l.root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, lc := range res.Lanes {
		if lc.Lane == l.lane && lc.Pending != 0 {
			t.Errorf("neither entry is pending work: %+v", lc)
		}
	}
	if res.Stalled != 0 {
		t.Errorf("neither entry is stalled work: %v", res.Lines)
	}
	if len(res.Blocked) != 1 || len(res.Strays) != 1 {
		t.Errorf("each must be reported by its own check: blocked=%+v strays=%+v", res.Blocked, res.Strays)
	}

	putTicket(t, l.root, l.lane, "pending", "2-ready.md", "ready", "")
	res, _ = Sweep(l.root, false)
	if res.Stalled != 1 || strings.Join(res.StalledLanes[0].Entries, " ") != "2-ready.md" || res.StalledLanes[0].Pending != 1 {
		t.Fatalf("the ready ticket alone is stalled work: %+v", res.StalledLanes)
	}
}
