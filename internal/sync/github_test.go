package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/hierarchy"
)

// fakeGitHub is an in-memory stand-in for gh's issue API. It records every call so
// tests assert on what was SENT, not on what the executor says it did.
type fakeGitHub struct {
	issues map[int]*fakeIssue
	next   int
	lag    int      // the listing hides this many of the newest issues, as GitHub's replica does
	calls  []string // "GET list" | "GET #n" | "POST create" | "PATCH #n"
	posts  []map[string]any
	patchs map[int][]map[string]any
	public bool   // the repo is public: the visibility check must refuse it before any other call
	vis    string // when set, the raw answer to the visibility query (an unreadable or odd one)
}

type fakeIssue struct {
	Number      int
	Title, Body string
	State       string
	StateReason string
	Labels      []string
	IsPR        bool
}

func newFake() *fakeGitHub {
	return &fakeGitHub{issues: map[int]*fakeIssue{}, next: 1, patchs: map[int][]map[string]any{}}
}

func (f *fakeGitHub) writes() int {
	n := 0
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET") {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	if len(args) < 2 || args[0] != "api" {
		return nil, fmt.Errorf("fake gh: unexpected args %v", args)
	}
	joined := strings.Join(args, " ")
	switch {
	case len(args) == 4 && args[2] == "--jq" && args[3] == ".private": // the visibility check
		f.calls = append(f.calls, "GET visibility")
		if f.vis != "" {
			return []byte(f.vis), nil
		}
		if f.public {
			return []byte("false\n"), nil
		}
		return []byte("true\n"), nil
	case strings.Contains(joined, "--paginate"):
		f.calls = append(f.calls, "GET list")
		var page []map[string]any
		for n := 1; n < f.next-f.lag; n++ {
			is, ok := f.issues[n]
			if !ok {
				continue
			}
			labels := []map[string]string{}
			for _, l := range is.Labels {
				labels = append(labels, map[string]string{"name": l})
			}
			m := map[string]any{"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State, "state_reason": is.StateReason, "labels": labels}
			if is.IsPR {
				m["pull_request"] = map[string]string{"url": "x"}
			}
			page = append(page, m)
		}
		return json.Marshal([][]map[string]any{page}) // --slurp shape
	case len(args) == 2: // GET one issue by number — never lags
		var n int
		if _, err := fmt.Sscanf(args[1][strings.LastIndex(args[1], "/")+1:], "%d", &n); err != nil {
			return nil, err
		}
		f.calls = append(f.calls, fmt.Sprintf("GET #%d", n))
		is, ok := f.issues[n]
		if !ok {
			return nil, errors.New("gh: Not Found (HTTP 404)")
		}
		m := map[string]any{"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State, "state_reason": is.StateReason, "labels": []any{}}
		if is.IsPR {
			m["pull_request"] = map[string]string{"url": "x"}
		}
		return json.Marshal(m)
	case args[1] == "-X" && args[2] == "POST":
		f.calls = append(f.calls, "POST create")
		var p map[string]any
		if err := json.Unmarshal(stdin, &p); err != nil {
			return nil, err
		}
		f.posts = append(f.posts, p)
		is := &fakeIssue{Number: f.next, Title: p["title"].(string), Body: p["body"].(string), State: "open"}
		for _, l := range p["labels"].([]any) {
			is.Labels = append(is.Labels, l.(string))
		}
		f.issues[is.Number] = is
		f.next++
		return json.Marshal(map[string]int{"number": is.Number})
	case args[1] == "-X" && args[2] == "PATCH":
		var n int
		if _, err := fmt.Sscanf(args[3][strings.LastIndex(args[3], "/")+1:], "%d", &n); err != nil {
			return nil, err
		}
		f.calls = append(f.calls, fmt.Sprintf("PATCH #%d", n))
		var p map[string]any
		if err := json.Unmarshal(stdin, &p); err != nil {
			return nil, err
		}
		f.patchs[n] = append(f.patchs[n], p)
		is := f.issues[n]
		if is == nil {
			return nil, errors.New("404")
		}
		if v, ok := p["title"].(string); ok {
			is.Title = v
		}
		if v, ok := p["body"].(string); ok {
			is.Body = v
		}
		if v, ok := p["state"].(string); ok {
			is.State = v
			is.StateReason, _ = p["state_reason"].(string)
		}
		if v, ok := p["labels"].([]any); ok {
			is.Labels = nil
			for _, l := range v {
				is.Labels = append(is.Labels, l.(string))
			}
		}
		return []byte("{}"), nil
	}
	return nil, fmt.Errorf("fake gh: unhandled %v", args)
}

const testRepo = "acme/mirror"

// recorder is an in-memory RecordID: the trackers a run has written back.
type recorder struct {
	ids  map[string]string
	fail func(localID string) error // non-nil error = the local write failed
}

func (r *recorder) record(output, localID, trackerID string) error {
	if output != "github" {
		return fmt.Errorf("recorded under %q, want github", output)
	}
	if r.fail != nil {
		if err := r.fail(localID); err != nil {
			return err
		}
	}
	if r.ids == nil {
		r.ids = map[string]string{}
	}
	r.ids[localID] = trackerID
	return nil
}

func tickets() []hierarchy.Subtask {
	return []hierarchy.Subtask{
		{ID: "10", Title: `"Ten, quoted"`, Status: "pending", Type: "developer", Priority: "P1", Trackers: map[string]string{}},
		{ID: "2", Title: "Two", Status: "in_review", Type: "qa-agent", Priority: "P0", Trackers: map[string]string{}},
		{ID: "3", Title: "Three", Status: "done", Type: "developer", Priority: "P2", Trackers: map[string]string{}},
		{ID: "4", Title: "Four", Status: "dropped", Type: "developer", Priority: "P3", Trackers: map[string]string{}},
	}
}

// modelWith returns the ticket model with ids applied as recorded trackers.github.
func modelWith(t *testing.T, ids map[string]string) (Model, []Op) {
	t.Helper()
	subs := tickets()
	for i := range subs {
		if id, ok := ids[subs[i].ID]; ok {
			subs[i].Trackers = map[string]string{"github": id}
		}
	}
	m := BuildTicketModel(subs)
	ops, err := GitHubAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, ops
}

func newExec(f *fakeGitHub, r *recorder, apply bool) *GitHubExecutor {
	return &GitHubExecutor{Repo: testRepo, Run: f.run, Record: r.record, Apply: apply}
}

func runOnce(t *testing.T, e *GitHubExecutor, r *recorder) (GitHubResult, error) {
	t.Helper()
	m, ops := modelWith(t, r.ids)
	return e.Execute(context.Background(), m, ops)
}

func TestGitHub_SecondRunCreatesNothingAndWritesNothing(t *testing.T) {
	f, r := newFake(), &recorder{}
	e := newExec(f, r, true)

	if _, err := runOnce(t, e, r); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 4 || len(r.ids) != 4 {
		t.Fatalf("first run: %d issues, %d ids recorded, want 4 and 4", len(f.issues), len(r.ids))
	}
	callsAfterFirst := len(f.calls)

	res, err := runOnce(t, e, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls[callsAfterFirst:] {
		if !strings.HasPrefix(c, "GET") {
			t.Errorf("second run made a write: %v", f.calls[callsAfterFirst:])
		}
	}
	if len(f.issues) != 4 {
		t.Errorf("second run left %d issues, want still 4", len(f.issues))
	}
	if res.Count(ActUnchanged) != 4 {
		t.Errorf("second run outcomes %+v, want 4 unchanged", res.Outcomes)
	}
}

func TestGitHub_CreatesInTicketOrderWithMirrorShape(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	// numeric id order (2, 3, 4, 10), not lexical (10, 2, 3, 4)
	if f.issues[1].Title != "[2] Two" || f.issues[4].Title != "[10] Ten, quoted" {
		t.Errorf("titles = %q ... %q, want id-prefixed, numeric order, quotes stripped", f.issues[1].Title, f.issues[4].Title)
	}
	if r.ids["2"] != "acme/mirror#1" {
		t.Errorf("recorded %q, want acme/mirror#1", r.ids["2"])
	}
	if got := strings.Join(f.issues[1].Labels, ","); got != "type:qa-agent,priority:P0,status:in_review" {
		t.Errorf("labels = %q", got)
	}
	// terminal tickets are created, then closed — done as completed, dropped as not planned
	if f.issues[2].State != "closed" || f.issues[2].StateReason != "completed" {
		t.Errorf("done ticket issue = %s/%s, want closed/completed", f.issues[2].State, f.issues[2].StateReason)
	}
	if f.issues[3].State != "closed" || f.issues[3].StateReason != "not_planned" {
		t.Errorf("dropped ticket issue = %s/%s, want closed/not_planned", f.issues[3].State, f.issues[3].StateReason)
	}
	if f.issues[1].State != "open" {
		t.Errorf("in_review ticket issue is %s, want open", f.issues[1].State)
	}
}

// The window that matters: the issue exists remotely but the id never reached the
// ticket. The re-run must find the issue by its marker, not create a second one.
func TestGitHub_InterruptedBetweenCreateAndRecordResumesWithoutDuplicating(t *testing.T) {
	f, r := newFake(), &recorder{}
	r.fail = func(id string) error {
		if id == "3" {
			return errors.New("simulated crash before the id was written")
		}
		return nil
	}
	_, err := runOnce(t, newExec(f, r, true), r)
	if err == nil || !strings.Contains(err.Error(), "re-run will adopt") {
		t.Fatalf("want the run to stop naming the adoption path, got %v", err)
	}
	if len(f.issues) != 2 { // tickets 2 and 3 were created; 3's id was lost
		t.Fatalf("after the interrupted run: %d issues, want 2", len(f.issues))
	}
	if _, ok := r.ids["3"]; ok {
		t.Fatal("fixture broken: ticket 3's id should not have been recorded")
	}

	r.fail = nil
	res, err := runOnce(t, newExec(f, r, true), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 4 {
		t.Errorf("after resume: %d issues, want 4 (one per ticket, no duplicate of ticket 3)", len(f.issues))
	}
	var three Outcome
	for _, o := range res.Outcomes {
		if o.LocalID == "3" {
			three = o
		}
	}
	if three.Action != ActAdopted || r.ids["3"] != "acme/mirror#2" {
		t.Errorf("ticket 3 = %+v, recorded %q, want adopted as #2", three, r.ids["3"])
	}
	// the interrupted create never got to close the terminal ticket; the resume must
	if f.issues[2].State != "closed" {
		t.Errorf("adopted done ticket is %s, want the resume to close it", f.issues[2].State)
	}
}

// A create call that errors has an unknown outcome: stop, record nothing.
func TestGitHub_FailedCreateStopsWithoutRecording(t *testing.T) {
	f, r := newFake(), &recorder{}
	e := newExec(f, r, true)
	e.Run = func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "POST") {
			return nil, errors.New("HTTP 502")
		}
		return f.run(ctx, stdin, args...)
	}
	_, err := runOnce(t, e, r)
	if err == nil || len(r.ids) != 0 {
		t.Fatalf("err=%v recorded=%v, want an error and nothing recorded", err, r.ids)
	}
}

func TestGitHub_RemotelyDeletedIssueIsReportedNotRecreated(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	delete(f.issues, 2) // ticket 3's issue is deleted on GitHub
	before := len(f.calls)
	recordsBefore := len(r.ids)

	res, err := runOnce(t, newExec(f, r, true), r)
	if err != nil {
		t.Fatal(err)
	}
	var three Outcome
	for _, o := range res.Outcomes {
		if o.LocalID == "3" {
			three = o
		}
	}
	if three.Action != ActMissing || three.Number != 2 {
		t.Errorf("ticket 3 = %+v, want missing #2", three)
	}
	if len(res.Problems()) != 1 {
		t.Errorf("problems = %+v, want exactly the missing one", res.Problems())
	}
	for _, c := range f.calls[before:] {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("a POST was made for a deleted issue: %v", f.calls[before:])
		}
	}
	if len(f.issues) != 3 || len(r.ids) != recordsBefore || r.ids["3"] != "acme/mirror#2" {
		t.Errorf("issues=%d ids=%v, want the record left as it was and nothing re-created", len(f.issues), r.ids)
	}
	if !strings.Contains(FormatGitHubResult(res), "missing") {
		t.Error("the report must name the missing ticket")
	}
}

func TestGitHub_DryRunReadsOnlyAndRecordsNothing(t *testing.T) {
	f, r := newFake(), &recorder{}
	res, err := runOnce(t, newExec(f, r, false), r)
	if err != nil {
		t.Fatal(err)
	}
	if f.writes() != 0 || f.calls[0] != "GET visibility" || f.calls[1] != "GET list" {
		t.Errorf("dry run calls = %v, want reads only", f.calls)
	}
	if len(r.ids) != 0 {
		t.Errorf("dry run recorded %v", r.ids)
	}
	if !res.DryRun || res.Count(ActCreated) != 4 || !strings.Contains(FormatGitHubResult(res), "would create") {
		t.Errorf("dry run result = %+v\n%s", res, FormatGitHubResult(res))
	}

	// and over an existing mirror: drift and adoption are reported, not applied
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	f.issues[1].Title = "edited on GitHub"
	delete(r.ids, "3")
	calls := len(f.calls)
	res, err = runOnce(t, newExec(f, r, false), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls[calls:] {
		if !strings.HasPrefix(c, "GET") {
			t.Errorf("dry run over a drifted mirror made calls %v", f.calls[calls:])
		}
	}
	if f.issues[1].Title != "edited on GitHub" || r.ids["3"] != "" {
		t.Error("dry run changed the remote or the record")
	}
	if res.Count(ActUpdated) != 1 || res.Count(ActAdopted) != 1 {
		t.Errorf("outcomes %+v, want one update and one adoption reported", res.Outcomes)
	}
}

// The security default. With no Body func a ticket's text is never read, sent or
// compared; with one, it is sent.
func TestGitHub_BodiesAreOptIn(t *testing.T) {
	const secret = "SECRET-DEFECT-DESCRIPTION"
	bodyCalls := 0
	body := func(string) (string, error) { bodyCalls++; return secret, nil }

	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.posts {
		if strings.Contains(p["body"].(string), secret) || !strings.Contains(p["body"].(string), "<!-- leadkit-sync:ticket=") {
			t.Errorf("default body = %q, want the marker and no ticket text", p["body"])
		}
	}
	// a body edited remotely is not touched when bodies are off
	f.issues[1].Body = "edited " + marker("2")
	calls := len(f.calls)
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	if len(f.patchs[1]) != 0 {
		t.Errorf("metadata-only run patched a remote body: %v", f.calls[calls:])
	}

	// opted in
	f2, r2 := newFake(), &recorder{}
	e := newExec(f2, r2, true)
	e.Body = body
	if _, err := runOnce(t, e, r2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f2.posts[0]["body"].(string), secret) {
		t.Errorf("opted-in body = %q, want the ticket text", f2.posts[0]["body"])
	}
	if bodyCalls != 4 {
		t.Errorf("Body read %d times, want once per ticket", bodyCalls)
	}
	// ...and then it is kept in step
	f2.issues[1].Body = marker("2") + "\n\nold"
	if _, err := runOnce(t, e, r2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f2.issues[1].Body, secret) {
		t.Error("opted-in run left a stale body")
	}
}

func TestGitHub_LabelsKeepForeignReplaceManaged(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	f.issues[1].Labels = []string{"help wanted", "status:pending", "type:qa-agent", "priority:P0"}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.issues[1].Labels, ",")
	if got != "help wanted,type:qa-agent,priority:P0,status:in_review" {
		t.Errorf("labels = %q, want the foreign label kept and the stale status replaced", got)
	}
}

func TestGitHub_ReopensWhenTicketIsNotTerminal(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	f.issues[1].State = "closed" // someone closed ticket 2's issue; the ticket is in_review
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	if f.issues[1].State != "open" {
		t.Error("files are authoritative: an open ticket's closed issue must be reopened")
	}
}

func TestGitHub_ForeignRepoIdIsLeftAlone(t *testing.T) {
	for _, id := range []string{"someone/else#1", "7"} {
		f := newFake()
		r := &recorder{ids: map[string]string{"2": id}}
		res, err := runOnce(t, newExec(f, r, true), r)
		if err != nil {
			t.Fatal(err)
		}
		var two Outcome
		for _, o := range res.Outcomes {
			if o.LocalID == "2" {
				two = o
			}
		}
		if two.Action != ActForeign || r.ids["2"] != id {
			t.Errorf("id %q: outcome %+v, recorded %q, want foreign and untouched", id, two, r.ids["2"])
		}
		for _, is := range f.issues {
			if strings.HasPrefix(is.Title, "[2]") {
				t.Errorf("id %q: an issue was created for a ticket recorded elsewhere", id)
			}
		}
	}
}

func TestGitHub_RepoCaseDoesNotMatter(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	e := newExec(f, r, true)
	e.Repo = "ACME/Mirror"
	res, err := runOnce(t, e, r)
	if err != nil || res.Count(ActUnchanged) != 4 {
		t.Errorf("err=%v outcomes=%+v, want 4 unchanged", err, res.Outcomes)
	}
}

func TestGitHub_MarkerNamingAnotherTicketIsAMismatch(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	f.issues[1].Body = marker("999")
	res, _ := runOnce(t, newExec(f, r, true), r)
	if res.Count(ActMismatch) != 1 {
		t.Errorf("outcomes %+v, want one mismatch", res.Outcomes)
	}
	if f.issues[1].Title != "[2] Two" || len(f.patchs[1]) != 0 {
		t.Error("a mismatched issue must not be patched")
	}
}

func TestGitHub_PullRequestsAreNeverAdoptedOrCounted(t *testing.T) {
	f, r := newFake(), &recorder{}
	f.issues[1] = &fakeIssue{Number: 1, Title: "a PR", Body: marker("2"), State: "open", IsPR: true}
	f.next = 2
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	if r.ids["2"] == "acme/mirror#1" {
		t.Error("ticket 2 adopted a pull request")
	}
}

func TestGitHub_PacesWrites(t *testing.T) {
	f, r := newFake(), &recorder{}
	e := newExec(f, r, true)
	e.Pace = time.Second
	slept := 0
	e.Sleep = func(d time.Duration) {
		if d != time.Second {
			t.Errorf("slept %v", d)
		}
		slept++
	}
	if _, err := runOnce(t, e, r); err != nil {
		t.Fatal(err)
	}
	if want := f.writes(); slept != want {
		t.Errorf("slept %d times for %d writes", slept, want)
	}
}

func TestGitHub_RejectsAMalformedRepoBeforeAnyCall(t *testing.T) {
	for _, repo := range []string{"", "noslash", "a/b/c", "a/b?x=1", "a/..", "a b/c"} {
		f, r := newFake(), &recorder{}
		e := newExec(f, r, true)
		e.Repo = repo
		if _, err := runOnce(t, e, r); err == nil || len(f.calls) != 0 {
			t.Errorf("repo %q: err=%v calls=%v, want a refusal before any call", repo, err, f.calls)
		}
	}
}

func TestGitHubAdapter_Plan(t *testing.T) {
	m := BuildTicketModel([]hierarchy.Subtask{
		{ID: "1", Title: "new", Status: "pending", Type: "developer", Priority: "P1"},
		{ID: "2", Title: "synced", Status: "pending", Trackers: map[string]string{"github": "a/b#5"}},
		{ID: "3", Title: "done+synced", Status: "done", Trackers: map[string]string{"github": "a/b#6"}},
		{ID: "4", Title: "linear only", Status: "pending", Trackers: map[string]string{"linear": "LIN-1"}},
	})
	ops, err := GitHubAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, op := range ops {
		got = append(got, op.LocalID+":"+op.Action+":"+op.Level)
	}
	want := "1:create:issue 2:update:issue 3:close:issue 4:create:issue"
	if strings.Join(got, " ") != want {
		t.Errorf("ops = %v, want %s", got, want)
	}
	if ops[0].Fields["type"] != "developer" || ops[0].Fields["priority"] != "P1" {
		t.Errorf("fields = %v", ops[0].Fields)
	}
}

func TestGitHubAdapter_RefusesAHierarchyModel(t *testing.T) {
	m := buildFixtureSyncModel(t) // project/milestone/epic/issue/task nodes
	if _, err := (GitHubAdapter{}).Plan(m); err == nil {
		t.Error("the github adapter must refuse nodes that are not tickets")
	}
}

// BuildSyncModel drops every ticket with no parent; the live store has none with one.
// This is the reason BuildTicketModel exists, pinned against the same input.
func TestBuildTicketModel_KeepsTheTicketsBuildSyncModelDrops(t *testing.T) {
	subs := []hierarchy.Subtask{{ID: "1", Title: "orphan", Status: "pending"}, {ID: "2", Title: "orphan too", Status: "pending"}}
	mr := hierarchy.ComputeRollup(hierarchy.Model{Project: "p"}, nil, subs)
	if n := len(BuildSyncModel(mr, subs).Nodes); n != 1 { // the project root only
		t.Fatalf("fixture no longer shows the hierarchy model dropping parentless tickets: %d nodes", n)
	}
	if n := len(BuildTicketModel(subs).Nodes); n != 2 {
		t.Errorf("BuildTicketModel kept %d of 2 tickets", n)
	}
}

func TestBuildModelFor_PicksByOutput(t *testing.T) {
	subs := []hierarchy.Subtask{{ID: "1", Title: "orphan", Status: "pending"}}
	mr := hierarchy.ComputeRollup(hierarchy.Model{Project: "p"}, nil, subs)
	if n := len(BuildModelFor(GitHubAdapter{}, mr, subs).Nodes); n != 1 || BuildModelFor(GitHubAdapter{}, mr, subs).Nodes[0].Level != "task" {
		t.Error("github must get the flat ticket model")
	}
	if BuildModelFor(LinearAdapter{}, mr, subs).Nodes[0].Level != "project" {
		t.Error("linear must keep the hierarchy model")
	}
}

func TestUnquoteScalar(t *testing.T) {
	for in, want := range map[string]string{
		`plain`: `plain`, `"a \"b\" c"`: `a "b" c`, `'it''s'`: `it's`, `"`: `"`, ` spaced `: `spaced`,
	} {
		if got := unquoteScalar(in); got != want {
			t.Errorf("unquoteScalar(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseGithubID(t *testing.T) {
	for in, ok := range map[string]bool{"a/b#1": true, "a/b#0": false, "a/b#x": false, "a/b": false, "7": false, "#7": false, "ab#7": false} {
		if _, _, got := parseGithubID(in); got != ok {
			t.Errorf("parseGithubID(%q) ok=%v, want %v", in, got, ok)
		}
	}
}

func TestParseIssueListing_SlurpedAndFlat(t *testing.T) {
	for name, in := range map[string]string{
		"slurped": `[[{"number":1,"body":"a"}],[{"number":2},{"number":3,"pull_request":{"url":"u"}}]]`,
		"flat":    `[{"number":1,"body":"a"},{"number":2},{"number":3,"pull_request":{"url":"u"}}]`,
	} {
		got, max, err := parseIssueListing([]byte(in))
		if err != nil || len(got) != 2 || max != 3 {
			t.Errorf("%s: %v, %d issues, max %d, want 2 issues (the PR dropped) and max 3 (the PR still counts)", name, err, len(got), max)
		}
	}
	if _, _, err := parseIssueListing([]byte(`{"message":"Not Found"}`)); err == nil {
		t.Error("an error object must not parse as an empty listing")
	}
}

func TestTicketBody_StripsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "5-x.md"), []byte("---\nid: 5\ntitle: t\n---\n\nthe body\n\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := TicketBody(dir, dir, "5")
	if err != nil || got != "the body\n\nmore\n" {
		t.Errorf("body = %q, %v", got, err)
	}
	if _, err := TicketBody(dir, dir, "99"); err == nil {
		t.Error("want an error for an unknown ticket")
	}
}

// The recorded id round-trips through the real RecordID and the frontmatter reader.
func TestGitHub_RecordedIDSurvivesTheRealFrontmatterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "7-x.md")
	if err := os.WriteFile(path, []byte("---\nid: 7\ntitle: seven\nstatus: pending\ntrackers: {}\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordID(dir, dir, "github", "7", githubID("acme/mirror", 12)); err != nil {
		t.Fatal(err)
	}
	subs, err := hierarchy.LoadSubtasks(dir)
	if err != nil || len(subs) != 1 {
		t.Fatal(err, subs)
	}
	if got := subs[0].Trackers["github"]; got != "acme/mirror#12" {
		t.Errorf("trackers.github read back as %q", got)
	}
}

// A dry run is the owner's only look at what would be published, so it must show titles.
func TestFormatGitHubResult_ShowsTheTitleThatWouldBePublished(t *testing.T) {
	f, r := newFake(), &recorder{}
	res, err := runOnce(t, newExec(f, r, false), r)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatGitHubResult(res); !strings.Contains(out, `"[10] Ten, quoted"`) {
		t.Errorf("dry run does not show the title:\n%s", out)
	}
}

// The listing lags (measured against real GitHub: an issue created a moment earlier was
// missing from it every time). An immediate resume must still not duplicate.
func TestGitHub_ResumeUnderALaggingListingDoesNotDuplicate(t *testing.T) {
	f, r := newFake(), &recorder{}
	r.fail = func(id string) error {
		if id == "3" {
			return errors.New("simulated crash before the id was written")
		}
		return nil
	}
	if _, err := runOnce(t, newExec(f, r, true), r); err == nil {
		t.Fatal("fixture: want the interrupted run to fail")
	}
	f.lag = 2 // the listing cannot yet see the two issues the dead run made

	r.fail = nil
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 4 {
		t.Errorf("%d issues after an immediate resume, want 4: the lagging listing hid the dead run's issue and it was created twice", len(f.issues))
	}
	if r.ids["3"] != "acme/mirror#2" {
		t.Errorf("ticket 3 recorded %q, want the issue the dead run made (#2)", r.ids["3"])
	}
}

// A recorded issue the lagging listing has not caught up with is not "deleted".
func TestGitHub_LaggingListingDoesNotMakeALiveIssueMissing(t *testing.T) {
	f, r := newFake(), &recorder{}
	if _, err := runOnce(t, newExec(f, r, true), r); err != nil {
		t.Fatal(err)
	}
	f.lag = 2
	res, err := runOnce(t, newExec(f, r, true), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count(ActMissing) != 0 || len(res.Problems()) != 0 {
		t.Errorf("outcomes %+v: a live issue was reported as missing", res.Outcomes)
	}
}

// Missing means GitHub answered "gone" for that number — 404 or 410 — not merely that
// the listing omitted it, and any other failure is an error, never a verdict.
func TestGitHub_OnlyGoneMeansMissing(t *testing.T) {
	for _, tc := range []struct {
		msg     string
		missing bool
	}{{"gh: Not Found (HTTP 404)", true}, {"gh: Gone (HTTP 410)", true}, {"gh: Server Error (HTTP 502)", false}} {
		f := newFake()
		r := &recorder{ids: map[string]string{"2": "acme/mirror#5"}}
		e := newExec(f, r, true)
		e.Run = func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
			if len(args) == 2 && strings.HasSuffix(args[1], "/issues/5") {
				return nil, errors.New(tc.msg)
			}
			return f.run(ctx, stdin, args...)
		}
		res, err := runOnce(t, e, r)
		if tc.missing && (err != nil || res.Count(ActMissing) != 1) {
			t.Errorf("%s: err=%v outcomes=%+v, want one missing", tc.msg, err, res.Outcomes)
		}
		if !tc.missing && (err == nil || res.Count(ActMissing) != 0) {
			t.Errorf("%s: err=%v outcomes=%+v, want an error and no missing verdict", tc.msg, err, res.Outcomes)
		}
	}
}

// parentlessTitle is ticket i's title as a person wrote it; every 7th is stored quoted.
func parentlessTitle(i int) string { return fmt.Sprintf("ticket number %d: it's a title", i) }

// writeParentlessStore lays out n tickets exactly as `task create --unparented` leaves
// them: no parent, no milestones.yaml, a mix of statuses and quoting. It returns the dir.
func writeParentlessStore(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	statuses := []string{"pending", "in_review", "done", "dropped", "superseded"}
	for i := 1; i <= n; i++ {
		title := parentlessTitle(i)
		if i%7 == 0 {
			title = fmt.Sprintf("%q", title)
		}
		body := fmt.Sprintf("---\nid: %d\ntitle: %s\nstatus: %s\ntype: developer\npriority: P%d\ntrackers: {}\n---\n\nbody %d\n",
			i, title, statuses[i%len(statuses)], i%4, i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d-t.md", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The data-loss pin (ticket 193/217). The owner retires docs/BACKLOG.md in favour of
// the issues this sync creates, so a sync that silently moves fewer tickets than the
// store holds is followed by deleting the only committed copy. The assertion is a
// COUNT at every stage, through the real frontmatter loader, because "it ran" is
// exactly what the dropping BuildSyncModel also did.
func TestGitHub_EveryParentlessTicketGoesIn_AndAnIssueComesOut(t *testing.T) {
	const n = 216 // a store the size of the live one
	dir := writeParentlessStore(t, n)

	subs, err := hierarchy.LoadSubtasks(dir)
	if err != nil || len(subs) != n {
		t.Fatalf("loader returned %d of %d tickets (%v)", len(subs), n, err)
	}
	mr := hierarchy.ComputeRollup(hierarchy.Model{Project: "p"}, nil, subs)
	// the fixture must be the dropping shape, or every count below is vacuous
	if got := len(BuildSyncModel(mr, subs).Nodes); got != 1 {
		t.Fatalf("fixture is not parentless: the hierarchy model kept %d nodes, want the lone root", got)
	}

	adapter := GitHubAdapter{}
	model := BuildModelFor(adapter, mr, subs)
	if len(model.Nodes) != n {
		t.Fatalf("model holds %d of %d tickets", len(model.Nodes), n)
	}
	ops, err := adapter.Plan(model)
	if err != nil || len(ops) != n {
		t.Fatalf("plan holds %d of %d tickets (%v)", len(ops), n, err)
	}

	f, r := newFake(), &recorder{}
	res, err := newExec(f, r, true).Execute(context.Background(), model, ops)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count(ActCreated) != n || len(f.issues) != n || len(r.ids) != n {
		t.Fatalf("%d tickets in: %d created, %d issues exist, %d ids recorded; want %d of each",
			n, res.Count(ActCreated), len(f.issues), len(r.ids), n)
	}
	titles := map[string]bool{}
	for _, is := range f.issues {
		titles[is.Title] = true
	}
	for i := 1; i <= n; i++ {
		if want := fmt.Sprintf("[%d] %s", i, parentlessTitle(i)); !titles[want] {
			t.Errorf("no issue titled %q", want)
		}
		if r.ids[fmt.Sprint(i)] == "" {
			t.Errorf("ticket %d has no recorded issue id", i)
		}
	}
}

// Ticket 196. The ticket titles are an index of unfixed defects and a published title cannot be unpublished,
// while the project keeps a public repository beside the private one it develops in. The mirror target must
// be CONFIRMED private by the forge, in code, by default, for a dry run as well as an apply. Nothing else
// is called before the answer: no listing, no write, no record.
func TestGitHub_APublicRepoIsRefusedBeforeAnyOtherCallDryRunAndApplyAlike(t *testing.T) {
	for _, apply := range []bool{false, true} {
		f, r := newFake(), &recorder{}
		f.public = true
		res, err := runOnce(t, newExec(f, r, apply), r)
		if err == nil || !strings.Contains(err.Error(), "not private") || !strings.Contains(err.Error(), "no flag that overrides") {
			t.Fatalf("apply=%v: a public repo must be refused, naming that nothing overrides it; err=%v", apply, err)
		}
		if len(f.calls) != 1 || f.calls[0] != "GET visibility" || len(r.ids) != 0 || len(res.Outcomes) != 0 {
			t.Errorf("apply=%v: only the visibility query may run; calls=%v recorded=%v outcomes=%d", apply, f.calls, r.ids, len(res.Outcomes))
		}
	}
}

// An answer that is not exactly "true", or none, is a refusal: the check fails closed.
func TestGitHub_AVisibilityThatCannotBeConfirmedIsRefused(t *testing.T) {
	for name, vis := range map[string]string{"false": "false", "empty object": "{}", "word": "private", "null": "null", "TRUE": "TRUE", "yes": "yes"} {
		f, r := newFake(), &recorder{}
		f.vis = vis
		if _, err := runOnce(t, newExec(f, r, true), r); err == nil || f.writes() != 0 || len(f.calls) != 1 {
			t.Errorf("%s (%q): not a confirmed private repo, must be refused with no other call; err=%v calls=%v", name, vis, err, f.calls)
		}
	}
	// The forge cannot be reached or the repo cannot be seen (a 404 reads as private to the unauthenticated, not here).
	r := &recorder{}
	e := &GitHubExecutor{Repo: testRepo, Record: r.record, Apply: true, Run: func(context.Context, []byte, ...string) ([]byte, error) {
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}}
	m, ops := modelWith(t, nil)
	if _, err := e.Execute(context.Background(), m, ops); err == nil || !strings.Contains(err.Error(), "cannot confirm it is private") {
		t.Errorf("an unreadable visibility must be refused, got %v", err)
	}
}

// CONTROL: a private repo goes through exactly as before.
func TestGitHub_APrivateRepoIsMirroredAsBefore(t *testing.T) {
	f, r := newFake(), &recorder{}
	res, err := runOnce(t, newExec(f, r, true), r)
	if err != nil || res.Count(ActCreated) != 4 || f.calls[0] != "GET visibility" {
		t.Errorf("a confirmed private repo must mirror as before: err=%v created=%d calls=%v", err, res.Count(ActCreated), f.calls)
	}
}
