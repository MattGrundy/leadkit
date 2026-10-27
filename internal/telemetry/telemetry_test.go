package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFixture writes lines (already-formed JSON objects, one per line) to a temp
// tool-usage.jsonl and returns its path.
func writeFixture(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool-usage.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// entry builds one tool-usage.jsonl line at `ago` before now with the given tool.
func entry(now time.Time, ago time.Duration, tool string) string {
	ts := now.Add(-ago).UTC().Format(time.RFC3339)
	return `{"ts":"` + ts + `","tool":"` + tool + `"}`
}

func TestRate_FiltersToWindowAndCountsCategories(t *testing.T) {
	now := time.Now()
	path := writeFixture(t, []string{
		entry(now, 5*time.Minute, "Bash"),                                   // in window
		entry(now, 10*time.Minute, "Agent"),                                 // in window
		entry(now, 10*time.Minute, "Task"),                                  // in window, also an agent-spawn
		entry(now, 59*time.Minute, "Read"),                                  // in window
		entry(now, 90*time.Minute, "Bash"),                                  // OUTSIDE the 60m window
		`not json at all`,                                                   // corrupt line, must not blind the tick
		`{"ts":"` + now.Add(-time.Minute).UTC().Format(time.RFC3339) + `"}`, // no tool field
	})

	res, err := Rate(path, 60*time.Minute)
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if res.Total != 4 {
		t.Errorf("Total = %d, want 4", res.Total)
	}
	if res.Agents != 2 {
		t.Errorf("Agents = %d, want 2 (Agent + Task)", res.Agents)
	}
	if res.Bash != 1 {
		t.Errorf("Bash = %d, want 1 (the out-of-window Bash must not count)", res.Bash)
	}
	if res.WindowMinutes != 60 {
		t.Errorf("WindowMinutes = %d, want 60", res.WindowMinutes)
	}
	// per_hr = total*60/window_minutes = 4*60/60 = 4; agents_hr = 2*60/60 = 2.
	if res.PerHr != 4 {
		t.Errorf("PerHr = %d, want 4", res.PerHr)
	}
	if res.AgentsHr != 2 {
		t.Errorf("AgentsHr = %d, want 2", res.AgentsHr)
	}
}

func TestRate_TopOrdering(t *testing.T) {
	now := time.Now()
	var lines []string
	counts := map[string]int{"Read": 5, "Bash": 3, "Edit": 3, "Grep": 2, "Write": 1}
	// Insertion order fixes the tie-break for Bash vs Edit (both count 3): Bash
	// appears first in the fixture, so it must sort ahead in the top list.
	for _, tool := range []string{"Read", "Bash", "Edit", "Grep", "Write"} {
		for i := 0; i < counts[tool]; i++ {
			lines = append(lines, entry(now, time.Minute, tool))
		}
	}
	path := writeFixture(t, lines)

	res, err := Rate(path, 60*time.Minute)
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if len(res.Top) != 4 {
		t.Fatalf("Top has %d entries, want 4 (topN, Write excluded): %+v", len(res.Top), res.Top)
	}
	want := []ToolCount{{"Read", 5}, {"Bash", 3}, {"Edit", 3}, {"Grep", 2}}
	for i, w := range want {
		if res.Top[i] != w {
			t.Errorf("Top[%d] = %+v, want %+v", i, res.Top[i], w)
		}
	}
}

func TestRate_AbsentLogFileIsZeroNotError(t *testing.T) {
	res, err := Rate(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), 60*time.Minute)
	if err != nil {
		t.Fatalf("Rate: want nil error for a missing log, got %v", err)
	}
	if res.Total != 0 || res.Agents != 0 || res.Bash != 0 || len(res.Top) != 0 {
		t.Errorf("want a zero result for a missing log, got %+v", res)
	}
	if res.WindowMinutes != 60 {
		t.Errorf("WindowMinutes = %d, want 60 (still set even on the zero path)", res.WindowMinutes)
	}
}
