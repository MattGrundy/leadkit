package queue

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claimLane builds a reviewer lane holding one ticket in pending.
func claimLane(t *testing.T) (root, lane, pending string) {
	t.Helper()
	root = t.TempDir()
	lane = filepath.Join(root, "code-review")
	pending = putTicket(t, root, "code-review", "pending", "9-x.md", "in_review", "")
	return root, lane, pending
}

func inProgress(lane string) string { return filepath.Join(lane, "in-progress", "9-x.md") }

func TestClaimEntry_TheSecondOwnerIsRefusedAndToldWhoHoldsIt(t *testing.T) {
	root, lane, pending := claimLane(t)
	now := time.Now()
	if _, err := ClaimEntry(root, pending, "cr-owner-4", now, 0); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := os.Lstat(inProgress(lane)); err != nil {
		t.Fatalf("the claim must move the ticket into in-progress: %v", err)
	}
	_, err := ClaimEntry(root, inProgress(lane), "cr-owner-5", now.Add(time.Minute), 0)
	if err == nil {
		t.Fatal("a second owner claimed a ticket a live claim holds")
	}
	for _, want := range []string{"cr-owner-4", "already", "expires"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if c, _ := ReadClaim(inProgress(lane)); c.By != "cr-owner-4" {
		t.Errorf("a refused claim changed the holder: %+v", c)
	}
}

func TestClaimEntry_RenewTakeoverAndAdoption(t *testing.T) {
	root, lane, pending := claimLane(t)
	t0 := time.Now().Truncate(time.Second)
	if _, err := ClaimEntry(root, pending, "cr-owner-4", t0, time.Hour); err != nil {
		t.Fatal(err)
	}

	// The holder renews: the clock restarts.
	c, err := ClaimEntry(root, inProgress(lane), "cr-owner-4", t0.Add(50*time.Minute), time.Hour)
	if err != nil || !c.At.Equal(t0.Add(50*time.Minute)) {
		t.Fatalf("renew: %+v %v", c, err)
	}
	// ...so at t0+90m (past the original expiry) it still holds against a peer.
	if _, err := ClaimEntry(root, inProgress(lane), "cr-owner-5", t0.Add(90*time.Minute), time.Hour); err == nil {
		t.Fatal("a renewed claim lapsed at its original expiry")
	}
	// A claim that has lapsed is available again, to anyone.
	c, err = ClaimEntry(root, inProgress(lane), "cr-owner-5", t0.Add(3*time.Hour), time.Hour)
	if err != nil || c.By != "cr-owner-5" {
		t.Fatalf("takeover of a lapsed claim: %+v %v", c, err)
	}
	if got, _ := ReadClaim(inProgress(lane)); got.By != "cr-owner-5" {
		t.Errorf("holder after takeover = %+v", got)
	}

	// An in-progress ticket nobody claimed (the way every one was claimed before this verb) is adopted.
	root2, lane2, p2 := claimLane(t)
	if err := Move(root2, p2, filepath.Join(lane2, "in-progress")+"/", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadClaim(inProgress(lane2)); ok {
		t.Fatal("a plain move must not invent a claim")
	}
	if c, err := ClaimEntry(root2, inProgress(lane2), "cr-owner-4", t0, 0); err != nil || c.By != "cr-owner-4" {
		t.Fatalf("adopting an unclaimed in-progress ticket: %+v %v", c, err)
	}
}

func TestClaimEntry_OneWinnerWhenOwnersRace(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) (root, entry string, at time.Time){
		"from pending": func(t *testing.T) (string, string, time.Time) {
			root, _, pending := claimLane(t)
			return root, pending, time.Now()
		},
		"adopting an unclaimed in-progress ticket": func(t *testing.T) (string, string, time.Time) {
			root, lane, pending := claimLane(t)
			if err := Move(root, pending, filepath.Join(lane, "in-progress")+"/", false); err != nil {
				t.Fatal(err)
			}
			return root, inProgress(lane), time.Now()
		},
		"taking over a lapsed claim": func(t *testing.T) (string, string, time.Time) {
			root, lane, pending := claimLane(t)
			old := time.Now().Add(-5 * time.Hour)
			if _, err := ClaimEntry(root, pending, "dead-owner", old, time.Hour); err != nil {
				t.Fatal(err)
			}
			return root, inProgress(lane), time.Now()
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Repeat: a race that is lost by luck once is not a pin.
			for run := 0; run < 25; run++ {
				root, entry, at := setup(t)
				var won atomic.Int32
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						by := string(rune('a'+i)) + "-owner"
						if _, err := ClaimEntry(root, entry, by, at, time.Hour); err == nil {
							won.Add(1)
						}
					}(i)
				}
				close(start)
				wg.Wait()
				if won.Load() != 1 {
					t.Fatalf("run %d: %d owners believe they hold the claim, want exactly 1", run, won.Load())
				}
			}
		})
	}
}

func TestClaimEntry_RefusesWhatItCannotClaim(t *testing.T) {
	root, lane, pending := claimLane(t)
	for _, by := range []string{"", "  ", "a/b", "a\nb"} {
		if _, err := ClaimEntry(root, pending, by, time.Now(), 0); err == nil {
			t.Errorf("claimant %q accepted", by)
		}
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Fatal("a refused claim moved the ticket")
	}
	// A blocked ticket is refused by the move it rides on.
	bl := putTicket(t, root, "code-review", "pending", "10-blocked.md", "blocked", "owner")
	if _, err := ClaimEntry(root, bl, "cr-owner-4", time.Now(), 0); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("claiming a blocked ticket: %v", err)
	}
	// Only pending and in-progress entries are claimable.
	done := putTicket(t, root, "code-review", "done", "11-done.md", "done", "")
	if _, err := ClaimEntry(root, done, "cr-owner-4", time.Now(), 0); err == nil {
		t.Error("claimed a done ticket")
	}
	_ = lane
}

func TestMove_LeavingInProgressEndsTheClaimAndArrivingStartsAnUnclaimedStay(t *testing.T) {
	root, lane, pending := claimLane(t)
	if _, err := ClaimEntry(root, pending, "cr-owner-4", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	sidecar := claimFile(inProgress(lane))
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("fixture: no claim written: %v", err)
	}
	if err := Move(root, inProgress(lane), filepath.Join(lane, "done")+"/", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecar); err == nil {
		t.Error("the claim survived the ticket leaving in-progress")
	}

	// A sidecar left behind by an earlier occupant (a crash, a manual rm) is not the new arrival's claim.
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte(`{"by":"ghost","at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Move(root, filepath.Join(lane, "done", "9-x.md"), filepath.Join(lane, "in-progress")+"/", false); err != nil {
		t.Fatal(err)
	}
	if c, ok := ReadClaim(inProgress(lane)); ok {
		t.Errorf("a stale sidecar was read as the new arrival's claim: %+v", c)
	}
}

func TestWaitAs_APeersLiveClaimIsNotAnOrphanForMe(t *testing.T) {
	root, lane, pending := claimLane(t)
	if _, err := ClaimEntry(root, pending, "cr-owner-4", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	wait := func(by string) WaitResult {
		t.Helper()
		res, err := WaitAs(Claimant{By: by}, lane, 80*time.Millisecond, 10*time.Millisecond, "")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := wait("cr-owner-5"); r.Kind != "TIMEOUT" {
		t.Errorf("a peer's live claim woke another owner: %s %q", r.Kind, r.Msg)
	}
	if r := wait("cr-owner-4"); r.Kind != "ORPHAN-INPROGRESS" {
		t.Errorf("the holder must still be woken for its own claim after a restart: %s %q", r.Kind, r.Msg)
	}
	// The zero value asks nothing of claims: today's behaviour for a caller that does not say who it is.
	if r := wait(""); r.Kind != "ORPHAN-INPROGRESS" {
		t.Errorf("an anonymous wait must see every in-progress ticket: %s %q", r.Kind, r.Msg)
	}

	// A claim that lapses stops protecting the ticket: the dead holder's work is up for grabs.
	if _, err := ClaimEntry(root, inProgress(lane), "cr-owner-4", time.Now().Add(-3*time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if r := wait("cr-owner-5"); r.Kind != "ORPHAN-INPROGRESS" || r.Msg != "9-x.md" {
		t.Errorf("a lapsed claim must wake a peer: %s %q", r.Kind, r.Msg)
	}
}

func TestWaitAs_APeerClaimingWhileIWaitDoesNotWakeMeAndAnUnclaimedOneDoes(t *testing.T) {
	root, lane, _ := claimLane(t)
	p2 := putTicket(t, root, "code-review", "pending", "10-y.md", "in_review", "")
	// pending holds work, so claim both away first
	if err := Move(root, filepath.Join(lane, "pending", "9-x.md"), filepath.Join(lane, "in-progress")+"/", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimEntry(root, filepath.Join(lane, "in-progress", "9-x.md"), "cr-owner-4", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if err := Move(root, p2, filepath.Join(lane, "in-progress")+"/", false); err != nil {
		t.Fatal(err)
	}
	res, err := WaitAs(Claimant{By: "cr-owner-5"}, lane, time.Second, 10*time.Millisecond, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "ORPHAN-INPROGRESS" || res.Msg != "10-y.md" {
		t.Errorf("got %s %q: only the unclaimed ticket is mine to resume", res.Kind, res.Msg)
	}
}
