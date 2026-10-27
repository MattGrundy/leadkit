package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateFrontmatter_ReplacesExistingPreservesOrderAppendsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.md")
	original := "---\ntitle: t\nstatus: backlog\nowner: someone\n---\n\n# body\ntext\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	err := UpdateFrontmatter(path, []KV{
		{"status", "in_review"},
		{"sha", "deadbeef"},
		{"gate", "true"},
	})
	if err != nil {
		t.Fatalf("UpdateFrontmatter: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(got), "\n")
	// title/owner are untouched keys and keep their original position; status is
	// replaced in place (not moved); sha/gate are new, appended before the closing
	// "---" in the order given.
	want := []string{
		"---", "title: t", "status: in_review", "owner: someone",
		"sha: deadbeef", "gate: true", "---", "", "# body", "text", "",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d\ngot:\n%s", len(lines), len(want), string(got))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestUpdateFrontmatter_NoFrontmatterBlockErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.md")
	if err := os.WriteFile(path, []byte("just a body, no frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFrontmatter(path, []KV{{"sha", "x"}}); err == nil {
		t.Fatal("want an error for a file with no frontmatter block")
	}
}

func TestReadFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.md")
	content := "---\ntitle: t\nstatus: backlog\n---\nbody\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, err := ReadFrontmatter(path)
	if err != nil {
		t.Fatalf("ReadFrontmatter: %v", err)
	}
	if fm["title"] != "t" || fm["status"] != "backlog" {
		t.Errorf("fm = %+v, want title=t status=backlog", fm)
	}
}
