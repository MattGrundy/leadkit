package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// strayLane builds a lane the way the incident left it: one real ticket in the store, an EMPTY directory
// named `in-progress` inside the lane's pending/ (what a `queue move` with an empty ticket name left behind
// in critique/pending), and the other shapes that are not tickets.
type strayLane struct {
	root, lane string
	pending    string
}

func newStrayLane(t *testing.T) strayLane {
	t.Helper()
	root := t.TempDir()
	l := strayLane{root: root, lane: "critique", pending: filepath.Join(root, "critique", "pending")}
	mustMkdirAll(t, l.pending)
	mustMkdirAll(t, filepath.Join(root, "critique", "in-progress"))
	return l
}

func (l strayLane) mkdir(t *testing.T, state, name string) string {
	t.Helper()
	p := filepath.Join(l.root, l.lane, state, name)
	mustMkdirAll(t, p)
	return p
}

func (l strayLane) file(t *testing.T, state, name, content string) string {
	t.Helper()
	p := filepath.Join(l.root, l.lane, state, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (l strayLane) ticket(t *testing.T, state, name string) {
	t.Helper()
	setupSymlinkEntry(t, filepath.Join(l.root, "tasks"), filepath.Join(l.root, l.lane, state, name), name)
}

// THE FAILING CASE from the incident: an empty directory in pending woke the owner, every time.
func TestWait_AStrayDirectoryIsNotWork(t *testing.T) {
	for _, state := range []string{"pending", "in-progress"} {
		t.Run(state, func(t *testing.T) {
			l := newStrayLane(t)
			l.mkdir(t, state, "in-progress")
			l.file(t, state, "notes.txt", "x")
			l.file(t, state, "README", "")
			// a dangling link cannot be claimed either
			if err := os.Symlink(filepath.Join(l.root, "tasks", "gone.md"), filepath.Join(l.root, l.lane, state, "gone.md")); err != nil {
				t.Fatal(err)
			}
			// ...nor can a link to a directory, even one named like a ticket
			mustMkdirAll(t, filepath.Join(l.root, "tasks", "adir.md"))
			if err := os.Symlink(filepath.Join(l.root, "tasks", "adir.md"), filepath.Join(l.root, l.lane, state, "adir.md")); err != nil {
				t.Fatal(err)
			}

			res, err := Wait(filepath.Join(l.root, l.lane), 300*time.Millisecond, 20*time.Millisecond, "")
			if err != nil {
				t.Fatal(err)
			}
			if res.Kind != "TIMEOUT" {
				t.Fatalf("entries that are not tickets must not wake an owner: %s %q", res.Kind, res.Msg)
			}
		})
	}
}

func TestWait_AStrayDirectoryAppearingWhileWaitingDoesNotWakeIt(t *testing.T) {
	l := newStrayLane(t)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.MkdirAll(filepath.Join(l.pending, "in-progress"), 0o755)
	}()
	res, err := Wait(filepath.Join(l.root, l.lane), 600*time.Millisecond, 50*time.Millisecond, "")
	if err != nil || res.Kind != "TIMEOUT" {
		t.Fatalf("a directory created mid-wait is not work: %v %+v", err, res)
	}
}

// The stray must not mask a real ticket sitting beside it.
func TestWait_ARealTicketBesideAStrayStillWakes(t *testing.T) {
	l := newStrayLane(t)
	l.mkdir(t, "pending", "in-progress")
	l.ticket(t, "pending", "7-real.md")
	res, err := Wait(filepath.Join(l.root, l.lane), time.Second, 20*time.Millisecond, "")
	if err != nil || res.Kind != "WORK-PENDING" || res.Msg != "7-real.md" {
		t.Fatalf("want WORK-PENDING for the ticket alone: %v %+v", err, res)
	}
}

func TestWait_APlainMarkdownFileIsStillATicket(t *testing.T) {
	l := newStrayLane(t)
	l.file(t, "pending", "9-plain.md", "---\nid: 9\n---\n")
	res, _ := Wait(filepath.Join(l.root, l.lane), time.Second, 20*time.Millisecond, "")
	if res.Kind != "WORK-PENDING" {
		t.Fatalf("a regular .md file is work: %+v", res)
	}
}

func TestMaybeWake_AStrayDirectoryIsNotAPendingTicket(t *testing.T) {
	l := newStrayLane(t)
	l.mkdir(t, "pending", "in-progress")
	log, lines := captureLog()
	maybeWake(l.root, l.lane, "", false, log)
	if len(*lines) != 0 {
		t.Fatalf("the waker must not announce work for a directory: %v", *lines)
	}
}

func TestSweep_ReportsAStrayAndDoesNotCountItAsWork(t *testing.T) {
	l := newStrayLane(t)
	l.mkdir(t, "pending", "in-progress")
	l.file(t, "in-progress", "notes.txt", "x")

	res, err := Sweep(l.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stalled != 0 {
		t.Errorf("a stray is not pending work, so the lane is not STALLED: %v", res.Lines)
	}
	if len(res.Lanes) != 1 || res.Lanes[0].Pending != 0 || res.Lanes[0].InProgress != 0 {
		t.Errorf("the lane census must count no work: %+v", res.Lanes)
	}
	joined := strings.Join(res.Lines, "\n")
	for _, want := range []string{"STRAY   critique/pending/in-progress — empty directory", "STRAY   critique/in-progress/notes.txt — file that is not a ticket"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the sweep must report %q:\n%s", want, joined)
		}
	}
	if len(res.Strays) != 2 {
		t.Errorf("Strays = %+v", res.Strays)
	}
}

func TestSweep_ARealTicketAndAStrayAreCountedApart(t *testing.T) {
	l := newStrayLane(t)
	l.mkdir(t, "pending", "in-progress")
	l.ticket(t, "pending", "1-a.md")
	res, _ := Sweep(l.root, false)
	if res.Stalled != 1 || res.StalledLanes[0].Pending != 1 || strings.Contains(strings.Join(res.StalledLanes[0].Entries, " "), "in-progress") {
		t.Fatalf("one ticket is pending and the stray is not in its list: %+v", res.StalledLanes)
	}
}

func TestRepair_ClearsAnEmptyStrayAndOnlyThat(t *testing.T) {
	l := newStrayLane(t)
	empty := l.mkdir(t, "pending", "in-progress")
	nested := filepath.Join(l.mkdir(t, "pending", "outer"), "inner")
	mustMkdirAll(t, nested)
	emptyFile := l.file(t, "in-progress", "zero", "")
	holding := l.mkdir(t, "pending", "holds")
	l.file(t, "pending", "holds/keep.txt", "x")
	data := l.file(t, "in-progress", "notes.txt", "x")
	l.ticket(t, "pending", "1-a.md")

	// dry-run mutates nothing and says what it would do
	res, err := Repair(l.root, filepath.Join(l.root, "tasks"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{empty, nested, emptyFile, holding, data} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("dry-run removed %s", p)
		}
	}
	out := FormatRepair(res)
	for _, want := range []string{"CLEAR stray critique/pending/in-progress (empty directory)", "CLEAR stray critique/in-progress/zero (empty file)",
		"stray critique/pending/holds (directory holding files, inspect by hand) — reported, NOT removed", "5 stray non-ticket entr(ies), 3 clearable (dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report lacks %q:\n%s", want, out)
		}
	}

	res, err = Repair(l.root, filepath.Join(l.root, "tasks"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{empty, filepath.Dir(nested), emptyFile} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s should have been cleared", gone)
		}
	}
	for _, kept := range []string{holding, filepath.Join(holding, "keep.txt"), data, filepath.Join(l.pending, "1-a.md")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s holds something and must be kept: %v", kept, err)
		}
	}
	if len(res.ClearedStrays) != 3 || len(res.StrayErrors) != 0 {
		t.Errorf("cleared = %+v, errors = %v (a stray that holds something is not attempted)", res.ClearedStrays, res.StrayErrors)
	}
	if out := FormatRepair(res); !strings.Contains(out, "cleared stray critique/pending/in-progress") || !strings.Contains(out, "cleared 3") {
		t.Errorf("apply report: %s", out)
	}

	// and a second run is clean of anything clearable
	res, _ = Repair(l.root, filepath.Join(l.root, "tasks"), true, false)
	if len(res.ClearedStrays) != 0 {
		t.Errorf("nothing left to clear: %+v", res.ClearedStrays)
	}
}

// A queue with no strays must report exactly what it did before.
func TestRepair_ACleanQueueSaysNothingAboutStrays(t *testing.T) {
	l := newStrayLane(t)
	l.ticket(t, "pending", "1-a.md")
	res, _ := Repair(l.root, filepath.Join(l.root, "tasks"), false, false)
	if out := FormatRepair(res); strings.Contains(out, "stray") {
		t.Errorf("no strays, no stray lines: %s", out)
	}
}

// Alias inboxes are left alone, as the sweep leaves them, and so are non-canonical states (ticket 89's).
func TestFindStrays_LeavesAliasInboxesAndOtherStatesAlone(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "real", "pending", "stray-dir"))
	mustMkdirAll(t, filepath.Join(root, "alias"))
	if err := os.Symlink(filepath.Join(root, "real", "pending"), filepath.Join(root, "alias", "pending")); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(root, "real", "done", "whatever"))
	got, err := FindStrays(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Rel() != "real/pending/stray-dir" {
		t.Fatalf("only the real pending stray, once: %+v", got)
	}
}

// --- queue move: the source side, which is what created the stray ---------------------------------

// THE ROOT CAUSE: an empty ticket name resolves <lane>/in-progress/ + "" to the state directory itself,
// and Move relocated it. Reproduced exactly: the destination is another lane's pending.
func TestMove_RefusesADirectorySource_TheEmptyTicketNameCase(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "code-review", "in-progress"))
	mustMkdirAll(t, filepath.Join(root, "critique", "pending"))
	state := filepath.Join(root, "code-review", "in-progress")
	dest := filepath.Join(root, "critique", "pending") + "/"

	for _, from := range []string{state, state + "/", filepath.Join(state, "")} {
		err := Move(root, from, dest, false)
		if err == nil || !strings.Contains(err.Error(), "refuse") {
			t.Fatalf("moving a state directory must be refused (%q): %v", from, err)
		}
	}
	if _, err := os.Lstat(state); err != nil {
		t.Fatalf("the state directory must still be there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "critique", "pending", "in-progress")); err == nil {
		t.Fatal("nothing may land in the destination")
	}
}

func TestMove_RefusesAnEmptySourceAndALinkToADirectory(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "a", "pending"))
	if err := Move(root, "", filepath.Join(root, "a", "pending")+"/", false); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty source is refused: %v", err)
	}
	if err := Move(root, "  ", filepath.Join(root, "a", "pending")+"/", false); err == nil {
		t.Error("a blank source is refused")
	}
	mustMkdirAll(t, filepath.Join(root, "tasks", "d.md"))
	link := filepath.Join(root, "a", "in-progress", "d.md")
	mustMkdirAll(t, filepath.Dir(link))
	if err := os.Symlink(filepath.Join(root, "tasks", "d.md"), link); err != nil {
		t.Fatal(err)
	}
	if err := Move(root, link, filepath.Join(root, "a", "pending")+"/", false); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("a symlink to a directory is a directory: %v", err)
	}
}

func TestMove_RefusesADestinationDirectlyUnderALane(t *testing.T) {
	root := t.TempDir()
	from := filepath.Join(root, "a", "pending", "t.md")
	setupSymlinkEntry(t, filepath.Join(root, "tasks"), from, "t.md")
	mustMkdirAll(t, filepath.Join(root, "b"))

	for _, to := range []string{filepath.Join(root, "b") + "/", filepath.Join(root, "b"), filepath.Join(root, "b", "t.md")} {
		err := Move(root, from, to, false)
		if err == nil || !strings.Contains(err.Error(), "directly under a lane") {
			t.Errorf("dest %s names a lane where a state belongs: %v", to, err)
		}
	}
	if _, err := os.Lstat(from); err != nil {
		t.Fatalf("the ticket must not have moved: %v", err)
	}
	// the right form still works
	if err := Move(root, from, filepath.Join(root, "b", "pending")+"/", false); err != nil {
		t.Fatalf("lane/state/ is the valid form: %v", err)
	}
}

// A dangling link is the BROKEN finding and the reaper's, not a stray: reporting it twice would hide which
// verb fixes it.
func TestFindStrays_ADanglingLinkIsBrokenNotStray(t *testing.T) {
	l := newStrayLane(t)
	if err := os.Symlink(filepath.Join(l.root, "tasks", "gone.md"), filepath.Join(l.pending, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if got, _ := FindStrays(l.root); len(got) != 0 {
		t.Errorf("a dangling link is not a stray: %+v", got)
	}
	res, _ := Sweep(l.root, false)
	if res.Broken != 1 || len(res.Strays) != 0 {
		t.Errorf("it is reported as BROKEN: %+v", res)
	}
}

// removeStray is the one path here that destroys data, and it must not trust the classification it was
// handed: the entry may have changed between the scan and the removal. Each case hands it an entry
// marked removable that is not removable at removal time.
func TestRemoveStray_ReChecksAtTheMomentOfRemoval(t *testing.T) {
	l := newStrayLane(t)

	// a file that was empty at the scan and has since been written to
	f := l.file(t, "in-progress", "late-write", "")
	st := StrayEntry{Lane: l.lane, State: "in-progress", Name: "late-write", Path: f, Kind: "file", Removable: true}
	if err := os.WriteFile(f, []byte("someone wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStray(st); err == nil {
		t.Error("a file that now holds data must be refused")
	}
	if b, err := os.ReadFile(f); err != nil || string(b) != "someone wrote this" {
		t.Errorf("the file and its content must survive: %v %q", err, b)
	}

	// a file that was replaced by a symlink to something that matters
	target := l.file(t, "pending", "precious.txt", "keep")
	link := filepath.Join(l.root, l.lane, "in-progress", "swapped")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := removeStray(StrayEntry{Lane: l.lane, State: "in-progress", Name: "swapped", Path: link, Kind: "file", Removable: true}); err == nil {
		t.Error("a symlink is not an empty file and must be refused")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("the link must survive")
	}

	// a directory that was empty at the scan and has since gained a file
	d := l.mkdir(t, "pending", "was-empty")
	dst := StrayEntry{Lane: l.lane, State: "pending", Name: "was-empty", Path: d, Kind: "directory", Removable: true}
	inner := filepath.Join(d, "arrived.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStray(dst); err == nil {
		t.Error("a directory that now holds a file must be refused")
	}
	if _, err := os.Stat(inner); err != nil {
		t.Errorf("the file inside must survive: %v", err)
	}

	// something of size zero that is not a regular file (a named pipe) is not an empty file either
	fifo := filepath.Join(l.root, l.lane, "pending", "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeStray(StrayEntry{Lane: l.lane, State: "pending", Name: "pipe", Path: fifo, Kind: "file", Removable: true}); err == nil {
		t.Error("a named pipe is not a regular file and must be refused")
	}
	if _, err := os.Lstat(fifo); err != nil {
		t.Error("the pipe must survive")
	}

	// and an entry never classified removable is refused outright, even when it would pass every re-check
	empty := l.file(t, "pending", "empty-but-not-cleared", "")
	if err := removeStray(StrayEntry{Path: empty, Kind: "file", Removable: false}); err == nil {
		t.Error("a non-removable entry must be refused")
	}
	if _, err := os.Stat(empty); err != nil {
		t.Error("the entry must survive")
	}
}

// The sweep's unattended repair does only what self_repair: auto promises, and clears no stray.
func TestRepairDanglingOnly_ReapsDanglingLinksAndLeavesStraysAlone(t *testing.T) {
	l := newStrayLane(t)
	empty := l.mkdir(t, "pending", "in-progress")
	zero := l.file(t, "in-progress", "zero", "")
	setupSymlinkEntry(t, filepath.Join(l.root, "tasks"), filepath.Join(l.pending, "dead.md"), "dead.md")
	if err := os.Remove(filepath.Join(l.root, "tasks", "dead.md")); err != nil {
		t.Fatal(err)
	}

	res, err := RepairDanglingOnly(l.root, filepath.Join(l.root, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reaped) != 1 {
		t.Errorf("the dangling link is what auto repairs: %+v", res.Reaped)
	}
	for _, p := range []string{empty, zero} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("auto repair must not delete %s: %v", p, err)
		}
	}
	if len(res.Strays) != 0 || len(res.ClearedStrays) != 0 {
		t.Errorf("auto repair neither lists nor clears strays: %+v", res)
	}
	// the explicit verb still does
	if res, _ := Repair(l.root, filepath.Join(l.root, "tasks"), true, false); len(res.ClearedStrays) != 2 {
		t.Errorf("repair --apply clears them: %+v", res.ClearedStrays)
	}
}
