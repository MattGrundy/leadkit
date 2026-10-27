package taskcreate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeAgentDef(t *testing.T, agentsDir, name string) {
	t.Helper()
	mustWrite(t, filepath.Join(agentsDir, name+".md"), "---\nname: "+name+"\n---\n")
}

// baseOpts defaults to Unparented: true — most of these tests aren't exercising the
// --issue requirement itself (that's TestCreate_RequiresIssueUnlessUnparented and
// the dedicated --issue validation tests below), so they'd otherwise all need to
// fabricate a real issue just to get past validation.
func baseOpts(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	queueRoot := filepath.Join(root, "queue")
	agentsDir := filepath.Join(root, "team-agents")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentDef(t, agentsDir, "devops-agent")
	return Options{
		Title: "Fix the thing", Type: "devops-agent", Unparented: true,
		StoreRoot: storeRoot, QueueRoot: queueRoot, RolesPath: filepath.Join(queueRoot, "ROLES"),
		AgentsDir: agentsDir, IssuesRoot: issuesRoot,
	}
}

// writeIssue drops a minimal issues/*.md file so --issue validation can find it.
func writeIssue(t *testing.T, issuesRoot, id string) {
	t.Helper()
	mustWrite(t, filepath.Join(issuesRoot, id+"-x.md"), "---\nid: "+id+"\nstatus: pending\nparent: some-epic\n---\n")
}

func TestCreate_FirstTaskGetsID1(t *testing.T) {
	opts := baseOpts(t)
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != 1 {
		t.Errorf("ID = %d, want 1 (empty store)", res.ID)
	}
	if filepath.Base(res.Path) != "1-fix-the-thing.md" {
		t.Errorf("Path = %s, want 1-fix-the-thing.md", filepath.Base(res.Path))
	}
}

func TestCreate_IncrementsFromExistingMax(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, filepath.Join(opts.StoreRoot, "41-existing.md"), "---\nid: 41\nstatus: pending\n---\n")
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != 42 {
		t.Errorf("ID = %d, want 42", res.ID)
	}
}

func TestCreate_NeverReusesAnExistingID(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, filepath.Join(opts.StoreRoot, "5-old.md"), "---\nid: 5\nstatus: pending\n---\n")
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID == 5 {
		t.Error("must never reuse an existing id")
	}
}

func TestCreate_ClaimCollisionRetriesToNextID(t *testing.T) {
	opts := baseOpts(t)
	// Simulate a concurrent creator having already won id 1's claim marker.
	mustWrite(t, filepath.Join(opts.StoreRoot, ".claim-1"), "")
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != 2 {
		t.Errorf("ID = %d, want 2 (id 1's claim marker was already held)", res.ID)
	}
	// The pre-existing marker for 1 must be left exactly as it was (not ours to touch).
	if _, err := os.Stat(filepath.Join(opts.StoreRoot, ".claim-1")); err != nil {
		t.Error("a marker this call didn't create must not be removed")
	}
}

func TestCreate_TwoSequentialCallsGetDistinctIDs(t *testing.T) {
	opts := baseOpts(t)
	res1, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts2 := opts
	opts2.Title = "A different task"
	res2, err := Create(opts2)
	if err != nil {
		t.Fatal(err)
	}
	if res1.ID == res2.ID {
		t.Errorf("want distinct ids, got %d and %d", res1.ID, res2.ID)
	}
}

func TestCreate_NoLeftoverClaimMarkerOnSuccess(t *testing.T) {
	opts := baseOpts(t)
	if _, err := Create(opts); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(opts.StoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claim-") {
			t.Errorf("claim marker %q left behind on the happy path", e.Name())
		}
	}
}

func TestCreate_NonCanonicalTypeRefused(t *testing.T) {
	opts := baseOpts(t)
	opts.Type = "not-a-real-type"
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for a non-canonical --type")
	}
	entries, _ := os.ReadDir(opts.StoreRoot)
	if len(entries) != 0 {
		t.Errorf("refused creation must write nothing, found: %v", entries)
	}
}

func TestCreate_RequiresIssueUnlessUnparented(t *testing.T) {
	opts := baseOpts(t)
	opts.Unparented = false // no --issue given either
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error: --issue is required unless --unparented")
	}
}

func TestCreate_UnparentedAllowsNoIssue(t *testing.T) {
	opts := baseOpts(t) // Unparented: true, Parent: ""
	res, err := Create(opts)
	if err != nil {
		t.Fatalf("want --unparented to allow creation with no --issue, got: %v", err)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "parent:") {
		t.Errorf("an unparented task must not write a parent: line, got:\n%s", data)
	}
}

func TestCreate_DanglingIssueRefused(t *testing.T) {
	opts := baseOpts(t)
	opts.Unparented = false
	opts.Parent = "I999" // doesn't exist in (empty) issues/
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for a dangling --issue")
	}
}

func TestCreate_ValidIssueAccepted(t *testing.T) {
	opts := baseOpts(t)
	opts.Unparented = false
	writeIssue(t, opts.IssuesRoot, "I7")
	opts.Parent = "I7"
	res, err := Create(opts)
	if err != nil {
		t.Fatalf("want a valid --issue accepted, got: %v", err)
	}
	data, _ := os.ReadFile(res.Path)
	if !strings.Contains(string(data), "parent: I7") {
		t.Errorf("want parent: I7 in frontmatter, got:\n%s", data)
	}
}

func TestCreate_DanglingMilestoneRefusedWhenHierarchyHasMilestones(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, milestonesPath(opts.StoreRoot), "goal: g\nmilestones:\n  - id: m1\n    title: M1\n    state: pending\n")
	opts.Milestone = "m999"
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for a dangling --milestone when milestones.yaml has real milestones")
	}
}

func TestCreate_MilestoneAllowedUnvalidatedWhenNoHierarchy(t *testing.T) {
	opts := baseOpts(t)
	opts.Milestone = "m999" // no milestones.yaml at all — hierarchy not built yet
	if _, err := Create(opts); err != nil {
		t.Fatalf("want --milestone accepted unvalidated with no hierarchy, got: %v", err)
	}
}

func TestCreate_FrontmatterMatchesCanonicalShape(t *testing.T) {
	opts := baseOpts(t)
	opts.Priority = "P1"
	opts.Owner = "devops-agent"
	fixed := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = orig }()

	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"id: 1", "title: Fix the thing", "status: pending", "type: devops-agent",
		"priority: P1", "owner: devops-agent", "created: 2026-09-08", "updated: 2026-09-08", "trackers: {}",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, content)
		}
	}
	if !strings.HasPrefix(content, "---\n") {
		t.Errorf("frontmatter must open with ---, got:\n%s", content)
	}
}

func TestSlugify_IsSane(t *testing.T) {
	cases := map[string]string{
		"Fix the thing!!":        "fix-the-thing",
		"  leading/trailing  ":   "leading-trailing",
		"Already-slug-like":      "already-slug-like",
		"":                       "task",
		"!!!":                    "task",
		strings.Repeat("x", 100): strings.Repeat("x", 60),
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreate_AssignSymlinksIntoRolesPending is the role-keyed-lanes regression
// (docs/build-tasks/role-keyed-lanes.md): --assign routes into the ROLE's own
// lane (queue/devops/), not a live instance dir resolved through queue/ROLES —
// the ROLES row here ("devops devops-lane") is deliberately a DECOY, proving
// queue.Assign ignores it for the destination.
func TestCreate_AssignSymlinksIntoRolesPending(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, opts.RolesPath, "devops devops-lane\n")
	if err := os.MkdirAll(filepath.Join(opts.QueueRoot, "devops", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts.Assign = "devops"

	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Lane != "devops" {
		t.Errorf("Lane = %q, want devops (the role lane, ignoring the decoy ROLES mapping)", res.Lane)
	}
	if _, err := os.Lstat(filepath.Join(opts.QueueRoot, "devops", "pending", filepath.Base(res.Path))); err != nil {
		t.Errorf("want the new task symlinked into devops/pending: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(opts.QueueRoot, "devops-lane")); err == nil {
		t.Error("must not have touched the decoy ROLES-mapped instance dir")
	}
}

func TestCreate_AssignRefusesUnknownRole(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, opts.RolesPath, "devops devops-lane\n")
	opts.Assign = "no-such-role"
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for an unknown --assign role")
	}
}
