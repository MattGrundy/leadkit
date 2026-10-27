package recordlog

import (
	"os"
	"path/filepath"
	"testing"
)

type sample struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func TestAppendThenReadAll_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "log.jsonl")
	if err := Append(path, sample{A: "x", B: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, sample{A: "y", B: 2}); err != nil {
		t.Fatal(err)
	}

	got, err := ReadAll[sample](path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].A != "x" || got[1].B != 2 {
		t.Fatalf("got %+v, want 2 records in append order", got)
	}
}

func TestReadAll_MissingFileIsNotAnError(t *testing.T) {
	got, err := ReadAll[sample](filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("want no error for a missing log, got %v", err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestReadAll_SkipsOneCorruptLineWithoutFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	content := "{\"a\":\"x\",\"b\":1}\nnot json at all\n{\"a\":\"z\",\"b\":3}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAll[sample](path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].A != "x" || got[1].A != "z" {
		t.Fatalf("got %+v, want the 2 valid records with the corrupt line skipped", got)
	}
}
