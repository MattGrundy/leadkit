package issuecreate

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

func baseOpts(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	storeRoot := filepath.Join(root, "tasks")
	issuesRoot := filepath.Join(root, "issues")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(storeRoot, "milestones.yaml"),
		"goal: g\nmilestones:\n  - id: m1\n    title: M1\n    state: pending\n    epics:\n      - id: dev-portal\n")
	return Options{
		Title: "Fix the thing", Epic: "dev-portal",
		IssuesRoot: issuesRoot, StoreRoot: storeRoot,
	}
}

func TestCreate_FirstIssueGetsI1(t *testing.T) {
	opts := baseOpts(t)
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "I1" {
		t.Errorf("ID = %q, want I1 (empty issues dir)", res.ID)
	}
	if filepath.Base(res.Path) != "I1-fix-the-thing.md" {
		t.Errorf("Path = %s, want I1-fix-the-thing.md", filepath.Base(res.Path))
	}
}

func TestCreate_IncrementsFromExistingMax(t *testing.T) {
	opts := baseOpts(t)
	mustWrite(t, filepath.Join(opts.IssuesRoot, "I41-existing.md"), "---\nid: I41\nstatus: pending\n---\n")
	res, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "I42" {
		t.Errorf("ID = %q, want I42", res.ID)
	}
}

func TestCreate_UnknownEpicRefused(t *testing.T) {
	opts := baseOpts(t)
	opts.Epic = "no-such-epic"
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for an --epic that doesn't resolve in milestones.yaml")
	}
	entries, _ := os.ReadDir(opts.IssuesRoot)
	if len(entries) != 0 {
		t.Errorf("refused creation must write nothing, found: %v", entries)
	}
}

func TestCreate_MissingTitleRefused(t *testing.T) {
	opts := baseOpts(t)
	opts.Title = ""
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for a missing --title")
	}
}

func TestCreate_MissingEpicRefused(t *testing.T) {
	opts := baseOpts(t)
	opts.Epic = ""
	if _, err := Create(opts); err == nil {
		t.Fatal("want an error for a missing --epic")
	}
}

func TestCreate_FrontmatterMatchesCanonicalShape(t *testing.T) {
	opts := baseOpts(t)
	opts.Priority = "P1"
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
		"id: I1", "title: Fix the thing", "status: pending", "parent: dev-portal",
		"priority: P1", "created: 2026-09-08", "updated: 2026-09-08", "trackers: {}",
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
		"Fix the thing!!":      "fix-the-thing",
		"  leading/trailing  ": "leading-trailing",
		"":                     "issue",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreate_TwoSequentialCallsGetDistinctIDs(t *testing.T) {
	opts := baseOpts(t)
	res1, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res1.ID == res2.ID {
		t.Errorf("want distinct ids, both got %q", res1.ID)
	}
}
