package routing

import (
	"fmt"
	"time"

	"mgpp.dev/leadkit/internal/registry"
	"mgpp.dev/leadkit/internal/usage"
)

// ReservedSigils are the non-project `to_slug` values the substrate accepts as valid
// addresses even though they don't name a registered project. `@review-pool` is build
// 3's central-gate destination; it is listed here (kind-agnostically) so the substrate
// accepts it now, ahead of that build, without the addressing layer needing to change.
// A sigil always begins with `@` (a project slug never does — slugs are
// [a-z0-9-]-shaped), so the two spaces can't collide.
var ReservedSigils = []string{"@review-pool"}

// IsReservedSigil reports whether slug is one of the reserved sigils.
func IsReservedSigil(slug string) bool {
	for _, s := range ReservedSigils {
		if slug == s {
			return true
		}
	}
	return false
}

// RecipientDirs bundles the account-level registries recipient VALIDATION and delivery
// RESOLUTION consult, plus the freshness windows and clock they age liveness against. All
// three are account-level (usage.Default*Dir / registry.DefaultRegistryRepo); any one may
// be "" (that source simply contributes nothing). Everything here is BEST-EFFORT — a read
// failure on any source degrades to the remaining sources, it never errors a send.
type RecipientDirs struct {
	ProjectsDir  string        // usage project registry (a registered PROJECT)
	RegistryRepo string        // git-backed lead-identity registry (a registered-LED WORKSTREAM)
	PresenceDir  string        // usage presence registry (a LIVE presence slug)
	RegistryTTL  time.Duration // liveness window for a registry entry
	PresenceTTL  time.Duration // liveness window for a presence entry
	Now          time.Time
}

// ResolveRecipient validates a `to_slug` for delivery. It is VALID (nil) when ANY of four
// kinds match:
//   - a reserved SIGIL (@review-pool …) — needs no registry;
//   - a registered PROJECT (the usage project registry);
//   - a registered-LED WORKSTREAM — some LIVE registry entry leads it (a session declared
//     `register --leads <target>`), so a workstream name is addressable even though no
//     project by that name is registered;
//   - a LIVE PRESENCE slug — some presence entry with slug==target, fresh within the TTL,
//     so a config-less active session is addressable without a full project init.
//
// It is REFUSED only when NONE match — with a message listing what was tried, so the
// sender knows which of the three registries to remedy. Best-effort throughout: a read
// failure on any single source is treated as "that source contributed no match" (never a
// hard error), so an unreadable project registry with no other match still refuses (the
// union simply found nothing) while a sigil or a live presence slug still passes.
func ResolveRecipient(toSlug string, d RecipientDirs) error {
	if toSlug == "" {
		return fmt.Errorf("routing: empty to_slug")
	}
	if IsReservedSigil(toSlug) {
		return nil
	}
	if projectRegistered(toSlug, d.ProjectsDir) {
		return nil
	}
	if registryLeadsTarget(toSlug, d.RegistryRepo, d.RegistryTTL, d.Now) {
		return nil
	}
	if livePresenceSlug(toSlug, d.PresenceDir, d.PresenceTTL, d.Now) {
		return nil
	}
	return fmt.Errorf("routing: unknown recipient %q — no project (project registry %s), no live session (presence registry), and no registered lead for it (lead-identity registry), and not a reserved sigil (%v); register the project (`leadkit init`), start a live session under that slug, or have its lead `register --leads %s`",
		toSlug, d.ProjectsDir, ReservedSigils, toSlug)
}

// projectRegistered reports whether slug names a registered project. Best-effort: an empty
// dir or an unreadable registry contributes no match (false), never an error.
func projectRegistered(slug, dir string) bool {
	if dir == "" {
		return false
	}
	entries, ok := usage.ReadProjectRegistry(dir)
	if !ok {
		return false
	}
	for _, e := range entries {
		if e.Slug == slug {
			return true
		}
	}
	return false
}

// registryLeadsTarget reports whether any LIVE registry entry leads slug. Best-effort: an
// empty repo or an unreadable/absent registry contributes no match (false), never an error.
func registryLeadsTarget(slug, repo string, ttl time.Duration, now time.Time) bool {
	for _, e := range liveRegistryEntries(repo, ttl, now) {
		for _, l := range e.LeadsOrProject() {
			if l == slug {
				return true
			}
		}
	}
	return false
}

// livePresenceSlug reports whether some presence entry with slug==slug is fresh within the
// TTL. Best-effort: an empty dir or an unreadable presence registry contributes no match.
func livePresenceSlug(slug, dir string, ttl time.Duration, now time.Time) bool {
	if dir == "" {
		return false
	}
	entries, ok := usage.ReadPresence(dir, now.Add(-ttl))
	if !ok {
		return false
	}
	for _, e := range entries {
		if e.Slug == slug {
			return true
		}
	}
	return false
}

// liveRegistryEntries reads the git-backed lead-identity registry LOCALLY (no network — it
// is a read of the on-disk sessions/ files, the same best-effort posture register/who
// follow) and returns the entries still live within ttl. An empty repo, an absent registry
// dir, or a read failure yields nil — the registry contributed nothing, the send proceeds.
func liveRegistryEntries(repo string, ttl time.Duration, now time.Time) []registry.Entry {
	if repo == "" {
		return nil
	}
	all, ok := registry.Open(repo).ReadAll()
	if !ok {
		return nil
	}
	return registry.Live(all, now.Add(-ttl))
}
