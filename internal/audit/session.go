package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SessionTask is one ~/.claude/tasks/session-<id>/*.json file's shape.
type SessionTask struct {
	ID          string                 `json:"id"`
	Subject     string                 `json:"subject"`
	Status      string                 `json:"status"`
	Description string                 `json:"description"`
	Blocks      []string               `json:"blocks"`
	BlockedBy   []string               `json:"blockedBy"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Owner       string                 `json:"owner,omitempty"`
	ActiveForm  string                 `json:"activeForm,omitempty"`
}

// isGate reports whether t is SANCTIONED — the ephemeral gate-coordination use the
// operating model permits — iff metadata.gate == true.
func (t SessionTask) isGate() bool {
	if t.Metadata == nil {
		return false
	}
	b, _ := t.Metadata["gate"].(bool)
	return b
}

// WorkishConfig is the session-task "looks like real work, not gate coordination"
// heuristic's tunables — mirrors the profile's session_task_workish block, kept as a
// plain struct here so this package doesn't need to import internal/config.
type WorkishConfig struct {
	DescriptionLengthThreshold int
	Markers                    []string
}

// IsWorkish reports whether desc reads like real task content: over the length
// threshold, or containing one of cfg.Markers (matched case-insensitively) — a `##`
// heading, a fenced code block, or a structural marker like "acceptance"/"DoD".
//
// Exported and called DIRECTLY by internal/hook's task-mechanism guard (the active,
// at-creation-time analogue of this after-the-fact lens) — one predicate, so the
// guard and the audit can never disagree about what "workish" means.
func IsWorkish(desc string, cfg WorkishConfig) bool {
	if len(desc) > cfg.DescriptionLengthThreshold {
		return true
	}
	lower := strings.ToLower(desc)
	for _, m := range cfg.Markers {
		if m != "" && strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// SessionCounts is one session dir's task tally.
type SessionCounts struct {
	Total  int `json:"total"`
	Gate   int `json:"gate"`
	Misuse int `json:"misuse"`
}

// SessionLens is audit Lens 2: the "wrong mechanism" check. Reads every
// tasksRoot/session-*/​*.json (or just session-<sessionID> when sessionID is
// non-empty), flags each non-gate task whose description is workish, and returns
// per-session counts. A missing tasksRoot is not an error — no sessions yet is a
// valid, empty result. Never opens a transcript jsonl, only the task json files.
func SessionLens(tasksRoot, sessionID string, cfg WorkishConfig) ([]Finding, map[string]SessionCounts, error) {
	var sessionDirs []string
	if sessionID != "" {
		sessionDirs = []string{filepath.Join(tasksRoot, "session-"+sessionID)}
	} else {
		entries, err := os.ReadDir(tasksRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("audit: read session tasks root %s: %w", tasksRoot, err)
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "session-") {
				sessionDirs = append(sessionDirs, filepath.Join(tasksRoot, e.Name()))
			}
		}
	}

	var findings []Finding
	counts := map[string]SessionCounts{}

	for _, dir := range sessionDirs {
		sessionName := filepath.Base(dir)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue // best-effort — a session dir that vanished mid-audit is skipped, not fatal
		}
		var c SessionCounts
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			var t SessionTask
			if err := json.Unmarshal(data, &t); err != nil {
				continue
			}
			c.Total++
			if t.isGate() {
				c.Gate++
				continue
			}
			if IsWorkish(t.Description, cfg) {
				c.Misuse++
				findings = append(findings, Finding{
					Category: CategorySessionMisuse, Severity: SeverityWarn,
					ID:     t.ID,
					Detail: fmt.Sprintf("%s/%s: %q looks like real work, not gate-coordination (no metadata.gate) — belongs in a disk task", sessionName, f.Name(), t.Subject),
				})
			}
		}
		counts[sessionName] = c
	}

	return findings, counts, nil
}
