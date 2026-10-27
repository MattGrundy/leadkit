package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/lock"
	"mgpp.dev/leadkit/internal/orchestrate"
)

// runOrchestrate is `leadkit orchestrate <plan|apply>`.
func runOrchestrate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit orchestrate <plan|apply> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "plan":
		runOrchestratePlan(args[1:])
	case "apply":
		runOrchestrateApply(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit orchestrate: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// readRosterArg reads --roster's JSON: a file path, or "-" for stdin — the live
// roster the orchestrator agent captured from ListAgents (leadkit itself has no way
// to call that).
func readRosterArg(arg string) ([]orchestrate.RosterEntry, error) {
	var data []byte
	var err error
	if arg == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(arg)
	}
	if err != nil {
		return nil, fmt.Errorf("read roster: %w", err)
	}
	var roster []orchestrate.RosterEntry
	if err := json.Unmarshal(data, &roster); err != nil {
		return nil, fmt.Errorf("parse roster JSON: %w", err)
	}
	return roster, nil
}

// runOrchestratePlan is `leadkit orchestrate plan --roster <file|-> [--json]` —
// READ-ONLY: computes a plan, executes nothing, always exits 0 (a report, not a
// gate). The burn verdict is read the same way the rest of leadkit does: the pause
// lock file IS the disk-persisted form of "burn says HOLD" (see internal/lock) — no
// live usage fetch needed for a deterministic plan.
func runOrchestratePlan(args []string) {
	fs := flag.NewFlagSet("orchestrate plan", flag.ContinueOnError)
	rosterArg := fs.String("roster", "", "path to the live roster JSON (from ListAgents), or - for stdin")
	jsonOut := fs.Bool("json", false, "emit the PlanResult as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate plan: %v\n", err)
		os.Exit(0)
	}
	if *rosterArg == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit orchestrate plan --roster <file|-> [--json]")
		os.Exit(0)
	}

	roster, err := readRosterArg(*rosterArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate plan: %v\n", err)
		os.Exit(0)
	}

	prof := loadProfileOrDefaults()
	lockPath := prof.Resolve(prof.Throttle.LockFile)
	burnHold, meta, _ := lock.Status(lockPath)
	holdReason := ""
	if burnHold {
		holdReason = meta.Reason
	}

	// A gate wired to the profile's records_dir + enforced_remotes (the SAME scope
	// wiring buildPushGateConfig uses) so BuildPlanInput can skip a fully-gated ticket
	// (Fix 2). gate.New() supplies the real git + default records dir; records_dir
	// overrides it only when the profile resolves a non-empty path.
	orchGate := gate.New()
	if rd := prof.Resolve(prof.RecordsDir); rd != "" {
		orchGate.RecordsDir = rd
	}
	orchGate.EnforcedRemotes = prof.EnforcedRemotes
	orchGate.ProjectRoot = prof.ProjectRoot()

	input, err := orchestrate.BuildPlanInput(
		orchGate,
		prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.Resolve(prof.RolesPath),
		filepath.Join(prof.Resolve(prof.Team.TeamDir), "agents"),
		roster, prof.LaneAliases, prof.StatusAliases, 0, burnHold, holdReason,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate plan: %v\n", err)
		os.Exit(0)
	}

	res := orchestrate.Plan(input)

	if *jsonOut {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit orchestrate plan: marshal: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(orchestrate.FormatPlanResult(res))
	os.Exit(0)
}

// runOrchestrateApply is `leadkit orchestrate apply --plan <file> [--apply
// --confirm]` — executes ONLY a plan's route/requeue items, same confirmGuard as
// init/audit/team install: bare --apply refuses.
func runOrchestrateApply(args []string) {
	fs := flag.NewFlagSet("orchestrate apply", flag.ContinueOnError)
	planPath := fs.String("plan", "", "path to a PlanResult JSON (from `orchestrate plan --json`)")
	apply := fs.Bool("apply", false, "execute route/requeue (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually mutate — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate apply: %v\n", err)
		os.Exit(2)
	}
	if *planPath == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit orchestrate apply --plan <file> [--apply --confirm]")
		os.Exit(2)
	}

	data, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate apply: read %s: %v\n", *planPath, err)
		os.Exit(1)
	}
	var plan orchestrate.PlanResult
	if err := json.Unmarshal(data, &plan); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit orchestrate apply: parse %s: %v\n", *planPath, err)
		os.Exit(1)
	}

	prof := loadProfileOrDefaults()
	queueRoot, storeRoot, rolesPath := prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.Resolve(prof.RolesPath)
	logPath := prof.Resolve(".leadkit/orchestrate-apply.jsonl")

	guard := confirmGuard(*apply, *confirm)
	res := orchestrate.ApplyPlan(queueRoot, storeRoot, rolesPath, rosterTypes(prof), plan, guard != guardMutate, logPath)

	fmt.Printf("route: %d, requeue: %d, skipped: %d, errors: %d\n", len(res.Routed), len(res.Requeued), len(res.Skipped), len(res.Errors))
	for _, s := range res.Skipped {
		fmt.Printf("  skip (%s %s): %s\n", s.Kind, s.ID, s.Reason)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}

	switch guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", len(plan.Route)+len(plan.Requeue))
		os.Exit(1)
	default:
		os.Exit(0)
	}
}
