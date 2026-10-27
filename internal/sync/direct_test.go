package sync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/linearapi"
)

const testKey = "lin_api_TESTSECRET_do_not_leak"

type capture struct {
	query string
	vars  map[string]any
	auth  string
}

// fakeLinear answers by query shape and records every request. Nothing here ever
// reaches the network.
type fakeLinear struct {
	reqs           []capture
	existing       map[string]string // query-name keyword -> id already in Linear (find-by-name hit)
	issueIDs       []string          // ids Linear "has" for the recorded-id existence check
	createdSeq     int
	failEverything bool
	failAlias      string // e.g. "op1": that aliased issueCreate errors; the rest succeed (partial data + errors)
}

func (f *fakeLinear) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(b, &in)
	f.reqs = append(f.reqs, capture{query: in.Query, vars: in.Variables, auth: r.Header.Get("Authorization")})
	reply := func(s string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(s))}, nil
	}
	q := in.Query
	if f.failEverything {
		return reply(`{"errors":[{"message":"boom ` + testKey + `"}]}`)
	}
	switch {
	case strings.Contains(q, "teams(filter"):
		return reply(`{"data":{"teams":{"nodes":[{"id":"team-uuid"}]}}}`)
	case strings.Contains(q, "states(first"):
		return reply(`{"data":{"team":{"states":{"nodes":[{"id":"st-todo","name":"Todo"},{"id":"st-done","name":"Done"}],"pageInfo":{"hasNextPage":false}}}}}`)
	case strings.Contains(q, "initiatives(filter"):
		return reply(`{"data":{"initiatives":{"nodes":[]}}}`)
	case strings.Contains(q, "initiativeCreate"):
		return reply(`{"data":{"initiativeCreate":{"initiative":{"id":"init-1"}}}}`)
	case strings.Contains(q, "projects(filter"):
		if id := f.existing["project"]; id != "" {
			return reply(`{"data":{"projects":{"nodes":[{"id":"` + id + `"}]}}}`)
		}
		return reply(`{"data":{"projects":{"nodes":[]}}}`)
	case strings.Contains(q, "projectCreate"):
		return reply(`{"data":{"projectCreate":{"project":{"id":"proj-1"}}}}`)
	case strings.Contains(q, "initiativeToProjectCreate"):
		return reply(`{"data":{"initiativeToProjectCreate":{"success":true}}}`)
	case strings.Contains(q, "projectMilestones(filter"):
		return reply(`{"data":{"projectMilestones":{"nodes":[]}}}`)
	case strings.Contains(q, "projectMilestoneCreate"):
		return reply(`{"data":{"projectMilestoneCreate":{"projectMilestone":{"id":"pm-1"}}}}`)
	case strings.Contains(q, "issues(filter"):
		var nodes []string
		for _, id := range f.issueIDs {
			nodes = append(nodes, `{"id":"`+id+`"}`)
		}
		return reply(`{"data":{"issues":{"nodes":[` + strings.Join(nodes, ",") + `],"pageInfo":{"hasNextPage":false}}}}`)
	case strings.HasPrefix(q, "mutation Batch"):
		out := map[string]any{}
		for i := 0; i < 50; i++ {
			a := "op" + string(rune('0'+i%10))
			if i >= 10 {
				break
			}
			if strings.Contains(q, a+": issueCreate") && a == f.failAlias {
				out[a] = nil
			} else if strings.Contains(q, a+": issueCreate") {
				f.createdSeq++
				out[a] = map[string]any{"success": true, "issue": map[string]any{"id": "new-" + string(rune('0'+f.createdSeq))}}
			} else if strings.Contains(q, a+": issueUpdate") {
				out[a] = map[string]any{"success": true}
			}
		}
		payload := map[string]any{"data": out}
		if f.failAlias != "" {
			payload["errors"] = []map[string]any{{"message": "issueCreate failed for " + f.failAlias}}
		}
		bb, _ := json.Marshal(payload)
		return reply(string(bb))
	}
	return reply(`{"errors":[{"message":"unexpected query"}]}`)
}

func (f *fakeLinear) count(sub string) int {
	n := 0
	for _, r := range f.reqs {
		if strings.Contains(r.query, sub) {
			n++
		}
	}
	return n
}

func syncStore(t *testing.T) (storeRoot, issuesRoot string) {
	t.Helper()
	root := t.TempDir()
	storeRoot, issuesRoot = filepath.Join(root, "tasks"), filepath.Join(root, "issues")
	for _, d := range []string{storeRoot, issuesRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(filepath.Join(issuesRoot, "I1-x.md"), "---\nid: I1\ntitle: Portal cleanup\nstatus: pending\nparent: dev-portal\ntrackers: {}\n---\n")
	w(filepath.Join(storeRoot, "1-a.md"), "---\nid: 1\ntitle: Task one\nstatus: pending\nparent: I1\ntrackers: {}\n---\n")
	return
}

func runDirect(t *testing.T, f *fakeLinear, storeRoot, issuesRoot string) (DirectResult, error) {
	t.Helper()
	m := buildFixtureSyncModel(t)
	ops, err := LinearAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	c := linearapi.New(testKey, f)
	c.Sleep = func(d time.Duration) {}
	e := &DirectExecutor{
		C:   c,
		Cfg: DirectConfig{TeamKey: "CI", ClosedState: "Done", DefaultState: "Todo"},
		Record: func(output, local, id string) error {
			return RecordID(storeRoot, issuesRoot, output, local, id)
		},
	}
	return e.Execute(context.Background(), m, ops)
}

// The direct executor turns the plan into the right GraphQL: structure nodes
// found-or-created by name, issue creates as ONE batched aliased mutation per
// wave (issues, then sub-issues once their parent id is known), closes/updates
// batched, and every created id recorded back via record-id.
func TestDirectExecutor_BuildsCreateAndUpdateMutationsAndRecordsIDs(t *testing.T) {
	store, issues := syncStore(t)
	f := &fakeLinear{issueIDs: []string{"LIN-2", "LIN-20"}}
	res, err := runDirect(t, f, store, issues)
	if err != nil {
		t.Fatal(err)
	}
	// project(initiative)+milestone(project)+epic(milestone)+I1+task1 created; I2, task2 closed.
	if res.Created != 5 || res.Closed != 2 || res.Updated != 0 {
		t.Fatalf("result = %+v", res)
	}

	var issueCreate, closeBatch, subCreate *capture
	for i := range f.reqs {
		r := &f.reqs[i]
		switch {
		case strings.Contains(r.query, "issueCreate") && strings.Contains(r.query, "$in0: IssueCreateInput!") && r.vars["in0"].(map[string]any)["title"] == "Portal cleanup":
			issueCreate = r
		case strings.Contains(r.query, "issueCreate") && r.vars["in0"].(map[string]any)["title"] == "Task one":
			subCreate = r
		case strings.Contains(r.query, "issueUpdate"):
			closeBatch = r
		}
	}
	if issueCreate == nil || subCreate == nil || closeBatch == nil {
		t.Fatalf("missing expected requests: %+v", f.reqs)
	}
	in := issueCreate.vars["in0"].(map[string]any)
	if in["teamId"] != "team-uuid" || in["projectMilestoneId"] != "pm-1" || in["projectId"] != "proj-1" || in["stateId"] != "st-todo" {
		t.Errorf("issue create input = %v", in)
	}
	sub := subCreate.vars["in0"].(map[string]any)
	if sub["parentId"] != "new-1" {
		t.Errorf("sub-issue must be parented on the id the issue create just returned, got %v", sub)
	}
	if !strings.Contains(closeBatch.query, "op0: issueUpdate(id: $id0, input: $in0)") || !strings.Contains(closeBatch.query, "op1: issueUpdate") {
		t.Errorf("close/updates must be ONE aliased batch:\n%s", closeBatch.query)
	}
	if closeBatch.vars["id0"] != "LIN-2" || closeBatch.vars["in0"].(map[string]any)["stateId"] != "st-done" {
		t.Errorf("close batch vars = %v", closeBatch.vars)
	}
	if f.count("initiativeToProjectCreate") != 1 {
		t.Error("the new Linear project must be linked under the initiative")
	}

	// record-id converged: the created ids are now on disk, so the NEXT plan updates.
	b, _ := os.ReadFile(filepath.Join(issues, "I1-x.md"))
	if !strings.Contains(string(b), "new-1") {
		t.Errorf("I1 tracker id not recorded:\n%s", b)
	}
	b, _ = os.ReadFile(filepath.Join(store, "1-a.md"))
	if !strings.Contains(string(b), "new-2") {
		t.Errorf("task 1 tracker id not recorded:\n%s", b)
	}
}

// A node with a recorded id is updated/closed, never created; with every id
// recorded a re-run creates nothing in Linear. Stale recorded ids fail loud.
func TestDirectExecutor_RecordedIDsUpdateNeverRecreate(t *testing.T) {
	store, issues := syncStore(t)
	f := &fakeLinear{issueIDs: []string{"LIN-2", "LIN-20"}}
	if _, err := runDirect(t, f, store, issues); err != nil {
		t.Fatal(err)
	}

	// Re-run against the now-recorded store. Rebuild the model from disk-equivalent
	// state: I1 -> new-1, task 1 -> new-2.
	m := buildFixtureSyncModel(t)
	for i, n := range m.Nodes {
		switch n.LocalID {
		case "I1":
			m.Nodes[i].Trackers = map[string]string{"linear": "new-1"}
		case "1":
			m.Nodes[i].Trackers = map[string]string{"linear": "new-2"}
		}
	}
	ops, _ := LinearAdapter{}.Plan(m)
	for _, op := range ops {
		if op.Level == "issue" || op.Level == "sub-issue" {
			if op.Action == "create" {
				t.Fatalf("plan still wants to create %s after recording", op.LocalID)
			}
		}
	}
	f2 := &fakeLinear{issueIDs: []string{"LIN-2", "LIN-20", "new-1", "new-2"}, existing: map[string]string{"project": "proj-1"}}
	c := linearapi.New(testKey, f2)
	c.Sleep = func(time.Duration) {}
	e := &DirectExecutor{C: c, Cfg: DirectConfig{TeamKey: "CI", ClosedState: "Done", DefaultState: "Todo"}}
	if _, err := e.Execute(context.Background(), m, ops); err != nil {
		t.Fatal(err)
	}
	if f2.count("issueCreate") != 0 {
		t.Errorf("re-run must not issueCreate anything, got %d", f2.count("issueCreate"))
	}
	if f2.count("issueUpdate") == 0 {
		t.Error("recorded nodes must be updated")
	}

	// Stale recorded id: refuses, does not recreate.
	f3 := &fakeLinear{issueIDs: []string{"LIN-2"}}
	c3 := linearapi.New(testKey, f3)
	c3.Sleep = func(time.Duration) {}
	e3 := &DirectExecutor{C: c3, Cfg: e.Cfg}
	if _, err := e3.Execute(context.Background(), m, ops); err == nil || !strings.Contains(err.Error(), "not found in Linear") {
		t.Errorf("a recorded id missing from Linear must fail loud, got %v", err)
	}
	if f3.count("issueCreate") != 0 {
		t.Error("a stale id must never trigger a recreate")
	}
}

// SECURITY: the key goes only into the Authorization header. It is never in a
// request body/variables, never in the store files record-id writes, and never in
// an error even when the server echoes it back.
func TestDirectExecutor_KeyNeverWrittenOrLeaked(t *testing.T) {
	store, issues := syncStore(t)
	f := &fakeLinear{issueIDs: []string{"LIN-2", "LIN-20"}}
	if _, err := runDirect(t, f, store, issues); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.reqs {
		if r.auth != testKey {
			t.Errorf("Authorization = %q", r.auth)
		}
		body, _ := json.Marshal(map[string]any{"q": r.query, "v": r.vars})
		if strings.Contains(string(body), testKey) {
			t.Errorf("key leaked into a request body: %s", body)
		}
	}
	for _, dir := range []string{store, issues} {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), testKey) {
					t.Errorf("key written to store file %s", p)
				}
			}
			return nil
		})
	}

	// Failure path: server echoes the key in its error; ours must not.
	bad := &fakeLinear{failEverything: true}
	_, err := runDirect(t, bad, store, issues)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("key leaked in error: %v", err)
	}
}

// three issues under one epic -> one create wave of three aliased issueCreates.
func threeIssueModel(t *testing.T) (Model, []Op) {
	t.Helper()
	m := Model{Nodes: []Node{
		{Level: "project", LocalID: "project", Title: "P"},
		{Level: "milestone", LocalID: "ms", Title: "M", ParentLocalID: "project"},
		{Level: "epic", LocalID: "ep", Title: "ep", ParentLocalID: "ms"},
		{Level: "issue", LocalID: "IA", Title: "A", ParentLocalID: "ep"},
		{Level: "issue", LocalID: "IB", Title: "B", ParentLocalID: "ep"},
		{Level: "issue", LocalID: "IC", Title: "C", ParentLocalID: "ep"},
	}}
	ops, err := LinearAdapter{}.Plan(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, ops
}

func testExec(f *fakeLinear, rec func(string, string, string) error) *DirectExecutor {
	c := linearapi.New(testKey, f)
	c.Sleep = func(time.Duration) {}
	return &DirectExecutor{C: c, Cfg: DirectConfig{TeamKey: "CI", ClosedState: "Done", DefaultState: "Todo"}, Record: rec}
}

// One aliased create in a batch fails: Linear returns data for the others
// alongside errors. Every id that WAS created must be recorded (else a re-run
// double-creates it) and the error still surfaces.
func TestDirectExecutor_PartialBatchRecordsEveryCreatedID(t *testing.T) {
	m, ops := threeIssueModel(t)
	f := &fakeLinear{failAlias: "op1"}
	recorded := map[string]string{}
	e := testExec(f, func(_, local, id string) error { recorded[local] = id; return nil })
	_, err := e.Execute(context.Background(), m, ops)
	if err == nil || !strings.Contains(err.Error(), "issueCreate failed for op1") {
		t.Fatalf("the partial failure must still surface, got %v", err)
	}
	if recorded["IA"] == "" || recorded["IC"] == "" || recorded["IB"] != "" || len(recorded) != 2 {
		t.Errorf("want IA and IC recorded (created) and IB not, got %v", recorded)
	}
}

// A Record failure mid-wave must not stop the remaining created ids being
// recorded; all failures are aggregated.
func TestDirectExecutor_RecordFailureMidWaveStillRecordsRest(t *testing.T) {
	m, ops := threeIssueModel(t)
	recorded := map[string]string{}
	e := testExec(&fakeLinear{}, func(_, local, id string) error {
		if local == "IB" {
			return errors.New("disk full")
		}
		recorded[local] = id
		return nil
	})
	_, err := e.Execute(context.Background(), m, ops)
	if err == nil || !strings.Contains(err.Error(), "IB") || !strings.Contains(err.Error(), "record-id failed") {
		t.Fatalf("want the aggregated record failure naming IB, got %v", err)
	}
	if recorded["IA"] == "" || recorded["IC"] == "" {
		t.Errorf("IA and IC must still be recorded after IB's Record failed, got %v", recorded)
	}
}

// projectCreate's teamIds is a required ARRAY in Linear's schema.
func TestDirectExecutor_ProjectCreateSendsTeamIdsArray(t *testing.T) {
	m, ops := threeIssueModel(t)
	f := &fakeLinear{}
	if _, err := testExec(f, nil).Execute(context.Background(), m, ops); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.reqs {
		if strings.Contains(r.query, "projectCreate") {
			in := r.vars["in"].(map[string]any)
			if ids, ok := in["teamIds"].([]any); !ok || len(ids) != 1 || ids[0] != "team-uuid" {
				t.Errorf("teamIds = %#v, want [team-uuid]", in["teamIds"])
			}
			return
		}
	}
	t.Error("no projectCreate request seen")
}
