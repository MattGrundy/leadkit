package spikerec

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppend_ThenShowReturnsIt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "spikes.jsonl")
	fixed := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = orig }()

	if err := Append(logPath, "abc123", VerdictGreen, "I1", "release build proven", "https://argo/run/1", "clean run"); err != nil {
		t.Fatal(err)
	}

	records, err := Show(logPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	r := records[0]
	if r.TS != "2026-09-08T12:00:00Z" || r.Ref != "abc123" || r.Verdict != VerdictGreen || r.Issue != "I1" {
		t.Errorf("record = %+v, unexpected fields", r)
	}
}

func TestAppend_RequiresRefAndValidVerdict(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "spikes.jsonl")
	if err := Append(logPath, "", VerdictGreen, "", "", "", ""); err == nil {
		t.Error("want an error for a missing --ref")
	}
	if err := Append(logPath, "abc", "yellow", "", "", "", ""); err == nil {
		t.Error("want an error for an invalid --verdict")
	}
}

func TestShow_FiltersByIssueAndRef(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "spikes.jsonl")
	mustAppend := func(ref, issue string) {
		t.Helper()
		if err := Append(logPath, ref, VerdictGreen, issue, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend("sha1", "I1")
	mustAppend("sha2", "I2")
	mustAppend("sha3", "I1")

	byIssue, err := Show(logPath, "I1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byIssue) != 2 {
		t.Errorf("Show(issue=I1) = %+v, want 2", byIssue)
	}

	byRef, err := Show(logPath, "", "sha2")
	if err != nil {
		t.Fatal(err)
	}
	if len(byRef) != 1 || byRef[0].Ref != "sha2" {
		t.Errorf("Show(ref=sha2) = %+v, want just sha2", byRef)
	}
}

func TestShow_AbsentLogReturnsEmptyNotError(t *testing.T) {
	records, err := Show(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), "", "")
	if err != nil {
		t.Fatalf("want no error for an absent log, got %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want empty, got %+v", records)
	}
}
