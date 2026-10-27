package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitResultWithin runs WaitAs in a goroutine and returns its result, failing the test if it
// has not returned within bound.
func waitResultWithin(t *testing.T, dir string, interval, bound time.Duration, arm func()) WaitResult {
	t.Helper()
	type out struct {
		res WaitResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := WaitAs(Claimant{}, dir, time.Minute, interval)
		ch <- out{r, err}
	}()
	time.Sleep(100 * time.Millisecond) // let the watch arm; the poll is what is under test
	arm()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatal(o.err)
		}
		return o.res
	case <-time.After(bound):
		t.Fatalf("WaitAs did not wake within %s: an entry is stranded", bound)
		return WaitResult{}
	}
}

// This pins EXISTING wake behaviour (it passes without the skip log); it is not a test of the diagnostic.
// A ticket that arrives where no inotify event fires in the watched dir (a symlink whose target
// lands later, in the flat store) is caught by the interval re-scan, not stranded.
func TestWaitAs_PollCatchesDanglingLinkThatResolvesLater(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "lane")
	if err := os.MkdirAll(filepath.Join(lane, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(store, "t.md")
	if err := os.Symlink(target, filepath.Join(lane, "pending", "t.md")); err != nil {
		t.Fatal(err)
	}
	res := waitResultWithin(t, lane, 200*time.Millisecond, 3*time.Second, func() {
		if err := os.WriteFile(target, []byte("---\nstatus: pending\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if res.Kind != "WORK-PENDING" {
		t.Fatalf("got %+v", res)
	}
}

// This pins EXISTING wake behaviour (it passes without the skip log); it is not a test of the diagnostic.
// If the lane's pending/ is replaced by another directory (a repaired symlink) the watch is on the
// wrong inode and no event arrives for the new one; the interval re-scan must still find the entry.
func TestWaitAs_PollCatchesEntryAfterPendingDirSwapped(t *testing.T) {
	root := t.TempDir()
	lane := filepath.Join(root, "lane")
	if err := os.MkdirAll(filepath.Join(lane, "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(root, "flat")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	res := waitResultWithin(t, lane, 200*time.Millisecond, 3*time.Second, func() {
		if err := os.Remove(filepath.Join(lane, "pending")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(flat, filepath.Join(lane, "pending")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(flat, "t.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if res.Kind != "WORK-PENDING" {
		t.Fatalf("got %+v", res)
	}
}

// An entry that looks like work but is not counted is reported on the skip log, once, with why.
func TestWaitAs_LogsWhyAWorkLookingEntryIsSkipped(t *testing.T) {
	lane := filepath.Join(t.TempDir(), "lane")
	pend := filepath.Join(lane, "pending")
	if err := os.MkdirAll(pend, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(pend, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("blocked.md", "---\nstatus: blocked\nblocked_on: x\n---\n")
	write("done.md", "---\nstatus: done\n---\n")
	write("notes.txt", "x")
	if err := os.Symlink(filepath.Join(lane, "gone.md"), filepath.Join(pend, "dangling.md")); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	old := waitSkipLog
	waitSkipLog = &buf
	defer func() { waitSkipLog = old }()

	res, err := WaitAs(Claimant{}, lane, 300*time.Millisecond, 50*time.Millisecond)
	if err != nil || res.Kind != "TIMEOUT" {
		t.Fatalf("got %+v, %v", res, err)
	}
	got := buf.String()
	for _, want := range []string{"blocked.md: status blocked", "done.md: terminal status done", "notes.txt: stray", "dangling.md: dangling"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want exactly one %q in:\n%s", want, got)
		}
	}
}
