package routing

import (
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/usage"
)

// TestResolveRecipient covers addressing validation: an unknown non-sigil slug is
// refused; a known registered slug is accepted; a reserved sigil is accepted.
func TestResolveRecipient(t *testing.T) {
	projectsDir := t.TempDir()
	if err := usage.WriteProjectEntry(projectsDir, "container-base", "docker", "/q", "/s", "/i", time.Now()); err != nil {
		t.Fatal(err)
	}
	d := RecipientDirs{ProjectsDir: projectsDir, Now: time.Now()}

	// Known project → accepted.
	if err := ResolveRecipient("container-base", d); err != nil {
		t.Errorf("known slug must be accepted: %v", err)
	}
	// Reserved sigil → accepted (build 3's @review-pool), even absent from the registry.
	if err := ResolveRecipient("@review-pool", d); err != nil {
		t.Errorf("reserved sigil must be accepted: %v", err)
	}
	// Unknown non-sigil → refused.
	if err := ResolveRecipient("no-such-project", d); err == nil {
		t.Error("an unknown non-sigil slug must be refused, not filed at an unreachable address")
	}
	// Empty → refused.
	if err := ResolveRecipient("", d); err == nil {
		t.Error("an empty slug must be refused")
	}
}

// TestResolveRecipientUnreadableRegistryFailsClosed: addressing is a correctness gate, so
// an unreadable project registry with no other matching source still refuses (the union
// found nothing) rather than accepting anything. A sigil still passes, needing no registry.
func TestResolveRecipientUnreadableRegistryFailsClosed(t *testing.T) {
	// A path whose PARENT is a file, so ReadProjectRegistry's ReadDir fails (not
	// os.IsNotExist) → ok=false → contributes no match.
	tmp := t.TempDir()
	fileAsDir := tmp + "/afile/sub"
	if err := writeFileAtomic(tmp+"/afile", []byte("x")); err != nil {
		t.Fatal(err)
	}
	d := RecipientDirs{ProjectsDir: fileAsDir, Now: time.Now()}
	if err := ResolveRecipient("anything", d); err == nil {
		t.Error("an unreadable project registry with no other source must refuse, not accept blindly")
	}
	if err := ResolveRecipient("@review-pool", d); err != nil {
		t.Errorf("a sigil needs no registry and must still pass: %v", err)
	}
}

// TestResolveRecipient_LivePresenceAddressable: a slug that is NOT a registered project but
// has a live presence entry (the config-less container-base collaborator case) is
// accepted — refused before this build, addressable now.
func TestResolveRecipient_LivePresenceAddressable(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	projectsDir := t.TempDir() // empty: NOT a registered project
	presenceDir := t.TempDir()
	if err := usage.WritePresenceEntry(presenceDir, "container-base", "dpb-live", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	d := RecipientDirs{ProjectsDir: projectsDir, PresenceDir: presenceDir, PresenceTTL: 15 * time.Minute, Now: now}
	if err := ResolveRecipient("container-base", d); err != nil {
		t.Errorf("a live-presence slug must be addressable even without a project registration: %v", err)
	}
	// A STALE presence entry (older than the TTL) must NOT make the slug addressable.
	staleDir := t.TempDir()
	if err := usage.WritePresenceEntry(staleDir, "container-base", "dpb-old", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale := RecipientDirs{ProjectsDir: projectsDir, PresenceDir: staleDir, PresenceTTL: 15 * time.Minute, Now: now}
	if err := ResolveRecipient("container-base", stale); err == nil {
		t.Error("a stale presence entry must not make a slug addressable")
	}
}

// TestResolveRecipient_RegistryLedWorkstreamAddressable: a WORKSTREAM name that no project
// registers, but that a live registry entry LEADS (`register --leads legacy-migration`),
// is accepted.
func TestResolveRecipient_RegistryLedWorkstreamAddressable(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	projectsDir := t.TempDir() // no project named legacy-migration
	repo := seedLead(t, "mac-1", "helm-chart-cache", "helm-lead",
		[]string{"legacy-migration"}, "/tmp/cc-socks/hc.sock", "uuid-hc", now.Add(-time.Minute))
	d := RecipientDirs{ProjectsDir: projectsDir, RegistryRepo: repo, RegistryTTL: 30 * time.Minute, Now: now}
	if err := ResolveRecipient("legacy-migration", d); err != nil {
		t.Errorf("a registry-led workstream must be addressable: %v", err)
	}
	// A STALE lead entry must not keep the workstream addressable.
	staleRepo := seedLead(t, "mac-1", "helm-chart-cache", "helm-lead",
		[]string{"legacy-migration"}, "", "", now.Add(-time.Hour))
	staleD := RecipientDirs{ProjectsDir: projectsDir, RegistryRepo: staleRepo, RegistryTTL: 30 * time.Minute, Now: now}
	if err := ResolveRecipient("legacy-migration", staleD); err == nil {
		t.Error("a stale registry lead must not keep a workstream addressable")
	}
}

// TestResolveRecipient_GarbageRefusedListsTried: a target matching NONE of the four kinds
// is refused, and the message names the sources that were tried.
func TestResolveRecipient_GarbageRefusedListsTried(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := seedLead(t, "mac-1", "helm-chart-cache", "helm-lead",
		[]string{"legacy-migration"}, "", "", now.Add(-time.Minute))
	d := RecipientDirs{
		ProjectsDir: t.TempDir(), RegistryRepo: repo, PresenceDir: t.TempDir(),
		RegistryTTL: 30 * time.Minute, PresenceTTL: 15 * time.Minute, Now: now,
	}
	err := ResolveRecipient("total-garbage", d)
	if err == nil {
		t.Fatal("a target matching no source must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"project", "live session", "registered lead"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must list the tried source %q, got %q", want, msg)
		}
	}
}
