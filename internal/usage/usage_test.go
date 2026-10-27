package usage

import (
	"path/filepath"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/burn"
)

const fixtureJSON = `{"five_hour":{"utilization":6.0,"resets_at":"2026-08-26T11:30:00.152284+00:00"},"seven_day":{"utilization":1.0,"resets_at":"2026-08-31T05:00:00.152305+00:00"}}`

func TestParseJSON(t *testing.T) {
	got, err := ParseJSON([]byte(fixtureJSON))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if got.SessionPct != 6.0 {
		t.Errorf("SessionPct = %v, want 6.0", got.SessionPct)
	}
	if got.WeekPct != 1.0 {
		t.Errorf("WeekPct = %v, want 1.0", got.WeekPct)
	}
	wantSessionReset := time.Date(2026, 8, 26, 11, 30, 0, 152284000, time.UTC)
	if !got.SessionResetsAt.Equal(wantSessionReset) {
		t.Errorf("SessionResetsAt = %v, want %v", got.SessionResetsAt, wantSessionReset)
	}
}

func TestParseJSON_InvalidInput(t *testing.T) {
	if _, err := ParseJSON([]byte("not json")); err == nil {
		t.Fatal("want error for invalid json, got nil")
	}
}

func TestHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-history.jsonl")

	// Missing file: valid empty history, not an error.
	got, err := LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory on missing file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty history, got %d entries", len(got))
	}

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	samples := []burn.HistorySample{
		{Ts: now.Add(-30 * time.Minute), SessionPct: 5, WeekPct: 10},
		{Ts: now, SessionPct: 6, WeekPct: 11},
	}
	for _, s := range samples {
		if err := AppendHistory(path, s); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}

	got, err = LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].SessionPct != 5 || got[1].SessionPct != 6 {
		t.Fatalf("history entries out of order or wrong: %+v", got)
	}
}

func TestPruneOlderThan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-history.jsonl")
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	old := burn.HistorySample{Ts: now.Add(-48 * time.Hour), SessionPct: 1, WeekPct: 1}
	recent := burn.HistorySample{Ts: now.Add(-10 * time.Minute), SessionPct: 5, WeekPct: 5}
	if err := AppendHistory(path, old); err != nil {
		t.Fatal(err)
	}
	if err := AppendHistory(path, recent); err != nil {
		t.Fatal(err)
	}

	if err := PruneOlderThan(path, now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}

	got, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 entry after pruning, got %d: %+v", len(got), got)
	}
	if got[0].SessionPct != 5 {
		t.Fatalf("pruned the wrong entry: %+v", got)
	}
}
