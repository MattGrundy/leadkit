package decisionrec

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppend_ThenListReturnsIt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	fixed := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return fixed }
	defer func() { nowFunc = orig }()

	if err := Append(logPath, "PVC wipe approved", "matt", "wipe stale PVCs on control", "capacity cleanup", "ticket-123", "cluster-wide"); err != nil {
		t.Fatal(err)
	}

	records, err := List(logPath, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	r := records[0]
	if r.TS != "2026-09-08T12:00:00Z" || r.What != "PVC wipe approved" || r.By != "matt" || r.Authorizes != "wipe stale PVCs on control" {
		t.Errorf("record = %+v, unexpected fields", r)
	}
}

func TestAppend_RequiresWhatAndBy(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	if err := Append(logPath, "", "matt", "", "", "", ""); err == nil {
		t.Error("want an error for a missing --what")
	}
	if err := Append(logPath, "something", "", "", "", "", ""); err == nil {
		t.Error("want an error for a missing --by")
	}
}

func TestList_FiltersByByAndRef(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	mustAppend := func(by, ref string) {
		t.Helper()
		if err := Append(logPath, "decision text", by, "", "", ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend("matt", "t1")
	mustAppend("lead", "t2")
	mustAppend("matt", "t3")

	byMatt, err := List(logPath, "matt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byMatt) != 2 {
		t.Errorf("List(by=matt) = %+v, want 2", byMatt)
	}

	byRef, err := List(logPath, "", "t2", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byRef) != 1 || byRef[0].Ref != "t2" {
		t.Errorf("List(ref=t2) = %+v, want just t2", byRef)
	}
}

func TestList_FiltersBySince(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	orig := nowFunc
	defer func() { nowFunc = orig }()

	nowFunc = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	if err := Append(logPath, "old decision", "matt", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	nowFunc = func() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) }
	if err := Append(logPath, "new decision", "matt", "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	recent, err := List(logPath, "", "", "2026-06-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].What != "new decision" {
		t.Fatalf("List(since=2026-06-01) = %+v, want just the new decision", recent)
	}
}

func TestList_AbsentLogReturnsEmptyNotError(t *testing.T) {
	records, err := List(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), "", "", "")
	if err != nil {
		t.Fatalf("want no error for an absent log, got %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want empty, got %+v", records)
	}
}
