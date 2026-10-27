package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/redact"
	"mgpp.dev/leadkit/internal/registry"
)

// whoamiInfo is `leadkit whoami`'s resolved view — the session's cwd-honest IDENTITY
// (slug + source, session-id + source, the harness handle) alongside the ambient POLICY
// slug, so a mis-identified session sees the divergence instantly.
type whoamiInfo struct {
	IdentitySlug    string `json:"identity_slug"`
	IdentitySource  string `json:"identity_source"`
	SessionID       string `json:"session_id"`
	SessionIDSource string `json:"session_id_source"`
	MessagingSocket string `json:"messaging_socket,omitempty"`
	HarnessSession  string `json:"harness_session,omitempty"`
	GitToplevel     string `json:"git_toplevel,omitempty"`
	Cwd             string `json:"cwd"`
	PolicySlug      string `json:"policy_slug"`
	Mismatch        bool   `json:"mismatch"`
	// ConfigSource names WHICH config resolved (defaults/in-repo/external) and
	// ConfigLocation from where (the file path; empty for defaults) — the external-config
	// resolution's source line (docs/build-tasks/external-config-resolution.md).
	ConfigSource   string `json:"config_source"`
	ConfigLocation string `json:"config_location,omitempty"`
	// WouldRead (defaults only) is the external config file leadkit would read if created.
	WouldRead string `json:"would_read,omitempty"`
}

// runWhoami is `leadkit whoami [--json]` — the identity diagnostic
// (docs/build-tasks/session-identity-cwd-honest.md). It prints the resolved IDENTITY
// slug + its source (own project_id / own queue_root / git-repo or cwd basename), the
// resolved session-id + its source, the harness messaging socket, the session's git
// toplevel + cwd, and — crucially — the OLD POLICY-resolved slug (what config.Load →
// install.ResolveProjectSlug gives) when it DIFFERS, with a MISMATCH warning. Mirrors
// `config show`'s effective-line: a mis-identified session (an inherited
// $LEADKIT_PROFILE/env or an ancestor leadkit.yaml shadowing its real project) sees it
// at a glance. Read-only, always exit 0.
func runWhoami(args []string) {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the resolved identity as JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit whoami: %v\n", err)
		os.Exit(2)
	}

	cwd := sessionCwd()
	idSlug, idSource := sessionIdentitySlug(cwd)
	sessionID, sessionIDSource := resolveSessionID(idSlug)

	gitTop := ""
	if cwd != "" {
		if top, ok := gitToplevel(cwd); ok {
			gitTop = top
		}
	}

	// The POLICY slug: what the ambient profile (config.Load — honors $LEADKIT_PROFILE /
	// walks up to an ancestor leadkit.yaml / an external <slug>.yaml) resolves.
	// Deliberately the UNCHANGED policy path; whoami exists to SURFACE the divergence,
	// not hide it. The path lets whoami name WHICH config resolved + from where.
	prof, profPath, loadErr := config.Load()
	if loadErr != nil || prof == nil {
		d := config.Defaults()
		prof, profPath = &d, ""
	}
	policySlug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	cfgKind, cfgLoc, _ := config.ConfigSource(prof, profPath)

	info := whoamiInfo{
		IdentitySlug:    idSlug,
		IdentitySource:  idSource,
		SessionID:       sessionID,
		SessionIDSource: sessionIDSource,
		MessagingSocket: os.Getenv(messagingSocketEnvVar),
		HarnessSession:  os.Getenv(harnessSessionEnvVar),
		GitToplevel:     gitTop,
		Cwd:             cwd,
		PolicySlug:      policySlug,
		Mismatch:        policySlug != "" && policySlug != idSlug,
		ConfigSource:    cfgKind,
		ConfigLocation:  cfgLoc,
	}
	var locs config.Locations
	if cfgKind == "defaults" {
		locs = config.ResolveLocations(cwd)
		info.WouldRead = locs.ExternalPath
	}

	if *jsonOut {
		data, err := json.Marshal(info)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit whoami: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}

	fmt.Printf("identity slug:   %s (source: %s)\n", info.IdentitySlug, info.IdentitySource)
	fmt.Printf("session id:      %s (source: %s)\n", info.SessionID, info.SessionIDSource)
	socket := info.MessagingSocket
	if socket == "" {
		socket = "(none — $CLAUDE_CODE_MESSAGING_SOCKET unset)"
	}
	fmt.Printf("messaging socket: %s\n", socket)
	if info.GitToplevel != "" {
		fmt.Printf("git toplevel:    %s\n", info.GitToplevel)
	} else {
		fmt.Printf("git toplevel:    (cwd is not a git repo)\n")
	}
	fmt.Printf("cwd:             %s\n", info.Cwd)
	if info.ConfigLocation != "" {
		fmt.Printf("config source:   %s (%s)\n", info.ConfigSource, info.ConfigLocation)
	} else {
		fmt.Printf("config source:   %s\n", info.ConfigSource)
	}
	if hint := locs.DefaultsHint(); hint != "" {
		fmt.Printf("                 %s\n", hint)
	}
	fmt.Printf("policy slug:     %s\n", info.PolicySlug)
	if info.Mismatch {
		fmt.Printf("MISMATCH: identity=%s but the ambient profile resolves to %s — an inherited $LEADKIT_PROFILE/env or an ancestor leadkit.yaml; your inbox/addressing uses %s.\n",
			info.IdentitySlug, info.PolicySlug, info.IdentitySlug)
	} else {
		fmt.Printf("OK: identity and ambient policy slug agree (%s).\n", info.IdentitySlug)
	}
	os.Exit(0)
}

// registryIdentity resolves THIS session's OWN identity for the git-backed registry: host
// from the machine (os.Hostname), session id + project slug from the tool
// (sessionSlugAndID). This is the own-identity rule — host/session_id come from the tool,
// NEVER from a flag, so a session can only ever write its OWN entry (no spoofing).
func registryIdentity(prof *config.Profile) (host, sessionID, project string) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	slug, sid := sessionSlugAndID(prof)
	return host, sid, slug
}

// resolveRegistryRepo picks the registry repo path: an explicit --registry-repo flag wins,
// else the profile's registry_repo (Resolve handles ~/ and $ENV; default
// ~/.claude/leadkit/registry).
func resolveRegistryRepo(prof *config.Profile, flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return prof.Resolve(prof.RegistryRepo)
}

// printRegistrySync prints a one-line best-effort sync outcome — never an error (a sync
// failure is degraded infra, not a command failure).
func printRegistrySync(s registry.SyncResult) {
	switch {
	case s.Pushed:
		fmt.Println("  synced to the shared registry (pushed)")
	case s.Note != "":
		fmt.Printf("  sync: %s\n", s.Note)
	case s.Committed:
		fmt.Println("  committed to the local registry (no push attempted)")
	}
}

// runRegister is `leadkit register --role <r> [--leads a,b] [--name n] [--touch] [--clear]`:
// this session declares ITS OWN entry in the git-backed identity registry, then best-effort
// syncs. host/session_id are resolved by the tool (own-identity, never a flag). A git/
// network failure NEVER errors the command — identity is best-effort infra.
func runRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	role := fs.String("role", "", "this session's role: lead|ic|reviewer|watcher")
	leadsFlag := fs.String("leads", "", "comma-separated workstream/project slug(s) this session LEADS (for --role lead; usually the project)")
	name := fs.String("name", "", "display name for this session (default: the resolved session id)")
	touch := fs.Bool("touch", false, "refresh only updated_at (heartbeat) — keep the existing role/leads")
	clear := fs.Bool("clear", false, "remove this session's entry (graceful exit)")
	repoFlag := fs.String("registry-repo", "", "override registry_repo for this run")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	host, sessionID, project := registryIdentity(prof)
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "leadkit register: this session's id could not be resolved (no project slug and $LEADKIT_SESSION_ID unset) — nothing stable to key an entry on")
		os.Exit(1)
	}
	store := registry.Open(resolveRegistryRepo(prof, *repoFlag)).WithRemote(prof.RegistryRemote)
	now := time.Now().UTC()

	if *clear {
		_, sync, err := store.Register(registry.RegisterParams{SessionID: sessionID, Host: host, Name: *name, Now: now, Clear: true})
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit register: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("register: cleared this session's entry (%s@%s)\n", displayName(*name, sessionID), host)
		printRegistrySync(sync)
		os.Exit(0)
	}

	if !*touch && *role == "" {
		fmt.Fprintln(os.Stderr, "leadkit register: --role is required (lead|ic|reviewer|watcher); use --touch to only refresh an existing entry, or --clear to remove it")
		os.Exit(2)
	}
	if *role != "" && !registry.ValidRole(*role) {
		fmt.Fprintf(os.Stderr, "leadkit register: invalid --role %q (want lead|ic|reviewer|watcher)\n", *role)
		os.Exit(2)
	}

	var leads []string
	for _, l := range strings.Split(*leadsFlag, ",") {
		if t := strings.TrimSpace(l); t != "" {
			leads = append(leads, t)
		}
	}

	e, sync, err := store.Register(registry.RegisterParams{
		SessionID: sessionID, Name: *name, Host: host, Role: registry.Role(*role),
		Project: project, Leads: leads, Now: now, Touch: *touch,
		// The HARNESS ADDRESSING HANDLE from env (best-effort, may be empty), so
		// routing.DeliveryHint can name a real SendMessage target when it resolves a
		// recipient through this registry (a lead of a workstream whose cwd slug differs).
		MessagingSocket: os.Getenv(messagingSocketEnvVar),
		HarnessSession:  os.Getenv(harnessSessionEnvVar),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit register: %v\n", err)
		os.Exit(1)
	}
	leadsStr := "-"
	if len(e.Leads) > 0 {
		leadsStr = strings.Join(e.Leads, ",")
	}
	fmt.Printf("register: %s@%s role=%s leads=%s project=%s\n", e.DisplayName(), e.Host, e.Role, leadsStr, e.Project)
	printRegistrySync(sync)
	os.Exit(0)
}

// displayName is a session's display name, falling back to its id when unnamed (the CLI's
// own copy of registry.Entry.DisplayName for the pre-write --clear path).
func displayName(name, sessionID string) string {
	if name != "" {
		return name
	}
	return sessionID
}

// runWho is `leadkit who [--project X] [--role R] [--host H] [--all] [--json]`: SYNC-pull
// first (best-effort), then read all LIVE entries (updated_at within registry_ttl) and
// print a table — flagging AMBIGUOUS-LEADERSHIP when 2+ live leads claim one slug. On a
// git failure it falls back to the local cache marked "(possibly stale — sync failed)".
func runWho(args []string) {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	project := fs.String("project", "", "filter to sessions for/leading this project/workstream slug")
	role := fs.String("role", "", "filter to this role (lead|ic|reviewer|watcher)")
	host := fs.String("host", "", "filter to this host")
	all := fs.Bool("all", false, "include stale entries (past registry_ttl)")
	jsonOut := fs.Bool("json", false, "emit the entries as JSON")
	repoFlag := fs.String("registry-repo", "", "override registry_repo for this run")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	store := registry.Open(resolveRegistryRepo(prof, *repoFlag)).WithRemote(prof.RegistryRemote)

	// LOCAL mode returns (true, "") — the local dir is the complete registry, so the table
	// below is clean (no "possibly stale"/"not initialized" framing). SYNC mode best-effort
	// pulls and degrades to the local cache on failure.
	syncOK, syncNote := store.PullBestEffort()
	entries, ok := store.ReadAll()
	if !ok {
		// A real read failure (not just "nothing registered") — degrade to empty, never error.
		fmt.Println("who: registry is unreadable — showing no entries (registry is best-effort infra, not a failure)")
		os.Exit(0)
	}

	now := time.Now().UTC()
	cutoff := now.Add(-prof.RegistryTTLDur())
	filter := registry.Filter{Project: *project, Role: *role, Host: *host}
	live := registry.Live(entries, cutoff)
	liveFiltered := filter.Apply(live)

	var staleFiltered []registry.Entry
	if *all {
		var stale []registry.Entry
		for _, e := range entries {
			if e.UpdatedAt.Before(cutoff) {
				stale = append(stale, e)
			}
		}
		staleFiltered = filter.Apply(stale)
	}

	note := ""
	if !syncOK {
		note = "(possibly stale — sync failed: " + syncNote + ")"
	}

	if *jsonOut {
		payload := struct {
			Live      []registry.Entry     `json:"live"`
			Stale     []registry.Entry     `json:"stale,omitempty"`
			Ambiguous []registry.LeadClaim `json:"ambiguous,omitempty"`
			SyncOK    bool                 `json:"sync_ok"`
			SyncNote  string               `json:"sync_note,omitempty"`
		}{
			Live:      liveFiltered,
			Stale:     staleFiltered,
			Ambiguous: registry.AmbiguousLeadership(live),
			SyncOK:    syncOK,
			SyncNote:  syncNote,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit who: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}

	fmt.Print(registry.FormatWho(liveFiltered, staleFiltered, note, now))
	os.Exit(0)
}

// runRegistry is `leadkit registry <init|prune>` — the registry's admin surface.
func runRegistry(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit registry <init|prune> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "init":
		runRegistryInit(args[1:])
	case "prune":
		runRegistryPrune(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit registry: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runRegistryInit is `leadkit registry init --remote <url>`: clone/attach the shared
// registry repo to registry_repo. IDEMPOTENT — an already-initialized repo (a git repo
// with an origin) is a no-op that reports the existing remote.
func runRegistryInit(args []string) {
	fs := flag.NewFlagSet("registry init", flag.ContinueOnError)
	remote := fs.String("remote", "", "shared registry git remote URL to clone/attach (default: registry_remote)")
	repoFlag := fs.String("registry-repo", "", "override registry_repo for this run")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	remoteURL := *remote
	if remoteURL == "" {
		remoteURL = prof.RegistryRemote
	}
	store := registry.Open(resolveRegistryRepo(prof, *repoFlag))
	res, err := store.Init(remoteURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit registry init: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("registry init: %s at %s\n", res.Action, res.Repo)
	if res.Remote != "" {
		fmt.Printf("  remote: %s\n", redact.Origin(res.Remote))
	}
	if res.Note != "" {
		fmt.Printf("  %s\n", res.Note)
	}
	os.Exit(0)
}

// runRegistryPrune is `leadkit registry prune [--apply]`: remove stale entry files
// (updated_at past registry_ttl). DRY-RUN by default; --apply removes them and best-effort
// syncs the removal.
func runRegistryPrune(args []string) {
	fs := flag.NewFlagSet("registry prune", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "actually remove the stale entries and sync the removal (default: dry-run)")
	ttlFlag := fs.String("ttl", "", "override registry_ttl for this run (a time.ParseDuration string)")
	repoFlag := fs.String("registry-repo", "", "override registry_repo for this run")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	ttl := prof.RegistryTTLDur()
	if *ttlFlag != "" {
		if d, err := time.ParseDuration(*ttlFlag); err == nil && d > 0 {
			ttl = d
		} else {
			fmt.Fprintf(os.Stderr, "leadkit registry prune: invalid --ttl %q\n", *ttlFlag)
			os.Exit(2)
		}
	}
	store := registry.Open(resolveRegistryRepo(prof, *repoFlag)).WithRemote(prof.RegistryRemote)
	now := time.Now().UTC()
	res, err := store.Prune(now.Add(-ttl), *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit registry prune: %v\n", err)
		os.Exit(1)
	}
	if len(res.Candidates) == 0 {
		fmt.Printf("registry prune: nothing to prune (no entries staler than %s)\n", ttl)
		os.Exit(0)
	}
	for _, e := range res.Candidates {
		verb := "would remove"
		if *apply {
			verb = "removed"
		}
		fmt.Printf("  %s %s@%s (role %s, age %s)\n", verb, e.DisplayName(), e.Host, e.Role, now.Sub(e.UpdatedAt).Round(time.Second))
	}
	if *apply {
		fmt.Printf("registry prune: removed %d stale entr%s past %s\n", len(res.Removed), pluralY(len(res.Removed)), ttl)
		printRegistrySync(res.Sync)
	} else {
		fmt.Printf("registry prune: %d stale entr%s past %s (dry-run — nothing removed; re-run with --apply)\n", len(res.Candidates), pluralY(len(res.Candidates)), ttl)
	}
	os.Exit(0)
}

// pluralY pluralizes "entry"/"entries".
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
