package routing

import (
	"fmt"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/registry"
	"mgpp.dev/leadkit/internal/usage"
)

// DeliveryHint is the CLI's best-effort delivery FACILITATION — and it is ONLY a printed
// hint. The Go binary CANNOT SendMessage (that is an agent tool), so "delivery" is the
// durable write (already done by Create) PLUS this line: it resolves the recipient's LIVE
// session(s) and names them, so the sending agent knows whom to SendMessage to wake. It
// sends NOTHING itself; it returns text. If no live session, it says the item is queued
// durably for the recipient's next session — the item's durability is the source of truth,
// the nudge is convenience, and an un-hintable nudge never loses the item.
//
// Resolution ORDER is registry FIRST, then presence, so a WORKSTREAM name reaches its LEAD
// whose cwd slug differs:
//  1. REGISTRY: any LIVE registry entry that LEADS toSlug → that session (its harness
//     handle), named as the lead of the workstream. This is how a brief to a WORKSTREAM
//     reaches the lead whose own presence slug (its cwd/repo) differs from that workstream.
//  2. PRESENCE: any live presence entry whose slug==toSlug → that session. This reaches a
//     session addressed by its OWN slug (a config-less active collaborator).
//  3. Union + DEDUPE across 1+2 (one session may resolve via both); MULTIPLE distinct live
//     sessions → all listed + marked AMBIGUOUS (the sender picks deliberately).
//  4. None live → the durable-queued hint (unchanged).
//
// Best-effort throughout: a registry read failure degrades to presence-only, a presence
// read failure degrades to registry-only — neither errors. This function is deliberately
// pure-string + read-only: it is the single place the "no SendMessage from Go" invariant is
// embodied, and a test asserts its output is a hint, never an action.
func DeliveryHint(toSlug string, d RecipientDirs) string {
	if IsReservedSigil(toSlug) {
		// A sigil (e.g. @review-pool) is a logical destination, not a live session to
		// resolve — there is nothing to nudge; the durable item waits to be pulled.
		return fmt.Sprintf("nudge: %s is a routing pool (no single live session to nudge); queued durably for a puller", toSlug)
	}

	// 1. REGISTRY first: a live entry LEADING toSlug reaches the lead whose cwd slug differs.
	var (
		live []liveTarget
		seen = map[string]bool{}
	)
	for _, e := range liveRegistryEntries(d.RegistryRepo, d.RegistryTTL, d.Now) {
		if !entryLeads(e, toSlug) {
			continue
		}
		t := registryTarget(e, toSlug)
		if seen[t.key] {
			continue
		}
		seen[t.key] = true
		live = append(live, t)
	}

	// 2. PRESENCE: a live entry addressed by its OWN slug. Union + dedupe with (1).
	entries, presenceOK := usage.ReadPresence(d.PresenceDir, d.Now.Add(-d.PresenceTTL))
	if presenceOK {
		for _, e := range entries {
			if e.Slug != toSlug {
				continue
			}
			t := presenceTarget(e)
			if seen[t.key] {
				continue
			}
			seen[t.key] = true
			live = append(live, t)
		}
	}

	if len(live) == 0 {
		return fmt.Sprintf("no live session for %s; queued durably for its next session", toSlug)
	}
	// Stable order so the hint (and its tests) are deterministic across a directory read's
	// arbitrary ordering.
	sort.Slice(live, func(i, j int) bool { return live[i].key < live[j].key })

	descs := make([]string, 0, len(live))
	for _, t := range live {
		descs = append(descs, t.desc)
	}
	if len(live) == 1 {
		return fmt.Sprintf("nudge: recipient live session — %s; SendMessage that session to wake it", descs[0])
	}
	// MULTIPLE live sessions match one slug: the sender must pick deliberately rather than
	// blind-nudge one and risk MISAPPROPRIATING the message to the wrong instance.
	return fmt.Sprintf("nudge: AMBIGUOUS — %d live sessions match %s; pick the intended one deliberately: %s",
		len(live), toSlug, strings.Join(descs, "; "))
}

// liveTarget is one resolved live recipient session: a dedupe key (so a session appearing
// in BOTH the registry and presence is listed once) and the SendMessage-actionable
// description the hint prints.
type liveTarget struct {
	key  string
	desc string
}

// entryLeads reports whether registry entry e leads slug (its explicit Leads, or — for a
// lead that declared none — its own project; LeadsOrProject captures both).
func entryLeads(e registry.Entry, slug string) bool {
	for _, l := range e.LeadsOrProject() {
		if l == slug {
			return true
		}
	}
	return false
}

// sessionKey is the cross-source dedupe key for a live session: its harness UUID when
// present (the identity the SendMessage layer actually addresses), else its leadkit
// session id. The same session has the SAME value in its registry entry and its presence
// entry (both derive from resolveSessionID), so a session resolved via both dedupes to one.
func sessionKey(harnessUUID, sessionID string) string {
	if harnessUUID != "" {
		return harnessUUID
	}
	return sessionID
}

// registryTarget describes a live registry entry that leads toSlug as the workstream's
// lead, naming the harness handle (session uuid + messaging socket) the sender SendMessages.
func registryTarget(e registry.Entry, toSlug string) liveTarget {
	uuid := e.HarnessSession
	if uuid == "" {
		uuid = e.SessionID
	}
	var desc string
	switch {
	case e.MessagingSocket != "":
		desc = fmt.Sprintf("lead of %s: session %s socket %s", toSlug, uuid, e.MessagingSocket)
	default:
		// No socket recorded (older/handle-less register): name the session + host so the
		// sender can still locate it, never claim a socket it doesn't have.
		desc = fmt.Sprintf("lead of %s: session %s (host %s)", toSlug, uuid, e.Host)
	}
	return liveTarget{key: sessionKey(e.HarnessSession, e.SessionID), desc: desc}
}

// presenceTarget describes a live presence entry addressed by its own slug, reusing the
// socket/uuid handle logic the presence hint has always used.
func presenceTarget(e usage.PresenceEntry) liveTarget {
	return liveTarget{key: sessionKey(e.HarnessSession, e.SessionID), desc: describeLiveSession(e)}
}

// describeLiveSession names one live PRESENCE recipient session as a SendMessage-actionable
// handle: its harness messaging SOCKET + session UUID when the writer recorded them (the
// fields the Claude cross-session SendMessage layer actually addresses by), else a bare
// session-id hint for an older entry that predates the handle (backward-safe).
func describeLiveSession(e usage.PresenceEntry) string {
	uuid := e.HarnessSession
	if uuid == "" {
		uuid = e.SessionID
	}
	if e.MessagingSocket != "" {
		return fmt.Sprintf("socket %s (uuid %s)", e.MessagingSocket, uuid)
	}
	if e.HarnessSession != "" {
		return fmt.Sprintf("session %s (uuid %s)", e.SessionID, e.HarnessSession)
	}
	// Older writer: neither handle field present — degrade to the slug/uuid hint as before,
	// so the nudge still names a session, never fails.
	return fmt.Sprintf("session %s", e.SessionID)
}
