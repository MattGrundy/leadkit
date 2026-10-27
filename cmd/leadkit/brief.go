package main

// `leadkit brief` — durable, project-addressed CROSS-SESSION brief distribution with a
// receiver-writes-outcome / sender-confirms accountability loop (docs/build-tasks/
// leadkit-brief.md, build 2 of docs/design/account-routing-brief-central-pool.md). It is
// a THIN wrapper over the kind-agnostic routing substrate (internal/routing): the
// substrate owns the durable store, atomic writes, optimistic-concurrency guard, delivery
// hint, inbox, named secondary bodies and retention; THIS file adds only the brief KIND's
// lifecycle legality + the role guards + the optional --as-task materialization.
//
// SECURITY-CRITICAL (adversarial, cross-session anti-permission-laundering): the acting
// session's slug is resolved the SAME way refreshPresence derives it — sessionSlugAndID
// -> install.ResolveProjectSlug(profile) — NEVER from a flag or a claim. Only the
// recipient (to_slug) may ack/outcome/decline; only the sender (from_slug) may
// confirm/withdraw. No brief command accepts an --actor/--slug/--as override for the
// acting identity, so a session can never act as a party it is not. Every mutating
// command performs a routing Transition with the EXPECTED prior status, so an out-of-order
// or raced flip is refused by the substrate (ErrConflict) rather than silently applied;
// ErrConflict is surfaced with a clear re-check message + a non-zero exit and is NEVER
// forced.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/taskcreate"
	"mgpp.dev/leadkit/internal/usage"
)

// briefKind is the routing <kind> label every brief item is filed under.
const briefKind = "brief"

// The brief lifecycle statuses (enforced by THIS CLI; opaque to the substrate). Terminal
// states are `closed` and `withdrawn`; `declined` is an off-ramp back to the sender's
// attention that is itself closed later.
const (
	briefSent      = "sent"
	briefAcked     = "acked"
	briefOutcome   = "outcome"
	briefConfirmed = "confirmed"
	briefClosed    = "closed"
	briefDeclined  = "declined"
	briefWithdrawn = "withdrawn"
	// briefAwaitingUser is the PARKED state (docs/design/cross-project-coordination.md
	// §2.1): the recipient cannot digest or clarify the brief without the owner, so it
	// parks it here rather than leaving "an agent is stuck" implicit. It is non-terminal
	// and recipient-owned; `brief awaiting` is the one query over every such brief.
	briefAwaitingUser = "awaiting-user"
	// briefEscalated and briefRouted are the UP/DOWN half of design §2.1. An ESCALATED
	// brief is addressed to the overarching lead and is not workable by it: it is waiting
	// to be routed to the project that owns the work (so it cannot be acked). Routing
	// moves it to ROUTED - non-terminal, parked at the overarching lead - and files a NEW
	// child brief to the target; see the escalate/route section below.
	briefEscalated = "escalated"
	briefRouted    = "routed"
)

// outcomeBodyName is the named-secondary-body key the receiver's outcome deliverable is
// stored under (routing writes it as <id>.outcome.md alongside the envelope).
const outcomeBodyName = "outcome"

// parseBriefFlags parses a brief subcommand's flags allowing flags and positional args to
// be INTERSPERSED (Go's stdlib flag stops at the first positional, so `ack <id> --as-task`
// would otherwise not parse). It repeatedly parses, collecting each positional it hits and
// continuing with the remainder — the well-known idiom — so `<id>` may appear before or
// after the flags. Returns the positional args in order.
func parseBriefFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// runBrief is `leadkit brief <send|inbox|show|ack|await-user|resume|awaiting|escalate|route|outcome|confirm|close|decline|withdraw>`.
func runBrief(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief <send|inbox|show|ack|await-user|resume|awaiting|escalate|route|outcome|confirm|close|decline|withdraw> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "send":
		runBriefSend(args[1:])
	case "inbox":
		runBriefInbox(args[1:])
	case "show":
		runBriefShow(args[1:])
	case "ack":
		runBriefAck(args[1:])
	case "await-user":
		runBriefAwaitUser(args[1:])
	case "resume":
		runBriefResume(args[1:])
	case "awaiting":
		runBriefAwaiting(args[1:])
	case "escalate":
		runBriefEscalate(args[1:])
	case "route":
		runBriefRoute(args[1:])
	case "outcome":
		runBriefOutcome(args[1:])
	case "confirm":
		runBriefConfirm(args[1:])
	case "close":
		runBriefClose(args[1:])
	case "decline":
		runBriefDecline(args[1:])
	case "withdraw":
		runBriefWithdraw(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit brief: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// briefActor bundles the acting session's resolved identity — its OWN project slug (the
// authority for every role check) and session id. Resolved via sessionSlugAndID, the same
// derivation refreshPresence/the daemon/burn registries use; never from a flag.
type briefActor struct {
	slug    string
	session string
}

// resolveBriefActor loads the profile and resolves THIS session's own slug + session id.
// An unresolvable slug is fatal for any role-gated command — a session with no identity
// cannot be authorized to act on a brief (fail closed).
func resolveBriefActor() (*config.Profile, briefActor) {
	prof := loadProfileOrDefaults()
	slug, session := sessionSlugAndID(prof)
	return prof, briefActor{slug: slug, session: session}
}

// locateBrief finds a brief by id across all recipient dirs (the sender doesn't know the
// to_slug segment). A not-found id or a store-read error is fatal with a clear message.
func locateBrief(store *routing.Store, id string) routing.Envelope {
	env, err := store.Locate(briefKind, id)
	if errors.Is(err, routing.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "leadkit brief: no brief with id %q\n", id)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief: %v\n", err)
		os.Exit(1)
	}
	return env
}

// requireRecipient refuses unless the acting session's own slug IS the recipient
// (to_slug) — the anti-laundering gate for ack/outcome/decline.
func requireRecipient(env routing.Envelope, actor briefActor, verb string) {
	if actor.slug == "" || actor.slug != env.ToSlug {
		fmt.Fprintf(os.Stderr,
			"leadkit brief %s: REFUSED — only the recipient project (%q) may %s this brief; this session resolves to %q. A session may only act as its OWN slug (never a claimed one).\n",
			verb, env.ToSlug, verb, actor.slug)
		os.Exit(1)
	}
}

// requireSender refuses unless the acting session's own slug IS the sender (from_slug) —
// the anti-laundering gate for confirm/withdraw.
func requireSender(env routing.Envelope, actor briefActor, verb string) {
	if actor.slug == "" || actor.slug != env.FromSlug {
		fmt.Fprintf(os.Stderr,
			"leadkit brief %s: REFUSED — only the sender project (%q) may %s this brief; this session resolves to %q. A session may only act as its OWN slug (never a claimed one).\n",
			verb, env.FromSlug, verb, actor.slug)
		os.Exit(1)
	}
}

// requireParty refuses unless the acting session is the sender OR the recipient — used by
// `close`, which either party may perform (but no unrelated third session may).
func requireParty(env routing.Envelope, actor briefActor, verb string) {
	if actor.slug != "" && (actor.slug == env.ToSlug || actor.slug == env.FromSlug) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"leadkit brief %s: REFUSED — only the sender (%q) or recipient (%q) may %s this brief; this session resolves to %q.\n",
		verb, env.FromSlug, env.ToSlug, verb, actor.slug)
	os.Exit(1)
}

// briefTransition runs a routing Transition and centralizes the ErrConflict surfacing:
// a stale/raced/out-of-order flip prints a clear re-check line + exits non-zero, NEVER
// forcing. Any other error is fatal too. On success it returns the new envelope.
func briefTransition(store *routing.Store, env routing.Envelope, p routing.TransitionParams, verb string) routing.Envelope {
	// Always pin the expected updated_at to what we just read, so even a same-status
	// concurrent write is caught.
	if p.ExpectedUpdatedAt.IsZero() {
		p.ExpectedUpdatedAt = env.UpdatedAt
	}
	out, err := store.Transition(briefKind, env.ToSlug, env.ID, p)
	if errors.Is(err, routing.ErrConflict) {
		fmt.Fprintf(os.Stderr,
			"leadkit brief %s: brief moved to %q since you read it (expected %q) — re-check with `leadkit brief show %s` and retry; not forcing.\n",
			verb, currentStatus(store, env.ID), p.ExpectedStatus, env.ID)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief %s: %v\n", verb, err)
		os.Exit(1)
	}
	return out
}

// currentStatus re-reads a brief's live status for the re-check message (best-effort:
// "?" if it can't be re-read, e.g. it was pruned between the conflict and now).
func currentStatus(store *routing.Store, id string) string {
	env, err := store.Locate(briefKind, id)
	if err != nil {
		return "?"
	}
	return env.Status
}

// --- send -------------------------------------------------------------------

func runBriefSend(args []string) {
	fs := flag.NewFlagSet("brief send", flag.ContinueOnError)
	to := fs.String("to", "", "recipient PROJECT SLUG (required; validated against the project registry)")
	subject := fs.String("subject", "", "one-line subject (required)")
	bodyFile := fs.String("body-file", "", "the brief prose file, stored as the item's durable <id>.md body")
	routingDir := fs.String("routing-dir", "", "override the routing store dir (default: $LEADKIT_ROUTING_DIR or ~/.claude/leadkit/routing)")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "project registry dir used to validate the recipient slug")
	presenceDir := fs.String("presence-dir", usage.DefaultPresenceDir, "presence registry dir used to resolve the recipient's live session(s)")
	registryRepo := fs.String("registry-repo", "", "lead-identity registry repo used to resolve a registry-led workstream recipient (default: registry_repo)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *to == "" || *subject == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief send --to <slug> --subject <s> [--body-file <brief.md>]")
		os.Exit(2)
	}

	prof, actor := resolveBriefActor()

	// Addressing is a correctness gate: refuse an unknown, non-sigil recipient rather than
	// filing a brief at an address no session can ever pick up. A recipient is valid if it
	// is a sigil, a registered project, a registry-LED workstream, or a live-presence slug —
	// resolved best-effort across the three account-level registries.
	dirs := routing.RecipientDirs{
		ProjectsDir:  *projectsDir,
		RegistryRepo: resolveRegistryRepo(prof, *registryRepo),
		PresenceDir:  *presenceDir,
		RegistryTTL:  prof.RegistryTTLDur(),
		PresenceTTL:  prof.Burn.WorkstreamStaleWindowDur(),
		Now:          time.Now().UTC(),
	}
	if err := routing.ResolveRecipient(*to, dirs); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief send: %v\n", err)
		os.Exit(1)
	}

	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit brief send: read --body-file: %v\n", err)
			os.Exit(1)
		}
		body = b
	}

	store := routing.Open(resolveRoutingDir(*routingDir))
	// from_slug is the sender's OWN resolved slug — never a flag (a spoofed sender could
	// only sabotage itself, but the anti-laundering posture is uniform: identity is owned).
	env, err := store.Create(routing.CreateParams{
		Kind: briefKind, FromSlug: actor.slug, FromSession: actor.session, ToSlug: *to,
		Subject: *subject, Status: briefSent, Body: body, Now: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief send: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("brief sent: %s -> %s (status %q)\n", env.ID, env.ToSlug, env.Status)
	fmt.Println(routing.DeliveryHint(*to, dirs))
	os.Exit(0)
}

// --- inbox ------------------------------------------------------------------

func runBriefInbox(args []string) {
	fs := flag.NewFlagSet("brief inbox", flag.ContinueOnError)
	allStatus := fs.Bool("all-status", false, "include terminal briefs (closed/withdrawn); default hides them")
	slug := fs.String("slug", "", "recipient slug to list (default: this session's resolved slug UNION the workstreams it registered as leading)")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	registryRepo := fs.String("registry-repo", "", "override registry_repo (used to union the workstreams this session leads)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	targets := inboxTargets(prof, *slug, *registryRepo)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "leadkit brief inbox: no --slug given and this session's slug could not be resolved")
		os.Exit(2)
	}
	target := strings.Join(targets, ",")
	store := routing.Open(resolveRoutingDir(*routingDir))
	items, err := unionInbox(store, targets, routing.InboxFilter{Kind: briefKind})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief inbox: %v\n", err)
		os.Exit(1)
	}
	// Default hides terminal briefs (closed/withdrawn); --all-status shows everything.
	var shown []routing.Envelope
	for _, e := range items {
		if e.Terminal() && !*allStatus {
			continue
		}
		shown = append(shown, e)
	}
	if len(shown) == 0 {
		fmt.Printf("brief inbox for %s: (empty)\n", target)
		os.Exit(0)
	}
	now := time.Now().UTC()
	fmt.Printf("brief inbox for %s (%d brief(s), newest first):\n", target, len(shown))
	for _, e := range shown {
		term := ""
		if e.Terminal() {
			term = " [terminal]"
		}
		fmt.Printf("  %s  status=%s  from=%s  age=%s%s  %s\n",
			e.ID, e.Status, e.FromSlug, now.Sub(e.CreatedAt).Round(time.Second), term, e.Subject)
	}
	os.Exit(0)
}

// --- show -------------------------------------------------------------------

func runBriefShow(args []string) {
	fs := flag.NewFlagSet("brief show", flag.ContinueOnError)
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief show <id> [--routing-dir <dir>]")
		os.Exit(2)
	}
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])

	fmt.Printf("brief %s\n", env.ID)
	fmt.Printf("  from:    %s\n", env.FromSlug)
	fmt.Printf("  to:      %s\n", env.ToSlug)
	fmt.Printf("  subject: %s\n", env.Subject)
	fmt.Printf("  status:  %s\n", env.Status)
	if env.Terminal() {
		fmt.Printf("  closed:  %s\n", env.ClosedAt.UTC().Format(time.RFC3339))
	}
	printRouteTrail(store, env)

	if body, err := store.ReadBody(env); err == nil && len(body) > 0 {
		fmt.Printf("\n--- brief body ---\n%s\n", string(body))
	}
	if out, err := store.ReadNamedBody(env, outcomeBodyName); err == nil && len(out) > 0 {
		fmt.Printf("\n--- outcome ---\n%s\n", string(out))
	}

	fmt.Printf("\n--- history ---\n")
	for _, ev := range env.History {
		note := ""
		if ev.Note != "" {
			note = "  " + ev.Note
		}
		fmt.Printf("  %s  %-9s by %s (%s)%s\n",
			ev.TS.UTC().Format(time.RFC3339), ev.Action, ev.BySlug, ev.BySession, note)
	}
	os.Exit(0)
}

// --- ack --------------------------------------------------------------------

func runBriefAck(args []string) {
	fs := flag.NewFlagSet("brief ack", flag.ContinueOnError)
	asTask := fs.Bool("as-task", false, "ALSO materialize a linked task in the recipient project's queue (carries brief_id)")
	taskType := fs.String("task-type", "", "canonical agent type for the --as-task materialized task (required with --as-task)")
	taskIssue := fs.String("task-issue", "", "optional Issue id to parent the --as-task task to (default: unparented)")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief ack <id> [--as-task --task-type <canonical> [--task-issue <id>]]")
		os.Exit(2)
	}
	id := rest[0]
	if *asTask && *taskType == "" {
		fmt.Fprintln(os.Stderr, "leadkit brief ack: --as-task requires --task-type <canonical> (the recipient's roster decides valid types)")
		os.Exit(2)
	}

	prof, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, id)
	requireRecipient(env, actor, "ack")
	if env.Status == briefEscalated {
		fmt.Fprintf(os.Stderr, "leadkit brief ack: brief %s is %q - it was escalated to you to be ROUTED, not worked: `leadkit brief route %s --to <project>` (or decline it)\n",
			env.ID, env.Status, env.ID)
		os.Exit(1)
	}

	// The status flip is the authoritative, role+order-gated action — do it FIRST, so a
	// raced flip (ErrConflict) never leaves an orphan materialized task behind.
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: briefSent, NewStatus: briefAcked,
		Action: "acked", ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "ack")
	fmt.Printf("brief acked: %s (status %q)\n", env.ID, env.Status)

	if *asTask {
		res, err := taskcreate.Create(taskcreate.Options{
			Title:      "Brief: " + env.Subject,
			Type:       *taskType,
			Parent:     *taskIssue,
			Unparented: *taskIssue == "",
			BriefID:    env.ID,
			Body: fmt.Sprintf("Materialized from routing brief `%s` (from %s).\n\nSubject: %s\n\nRun `leadkit brief show %s` for the brief body; run `leadkit brief outcome %s --body-file <outcome.md>` when this task's result is ready.\n",
				env.ID, env.FromSlug, env.Subject, env.ID, env.ID),
			RosterTypes: rosterTypes(prof),
			StoreRoot:   prof.Resolve(prof.StoreRoot),
			QueueRoot:   prof.Resolve(prof.QueueRoot),
			RolesPath:   prof.Resolve(prof.RolesPath),
			AgentsDir:   filepath.Join(prof.Resolve(prof.Team.TeamDir), "agents"),
			IssuesRoot:  prof.Resolve(prof.IssuesRoot),
		})
		if err != nil {
			// The ack succeeded and stands; the materialization is loosely coupled, so a
			// failure here is reported (non-zero) but does not undo the ack.
			fmt.Fprintf(os.Stderr, "leadkit brief ack: brief acked, but --as-task materialization failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("as-task: created %s (id %d) in the recipient queue, linked brief_id=%s\n", res.Path, res.ID, env.ID)
	}
	os.Exit(0)
}

// --- await-user / resume / awaiting -----------------------------------------
//
// The `awaiting-user` status (design §2.1) is the clarification escape hatch: a domain
// lead that cannot digest a brief parks it here AND flags the sender, so the owner has ONE
// queryable place for "what is waiting on me".
//
// ARCHITECTURAL CONSTRAINT (design §2.4b) — DO NOT "IMPROVE" THIS WITH A SHARED STORE.
// There is NO shared cross-repo queue and there must not be. Per-repo single-writer stays:
// each repo owns its own queue, and the overarching lead's OWN queue is the coordination
// backbone. `awaiting` is therefore a READ over the brief store that already exists (the
// account-level routing substrate, where briefs have always lived as the cross-repo
// hand-off); it writes nothing, and it never touches any project's task queue. Parking a
// brief writes only that one brief's envelope, as its recipient, through the same
// optimistic-concurrency Transition every other verb uses. If a feature seems to need a
// shared work-queue across repos, the answer is a task in the overarching lead's queue
// that spawns a brief and is blocked_on it - not a shared store, whatever it would buy.

// priorLiveStatus is where a parked brief returns to on resume: `acked` if the recipient
// ever acked it, else `sent`. Derived from the append-only history rather than a stored
// field, so the envelope schema is unchanged and the answer cannot drift from the trail.
// Resuming to the ORIGINAL status (not always `acked`) matters: a brief parked before ack
// must still be ackable afterwards, with `ack --as-task`, rather than skipping it.
func priorLiveStatus(env routing.Envelope) string {
	for _, ev := range env.History {
		if ev.Action == "acked" {
			return briefAcked
		}
	}
	return briefSent
}

func runBriefAwaitUser(args []string) {
	fs := flag.NewFlagSet("brief await-user", flag.ContinueOnError)
	note := fs.String("note", "", "what the owner must clarify (required - this is what the owner reads in `brief awaiting`)")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "project registry dir used to resolve the sender's live session for the nudge hint")
	presenceDir := fs.String("presence-dir", usage.DefaultPresenceDir, "presence registry dir used to resolve the sender's live session(s)")
	registryRepo := fs.String("registry-repo", "", "lead-identity registry repo used to resolve a registry-led sender (default: registry_repo)")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 || *note == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief await-user <id> --note <what the owner must clarify>")
		os.Exit(2)
	}
	prof, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireRecipient(env, actor, "await-user")

	// Only a live, not-yet-resulted brief can be parked. A parked one is refused (already
	// parked), as is anything past `acked` (outcome/confirmed/declined/terminal).
	if env.Status != briefSent && env.Status != briefAcked {
		fmt.Fprintf(os.Stderr, "leadkit brief await-user: brief %s is %q - only a %q or %q brief may be parked\n",
			env.ID, env.Status, briefSent, briefAcked)
		os.Exit(1)
	}
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: env.Status, NewStatus: briefAwaitingUser,
		Action: "awaiting-user", Note: *note, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "await-user")
	fmt.Printf("brief parked: %s (status %q) - waiting on the owner; it now shows in `leadkit brief awaiting`\n", env.ID, env.Status)

	// Go cannot SendMessage (an agent tool), so - exactly as `brief send` does - the
	// durable write is the record and the nudge is a printed HINT the agent acts on. The
	// sender is who must be flagged: the overarching lead, who relays to the owner.
	dirs := routing.RecipientDirs{
		ProjectsDir:  *projectsDir,
		RegistryRepo: resolveRegistryRepo(prof, *registryRepo),
		PresenceDir:  *presenceDir,
		RegistryTTL:  prof.RegistryTTLDur(),
		PresenceTTL:  prof.Burn.WorkstreamStaleWindowDur(),
		Now:          time.Now().UTC(),
	}
	fmt.Printf("flag the sender (%s) - parking is the record, the message is the nudge:\n", env.FromSlug)
	fmt.Println(routing.DeliveryHint(env.FromSlug, dirs))
	os.Exit(0)
}

func runBriefResume(args []string) {
	fs := flag.NewFlagSet("brief resume", flag.ContinueOnError)
	note := fs.String("note", "", "optional note on what the owner decided")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief resume <id> [--note <s>]")
		os.Exit(2)
	}
	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireRecipient(env, actor, "resume")

	if env.Status != briefAwaitingUser {
		fmt.Fprintf(os.Stderr, "leadkit brief resume: brief %s is %q - only a %q brief may be resumed\n",
			env.ID, env.Status, briefAwaitingUser)
		os.Exit(1)
	}
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: briefAwaitingUser, NewStatus: priorLiveStatus(env),
		Action: "resumed", Note: *note, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "resume")
	fmt.Printf("brief resumed: %s (status %q)\n", env.ID, env.Status)
	os.Exit(0)
}

// runBriefAwaiting lists every brief parked in awaiting-user, across ALL recipients - the
// owner's one queue of clarifications. Read-only (see the §2.4b note above).
func runBriefAwaiting(args []string) {
	fs := flag.NewFlagSet("brief awaiting", flag.ContinueOnError)
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	store := routing.Open(resolveRoutingDir(*routingDir))
	items, err := store.List(routing.InboxFilter{Kind: briefKind, Status: briefAwaitingUser})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief awaiting: %v\n", err)
		os.Exit(1)
	}
	if len(items) == 0 {
		fmt.Println("brief awaiting: nothing is waiting on the owner")
		os.Exit(0)
	}
	now := time.Now().UTC()
	fmt.Printf("brief awaiting the owner (%d, oldest-parked first):\n", len(items))
	for _, e := range items {
		fmt.Printf("  %s  to=%s  from=%s  parked=%s ago  %s\n",
			e.ID, e.ToSlug, e.FromSlug, now.Sub(e.UpdatedAt).Round(time.Second), e.Subject)
		if q := parkedNote(e); q != "" {
			fmt.Printf("      needs: %s\n", q)
		}
	}
	os.Exit(0)
}

// parkedNote is the question the recipient attached when it parked the brief - the
// latest awaiting-user event's note.
func parkedNote(env routing.Envelope) string {
	for i := len(env.History) - 1; i >= 0; i-- {
		if env.History[i].Action == "awaiting-user" {
			return env.History[i].Note
		}
	}
	return ""
}

// --- escalate / route -------------------------------------------------------
//
// The UP/DOWN half of design §2.1 (ticket 280). A domain lead that finds it needs work
// from a THIRD project does not brief that project: it ESCALATES a brief to the
// overarching lead (`escalate`), who ROUTES it DOWN to the project that owns the work
// (`route`). Without this, leads brief each other directly and the hub-and-spoke shape
// degrades into a mesh.
//
// ARCHITECTURAL CONSTRAINT (design §2.4b) - the same one `awaiting` states, and it holds
// here too. There is NO shared cross-repo queue and there must not be: per-repo
// single-writer stays, and the overarching lead's OWN queue is the backbone. Escalate and
// route move BRIEFS, which live in the account-level routing store; they write only brief
// envelopes there (a new one at most) and NEVER touch any project's task queue - not the
// requester's, not the router's, not the target's. The target creates its own task, in
// its own queue, if and when it acks with --as-task. If a change here seems to need a
// write into another project's queue, the answer is a brief, not a queue write.
//
// DECISIONS (ticket 280 asked for these to be recorded rather than assumed):
//
//  1. Route creates a NEW brief to the target; it does not move the original. The
//     escalated brief B (requester -> router) stays put and goes to `routed`; the router
//     files a fresh child C (router -> target, Parent=B). Why: a brief's file is keyed by
//     its recipient, so a move is a copy plus a delete across two directories that no
//     optimistic-concurrency guard spans (ticket 279 is the race under all brief verbs);
//     and a new brief reuses ack/outcome/confirm unchanged, each hop with the ordinary
//     role guards. The accountability loop stays coherent hop by hop: the target owes its
//     outcome to the ROUTER, who verifies it (confirm) - the hub keeps its audit role - and
//     confirming C relays C's outcome onto B, so the REQUESTER learns the outcome through
//     the brief it itself sent and confirms it as usual. Nobody is laundered: B records who
//     asked, C records (Parent) where it came from.
//  2. "Overarching" is not inferred. The registry records who leads which projects, not a
//     hierarchy, and nothing here reads ambient session context to decide who is on top
//     (the defect class in tickets 105, 54 and 183). Authority is the explicit address on
//     the durable record: the requester names the router in `escalate --to`, and the only
//     party who may `route` that brief is its recipient, resolved from the session's OWN
//     slug like every brief verb. This does not forbid a lead briefing a peer directly -
//     `brief send` is open and always was - so hub-and-spoke remains a protocol the leads
//     follow, not one this code enforces; recording a hierarchy to enforce it is a
//     separate decision (follow-up ticket), not something to guess at.
//  3. Escalate does NOT imply awaiting-user. They block on different parties (another
//     lead vs the owner) and neither sets the other. A brief has one status, so the two
//     do not compose on one brief yet: an escalated or routed brief cannot be parked.

// escalatedBody is the escalated brief's durable body: a generated header carrying the
// requester's suggested target, then the requester's own prose, so the router can route
// without re-interrogating.
func escalatedBody(from, target string, user []byte) []byte {
	return []byte(fmt.Sprintf("Escalated by %s: this work belongs to another project, not to you.\nSuggested target project: %s\nRoute it with `leadkit brief route <id> --to <project>`.\n\n--- requester's brief ---\n%s",
		from, target, user))
}

// routedBody is the child brief's durable body: who routed it, who asked, and
// the whole escalated brief verbatim, so the target sees the true origin of the request.
func routedBody(router string, parent routing.Envelope, parentBody []byte) []byte {
	return []byte(fmt.Sprintf("Routed by %s from escalated brief %s, requested by %s (path: %s -> %s -> target).\nReport your outcome on THIS brief; %s verifies it and relays it to %s.\n\n--- escalated brief %s ---\n%s",
		router, parent.ID, parent.FromSlug, parent.FromSlug, router, router, parent.FromSlug, parent.ID, parentBody))
}

func runBriefEscalate(args []string) {
	fs := flag.NewFlagSet("brief escalate", flag.ContinueOnError)
	to := fs.String("to", "", "the overarching lead's project slug to hand the brief UP to (required)")
	target := fs.String("target", "", "the project that should do the work - a suggestion the router may override (required)")
	subject := fs.String("subject", "", "one-line subject (required)")
	bodyFile := fs.String("body-file", "", "the brief prose file, stored as the item's durable <id>.md body")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "project registry dir used to validate the slugs")
	presenceDir := fs.String("presence-dir", usage.DefaultPresenceDir, "presence registry dir used to resolve the router's live session(s)")
	registryRepo := fs.String("registry-repo", "", "lead-identity registry repo (default: registry_repo)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *to == "" || *target == "" || *subject == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief escalate --to <overarching> --target <project> --subject <s> [--body-file <brief.md>]")
		os.Exit(2)
	}
	prof, actor := resolveBriefActor()
	if actor.slug == "" {
		fmt.Fprintln(os.Stderr, "leadkit brief escalate: this session's own slug could not be resolved - a session with no identity cannot be the requester of record")
		os.Exit(1)
	}
	if *to == actor.slug || *target == actor.slug || *target == *to {
		fmt.Fprintf(os.Stderr, "leadkit brief escalate: --to (%q), --target (%q) and this session (%q) must be three different projects - work for the router itself, or for yourself, is a plain `brief send`\n",
			*to, *target, actor.slug)
		os.Exit(2)
	}
	dirs := routing.RecipientDirs{
		ProjectsDir:  *projectsDir,
		RegistryRepo: resolveRegistryRepo(prof, *registryRepo),
		PresenceDir:  *presenceDir,
		RegistryTTL:  prof.RegistryTTLDur(),
		PresenceTTL:  prof.Burn.WorkstreamStaleWindowDur(),
		Now:          time.Now().UTC(),
	}
	// Both ends are addressing gates: a typo'd router would file the brief where no one
	// picks it up, and a typo'd target would only surface after the router had read it.
	for _, slug := range []string{*to, *target} {
		if err := routing.ResolveRecipient(slug, dirs); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit brief escalate: %v\n", err)
			os.Exit(1)
		}
	}
	var user []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit brief escalate: read --body-file: %v\n", err)
			os.Exit(1)
		}
		user = b
	}
	store := routing.Open(resolveRoutingDir(*routingDir))
	env, err := store.Create(routing.CreateParams{
		Kind: briefKind, FromSlug: actor.slug, FromSession: actor.session, ToSlug: *to,
		Subject: *subject, Status: briefEscalated, Body: escalatedBody(actor.slug, *target, user), Now: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief escalate: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("brief escalated: %s -> %s (status %q; suggested target %s)\n", env.ID, env.ToSlug, env.Status, *target)
	fmt.Println(routing.DeliveryHint(*to, dirs))
	os.Exit(0)
}

func runBriefRoute(args []string) {
	fs := flag.NewFlagSet("brief route", flag.ContinueOnError)
	to := fs.String("to", "", "the project that should own the work (required; validated against the registries)")
	note := fs.String("note", "", "optional note on why this target")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "project registry dir used to validate the target")
	presenceDir := fs.String("presence-dir", usage.DefaultPresenceDir, "presence registry dir used to resolve live session(s)")
	registryRepo := fs.String("registry-repo", "", "lead-identity registry repo (default: registry_repo)")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 || *to == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief route <id> --to <project> [--note <s>]")
		os.Exit(2)
	}
	prof, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	// The recipient of the escalation is the only party who may route it: authority is the
	// explicit address on the record, never a role the session claims or infers.
	requireRecipient(env, actor, "route")
	if env.Status != briefEscalated {
		fmt.Fprintf(os.Stderr, "leadkit brief route: brief %s is %q - only an %q brief may be routed\n", env.ID, env.Status, briefEscalated)
		os.Exit(1)
	}
	if *to == actor.slug || *to == env.FromSlug {
		fmt.Fprintf(os.Stderr, "leadkit brief route: --to %q is the router or the requester - route DOWN to the project that owns the work (to refuse the brief, `leadkit brief decline %s --note ...`)\n", *to, env.ID)
		os.Exit(2)
	}
	dirs := routing.RecipientDirs{
		ProjectsDir:  *projectsDir,
		RegistryRepo: resolveRegistryRepo(prof, *registryRepo),
		PresenceDir:  *presenceDir,
		RegistryTTL:  prof.RegistryTTLDur(),
		PresenceTTL:  prof.Burn.WorkstreamStaleWindowDur(),
		Now:          time.Now().UTC(),
	}
	if err := routing.ResolveRecipient(*to, dirs); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief route: %v\n", err)
		os.Exit(1)
	}
	parentBody, err := store.ReadBody(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief route: read the escalated brief's body: %v\n", err)
		os.Exit(1)
	}

	// Flip the status FIRST (as ack does), so a refused flip files no child. That guard is
	// only as strong as Transition's, which is an unlocked read-check-write (ticket 279): two
	// sessions routing the same brief can both succeed and file two children. This inherits
	// that exposure; it does not close it.
	routedNote := "routed to " + *to
	if *note != "" {
		routedNote += ": " + *note
	}
	parent := briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: briefEscalated, NewStatus: briefRouted,
		Action: "routed", Note: routedNote, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "route")
	child, err := store.Create(routing.CreateParams{
		Kind: briefKind, FromSlug: actor.slug, FromSession: actor.session, ToSlug: *to,
		Subject: env.Subject, Status: briefSent, Parent: env.ID, Body: routedBody(actor.slug, env, parentBody), Now: time.Now().UTC(),
	})
	if err != nil {
		// Put the escalated brief back so it can be routed again rather than stranded.
		_, rerr := store.Transition(briefKind, parent.ToSlug, parent.ID, routing.TransitionParams{
			ExpectedStatus: briefRouted, NewStatus: briefEscalated,
			Action: "route-failed", Note: err.Error(), ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
		})
		fmt.Fprintf(os.Stderr, "leadkit brief route: filing the brief to %s failed: %v (restore of the escalated brief: %v)\n", *to, err, rerr)
		os.Exit(1)
	}
	fmt.Printf("brief routed: %s -> %s as %s (status %q); %s is now %q, parked here until %s confirms the outcome\n",
		parent.ID, *to, child.ID, child.Status, parent.ID, parent.Status, child.ID)
	fmt.Printf("nudge the target (%s):\n%s\n", *to, routing.DeliveryHint(*to, dirs))
	fmt.Printf("tell the requester (%s) it was routed:\n%s\n", parent.FromSlug, routing.DeliveryHint(parent.FromSlug, dirs))
	os.Exit(0)
}

// derivedFrom returns every brief whose Parent is id - the children a route filed.
// Read-only; a store-read failure yields none (it only decorates `show`).
func derivedFrom(store *routing.Store, id string) []routing.Envelope {
	all, err := store.List(routing.InboxFilter{Kind: briefKind})
	if err != nil {
		return nil
	}
	var out []routing.Envelope
	for _, e := range all {
		if e.Parent == id {
			out = append(out, e)
		}
	}
	return out
}

// printRouteTrail is `show`'s view of the escalation path: where a routed child came
// from, and where an escalated brief went and how that is going. Both ends can read the
// live state of the other hop without being a party to it. Read-only.
func printRouteTrail(store *routing.Store, env routing.Envelope) {
	if env.Status == briefEscalated {
		fmt.Printf("  route:   escalated by %s, waiting for %s to route it\n", env.FromSlug, env.ToSlug)
	}
	if env.Parent != "" {
		if p, err := store.Locate(briefKind, env.Parent); err == nil {
			fmt.Printf("  route:   routed by %s from %s, requested by %s\n", env.FromSlug, p.ID, p.FromSlug)
		} else {
			fmt.Printf("  route:   routed from %s (no longer in the store)\n", env.Parent)
		}
	}
	for _, c := range derivedFrom(store, env.ID) {
		fmt.Printf("  route:   routed to %s as %s (status %s)\n", c.ToSlug, c.ID, c.Status)
	}
}

// relayRoutedOutcome carries a confirmed child's outcome back onto the escalated brief it
// was routed from, so the lead that asked learns the result on the brief it
// sent and confirms it as usual. It is the router verifying and passing on, not the target
// answering over the router's head. It does nothing for a brief that was not routed, and
// for a routed one whose parent is not waiting on it (withdrawn, declined, gone): the
// confirm stands and there is no one to tell. Writes only the original brief's envelope.
func relayRoutedOutcome(store *routing.Store, child routing.Envelope, actor briefActor) error {
	if child.Parent == "" {
		return nil
	}
	parent, err := store.Locate(briefKind, child.Parent)
	if err != nil {
		fmt.Printf("outcome not relayed: the originating brief %s is no longer in the store\n", child.Parent)
		return nil
	}
	if parent.Status != briefRouted || parent.ToSlug != actor.slug {
		fmt.Printf("outcome not relayed: the originating brief %s is %q, not waiting on this outcome\n", parent.ID, parent.Status)
		return nil
	}
	out, err := store.ReadNamedBody(child, outcomeBodyName)
	if err != nil {
		return fmt.Errorf("read the outcome of %s: %w", child.ID, err)
	}
	relayed := []byte(fmt.Sprintf("Relayed by %s from brief %s, executed by %s and confirmed by %s.\n\n%s", actor.slug, child.ID, child.ToSlug, actor.slug, out))
	parent, err = store.Transition(briefKind, parent.ToSlug, parent.ID, routing.TransitionParams{
		ExpectedStatus: briefRouted, ExpectedUpdatedAt: parent.UpdatedAt, NewStatus: briefOutcome,
		Action: "outcome-relayed", Note: fmt.Sprintf("from %s (%s)", child.ID, child.ToSlug),
		ActorSlug: actor.slug, ActorSession: actor.session,
		AttachBodies: map[string][]byte{outcomeBodyName: relayed}, Now: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("outcome relayed: %s is now %q - %s confirms it with `leadkit brief confirm %s`\n", parent.ID, parent.Status, parent.FromSlug, parent.ID)
	return nil
}

// --- outcome ----------------------------------------------------------------

func runBriefOutcome(args []string) {
	fs := flag.NewFlagSet("brief outcome", flag.ContinueOnError)
	bodyFile := fs.String("body-file", "", "the outcome/result prose file (required) — stored as the durable <id>.outcome.md sibling")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 || *bodyFile == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief outcome <id> --body-file <outcome.md>")
		os.Exit(2)
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief outcome: read --body-file: %v\n", err)
		os.Exit(1)
	}

	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireRecipient(env, actor, "outcome")

	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: briefAcked, NewStatus: briefOutcome,
		Action: "outcome", ActorSlug: actor.slug, ActorSession: actor.session,
		AttachBodies: map[string][]byte{outcomeBodyName: body}, Now: time.Now().UTC(),
	}, "outcome")
	fmt.Printf("brief outcome recorded: %s (status %q; outcome stored as %s)\n", env.ID, env.Status, env.Bodies[outcomeBodyName])
	os.Exit(0)
}

// --- confirm ----------------------------------------------------------------

func runBriefConfirm(args []string) {
	fs := flag.NewFlagSet("brief confirm", flag.ContinueOnError)
	note := fs.String("note", "", "optional sign-off note")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief confirm <id> [--note <s>]")
		os.Exit(2)
	}
	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireSender(env, actor, "confirm")

	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: briefOutcome, NewStatus: briefConfirmed,
		Action: "confirmed", Note: *note, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "confirm")
	fmt.Printf("brief confirmed: %s (status %q)\n", env.ID, env.Status)
	// A routed child's confirmation is what carries the outcome back to the lead that
	// asked (see relayRoutedOutcome). A brief that was not routed is untouched.
	if err := relayRoutedOutcome(store, env, actor); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit brief confirm: brief confirmed, but relaying the outcome to the originating brief failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// --- close ------------------------------------------------------------------

func runBriefClose(args []string) {
	fs := flag.NewFlagSet("brief close", flag.ContinueOnError)
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief close <id>")
		os.Exit(2)
	}
	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireParty(env, actor, "close")

	// Either party closes a confirmed brief or a declined one (the sender's off-ramp
	// acknowledgement). Any other prior status is not closeable via `close`.
	if env.Status != briefConfirmed && env.Status != briefDeclined {
		fmt.Fprintf(os.Stderr, "leadkit brief close: brief %s is %q — only a %q or %q brief may be closed (withdraw is already terminal)\n",
			env.ID, env.Status, briefConfirmed, briefDeclined)
		os.Exit(1)
	}
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: env.Status, NewStatus: briefClosed, Terminal: true,
		Action: "closed", ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "close")
	fmt.Printf("brief closed: %s (status %q, terminal)\n", env.ID, env.Status)
	os.Exit(0)
}

// --- decline ----------------------------------------------------------------

func runBriefDecline(args []string) {
	fs := flag.NewFlagSet("brief decline", flag.ContinueOnError)
	note := fs.String("note", "", "reason for declining (required — goes back to the sender's attention)")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 || *note == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief decline <id> --note <s>")
		os.Exit(2)
	}
	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireRecipient(env, actor, "decline")

	// A recipient may decline a brief they haven't yet taken to a result: sent or acked -
	// or, for the overarching lead, one escalated to it (it cannot place the work) or one
	// it routed whose child came to nothing (so the requester is told rather than left
	// waiting on a brief that will never relay).
	switch env.Status {
	case briefSent, briefAcked, briefEscalated, briefRouted:
	default:
		fmt.Fprintf(os.Stderr, "leadkit brief decline: brief %s is %q — only a %q, %q, %q or %q brief may be declined\n",
			env.ID, env.Status, briefSent, briefAcked, briefEscalated, briefRouted)
		os.Exit(1)
	}
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: env.Status, NewStatus: briefDeclined,
		Action: "declined", Note: *note, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "decline")
	fmt.Printf("brief declined: %s (status %q — back to the sender's attention; close when acknowledged)\n", env.ID, env.Status)
	os.Exit(0)
}

// --- withdraw ---------------------------------------------------------------

func runBriefWithdraw(args []string) {
	fs := flag.NewFlagSet("brief withdraw", flag.ContinueOnError)
	note := fs.String("note", "", "optional reason for withdrawing")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	rest, err := parseBriefFlags(fs, args)
	if err != nil {
		os.Exit(2)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit brief withdraw <id> [--note <s>]")
		os.Exit(2)
	}
	_, actor := resolveBriefActor()
	store := routing.Open(resolveRoutingDir(*routingDir))
	env := locateBrief(store, rest[0])
	requireSender(env, actor, "withdraw")

	// The sender may pull any NON-terminal brief.
	if env.Terminal() {
		fmt.Fprintf(os.Stderr, "leadkit brief withdraw: brief %s is already terminal (%q) — nothing to withdraw\n", env.ID, env.Status)
		os.Exit(1)
	}
	env = briefTransition(store, env, routing.TransitionParams{
		ExpectedStatus: env.Status, NewStatus: briefWithdrawn, Terminal: true,
		Action: "withdrawn", Note: *note, ActorSlug: actor.slug, ActorSession: actor.session, Now: time.Now().UTC(),
	}, "withdraw")
	fmt.Printf("brief withdrawn: %s (status %q, terminal)\n", env.ID, env.Status)
	os.Exit(0)
}
