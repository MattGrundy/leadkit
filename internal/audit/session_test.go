package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var workishFixture = WorkishConfig{DescriptionLengthThreshold: 280, Markers: []string{"##", "```", "acceptance", "DoD"}}

func writeSessionTask(t *testing.T, sessionDir, filename string, task SessionTask) {
	t.Helper()
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, filename), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLens(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-abc123")
	longDesc := strings.Repeat("x", 300)

	writeSessionTask(t, sessionDir, "1.json", SessionTask{
		ID: "1", Subject: "gate task", Description: longDesc,
		Metadata: map[string]interface{}{"gate": true},
	})
	writeSessionTask(t, sessionDir, "2.json", SessionTask{
		ID: "2", Subject: "short non-gate", Description: "fix the thing",
	})
	writeSessionTask(t, sessionDir, "3.json", SessionTask{
		ID: "3", Subject: "long non-gate", Description: longDesc,
	})
	writeSessionTask(t, sessionDir, "4.json", SessionTask{
		ID: "4", Subject: "code-fence non-gate", Description: "short but has\n```go\nfmt.Println()\n```\n",
	})

	findings, counts, err := SessionLens(root, "", workishFixture)
	if err != nil {
		t.Fatalf("SessionLens: %v", err)
	}

	flagged := map[string]bool{}
	for _, f := range findings {
		flagged[f.ID] = true
		if f.Category != CategorySessionMisuse {
			t.Errorf("category = %q, want %q", f.Category, CategorySessionMisuse)
		}
	}
	if flagged["1"] {
		t.Error("gate task (id 1) must not be flagged, regardless of description length")
	}
	if flagged["2"] {
		t.Error("short non-gate task (id 2) must not be flagged")
	}
	if !flagged["3"] {
		t.Error("long non-gate task (id 3) must be flagged")
	}
	if !flagged["4"] {
		t.Error("code-fence non-gate task (id 4) must be flagged")
	}

	c := counts["session-abc123"]
	if c.Total != 4 || c.Gate != 1 || c.Misuse != 2 {
		t.Errorf("counts = %+v, want total=4 gate=1 misuse=2", c)
	}
}

func TestSessionLens_SessionIDFilter(t *testing.T) {
	root := t.TempDir()
	writeSessionTask(t, filepath.Join(root, "session-aaa"), "1.json", SessionTask{ID: "1", Description: "short"})
	writeSessionTask(t, filepath.Join(root, "session-bbb"), "1.json", SessionTask{ID: "1", Description: "short"})

	_, counts, err := SessionLens(root, "aaa", workishFixture)
	if err != nil {
		t.Fatalf("SessionLens: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("want exactly one session in counts when -session filters, got %v", counts)
	}
	if _, ok := counts["session-aaa"]; !ok {
		t.Errorf("counts = %v, want session-aaa present", counts)
	}
}

func TestSessionLens_MissingRootIsNotAnError(t *testing.T) {
	findings, counts, err := SessionLens(filepath.Join(t.TempDir(), "does-not-exist"), "", workishFixture)
	if err != nil {
		t.Fatalf("SessionLens: want nil error for a missing root, got %v", err)
	}
	if len(findings) != 0 || len(counts) != 0 {
		t.Errorf("want empty findings/counts for a missing root, got %v / %v", findings, counts)
	}
}
