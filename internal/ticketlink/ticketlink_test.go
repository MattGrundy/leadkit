package ticketlink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/ticketstore"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
)

func TestWithTrailers(t *testing.T) {
	const m = "Leadkit-Merge: pr-merge"
	for name, tc := range map[string]struct{ body, id, want string }{
		"empty body, no ticket":  {"", "", m},
		"empty body, ticket":     {"", "265", m + "\nTicket: 265"},
		"body, no ticket":        {"* one\n\n* two", "", "* one\n\n* two\n\n" + m},
		"body, ticket":           {"* one\n\n* two", "265", "* one\n\n* two\n\n" + m + "\nTicket: 265"},
		"trailing newlines":      {"* one\n\n", "265", "* one\n\n" + m + "\nTicket: 265"},
		"already present":        {"* one\n\n" + m + "\nTicket: 265", "265", "* one\n\n" + m + "\nTicket: 265"},
		"marker present, ticket": {"* one\n\n" + m, "265", "* one\n\n" + m + "\n\nTicket: 265"},
	} {
		if got := WithTrailers(tc.body, tc.id); got != tc.want {
			t.Errorf("%s: WithTrailers(%q, %q) = %q, want %q", name, tc.body, tc.id, got, tc.want)
		}
	}
	// a different ticket's line is not this one's
	if got := WithTrailers("x\n\nTicket: 26", "265"); !strings.HasSuffix(got, "\nTicket: 265") {
		t.Errorf("a prefix of the id must not count as the trailer: %q", got)
	}
	if TrailerBlock("") != "Leadkit-Merge: pr-merge" || TrailerBlock("7") != "Leadkit-Merge: pr-merge\nTicket: 7" {
		t.Errorf("TrailerBlock: %q %q", TrailerBlock(""), TrailerBlock("7"))
	}
}

func TestHasMarker(t *testing.T) {
	if !HasMarker("subject\n\nLeadkit-Merge: pr-merge\nCo-authored-by: x <x@y>") {
		t.Error("a marker followed by a forge's own trailer must still be found")
	}
	for _, msg := range []string{"", "Leadkit-Merge: other", "not a Leadkit-Merge: pr-merge", "Leadkit-Merge:pr-merge"} {
		if HasMarker(msg) {
			t.Errorf("%q must not read as marked", msg)
		}
	}
}

func TestHasTrailer(t *testing.T) {
	if !HasTrailer("subject\n\nTicket: 265\nCo-authored-by: x <x@y>", "265") {
		t.Error("a trailer followed by a forge's own trailer must still be found")
	}
	for _, msg := range []string{"", "Ticket: 2650", "Ticket: 26", "not a Ticket: 265", "Ticket:265"} {
		if HasTrailer(msg, "265") {
			t.Errorf("%q must not read as carrying Ticket: 265", msg)
		}
	}
}

// store builds a store with the given files and returns its root.
func store(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func ticket(id, status, extra string) string {
	return "---\nid: " + id + "\ntitle: A ticket\nstatus: " + status + "\ntype: developer\n" + extra + "trackers: {}\n---\n\nthe body\n\n## lead\n\na note\n"
}

func TestResolve(t *testing.T) {
	root := store(t, map[string]string{
		"265-the-ticket.md":  ticket("265", "gated", ""),
		"7-a.md":             ticket("7", "pending", ""),
		"7-b.md":             ticket("7", "pending", ""),
		"9-mismatch.md":      ticket("10", "pending", ""),
		"11-noncanonical.md": "---\nid: 11\ntitle: t\nstatus: pending\nsha: >\n  folded\n---\n",
	})
	lane := filepath.Join(t.TempDir(), "queue", "developer", "pending")
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "265-the-ticket.md"), filepath.Join(lane, "265-the-ticket.md")); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "265-the-ticket.md"))

	for _, arg := range []string{"265", "265-the-ticket", "265-the-ticket.md", filepath.Join(root, "265-the-ticket.md"), filepath.Join(lane, "265-the-ticket.md")} {
		ref, err := Resolve(arg, root)
		if err != nil || ref.ID != "265" || ref.Path != want {
			t.Errorf("Resolve(%q) = %+v, %v; want id 265 at %s", arg, ref, err, want)
		}
	}
	for arg, why := range map[string]string{
		"":                  "no ticket",
		"404":               "no ticket 404",
		"7":                 "ambiguous",
		"9-mismatch":        "refusing to guess",
		"11-noncanonical":   "cannot be written",
		"/etc/hosts":        "not a ticket directly under the store",
		"265-the-ticket/..": "not found",
	} {
		if _, err := Resolve(arg, root); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("Resolve(%q) = %v; want a refusal mentioning %q", arg, err, why)
		}
	}
}

const (
	pr1 = "https://github.com/MattGrundy/leadkit/pull/110"
	pr2 = "https://github.com/MattGrundy/leadkit/pull/111"
)

func load(t *testing.T, ref Ref) *ticketstore.Document {
	t.Helper()
	data, _ := os.ReadFile(ref.Path)
	doc := ticketstore.Parse(ref.Path, string(data))
	if len(doc.Findings) > 0 {
		t.Fatalf("the written ticket must be well formed: %v\n%s", doc.Findings, data)
	}
	return doc
}

func TestRecordMerge_RecordsTheShaAndNeverTouchesTheStatus(t *testing.T) {
	root := store(t, map[string]string{"265-t.md": ticket("265", "gated", "sha: "+sha1+"\nstage: finalise\n")})
	ref, err := Resolve("265", root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(ref.Path)
	ch, err := RecordMerge(ref, sha2, sha3, pr1)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Status != "gated" || ch.OldSHA != sha1 || ch.NewSHA != sha2 || ch.OldPR != "" || ch.NewPR != pr1 {
		t.Errorf("Change = %+v", ch)
	}
	doc := load(t, ref)
	for k, want := range map[string]string{"status": "gated", "sha": sha2, "reviewed_sha": sha3, "stage": "finalise", "title": "A ticket", "id": "265"} {
		if got := doc.Value(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, _ := doc.Lookup("trackers", "pr"); got != pr1 {
		t.Errorf("trackers.pr = %q, want %q", got, pr1)
	}
	after, _ := os.ReadFile(ref.Path)
	if strings.Contains(string(after), "status: done") || strings.Contains(string(after), "status: completed") {
		t.Errorf("a merge records a fact and must not close the ticket:\n%s", after)
	}
	if !strings.Contains(string(after), "the body\n\n## lead\n\na note\n") || len(after) <= len(before) {
		t.Errorf("the body and its notes must be untouched:\n%s", after)
	}
}

func TestRecordMerge_WithNoPRURLLeavesTrackersAlone(t *testing.T) {
	root := store(t, map[string]string{"1-t.md": ticket("1", "in_review", "")})
	ref, _ := Resolve("1", root)
	ch, err := RecordMerge(ref, sha2, "", "")
	if err != nil || ch.NewPR != "" {
		t.Fatalf("%+v %v", ch, err)
	}
	doc := load(t, ref)
	if doc.Value("sha") != sha2 || doc.Value("status") != "in_review" {
		t.Errorf("sha not recorded or status moved: %+v", doc.Fields)
	}
	if _, ok := doc.Get("reviewed_sha"); ok {
		t.Errorf("with no reviewed head given there is nothing to record as reviewed_sha")
	}
	if _, ok := doc.Lookup("trackers", "pr"); ok {
		t.Errorf("no PR URL was given, so none may be recorded")
	}
}

func TestRecordMerge_RefusesAShaThatIsNotEvidence(t *testing.T) {
	root := store(t, map[string]string{"1-t.md": ticket("1", "gated", "")})
	ref, _ := Resolve("1", root)
	before, _ := os.ReadFile(ref.Path)
	for _, bad := range []string{"", "abc1234", strings.Repeat("g", 40)} {
		if _, err := RecordMerge(ref, bad, "", ""); err == nil {
			t.Errorf("RecordMerge(%q) must be refused", bad)
		}
	}
	if _, err := RecordMerge(ref, sha2, "", "not a url"); err == nil {
		t.Error("a PR that is not a URL must be refused")
	}
	if after, _ := os.ReadFile(ref.Path); string(after) != string(before) {
		t.Fatalf("a refused record must leave the ticket alone:\n%s", after)
	}
}

func TestRecordMerge_IsIdempotent(t *testing.T) {
	root := store(t, map[string]string{"1-t.md": ticket("1", "gated", "sha: "+sha1+"\n")})
	ref, _ := Resolve("1", root)
	if _, err := RecordMerge(ref, sha2, sha3, pr1); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(ref.Path)
	if _, err := RecordMerge(ref, sha2, sha3, pr1); err != nil {
		t.Fatal(err)
	}
	if second, _ := os.ReadFile(ref.Path); string(first) != string(second) {
		t.Errorf("recording the same merge twice must not change the file:\n%s\n---\n%s", first, second)
	}
}

func TestRecordPR_RecordsTheURLAndKeepsTheLatest(t *testing.T) {
	root := store(t, map[string]string{"1-t.md": ticket("1", "pending", "")})
	ref, _ := Resolve("1", root)
	ch, err := RecordPR(ref, pr1)
	if err != nil || ch.OldPR != "" || ch.NewPR != pr1 || ch.Status != "pending" {
		t.Fatalf("%+v %v", ch, err)
	}
	doc := load(t, ref)
	if got, _ := doc.Lookup("trackers", "pr"); got != pr1 || doc.Value("status") != "pending" {
		t.Errorf("trackers.pr = %q, status = %q", got, doc.Value("status"))
	}
	// a rework opens a second PR: the ticket holds the latest, and says what it replaced
	ch, err = RecordPR(ref, pr2)
	if err != nil || ch.OldPR != pr1 || ch.NewPR != pr2 {
		t.Fatalf("%+v %v", ch, err)
	}
	if got, _ := load(t, ref).Lookup("trackers", "pr"); got != pr2 {
		t.Errorf("trackers.pr = %q, want the latest %q", got, pr2)
	}
}

func TestRecordPR_KeepsOtherTrackers(t *testing.T) {
	root := store(t, map[string]string{"1-t.md": "---\nid: 1\ntitle: t\nstatus: pending\ntrackers: {linear: ABC-1}\n---\n"})
	ref, _ := Resolve("1", root)
	if _, err := RecordPR(ref, pr1); err != nil {
		t.Fatal(err)
	}
	doc := load(t, ref)
	if a, _ := doc.Lookup("trackers", "linear"); a != "ABC-1" {
		t.Errorf("an existing tracker was lost")
	}
	if b, _ := doc.Lookup("trackers", "pr"); b != pr1 {
		t.Errorf("pr not recorded")
	}
}

func TestPreflightPR_RefusesATrackersFieldItCannotWriteInto(t *testing.T) {
	root := store(t, map[string]string{
		"1-t.md": ticket("1", "pending", ""),
		"2-t.md": "---\nid: 2\ntitle: t\nstatus: pending\n---\n",
		"3-t.md": "---\nid: 3\ntitle: t\nstatus: pending\ntrackers:\n  linear: ABC-1\n---\n",
	})
	for id, wantErr := range map[string]bool{"1": false, "2": false, "3": true} {
		ref, err := Resolve(id, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := PreflightPR(ref); (err != nil) != wantErr {
			t.Errorf("ticket %s: PreflightPR = %v, want error=%v", id, err, wantErr)
		}
	}
}

func TestFindByPR_IsAnExactMatchOnTheWholeURL(t *testing.T) {
	root := store(t, map[string]string{
		"1-t.md": ticket("1", "pending", ""),
		"2-t.md": "---\nid: 2\ntitle: t\nstatus: gated\ntrackers: {pr: " + pr1 + "}\n---\n",
		"3-t.md": "---\nid: 3\ntitle: t\nstatus: gated\ntrackers: {pr: " + pr2 + "}\n---\n",
		"4-t.md": "---\nid: 4\ntitle: t\nstatus: gated\ntrackers: {pr: " + pr1 + "0}\n---\nmentions " + pr1 + " in prose\n",
		"5-t.md": "---\nid: 5\ntitle: t\nstatus: gated\ntrackers: {linear: " + pr1 + "}\n---\n",
	})
	refs, err := FindByPR(root, pr1)
	if err != nil || len(refs) != 1 || refs[0].ID != "2" {
		t.Fatalf("want exactly ticket 2 (not 3, a different PR; not 4, a longer URL and a mention; not 5, another tracker), got %+v %v", refs, err)
	}
	if refs, err := FindByPR(root, "https://github.com/MattGrundy/leadkit/pull/999"); err != nil || len(refs) != 0 {
		t.Errorf("an unrecorded PR matches nothing: %+v %v", refs, err)
	}
	if refs, err := FindByPR(filepath.Join(root, "missing"), pr1); err != nil || len(refs) != 0 {
		t.Errorf("a store with no directory has no tickets, and so no claimants: %+v %v", refs, err)
	}
	if refs, _ := FindByPR(root, "not a url"); len(refs) != 0 {
		t.Errorf("a non-URL must match nothing, got %+v", refs)
	}
}

func TestExtractPRURL(t *testing.T) {
	for out, want := range map[string]string{
		"https://github.com/o/r/pull/110\n": "https://github.com/o/r/pull/110",
		"Creating pull request for a into b in o/r\nsee https://github.com/o/r/pull/3 and\nhttps://github.com/o/r/pull/110\n": "https://github.com/o/r/pull/110",
		"https://gitlab.com/g/p/-/merge_requests/7": "https://gitlab.com/g/p/-/merge_requests/7",
		"no url here":                     "",
		"https://github.com/o/r/issues/4": "",
	} {
		if got := ExtractPRURL(out); got != want {
			t.Errorf("ExtractPRURL(%q) = %q, want %q", out, got, want)
		}
	}
	if !IsPRURL("https://github.com/o/r/pull/1") || IsPRURL("https://github.com/o/r/pull/1/files") || IsPRURL("") {
		t.Error("IsPRURL must accept exactly one bare PR URL")
	}
}

// A claimant that cannot be written still CLAIMS the PR. Dropping it from the count is how a
// two-claimant refusal fails open: one tidy ticket plus one malformed one read as a sole claimant.
func TestFindByPR_CountsAClaimantThatCannotBeResolved(t *testing.T) {
	root := store(t, map[string]string{
		"2-t.md": "---\nid: 2\ntitle: t\nstatus: gated\ntrackers: {pr: " + pr1 + "}\n---\n",
		"3-t.md": "---\nid: 99\ntitle: t\nstatus: gated\ntrackers: {pr: " + pr1 + "}\n---\n",                  // id disagrees with its file name
		"4-t.md": "---\nid: 4\ntitle: t\nstatus: gated\nsha: >\n  folded\ntrackers: {pr: " + pr1 + "}\n---\n", // a finding
	})
	claims, err := FindByPR(root, pr1)
	if err != nil || len(claims) != 3 {
		t.Fatalf("want all three claimants counted, got %+v %v", claims, err)
	}
	bad := map[string]bool{}
	for _, c := range claims {
		if c.Err != nil {
			bad[c.ID] = true
		}
	}
	if len(bad) != 2 || !bad["3"] || !bad["4"] || claims[0].Err != nil {
		t.Errorf("tickets 3 and 4 must carry why they cannot be written, and 2 must not: %+v", claims)
	}
}

func TestFindByPR_AFileItCannotReadIsAnErrorNotAbsence(t *testing.T) {
	root := store(t, map[string]string{"2-t.md": ticket("2", "gated", "")})
	p := filepath.Join(root, "2-t.md")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o644)
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("file permissions are not enforced here (running as root?)")
	}
	if claims, err := FindByPR(root, pr1); err == nil {
		t.Errorf("a ticket that cannot be read may be a claimant: that is an error, got %+v", claims)
	}
}
