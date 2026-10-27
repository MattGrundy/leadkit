// Package recordlog is the tiny shared append/read helper behind `leadkit spike`
// and `leadkit decision` — both are append-only logs, one JSON object per line, so
// this is the one place that shape lives. Deliberately not over-abstracted: just
// Append + ReadAll, nothing else either log needs (no locking/rotation/indexing).
package recordlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Append JSON-encodes record as one line and appends it to path, creating the
// parent dir and the file itself as needed. A normal O_APPEND write — no locking,
// same single-local-process tolerance internal/telemetry's own log already assumes
// on the read side. Never blocks: this is a local file write, not a network call.
func Append(path string, record any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recordlog: create dir for %s: %w", path, err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("recordlog: marshal record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("recordlog: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("recordlog: write %s: %w", path, err)
	}
	return nil
}

// ReadAll reads every line of path as one record of type T. A missing file returns
// nil, nil — not an error, matching every other "absence is normal" disk lens in
// this codebase (milestones.yaml, issues/, ...). One corrupt line is skipped rather
// than failing the whole read (mirrors internal/telemetry's own tolerance for a
// malformed tool-usage.jsonl line).
func ReadAll[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("recordlog: open %s: %w", path, err)
	}
	defer f.Close()

	var out []T
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec T
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // one corrupt line shouldn't blind the whole read
		}
		out = append(out, rec)
	}
	return out, nil
}
