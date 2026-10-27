package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/registry"
	"mgpp.dev/leadkit/internal/usage"
)

// presenceOnly builds a RecipientDirs that consults ONLY the presence registry (no
// project/registry sources), the shape the pre-existing presence hint tests exercise.
func presenceOnly(presenceDir string, ttl time.Duration, now time.Time) RecipientDirs {
	return RecipientDirs{PresenceDir: presenceDir, PresenceTTL: ttl, Now: now}
}

// TestDeliveryHintFreshPresencePrintsLiveSessions: with a fresh presence entry for the
// recipient slug, the hint names the live session id(s).
func TestDeliveryHintFreshPresencePrintsLiveSessions(t *testing.T) {
	presenceDir := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if err := usage.WritePresenceEntry(presenceDir, "container-base", "container-base-3d", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	hint := DeliveryHint("container-base", presenceOnly(presenceDir, 15*time.Minute, now))
	if !strings.Contains(hint, "container-base-3d") {
		t.Errorf("fresh presence hint must name the live session id, got %q", hint)
	}
	if !strings.Contains(hint, "SendMessage") {
		t.Errorf("hint should point the AGENT at the nudge, got %q", hint)
	}
}

// TestDeliveryHintNoLiveSessionSaysQueued: with no live (or only stale) presence, the
// hint says the item is queued durably for the next session.
func TestDeliveryHintNoLiveSessionSaysQueued(t *testing.T) {
	presenceDir := t.TempDir()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	// A STALE entry (older than the window) must not count as live.
	if err := usage.WritePresenceEntry(presenceDir, "container-base", "old-session", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	hint := DeliveryHint("container-base", presenceOnly(presenceDir, 15*time.Minute, now))
	if !strings.Contains(hint, "no live session") || !strings.Contains(hint, "queued durably") {
		t.Errorf("stale/no presence must yield the queued-durably hint, got %q", hint)
	}

	// A slug with no presence at all → same queued message.
	empty := DeliveryHint("some-other-slug", presenceOnly(t.TempDir(), 15*time.Minute, now))
	if !strings.Contains(empty, "queued durably") {
		t.Errorf("no presence must yield the queued-durably hint, got %q", empty)
	}
}

// TestDeliveryIsHintOnlyNeverSends is the load-bearing "Go never SendMessages" assertion:
// DeliveryHint is pure — it returns TEXT and writes NOTHING (no file, no presence
// mutation, no process spawn). It is the CLI's only "delivery" action, and it is a hint.
// A static source scan additionally proves no routing source references a send primitive.
func TestDeliveryIsHintOnlyNeverSends(t *testing.T) {
	presenceDir := t.TempDir()
	if err := usage.WritePresenceEntry(presenceDir, "recv", "recv-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := dirSnapshot(t, presenceDir)

	_ = DeliveryHint("recv", presenceOnly(presenceDir, time.Hour, time.Now()))

	after := dirSnapshot(t, presenceDir)
	if before != after {
		t.Errorf("DeliveryHint must be read-only (no send, no write): dir changed\nbefore=%v\nafter=%v", before, after)
	}

	// No source file in this package may import an exec/network send MECHANISM — the
	// only way Go could ever attempt an out-of-band nudge is by shelling out or opening a
	// socket, and it does neither. (The word "SendMessage" appears only in doc comments
	// asserting this invariant, so we scan for the machinery, not the word.)
	for _, f := range goSourceFiles(t, ".") {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, banned := range []string{`"os/exec"`, "exec.Command", `"net"`, `"net/http"`} {
			if strings.Contains(src, banned) {
				t.Errorf("%s references %q — the routing substrate must never attempt to send/exec", f, banned)
			}
		}
	}
}

// TestDeliveryHint_NamesSocketAndUUID: a presence entry carrying the harness handle
// (messaging_socket + harness_session) makes DeliveryHint name the SOCKET + UUID for the
// live recipient — the SendMessage-actionable facts, not just a leadkit session id.
func TestDeliveryHint_NamesSocketAndUUID(t *testing.T) {
	presenceDir := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "container-base", "dpb-3d",
		"/tmp/cc-socks/65073.sock", "b7e19500-uuid", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	hint := DeliveryHint("container-base", presenceOnly(presenceDir, 15*time.Minute, now))
	if !strings.Contains(hint, "/tmp/cc-socks/65073.sock") {
		t.Errorf("hint must name the messaging socket, got %q", hint)
	}
	if !strings.Contains(hint, "b7e19500-uuid") {
		t.Errorf("hint must name the harness uuid, got %q", hint)
	}
	if strings.Contains(hint, "AMBIGUOUS") {
		t.Errorf("a single live session must not be marked AMBIGUOUS, got %q", hint)
	}
}

// TestDeliveryHint_AmbiguousMultipleLive: two live sessions for one slug → all listed and
// marked AMBIGUOUS, so the sender picks deliberately rather than blind-nudging one.
func TestDeliveryHint_AmbiguousMultipleLive(t *testing.T) {
	presenceDir := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "container-base", "dpb-8b",
		"/tmp/cc-socks/8b.sock", "uuid-8b", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "container-base", "dpb-84",
		"/tmp/cc-socks/84.sock", "uuid-84", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	hint := DeliveryHint("container-base", presenceOnly(presenceDir, 15*time.Minute, now))
	if !strings.Contains(hint, "AMBIGUOUS") {
		t.Fatalf("multiple live sessions must be marked AMBIGUOUS, got %q", hint)
	}
	if !strings.Contains(hint, "/tmp/cc-socks/8b.sock") || !strings.Contains(hint, "/tmp/cc-socks/84.sock") {
		t.Errorf("AMBIGUOUS hint must list BOTH sockets, got %q", hint)
	}
}

// TestDeliveryHint_BackwardSafeMissingFields: an entry written by an OLDER writer (no
// messaging_socket/harness_session) still yields a session-id hint, never a failure.
func TestDeliveryHint_BackwardSafeMissingFields(t *testing.T) {
	presenceDir := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if err := usage.WritePresenceEntry(presenceDir, "legacy-proj", "legacy-1", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	hint := DeliveryHint("legacy-proj", presenceOnly(presenceDir, 15*time.Minute, now))
	if !strings.Contains(hint, "legacy-1") {
		t.Errorf("backward-safe hint must still name the session id, got %q", hint)
	}
	if strings.Contains(hint, "socket ") {
		t.Errorf("an entry with no socket must not claim one, got %q", hint)
	}
}

// seedLead registers one lead session in a temp registry repo leading `leads`, with the
// given harness handle, fresh at now. Returns the repo path.
func seedLead(t *testing.T, host, sessionID, name string, leads []string, socket, uuid string, now time.Time) string {
	t.Helper()
	repo := t.TempDir()
	if _, _, err := registry.Open(repo).Register(registry.RegisterParams{
		SessionID: sessionID, Host: host, Name: name, Role: registry.RoleLead,
		Leads: leads, MessagingSocket: socket, HarnessSession: uuid, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestDeliveryHint_RegistryLedWorkstreamReachesLead is the CORE fix: a lead registered
// `--leads legacy-migration` whose OWN cwd/presence slug is `helm-chart-cache` must be
// the resolved recipient for `--to legacy-migration`, named by its harness handle —
// even though NO presence entry has slug==legacy-migration.
func TestDeliveryHint_RegistryLedWorkstreamReachesLead(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := seedLead(t, "mac-1", "helm-chart-cache", "helm-lead",
		[]string{"legacy-migration"}, "/tmp/cc-socks/hc.sock", "uuid-hc", now.Add(-time.Minute))

	// Presence has the lead's OWN slug (helm-chart-cache), NOT the workstream — proving the
	// resolution came from the registry, not presence.
	presenceDir := t.TempDir()
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "helm-chart-cache", "helm-chart-cache",
		"/tmp/cc-socks/hc.sock", "uuid-hc", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	d := RecipientDirs{
		RegistryRepo: repo, PresenceDir: presenceDir,
		RegistryTTL: 30 * time.Minute, PresenceTTL: 15 * time.Minute, Now: now,
	}
	hint := DeliveryHint("legacy-migration", d)
	if !strings.Contains(hint, "lead of legacy-migration") {
		t.Errorf("hint must name the workstream's lead, got %q", hint)
	}
	if !strings.Contains(hint, "uuid-hc") || !strings.Contains(hint, "/tmp/cc-socks/hc.sock") {
		t.Errorf("hint must name the lead session's uuid + socket, got %q", hint)
	}
	if strings.Contains(hint, "AMBIGUOUS") {
		t.Errorf("a single resolving lead must not be AMBIGUOUS, got %q", hint)
	}
}

// TestDeliveryHint_RegistryAndPresenceUnionDedupe: a session resolvable via BOTH the
// registry (it leads the slug) and presence (its own slug IS the slug) is listed ONCE, not
// marked AMBIGUOUS.
func TestDeliveryHint_RegistryAndPresenceUnionDedupe(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := seedLead(t, "mac-1", "container-base", "dpb-lead",
		[]string{"container-base"}, "/tmp/cc-socks/dpb.sock", "uuid-dpb", now.Add(-time.Minute))
	presenceDir := t.TempDir()
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "container-base", "container-base",
		"/tmp/cc-socks/dpb.sock", "uuid-dpb", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	d := RecipientDirs{
		RegistryRepo: repo, PresenceDir: presenceDir,
		RegistryTTL: 30 * time.Minute, PresenceTTL: 15 * time.Minute, Now: now,
	}
	hint := DeliveryHint("container-base", d)
	if strings.Contains(hint, "AMBIGUOUS") {
		t.Errorf("one session resolvable via both sources must dedupe to a single hint, got %q", hint)
	}
	if strings.Count(hint, "uuid-dpb") != 1 {
		t.Errorf("the deduped session must appear exactly once, got %q", hint)
	}
}

// TestDeliveryHint_RegistryReadFailureDegradesToPresence: an unreadable registry repo must
// NOT error the hint — it degrades to presence-only resolution.
func TestDeliveryHint_RegistryReadFailureDegradesToPresence(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	// A repo path whose sessions/ parent is a FILE, so ReadAll's ReadDir fails (not
	// IsNotExist) → ok=false → registry contributes nothing.
	tmp := t.TempDir()
	if err := writeFileAtomic(filepath.Join(tmp, "sessions"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	presenceDir := t.TempDir()
	if err := usage.WritePresenceEntryWithHarness(presenceDir, "container-base", "dpb-live",
		"/tmp/cc-socks/live.sock", "uuid-live", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	d := RecipientDirs{
		RegistryRepo: tmp, PresenceDir: presenceDir,
		RegistryTTL: 30 * time.Minute, PresenceTTL: 15 * time.Minute, Now: now,
	}
	hint := DeliveryHint("container-base", d)
	if !strings.Contains(hint, "uuid-live") {
		t.Errorf("a registry read failure must degrade to presence-only, still naming the live session, got %q", hint)
	}
}

func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		b.WriteString(path)
		if !info.IsDir() {
			b.WriteString("|")
			b.WriteString(info.ModTime().String())
			b.WriteString("|")
			data, _ := os.ReadFile(path)
			b.Write(data)
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

func goSourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
