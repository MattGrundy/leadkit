package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// noteFixture is a queue + tasks tree with one ticket, reachable through a lane symlink.
func noteFixture(t *testing.T) (rv *Reviewer, canonical, lane string) {
	t.Helper()
	root := t.TempDir()
	tasks := filepath.Join(root, "tasks")
	queue := filepath.Join(root, "queue")
	for _, d := range []string{tasks, filepath.Join(queue, "code-review", "in-progress"), filepath.Join(queue, "critique", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	canonical = filepath.Join(tasks, "5-x.md")
	if err := os.WriteFile(canonical, []byte("---\ntitle: t\nstatus: in_review\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lane = filepath.Join(queue, "code-review", "in-progress", "5-x.md")
	if err := os.Symlink("../../../tasks/5-x.md", lane); err != nil {
		t.Fatal(err)
	}
	return &Reviewer{QueueRoot: queue, TasksDir: tasks}, canonical, lane
}

// samePath compares two spellings of one file.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("resolve %s / %s: %v %v", a, b, err1, err2)
	}
	return ra == rb
}

var noteAt = time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)

func TestAppendNote_LandsInTheTicketViaItsLanePath(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	res, err := rv.AppendNote(lane, "cr-1", "found nothing", noteAt)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(t, res.Path, canonical) {
		t.Errorf("landed at %s, want the canonical %s", res.Path, canonical)
	}
	got, _ := os.ReadFile(canonical)
	if !strings.HasSuffix(string(got), "\n## cr-1 — 2026-10-03T20:00:00Z\n\nfound nothing\n") {
		t.Errorf("ticket tail wrong:\n%s", got)
	}
}

// The failure this exists for: the lane path has been routed away, and a shell `>>` to it would
// have succeeded into a fresh stray file. The note must reach the ticket by name regardless.
func TestAppendNote_SurvivesTheLanePathBeingRoutedAway(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	if err := os.Remove(lane); err != nil { // routed: the symlink is gone from this lane
		t.Fatal(err)
	}
	if _, err := rv.AppendNote(lane, "cr-1", "routed before I wrote", noteAt); err != nil {
		t.Fatalf("a note for a routed ticket must still land: %v", err)
	}
	got, _ := os.ReadFile(canonical)
	if !strings.Contains(string(got), "routed before I wrote") {
		t.Errorf("note missing from the ticket:\n%s", got)
	}
	if _, err := os.Lstat(lane); !os.IsNotExist(err) {
		t.Errorf("AppendNote recreated the old lane path (stat err = %v): that is the stray-file bug", err)
	}
}

// A stray regular file at the lane path (what a raw `>>` left behind) must not capture the note.
func TestAppendNote_IgnoresAStrayFileAtTheLanePath(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	_ = os.Remove(lane)
	if err := os.WriteFile(lane, []byte("an earlier lost note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.AppendNote(lane, "cr-1", "the real one", noteAt); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(canonical)
	if !strings.Contains(string(got), "the real one") {
		t.Errorf("note missing from the ticket:\n%s", got)
	}
	stray, _ := os.ReadFile(lane)
	if string(stray) != "an earlier lost note\n" {
		t.Errorf("the stray file was written to: %q", stray)
	}
}

func TestAppendNote_FailsLoudlyAndWritesNothingForAnUnknownTicket(t *testing.T) {
	rv, _, _ := noteFixture(t)
	missing := filepath.Join(rv.QueueRoot, "code-review", "in-progress", "99-nope.md")
	_, err := rv.AppendNote(missing, "cr-1", "hello", noteAt)
	if err == nil || !strings.Contains(err.Error(), "nothing was written anywhere") {
		t.Fatalf("want a loud refusal, got %v", err)
	}
	if _, serr := os.Lstat(missing); !os.IsNotExist(serr) {
		t.Error("the refusal still created a file at the lane path")
	}
	entries, _ := os.ReadDir(rv.TasksDir)
	if len(entries) != 1 {
		t.Errorf("tasks dir changed: %v", entries)
	}
}

// A missing ticket named by a path under THIS tasks dir is refused for what it is (no such ticket),
// not misreported as a foreign path.
func TestAppendNote_AMissingTicketUnderTheTasksDirIsNotCalledForeign(t *testing.T) {
	rv, _, _ := noteFixture(t)
	_, err := rv.AppendNote(filepath.Join(rv.TasksDir, "99-nope.md"), "cr-1", "hello", noteAt)
	if err == nil || !strings.Contains(err.Error(), "no ticket 99-nope.md") {
		t.Fatalf("want the no-such-ticket refusal, got %v", err)
	}
}

func TestAppendNote_RefusesNoAuthorAndEmptyText(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	before, _ := os.ReadFile(canonical)
	if _, err := rv.AppendNote(lane, "", "x", noteAt); err == nil {
		t.Error("no --by must be refused")
	}
	if _, err := rv.AppendNote(lane, "cr-1", " \n", noteAt); err == nil {
		t.Error("an empty note must be refused")
	}
	if after, _ := os.ReadFile(canonical); string(after) != string(before) {
		t.Error("a refused note still changed the ticket")
	}
}

// Notes and frontmatter rewrites are both leadkit writers of the same file. Without the lock a
// rewrite's read-modify-write erased notes appended between its read and its write (measured:
// 131 of 1500 notes lost under this load on the unlocked code).
func TestAppendNote_IsNotLostToAConcurrentFrontmatterRewrite(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	big := strings.Repeat("filler line to make the file non-trivial\n", 400)
	f, _ := os.OpenFile(canonical, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(big)
	f.Close()

	const notes = 60
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if err := UpdateFrontmatter(canonical, []KV{{"stage", fmt.Sprintf("s%d", i)}}); err != nil {
				t.Errorf("UpdateFrontmatter: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < notes; i++ {
			if _, err := rv.AppendNote(lane, "cr-1", fmt.Sprintf("NOTE-%d", i), noteAt); err != nil {
				t.Errorf("AppendNote: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	got, _ := os.ReadFile(canonical)
	lost := 0
	for i := 0; i < notes; i++ {
		if !strings.Contains(string(got), fmt.Sprintf("\nNOTE-%d\n", i)) {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d notes lost to a concurrent frontmatter rewrite", lost, notes)
	}
	if !strings.Contains(string(got), "stage: s299") {
		t.Error("the last frontmatter update was lost")
	}
}

// The read-back is the only thing that tells the caller a write that "succeeded" did not stick.
func TestAppendNote_FailsWhenTheNoteCannotBeReadBack(t *testing.T) {
	rv, _, lane := noteFixture(t)
	orig := readBack
	defer func() { readBack = orig }()
	readBack = func(string) ([]byte, error) { return []byte("something else entirely"), nil }
	if _, err := rv.AppendNote(lane, "cr-1", "x", noteAt); err == nil || !strings.Contains(err.Error(), "could not read the note back") {
		t.Fatalf("a note that cannot be read back must not be reported as written: %v", err)
	}
}

// A raw writer that ignores the lock may append right after ours. The note landed, so that is not a failure.
func TestAppendNote_ToleratesAWriterThatAppendsAfterIt(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	orig := readBack
	defer func() { readBack = orig }()
	readBack = func(p string) ([]byte, error) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		_, _ = f.WriteString("\nan unrelated raw append\n")
		f.Close()
		return os.ReadFile(p)
	}
	if _, err := rv.AppendNote(lane, "cr-1", "mine", noteAt); err != nil {
		t.Fatalf("the note landed; a later raw append must not fail it: %v", err)
	}
	if got, _ := os.ReadFile(canonical); !strings.Contains(string(got), "mine") {
		t.Error("note missing")
	}
}

// A lane link whose name differs from its target's: the link, when it still resolves, names the
// ticket, not the name the caller typed.
func TestAppendNote_AResolvingLaneLinkNamesTheTicketNotItsOwnName(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	alias := filepath.Join(filepath.Dir(lane), "renamed-link.md")
	if err := os.Symlink("../../../tasks/5-x.md", alias); err != nil {
		t.Fatal(err)
	}
	// A different, unrelated ticket whose name matches the alias: by-name resolution alone would pick it.
	decoy := filepath.Join(rv.TasksDir, "renamed-link.md")
	if err := os.WriteFile(decoy, []byte("---\ntitle: decoy\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.AppendNote(alias, "cr-1", "for the real ticket", noteAt); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(canonical)
	if !strings.Contains(string(got), "for the real ticket") {
		t.Errorf("the note did not reach the ticket the link points at:\n%s", got)
	}
	if d, _ := os.ReadFile(decoy); strings.Contains(string(d), "for the real ticket") {
		t.Error("the note went to a ticket that merely shares the link's name")
	}
}

// The ambient-profile class: the session's tasks dir belongs to project A, the lane path names a
// ticket in project B, and the two projects both have a 5-x.md. The note must go to the ticket the
// caller NAMED, and the path reported must be that file. Before, only the base name was kept and it
// was joined onto A's tasks dir, so B's note landed in A's ticket and the verb reported success.
func TestAppendNote_AResolvingLanePathNamesItsOwnTicketNotThisSessionsSameNamedOne(t *testing.T) {
	rvA, ticketA, _ := noteFixture(t)
	_, ticketB, laneB := noteFixture(t) // a second, unrelated project with its own 5-x.md

	res, err := rvA.AppendNote(laneB, "cr-1", "note meant for B", noteAt)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(ticketB); !strings.Contains(string(got), "note meant for B") {
		t.Errorf("the note did not reach the ticket the lane path names:\n%s", got)
	}
	if got, _ := os.ReadFile(ticketA); strings.Contains(string(got), "note meant for B") {
		t.Error("the note went into this session's same-named ticket instead: the silent wrong-ticket write")
	}
	if !samePath(t, res.Path, ticketB) {
		t.Errorf("the reported path must be the file written, got %s want %s", res.Path, ticketB)
	}
}

// A regular file that is not under this session's queue or tasks dir is not a ticket the verb may
// take on the caller's word, and a path that names neither store cannot be read as this session's
// same-named ticket either: it is refused and nothing is written.
func TestAppendNote_ARegularFileOutsideTheTasksDirIsNeverWrittenTo(t *testing.T) {
	rv, canonical, _ := noteFixture(t)
	elsewhere := filepath.Join(t.TempDir(), "5-x.md")
	if err := os.WriteFile(elsewhere, []byte("not a ticket of this store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(canonical)
	if _, err := rv.AppendNote(elsewhere, "cr-1", "hello", noteAt); err == nil {
		t.Fatal("a path outside this session's queue and tasks dirs must be refused")
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != "not a ticket of this store\n" {
		t.Errorf("a regular file outside the tasks dir was written to: %q", got)
	}
	if got, _ := os.ReadFile(canonical); string(got) != string(before) {
		t.Errorf("this session's ticket was written to:\n%s", got)
	}
}

// The fallback branch's own ambient-profile hole: project B's ticket was routed away (its lane
// symlink is gone), and both projects hold a 5-x.md. The path does not resolve, so it cannot say
// which store it belonged to, but it does say which QUEUE it was under, and that is not this
// session's. Before, it fell back to A's tasks/5-x.md, returned nil, and the note landed in A.
func TestAppendNote_ARoutedAwayPathFromAnotherProjectIsRefused(t *testing.T) {
	rvA, ticketA, laneA := noteFixture(t)
	_, ticketB, laneB := noteFixture(t)
	if err := os.Remove(laneB); err != nil { // B's ticket was routed away
		t.Fatal(err)
	}
	beforeA, _ := os.ReadFile(ticketA)
	beforeB, _ := os.ReadFile(ticketB)

	res, err := rvA.AppendNote(laneB, "cr-1", "note meant for B", noteAt)
	if err == nil || !strings.Contains(err.Error(), "outside this session's queue") {
		t.Fatalf("want a refusal naming the foreign path, got res=%+v err=%v", res, err)
	}
	if got, _ := os.ReadFile(ticketA); string(got) != string(beforeA) {
		t.Errorf("the note went into this session's same-named ticket: the silent wrong-ticket write:\n%s", got)
	}
	if got, _ := os.ReadFile(ticketB); string(got) != string(beforeB) {
		t.Errorf("B's ticket changed:\n%s", got)
	}
	if _, serr := os.Lstat(laneB); !os.IsNotExist(serr) {
		t.Error("the refusal created a file at B's old lane path")
	}
	// A's own lane path is untouched and still names A's ticket.
	if res, err := rvA.AppendNote(laneA, "cr-1", "for A", noteAt); err != nil || !samePath(t, res.Path, ticketA) {
		t.Errorf("A's own lane path stopped working: %+v %v", res, err)
	}
}

// A routed-away path under THIS session's queue is the case the verb exists for and must keep
// working when the queue is reached through a different spelling of the same directory (the macOS
// /var vs /private/var shape): the containment check compares resolved spellings, not strings.
func TestAppendNote_ARoutedAwayPathUnderThisQueueSpelledThroughAnAliasStillFallsBack(t *testing.T) {
	rv, canonical, lane := noteFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(rv.QueueRoot), alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lane); err != nil {
		t.Fatal(err)
	}
	viaAlias := filepath.Join(alias, "queue", "code-review", "in-progress", "5-x.md")
	res, err := rv.AppendNote(viaAlias, "cr-1", "routed, spelled via an alias", noteAt)
	if err != nil || !samePath(t, res.Path, canonical) {
		t.Fatalf("a routed-away path under this queue must fall back by name: %+v %v", res, err)
	}
}

// A bare name carries no directory, so there is nothing to contradict: it names a ticket of this
// session's store.
func TestAppendNote_ABareNameNamesATicketOfThisSession(t *testing.T) {
	rv, canonical, _ := noteFixture(t)
	for _, name := range []string{"5-x", "5-x.md"} {
		res, err := rv.AppendNote(name, "cr-1", "by name "+name, noteAt)
		if err != nil || !samePath(t, res.Path, canonical) {
			t.Errorf("%q: %+v %v", name, res, err)
		}
	}
}

func TestAppendNote_ARegularFileUnderTheTasksDirIsThatTicket(t *testing.T) {
	rv, canonical, _ := noteFixture(t)
	res, err := rv.AppendNote(canonical, "cr-1", "direct", noteAt)
	if err != nil || !samePath(t, res.Path, canonical) {
		t.Fatalf("a tasks/ path names itself: %+v %v", res, err)
	}
}
