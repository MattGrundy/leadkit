package hierarchy

import (
	"os"
	"path/filepath"
	"testing"
)

func writeIssueFile(t *testing.T, issuesRoot, filename, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(issuesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(issuesRoot, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIssues_ReadsFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeIssueFile(t, root, "I1-x.md", "id: I1\ntitle: Fix the thing\nstatus: pending\nparent: dev-portal")

	issues, err := LoadIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].ID != "I1" || issues[0].Title != "Fix the thing" || issues[0].Status != "pending" || issues[0].Parent != "dev-portal" {
		t.Errorf("issue = %+v, want ID=I1 Title='Fix the thing' Status=pending Parent=dev-portal", issues[0])
	}
}

func TestLoadIssues_MissingDirIsNotAnError(t *testing.T) {
	issues, err := LoadIssues(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("want no error for a missing issues/ dir, got %v", err)
	}
	if issues != nil {
		t.Errorf("want nil, got %v", issues)
	}
}

func TestLoadIssues_IDFallsBackToFilenameWhenFrontmatterOmitsIt(t *testing.T) {
	root := t.TempDir()
	writeIssueFile(t, root, "I2-untitled.md", "status: pending")

	issues, err := LoadIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ID != "I2" {
		t.Fatalf("want ID=I2 from filename, got %+v", issues)
	}
}
