package queue

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/roles"
)

// CANONICAL SOURCE: the adopter's scripts/assign.sh. Keep this in sync by
// hand. Unlike the read-only audit package, Assign MUTATES THE QUEUE when it
// succeeds — it's a real pipeline move, the same class as Move/Sweep above. Building
// it here is additive; nothing runs it until an operator or a later cutover step does.

// AssignError carries the exit code assign.sh itself would return for a given
// refusal (2 for usage/role/ambiguous, 1 for already-queued/symlink-fail), so the CLI
// layer can match the bash's exit-code contract exactly.
type AssignError struct {
	Code int
	Msg  string
	// AlreadyQueued is true ONLY for the "ticket is already placed in a queue"
	// refusal (Code 1) — distinct from a symlink failure (also Code 1). Callers
	// that re-run assign on a LIVE queue (orchestrate apply) use it to tell the
	// idempotent "already handled" case apart from a genuine failure.
	AlreadyQueued bool
}

func (e *AssignError) Error() string { return e.Msg }

// AssignResult is what Assign did.
type AssignResult struct {
	Role string // the resolved canonical ROLE lane (role-keyed-lanes.md) — queue/<role>/
	Base string // matched ticket filename
	Dest string // queue/<role>/pending/<base>, the new symlink
}

// knownLaneRoles is the canonical role set Assign validates/resolves `role`
// against: roles.FixedRoles unioned with the leadkit.yaml `roster:` role types
// (rosterTypes — the MODERN, sanctioned source of a project's work-roles, what
// team-standup, owner-liveness, orchestrate and the viewer all use) AND every role
// name declared in rolesPath's first column — the legacy queue/ROLES file, read
// here ONLY for its role names, as a best-effort courtesy toward projects that
// haven't moved their project-role roster into leadkit.yaml yet. Roster comes first
// so it reads as the primary extra source; KnownRoles dedups against FixedRoles and
// against ROLES either way. A nil rosterTypes and a missing/unreadable rolesPath
// both just mean no extra roles beyond FixedRoles, never a refusal — neither the
// roster nor ROLES is REQUIRED for routing on its own (see Assign's own doc comment).
func knownLaneRoles(rolesPath string, rosterTypes []string) []string {
	extra := append([]string{}, rosterTypes...)
	for _, e := range roles.List(rolesPath) {
		extra = append(extra, e.Role)
	}
	return roles.KnownRoles(extra)
}

// Assign is assign.sh: the backlog→inbox pipeline move. ROLE-KEYED LANES
// (docs/build-tasks/role-keyed-lanes.md): the destination is role's own canonical
// LANE — role, resolved through roles.CanonicalLane (the SAME name→role resolver
// send-to-review/gate-verdict/the viewer/doctor's queue-lanes check all use), not a
// role->live-instance lookup through rolesPath. This lets role itself be an
// instance-ish name too (e.g. "cr-1-3", folded to "code-review") without changing
// the destination it resolves to. rolesPath is read ONLY for its declared role
// names — an extra, best-effort source of project roles beyond roles.FixedRoles,
// for projects that haven't moved their roster into leadkit.yaml yet; a
// missing/unreadable rolesPath is not a refusal (ROLES is no longer required for
// routing at all); rosterTypes are the leadkit.yaml roster's role names, the modern
// routing source of truth, unioned into the known set alongside FixedRoles + ROLES.
// Assign finds the ticket in storeRoot by number (exactly one of
// "<ticket>-*.md" or "<ticket>.md" — a wrong guess assigns the wrong work), refuses
// if it's already placed anywhere under queueRoot outside a done/ dir, then symlinks
// it into the resolved role's pending inbox and verifies the new symlink actually
// resolves before declaring success. dryRun runs every refusal check (role, ticket
// match, dedup) unchanged but stops before the symlink itself, printing the plan
// instead — mutates nothing.
func Assign(queueRoot, storeRoot, rolesPath string, rosterTypes []string, ticket, role string, dryRun bool) (AssignResult, error) {
	known := knownLaneRoles(rolesPath, rosterTypes)
	lane, ok := roles.CanonicalLane(role, known, nil)
	if !ok {
		var b strings.Builder
		fmt.Fprintf(&b, "assign: role %q is not a canonical role. Known roles:", role)
		for _, r := range known {
			fmt.Fprintf(&b, "\n  %s", r)
		}
		return AssignResult{}, &AssignError{Code: 2, Msg: b.String()}
	}

	pending := filepath.Join(queueRoot, lane, "pending")
	if fi, err := os.Stat(pending); err != nil || !fi.IsDir() {
		return AssignResult{}, &AssignError{Code: 2, Msg: fmt.Sprintf("assign: queue dir does not exist: %s", pending)}
	}

	matches, err := findTicketFiles(storeRoot, ticket)
	if err != nil {
		return AssignResult{}, &AssignError{Code: 2, Msg: fmt.Sprintf("assign: %v", err)}
	}
	if len(matches) != 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "assign: expected exactly 1 ticket matching %q, found %d", ticket, len(matches))
		for _, m := range matches {
			fmt.Fprintf(&b, "\n  %s", m)
		}
		return AssignResult{}, &AssignError{Code: 2, Msg: b.String()}
	}
	src := matches[0]
	base := filepath.Base(src)

	existing, err := findQueuedByTicket(queueRoot, ticket)
	if err != nil {
		return AssignResult{}, &AssignError{Code: 2, Msg: fmt.Sprintf("assign: %v", err)}
	}
	if len(existing) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "assign: ticket %s is ALREADY in a queue:", ticket)
		for _, e := range existing {
			fmt.Fprintf(&b, "\n  %s", e)
		}
		b.WriteString("\nassign: refusing. Move or remove it first if reassigning.")
		return AssignResult{}, &AssignError{Code: 1, Msg: b.String(), AlreadyQueued: true}
	}

	dest := filepath.Join(pending, base)
	rel := filepath.Join("..", "..", "..", "tasks", base) // fixed overlay depth, matches queue-move's convention

	if dryRun {
		fmt.Printf("DRY RUN: assign: %s -> %s -> %s (would symlink, nothing done)\n", ticket, role, dest)
		return AssignResult{Role: lane, Base: base, Dest: dest}, nil
	}

	if err := os.Symlink(rel, dest); err != nil {
		return AssignResult{}, &AssignError{Code: 1, Msg: fmt.Sprintf("assign: symlink failed: %v", err)}
	}

	// Verify it lands AND resolves — a dangling symlink looks queued and reads as
	// nothing.
	if _, err := os.Stat(dest); err != nil {
		_ = os.Remove(dest)
		return AssignResult{}, &AssignError{Code: 1, Msg: "assign: symlink created but does NOT resolve — dangling. Removed."}
	}

	return AssignResult{Role: lane, Base: base, Dest: dest}, nil
}

// findTicketFiles returns storeRoot's direct (non-recursive) children matching
// "<ticket>-*.md" or "<ticket>.md", full-path, sorted — mirrors assign.sh's
// `find "$ROOT/tasks" -maxdepth 1 \( -name "${TICKET}-*.md" -o -name "${TICKET}.md" \)`.
func findTicketFiles(storeRoot, ticket string) ([]string, error) {
	entries, err := os.ReadDir(storeRoot)
	if err != nil {
		return nil, fmt.Errorf("read store root %s: %w", storeRoot, err)
	}
	prefix, exact := ticket+"-", ticket+".md"
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == exact || (strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".md")) {
			matches = append(matches, filepath.Join(storeRoot, name))
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// findQueuedByTicket walks queueRoot for entries (files or symlinks, at any depth, never
// directories) that ARE ticket — by identity, not by substring — excluding anything under
// a "done" dir. An entry is ticket N's when its basename is "N-<slug>..." or "N.md"; see
// isTicketEntry. A real recursive walk, not a content grep: the tree is symlinks, and
// `grep -r` doesn't follow them, so it reports nothing whether a ticket is queued once,
// three times, or not at all.
func findQueuedByTicket(queueRoot, ticket string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(queueRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == queueRoot || d.IsDir() {
			return nil
		}
		if isTicketEntry(d.Name(), ticket) && !underDoneDir(queueRoot, path) {
			found = append(found, path)
		}
		return nil
	})
	sort.Strings(found)
	return found, err
}

// isTicketEntry reports whether a queue entry's basename belongs to ticket: it is exactly
// "<ticket>.md" or starts with "<ticket>-". The separator is what makes it an identity
// comparison: "1-" never matches "10-foo.md" or "cr-1".
func isTicketEntry(name, ticket string) bool {
	return name == ticket+".md" || strings.HasPrefix(name, ticket+"-")
}

// underDoneDir reports whether path has "done" as a path component relative to root
// — equivalent to the bash's `grep -v '/done/'` (the leading/trailing slashes there
// already make it component-exact, not a loose substring match).
func underDoneDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "done" {
			return true
		}
	}
	return false
}
