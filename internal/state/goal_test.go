package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGoal_LoadEmitRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.md")
	g := Goal{
		Title:            "Ship the thing",
		Objective:        "#123 green end to end",
		DefinitionOfDone: []string{"tests pass", "deployed"},
		Updated:          time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
	}
	if err := EmitGoal(path, g); err != nil {
		t.Fatalf("EmitGoal: %v", err)
	}

	got, err := LoadGoal(path)
	if err != nil {
		t.Fatalf("LoadGoal: %v", err)
	}
	if got.Title != g.Title || got.Objective != g.Objective || !got.Updated.Equal(g.Updated) {
		t.Errorf("got = %+v, want %+v", got, g)
	}
	if len(got.DefinitionOfDone) != 2 || got.DefinitionOfDone[0] != "tests pass" || got.DefinitionOfDone[1] != "deployed" {
		t.Errorf("DefinitionOfDone = %v, want [tests pass deployed]", got.DefinitionOfDone)
	}
}

func TestLoadGoal_MissingFileIsErrNotExist(t *testing.T) {
	_, err := LoadGoal(filepath.Join(t.TempDir(), "nope.md"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want an os.ErrNotExist-wrapping error, got %v", err)
	}
}

func TestEnsureGoal_WritesStubOnlyWhenAbsentNeverClobbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.md")

	_, created1, err := EnsureGoal(path)
	if err != nil {
		t.Fatalf("EnsureGoal (first): %v", err)
	}
	if !created1 {
		t.Fatal("want created=true on first call (no file existed)")
	}

	custom := Goal{Title: "Custom goal", Objective: "custom objective", Updated: time.Now().UTC()}
	if err := EmitGoal(path, custom); err != nil {
		t.Fatal(err)
	}

	g2, created2, err := EnsureGoal(path)
	if err != nil {
		t.Fatalf("EnsureGoal (second): %v", err)
	}
	if created2 {
		t.Fatal("want created=false — an existing goal must never be overwritten")
	}
	if g2.Title != "Custom goal" {
		t.Errorf("Title = %q, want the hand-set value to survive Ensure", g2.Title)
	}
}
