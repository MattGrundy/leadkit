package ticketstore

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func put(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func ticket(t *testing.T, content string) *Ticket {
	t.Helper()
	return ReadFile(put(t, t.TempDir(), "7-t.md", content))
}

func codes(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func hasCode(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

// A folded title is read as its text, not as the single character ">".
func TestFoldedTitleIsReadAsItsText(t *testing.T) {
	cases := map[string]string{
		"folded":       "---\nid: 7\ntitle: >\n  The long title that wraps\n  over two lines\nstatus: pending\n---\n",
		"folded strip": "---\nid: 7\ntitle: >-\n  The long title that wraps\n  over two lines\nstatus: pending\n---\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			tk := ticket(t, src)
			if want := "The long title that wraps over two lines"; tk.Title != want {
				t.Errorf("Title = %q, want %q", tk.Title, want)
			}
			if tk.Status != "pending" {
				t.Errorf("Status = %q: the key after a folded scalar must still be read", tk.Status)
			}
		})
	}
	lit := ticket(t, "---\nid: 7\ntitle: |\n  line one\n  line two\nstatus: pending\n---\n")
	if want := "line one\nline two"; lit.Title != want {
		t.Errorf("literal Title = %q, want %q", lit.Title, want)
	}
}

// A nested `trackers.github.pr` is not the ticket's own top-level `pr`.
func TestNestedTrackerPRIsNotTheTicketsPR(t *testing.T) {
	src := "---\nid: 7\ntitle: t\nstatus: pending\ntrackers:\n  github:\n    pr: 41\n    issue: 9\n---\n"
	tk := ticket(t, src)
	if tk.PR != "" {
		t.Errorf("PR = %q: a nested trackers.github.pr leaked into the ticket's own pr", tk.PR)
	}
	if got, ok := tk.Doc.Lookup("trackers", "github", "pr"); !ok || got != "41" {
		t.Errorf("Lookup(trackers.github.pr) = %q, %v; want 41", got, ok)
	}
	if got, ok := tk.Doc.Lookup("trackers", "github", "issue"); !ok || got != "9" {
		t.Errorf("a second nested key was lost: %q, %v", got, ok)
	}
	if len(tk.Findings) != 0 {
		t.Errorf("a nested trackers map is valid and must not be a finding: %v", tk.Findings)
	}

	both := ticket(t, "---\nid: 7\ntitle: t\nstatus: pending\npr: 12\ntrackers:\n  github:\n    pr: 41\n---\n")
	if both.PR != "12" {
		t.Errorf("PR = %q, want the top-level 12, not the nested 41", both.PR)
	}
	flow := ticket(t, "---\nid: 7\ntitle: t\nstatus: pending\ntrackers: {}\n---\n")
	if len(flow.Findings) != 0 || flow.PR != "" {
		t.Errorf("`trackers: {}` is the common empty form: findings %v pr %q", flow.Findings, flow.PR)
	}
}

// A block-list blocked_on yields its items.
func TestBlockListBlockedOnYieldsItsItems(t *testing.T) {
	tk := ticket(t, "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on:\n  - ticket 102\n  - \"owner decision\"\n  - {ticket: 103}\n---\n")
	want := []string{"ticket 102", "owner decision", "{ticket: 103}"}
	if !reflect.DeepEqual(tk.BlockedOn, want) {
		t.Errorf("BlockedOn = %q, want %q", tk.BlockedOn, want)
	}
	col0 := ticket(t, "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on:\n- a\n- b\npriority: P1\n---\n")
	if !reflect.DeepEqual(col0.BlockedOn, []string{"a", "b"}) || col0.Priority != "P1" {
		t.Errorf("column-0 list: BlockedOn %q Priority %q", col0.BlockedOn, col0.Priority)
	}
	one := ticket(t, "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on: \"ticket 63 landing\"\n---\n")
	if !reflect.DeepEqual(one.BlockedOn, []string{"ticket 63 landing"}) {
		t.Errorf("scalar BlockedOn = %q", one.BlockedOn)
	}
	cleared := ticket(t, "---\nid: 7\ntitle: t\nstatus: pending\nblocked_on: \"\"  # cleared 2026-10-04: decided\n---\n")
	if len(cleared.BlockedOn) != 0 || len(cleared.Findings) != 0 {
		t.Errorf("a cleared blocker with a trailing comment: BlockedOn %q findings %v", cleared.BlockedOn, cleared.Findings)
	}
}

// The reader is faithful, and the validator is what refuses: each misread form is also a finding,
// so it cannot be written back.
func TestMisreadFormsAreNonCanonicalFindings(t *testing.T) {
	for name, src := range map[string]string{
		"folded title":         "---\nid: 7\ntitle: >\n  a\n  b\nstatus: pending\n---\n",
		"literal title":        "---\nid: 7\ntitle: |\n  a\nstatus: pending\n---\n",
		"block-list blocker":   "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on:\n  - a\n---\n",
		"flow-list blocker":    "---\nid: 7\ntitle: t\nstatus: blocked\nblocked_on: [a, b]\n---\n",
		"continued title":      "---\nid: 7\ntitle: one\n  two\nstatus: pending\n---\n",
		"trackers as a scalar": "---\nid: 7\ntitle: t\nstatus: pending\ntrackers: nope\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			tk := ticket(t, src)
			if !hasCode(tk.Findings, CodeNonCanonical) {
				t.Errorf("want a %s finding, got %v", CodeNonCanonical, tk.Findings)
			}
		})
	}
}

func TestQuotingAndCommentsAreReadAsWritten(t *testing.T) {
	tk := ticket(t, strings.Join([]string{
		"---",
		"id: 7",
		`title: "Trim PR #11's \"comments\" \\ done"`,
		"status: 'pen''ding'",
		"type: developer # the kind of work",
		"priority: P1",
		"---", "",
	}, "\n"))
	if tk.Title != `Trim PR #11's "comments" \ done` {
		t.Errorf("Title = %q", tk.Title)
	}
	if tk.Status != "pen'ding" {
		t.Errorf("Status = %q", tk.Status)
	}
	// A plain value keeps ` #...`: titles say "#123", and every other reader of these files keeps it.
	plain := ticket(t, "---\nid: 7\ntitle: Record the surviving #123 prose hits\nstatus: pending\n---\n")
	if plain.Title != "Record the surviving #123 prose hits" {
		t.Errorf("plain Title = %q", plain.Title)
	}
	colon := ticket(t, "---\nid: 7\ntitle: Layer 0: forge-side protection\nstatus: pending\n---\n")
	if colon.Title != "Layer 0: forge-side protection" || len(colon.Findings) != 0 {
		t.Errorf("a title with a colon: %q, %v", colon.Title, colon.Findings)
	}
}

func TestMalformedValuesAreFindingsNotPanics(t *testing.T) {
	for name, src := range map[string]string{
		"unterminated quote": "---\nid: 7\ntitle: \"never closed\nstatus: pending\n---\n",
		"junk after quote":   "---\nid: 7\ntitle: \"closed\" junk\nstatus: pending\n---\n",
		"bare word line":     "---\nid: 7\ntitle: t\nstatus: pending\nwhat\n---\n",
		"indented orphan":    "---\n  stray: 1\nid: 7\ntitle: t\nstatus: pending\n---\n",
		"unterminated flow":  "---\nid: 7\ntitle: t\nstatus: pending\nx: [a, b\n---\n",
		"duplicate key":      "---\nid: 7\ntitle: t\nstatus: pending\nstatus: done\n---\n",
		"non-numeric id":     "---\nid: seven\ntitle: t\nstatus: pending\n---\n",
		"missing status":     "---\nid: 7\ntitle: t\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			tk := ticket(t, src)
			if len(tk.Findings) == 0 {
				t.Errorf("no finding for %s", name)
			}
		})
	}
	dup := ticket(t, "---\nid: 7\ntitle: t\nstatus: pending\nstatus: done\n---\n")
	if dup.Status != "pending" {
		t.Errorf("a repeated key reads as its first value, got %q", dup.Status)
	}
}

// A file with no frontmatter must be reported, never silently skipped.
func TestNoFrontmatterIsAFindingAndStaysInTheSet(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "1-ok.md", "---\nid: 1\ntitle: ok\nstatus: pending\n---\nbody\n")
	put(t, dir, "2-bare.md", "# just a heading\n\nno frontmatter at all\n")
	put(t, dir, "3-open.md", "---\nid: 3\ntitle: never closes\nstatus: pending\n")
	put(t, dir, "4-empty.md", "")
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tickets) != 4 {
		t.Fatalf("loaded %d tickets from 4 files: a file was dropped", len(l.Tickets))
	}
	byBase := map[string]*Ticket{}
	for _, tk := range l.Tickets {
		byBase[filepath.Base(tk.Path)] = tk
	}
	for name, code := range map[string]string{"2-bare.md": CodeNoFrontmatter, "3-open.md": CodeUnclosed, "4-empty.md": CodeNoFrontmatter} {
		if !hasCode(byBase[name].Findings, code) {
			t.Errorf("%s: want %s, got %v", name, code, byBase[name].Findings)
		}
		if !hasCode(l.Findings, code) {
			t.Errorf("the store-wide findings lack %s", code)
		}
	}
	if len(byBase["1-ok.md"].Findings) != 0 {
		t.Errorf("a clean ticket picked up findings: %v", byBase["1-ok.md"].Findings)
	}
}

// `id` is not unique in practice: the path is the key and a clash is a finding on both files.
func TestDuplicateIDIsAFindingOnBothFilesNotAnError(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "5-first.md", "---\nid: 5\ntitle: first\nstatus: pending\n---\n")
	put(t, dir, "5-second.md", "---\nid: 5\ntitle: second\nstatus: pending\n---\n")
	put(t, dir, "6-other.md", "---\nid: 6\ntitle: other\nstatus: pending\n---\n")
	l, err := Load(dir)
	if err != nil {
		t.Fatalf("a duplicate id must not be an error: %v", err)
	}
	if len(l.Tickets) != 3 {
		t.Fatalf("loaded %d of 3: both files of a duplicate pair must survive", len(l.Tickets))
	}
	for _, tk := range l.Tickets {
		dup := hasCode(tk.Findings, CodeDuplicateID)
		if want := strings.HasPrefix(filepath.Base(tk.Path), "5-"); dup != want {
			t.Errorf("%s: duplicate-id finding = %v, want %v", filepath.Base(tk.Path), dup, want)
		}
	}
	if tk := l.Tickets[0]; tk.Title != "first" || l.Tickets[1].Title != "second" {
		t.Errorf("the pair was merged or reordered: %q, %q", tk.Title, l.Tickets[1].Title)
	}
	var msg string
	for _, f := range l.Tickets[0].Findings {
		msg += f.Msg
	}
	if !strings.Contains(msg, "5-second.md") {
		t.Errorf("the finding must name the other file: %q", msg)
	}
}

func TestIDDisagreeingWithTheFileNameIsAFinding(t *testing.T) {
	tk := ReadFile(put(t, t.TempDir(), "9-x.md", "---\nid: 10\ntitle: t\nstatus: pending\n---\n"))
	if !hasCode(tk.Findings, CodeIDMismatch) {
		t.Errorf("want %s, got %v", CodeIDMismatch, tk.Findings)
	}
}

func TestLoadReportsAnUnreadableFileAndSkipsNonTickets(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "1-ok.md", "---\nid: 1\ntitle: ok\nstatus: pending\n---\n")
	put(t, dir, ".1-ok.md.tmp-123", "---\nid: 1\n")
	put(t, dir, "notes.txt", "x")
	put(t, dir, ".hidden.md", "x")
	if err := os.Mkdir(filepath.Join(dir, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone.md"), filepath.Join(dir, "2-dangling.md")); err != nil {
		t.Fatal(err)
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tickets) != 2 {
		t.Fatalf("got %d tickets, want 1 clean + 1 dangling: %v", len(l.Tickets), l.Tickets)
	}
	if !hasCode(l.Tickets[1].Findings, CodeUnreadable) {
		t.Errorf("a dangling symlink must be reported: %v", codes(l.Tickets[1].Findings))
	}
	if _, err := Load(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing store directory is an error")
	}
}

func TestParseRenderIsTheIdentityWhenNothingChanged(t *testing.T) {
	for name, src := range map[string]string{
		"plain":      "---\nid: 7\ntitle: t\nstatus: pending\n---\n\n# body\n",
		"crlf":       "---\r\nid: 7\r\ntitle: t\r\nstatus: pending\r\n---\r\nbody\r\n",
		"no newline": "---\nid: 7\ntitle: t\nstatus: pending\n---",
		"comments":   "---\n# a comment\nid: 7\n\ntitle: t\nstatus: pending\ntrackers:\n  github:\n    pr: 4\n---\nbody with --- inside\n---\n",
		"folded":     "---\nid: 7\ntitle: >\n  a\n  b\nstatus: pending\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := Parse("x", src).Render(); got != src {
				t.Errorf("Render changed an untouched file:\n got %q\nwant %q", got, src)
			}
		})
	}
}

func TestLoadReadsEveryFileOfAnArbitraryStoreWithoutLoss(t *testing.T) {
	dir := t.TempDir()
	const n = 300
	for i := 1; i <= n; i++ {
		var src string
		switch i % 5 {
		case 0:
			src = "no frontmatter\n"
		case 1:
			src = fmt.Sprintf("---\nid: %d\ntitle: >\n  folded %d\nstatus: pending\n---\n", i, i)
		case 2:
			src = fmt.Sprintf("---\nid: %d\ntitle: t\nstatus: blocked\nblocked_on:\n  - x\ntrackers:\n  github:\n    pr: %d\n---\n", i, i)
		default:
			src = fmt.Sprintf("---\nid: %d\ntitle: t\nstatus: pending\n---\n", i)
		}
		put(t, dir, fmt.Sprintf("%d-t.md", i), src)
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tickets) != n {
		t.Fatalf("loaded %d of %d", len(l.Tickets), n)
	}
}

// The live store is gitignored working state, so this runs only where one is named. With
// LEADKIT_LIVE_TASKS=<tasks dir> it reads every ticket in it and fails on any that is lost or
// carries a finding.
func TestLiveStoreIsReadWithoutLoss(t *testing.T) {
	dir := os.Getenv("LEADKIT_LIVE_TASKS")
	if dir == "" {
		t.Skip("set LEADKIT_LIVE_TASKS to a tasks/ directory to read a live store")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			files++
		}
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tickets) != files {
		t.Fatalf("read %d tickets from %d files", len(l.Tickets), files)
	}
	for _, f := range l.Findings {
		t.Errorf("finding: %s", f)
	}
	for _, tk := range l.Tickets {
		if tk.ID == "" || tk.Title == "" || tk.Status == "" {
			t.Errorf("%s: lost a required field (id %q title %q status %q)", tk.Path, tk.ID, tk.Title, tk.Status)
		}
	}
	t.Logf("read %d tickets, %d findings", len(l.Tickets), len(l.Findings))
}
