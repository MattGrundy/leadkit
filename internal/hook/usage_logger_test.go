package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageLogger_AppendsAParseableLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tool-usage.jsonl")

	res := UsageLogger(Input{
		ToolName:  "Bash",
		ToolInput: toolInput{Command: "ls -la"},
		SessionID: "sess-123",
	}, logPath)

	if res.ExitCode != 0 || res.Stdout != "" || res.Stderr != "" {
		t.Errorf("UsageLogger result = %+v, want a silent exit-0 (never blocks)", res)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var line logLine
	if err := json.Unmarshal(data[:len(data)-1], &line); err != nil { // strip trailing \n
		t.Fatalf("log line is not valid JSON: %v (%q)", err, data)
	}
	if line.Tool != "Bash" || line.Cmd != "ls -la" || line.Sid != "sess-123" {
		t.Errorf("line = %+v, want Tool=Bash Cmd=\"ls -la\" Sid=sess-123", line)
	}
	if line.Ts == "" {
		t.Error("Ts is empty")
	}
}

func TestUsageLogger_AppendsMultipleLines(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tool-usage.jsonl")
	UsageLogger(Input{ToolName: "Bash", ToolInput: toolInput{Command: "one"}}, logPath)
	UsageLogger(Input{ToolName: "Read", ToolInput: toolInput{Command: ""}}, logPath)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("got %d lines, want 2", lines)
	}
}

func TestUsageLogger_MissingFieldsFallBackToPlaceholder(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tool-usage.jsonl")
	UsageLogger(Input{}, logPath) // no tool_name, no session_id, no command

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var line logLine
	if err := json.Unmarshal(data[:len(data)-1], &line); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if line.Tool != "?" || line.Sid != "?" {
		t.Errorf("line = %+v, want Tool=? Sid=? (the bash's jq `// \"?\"` fallback)", line)
	}
	if line.Cmd != "" {
		t.Errorf("Cmd = %q, want empty (cmd's fallback is \"\", not \"?\")", line.Cmd)
	}
}

func TestUsageLogger_CreatesParentDirAndNeverErrors(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nested", "dir", "tool-usage.jsonl")
	res := UsageLogger(Input{ToolName: "Bash"}, logPath)
	if res.ExitCode != 0 {
		t.Fatalf("want exit 0 even needing to create the parent dir, got %+v", res)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file was not created: %v", err)
	}
}

func TestUsageLogger_UnwritablePathNeverBlocks(t *testing.T) {
	// A path under a file (not a dir) can never be created — MkdirAll fails, the
	// write is skipped, but this must still report exit 0.
	base := t.TempDir()
	notADir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(notADir, "sub", "tool-usage.jsonl")

	res := UsageLogger(Input{ToolName: "Bash"}, logPath)
	if res.ExitCode != 0 {
		t.Errorf("UsageLogger must never block on a write failure, got %+v", res)
	}
}
