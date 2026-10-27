package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first-run path: `init` and `team scaffold` must leave the two directories doctor calls CRITICAL
// in existence, or a fresh machine cannot reach a green doctor by following the steps.
func TestCLI_InitCreatesTheRecordsDir_OnlyWhenAppliedWithConfirm(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	records := filepath.Join(root, "gate", "records")
	queue := filepath.Join(root, "proj", "queue")
	if err := os.MkdirAll(queue, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: proj/queue\nrecords_dir: "+records+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profile}
	settings := filepath.Join(root, "settings.json")
	agents := t.TempDir()
	skills := t.TempDir()
	common := []string{"--settings", settings, "--launch-agents-dir", agents, "--skills-dir", skills, "--skip-launchctl"}

	out, code := runCLI(t, bin, env, append([]string{"init"}, common...)...)
	if code != 0 || !strings.Contains(out, "records dir: "+records+" (will create)") {
		t.Fatalf("a dry run must say it would create the records dir (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(records); err == nil {
		t.Fatal("a dry run created the records dir")
	}
	if _, code := runCLI(t, bin, env, append([]string{"init", "--apply"}, common...)...); code == 0 {
		t.Fatal("--apply alone must refuse")
	}
	if _, err := os.Stat(records); err == nil {
		t.Fatal("a refused --apply created the records dir")
	}

	out, code = runCLI(t, bin, env, append([]string{"init", "--apply", "--confirm"}, common...)...)
	if code != 0 {
		t.Fatalf("init --apply --confirm exited %d:\n%s", code, out)
	}
	if fi, err := os.Stat(records); err != nil || !fi.IsDir() {
		t.Fatalf("init --apply --confirm must create the records dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "created records dir") {
		t.Errorf("init must say it created it:\n%s", out)
	}
	out, _ = runCLI(t, bin, env, append([]string{"init", "--apply", "--confirm"}, common...)...)
	if !strings.Contains(out, "records dir: "+records+" (exists)") {
		t.Errorf("a second run must find it already there:\n%s", out)
	}
}

func TestCLI_TeamScaffoldCreatesTheTaskStore(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	profile := filepath.Join(root, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nroster:\n  - type: developer\n    count: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profile}
	store := filepath.Join(root, "tasks")

	out, code := runCLI(t, bin, env, "team", "scaffold")
	if code != 0 || !strings.Contains(out, "+ mkdir "+store) {
		t.Fatalf("the dry run must show the store (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(store); err == nil {
		t.Fatal("a dry run created the store")
	}

	out, code = runCLI(t, bin, env, "team", "scaffold", "--apply", "--confirm")
	if code != 0 {
		t.Fatalf("scaffold --apply --confirm exited %d:\n%s", code, out)
	}
	if fi, err := os.Stat(store); err != nil || !fi.IsDir() {
		t.Fatalf("scaffold must create the task store: %v\n%s", err, out)
	}
	for _, want := range []string{"leadkit task create --type developer", "--assign developer"} {
		if !strings.Contains(out, want) {
			t.Errorf("the first-task hint lacks %q:\n%s", want, out)
		}
	}
}
