// Package telemetry reads leadkit's own local tool-call log (~/.claude/tool-usage.jsonl)
// to report a short-window burn-rate readout — how many tool calls (and agent-spawns)
// have fired recently. This is distinct from internal/burn, which reasons about
// ACCOUNT usage percentages from Anthropic's API; Rate reads local telemetry instead,
// folding in burn-rate.sh.
package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultLogPath is the tool-call telemetry log burn-rate.sh reads.
var DefaultLogPath = filepath.Join(os.Getenv("HOME"), ".claude", "tool-usage.jsonl")

// topN is how many of the most-used tools Rate reports.
const topN = 4

// ToolCount pairs a tool name with its occurrence count within the window.
type ToolCount struct {
	Tool  string
	Count int
}

// RateResult is one Rate() tick's readout over its window.
type RateResult struct {
	WindowMinutes int
	Total         int // all entries with a non-empty tool
	Agents        int // tool == "Agent" || "Task"
	Bash          int // tool == "Bash"
	PerHr         int // Total*60/WindowMinutes, integer (matches the bash's integer arithmetic)
	AgentsHr      int // Agents*60/WindowMinutes, integer
	Top           []ToolCount
}

// logLine is one tool-usage.jsonl entry.
type logLine struct {
	Ts   time.Time `json:"ts"`
	Tool string    `json:"tool"`
}

// Rate is the burn-rate.sh equivalent: reads logPath (one JSON object per line, each
// with a `.ts` RFC3339 timestamp and a `.tool` name) and summarizes tool-call volume
// over the trailing window. A missing log file is not an error — it's a valid "no
// telemetry yet" zero result, so this never blocks a caller that treats it as advisory.
func Rate(logPath string, window time.Duration) (RateResult, error) {
	res := RateResult{WindowMinutes: int(window.Minutes())}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("telemetry: open %s: %w", logPath, err)
	}
	defer f.Close()

	cutoff := time.Now().Add(-window)
	counts := map[string]int{}
	var order []string // first-seen order — the tie-break for equal-count tools in Top

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ll logLine
		if err := json.Unmarshal(line, &ll); err != nil {
			continue // one corrupt line shouldn't blind the whole tick
		}
		if ll.Tool == "" || ll.Ts.Before(cutoff) {
			continue
		}

		res.Total++
		switch ll.Tool {
		case "Agent", "Task":
			res.Agents++
		case "Bash":
			res.Bash++
		}
		if _, seen := counts[ll.Tool]; !seen {
			order = append(order, ll.Tool)
		}
		counts[ll.Tool]++
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("telemetry: read %s: %w", logPath, err)
	}

	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	if len(order) > topN {
		order = order[:topN]
	}
	for _, tool := range order {
		res.Top = append(res.Top, ToolCount{Tool: tool, Count: counts[tool]})
	}

	if res.WindowMinutes > 0 {
		res.PerHr = res.Total * 60 / res.WindowMinutes
		res.AgentsHr = res.Agents * 60 / res.WindowMinutes
	}

	return res, nil
}
