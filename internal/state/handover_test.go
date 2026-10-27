package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandover_LoadEmitRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handover.md")
	h := Handover{
		Updated: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		InFlight: []InFlightItem{
			{ID: "123", Owner: "cr-1", State: "in_review", Note: "waiting on critique"},
		},
		OpenDecisions: []string{"do we cut over the waker daemon this week?"},
		RosterSnapshot: []RosterEntry{
			{Name: "cr-1", Type: "code-review-agent", LastSeen: time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)},
		},
		Notes: "nothing else outstanding",
	}
	if err := EmitHandover(path, h); err != nil {
		t.Fatalf("EmitHandover: %v", err)
	}

	got, err := LoadHandover(path)
	if err != nil {
		t.Fatalf("LoadHandover: %v", err)
	}
	if !got.Updated.Equal(h.Updated) {
		t.Errorf("Updated = %v, want %v", got.Updated, h.Updated)
	}
	if len(got.InFlight) != 1 || got.InFlight[0] != h.InFlight[0] {
		t.Errorf("InFlight = %+v, want %+v", got.InFlight, h.InFlight)
	}
	if len(got.OpenDecisions) != 1 || got.OpenDecisions[0] != h.OpenDecisions[0] {
		t.Errorf("OpenDecisions = %v, want %v", got.OpenDecisions, h.OpenDecisions)
	}
	if len(got.RosterSnapshot) != 1 || got.RosterSnapshot[0].Name != h.RosterSnapshot[0].Name || !got.RosterSnapshot[0].LastSeen.Equal(h.RosterSnapshot[0].LastSeen) {
		t.Errorf("RosterSnapshot = %+v, want %+v", got.RosterSnapshot, h.RosterSnapshot)
	}
	if got.Notes != h.Notes {
		t.Errorf("Notes = %q, want %q", got.Notes, h.Notes)
	}
}

func TestLoadHandover_MissingFileIsErrNotExist(t *testing.T) {
	_, err := LoadHandover(filepath.Join(t.TempDir(), "nope.md"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want an os.ErrNotExist-wrapping error, got %v", err)
	}
}

func TestFormatHandoverSummary_IsCompact(t *testing.T) {
	h := Handover{
		Updated:       time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		InFlight:      []InFlightItem{{ID: "1", Owner: "a", State: "s", Note: "n"}},
		OpenDecisions: []string{"d1"},
	}
	out := FormatHandoverSummary(h)
	if out == "" {
		t.Fatal("want a non-empty summary")
	}
}
