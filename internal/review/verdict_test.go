package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

const verdictTestSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fakeVerdictGit is a stub gate.Git — no real git repo, no real records dir.
type fakeVerdictGit struct{ remote string }

func (f fakeVerdictGit) RevParse(repo, ref string) (string, bool) {
	if ref == verdictTestSHA || ref == "HEAD" {
		return verdictTestSHA, true
	}
	return "", false
}
func (f fakeVerdictGit) RemoteURL(repo string) string { return f.remote }

// setupVerdictFixture builds a throwaway queue/tasks tree, plus a ticket already
// sitting in the code-review ROLE lane's (queue/code-review/) pending inbox —
// where send-to-review would have left it — with a *gate.Gate wired to a throwaway
// records dir and a stub Git. Role-keyed lanes: there is deliberately NO
// queue/ROLES file here — GateVerdict routes by fixed role name alone, no
// role->instance lookup. `By` values used by the tests below (e.g. "cr-1",
// "crit-1") are agent/instance IDENTITIES, unrelated to the lane a ticket sits
// in — the gate keys its verdict rules on gate.CanonicalRole(by), never on the
// lane (role-keyed-lanes.md's hard constraint). None of this touches the real
// queue, tasks, or records dir. repo is a real (throwaway) directory — gate.
// Check's repo-exists check requires that, even though the stub Git never
// actually runs git against it.
func setupVerdictFixture(t *testing.T) (rv *Reviewer, g *gate.Gate, ticketLink string, repo string) {
	t.Helper()
	repo = t.TempDir()
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	tasksDir := filepath.Join(root, "tasks")
	for _, d := range []string{
		tasksDir,
		filepath.Join(queueRoot, "code-review", "pending"),
		filepath.Join(queueRoot, "critique", "pending"),
		filepath.Join(queueRoot, "finalise", "pending"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	base := "9-example.md"
	ticket := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + repo + "\nsha: " + verdictTestSHA + "\nstage: code-review\ngate: true\n---\n\n# body\n"
	if err := os.WriteFile(filepath.Join(tasksDir, base), []byte(ticket), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(queueRoot, "code-review", "pending", base)
	if err := os.Symlink(filepath.Join("..", "..", "..", "tasks", base), link); err != nil {
		t.Fatal(err)
	}

	rv = &Reviewer{QueueRoot: queueRoot, TasksDir: tasksDir}
	// ProjectRoot set (non-empty) so the own-origin default puts this repo IN scope —
	// the stub git reports the same origin for the project root and the target repo.
	g = &gate.Gate{RecordsDir: t.TempDir(), Git: fakeVerdictGit{remote: "github.com/org/acmecorp-product"}, ProjectRoot: repo}
	return rv, g, link, repo
}

func TestGateVerdict_DryRunMutatesNothing(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	ticketPath := filepath.Join(rv.TasksDir, "9-example.md")

	before, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1", DryRun: true})
	if err != nil {
		t.Fatalf("GateVerdict (dry-run): %v", err)
	}
	if !res.DryRun || res.Layer != "code-review" {
		t.Errorf("res = %+v, want DryRun=true Layer=code-review", res)
	}

	after, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("dry-run must not modify the ticket frontmatter:\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("dry-run must leave the ticket at its original queue location")
	}
	if _, err := g.ReadRecord(verdictTestSHA); err == nil {
		t.Error("dry-run must not write a gate record (do not call gate.Record)")
	}
}

func TestGateVerdict_DryRunSimulatesCritiqueCompletionWithoutWriting(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1", DryRun: true})
	if err != nil {
		t.Fatalf("GateVerdict (dry-run): %v", err)
	}
	if res.GuardFailed {
		t.Errorf("want the dry-run to simulate all-required-layers-ready (code-review already ready + this critique), got GuardFailed=true: %s", res.Message)
	}
	if res.NewStage != "finalise" {
		t.Errorf("NewStage = %q, want finalise", res.NewStage)
	}

	// Still nothing written beyond the code-review record seeded above — critique
	// must NOT have been added to the real record by the dry-run.
	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if _, has := rec.Layers["critique"]; has {
		t.Error("dry-run must not have written a critique layer to the real record")
	}
	if _, err := os.Lstat(critLink); err != nil {
		t.Error("dry-run must leave the ticket at its original queue location")
	}
}

func TestGateVerdict_CodeReviewReadyRoutesToNext(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if res.Layer != "code-review" {
		t.Errorf("Layer = %q, want code-review", res.Layer)
	}
	if res.NewStage != "critique" || res.NewStatus != "pending" {
		t.Errorf("NewStage/Status = %s/%s, want critique/pending", res.NewStage, res.NewStatus)
	}
	wantDest := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if res.MovedTo != wantDest {
		t.Errorf("MovedTo = %s, want %s", res.MovedTo, wantDest)
	}
	if _, err := os.Lstat(wantDest); err != nil {
		t.Errorf("ticket not at new queue location: %v", err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("old queue location should no longer exist")
	}

	fm, err := ReadFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"))
	if err != nil {
		t.Fatal(err)
	}
	if fm["stage"] != "critique" || fm["status"] != "pending" {
		t.Errorf("frontmatter not updated: %+v", fm)
	}

	rec, err := g.ReadRecord(verdictTestSHA)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if rec.Layers["code-review"].Verdict != "ready" || rec.Layers["code-review"].By != "cr-1" {
		t.Errorf("record not written correctly: %+v", rec)
	}
}

func TestGateVerdict_CritiqueReadyRoutesToFinalise(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatalf("seed code-review record: %v", err)
	}
	// Simulate the prior code-review-ready GateVerdict transition having already
	// moved the ticket into crit-1's pending.
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"stage", "critique"}, {"status", "pending"}}); err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if res.GuardFailed {
		t.Fatalf("want GuardFailed=false, got true: %s", res.Message)
	}
	if res.NewStage != "finalise" || res.NewStatus != "gated" {
		t.Errorf("NewStage/Status = %s/%s, want finalise/gated", res.NewStage, res.NewStatus)
	}
	wantDest := filepath.Join(rv.QueueRoot, "finalise", "pending", "9-example.md")
	if res.MovedTo != wantDest {
		t.Errorf("MovedTo = %s, want %s", res.MovedTo, wantDest)
	}
}

func TestGateVerdict_CritiqueReadyFinaliseNotLiveFilesToDone(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if err := os.RemoveAll(filepath.Join(rv.QueueRoot, "finalise")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	wantDest := filepath.Join(rv.QueueRoot, "crit-1", "done", "9-example.md")
	if res.MovedTo != wantDest {
		t.Errorf("MovedTo = %s, want %s", res.MovedTo, wantDest)
	}
	if !strings.Contains(res.Message, "LEAD MUST PUSH MANUALLY") {
		t.Errorf("Message = %q, want it to mention LEAD MUST PUSH MANUALLY", res.Message)
	}
}

func TestGateVerdict_ChangesNeededRoutesToOwner(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "dev-owner", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}

	// "dev-owner" is the ticket's raw owner AND a canonical role in the roster, so it
	// reconciles to itself — a bare canonical owner still routes correctly (no
	// regression from the Fix-1 owner->canonical reconciliation).
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if res.NewStage != "needs_rework" || res.NewStatus != "pending" {
		t.Errorf("NewStage/Status = %s/%s, want needs_rework/pending", res.NewStage, res.NewStatus)
	}
	wantDest := filepath.Join(rv.QueueRoot, "dev-owner", "pending", "9-example.md")
	if res.MovedTo != wantDest {
		t.Errorf("MovedTo = %s, want %s", res.MovedTo, wantDest)
	}
}

// TestGateVerdict_ChangesNeededBouncesToCanonicalOwnerLane is Fix 1 of
// docs/build-tasks/routing-resolver-hardening-2.md, the #123 bug: a raw owner field
// like "owner-ci-infra" / "ci-infra-agent" / an instance name must reconcile to its
// canonical role lane (ci-infra) via the SAME resolver assign/orchestrate use, so
// rework bounces to queue/ci-infra/pending/ where the live owner is waiting — NEVER
// a stray queue/owner-ci-infra/pending/ lane no owner watches. Fails before the fix
// (dest built from the raw owner → queue.Move into a nonexistent lane errors), passes
// after.
func TestGateVerdict_ChangesNeededBouncesToCanonicalOwnerLane(t *testing.T) {
	for _, owner := range []string{"owner-ci-infra", "ci-infra-agent", "ci-infra-1"} {
		t.Run(owner, func(t *testing.T) {
			rv, g, link, _ := setupVerdictFixture(t)
			if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", owner}}); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "ci-infra", "pending"), 0o755); err != nil {
				t.Fatal(err)
			}

			res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"ci-infra"}})
			if err != nil {
				t.Fatalf("GateVerdict: %v", err)
			}
			wantDest := filepath.Join(rv.QueueRoot, "ci-infra", "pending", "9-example.md")
			if res.MovedTo != wantDest {
				t.Errorf("MovedTo = %s, want canonical lane %s (owner %q must reconcile to ci-infra)", res.MovedTo, wantDest, owner)
			}
			if _, statErr := os.Stat(filepath.Join(rv.QueueRoot, owner)); statErr == nil {
				t.Errorf("rework created a stray lane queue/%s — must route to the canonical ci-infra lane", owner)
			}
		})
	}
}

// TestGateVerdict_ChangesNeededRefusesUnreconcilableOwner: an owner that reconciles
// to no known role (a truly unknown/ephemeral owner) is REFUSED with a clear message
// rather than silently minting a stray owner-<x> lane the ticket would strand in. The
// refusal happens BEFORE gate.Record, so nothing is written.
func TestGateVerdict_ChangesNeededRefusesUnreconcilableOwner(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", "owner-479a"}}); err != nil {
		t.Fatal(err)
	}

	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"ci-infra"}})
	if err == nil {
		t.Fatal("want refusal for an owner that reconciles to no known role")
	}
	if !strings.Contains(err.Error(), "does not reconcile to a known role") {
		t.Errorf("err = %v, want a 'does not reconcile to a known role' refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(rv.QueueRoot, "owner-479a")); statErr == nil {
		t.Error("an unreconcilable owner must not create a stray owner-479a lane")
	}
	if _, rerr := g.ReadRecord(verdictTestSHA); rerr == nil {
		t.Error("an unreconcilable owner must be refused BEFORE any gate record is written")
	}
}

func TestGateVerdict_ChangesNeededRefusesWithoutOwner(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", ""}}); err != nil {
		t.Fatal(err)
	}

	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1"}); err == nil {
		t.Fatal("want refusal for changes-needed with no owner on the ticket")
	}
}

// Ticket 173: nothing sets `owner:` after creation, so a ticket with none must still be
// reconcilable — the rework destination defaults from the ticket's `type:`, reconciled by
// the SAME roles.CanonicalLane path an explicit owner takes (the -agent fold included).
func TestGateVerdict_ChangesNeededDefaultsTheOwnerFromType(t *testing.T) {
	for _, typ := range []string{"dev-owner", "dev-owner-agent"} {
		t.Run(typ, func(t *testing.T) {
			rv, g, link, _ := setupVerdictFixture(t)
			tp := filepath.Join(rv.TasksDir, "9-example.md")
			if err := UpdateFrontmatter(tp, []KV{{"owner", ""}, {"type", typ}}); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "dev-owner", "pending"), 0o755); err != nil {
				t.Fatal(err)
			}
			res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}})
			if err != nil {
				t.Fatalf("a ticket with no owner but a type must be reconcilable: %v", err)
			}
			if want := filepath.Join(rv.QueueRoot, "dev-owner", "pending", "9-example.md"); res.MovedTo != want {
				t.Errorf("MovedTo = %s, want %s", res.MovedTo, want)
			}
		})
	}
}

// An explicit owner always wins over type: the default only fills a gap.
func TestGateVerdict_ExplicitOwnerBeatsType(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t) // owner: dev-owner
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"type", "qa-agent"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "dev-owner", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner", "qa"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(rv.QueueRoot, "dev-owner", "pending", "9-example.md"); res.MovedTo != want {
		t.Errorf("MovedTo = %s, want %s (the explicit owner, not type)", res.MovedTo, want)
	}
}

// The defaulted owner is refused, with nothing written, when its type is no known role —
// and the message says the type was what was used, so the operator is not sent hunting
// for an owner: they never set.
func TestGateVerdict_ChangesNeededRefusesATypeThatIsNoKnownRole(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", ""}, {"type", "mystery-agent"}}); err != nil {
		t.Fatal(err)
	}
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}})
	if err == nil || !strings.Contains(err.Error(), "its type") {
		t.Fatalf("want a refusal naming the type that was used as owner, got %v", err)
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Error("a refused verdict must leave the ticket in place")
	}
}

// A type that names a GATE role must not become the rework owner: code-review-agent folds
// to code-review, so the "bounce" would land in the lane that raised it and the reviewer
// would re-claim it forever (ticket 173, reviewed round 2; #117 is this shape). Refused
// loudly, naming the type and the way out, with nothing recorded or moved.
func TestGateVerdict_ChangesNeededRefusesATypeThatNamesAGateRole(t *testing.T) {
	for _, typ := range []string{"code-review-agent", "critique", "finalise", "orchestrator"} {
		t.Run(typ, func(t *testing.T) {
			rv, g, link, _ := setupVerdictFixture(t)
			if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", ""}, {"type", typ}}); err != nil {
				t.Fatal(err)
			}
			_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}})
			if err == nil || !strings.Contains(err.Error(), typ) || !strings.Contains(err.Error(), "explicit owner") {
				t.Fatalf("want a refusal naming type %q and asking for an explicit owner, got %v", typ, err)
			}
			if _, statErr := os.Lstat(link); statErr != nil {
				t.Error("a refused verdict must leave the ticket in place")
			}
			if ents, _ := os.ReadDir(g.RecordsDir); len(ents) != 0 {
				t.Errorf("a refusal must precede any gate record, found %d entries", len(ents))
			}
		})
	}
}

// The refusal is only for the DERIVED owner: an explicit owner: of a gate role is a
// deliberate claim by whoever wrote it, and is left exactly as it was.
func TestGateVerdict_ExplicitGateRoleOwnerIsNotRefusedByTheTypeGuard(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := UpdateFrontmatter(filepath.Join(rv.TasksDir, "9-example.md"), []KV{{"owner", "code-review"}, {"type", "code-review-agent"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "changes-needed", By: "cr-1", Roster: []string{"dev-owner"}}); err != nil {
		t.Fatalf("an explicit owner must not hit the type-derived guard: %v", err)
	}
}

// A cwd-relative -ticket that is NOT also resolvable against the queue root's parent: the
// cwd branch of locateTicketArg is the only thing that finds it.
func TestGateVerdict_ResolvesACwdRelativeTicketPath(t *testing.T) {
	for _, form := range []string{"with-ext", "without-ext"} {
		t.Run(form, func(t *testing.T) {
			rv, g, _, _ := setupVerdictFixture(t)
			t.Chdir(rv.QueueRoot)
			arg := filepath.Join("code-review", "pending", "9-example.md")
			if form == "without-ext" {
				arg = strings.TrimSuffix(arg, ".md")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(rv.QueueRoot), arg)); err == nil {
				t.Fatal("fixture invalid: the path must not resolve against the queue root's parent")
			}
			if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: arg, Verdict: "ready", By: "cr-1", DryRun: true}); err != nil {
				t.Fatalf("a cwd-relative ticket path must resolve: %v", err)
			}
		})
	}
}

// -ticket without the .md extension resolves the ticket rather than reporting it missing:
// both the absolute and the queue/... forms are accepted.
func TestGateVerdict_AcceptsATicketPathWithoutTheMDExtension(t *testing.T) {
	for _, form := range []string{"absolute", "queue-relative"} {
		t.Run(form, func(t *testing.T) {
			rv, g, link, _ := setupVerdictFixture(t)
			arg := strings.TrimSuffix(link, ".md")
			if form == "queue-relative" {
				arg = filepath.Join("queue", "code-review", "pending", "9-example")
			}
			res, err := rv.GateVerdict(g, VerdictOpts{Ticket: arg, Verdict: "ready", By: "cr-1", DryRun: true})
			if err != nil {
				t.Fatalf("extensionless %s ticket path must resolve: %v", form, err)
			}
			if res.Layer != "code-review" {
				t.Errorf("layer = %q, want code-review", res.Layer)
			}
		})
	}
}

func TestGateVerdict_MissingTicketStillRefuses(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link + "-nope", Verdict: "ready", By: "cr-1", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "ticket not found") {
		t.Fatalf("a ticket that exists under neither spelling must still be refused as not found, got %v", err)
	}
}

func TestGateVerdict_AlreadyGatedIsNoOp(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "critique", Verdict: "ready", By: "crit-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if !res.NoOp {
		t.Fatal("want NoOp=true for an already-fully-gated sha")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("ticket should still be at its original location: %v", err)
	}
}

func TestGateVerdict_RefusesCrRoleRecordingCritique(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}

	// cr-2 canonicalizes to "cr" — a cr-role agent may never record critique.
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "cr-2", Layer: "critique"}); err == nil {
		t.Fatal("want refusal — a cr-role by can never record critique")
	}
}

func TestGateVerdict_RefusesSameNonCrRoleRecordingBothLayers(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	// An unusual setup: code-review recorded by a role that isn't cr/crit/ov at all.
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "custom-role-a", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}

	// custom-role-a canonicalizes to itself — same role as the code-review recorder,
	// even though neither is the literal "cr" role.
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "custom-role-a", Layer: "critique"}); err == nil {
		t.Fatal("want refusal — same role recorded both layers")
	}
}

// With a three-layer chain a ready is handed to the NEXT layer's lane, not to finalise, until the
// last layer is ready: the critique verdict does not end the chain, so the ticket must reach the
// security reviewer, and only that verdict sends it on.
func TestGateVerdict_AReadyOnAMiddleLayerHandsTheTicketToTheNextLayer(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	g.Chain = []string{"code-review", "critique", "security"}
	g.ReviewRoles = []string{"security"}
	if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "security", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}

	step := func(from, layer, by string) string {
		t.Helper()
		res, err := rv.GateVerdict(g, VerdictOpts{Ticket: from, Verdict: "ready", By: by})
		if err != nil {
			t.Fatalf("%s verdict by %s: %v", layer, by, err)
		}
		if res.Layer != layer {
			t.Fatalf("derived layer = %q, want %q (the first of the chain not yet ready)", res.Layer, layer)
		}
		return res.MovedTo
	}
	critLink := step(link, "code-review", "cr-1")
	secLink := step(critLink, "critique", "crit-1")
	if want := filepath.Join(rv.QueueRoot, "security", "pending", "9-example.md"); secLink != want {
		t.Fatalf("critique ready routed to %s, want %s: security is still to come", secLink, want)
	}
	if fm, _ := ReadFrontmatter(filepath.Join(rv.TasksDir, "9-example.md")); fm["stage"] != "security" {
		t.Errorf("stage = %q, want security", fm["stage"])
	}
	if ok, _ := g.Check(verdictTestSHA, repo); ok {
		t.Fatal("two of three layers must not satisfy the gate")
	}
	done := step(secLink, "security", "sec-1")
	if want := filepath.Join(rv.QueueRoot, "finalise", "pending", "9-example.md"); done != want {
		t.Errorf("the last layer ready routed to %s, want %s", done, want)
	}
	if ok, reason := g.Check(verdictTestSHA, repo); !ok {
		t.Errorf("all three layers ready must satisfy the gate: %s", reason)
	}
}

// A dry run on the last layer asks whether the chain WOULD be complete, which for three layers
// depends on all three.
func TestGateVerdict_DryRunSimulatesTheWholeChain(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	g.Chain = []string{"code-review", "critique", "security"}
	g.ReviewRoles = []string{"security"}
	for _, l := range []struct{ layer, by string }{{"code-review", "cr-1"}, {"critique", "crit-1"}} {
		if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: l.layer, Verdict: "ready", By: l.by, Repo: repo}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.GuardFailed || res.NewStage != "finalise" {
		t.Errorf("security ready on top of the two would complete the chain: %+v", res)
	}
	g.Chain = []string{"code-review", "critique", "security", "outcome"}
	g.ReviewRoles = []string{"security", "outcome"}
	res, err = rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewStage != "outcome" {
		t.Errorf("with a fourth layer security ready hands on to it, got %+v", res)
	}
}

// The dry run judges completeness against THIS repo's chain, not the default pair: a chain of one
// declared layer is complete when that layer is ready, and is not when it is merely the pair that is.
func TestGateVerdict_DryRunJudgesCompletenessAgainstTheReposChain(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	g.Chain = []string{"security"}
	g.ReviewRoles = []string{"security"}
	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.GuardFailed || res.NewStage != "finalise" {
		t.Errorf("security alone completes the chain [security]: %+v", res)
	}
	g.Chain = []string{"security", "code-review"}
	res, err = rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "sec-1", Layer: "security", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewStage != "code-review" {
		t.Errorf("a chain of [security code-review] hands security on to code-review: %+v", res)
	}
}

// TestGateVerdict_RespectsProfileScopeConfig is gate-scope-config's "no missed
// call site" proof for GateVerdict specifically: GateVerdict's own scope-dependent
// behavior (via g.Check) must react to the Gate's enforced_remotes, same as
// `gate check`/`gate check-push` do. (The CLI supplies GateForRepo so that Gate is the
// TICKET'S repo's own; see TestGateVerdict_JudgesTheTicketsRepoWithItsOwnGate.)
func TestGateVerdict_RespectsProfileScopeConfig(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	// setupVerdictFixture's stub Git reports remote "github.com/org/acmecorp-product"
	// — in scope because it is the project's own origin (ProjectRoot set). Simulate an
	// UNPROFILED gate (no ProjectRoot, so no own origin) whose enforced_remotes names a
	// DIFFERENT repo entirely (what cmd/leadkit's newGate would wire from
	// prof.EnforcedRemotes), so acmecorp-product is OUT of scope for this Gate
	// specifically. ProjectRoot must be cleared: a project's own origin is gated
	// whatever its list names.
	g.EnforcedRemotes = []string{"another-project"}
	g.ProjectRoot = ""

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if !res.NoOp {
		t.Errorf("want NoOp=true — the repo is now out-of-scope-allow per g.EnforcedRemotes (gate.Check already reports 'gated'), got %+v", res)
	}
	if !strings.Contains(res.Message, "out of scope") {
		t.Errorf("Message = %q, want it to say the repo reads as out of scope", res.Message)
	}
	// Nothing should have been recorded — the NoOp branch never calls gate.Record.
	if _, err := g.ReadRecord(verdictTestSHA); err == nil {
		t.Error("want no record written — the NoOp/out-of-scope branch must not record anything")
	}
}

// The ticket names a repo that need not be the session's. A Gate scoped by the SESSION's
// profile would call that repo "out of scope — allow" and record nothing; the Gate that
// governs the ticket's repo must be the one used, for the scope decision AND for where the
// record lands. Here the fallback gate (the session's) puts the repo out of scope and the
// resolved one puts it in.
func TestGateVerdict_JudgesTheTicketsRepoWithItsOwnGate(t *testing.T) {
	rv, session, link, repo := setupVerdictFixture(t)
	session.EnforcedRemotes = []string{"another-project"} // the session's policy: this repo is not ours
	session.ProjectRoot = ""                              // and it has no own origin that would cover it

	targetGate := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeVerdictGit{remote: "github.com/org/acmecorp-product"}, ProjectRoot: repo}
	var asked string
	res, err := rv.GateVerdict(session, VerdictOpts{
		Ticket: link, Verdict: "ready", By: "cr-1", Layer: "code-review",
		GateForRepo: func(r string) *gate.Gate { asked = r; return targetGate },
	})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	if asked != repo {
		t.Errorf("GateForRepo was asked for %q, want the ticket's repo %q", asked, repo)
	}
	if res.NoOp || strings.Contains(res.Message, "out of scope") {
		t.Errorf("the target's own gate puts the repo in scope; the session's scope must not decide it: %+v", res)
	}
	if _, err := targetGate.ReadRecord(verdictTestSHA); err != nil {
		t.Errorf("the record must land in the resolved gate's records dir: %v", err)
	}
	if _, err := session.ReadRecord(verdictTestSHA); err == nil {
		t.Error("nothing may be written through the session's gate")
	}

	// And with no resolver the passed gate is still used.
	rv2, session2, link2, _ := setupVerdictFixture(t)
	session2.EnforcedRemotes = []string{"another-project"}
	session2.ProjectRoot = ""
	res, err = rv2.GateVerdict(session2, VerdictOpts{Ticket: link2, Verdict: "ready", By: "cr-1"})
	if err != nil || !res.NoOp {
		t.Errorf("without GateForRepo the passed gate decides: res=%+v err=%v", res, err)
	}
}

func TestGateVerdict_RefusesTicketOutsideQueueRoot(t *testing.T) {
	rv, g, _, _ := setupVerdictFixture(t)
	outside := filepath.Join(t.TempDir(), "not-in-queue.md")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: outside, Verdict: "ready", By: "cr-1"}); err == nil {
		t.Fatal("want refusal for a ticket outside the queue root")
	}
}

// --- queue-mode-enforcement item 2: the queue-pull `gate verdict` path is the
// DEFAULT way a verdict lands on disk — these assert the full-sha contract and that
// nothing is recorded except through this path. ---

// shortShaGit always resolves any ref to a short (non-40-char) sha — simulates a
// ticket whose frontmatter sha can't be normalized to the full-sha contract
// GateVerdict requires before it will touch anything.
type shortShaGit struct{}

func (shortShaGit) RevParse(repo, ref string) (string, bool) { return "short123", true }
func (shortShaGit) RemoteURL(repo string) string             { return "github.com/org/acmecorp-product" }

func TestGateVerdict_RefusesWhenSHADoesNotResolveToFullSHA(t *testing.T) {
	rv, _, link, _ := setupVerdictFixture(t)
	g := &gate.Gate{RecordsDir: t.TempDir(), Git: shortShaGit{}}

	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"}); err == nil {
		t.Fatal("want a refusal — the ticket's sha does not resolve to a full 40-char sha")
	} else if !strings.Contains(err.Error(), "does not resolve to a full sha") {
		t.Errorf("err = %v, want the full-sha refusal message", err)
	}
}

// TestGateVerdict_NoRecordUntilGateVerdictCalled is the "recording off-path leaves no
// record" regression from the other direction: nothing is on disk, and gate.Check
// reports not-ready, until GateVerdict itself records — there is no other write path
// in this package that could have put a record there first.
func TestGateVerdict_NoRecordUntilGateVerdictCalled(t *testing.T) {
	_, g, _, repo := setupVerdictFixture(t)
	if ok, reason := g.Check(verdictTestSHA, repo); ok {
		t.Errorf("want gate.Check not-ready before any GateVerdict call, got ok=true (%s)", reason)
	}
	if _, err := g.ReadRecord(verdictTestSHA); err == nil {
		t.Error("want no record on disk before GateVerdict records one")
	}
}

// TestGateVerdict_FullSHARecordPersistsAndVisibleToGateCheck runs BOTH required
// layers through the queue-pull `gate verdict` path (GateVerdict, what `leadkit gate
// verdict` calls) and confirms: (1) gate.Check — what merge-guard/audit actually
// read — sees the result, and (2) the record is keyed on the FULL sha only; an
// abbreviated form is not the same record. This is the default-path invariant item 2
// exists to guarantee: a verdict that went through GateVerdict is the one recorded
// state everything downstream can see.
func TestGateVerdict_FullSHARecordPersistsAndVisibleToGateCheck(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("code-review verdict: %v", err)
	}
	critLink := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatalf("critique verdict: %v", err)
	}

	if ok, reason := g.Check(verdictTestSHA, repo); !ok {
		t.Fatalf("gate.Check after both layers recorded via GateVerdict = false (%s), want true", reason)
	}
	if _, err := g.ReadRecord(verdictTestSHA[:12]); err == nil {
		t.Error("want no record under an abbreviated sha — records are keyed on the full sha only")
	}
}

// --- role-keyed-lanes.md: routing to the ROLE lane, and the gate-unaffected
// regression the hard constraint demands. ---

// TestGateVerdict_CodeReviewReadyIgnoresROLESFileRoutesToRoleLane proves the
// role-keyed-lanes behaviour change directly: even when queue/ROLES maps
// critique to a totally different live instance dir, GateVerdict still routes
// the code-review-ready transition into queue/critique/ — the ROLES-based
// instance lookup is gone from this path entirely.
func TestGateVerdict_CodeReviewReadyIgnoresROLESFileRoutesToRoleLane(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	if err := os.WriteFile(filepath.Join(rv.QueueRoot, "ROLES"), []byte("critique\tcrit-9-decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-1"})
	if err != nil {
		t.Fatalf("GateVerdict: %v", err)
	}
	wantDest := filepath.Join(rv.QueueRoot, "critique", "pending", "9-example.md")
	if res.MovedTo != wantDest {
		t.Errorf("MovedTo = %s, want %s (the fixed critique role lane, ignoring the decoy ROLES mapping)", res.MovedTo, wantDest)
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "crit-9-decoy")); err == nil {
		t.Error("must not have touched the decoy ROLES-mapped instance dir at all")
	}
}

// TestGateVerdict_GateDecisionUnaffectedByQueueLaneName is the HARD CONSTRAINT
// regression role-keyed-lanes.md demands: gate.Record's chained/write-once/
// layer-independence rules key on the verdict's `by` name via gate.CanonicalRole
// — NEVER on the lane/directory a ticket happens to be sitting in. Run the
// IDENTICAL GateVerdict call (same `by`, same verdict) against two fixtures that
// differ ONLY in what directory name the ticket's queue entry sits under — one
// legacy instance-named ("cr-1"), one role-named ("code-review") — and assert the
// resulting gate record is identical (verdict + by; ts is a clock read, excluded)
// either way. Moving tasks to role lanes must never change a gate decision.
func TestGateVerdict_GateDecisionUnaffectedByQueueLaneName(t *testing.T) {
	run := func(laneDirName string) gate.Layer {
		t.Helper()
		root := t.TempDir()
		repo := t.TempDir()
		queueRoot := filepath.Join(root, "queue")
		tasksDir := filepath.Join(root, "tasks")
		for _, d := range []string{tasksDir, filepath.Join(queueRoot, laneDirName, "pending")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		base := "9-example.md"
		ticket := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + repo + "\nsha: " + verdictTestSHA + "\nstage: code-review\ngate: true\n---\n\n# body\n"
		if err := os.WriteFile(filepath.Join(tasksDir, base), []byte(ticket), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(queueRoot, laneDirName, "pending", base)
		if err := os.Symlink(filepath.Join("..", "..", "..", "tasks", base), link); err != nil {
			t.Fatal(err)
		}

		rv := &Reviewer{QueueRoot: queueRoot, TasksDir: tasksDir}
		g := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeVerdictGit{remote: "github.com/org/acmecorp-product"}, ProjectRoot: repo}

		if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-7"}); err != nil {
			t.Fatalf("GateVerdict (lane=%s): %v", laneDirName, err)
		}
		rec, err := g.ReadRecord(verdictTestSHA)
		if err != nil {
			t.Fatalf("ReadRecord (lane=%s): %v", laneDirName, err)
		}
		return rec.Layers["code-review"]
	}

	legacy := run("cr-1")
	roleKeyed := run("code-review")
	if legacy.Verdict != roleKeyed.Verdict || legacy.By != roleKeyed.By {
		t.Errorf("gate decision differs by lane name: legacy=%+v role-keyed=%+v — the gate must never key on the lane", legacy, roleKeyed)
	}
	if legacy.Verdict != "ready" || legacy.By != "cr-7" {
		t.Errorf("unexpected recorded layer: %+v", legacy)
	}
}

// TestGateVerdict_LayerIndependenceRefusalUnaffectedByQueueLaneName is the same
// regression, on the REFUSAL side: the critique layer-independence preflight
// (opts.By canonicalizing to the same role as the recorded code-review `by`) must
// refuse identically whether the ticket sits in a legacy instance-named lane or a
// role-named one.
func TestGateVerdict_LayerIndependenceRefusalUnaffectedByQueueLaneName(t *testing.T) {
	run := func(laneDirName string) error {
		t.Helper()
		root := t.TempDir()
		repo := t.TempDir()
		queueRoot := filepath.Join(root, "queue")
		tasksDir := filepath.Join(root, "tasks")
		if err := os.MkdirAll(tasksDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(queueRoot, laneDirName, "pending"), 0o755); err != nil {
			t.Fatal(err)
		}
		base := "9-example.md"
		ticket := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + repo + "\nsha: " + verdictTestSHA + "\nstage: critique\ngate: true\n---\n\n# body\n"
		if err := os.WriteFile(filepath.Join(tasksDir, base), []byte(ticket), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(queueRoot, laneDirName, "pending", base)
		if err := os.Symlink(filepath.Join("..", "..", "..", "tasks", base), link); err != nil {
			t.Fatal(err)
		}

		rv := &Reviewer{QueueRoot: queueRoot, TasksDir: tasksDir}
		g := &gate.Gate{RecordsDir: t.TempDir(), Git: fakeVerdictGit{remote: "github.com/org/acmecorp-product"}, ProjectRoot: repo}
		if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-2", Repo: repo}); err != nil {
			t.Fatal(err)
		}

		_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-3", Layer: "critique"})
		return err
	}

	legacyErr := run("crit-1")
	roleKeyedErr := run("critique")
	if legacyErr == nil || roleKeyedErr == nil {
		t.Fatalf("want both to refuse — cr-2/cr-3 canonicalize to the same role — legacyErr=%v roleKeyedErr=%v", legacyErr, roleKeyedErr)
	}
	if legacyErr.Error() != roleKeyedErr.Error() {
		t.Errorf("refusal message differs by lane name:\nlegacy:     %v\nrole-keyed: %v", legacyErr, roleKeyedErr)
	}
}

// Ticket 150: a ready code-review is held to the same independence as critique, and refused by the
// verb's own preflight (before it touches anything), not only by Record underneath it.
func TestGateVerdict_RefusesReadyCodeReviewFromTheCritiqueRoleOrTheRoleThatRecordedCritique(t *testing.T) {
	for _, tc := range []struct{ name, critBy, by, want string }{
		{"critique role", "crit-1", "crit-9", "critique-role agent"},
		{"role that recorded critique", "auditor-1", "auditor-2", "same role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, g, link, repo := setupVerdictFixture(t)
			for _, r := range []gate.RecordOpts{
				{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo},
				{SHA: verdictTestSHA, Layer: "critique", Verdict: "ready", By: tc.critBy, Repo: repo},
			} {
				if _, err := g.Record(r); err != nil {
					t.Fatal(err)
				}
			}
			_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: tc.by, Layer: "code-review"})
			if err == nil || !strings.HasPrefix(err.Error(), "gate-verdict: refuse") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want the verb's own preflight refusal containing %q, got %v", tc.want, err)
			}
			if rec, rerr := g.ReadRecord(verdictTestSHA); rerr != nil || rec.Layers["code-review"].By != "cr-1" {
				t.Errorf("a refused verdict changed the record: %+v %v", rec.Layers["code-review"], rerr)
			}
			if _, serr := os.Lstat(link); serr != nil {
				t.Errorf("a refused verdict moved the ticket: %v", serr)
			}
		})
	}
}
