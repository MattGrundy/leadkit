package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog returns a waker-shaped log func and a pointer to the lines it accumulates,
// so a test asserts what the waker logged WITHOUT any real exec of a wake binary.
func captureLog() (func(string, ...any), *[]string) {
	var lines []string
	return func(format string, a ...any) {
		lines = append(lines, fmt.Sprintf(format, a...))
	}, &lines
}

func TestMaybeWake_NoWakeCommand_ExecsNothingNeverLogsFailure(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	setupSymlinkEntry(t, store, filepath.Join(root, "agentA", "pending", "t1.md"), "t1.md")

	log, lines := captureLog()
	maybeWake(root, "agentA", "", false, log)

	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "waking agentA") {
		t.Errorf("expected the arrival observability line, got: %q", joined)
	}
	if !strings.Contains(joined, "its `queue wait` will pick it up") {
		t.Errorf("expected the pending-ticket pickup line, got: %q", joined)
	}
	// The whole point: no missing binary to fail on -> never the old failure line.
	if strings.Contains(joined, "claude-wake failed") {
		t.Errorf("must NOT log a `claude-wake failed` line by default, got: %q", joined)
	}
	if strings.Contains(joined, "failed") {
		t.Errorf("must NOT log any failure by default, got: %q", joined)
	}
}

func TestMaybeWake_WithWakeCommand_ExecsWithSubstitutedPlaceholders(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	setupSymlinkEntry(t, store, filepath.Join(root, "agentA", "pending", "t1.md"), "t1.md")

	// A harmless temp script that records its args — never a real claude-wake.
	outFile := filepath.Join(t.TempDir(), "args.txt")
	script := filepath.Join(t.TempDir(), "wake.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	log, lines := captureLog()
	maybeWake(root, "agentA", script+" {agent} {msg}", false, log)

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("wake command did not run (no recorded-args file): %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "agentA") {
		t.Errorf("{agent} was not substituted; recorded args: %q", got)
	}
	if !strings.Contains(got, "Gate ticket in your inbox") {
		t.Errorf("{msg} was not substituted; recorded args: %q", got)
	}
	if strings.Contains(strings.Join(*lines, "\n"), "failed") {
		t.Errorf("a successful wake must not log a failure, got: %v", *lines)
	}
}

func TestMaybeWake_WithFailingWakeCommand_LogsFailureLeavesTicket(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "tasks")
	link := filepath.Join(root, "agentA", "pending", "t1.md")
	setupSymlinkEntry(t, store, link, "t1.md")

	log, lines := captureLog()
	// A command guaranteed to fail to start (nonexistent binary) — opt-in, so a
	// failure IS logged here (today's behaviour, now only when wake_command is set).
	maybeWake(root, "agentA", "leadkit-nonexistent-wake-binary-xyz {agent} {msg}", false, log)

	joined := strings.Join(*lines, "\n")
	if !strings.Contains(joined, "failed") {
		t.Errorf("a failing configured wake_command must log a failure, got: %q", joined)
	}
	// maybeWake never moves the ticket — it's left in place for the next pass.
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("ticket must be left in place after a wake failure: %v", err)
	}
}

func TestSubstituteWakeArgs(t *testing.T) {
	args := substituteWakeArgs("claude-wake {agent} {msg}", "agentA", "hello world msg")
	want := []string{"claude-wake", "agentA", "hello world msg"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q (a {msg} with spaces must stay one argument)", i, args[i], want[i])
		}
	}
}
