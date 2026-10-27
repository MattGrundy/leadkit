package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// CANONICAL SOURCE: ~/.claude/hooks/usage-logger.sh. Keep this in sync by hand.
//
// This is the SAME file internal/telemetry.Rate reads (`leadkit rate`) — logLine's ts
// and tool field names must stay compatible with telemetry's parser.

// logLine is one tool-usage.jsonl record.
type logLine struct {
	Ts   string `json:"ts"`
	Tool string `json:"tool"`
	Cmd  string `json:"cmd"`
	Sid  string `json:"sid"`
}

// UsageLogger is usage-logger.sh: append one JSONL line to logPath. NEVER blocks —
// exit 0 even when the write itself fails (a missing dir, a permissions issue), just
// like the bash's trailing `|| true`.
func UsageLogger(in Input, logPath string) Result {
	line := logLine{
		Ts:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Tool: orPlaceholder(in.ToolName),
		Cmd:  in.ToolInput.Command,
		Sid:  orPlaceholder(in.SessionID),
	}
	if data, err := json.Marshal(line); err == nil {
		appendLine(logPath, data)
	}
	return Result{ExitCode: 0}
}

// orPlaceholder mirrors the bash's jq `// "?"` fallback for an absent field.
func orPlaceholder(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// appendLine best-effort appends data (a single JSON line) to path, creating its
// parent dir if needed. Every failure is silently swallowed — this hook never blocks.
func appendLine(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}
