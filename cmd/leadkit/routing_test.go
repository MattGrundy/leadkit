package main

// End-to-end coverage of the routing substrate CLI + the routing-inbox self-surfacing
// hook, through the real binary via the mandatory runCLI/stdin sandbox. Store writes go
// to a t.TempDir routing dir (never real account state); the project/presence registries
// are seeded with usage.WriteProjectEntry/WritePresenceEntry into temp dirs.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// runHookNameCLI execs `bin hook <name>` with stdin + the mandatory sandbox, plus any
// extraEnv (LEADKIT_ROUTING_DIR / LEADKIT_SESSION_ID / LEADKIT_PROFILE overrides).
func runHookNameCLI(t *testing.T, bin, name string, extraEnv []string, stdin string) (string, int) {
	t.Helper()
	home := t.TempDir()
	overrides := map[string]string{
		"HOME":            home,
		"LEADKIT_PROFILE": filepath.Join(home, "leadkit.yaml"),
	}
	for _, kv := range extraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			overrides[k] = v
		}
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			if _, replaced := overrides[k]; replaced {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command(bin, "hook", name)
	cmd.Dir = home
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %s hook %s: %v\n%s", bin, name, err, out)
	return "", -1
}

// TestCLI_RoutingSendInboxPrune exercises the full generic lifecycle through the binary:
// addressing validation on send, a delivery hint, inbox listing/filtering, and retention.
func TestCLI_RoutingSendInboxPrune(t *testing.T) {
	bin := testBinary(t)
	routingDir := t.TempDir()
	projectsDir := t.TempDir()
	presenceDir := t.TempDir()

	// Register the recipient project + a fresh live session for it.
	if err := usage.WriteProjectEntry(projectsDir, "recv", "recv", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := usage.WritePresenceEntry(presenceDir, "recv", "recv-live-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	common := []string{
		"--routing-dir", routingDir,
		"--projects-dir", projectsDir,
		"--presence-dir", presenceDir,
	}

	// Unknown recipient → refused (addressing gate).
	out, code := runCLI(t, bin, nil, append([]string{"routing", "send", "--kind", "brief", "--to", "nope", "--from", "sender", "--subject", "x"}, common...)...)
	if code == 0 {
		t.Fatalf("send to an unregistered non-sigil slug must be refused, got 0:\n%s", out)
	}

	// Known recipient + fresh presence → created + delivery hint names the live session.
	out, code = runCLI(t, bin, nil, append([]string{"routing", "send", "--kind", "brief", "--to", "recv", "--from", "sender", "--subject", "the brief"}, common...)...)
	if code != 0 {
		t.Fatalf("send to a registered slug must succeed, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "recv-live-1") || !strings.Contains(out, "nudge:") {
		t.Errorf("send must print a delivery hint naming the live session, got:\n%s", out)
	}

	// A reserved sigil is accepted too (build 3's @review-pool).
	if _, code := runCLI(t, bin, nil, append([]string{"routing", "send", "--kind", "gate", "--to", "@review-pool", "--from", "sender", "--subject", "g"}, common...)...); code != 0 {
		t.Errorf("send to the reserved @review-pool sigil must be accepted")
	}

	// Inbox for recv lists the brief (not the sigil-addressed gate).
	out, code = runCLI(t, bin, nil, "routing", "inbox", "--slug", "recv", "--routing-dir", routingDir)
	if code != 0 || !strings.Contains(out, "the brief") {
		t.Fatalf("inbox must list recv's brief, got %d:\n%s", code, out)
	}

	// --json inbox is a stable machine shape.
	out, code = runCLI(t, bin, nil, "routing", "inbox", "--slug", "recv", "--json", "--routing-dir", routingDir)
	if code != 0 {
		t.Fatalf("inbox --json exit=%d:\n%s", code, out)
	}
	var items []routing.Envelope
	jsonLine := out[strings.Index(out, "["):]
	if err := json.Unmarshal([]byte(jsonLine), &items); err != nil {
		t.Fatalf("inbox --json is not valid JSON: %v\n%s", err, out)
	}
	if len(items) != 1 || items[0].ToSlug != "recv" {
		t.Fatalf("inbox --json shape unexpected: %+v", items)
	}

	// Prune: nothing terminal yet → nothing to prune (non-terminal items are safe).
	out, _ = runCLI(t, bin, nil, "routing", "prune", "--routing-dir", routingDir)
	if !strings.Contains(out, "nothing to prune") {
		t.Errorf("prune with only non-terminal items must remove nothing:\n%s", out)
	}
}

// TestCLI_RoutingSend_NoLiveSessionSaysQueued: with no live presence, the hint says the
// item is queued durably — and the durable write still happened (delivery is durable +
// hint, never lost to a missing nudge).
func TestCLI_RoutingSend_NoLiveSessionSaysQueued(t *testing.T) {
	bin := testBinary(t)
	routingDir := t.TempDir()
	projectsDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, "recv", "recv", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, bin, nil, "routing", "send", "--kind", "brief", "--to", "recv",
		"--from", "sender", "--subject", "s", "--routing-dir", routingDir,
		"--projects-dir", projectsDir, "--presence-dir", t.TempDir())
	if code != 0 {
		t.Fatalf("send exit=%d:\n%s", code, out)
	}
	if !strings.Contains(out, "no live session") || !strings.Contains(out, "queued durably") {
		t.Errorf("no live session must yield the queued-durably hint, got:\n%s", out)
	}
	// The item is durably on disk regardless of the un-hintable nudge.
	items, err := routing.Open(routingDir).Inbox("recv", routing.InboxFilter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("the durable write must survive a missing nudge: items=%v err=%v", items, err)
	}
}

// TestCLI_RoutingPrune_DryRunThenApplyOnlyTerminal: a terminal item past the TTL is a
// dry-run candidate then removed on --apply; a non-terminal item is never touched.
func TestCLI_RoutingPrune_DryRunThenApplyOnlyTerminal(t *testing.T) {
	bin := testBinary(t)
	routingDir := t.TempDir()
	now := time.Now().UTC()
	s := routing.Open(routingDir)

	// Terminal, closed long ago → prune-eligible under a tiny TTL.
	term, err := s.Create(routing.CreateParams{Kind: "brief", ToSlug: "recv", Status: "sent", Now: now.Add(-100 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(term.Kind, term.ToSlug, term.ID, routing.TransitionParams{
		ExpectedStatus: "sent", NewStatus: "closed", Terminal: true, Now: now.Add(-99 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Non-terminal, equally old → must survive.
	live, err := s.Create(routing.CreateParams{Kind: "brief", ToSlug: "recv", Status: "sent", Now: now.Add(-100 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	// Dry-run names the terminal item, removes nothing.
	out, code := runCLI(t, bin, nil, "routing", "prune", "--routing-dir", routingDir, "--ttl", "1h")
	if code != 0 || !strings.Contains(out, "would remove") || !strings.Contains(out, "dry-run") {
		t.Fatalf("prune dry-run must list a candidate + say dry-run, got %d:\n%s", code, out)
	}
	if _, err := s.Read(term.Kind, term.ToSlug, term.ID); err != nil {
		t.Error("dry-run must not remove anything")
	}

	// Apply removes only the terminal item.
	out, code = runCLI(t, bin, nil, "routing", "prune", "--routing-dir", routingDir, "--ttl", "1h", "--apply")
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("prune --apply exit=%d:\n%s", code, out)
	}
	if _, err := s.Read(term.Kind, term.ToSlug, term.ID); err == nil {
		t.Error("--apply must remove the terminal item past the TTL")
	}
	if _, err := s.Read(live.Kind, live.ToSlug, live.ID); err != nil {
		t.Error("--apply must NEVER remove a non-terminal item, however old")
	}
}

// TestCLI_HookRoutingInbox_SurfacesOpenItemThenDedupesAndFailsOpen drives the real
// self-surfacing hook: an open item surfaces once, a repeat is deduped, an empty store is
// silent — all fail-open (exit 0, never a block).
func TestCLI_HookRoutingInbox_SurfacesOpenItemThenDedupesAndFailsOpen(t *testing.T) {
	bin := testBinary(t)
	routingDir := t.TempDir()

	// A profile whose project_id resolves this session's slug to "recv".
	profDir := t.TempDir()
	profPath := filepath.Join(profDir, "leadkit.yaml")
	if err := os.WriteFile(profPath, []byte("project_id: recv\nqueue_root: queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An open item addressed to recv.
	if _, err := routing.Open(routingDir).Create(routing.CreateParams{
		Kind: "brief", FromSlug: "sender", ToSlug: "recv", Subject: "s", Status: "sent", Now: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"LEADKIT_PROFILE=" + profPath,
		"LEADKIT_ROUTING_DIR=" + routingDir,
		"LEADKIT_SESSION_ID=recv-1",
	}
	// The PreToolUse payload carries the session's own cwd (its project dir), so the
	// cwd-honest identity resolver derives slug "recv" from profDir/leadkit.yaml — the
	// real mechanism (a hook's payload cwd IS the session cwd).
	stdin := `{"tool_name":"Bash","tool_input":{"command":"ls"},"session_id":"recv-1","cwd":"` + profDir + `"}`

	out, code := runHookNameCLI(t, bin, "routing-inbox", env, stdin)
	if code != 0 {
		t.Fatalf("the surfacing hook must fail open (exit 0), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "open routing item") || !strings.Contains(out, "leadkit routing inbox") {
		t.Fatalf("first activity with an open item must surface the one-liner, got:\n%s", out)
	}

	// Immediate repeat (same session) → deduped silent.
	out, code = runHookNameCLI(t, bin, "routing-inbox", env, stdin)
	if code != 0 {
		t.Fatalf("dedupe path must still exit 0, got %d:\n%s", code, out)
	}
	if strings.Contains(out, "open routing item") {
		t.Errorf("a repeat within the dedupe window must be silent, got:\n%s", out)
	}

	// A session whose project has NO items → silent, exit 0 (fail-open).
	emptyEnv := []string{
		"LEADKIT_PROFILE=" + profPath,
		"LEADKIT_ROUTING_DIR=" + t.TempDir(),
		"LEADKIT_SESSION_ID=recv-9",
	}
	out, code = runHookNameCLI(t, bin, "routing-inbox", emptyEnv, stdin)
	if code != 0 || strings.Contains(out, "open routing item") {
		t.Errorf("no open items must be silent + exit 0, got %d:\n%s", code, out)
	}
}
