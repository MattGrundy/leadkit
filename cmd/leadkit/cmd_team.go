package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/redact"
	"mgpp.dev/leadkit/internal/team"
)

// rosterTypes is the leadkit.yaml `roster:` role names (RosterSpec.Type) — the
// modern routing source of truth queue.Assign/orchestrate.ApplyPlan union into the
// known-role set alongside FixedRoles + the legacy ROLES file. Returns nil for an
// empty roster (KnownRoles then just yields FixedRoles + ROLES, exactly as before).
func rosterTypes(prof *config.Profile) []string {
	if len(prof.Roster) == 0 {
		return nil
	}
	types := make([]string, 0, len(prof.Roster))
	for _, r := range prof.Roster {
		types = append(types, r.Type)
	}
	return types
}

// teamConfig builds team.Config from prof — the same "package doesn't depend on
// internal/config" decoupling every other internal package here follows; only
// cmd/leadkit is allowed to bridge the two.
func teamConfig(prof *config.Profile) team.Config {
	roster := make([]team.RosterSpec, 0, len(prof.Roster))
	for _, r := range prof.Roster {
		roster = append(roster, team.RosterSpec{Type: r.Type, Count: r.Count, Lane: r.Lane, ReviewLayer: r.ReviewLayer})
	}
	return team.Config{
		AgentsDir:    prof.Resolve(prof.Team.AgentsDir),
		ProtocolFile: prof.Resolve(prof.Team.ProtocolFile),
		SopFile:      prof.Resolve(prof.Team.SopFile),
		TeamDir:      prof.Resolve(prof.Team.TeamDir),
		RolesPath:    prof.Resolve(prof.RolesPath),
		SkillsDir:    prof.Resolve(prof.Team.SkillsDir),
		Roster:       roster,
	}
}

// runTeam is `leadkit team <capture|status|install>`.
func runTeam(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit team <capture|status|install|fetch-reference|validate|scaffold|standup> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "capture":
		runTeamCapture(args[1:])
	case "status":
		runTeamStatus(args[1:])
	case "install":
		runTeamInstall(args[1:])
	case "fetch-reference":
		runTeamFetchReference(args[1:])
	case "validate":
		runTeamValidate(args[1:])
	case "scaffold":
		runTeamScaffold(args[1:])
	case "standup":
		runTeamStandup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit team: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runTeamCapture is `leadkit team capture` — PRIMARY, zero-impact: read-only on
// ~/.claude (agents_dir/protocol_file) and the project's sop_file, writes only into
// team_dir inside the repo. No guard needed — this can never touch the live env.
func runTeamCapture(args []string) {
	fs := flag.NewFlagSet("team capture", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team capture: %v\n", err)
		os.Exit(2)
	}
	cfg := teamConfig(loadProfileOrDefaults())
	res, err := team.Capture(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team capture: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("captured into %s:\n", res.TeamDir)
	fmt.Printf("  %d agent def(s): %s\n", len(res.AgentDefs), strings.Join(res.AgentDefs, ", "))
	fmt.Printf("  protocol copied: %v\n", res.ProtocolCopied)
	sopLine := fmt.Sprintf("  sop copied: %v", res.SopCopied)
	if res.SopGenerated {
		sopLine += " (GENERATED STARTER — replace with your project's own SOP)"
	}
	fmt.Println(sopLine)
	fmt.Printf("  inbox-manifest.yaml: %d durable lane(s)\n", res.InboxLanes)
	os.Exit(0)
}

// runTeamStatus is `leadkit team status` — read-only drift check.
func runTeamStatus(args []string) {
	fs := flag.NewFlagSet("team status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team status: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	cfg := teamConfig(prof)
	res, err := team.Status(cfg, prof.Resolve(prof.QueueRoot))
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team status: %v\n", err)
		os.Exit(1)
	}
	if res.Clean() {
		fmt.Println("team status: clean — no drift from the captured snapshot")
		os.Exit(0)
	}
	if len(res.Added) > 0 {
		fmt.Printf("added (live, not captured): %s\n", strings.Join(res.Added, ", "))
	}
	if len(res.Changed) > 0 {
		fmt.Printf("changed (differs from captured): %s\n", strings.Join(res.Changed, ", "))
	}
	if len(res.Removed) > 0 {
		fmt.Printf("removed (captured, gone live): %s\n", strings.Join(res.Removed, ", "))
	}
	if len(res.MissingLanes) > 0 {
		fmt.Printf("missing lanes (in manifest, not on disk): %s\n", strings.Join(res.MissingLanes, ", "))
	}
	os.Exit(1)
}

// runTeamInstall is `leadkit team install [--apply --confirm]` — the only mutating
// team subcommand, same guard as init/audit (see confirmGuard).
//
// Installing is the only definition "resolution" leadkit performs: it writes the user's agents
// directory and keeps any file already there that differs (--replace-existing overrides, with a
// backup). Which definition a session runs is then Claude Code's to decide; leadkit has no
// precedence of its own.
func runTeamInstall(args []string) {
	fs := flag.NewFlagSet("team install", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "reconstruct the team onto this machine (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	starters := fs.Bool("starters", false, "install the clean definitions embedded in the binary, ignoring any captured snapshot — use when the snapshot carries another project's specifics or roles that have since been retired")
	replaceExisting := fs.Bool("replace-existing", false, "overwrite an existing definition, protocol or SOP that differs from its source (each original is copied to a .bak-<timestamp> first). Off by default: install is create-only and keeps files you already have")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team install: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	cfg := teamConfig(prof)
	queueRoot := prof.Resolve(prof.QueueRoot)

	plan, err := team.ComputeInstallPlanWithOptions(cfg, queueRoot, team.InstallOptions{PreferEmbedded: *starters, ReplaceExisting: *replaceExisting})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team install: %v\n", err)
		os.Exit(1)
	}

	if plan.AgentDefsFromEmbedded {
		fmt.Println("source: definitions embedded in this binary (no captured snapshot used)")
	}
	if n := len(plan.DeprecatedCaptured); n > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d captured definition(s) declare themselves deprecated or retired: %s\n",
			n, strings.Join(plan.DeprecatedCaptured, ", "))
		fmt.Fprintln(os.Stderr, "         installing these teaches a pattern their authors abandoned; consider --starters")
	}
	fmt.Print(team.FormatInstallPlan(plan))

	switch guard := confirmGuard(*apply, *confirm); guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		count := 0
		for _, f := range plan.AgentDefs {
			if f.Changed {
				count++
			}
		}
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", count)
		os.Exit(1)
	}

	res, err := team.ApplyInstallPlan(plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team install: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d file(s), created %d lane dir(s)\n", len(res.WrittenFiles), len(res.CreatedDirs))
	for _, name := range res.KeptFiles {
		fmt.Printf("  kept yours %s (exists and differs; not replaced)\n", name)
	}
	for name, backup := range res.BackedUpTo {
		fmt.Printf("  backed up %s -> %s\n", name, backup)
	}
	os.Exit(0)
}

// runTeamScaffold is `leadkit team scaffold [--apply --confirm]` (queue-mode-
// enforcement item 1) — the frictionless-default entry into queue mode: for a
// project with a leadkit.yaml roster but no live queue, computes + (guarded) writes
// the minimum viable queue (one lane per roster role, plus orchestrator/finalise,
// each with pending/in-progress/done) and a starter ROLES file. Same guard as
// team install/init/audit. Idempotent: fills gaps only, never touches an
// already-routed role's lane.
func runTeamScaffold(args []string) {
	fs := flag.NewFlagSet("team scaffold", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the queue lane dirs + ROLES rows (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team scaffold: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	queueRoot := prof.Resolve(prof.QueueRoot)
	rolesPath := prof.Resolve(prof.RolesPath)
	roster := make([]team.RosterSpec, 0, len(prof.Roster))
	for _, r := range prof.Roster {
		roster = append(roster, team.RosterSpec{Type: r.Type, Count: r.Count, Lane: r.Lane, ReviewLayer: r.ReviewLayer})
	}

	plan, err := team.ComputeScaffoldPlan(roster, queueRoot, rolesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team scaffold: %v\n", err)
		os.Exit(2)
	}
	plan.PlanStore(prof.Resolve(prof.StoreRoot))
	fmt.Printf("queue root: %s\n", queueRoot)
	fmt.Printf("roles path: %s\n", rolesPath)
	fmt.Print(team.FormatScaffoldPlan(plan))

	switch guard := confirmGuard(*apply, *confirm); guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", plan.ChangeCount())
		os.Exit(1)
	}

	res, err := team.ApplyScaffoldPlan(plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team scaffold: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created %d dir(s), added %d ROLES row(s)\n", len(res.CreatedDirs), len(res.RolesAdded))
	if hint := team.FirstEnqueueHint(plan); hint != "" {
		fmt.Println(hint)
	} else {
		fmt.Println("no work role is declared in the roster, so there is no lane for a first task yet: add one to leadkit.yaml, then re-run `leadkit team scaffold --apply --confirm`")
	}
	os.Exit(0)
}

// runTeamStandup is `leadkit team standup [--json]` — the SCAFFOLD surface of the
// persistent-owner model (docs/build-tasks/team-standup-teach-check-scaffold.md). It
// READS the roster + FixedRoles + the live queue census and EMITS the concrete
// standup plan: the full consumer set (one persistent owner per roster work-role +
// the fixed review/finalise consumers), each with its exact idle `queue wait`
// invocation, the owner loop, the lead's dispatch-only role, and the cluster-vs-code
// split — annotated with which lanes already have an owner consuming vs are unstaffed.
// It never spawns agents: leadkit has no spawn primitive; spawning stays the lead's
// action in its own harness. Read-only.
func runTeamStandup(args []string) {
	fs := flag.NewFlagSet("team standup", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the standup plan as JSON for tooling instead of the human plan")
	scaffold := fs.Bool("scaffold", false, "create any missing role lane dirs the plan names (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --scaffold to actually create dirs — --scaffold alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team standup: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	queueRoot := prof.Resolve(prof.QueueRoot)
	rolesPath := prof.Resolve(prof.RolesPath)
	roster := make([]team.RosterSpec, 0, len(prof.Roster))
	for _, r := range prof.Roster {
		roster = append(roster, team.RosterSpec{Type: r.Type, Count: r.Count, Lane: r.Lane, ReviewLayer: r.ReviewLayer})
	}

	// Reuse Sweep's per-lane census for the live liveness annotation — a missing/
	// unreachable queue is not fatal: the plan is still worth emitting (the recipe
	// is the point), just without the staffed/unstaffed annotation.
	var lanes []queue.LaneCount
	if res, err := queue.Sweep(queueRoot, false); err == nil {
		lanes = res.Lanes
	}

	plan := team.ComputeStandupPlan(roster, queueRoot, rolesPath, lanes)

	// --scaffold turns standup from describe-only into describe-and-provision: it
	// creates the MISSING role lane dirs the plan names (the exact papercut this exists for — a
	// rostered role that can't be waited/assigned/ROLES-referenced because its lane was
	// never made). Guarded like init/audit/team scaffold: --scaffold alone refuses,
	// --scaffold --confirm actually writes. The no-flag path below is unchanged.
	if *scaffold {
		sp := team.ComputeStandupScaffoldPlan(plan)
		guard := confirmGuard(*scaffold, *confirm)
		if *asJSON {
			// Machine path: emit the plan (dry-run/refused) or, on --confirm, apply and
			// emit the result. Exit code still follows the guard.
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			var payload any = sp
			if guard == guardMutate {
				res, err := team.ApplyStandupScaffoldPlan(sp)
				if err != nil {
					fmt.Fprintf(os.Stderr, "leadkit team standup: %v\n", err)
					os.Exit(1)
				}
				payload = res
			}
			if err := enc.Encode(payload); err != nil {
				fmt.Fprintf(os.Stderr, "leadkit team standup: %v\n", err)
				os.Exit(1)
			}
			if guard == guardRefused {
				os.Exit(1)
			}
			os.Exit(0)
		}
		fmt.Printf("queue root: %s\n", queueRoot)
		fmt.Print(team.FormatStandupScaffoldPlan(sp))
		// Bare --scaffold refuses (the init/audit convention: the action flag alone never
		// mutates) — it has already listed exactly what it WOULD create above, and
		// creates nothing. Only --scaffold --confirm proceeds to the writes below.
		if guard != guardMutate {
			fmt.Printf("REFUSED — --scaffold without --confirm creates nothing (dry-run). This would create %d lane(s); re-run with `--scaffold --confirm` to execute.\n", sp.WouldCreate())
			os.Exit(1)
		}
		res, err := team.ApplyStandupScaffoldPlan(sp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit team standup: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("created %d lane(s) (%d dir(s)); %d already present; %d refused\n",
			len(res.CreatedLanes), len(res.CreatedDirs), len(res.PresentLanes), len(res.RefusedLanes))
		os.Exit(0)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit team standup: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	fmt.Print(team.FormatStandupPlan(plan))
	os.Exit(0)
}

// runTeamFetchReference is `leadkit team fetch-reference [--repo <url>] [--subdir
// <path>] [--ref <tag/sha>] [--name <name>]` — a public, read-only download, vendored
// into team_dir/reference/<name>/. Not a --apply-guarded command: it never touches
// the live env, and re-running it just replaces its own prior vendored snapshot (the
// same "capture is inherently safe" reasoning as `team capture`).
func runTeamFetchReference(args []string) {
	fs := flag.NewFlagSet("team fetch-reference", flag.ContinueOnError)
	repo := fs.String("repo", team.DefaultReferenceRepo, "reference collection's git repo URL")
	ref := fs.String("ref", team.DefaultReferenceRef, "branch/tag to shallow-clone (pin an exact tag for reproducibility)")
	subdir := fs.String("subdir", "", "only consider .md files under this path in the repo (default: \"categories\" for the default VoltAgent repo, else whole repo)")
	name := fs.String("name", "", "reference dir name under team_dir/reference/ (default: derived from --repo)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team fetch-reference: %v\n", err)
		os.Exit(2)
	}

	effectiveSubdir := *subdir
	if effectiveSubdir == "" && *repo == team.DefaultReferenceRepo {
		// VoltAgent's own agent defs live under categories/ — the rest of the repo
		// (README, tooling docs) is real .md content but not agent defs; excluding it
		// keeps `team validate`'s coverage-gap categories meaningful instead of
		// picking up e.g. a stray "subagent-catalog" pseudo-category from tooling docs.
		effectiveSubdir = "categories"
	}

	cfg := teamConfig(loadProfileOrDefaults())
	res, err := team.FetchReference(team.FetchReferenceOptions{
		RepoURL: *repo, Ref: *ref, Subdir: effectiveSubdir, Name: *name, TeamDir: cfg.TeamDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team fetch-reference: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(fetchedSummary(len(res.Files), *repo, *ref, res.ResolvedRef, res.Dir))
	os.Exit(0)
}

// fetchedSummary is the success line of `team fetch-reference`. The repo is whatever URL the caller
// typed, which can carry a credential, so it is shown without its userinfo.
func fetchedSummary(files int, repo, ref, resolved, dir string) string {
	return fmt.Sprintf("fetched %d .md file(s) from %s@%s (resolved %s) into %s", files, redact.Origin(repo), ref, resolved, dir)
}

// runTeamValidate is `leadkit team validate [--reference <dir>] [--json]` —
// READ-ONLY, advisory. Always exits 0: this is a report, never a gate.
func runTeamValidate(args []string) {
	fs := flag.NewFlagSet("team validate", flag.ContinueOnError)
	referenceDir := fs.String("reference", "", "reference dir to compare against (default: team_dir/reference/<default repo name>)")
	jsonOut := fs.Bool("json", false, "emit the ValidateResult as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit team validate: %v\n", err)
		os.Exit(0)
	}

	cfg := teamConfig(loadProfileOrDefaults())
	refDir := *referenceDir
	if refDir == "" {
		refDir = filepath.Join(cfg.TeamDir, "reference", team.DeriveRepoName(team.DefaultReferenceRepo))
	}

	res, err := team.Validate(filepath.Join(cfg.TeamDir, "agents"), cfg.AgentsDir, refDir)
	if err != nil {
		// Report the error AND whatever was gathered before it. Validate computes starter
		// drift before the read that can fail, precisely so a machine with no definitions
		// installed still learns what is available — discarding res here threw that away in
		// the one case it was written for.
		fmt.Fprintf(os.Stderr, "leadkit team validate: %v\n", err)
		if len(res.StarterDrift) > 0 {
			fmt.Print(team.FormatValidateResult(res))
		}
		os.Exit(0)
	}

	if *jsonOut {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit team validate: marshal: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}

	fmt.Print(team.FormatValidateResult(res))
	os.Exit(0)
}
