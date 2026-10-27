package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/registry"
	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// routingDirEnvVar overrides the routing store dir (usage.DefaultRoutingDir) for the CLI
// AND the routing-inbox hook — the hook can't take a flag (it reads stdin), so an env is
// the one seam a test/operator has to point it at a scratch store.
const routingDirEnvVar = "LEADKIT_ROUTING_DIR"

// resolveRoutingDir picks the routing store dir: an explicit flag wins, else
// $LEADKIT_ROUTING_DIR, else usage.DefaultRoutingDir.
func resolveRoutingDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv(routingDirEnvVar); env != "" {
		return env
	}
	return usage.DefaultRoutingDir
}

// inboxTargets resolves the slug(s) an `inbox` listing covers. An explicit --slug is an
// exact, single-inbox request (honored verbatim, unchanged). Otherwise the target is the
// session's OWN resolved slug UNION every workstream it `register`ed as leading (from the
// git-backed registry) — so a lead whose cwd slug ≠ its led workstream sees that
// workstream's inbox. Best-effort: a registry read failure degrades to own-slug only. The
// returned slugs are de-duplicated, own-slug first, then the led workstreams sorted.
func inboxTargets(prof *config.Profile, explicitSlug, registryRepoFlag string) []string {
	if explicitSlug != "" {
		return []string{explicitSlug}
	}
	ownSlug, _ := sessionSlugAndID(prof)
	if ownSlug == "" {
		return nil
	}
	targets := []string{ownSlug}
	seen := map[string]bool{ownSlug: true}
	for _, ws := range sessionLedWorkstreams(prof, registryRepoFlag) {
		if ws == "" || seen[ws] {
			continue
		}
		seen[ws] = true
		targets = append(targets, ws)
	}
	return targets
}

// sessionLedWorkstreams returns the workstream/project slug(s) THIS session registered as
// leading, read from its OWN entry in the git-backed identity registry (matched by
// host+session_id — a session only ever reads its own declared leads). Best-effort: an
// unreadable/absent registry, or no entry for this session, yields nil (own-slug only).
func sessionLedWorkstreams(prof *config.Profile, registryRepoFlag string) []string {
	host, sessionID, _ := registryIdentity(prof)
	if host == "" || sessionID == "" {
		return nil
	}
	all, ok := registry.Open(resolveRegistryRepo(prof, registryRepoFlag)).ReadAll()
	if !ok {
		return nil
	}
	for _, e := range all {
		if e.Host == host && e.SessionID == sessionID {
			return e.LeadsOrProject()
		}
	}
	return nil
}

// unionInbox reads every target slug's inbox and merges them into one newest-first listing,
// de-duplicating by envelope id (a defensive measure — the same item is only ever stored
// under a single to-slug, but a target set could in principle overlap). A read error on any
// single target is fatal (the same "can't read the store" condition a single-slug inbox
// surfaces); an empty target contributes nothing.
func unionInbox(store *routing.Store, targets []string, f routing.InboxFilter) ([]routing.Envelope, error) {
	var merged []routing.Envelope
	seen := map[string]bool{}
	for _, t := range targets {
		items, err := store.Inbox(t, f)
		if err != nil {
			return nil, err
		}
		for _, e := range items {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			merged = append(merged, e)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].CreatedAt.After(merged[j].CreatedAt)
		}
		return merged[i].ID > merged[j].ID
	})
	return merged, nil
}

// runRouting is `leadkit routing <send|inbox|prune>` — the KIND-AGNOSTIC account-level
// routing substrate CLI (docs/build-tasks/routing-substrate.md). It carries no `brief`/
// `gate` specifics; builds 2/3 wrap these generic verbs.
func runRouting(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit routing <send|inbox|prune> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "send":
		runRoutingSend(args[1:])
	case "inbox":
		runRoutingInbox(args[1:])
	case "prune":
		runRoutingPrune(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit routing: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runRoutingSend is `leadkit routing send` — create a durable, project-addressed item
// and PRINT a presence-derived delivery hint. It NEVER sends a nudge itself (Go cannot
// SendMessage); the durable write is the source of truth, the printed hint is what the
// sending agent acts on.
func runRoutingSend(args []string) {
	fs := flag.NewFlagSet("routing send", flag.ContinueOnError)
	kind := fs.String("kind", "", "item kind label ([a-z][a-z0-9-]*) — the generic type, e.g. `brief`/`gate` in later builds")
	to := fs.String("to", "", "recipient PROJECT SLUG (or a reserved sigil like @review-pool)")
	from := fs.String("from", "", "sender project slug (default: this session's resolved slug)")
	subject := fs.String("subject", "", "one-line subject")
	status := fs.String("status", "open", "the kind's initial status (opaque to the substrate)")
	bodyFile := fs.String("body-file", "", "optional prose body file stored as the item's <id>.md sibling")
	routingDir := fs.String("routing-dir", "", "override the routing store dir (default: $LEADKIT_ROUTING_DIR or ~/.claude/leadkit/routing)")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "project registry dir used to validate the recipient slug")
	presenceDir := fs.String("presence-dir", usage.DefaultPresenceDir, "presence registry dir used to resolve the recipient's live session(s)")
	registryRepo := fs.String("registry-repo", "", "lead-identity registry repo used to resolve a registry-led workstream recipient (default: registry_repo)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *kind == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "leadkit routing send: --kind and --to are required")
		os.Exit(2)
	}
	if !routing.ValidKind(*kind) {
		fmt.Fprintf(os.Stderr, "leadkit routing send: invalid --kind %q (want [a-z][a-z0-9-]*)\n", *kind)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	sessionSlug, sessionID := sessionSlugAndID(prof)
	fromSlug := *from
	if fromSlug == "" {
		fromSlug = sessionSlug
	}

	// Addressing is a correctness gate: refuse an unknown, non-sigil recipient rather
	// than filing an item at an address no session can ever pick up. A recipient is valid
	// if it is a sigil, a registered project, a registry-LED workstream, or a live-presence
	// slug — resolved best-effort across the three account-level registries.
	dirs := routing.RecipientDirs{
		ProjectsDir:  *projectsDir,
		RegistryRepo: resolveRegistryRepo(prof, *registryRepo),
		PresenceDir:  *presenceDir,
		RegistryTTL:  prof.RegistryTTLDur(),
		PresenceTTL:  prof.Burn.WorkstreamStaleWindowDur(),
		Now:          time.Now().UTC(),
	}
	if err := routing.ResolveRecipient(*to, dirs); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit routing send: %v\n", err)
		os.Exit(1)
	}

	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit routing send: read --body-file: %v\n", err)
			os.Exit(1)
		}
		body = b
	}

	store := routing.Open(resolveRoutingDir(*routingDir))
	env, err := store.Create(routing.CreateParams{
		Kind: *kind, FromSlug: fromSlug, FromSession: sessionID, ToSlug: *to,
		Subject: *subject, Status: *status, Body: body, Now: time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit routing send: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("routing: created %s/%s -> %s (status %q)\n", env.Kind, env.ID, env.ToSlug, env.Status)
	fmt.Println(routing.DeliveryHint(*to, dirs))
}

// runRoutingInbox is `leadkit routing inbox` — list items addressed to a slug (this
// session's project by default), newest-first, with an optional --json machine form
// (build 2/3 wrap it).
func runRoutingInbox(args []string) {
	fs := flag.NewFlagSet("routing inbox", flag.ContinueOnError)
	slug := fs.String("slug", "", "recipient slug to list (default: this session's resolved slug UNION the workstreams it registered as leading)")
	kind := fs.String("kind", "", "filter by kind")
	status := fs.String("status", "", "filter by status")
	jsonOut := fs.Bool("json", false, "emit the items as JSON")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	registryRepo := fs.String("registry-repo", "", "override registry_repo (used to union the workstreams this session leads)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	targets := inboxTargets(prof, *slug, *registryRepo)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "leadkit routing inbox: no --slug given and this session's slug could not be resolved")
		os.Exit(2)
	}
	store := routing.Open(resolveRoutingDir(*routingDir))
	items, err := unionInbox(store, targets, routing.InboxFilter{Kind: *kind, Status: *status})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit routing inbox: %v\n", err)
		os.Exit(1)
	}
	label := strings.Join(targets, ",")
	if *jsonOut {
		data, err := json.Marshal(items)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit routing inbox: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	if len(items) == 0 {
		fmt.Printf("routing inbox for %s: (empty)\n", label)
		os.Exit(0)
	}
	now := time.Now().UTC()
	fmt.Printf("routing inbox for %s (%d item(s), newest first):\n", label, len(items))
	for _, e := range items {
		term := ""
		if e.Terminal() {
			term = " [terminal]"
		}
		fmt.Printf("  %s  kind=%s  status=%s  from=%s  age=%s%s  %s\n",
			e.ID, e.Kind, e.Status, e.FromSlug, now.Sub(e.CreatedAt).Round(time.Second), term, e.Subject)
	}
	os.Exit(0)
}

// runRoutingPrune is `leadkit routing prune` — retention. DRY-RUN by default: lists
// terminal items past routing_ttl. --apply removes ONLY those (envelope + body together),
// NEVER a non-terminal item, at any age.
func runRoutingPrune(args []string) {
	fs := flag.NewFlagSet("routing prune", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "actually remove the terminal items past the TTL (default: dry-run)")
	ttlFlag := fs.String("ttl", "", "override routing_ttl for this run (a time.ParseDuration string)")
	routingDir := fs.String("routing-dir", "", "override the routing store dir")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	ttl := prof.RoutingTTLDur()
	if *ttlFlag != "" {
		if d, err := time.ParseDuration(*ttlFlag); err == nil && d > 0 {
			ttl = d
		} else {
			fmt.Fprintf(os.Stderr, "leadkit routing prune: invalid --ttl %q\n", *ttlFlag)
			os.Exit(2)
		}
	}
	store := routing.Open(resolveRoutingDir(*routingDir))
	res, err := store.Prune(ttl, time.Now().UTC(), *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit routing prune: %v\n", err)
		os.Exit(1)
	}
	if len(res.Candidates) == 0 {
		fmt.Printf("routing prune: nothing to prune (no terminal items older than %s; non-terminal items are never pruned)\n", ttl)
		os.Exit(0)
	}
	for _, c := range res.Candidates {
		verb := "would remove"
		if *apply {
			verb = "removed"
		}
		fmt.Printf("  %s %s/%s -> %s (status %q, closed %s ago)\n", verb, c.Env.Kind, c.Env.ID, c.Env.ToSlug, c.Env.Status, c.Age.Round(time.Second))
	}
	if *apply {
		fmt.Printf("routing prune: removed %d terminal item(s) past %s\n", len(res.Removed), ttl)
	} else {
		fmt.Printf("routing prune: %d terminal item(s) past %s (dry-run — nothing removed; re-run with --apply)\n", len(res.Candidates), ttl)
	}
	os.Exit(0)
}
