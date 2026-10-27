package main

// `spike record`/`decision record` are NOT confirm-guarded (normal ops, like `task
// create`) — the sandboxing in runCLI (HOME/cwd/LEADKIT_PROFILE) is the ONLY thing
// standing between a test/dogfood run and appending junk to the real logs. Every
// test here goes through runCLI for exactly that reason.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMinimalProfile(t *testing.T, root string) string {
	t.Helper()
	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\nspikes_log: " + filepath.Join(root, "spikes.jsonl") +
		"\ndecisions_log: " + filepath.Join(root, "decisions.jsonl") + "\nteam:\n  team_dir: team\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return profilePath
}

func TestCLI_SpikeRecordThenShow_WritesIntoTempLogOnly(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeMinimalProfile(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "spike", "record", "--ref", "abc123", "--verdict", "green", "--issue", "I1")
	if code != 0 {
		t.Fatalf("spike record: want exit 0, got %d. output:\n%s", code, out)
	}

	out, code = runCLI(t, bin, env, "spike", "show", "--issue", "I1")
	if code != 0 {
		t.Fatalf("spike show: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "green") {
		t.Errorf("spike show output missing recorded fields, got:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(root, "spikes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "abc123") {
		t.Errorf("temp log missing the record, got:\n%s", data)
	}
}

func TestCLI_SpikeRecord_InvalidVerdictRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeMinimalProfile(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "spike", "record", "--ref", "abc", "--verdict", "yellow")
	if code == 0 {
		t.Error("want a non-zero exit for an invalid --verdict")
	}
	if _, err := os.Stat(filepath.Join(root, "spikes.jsonl")); err == nil {
		t.Error("refused record must not create the log file")
	}
}

func TestCLI_DecisionRecordThenList_WritesIntoTempLogOnly(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeMinimalProfile(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "decision", "record", "--what", "PVC wipe approved", "--by", "matt", "--ref", "ticket-123")
	if code != 0 {
		t.Fatalf("decision record: want exit 0, got %d. output:\n%s", code, out)
	}

	out, code = runCLI(t, bin, env, "decision", "list", "--by", "matt")
	if code != 0 {
		t.Fatalf("decision list: want exit 0, got %d. output:\n%s", code, out)
	}
	if !strings.Contains(out, "PVC wipe approved") {
		t.Errorf("decision list output missing recorded fields, got:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(root, "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "PVC wipe approved") {
		t.Errorf("temp log missing the record, got:\n%s", data)
	}
}

func TestCLI_DecisionRecord_MissingByRefused(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profilePath := writeMinimalProfile(t, root)
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	_, code := runCLI(t, bin, env, "decision", "record", "--what", "something")
	if code == 0 {
		t.Error("want a non-zero exit for a missing --by")
	}
}
