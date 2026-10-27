package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/backfill"
	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/doctor"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/lock"
	"mgpp.dev/leadkit/internal/milestone"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/review"
	"mgpp.dev/leadkit/internal/roles"
)

// runQueue dispatches the queue subcommand group (move|sweep|wait|watch), the Go
// equivalents of queue-move.sh, queue-sweep.sh, queue-wait.sh, queue-waker.sh.
func runQueue(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue <move|claim|rm|unlink|sweep|wait|watch|assign|backfill-type|consolidate|repair> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "move":
		runQueueMove(args[1:])
	case "claim":
		runQueueClaim(args[1:])
	case "rm":
		runQueueRm(args[1:])
	case "unlink":
		runQueueUnlink(args[1:])
	case "sweep":
		runQueueSweep(args[1:])
	case "wait":
		runQueueWait(args[1:])
	case "watch":
		runQueueWatch(args[1:])
	case "assign":
		runQueueAssign(args[1:])
	case "backfill-type":
		runQueueBackfillType(args[1:])
	case "consolidate":
		runQueueConsolidate(args[1:])
	case "repair":
		runQueueRepair(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit queue: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runQueueConsolidate is `leadkit queue consolidate [--apply --confirm]` — the
// role-keyed-lanes migration tool (docs/build-tasks/role-keyed-lanes.md): computes
// (and, only with --apply --confirm, executes) the plan mapping every non-role
// queue lane on disk into its canonical role lane, via the SAME name→role
// resolver (roles.CanonicalLane) send-to-review/gate-verdict/assign/doctor's
// queue-lanes check all route through. Same guard convention as audit/init/team
// install/backfill-type: dry-run default, bare --apply refuses, --apply --confirm
// required to actually move anything.
//
// BUILT, NOT RUN HERE: the live queue migration is a separate,
// later, coordinated step — this command exists so that step has a tool to run,
// not so this build task runs it against the real queue.
func runQueueConsolidate(args []string) {
	fs := flag.NewFlagSet("queue consolidate", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "move entries into their resolved role lanes (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually move anything — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue consolidate: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	queueRoot := prof.Resolve(prof.QueueRoot)
	var rosterTypes []string
	for _, r := range prof.Roster {
		if r.Type != "" {
			rosterTypes = append(rosterTypes, r.Type)
		}
	}
	known := roles.KnownRoles(rosterTypes)

	plan, err := queue.ComputeConsolidatePlan(queueRoot, known, prof.LaneAliases)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue consolidate: %v\n", err)
		os.Exit(1)
	}

	guard := confirmGuard(*apply, *confirm)
	dryRun := guard != guardMutate

	var moved int
	if !dryRun {
		moved, err = queue.ApplyConsolidatePlan(plan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit queue consolidate: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Print(queue.FormatConsolidatePlan(plan, !dryRun))

	switch guard {
	case guardDryRun:
		fmt.Printf("(dry-run — nothing moved; %d entrie(s) planned, %d lane(s) unmappable; re-run with --apply --confirm)\n", len(plan.Moves), len(plan.Unmapped))
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm moves nothing. This would move %d entrie(s); re-run with `--apply --confirm` to execute.\n", len(plan.Moves))
		os.Exit(1)
	default:
		fmt.Printf("moved %d entrie(s)\n", moved)
		os.Exit(0)
	}
}

// runQueueBackfillType is `leadkit queue backfill-type [--apply --confirm]` — infers
// a canonical `type:` per task (type -> owner -> lane, same
// internal/typeinfer.Resolve rule internal/orchestrate's planner uses) and writes
// it into frontmatter. Same guard as audit/init/team install/orchestrate apply: dry-
// run default, bare --apply refuses, only --apply --confirm writes.
//
// POST-INCIDENT (see docs/build-tasks/type-field-backfill.md's HARD SAFETY note):
// the builder runs ONLY the dry-run against the real store in its report — the live
// write is a coordinated step run later, not part of building this.
func runQueueBackfillType(args []string) {
	fs := flag.NewFlagSet("queue backfill-type", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the inferred type: field (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue backfill-type: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	queueRoot, storeRoot, rolesPath := prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.Resolve(prof.RolesPath)
	agentsDir := filepath.Join(prof.Resolve(prof.Team.TeamDir), "agents")

	plan, err := backfill.ComputePlan(queueRoot, storeRoot, rolesPath, agentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue backfill-type: %v\n", err)
		os.Exit(1)
	}

	guard := confirmGuard(*apply, *confirm)
	dryRun := guard != guardMutate
	logPath := prof.Resolve(".leadkit/backfill-type.jsonl")

	n, err := backfill.Apply(plan, dryRun, logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue backfill-type: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(backfill.FormatPlan(plan, n, dryRun))

	switch guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", n)
		os.Exit(1)
	default:
		os.Exit(0)
	}
}

// runQueueAssign is assign.sh: assign a store ticket (by number) to a role's pending
// inbox. THIS MUTATES THE QUEUE, unlike audit — a real pipeline move, the same class
// as queue move/sweep. Exit codes match the bash: 2 for usage/role/ambiguous
// refusals, 1 for already-queued/symlink failures.
func runQueueAssign(args []string) {
	fs := flag.NewFlagSet("queue assign", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan (resolved reviewer + dest, dedup/ambiguity checks) and mutate nothing")
	owner := fs.String("owner", "", "also record the ticket's owner: the role that does the work and receives rework from a changes-needed verdict (written to the ticket's owner: frontmatter as its canonical role; refused if it is not a known role). Optional — a ticket with no owner: falls back to its type:")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue assign: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue assign [--dry-run] [--owner <role>] <ticket-number> <role>")
		os.Exit(2)
	}
	ticket, role := fs.Arg(0), fs.Arg(1)

	prof, _, err := config.Load()
	if err != nil {
		d := config.Defaults()
		prof = &d
	}

	// Reconcile --owner BEFORE assigning, so an unknown owner is refused with nothing
	// done — never a ticket queued whose owner could not be recorded.
	canonicalOwner := ""
	if *owner != "" {
		var ok bool
		canonicalOwner, ok = review.CanonicalOwner(*owner, rosterTypes(prof), prof.LaneAliases)
		if !ok {
			fmt.Fprintf(os.Stderr, "leadkit queue assign: --owner %q does not reconcile to a known role (known: %s)\n",
				*owner, strings.Join(roles.KnownRoles(rosterTypes(prof)), ", "))
			os.Exit(2)
		}
	}

	storeRoot := prof.Resolve(prof.StoreRoot)
	res, err := queue.Assign(
		prof.Resolve(prof.QueueRoot), storeRoot, prof.Resolve(prof.RolesPath),
		rosterTypes(prof), ticket, role, *dryRun,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		var ae *queue.AssignError
		if errors.As(err, &ae) {
			os.Exit(ae.Code)
		}
		os.Exit(1)
	}
	if *dryRun {
		if canonicalOwner != "" {
			fmt.Printf("DRY RUN: would set owner: %s on %s (nothing done)\n", canonicalOwner, res.Base)
		}
		return
	}
	fmt.Printf("assign: %s -> %s (queue/%s/pending/%s) — verified resolves\n", ticket, role, res.Role, res.Base)
	if canonicalOwner != "" {
		if err := review.UpdateFrontmatter(filepath.Join(storeRoot, res.Base), []review.KV{{Key: "owner", Value: canonicalOwner}}); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit queue assign: ticket is queued but its owner: could NOT be set to %s: %v\n", canonicalOwner, err)
			os.Exit(1)
		}
		fmt.Printf("assign: owner: %s\n", canonicalOwner)
	}
}

// resolveQueueAndStore is the ONE shared queue_root + store_root resolution every
// command that operates on the disk queue/tasks store goes through — `queue
// move`, `review send`, `gate verdict` (docs/build-tasks/profile-store-
// resolution.md: three separate adopters hit divergent/hardcoded resolution
// here). Precedence: $LEADKIT_QUEUE (env) overrides queueRoot outright — the
// same rule queue.DefaultRoot() (and so every plain queue subcommand's own
// -queue flag default) already gives it — else prof.QueueRoot. storeRoot is
// ALWAYS prof.Resolve(prof.StoreRoot), regardless of $LEADKIT_QUEUE: that env var
// has only ever meant "which queue", never "which store", and a profile's own
// store_root declaration shouldn't be silently abandoned just because a stale
// $LEADKIT_QUEUE crutch is still exported.
//
// BYTE-IDENTICAL TO TODAY when no leadkit.yaml is found at all: config.Defaults()
// already seeds QueueRoot=queue.DefaultRoot() and StoreRoot as that root's
// sibling "tasks" dir (queue.DefaultRoot() itself reads $LEADKIT_QUEUE, so
// Defaults() already reflects it) — so an un-configured project resolves exactly
// as review.New()/queue.DefaultRoot() always have, with no special-casing needed
// here. A live orchestration project runs this way; see the dedicated test.
func resolveQueueAndStore(prof *config.Profile) (queueRoot, storeRoot string) {
	queueRoot = prof.Resolve(prof.QueueRoot)
	if envQueue := os.Getenv("LEADKIT_QUEUE"); envQueue != "" {
		queueRoot = envQueue
	}
	storeRoot = prof.Resolve(prof.StoreRoot)
	return queueRoot, storeRoot
}

// resolveDefaultQueueRoot is `queue move`'s -queue flag default — the queueRoot
// half of resolveQueueAndStore (queue move has no use for storeRoot). Split out
// so it's directly testable without exec'ing the built binary or parsing flags/
// exiting.
func resolveDefaultQueueRoot() string {
	queueRoot, _ := resolveQueueAndStore(loadProfileOrDefaults())
	return queueRoot
}

// runQueueMove is queue-move.sh: relink a queue entry from one status dir to another.
func runQueueMove(args []string) {
	fs := flag.NewFlagSet("queue move", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue move [--queue <dir>] [--dry-run] <from> <to>")
		fmt.Fprintln(os.Stderr, "  <from> and <to> are paths resolved relative to the current directory (like mv), not to the queue root;")
		fmt.Fprintln(os.Stderr, "  both must still lie inside the queue root. Flags must come before the positionals.")
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "print the plan (src -> dest + the refusal checks) and mutate nothing")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue move: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 2 {
		fs.Usage()
		os.Exit(2)
	}
	// A flag-shaped positional is a misplaced flag, never a path. Go's flag parsing stops at
	// the first positional, so `queue move <from> --dry-run` leaves `--dry-run` as <to> and
	// the move SUCCEEDS: the entry is relinked to a file literally named "--dry-run" and
	// disappears from its lane, exit 0, no warning. That is silent loss of the one thing this
	// command exists to move, produced by the flag the caller used to ask for safety.
	//
	// Refuse instead. No legitimate queue path begins with "-", and `./-weird` remains
	// expressible for anyone who truly has one.
	for i, arg := range []string{fs.Arg(0), fs.Arg(1)} {
		if strings.HasPrefix(arg, "-") {
			name := "<from>"
			if i == 1 {
				name = "<to>"
			}
			fmt.Fprintf(os.Stderr, "leadkit queue move: refuse — %s is %q, which looks like a misplaced flag rather than a path.\n"+
				"Flags must come BEFORE the positionals: leadkit queue move [--queue <dir>] [--dry-run] <from> <to>\n"+
				"(If you really mean a path starting with \"-\", write it as ./%s)\n", name, arg, arg)
			os.Exit(2)
		}
	}
	if err := queue.Move(*root, fs.Arg(0), fs.Arg(1), *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue move: %v\n", err)
		os.Exit(1)
	}
}

// runQueueClaim is `leadkit queue claim`: make -by the holder of a ticket, which the rest of the lane
// then leaves alone. A pending entry is moved into in-progress and claimed; an in-progress entry is
// renewed (already yours), taken over (the holder's claim lapsed), or adopted (it never had one).
func runQueueClaim(args []string) {
	fs := flag.NewFlagSet("queue claim", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	by := fs.String("by", "", "who is claiming: the name `gate verdict -by` will be recorded under (default: $CLAUDE_AGENT_NAME when set; when it is set, any other name is refused; required when it is unset)")
	ttl := fs.Int("ttl", int(queue.DefaultClaimTTL/time.Second), "seconds a claim holds without renewal")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue claim: %v\n", err)
		os.Exit(2)
	}
	identity := os.Getenv("CLAUDE_AGENT_NAME")
	if fs.NArg() != 1 || (strings.TrimSpace(*by) == "" && strings.TrimSpace(identity) == "") {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue claim [--queue <dir>] [-by <name>] [-ttl <seconds>] <entry>  (-by is required unless $CLAUDE_AGENT_NAME is set)")
		os.Exit(2)
	}
	// The claim, the gate record and the verdict must carry ONE name: resolve it exactly as they do.
	who, bound, err := gate.ResolveBy(*by, identity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	*by = who
	if strings.HasPrefix(fs.Arg(0), "-") {
		fmt.Fprintf(os.Stderr, "leadkit queue claim: refuse — <entry> is %q, which looks like a misplaced flag; flags come BEFORE the entry\n", fs.Arg(0))
		os.Exit(2)
	}
	// Warn on the FIRST claim of a ticket only (the one from pending): a renewal, takeover or adoption of an
	// in-progress entry is the same consumer repeating itself, and a lease renewed every few minutes would
	// otherwise repeat the note each time. Placed after the misplaced-flag check so a refused call is silent.
	if filepath.Base(filepath.Dir(fs.Arg(0))) == "pending" {
		warnUnboundReviewer(os.Stderr, who, bound)
	}
	c, err := queue.ClaimEntry(*root, fs.Arg(0), *by, time.Now(), time.Duration(*ttl)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue claim: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("queue-claim: %s %s\n", filepath.Base(fs.Arg(0)), c.String())
}

// runQueueSweep is queue-sweep.sh, evolved into a self-surfacing daemon health check
// (docs/build-tasks/self-surfacing-diagnostics.md). It reports stalled inboxes and
// broken links, WRITES them to the findings artifact (sweep_status_file) every run,
// surfaces the remediation per diagnostics.self_repair, optionally reaps dangling
// links (self_repair: auto) and pushes an operator notifier (surface_notify).
//
// EXIT CODE is now a clean DAEMON-HEALTH signal, decoupled from backlog: exit 0 on a
// successful run (stalls/broken-links are normal reportable state, in the output +
// artifact), non-zero ONLY when the sweep itself cannot operate (queue root
// unreadable, a real I/O error). --strict restores the old exit-non-zero-on-stalls for
// anyone scripting the previous behavior (off by default).
func runQueueSweep(args []string) {
	fs := flag.NewFlagSet("queue sweep", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	poke := fs.Bool("poke", false, "bump the symlink mtime of every stalled inbox entry to re-fire a watcher event")
	strict := fs.Bool("strict", false, "exit non-zero when stalls/broken-links are found (legacy scripting behavior; default: exit 0 — findings are reportable state, non-zero means the sweep couldn't run)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue sweep: %v\n", err)
		os.Exit(2)
	}
	res, err := queue.Sweep(*root, *poke)
	if err != nil {
		// The sweep itself could not operate (unreadable queue root, real I/O error) —
		// THIS is the only non-zero-on-failure path; a stopped/broken daemon exit is
		// now reliable, never "there's backlog."
		fmt.Fprintf(os.Stderr, "leadkit queue sweep: %v\n", err)
		os.Exit(1)
	}
	for _, l := range res.Lines {
		fmt.Println(l)
	}
	fmt.Printf("queue-sweep: %d stalled, %d with broken links, %d stranded in a non-canonical state dir, %d blocked (reported, not stalled), %d finished (reported, not stalled)\n", res.Stalled, res.Broken, res.Stray, len(res.Blocked), len(res.Finished))
	if len(res.Strays) > 0 {
		fmt.Printf("queue-sweep: %d stray non-ticket entr(ies) in pending/in-progress; `leadkit queue repair --apply` clears the empty ones\n", len(res.Strays))
	}

	prof := loadProfileOrDefaults()
	diag := prof.Diagnostics

	// Project-scope the findings artifact + milestone re-anchor to the SWEPT QUEUE's
	// project, NOT the invoking cwd. A sweep is inherently about one `--queue`, so its
	// outputs must resolve relative to THAT queue's project root — walk up from the
	// queue root's parent to discover that project's profile and resolve
	// sweep_status_file + milestones_file against IT. So `queue sweep --queue <A>/queue`
	// invoked from project B writes A's artifact into A and reads A's milestones, never
	// B's. This is the exact bug: cwd-relative resolution wrote the migrate queue's
	// findings into the leadkit repo, and the sweep DAEMON (launchd cwd=`/`) wrote them
	// off `/` — nowhere doctor/status/board (which read <project>/.leadkit/...) look.
	// Degrade-safe: a bare/orphan queue with no resolvable project falls back to the
	// cwd-relative profile above — the artifact write is best-effort and must NEVER
	// fail the sweep.
	sweepProf := prof
	if abs, aerr := filepath.Abs(*root); aerr == nil {
		if qp, found, lerr := config.LoadFrom(filepath.Dir(abs)); found && lerr == nil {
			sweepProf = qp
		}
	}

	// Configurable self-repair: `auto` reaps the dangling links this run found (zero-
	// touch daemon mutation — only dangling links, never a live ticket); `report`/
	// `command` never mutate, they only surface (below).
	if diag.SelfRepair == config.SelfRepairAuto && res.Broken > 0 {
		// Repair-first: the sweep-auto reap goes through the SAME classification as the manual
		// command — mispointed-but-live links are RE-POINTED, only true orphans reaped, dir
		// aliases reported. Resolve the store root from the SWEPT queue's own scoped profile
		// (sweepProf), relative to *root; if it can't be resolved/read the reaper reaps nothing.
		if rr, rerr := queue.RepairDanglingOnly(*root, resolveStoreRoot(sweepProf, *root)); rerr != nil {
			fmt.Fprintf(os.Stderr, "queue-sweep: self_repair=auto reap failed: %v\n", rerr)
		} else {
			fmt.Printf("--- self-repair (auto) ---\n%s", queue.FormatRepair(rr))
		}
	}

	// Per-tick re-anchor: emit the milestone status in the sweep daemon's per-project
	// report, so the autonomous loop is reminded of the GOAL + how far the TEAM is from
	// DoD every tick — not just its role. Degrades cleanly (a "no milestone DoD declared"
	// line) when the artifact is absent, and never affects the sweep's own exit code.
	milestoneLine := milestone.FormatStatus(loadMilestoneReport(sweepProf))
	fmt.Print("--- milestone re-anchor ---\n")
	fmt.Print(milestoneLine)

	// Productivity watcher (docs/build-tasks/productivity-watcher.md): the busy≠productive
	// signal on the sweep's normal cadence, so a long-running unattended team surfaces an
	// "active but producing no deliverable" stall without a manual `leadkit doctor` run. A
	// REPORT line only — never an action (no reassign/kill/respawn; that's the human's/
	// lead's call). Same compute as the doctor productivity check.
	if line, stalled := doctor.ProductivityStallLine(time.Now(), sweepProf.Burn.ProductivityStallWindowDur(), doctor.RealProductivityProbe(sweepProf)); stalled {
		fmt.Print("--- productivity ---\n")
		fmt.Println(line)
	}

	// Frozen-claim detector (docs/build-tasks/frozen-claim-and-noninteractive.md): a
	// HUNG owner holding a claimed in-progress ticket that isn't advancing — invisible
	// to owner-liveness (reads in-progress>0 as consuming) and productivity (needs an
	// activity signal a hung agent doesn't emit). A REPORT line only (probe→TaskStop→
	// reassign is the human's/lead's call); the structured claims also feed the findings
	// artifact below so doctor/status/board self-surface them.
	frozenLines, frozenClaims := doctor.FrozenClaimReport(time.Now(), sweepProf.Burn.InprogressStallWindowDur(), doctor.RealFrozenClaimProbe(sweepProf))
	if len(frozenLines) > 0 {
		fmt.Print("--- frozen claims ---\n")
		for _, l := range frozenLines {
			fmt.Println(l)
		}
	}

	// The findings artifact — the single source doctor/status/board/notify all read.
	// Best-effort: a write failure NEVER fails the sweep (see WriteSweepStatus's doc).
	status := queue.BuildSweepStatus(res, milestoneLine, time.Now())
	status.FrozenClaims = frozenClaims
	if werr := queue.WriteSweepStatus(sweepProf.Resolve(sweepProf.SweepStatusFile), status); werr != nil {
		fmt.Fprintf(os.Stderr, "queue-sweep: findings artifact write failed (best-effort, ignored): %v\n", werr)
	}

	// Surface the remediation in the sweep's own output (report/command modes name the
	// fix; auto already reaped above but still surfaces what it did).
	if rem := status.Remediation(diag.SelfRepair); rem != "" {
		fmt.Printf("--- remediation ---\n%s\n", rem)
	}

	// Active-notifier PUSH (opt-in): exec the operator's notifier only when
	// surface_notify is on, a command is set, and findings cross the threshold. Never a
	// hard dependency — a failed exec is logged, not fatal.
	if diag.SurfaceNotify {
		if fired, nerr := queue.Notify(diag.NotifyCommand, diag.NotifyThreshold, status); fired {
			if nerr != nil {
				fmt.Fprintf(os.Stderr, "queue-sweep: notify_command failed (non-fatal): %v\n", nerr)
			} else {
				fmt.Println("--- notified ---")
			}
		}
	}

	if *strict && (res.Stalled > 0 || res.Broken > 0 || res.Stray > 0) {
		os.Exit(1)
	}
	os.Exit(0)
}

// runQueueRepair is `leadkit queue repair [--apply]` — the explicit operator self-
// repair (docs/build-tasks/self-surfacing-diagnostics.md, Part 4). It reaps ONLY
// dangling queue symlinks (a link whose store target is gone), NEVER a live ticket,
// and logs what it reaped. Dry-run by default (lists what would be reaped, mutates
// nothing); --apply actually removes them. Always available regardless of the
// diagnostics.self_repair mode — this is deliberate operator action. The opt-in
// --prune-empty-lanes flag ADDITIONALLY prunes empty UNREGISTERED lane dirs + empty
// stray non-lane dirs (registered ROLES lanes are never auto-pruned; a stray holding
// any file is only reported) — same dry-run-by-default / --apply convention.
func runQueueRepair(args []string) {
	fs := flag.NewFlagSet("queue repair", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	apply := fs.Bool("apply", false, "actually reap the dangling links / prune empty dirs (default: dry-run — list them and mutate nothing)")
	prune := fs.Bool("prune-empty-lanes", false, "ALSO prune empty UNREGISTERED lane dirs + empty stray non-lane dirs (registered ROLES lanes are never auto-pruned; a stray holding any file is only reported)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue repair: %v\n", err)
		os.Exit(2)
	}
	res, err := queue.Repair(*root, resolveStoreRoot(loadProfileOrDefaults(), *root), *apply, *prune)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue repair: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(queue.FormatRepair(res))
	os.Exit(0)
}

// resolveStoreRoot resolves the FLAT STORE ROOT the repair-first reaper
// (docs/build-tasks/reaper-repair-first.md) needs to tell a mispointed-but-LIVE link
// (re-point) from a true orphan (reap). It DEFAULTS to the queue root's sibling tasks/
// (<queueRoot>/../tasks) — always relative to the queue ACTUALLY being repaired, so a
// `--queue` pointed at another project resolves THAT project's store — and honors an
// operator's EXPLICITLY-configured store_root when it differs from that built-in default.
// It returns "" only when no path can be formed at all; Repair then reaps nothing and
// reports every broken link UNVERIFIABLE (fail-safe).
func resolveStoreRoot(prof *config.Profile, queueRoot string) string {
	// Default: the sibling tasks/ of the queue being repaired.
	sibling := ""
	if abs, err := filepath.Abs(queueRoot); err == nil && abs != "" {
		sibling = filepath.Join(filepath.Dir(abs), "tasks")
	}
	// An explicit profile store_root wins ONLY when it is a DELIBERATE override — i.e. not
	// merely the built-in default (which is itself <profileQueueRoot>/../tasks). Comparing
	// against the recomputed default avoids honoring a defaulted store_root that would point
	// at the WRONG project's tasks/ when --queue targets a different queue than the profile.
	if prof != nil {
		if sr := strings.TrimSpace(prof.StoreRoot); sr != "" {
			resolved := prof.Resolve(sr)
			def := ""
			if qr := strings.TrimSpace(prof.QueueRoot); qr != "" {
				if qabs, err := filepath.Abs(prof.Resolve(qr)); err == nil {
					def = filepath.Join(filepath.Dir(qabs), "tasks")
				}
			}
			if resolved != "" && resolved != def {
				return resolved // a deliberately-relocated store
			}
		}
	}
	return sibling
}

// runQueueRm is `leadkit queue rm --task <name> [--queue <root>] [--store <root>]
// [--allow-non-terminal] [--apply]` — the GOVERNED task delete, the missing counterpart
// to the queue-guard that (correctly) blocks a raw rm/mv/ln on the queue. It removes a
// task COMPLETELY: the flat-store file AND every overlay symlink that resolves to it,
// dry-run by DEFAULT (like repair) — the overlay links removed FIRST, then the store
// file, so no live link outlives the store file it points at. A NON-terminal (live) task
// refuses --apply unless --allow-non-terminal is passed. Only ever the named store file +
// the symlinks that resolve to IT is touched (never a dir, never another task, never a
// real non-symlink file). It reuses resolveStoreRoot + a FindDangling-style walk.
func runQueueRm(args []string) {
	fs := flag.NewFlagSet("queue rm", flag.ContinueOnError)
	task := fs.String("task", "", "the task file name/id to delete (e.g. gate-x.md or gate-x) — REQUIRED; resolved to exactly one flat-store file")
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	store := fs.String("store", "", "path to the flat task store root (default: the queue's sibling tasks/, or the profile's store_root)")
	allowNonTerminal := fs.Bool("allow-non-terminal", false, "permit deleting a LIVE (non-terminal) task; without it --apply on a non-terminal task refuses")
	apply := fs.Bool("apply", false, "actually delete the store file + its overlay symlinks (default: dry-run — report the plan and mutate nothing)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue rm: %v\n", err)
		os.Exit(2)
	}
	if strings.TrimSpace(*task) == "" {
		fmt.Fprintln(os.Stderr, "leadkit queue rm: --task <name> is required")
		os.Exit(2)
	}
	storeRoot := *store
	if strings.TrimSpace(storeRoot) == "" {
		storeRoot = resolveStoreRoot(loadProfileOrDefaults(), *root)
	}
	res, err := queue.RemoveTask(*root, storeRoot, *task, *allowNonTerminal, *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(queue.FormatRemove(res))
	if res.Guarded {
		os.Exit(1) // apply requested on a live task without --allow-non-terminal — refused
	}
	os.Exit(0)
}

// runQueueUnlink is `leadkit queue unlink <entry>`: remove ONE overlay link, the governed
// alternative to a raw `rm` (which queue-guard refuses). `queue rm` deletes every link plus the
// store file, which is far too much for a stray or duplicate link. Dry-run unless --apply, and it
// refuses to remove a ticket's only link unless --allow-last, since that orphans the ticket.
func runQueueUnlink(args []string) {
	fs := flag.NewFlagSet("queue unlink", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	apply := fs.Bool("apply", false, "actually remove the link (default: dry-run — report and mutate nothing)")
	allowLast := fs.Bool("allow-last", false, "permit removing a ticket's ONLY link, which leaves it in no lane")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue unlink: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue unlink [-queue <root>] [-apply] [-allow-last] <queue/<lane>/<dir>/<ticket>>")
		os.Exit(2)
	}
	res, err := queue.Unlink(*root, fs.Arg(0), *allowLast, *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue unlink: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(queue.FormatUnlink(res))
	os.Exit(0)
}

// runQueueWait is queue-wait.sh: block at zero token cost until a role-queue dir has
// work, then print the reason and return.
func runQueueWait(args []string) {
	fs := flag.NewFlagSet("queue wait", flag.ContinueOnError)
	by := fs.String("by", "", "who is waiting: an in-progress ticket another owner holds a live claim on is then theirs, not an orphan, and does not wake you")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue wait: %v\n", err)
		os.Exit(2)
	}
	args = fs.Args()
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit queue wait [-by <name>] <dir> [timeout-seconds] [interval-seconds]")
		os.Exit(2)
	}
	dir := args[0]
	timeout := 3600 * time.Second
	interval := 20 * time.Second
	if len(args) >= 2 {
		secs, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit queue wait: invalid timeout-seconds %q: %v\n", args[1], err)
			os.Exit(2)
		}
		timeout = time.Duration(secs) * time.Second
	}
	if len(args) >= 3 {
		secs, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit queue wait: invalid interval-seconds %q: %v\n", args[2], err)
			os.Exit(2)
		}
		interval = time.Duration(secs) * time.Second
	}

	prof := loadProfileOrDefaults()
	lockPath := prof.Resolve(prof.Throttle.LockFile)

	// Honor BOTH the per-project lock AND the account-level burn-enforce lock
	// (docs/build-tasks/burn-auto-enforce.md): the account throttle is account-scoped,
	// so an account-wide pause must reach a session even if its own project isn't
	// locked (or has no .leadkit/ at all).
	res, err := queue.WaitAs(queue.Claimant{By: *by}, dir, timeout, interval, lockPath, lock.DefaultAccountLockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue wait: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s: %s\n", res.Kind, res.Msg)
	os.Exit(0)
}

// runQueueWatch is queue-waker.sh: the recursive filesystem watch that wakes an agent
// when a ticket lands in its pending/ inbox.
func runQueueWatch(args []string) {
	fs := flag.NewFlagSet("queue watch", flag.ContinueOnError)
	root := fs.String("queue", resolveDefaultQueueRoot(), "path to the task queue overlay root (default: the profile's queue_root, overridden by $LEADKIT_QUEUE)")
	dryRun := fs.Bool("dry-run", false, "print what would be sent instead of waking agents")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue watch: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	if err := queue.Watch(*root, prof.WakeCommand, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit queue watch: %v\n", err)
		os.Exit(1)
	}
}
