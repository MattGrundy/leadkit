package ticketstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const base = "---\nid: 7\ntitle: t\nstatus: pending\ntrackers: {}\n---\n\n# body\n"

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func set(k, v string) Edit { return func(d *Document) error { return d.Set(k, v) } }

func TestSetReplacesInPlaceAndAppendsAfterTheLastField(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", "---\n# keep\nid: 7\ntitle: t\nstatus: pending\n\n---\n\n# body\n")
	if _, err := Update(p, func(d *Document) error {
		if err := d.Set("status", "in_review"); err != nil {
			return err
		}
		return d.Set("sha", "abc123")
	}); err != nil {
		t.Fatal(err)
	}
	want := "---\n# keep\nid: 7\ntitle: t\nstatus: in_review\nsha: abc123\n\n---\n\n# body\n"
	if got := read(t, p); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestSetQuotesWhatWouldNotReadBackAndRefusesWhatCannot(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	for _, v := range []string{"Layer 0: forge", `say "hi" \ bye`, "fix #12", "- dash", "", " padded ", "trailing:", ">folded"} {
		if _, err := Update(p, set("title", v)); err != nil {
			if v == "" {
				continue // an empty title leaves the ticket without a required field, and is refused
			}
			t.Errorf("Set(%q): %v", v, err)
			continue
		}
		if got := ReadFile(p).Title; got != v {
			t.Errorf("Set(%q) read back as %q", v, got)
		}
	}
	before := read(t, p)
	if _, err := Update(p, set("title", "two\nlines")); err == nil {
		t.Error("a multi-line value must be refused")
	}
	if _, err := Update(p, set("trackers", "x")); err == nil {
		t.Error("setting a scalar over a map key must be refused")
	}
	if _, err := Update(p, set("bad key", "x")); err == nil {
		t.Error("an invalid key must be refused")
	}
	if _, err := Update(p, set("title", "")); err == nil {
		t.Error("blanking a required field must be refused")
	}
	if read(t, p) != before {
		t.Error("a refused write changed the file")
	}
}

// The validator refuses to write a document that still contains a form the reader cannot
// round-trip, even when the edit itself is unrelated.
func TestUpdateRefusesADocumentWithNonCanonicalForms(t *testing.T) {
	for name, src := range map[string]string{
		"folded title":       "---\nid: 7\ntitle: >\n  a\n  b\nstatus: pending\n---\n",
		"block-list blocker": "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on:\n  - a\n---\n",
		"no frontmatter":     "just text\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := put(t, t.TempDir(), "7-t.md", src)
			_, err := Update(p, set("priority", "P1"))
			if err == nil {
				t.Fatal("the write was accepted")
			}
			if read(t, p) != src {
				t.Error("a refused write changed the file")
			}
		})
	}
	// Fixing the form is itself an allowed write.
	p := put(t, t.TempDir(), "7-t.md", "---\nid: 7\ntitle: >\n  a\n  b\nstatus: pending\n---\n")
	if _, err := Update(p, set("title", "a b")); err != nil {
		t.Fatalf("rewriting a folded title as a single line must be accepted: %v", err)
	}
	if got := read(t, p); got != "---\nid: 7\ntitle: a b\nstatus: pending\n---\n" {
		t.Errorf("got %q", got)
	}
}

func TestUpdateThatChangesNothingDoesNotWrite(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	want, _ := os.Stat(p)
	if _, err := Update(p, set("status", "pending")); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(p, func(*Document) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); !info.ModTime().Equal(want.ModTime()) {
		t.Error("a no-op Update rewrote the file")
	}
}

// The lost update: another writer changes the file between our read and our commit.
// The compare-and-swap must notice, discard its stale result, and redo the edit on the new bytes.
func TestUpdateRetriesWhenTheFileChangedUnderIt(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	raced := 0
	beforeCommit = func(path string) {
		if raced > 0 {
			return
		}
		raced++
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		f.WriteString("\n## reviewer\n\nthe note another writer appended in place\n")
		f.Close()
	}
	t.Cleanup(func() { beforeCommit = func(string) {} })

	if _, err := Update(p, set("status", "in_review")); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	if !strings.Contains(got, "the note another writer appended in place") {
		t.Errorf("the concurrent note was lost:\n%s", got)
	}
	if ReadFile(p).Status != "in_review" {
		t.Errorf("our edit was lost:\n%s", got)
	}
}

func TestUpdateGivesUpWithErrConflictWhenTheFileNeverSettles(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	n := 0
	beforeCommit = func(path string) {
		n++
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		fmt.Fprintf(f, "churn %d\n", n)
		f.Close()
	}
	t.Cleanup(func() { beforeCommit = func(string) {} })
	_, err := Update(p, set("status", "x"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if n != maxAttempts {
		t.Errorf("tried %d times, want %d", n, maxAttempts)
	}
	if strings.Contains(read(t, p), "status: x") {
		t.Error("a write that lost every race still landed")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".*.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// Many writers, each a read-modify-write of one file, as `task note` and `task set` are. Every
// one of their changes must be in the result.
func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Update(p, func(d *Document) error {
				d.AppendBody(fmt.Sprintf("note %d\n", i))
				return d.Set(fmt.Sprintf("k%d", i), "v")
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := read(t, p)
	tk := ReadFile(p)
	for i := 0; i < writers; i++ {
		if !strings.Contains(got, fmt.Sprintf("note %d\n", i)) {
			t.Errorf("note %d was lost", i)
		}
		if v, _ := tk.Doc.Lookup(fmt.Sprintf("k%d", i)); v != "v" {
			t.Errorf("field k%d was lost", i)
		}
	}
	if len(tk.Findings) != 0 {
		t.Errorf("the file is no longer well formed: %v", tk.Findings)
	}
}

func TestUpdateThroughASymlinkReplacesTheTicketNotTheLink(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks")
	lane := filepath.Join(dir, "queue", "developer", "pending")
	for _, d := range []string{store, lane} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := put(t, store, "7-t.md", base)
	link := filepath.Join(lane, "7-t.md")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(link, set("status", "in_progress")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the lane symlink was replaced by a regular file: %v", err)
	}
	if ReadFile(real).Status != "in_progress" {
		t.Error("the ticket in the store was not updated")
	}
}

func TestUpdateKeepsTheFileMode(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", base)
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(p, set("status", "done")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestUpdateOfAMissingFileIsAnError(t *testing.T) {
	if _, err := Update(filepath.Join(t.TempDir(), "nope.md"), set("a", "b")); err == nil {
		t.Error("want an error")
	}
}

func TestAppendBodyStartsOnAFreshLine(t *testing.T) {
	p := put(t, t.TempDir(), "7-t.md", "---\nid: 7\ntitle: t\nstatus: pending\n---\nno trailing newline")
	if _, err := Update(p, func(d *Document) error { d.AppendBody("\n## note\n"); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "---\nid: 7\ntitle: t\nstatus: pending\n---\nno trailing newline\n\n## note\n" {
		t.Errorf("got %q", got)
	}
}

func TestUpdateFixesADuplicateKeyFile(t *testing.T) {
	src := "---\nid: 7\ntitle: t\nstatus: pending\nstatus: done\n---\n\n# body\n"
	p := put(t, t.TempDir(), "7-t.md", src)
	if _, err := Update(p, func(*Document) error { return nil }); err == nil {
		t.Error("an Update of a file with a Finding was accepted")
	}
	if read(t, p) != src {
		t.Fatal("a refused write changed the file")
	}
	if _, err := Update(p, set("status", "pending")); err != nil {
		t.Fatalf("setting the repeated key must drop its duplicates: %v", err)
	}
	if got := read(t, p); got != "---\nid: 7\ntitle: t\nstatus: pending\n---\n\n# body\n" {
		t.Errorf("got %q", got)
	}
}
