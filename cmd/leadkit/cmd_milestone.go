package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/milestone"
	"mgpp.dev/leadkit/internal/prcreate"
)

// buildMilestoneResolvers wires the REAL signal resolvers for the milestone idle-gate
// from prof: the `tasks` counter reuses internal/hierarchy over store_root; `gate`
// reuses internal/gate records (via newGate); `pr` shells to the forge CLI through
// internal/prcreate's Runner seam (read-only `gh pr view`). `ci` is left nil for now —
// it reads as "no ci resolver wired" → unverified (keeps the gate closed, never a false
// GO), and graduates to a real resolver when that check is built (Layer 3).
func buildMilestoneResolvers(prof *config.Profile) milestone.Resolvers {
	return milestone.Resolvers{
		Tasks: milestone.NewTaskCounter(prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot)),
		Gate:  milestone.NewGateResolver(newGate(prof), ""),
		PR:    milestone.NewGHPRResolver(prcreate.ExecRunner{}, ""),
		// `type: script` — the portable, configurable DoD signal that EXECUTES a
		// lead+user-authored bash command and reads its exit status (supersedes a native
		// CI resolver, which `ci` never wired). It runs behind the enable_script_signals
		// opt-in, timeout-bounded, with the project root as cwd + prof.Resolve for a
		// relative script path. See docs/build-tasks/milestone-script-signal.md.
		Script:        milestone.NewScriptRunner(),
		ScriptEnabled: prof.Milestone.EnableScriptSignals,
		ScriptTimeout: prof.Milestone.ScriptTimeoutDur(),
		ScriptWorkdir: prof.Resolve("."),
		ScriptResolve: prof.Resolve,
	}
}

// loadMilestoneReport parses the milestones_file DoD anchor and evaluates it with the
// real resolvers — the shared body of `milestone status`/`check` and the sweep line.
func loadMilestoneReport(prof *config.Profile) milestone.Report {
	art, present, err := milestone.Parse(prof.Resolve(prof.MilestonesFile))
	if err != nil {
		// A malformed artifact degrades to "not declared" rather than crashing the
		// re-anchor/gate — the error is surfaced by `milestone status` itself when it's
		// the primary command (see runMilestone), but the sweep line must never die on it.
		return milestone.Report{Declared: false}
	}
	return milestone.Evaluate(art, present, buildMilestoneResolvers(prof))
}

// runMilestone dispatches `leadkit milestone <status|check|init>` — the machine-readable
// milestone/DoD anchor's readers (docs/build-tasks/milestone-anchor-idle-gate.md).
func runMilestone(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit milestone <status|check|init> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "status":
		runMilestoneStatus(args[1:])
	case "check":
		runMilestoneCheck(args[1:])
	case "init":
		runMilestoneInit(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit milestone: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runMilestoneStatus is `leadkit milestone status [--json]` — the per-tick re-anchor:
// the current milestone + each DoD condition's met/unmet/unverified state + the open
// milestone-task count. Degrades cleanly to "no milestone DoD declared" (exit 0) when
// the artifact is absent. Always exit 0 — status only reports, it never gates.
func runMilestoneStatus(args []string) {
	fs := flag.NewFlagSet("milestone status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the Report as JSON instead of the human re-anchor")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone status: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	rep := loadMilestoneReport(prof)
	if *jsonOut {
		data, err := json.Marshal(rep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit milestone status: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(milestone.FormatStatus(rep))
	os.Exit(0)
}

// runMilestoneCheck is `leadkit milestone check [--mode block|warn]` — the idle-gate.
// GO (exit 0) only when every DoD condition is met AND zero open milestone tasks.
// Otherwise the mode decides: block → exit non-zero; warn → same report, exit 0. Mode
// resolution: --mode flag > profile milestone.idle_gate_mode > default block. The
// report NAMES exactly what's blocking. An absent artifact BLOCKs (block mode) rather
// than reporting a false GO.
func runMilestoneCheck(args []string) {
	fs := flag.NewFlagSet("milestone check", flag.ContinueOnError)
	mode := fs.String("mode", "", "gate mode: block (exit non-zero when blocked) | warn (report, exit 0). Default: profile milestone.idle_gate_mode, else block")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone check: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	resolvedMode := milestone.ResolveMode(*mode, prof.Milestone.IdleGateMode)
	rep := loadMilestoneReport(prof)
	fmt.Print(milestone.FormatCheck(rep, resolvedMode))
	if rep.Blocked() && resolvedMode == milestone.ModeBlock {
		os.Exit(1)
	}
	os.Exit(0)
}

// milestoneInitTemplate is the starter DoD anchor `milestone init` scaffolds.
const milestoneInitTemplate = `# leadkit milestones — the machine-readable milestone + DoD anchor.
# See docs/PORTABLE-OPERATING-KIT.md. This is the queryable goal ` + "`leadkit milestone" + `
# status` + "`" + ` re-anchors to and ` + "`leadkit milestone check`" + ` gates on. It is DISTINCT
# from the taxonomy's <store_root>/milestones.yaml (project/milestones/epics tree).

# The milestone currently in flight — its id matches the ` + "`milestone:`" + `/` + "`parent:`" + `
# frontmatter on the store tasks that belong to it.
current_milestone: CHANGE-ME

# Ordered Definition-of-Done conditions. Each evaluates to met / unmet / unverified.
# type ∈ tasks | pr | gate | ci | manual:
#   tasks  — met when ZERO open (non-terminal) store tasks resolve to the milestone.
#   pr     — met when every listed PR ref is merged (prs: [...]).
#   gate   — met when a recorded ready gate verdict exists for sha: (or branch:).
#   ci     — met when the named check: is Succeeded.
#   manual — no automated signal yet; defaults to UNVERIFIED (keeps the gate closed
#            until the check is built or a human marks it — you can't declare done what
#            you can't verify).
dod:
  - name: all-milestone-tasks-closed
    type: tasks
  # - name: extraction-prs-merged
  #   type: pr
  #   prs: ["20775", "20776"]
  # - name: spike-green
  #   type: gate
  #   sha: <full-sha>
  # - name: dev-portal-login-smoke
  #   type: manual
`

// runMilestoneInit is `leadkit milestone init` — scaffolds a starter milestones_file
// DoD anchor if one doesn't already exist (never overwrites). Cross-linked from the
// "no milestone DoD declared" degrade message.
func runMilestoneInit(args []string) {
	fs := flag.NewFlagSet("milestone init", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone init: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	path := prof.Resolve(prof.MilestonesFile)
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("milestone init: %s already exists — leaving it untouched\n", path)
		os.Exit(0)
	}
	if err := orphanRuntimeDir(prof, path); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone init: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone init: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, []byte(milestoneInitTemplate), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit milestone init: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("milestone init: wrote %s — edit current_milestone + dod, then run `leadkit milestone status`\n", path)
	os.Exit(0)
}
