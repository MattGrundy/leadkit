package queue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupAssignFixture builds a throwaway store+queue tree — never the real queue —
// with the code-review and critique ROLE lanes already live (role-keyed lanes:
// tasks route to queue/<role>/, not a resolved instance). rolesPath is
// deliberately pointed at a file that doesn't exist — Assign no longer requires
// queue/ROLES to exist at all.
func setupAssignFixture(t *testing.T) (queueRoot, storeRoot, rolesPath string) {
	t.Helper()
	root := t.TempDir()
	queueRoot = filepath.Join(root, "queue")
	storeRoot = filepath.Join(root, "tasks")
	for _, d := range []string{storeRoot, filepath.Join(queueRoot, "code-review", "pending"), filepath.Join(queueRoot, "critique", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rolesPath = filepath.Join(queueRoot, "ROLES")
	return queueRoot, storeRoot, rolesPath
}

func writeStoreTicket(t *testing.T, storeRoot, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(storeRoot, name), []byte("---\ntitle: t\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assignErrCode(t *testing.T, err error) int {
	t.Helper()
	var ae *AssignError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v is not an *AssignError", err)
	}
	return ae.Code
}

func TestAssign_DryRunMutatesNothing(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", true)
	if err != nil {
		t.Fatalf("Assign (dry-run): %v", err)
	}
	if res.Role != "code-review" || res.Base != "42-example.md" {
		t.Errorf("res = %+v, want the plan filled in (Role=code-review Base=42-example.md)", res)
	}
	dest := filepath.Join(queueRoot, "code-review", "pending", "42-example.md")
	if _, err := os.Lstat(dest); err == nil {
		t.Error("dry-run must not actually create the symlink")
	}
}

func TestAssign_HappyPath(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Role != "code-review" || res.Base != "42-example.md" {
		t.Errorf("res = %+v, want Role=code-review Base=42-example.md", res)
	}

	dest := filepath.Join(queueRoot, "code-review", "pending", "42-example.md")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("symlink does not resolve: %v", err)
	}
	target, err := os.Readlink(dest)
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := filepath.Join("..", "..", "..", "tasks", "42-example.md")
	if target != wantTarget {
		t.Errorf("symlink target = %q, want %q", target, wantTarget)
	}
}

func TestAssign_UnknownRoleListsKnownRoles(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")

	_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "not-a-role", false)
	if err == nil {
		t.Fatal("want refusal for an unknown role")
	}
	if got := assignErrCode(t, err); got != 2 {
		t.Errorf("code = %d, want 2", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "code-review") || !strings.Contains(msg, "critique") {
		t.Errorf("error should list known roles, got: %s", msg)
	}
	if strings.Contains(msg, "outcome-verifier") {
		t.Errorf("error lists the retired outcome-verifier as a canonical role, got: %s", msg)
	}
}

func TestAssign_ZeroMatchRefuses(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)

	_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "999", "code-review", false)
	if err == nil {
		t.Fatal("want refusal for a ticket with no match")
	}
	if got := assignErrCode(t, err); got != 2 {
		t.Errorf("code = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "found 0") {
		t.Errorf("error = %q, want it to say found 0", err.Error())
	}
}

func TestAssign_MultiMatchRefuses(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-a.md")
	writeStoreTicket(t, storeRoot, "42-b.md")

	_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err == nil {
		t.Fatal("want refusal for an ambiguous ticket number")
	}
	if got := assignErrCode(t, err); got != 2 {
		t.Errorf("code = %d, want 2", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "found 2") || !strings.Contains(msg, "42-a.md") || !strings.Contains(msg, "42-b.md") {
		t.Errorf("error should list both matches, got: %s", msg)
	}
}

func TestAssign_AlreadyQueuedRefuses(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")
	setupSymlinkEntry(t, storeRoot, filepath.Join(queueRoot, "critique", "pending", "42-example.md"), "42-example.md")

	_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err == nil {
		t.Fatal("want refusal — ticket already queued in critique/pending")
	}
	if got := assignErrCode(t, err); got != 1 {
		t.Errorf("code = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "ALREADY in a queue") {
		t.Errorf("error = %q, want it to say ALREADY in a queue", err.Error())
	}
}

func TestAssign_AlreadyInDoneDirIsAllowed(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")
	// A done/ placement (elsewhere) must NOT block a fresh assign.
	setupSymlinkEntry(t, storeRoot, filepath.Join(queueRoot, "critique", "done", "42-example.md"), "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err != nil {
		t.Fatalf("Assign: want success (done/ placements don't block), got: %v", err)
	}
	if res.Role != "code-review" {
		t.Errorf("res.Role = %q, want code-review", res.Role)
	}
}

func TestAssign_DanglingSymlinkIsCleanedUp(t *testing.T) {
	// Deliberately mismatch queueRoot/storeRoot's relative depth so the fixed
	// "../../../tasks/<base>" symlink Assign creates cannot possibly resolve —
	// exercising the post-symlink verify+cleanup path.
	root := t.TempDir()
	queueRoot := filepath.Join(root, "extra", "queue")
	storeRoot := filepath.Join(root, "tasks")
	for _, d := range []string{storeRoot, filepath.Join(queueRoot, "code-review", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rolesPath := filepath.Join(queueRoot, "ROLES")
	if err := os.WriteFile(rolesPath, []byte("code-review\tcr-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "42-example.md")

	_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err == nil {
		t.Fatal("want a dangling-symlink refusal given the mismatched depth")
	}
	if got := assignErrCode(t, err); got != 1 {
		t.Errorf("code = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "dangling") {
		t.Errorf("error = %q, want it to mention dangling", err.Error())
	}

	dest := filepath.Join(queueRoot, "code-review", "pending", "42-example.md")
	if _, lerr := os.Lstat(dest); lerr == nil {
		t.Error("the dangling symlink should have been removed, but it still exists")
	}
}

// --- role-keyed-lanes.md: the destination is the canonical ROLE lane, resolved
// via roles.CanonicalLane — never a queue/ROLES role->instance lookup. ---

// TestAssign_IgnoresROLESFileInstanceMapping proves the behaviour change directly:
// even when queue/ROLES maps code-review to a totally different live instance dir,
// Assign still routes into queue/code-review/ — the ROLES-based instance lookup is
// gone from this path entirely.
func TestAssign_IgnoresROLESFileInstanceMapping(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.WriteFile(rolesPath, []byte("code-review\tcr-9-decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "code-review", false)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Role != "code-review" {
		t.Errorf("Role = %q, want the fixed role lane code-review (the ROLES-mapped instance must be ignored)", res.Role)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-9-decoy")); err == nil {
		t.Error("must not have touched the decoy ROLES-mapped instance dir")
	}
}

// TestAssign_FoldsInstanceNameRoleArgToCanonicalLane proves the resolver's other
// job: a `role` argument that is itself an instance-ish name (as a human or an
// older script might still pass) folds to the same canonical lane a plain role
// name would.
func TestAssign_FoldsInstanceNameRoleArgToCanonicalLane(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "cr-1-3", false)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Role != "code-review" {
		t.Errorf("Role = %q, want cr-1-3 folded to the canonical code-review lane", res.Role)
	}
}

// TestAssign_ResolvesProjectOwnerRoleFromROLESDeclaredRoles is the "an
// owner-instance -> its owner role" case: a project role declared only in
// queue/ROLES's first column (not one of roles.FixedRoles) is still known to the
// resolver, and an instance name for it folds to that owner role.
func TestAssign_ResolvesProjectOwnerRoleFromROLESDeclaredRoles(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.WriteFile(rolesPath, []byte("ci-infra\tci-infra-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(queueRoot, "ci-infra", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "ci-infra-1", false)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if res.Role != "ci-infra" {
		t.Errorf("Role = %q, want ci-infra-1 folded to its declared owner role ci-infra", res.Role)
	}
}

// --- routing-consistency.md Fix 1: Assign knows the leadkit.yaml roster ---

// TestAssign_RoutesToRosterOnlyRole is the #123 case: a role declared in the
// leadkit.yaml roster (passed as rosterTypes) but NOT in the legacy queue/ROLES
// file must still route to queue/<role>/pending. FAILS BEFORE (Assign never read
// the roster), PASSES AFTER. rolesPath is deliberately absent — the roster alone
// is enough to make the role known.
func TestAssign_RoutesToRosterOnlyRole(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.MkdirAll(filepath.Join(queueRoot, "ci-pipeline", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "1453-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, []string{"ci-pipeline"}, "1453", "ci-pipeline", false)
	if err != nil {
		t.Fatalf("Assign to a roster-only role should succeed, got: %v", err)
	}
	if res.Role != "ci-pipeline" {
		t.Errorf("Role = %q, want ci-pipeline", res.Role)
	}
	if _, err := os.Stat(filepath.Join(queueRoot, "ci-pipeline", "pending", "1453-example.md")); err != nil {
		t.Errorf("ticket should be routed into queue/ci-pipeline/pending: %v", err)
	}
}

// TestAssign_RosterOnlyRoleRefusedWithoutRoster is the fails-before half made
// explicit: the SAME role, with no rosterTypes and not in ROLES, is refused — proof
// the roster is what makes the difference (and that Assign never routes an unknown
// role to a wrong lane).
func TestAssign_RosterOnlyRoleRefusedWithoutRoster(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.MkdirAll(filepath.Join(queueRoot, "ci-pipeline", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "1453-example.md")

	if _, err := Assign(queueRoot, storeRoot, rolesPath, nil, "1453", "ci-pipeline", false); err == nil {
		t.Fatal("without the roster and without a ROLES entry, ci-pipeline must be refused (proves the roster is load-bearing)")
	}
}

// TestAssign_AgentFoldThroughRoster proves the just-shipped -agent lane fold flows
// through the roster-aware known set: `ci-pipeline-agent` folds to `ci-pipeline`
// once ci-pipeline is rostered — no per-lane config, routing to queue/ci-pipeline/.
func TestAssign_AgentFoldThroughRoster(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.MkdirAll(filepath.Join(queueRoot, "ci-pipeline", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "1453-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, []string{"ci-pipeline"}, "1453", "ci-pipeline-agent", false)
	if err != nil {
		t.Fatalf("Assign ci-pipeline-agent with ci-pipeline rostered should succeed, got: %v", err)
	}
	if res.Role != "ci-pipeline" {
		t.Errorf("Role = %q, want ci-pipeline-agent folded to the rostered ci-pipeline lane", res.Role)
	}
}

// TestAssign_RolesFileOnlyRoleStillRoutes is back-compat: a role in ONLY the legacy
// ROLES file (no rosterTypes at all) still routes — the ROLES courtesy source is
// unioned in exactly as before.
func TestAssign_RolesFileOnlyRoleStillRoutes(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.WriteFile(rolesPath, []byte("ci-infra\tci-infra-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(queueRoot, "ci-infra", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "42-example.md")

	res, err := Assign(queueRoot, storeRoot, rolesPath, nil, "42", "ci-infra", false)
	if err != nil {
		t.Fatalf("a ROLES-file-only role must still route (back-compat), got: %v", err)
	}
	if res.Role != "ci-infra" {
		t.Errorf("Role = %q, want ci-infra", res.Role)
	}
}

// TestAssign_UnknownRoleListsRosterAndRolesAndFixed: a truly-unknown role is refused
// with a message that lists the FULL known set — FixedRoles, the roster role, AND
// the ROLES-file role — so the operator sees every source, not just ROLES.
func TestAssign_UnknownRoleListsRosterAndRolesAndFixed(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	if err := os.WriteFile(rolesPath, []byte("legacy-role\tlegacy-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStoreTicket(t, storeRoot, "42-example.md")

	_, err := Assign(queueRoot, storeRoot, rolesPath, []string{"ci-pipeline"}, "42", "not-a-role", false)
	if err == nil {
		t.Fatal("want refusal for an unknown role")
	}
	msg := err.Error()
	for _, want := range []string{"code-review", "ci-pipeline", "legacy-role"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal should list %q (FixedRoles + roster + ROLES), got: %s", want, msg)
		}
	}
}

// Two ways a name can contain the ticket id without being the ticket: a lane DIRECTORY, and
// ANOTHER ticket's filename.
func TestAssign_LaneDirectoryIsNotAQueuedTicket(t *testing.T) {
	queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
	writeStoreTicket(t, storeRoot, "1-example.md")
	// Stale lanes named for instances: "cr-1" contains "1", and so do cr-2/crit-2 for "2".
	for _, d := range []string{"cr-1", "cr-2", "crit-2"} {
		if err := os.MkdirAll(filepath.Join(queueRoot, d, "pending"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1"} {
		if _, err := Assign(queueRoot, storeRoot, rolesPath, nil, id, "code-review", true); err != nil {
			t.Errorf("ticket %s was refused because of a lane directory: %v", id, err)
		}
	}
}

// The lane-directory test above uses names the identity match rejects on its own, so it
// cannot see the "a directory is never a queued ticket" rule. These directories are named
// the way a ticket file is ("N-slug"), so only the directory guard keeps them from counting.
func TestAssign_ADirectoryNamedLikeTheTicketIsNotAQueuedTicket(t *testing.T) {
	for _, dir := range []string{"1-archive", "7-wip", "1-archive/nested/1-deeper"} {
		t.Run(dir, func(t *testing.T) {
			queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
			id := strings.SplitN(filepath.Base(dir), "-", 2)[0]
			writeStoreTicket(t, storeRoot, id+"-wanted.md")
			if err := os.MkdirAll(filepath.Join(queueRoot, "critique", "pending", dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := Assign(queueRoot, storeRoot, rolesPath, nil, id, "code-review", true); err != nil {
				t.Errorf("ticket %s was refused because of a directory named %q: %v", id, dir, err)
			}
		})
	}
}

func TestAssign_AnotherTicketsFilenameIsNotThisTicket(t *testing.T) {
	cases := []struct {
		name, ticket, other string // other: a DIFFERENT ticket, already queued, whose name contains ticket
	}{
		{"1 vs 10", "1", "10-something.md"},
		{"1 vs 21", "1", "21-something.md"},
		{"7 vs title containing pr-7", "7", "3-apply-pr-7-findings.md"},
		{"2 vs title containing pr-2", "2", "6-gate-pr-2-and-pr-8.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
			writeStoreTicket(t, storeRoot, c.ticket+"-wanted.md")
			writeStoreTicket(t, storeRoot, c.other)
			setupSymlinkEntry(t, storeRoot, filepath.Join(queueRoot, "critique", "pending", c.other), c.other)

			res, err := Assign(queueRoot, storeRoot, rolesPath, nil, c.ticket, "code-review", true)
			if err != nil {
				t.Fatalf("ticket %s was refused because of ticket %q: %v", c.ticket, c.other, err)
			}
			if res.Base != c.ticket+"-wanted.md" {
				t.Errorf("res.Base = %q", res.Base)
			}
		})
	}
}

// The fix must not loosen the real check: the ticket itself, queued, is still refused —
// both the "N-slug.md" and the bare "N.md" shapes.
func TestAssign_TheTicketItselfStillRefuses(t *testing.T) {
	for _, name := range []string{"7-slug.md", "7.md"} {
		t.Run(name, func(t *testing.T) {
			queueRoot, storeRoot, rolesPath := setupAssignFixture(t)
			writeStoreTicket(t, storeRoot, name)
			setupSymlinkEntry(t, storeRoot, filepath.Join(queueRoot, "critique", "pending", name), name)
			_, err := Assign(queueRoot, storeRoot, rolesPath, nil, "7", "code-review", false)
			if err == nil || !strings.Contains(err.Error(), "ALREADY in a queue") {
				t.Fatalf("want ALREADY in a queue, got %v", err)
			}
		})
	}
}

func TestIsTicketEntry(t *testing.T) {
	cases := []struct {
		name, ticket string
		want         bool
	}{
		{"1-foo.md", "1", true}, {"1.md", "1", true},
		{"10-foo.md", "1", false}, {"21-foo.md", "1", false}, {"cr-1", "1", false},
		{"3-apply-pr-7-findings.md", "7", false}, {"11.md", "1", false},
		{"13-x.md", "13", true}, {"113-x.md", "13", false},
	}
	for _, c := range cases {
		if got := isTicketEntry(c.name, c.ticket); got != c.want {
			t.Errorf("isTicketEntry(%q,%q) = %v, want %v", c.name, c.ticket, got, c.want)
		}
	}
}
