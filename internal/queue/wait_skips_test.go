package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skipFixture is a lane with pending/ and in-progress/.
type skipFixture struct {
	t    *testing.T
	lane string
}

func newSkipFixture(t *testing.T) *skipFixture {
	t.Helper()
	lane := filepath.Join(t.TempDir(), "lane")
	for _, st := range workStates {
		if err := os.MkdirAll(filepath.Join(lane, st), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &skipFixture{t: t, lane: lane}
}

func (f *skipFixture) write(state, name, body string) string {
	f.t.Helper()
	p := filepath.Join(f.lane, state, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func ticket(status string) string { return "---\nstatus: " + status + "\n---\n" }

// claim records by as the holder of the in-progress entry p for ttl from now.
func (f *skipFixture) claim(p, by string, at time.Time, ttl time.Duration) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(claimFile(p)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := writeClaim(p, Claim{By: by, At: at, TTLSeconds: int(ttl.Seconds())}); err != nil {
		f.t.Fatal(err)
	}
}

// capture swaps the skip log for a buffer for the rest of the test and returns it.
func captureSkipLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	old := waitSkipLog
	waitSkipLog = &buf
	t.Cleanup(func() { waitSkipLog = old })
	return &buf
}

// reportOnce runs one reporter pass over the lane as who, and returns what it logged.
func (f *skipFixture) reportOnce(who Claimant) string {
	f.t.Helper()
	buf := captureSkipLog(f.t)
	r := newSkipReporter(who)
	r.report(f.lane)
	r.close()
	return buf.String()
}

func wantLine(t *testing.T, got, want string) {
	t.Helper()
	if strings.Count(got, want) != 1 {
		t.Errorf("want exactly one %q in:\n%s", want, got)
	}
}

// A ticket in in-progress/ that a peer holds a live claim on is the stranded lane a waiter most needs
// explained: the waiter is not woken, on purpose, and the log says by whom.
func TestSkipReport_LiveClaimOfAPeerIsNamed(t *testing.T) {
	f := newSkipFixture(t)
	p := f.write("in-progress", "held.md", ticket("in_progress"))
	f.claim(p, "peer", time.Now(), time.Hour)
	got := f.reportOnce(Claimant{By: "me"})
	wantLine(t, got, "in-progress/held.md: held by live claim of peer\n")
}

func TestSkipReport_OwnClaimLapsedClaimAndAnonymousWaiterAreNotReported(t *testing.T) {
	f := newSkipFixture(t)
	own := f.write("in-progress", "mine.md", ticket("in_progress"))
	f.claim(own, "me", time.Now(), time.Hour)
	lapsed := f.write("in-progress", "lapsed.md", ticket("in_progress"))
	f.claim(lapsed, "peer", time.Now().Add(-3*time.Hour), time.Hour)
	if got := f.reportOnce(Claimant{By: "me"}); got != "" {
		t.Errorf("own and lapsed claims are not skips; logged:\n%s", got)
	}

	// A waiter that does not say who it is is woken by a claimed ticket (unclaimedByOthers), so there
	// is nothing to explain for it either.
	held := f.write("in-progress", "held.md", ticket("in_progress"))
	f.claim(held, "peer", time.Now(), time.Hour)
	if got := f.reportOnce(Claimant{}); got != "" {
		t.Errorf("an anonymous waiter is not skipping a claimed ticket; logged:\n%s", got)
	}
}

// The same claim, through WaitAs: the waiter's own name must reach the reporter.
func TestWaitAs_LogsALiveClaimOfAPeerAndNotItsOwn(t *testing.T) {
	f := newSkipFixture(t)
	theirs := f.write("in-progress", "theirs.md", ticket("in_progress"))
	f.claim(theirs, "peer", time.Now(), time.Hour)
	mine := f.write("in-progress", "mine.md", ticket("in_progress"))
	f.claim(mine, "me", time.Now(), time.Hour)
	// mine.md is the waiter's own, so wait wakes for it; take it out of the way to watch the idle path.
	if err := os.Remove(mine); err != nil {
		t.Fatal(err)
	}
	buf := captureSkipLog(t)
	res, err := WaitAs(Claimant{By: "me"}, f.lane, 300*time.Millisecond, 50*time.Millisecond)
	if err != nil || res.Kind != "TIMEOUT" {
		t.Fatalf("got %+v, %v", res, err)
	}
	wantLine(t, buf.String(), "in-progress/theirs.md: held by live claim of peer\n")
}

func TestSkipReport_DirectoryNamedLikeATicket(t *testing.T) {
	f := newSkipFixture(t)
	if err := os.Mkdir(filepath.Join(f.lane, "pending", "x.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantLine(t, f.reportOnce(Claimant{}), "pending/x.md: not a regular file\n")
}

func TestSkipReport_InProgressEntriesAreReported(t *testing.T) {
	f := newSkipFixture(t)
	f.write("in-progress", "blocked.md", "---\nstatus: blocked\nblocked_on: other\n---\n")
	f.write("in-progress", "done.md", ticket("done"))
	f.write("in-progress", "notes.txt", "x")
	got := f.reportOnce(Claimant{})
	wantLine(t, got, "in-progress/blocked.md: status blocked (blocked_on: other)\n")
	wantLine(t, got, "in-progress/done.md: terminal status done\n")
	wantLine(t, got, "in-progress/notes.txt: stray, not a .md ticket\n")
}

// The reason names the ticket's own terminal status, not a fixed one.
func TestSkipReport_NamesEachTerminalStatus(t *testing.T) {
	f := newSkipFixture(t)
	for _, st := range []string{"completed", "dropped", "superseded"} {
		f.write("pending", st+".md", ticket(st))
	}
	got := f.reportOnce(Claimant{})
	for _, st := range []string{"completed", "dropped", "superseded"} {
		wantLine(t, got, "pending/"+st+".md: terminal status "+st+"\n")
	}
}

func TestSkipReport_NamesWhatABlockedTicketIsBlockedOn(t *testing.T) {
	f := newSkipFixture(t)
	f.write("pending", "a.md", "---\nstatus: blocked\nblocked_on: ticket-42 review\n---\n")
	wantLine(t, f.reportOnce(Claimant{}), "pending/a.md: status blocked (blocked_on: ticket-42 review)\n")
}

// A reason that changes for the same path is a new fact and is logged again; an unchanged one is not.
func TestSkipReport_ChangedReasonForTheSamePathIsLoggedAgain(t *testing.T) {
	f := newSkipFixture(t)
	p := f.write("pending", "x.md", "---\nstatus: blocked\nblocked_on: y\n---\n")
	buf := captureSkipLog(t)
	r := newSkipReporter(Claimant{})
	r.report(f.lane)
	r.report(f.lane) // unchanged: not repeated
	if err := os.WriteFile(p, []byte(ticket("dropped")), 0o644); err != nil {
		t.Fatal(err)
	}
	r.report(f.lane)
	r.report(f.lane)
	r.close()
	wantLine(t, buf.String(), "pending/x.md: status blocked (blocked_on: y)\n")
	wantLine(t, buf.String(), "pending/x.md: terminal status dropped\n")
}

// The wait names the directory it watches, absolute and once, however it was passed.
func TestWaitAs_LogsTheAbsoluteWaitDirOnce(t *testing.T) {
	f := newSkipFixture(t)
	f.write("pending", "done.md", ticket("done")) // gives the re-scans something to log
	t.Chdir(filepath.Dir(f.lane))
	want, err := filepath.Abs("lane")
	if err != nil || !filepath.IsAbs(want) {
		t.Fatalf("abs: %q, %v", want, err)
	}
	buf := captureSkipLog(t)
	res, err := WaitAs(Claimant{}, "lane", 300*time.Millisecond, 50*time.Millisecond)
	if err != nil || res.Kind != "TIMEOUT" {
		t.Fatalf("got %+v, %v", res, err)
	}
	wantLine(t, buf.String(), "queue wait: watching "+want+"\n")
	if n := strings.Count(buf.String(), "watching"); n != 1 {
		t.Errorf("want the directory logged once, got %d in:\n%s", n, buf.String())
	}
}

// blockingWriter stands in for a stderr pipe whose reader has stalled: every write blocks until released.
type blockingWriter struct{ release chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// gatedWriter is a log that takes nothing until released, then keeps what it was given.
type gatedWriter struct {
	release chan struct{}
	sb      *strings.Builder
}

func (w gatedWriter) Write(p []byte) (int, error) {
	<-w.release
	return w.sb.Write(p)
}

// Reporting returns while the log is stalled, even with more lines than the buffer holds; the lines
// that did not fit are not lost, they are logged on a later pass once the log moves.
func TestSkipReport_StalledLogDoesNotBlockAndDroppedLinesAreRetried(t *testing.T) {
	const n = 3 * skipLogBuffer
	f := newSkipFixture(t)
	for i := 0; i < n; i++ {
		f.write("pending", fmt.Sprintf("note-%03d.txt", i), "x")
	}
	w := gatedWriter{release: make(chan struct{}), sb: &strings.Builder{}}
	old := waitSkipLog
	waitSkipLog = w
	t.Cleanup(func() { waitSkipLog = old })
	r := newSkipReporter(Claimant{})

	done := make(chan struct{})
	go func() { r.report(f.lane); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(w.release)
		t.Fatal("report blocked on a stalled log")
	}
	close(w.release)
	for i := 0; i < 100 && len(r.seen) < n; i++ {
		time.Sleep(10 * time.Millisecond)
		r.report(f.lane)
	}
	r.close()
	got := w.sb.String()
	for i := 0; i < n; i++ {
		wantLine(t, got, fmt.Sprintf("pending/note-%03d.txt: stray, not a .md ticket\n", i))
	}
}

// A stalled log must not stall the wait. With the write in the loop the waiter would sit in it, alive and
// never re-checking, and it would not even reach its timeout: the symptom of ticket 304. Here the log
// blocks on the very first line, and the wait must still run out its time and return.
func TestWaitAs_StalledSkipLogDoesNotStallTheWait(t *testing.T) {
	f := newSkipFixture(t) // an empty lane: the "watching" line alone is enough to stall the log
	w := blockingWriter{release: make(chan struct{})}
	old := waitSkipLog
	waitSkipLog = w
	t.Cleanup(func() { close(w.release); waitSkipLog = old })

	type out struct {
		res WaitResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := WaitAs(Claimant{}, f.lane, 300*time.Millisecond, 50*time.Millisecond)
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil || o.res.Kind != "TIMEOUT" {
			t.Fatalf("got %+v, %v", o.res, o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitAs did not return: a stalled skip log is holding the wait loop")
	}
}
