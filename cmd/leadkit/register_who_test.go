package main

// End-to-end coverage of `leadkit register` / `who` / `registry prune` through the real
// binary via the mandatory runCLI sandbox. The registry repo is an explicit t.TempDir
// (--registry-repo), never real account state, and never a git repo — so with no remote
// configured these exercise the LOCAL-by-default path (register/who work with zero setup,
// no git, no warning; docs/build-tasks/registry-local-default.md).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/registry"
)

// TestCLI_Register_LocalByDefault: register on a plain local registry (no remote, no git)
// writes the local entry and prints NO git/degrade warning, exit 0 — LOCAL is the normal,
// non-degraded mode.
func TestCLI_Register_LocalByDefault(t *testing.T) {
	bin := testBinary(t)
	repo := t.TempDir()

	out, code := runCLI(t, bin, []string{"LEADKIT_SESSION_ID=s1"},
		"register", "--role", "lead", "--leads", "leadkit", "--name", "alice", "--registry-repo", repo)
	if code != 0 {
		t.Fatalf("register exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "register: alice") {
		t.Errorf("missing register line:\n%s", out)
	}
	// LOCAL mode: no "not a git repo"/sync framing, no init nag.
	if strings.Contains(out, "not a git repo") || strings.Contains(out, "sync:") || strings.Contains(out, "registry init") {
		t.Errorf("LOCAL register must not print any git/sync warning:\n%s", out)
	}
	// The local entry landed.
	entries, ok := registry.Open(repo).ReadAll()
	if !ok || len(entries) != 1 || entries[0].Name != "alice" {
		t.Fatalf("expected 1 local entry alice, got ok=%v %+v", ok, entries)
	}
}

// TestCLI_Register_RequiresRole: no --role (and not --touch/--clear) is a usage error.
func TestCLI_Register_RequiresRole(t *testing.T) {
	bin := testBinary(t)
	repo := t.TempDir()
	_, code := runCLI(t, bin, []string{"LEADKIT_SESSION_ID=s1"}, "register", "--registry-repo", repo)
	if code != 2 {
		t.Fatalf("register with no --role should exit 2, got %d", code)
	}
}

// TestCLI_Who_FlagsAmbiguousLeadership: two live leads claiming the same slug → `who`
// prints the AMBIGUOUS-LEADERSHIP banner; --json is parseable and carries the conflict.
func TestCLI_Who_FlagsAmbiguousLeadership(t *testing.T) {
	bin := testBinary(t)
	repo := t.TempDir()

	for _, sess := range []string{"s1", "s2"} {
		_, code := runCLI(t, bin, []string{"LEADKIT_SESSION_ID=" + sess},
			"register", "--role", "lead", "--leads", "leadkit", "--name", "lead-"+sess, "--registry-repo", repo)
		if code != 0 {
			t.Fatalf("register %s failed exit=%d", sess, code)
		}
	}

	out, code := runCLI(t, bin, nil, "who", "--registry-repo", repo)
	if code != 0 {
		t.Fatalf("who exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "AMBIGUOUS-LEADERSHIP") {
		t.Errorf("who should flag ambiguous leadership:\n%s", out)
	}
	// LOCAL mode: the table is clean — no degrade framing even though there is no remote.
	if strings.Contains(out, "possibly stale") || strings.Contains(out, "not initialized") {
		t.Errorf("LOCAL who must not print degrade framing:\n%s", out)
	}

	jsonOut, code := runCLI(t, bin, nil, "who", "--registry-repo", repo, "--json")
	if code != 0 {
		t.Fatalf("who --json exit=%d\n%s", code, jsonOut)
	}
	var payload struct {
		Live      []registry.Entry `json:"live"`
		Ambiguous []struct {
			Slug string `json:"Slug"`
		} `json:"ambiguous"`
		SyncOK bool `json:"sync_ok"`
	}
	// The JSON is the last line (a possibly-stale note may precede nothing here since --json
	// prints only the payload).
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &payload); err != nil {
		t.Fatalf("who --json not parseable: %v\n%s", err, jsonOut)
	}
	if len(payload.Live) != 2 {
		t.Errorf("expected 2 live entries, got %d", len(payload.Live))
	}
	if len(payload.Ambiguous) != 1 || payload.Ambiguous[0].Slug != "leadkit" {
		t.Errorf("expected ambiguous leadkit claim, got %+v", payload.Ambiguous)
	}
	// LOCAL mode is complete/non-degraded: sync_ok is TRUE (the local dir IS the registry).
	if !payload.SyncOK {
		t.Error("LOCAL registry should report sync_ok=true (complete, non-degraded)")
	}
}

// TestCLI_RegistryPrune_DryRunThenApply removes only entries past the (overridden) ttl.
func TestCLI_RegistryPrune_DryRunThenApply(t *testing.T) {
	bin := testBinary(t)
	repo := t.TempDir()

	_, code := runCLI(t, bin, []string{"LEADKIT_SESSION_ID=s1"},
		"register", "--role", "lead", "--name", "alice", "--registry-repo", repo)
	if code != 0 {
		t.Fatalf("register failed exit=%d", code)
	}

	// ttl=0 makes the just-written entry immediately stale (its age is > 0).
	dry, code := runCLI(t, bin, nil, "registry", "prune", "--ttl", "1ns", "--registry-repo", repo)
	if code != 0 {
		t.Fatalf("prune dry-run exit=%d\n%s", code, dry)
	}
	if !strings.Contains(dry, "would remove") || !strings.Contains(dry, "dry-run") {
		t.Errorf("dry-run output unexpected:\n%s", dry)
	}
	// Dry-run mutated nothing.
	if _, err := os.Stat(filepath.Join(repo, "sessions")); err != nil {
		t.Fatalf("sessions dir should still exist after dry-run: %v", err)
	}
	if entries, _ := registry.Open(repo).ReadAll(); len(entries) != 1 {
		t.Fatalf("dry-run must not remove entries, have %d", len(entries))
	}

	apply, code := runCLI(t, bin, nil, "registry", "prune", "--ttl", "1ns", "--apply", "--registry-repo", repo)
	if code != 0 {
		t.Fatalf("prune apply exit=%d\n%s", code, apply)
	}
	if !strings.Contains(apply, "removed") {
		t.Errorf("apply output should confirm removal:\n%s", apply)
	}
	if entries, _ := registry.Open(repo).ReadAll(); len(entries) != 0 {
		t.Fatalf("apply should have removed the stale entry, have %d", len(entries))
	}
}
