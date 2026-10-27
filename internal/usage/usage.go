// Package usage fetches live account utilization and maintains the rolling history
// log the burn governor uses to compute actual pace.
//
// Live data source: ~/.claude/usage-check.sh --json, which hits the same
// OAuth-authenticated endpoint (GET /api/oauth/usage) Claude Code itself uses
// internally, reading the token from the macOS Keychain. Shelling out to the existing,
// already-vetted script avoids adding a second place that touches the credential.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"mgpp.dev/leadkit/internal/burn"
)

// DefaultScript is the usage-check.sh path this package shells out to.
var DefaultScript = filepath.Join(os.Getenv("HOME"), ".claude", "usage-check.sh")

// DefaultHistoryPath is the rolling-log file the governor reads/appends each tick.
var DefaultHistoryPath = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "usage-history.jsonl")

// DefaultConfigDir is the account-wide directory external per-project leadkit
// configs live under — a repo that keeps NO committed leadkit.yaml is governed by
// ~/.claude/leadkit/config/<slug>.yaml here, keyed by the repo's cwd-honest identity
// slug (its git-toplevel basename). Same ~/.claude/leadkit account-state home as the
// projects/presence/registry siblings. Overridable (tests, adopters).
var DefaultConfigDir = filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "config")

// rawUsage mirrors the subset of usage-check.sh --json's output this package needs.
type rawUsage struct {
	FiveHour struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	} `json:"seven_day"`
}

// ParseJSON parses usage-check.sh --json's stdout into a burn.Usage. Exported and
// pure (no I/O) so it's directly unit-testable against fixture JSON.
func ParseJSON(data []byte) (burn.Usage, error) {
	var raw rawUsage
	if err := json.Unmarshal(data, &raw); err != nil {
		return burn.Usage{}, fmt.Errorf("usage: parse json: %w", err)
	}
	return burn.Usage{
		SessionPct:      raw.FiveHour.Utilization,
		SessionResetsAt: raw.FiveHour.ResetsAt,
		WeekPct:         raw.SevenDay.Utilization,
		WeekResetsAt:    raw.SevenDay.ResetsAt,
	}, nil
}

// FetchLive shells out to DefaultScript --json and parses the result.
func FetchLive(script string) (burn.Usage, error) {
	if script == "" {
		script = DefaultScript
	}
	out, err := exec.Command(script, "--json").Output()
	if err != nil {
		return burn.Usage{}, fmt.Errorf("usage: run %s --json: %w", script, err)
	}
	return ParseJSON(out)
}

// historyLine is the on-disk JSONL shape for one rolling-log entry.
type historyLine struct {
	Ts         time.Time `json:"ts"`
	SessionPct float64   `json:"session_pct"`
	WeekPct    float64   `json:"week_pct"`
}

// LoadHistory reads every sample from path. A missing file is not an error — an empty
// history is a valid "warming up" state.
func LoadHistory(path string) ([]burn.HistorySample, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("usage: open history %s: %w", path, err)
	}
	defer f.Close()

	var out []burn.HistorySample
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var hl historyLine
		if err := json.Unmarshal(line, &hl); err != nil {
			// One corrupt line shouldn't blind the whole tick — skip it.
			continue
		}
		out = append(out, burn.HistorySample{Ts: hl.Ts, SessionPct: hl.SessionPct, WeekPct: hl.WeekPct})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("usage: read history %s: %w", path, err)
	}
	return out, nil
}

// AppendHistory appends one sample to path, creating the file/parent dir if needed.
func AppendHistory(path string, s burn.HistorySample) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("usage: mkdir for history %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("usage: open history %s for append: %w", path, err)
	}
	defer f.Close()

	line, err := json.Marshal(historyLine{Ts: s.Ts, SessionPct: s.SessionPct, WeekPct: s.WeekPct})
	if err != nil {
		return fmt.Errorf("usage: marshal history entry: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("usage: write history %s: %w", path, err)
	}
	return nil
}

// PruneOlderThan drops history entries older than cutoff so the log doesn't grow
// unbounded. Rewrites the file; safe to call before every append.
func PruneOlderThan(path string, cutoff time.Time) error {
	samples, err := LoadHistory(path)
	if err != nil {
		return err
	}
	kept := samples[:0]
	for _, s := range samples {
		if !s.Ts.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(samples) {
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("usage: rewrite history %s: %w", path, err)
	}
	defer f.Close()
	for _, s := range kept {
		line, err := json.Marshal(historyLine{Ts: s.Ts, SessionPct: s.SessionPct, WeekPct: s.WeekPct})
		if err != nil {
			return fmt.Errorf("usage: marshal history entry: %w", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("usage: write history %s: %w", path, err)
		}
	}
	return nil
}
