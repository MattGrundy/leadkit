package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hierarchy"
	"mgpp.dev/leadkit/internal/hierarchyapply"
)

// runHierarchy is `leadkit hierarchy <show|propose>` — both READ-ONLY, zero-impact.
func runHierarchy(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit hierarchy <show|propose|cluster-issues|apply> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "show":
		runHierarchyShow(args[1:])
	case "propose":
		runHierarchyPropose(args[1:])
	case "cluster-issues":
		runHierarchyClusterIssues(args[1:])
	case "apply":
		runHierarchyApply(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit hierarchy: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// loadHierarchyModel reads <store_root>/milestones.yaml (docs/HIERARCHY-AND-SYNC.md's
// own wording: "milestones.yaml at the store root") + every store task's frontmatter.
// A missing milestones.yaml is not fatal — hierarchy.Parse's own "not found" case —
// both hierarchy subcommands still run (an empty model, so everything reads as
// Unmapped) rather than erroring before the model is seeded. Used by `hierarchy
// propose`, which works at the Epic level and has no need for the Issue level.
func loadHierarchyModel(prof *config.Profile) (hierarchy.Model, []hierarchy.Subtask, error) {
	model, _, subtasks, err := loadHierarchyData(prof)
	return model, subtasks, err
}

// loadHierarchyData is loadHierarchyModel plus issues/ — the taxonomy's Issue level
// (Epic -> Issue -> Task). A missing issues/ dir is equally not fatal (hierarchy.
// LoadIssues' own "not seeded yet" case).
func loadHierarchyData(prof *config.Profile) (hierarchy.Model, []hierarchy.Issue, []hierarchy.Subtask, error) {
	storeRoot := prof.Resolve(prof.StoreRoot)
	model, _, err := hierarchy.Parse(filepath.Join(storeRoot, "milestones.yaml"))
	if err != nil {
		return hierarchy.Model{}, nil, nil, fmt.Errorf("parse milestones.yaml: %w", err)
	}
	// LoadSubtasksResolved infers each task's type on the fly (type -> owner -> lane,
	// same rule `queue backfill-type` writes) rather than trusting only the persisted
	// `type:` field — propose's mapping quality must not depend on that migration
	// having been --applied. Mirrors runQueueBackfillType's own path resolution.
	queueRoot, rolesPath := prof.Resolve(prof.QueueRoot), prof.Resolve(prof.RolesPath)
	agentsDir := filepath.Join(prof.Resolve(prof.Team.TeamDir), "agents")
	subtasks, err := hierarchy.LoadSubtasksResolved(storeRoot, queueRoot, rolesPath, agentsDir)
	if err != nil {
		return hierarchy.Model{}, nil, nil, fmt.Errorf("load subtasks: %w", err)
	}
	issues, err := hierarchy.LoadIssues(prof.Resolve(prof.IssuesRoot))
	if err != nil {
		return hierarchy.Model{}, nil, nil, fmt.Errorf("load issues: %w", err)
	}
	return model, issues, subtasks, nil
}

// loadHierarchyRollup is loadHierarchyData + hierarchy.ComputeRollup in one step —
// what `hierarchy show`/`get`/`list`/`tree` all actually need (the full 5-level
// tree, plus the raw subtasks List needs for per-issue task ids).
func loadHierarchyRollup(prof *config.Profile) (hierarchy.ModelRollup, []hierarchy.Subtask, error) {
	model, issues, subtasks, err := loadHierarchyData(prof)
	if err != nil {
		return hierarchy.ModelRollup{}, nil, err
	}
	return hierarchy.ComputeRollup(model, issues, subtasks), subtasks, nil
}

// runHierarchyShow is `leadkit hierarchy show [--json]`.
func runHierarchyShow(args []string) {
	fs := flag.NewFlagSet("hierarchy show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the ModelRollup as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy show: %v\n", err)
		os.Exit(0)
	}

	mr, _, err := loadHierarchyRollup(loadProfileOrDefaults())
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy show: %v\n", err)
		os.Exit(0)
	}

	if *jsonOut {
		data, err := json.Marshal(mr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit hierarchy show: marshal: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(hierarchy.FormatShow(mr))
	os.Exit(0)
}

// runHierarchyPropose is `leadkit hierarchy propose [--json] [--active-only]` —
// READ-ONLY, writes nothing (see internal/hierarchy's own package doc). Default
// covers EVERY unmapped subtask (done/archived included); --active-only narrows to
// non-terminal ones.
// hierarchyHints translates the profile's OPTIONAL config.Hierarchy hint tables into
// the hierarchy package's own Hints shape (which stays free of a config import). An
// unset block yields a zero Hints — "generic model-driven alias/title match only",
// the agnostic default. leadkit source ships no baked-in project epic names.
func hierarchyHints(prof *config.Profile) hierarchy.Hints {
	h := hierarchy.Hints{Type: prof.Hierarchy.TypeHints}
	for _, kh := range prof.Hierarchy.KeywordHints {
		h.Keyword = append(h.Keyword, hierarchy.KeywordHint{Keywords: kh.Keywords, Epic: kh.Epic})
	}
	return h
}

func runHierarchyPropose(args []string) {
	fs := flag.NewFlagSet("hierarchy propose", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the ProposeResult as JSON instead of the human report")
	activeOnly := fs.Bool("active-only", false, "skip terminal-status subtasks (default: propose for all, incl. done/archived)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy propose: %v\n", err)
		os.Exit(0)
	}

	prof := loadProfileOrDefaults()
	model, subtasks, err := loadHierarchyModel(prof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy propose: %v\n", err)
		os.Exit(0)
	}
	res := hierarchy.Propose(model, subtasks, *activeOnly, hierarchyHints(prof))

	if *jsonOut {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit hierarchy propose: marshal: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(hierarchy.FormatProposeResult(res))
	os.Exit(0)
}

// runHierarchyClusterIssues is `leadkit hierarchy cluster-issues [--epic <id>]
// [--min N] [--json]` — READ-ONLY, proposes the Issue level within each Epic (see
// internal/hierarchy's own cluster.go doc comment). Default is the per-epic SUMMARY
// across all ~24 epics; --epic switches to the DETAIL for just that one.
func runHierarchyClusterIssues(args []string) {
	fs := flag.NewFlagSet("hierarchy cluster-issues", flag.ContinueOnError)
	epicFilter := fs.String("epic", "", "detail mode: candidate issues for just this epic")
	minSize := fs.Int("min", 2, "minimum shared-token frequency to form a multi-task cluster (below this: singleton candidates)")
	jsonOut := fs.Bool("json", false, "emit the ClusterResult as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy cluster-issues: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	model, subtasks, err := loadHierarchyModel(prof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy cluster-issues: %v\n", err)
		os.Exit(0)
	}
	res := hierarchy.ClusterIssues(model, subtasks, *epicFilter, *minSize, hierarchyHints(prof))

	if *jsonOut {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit hierarchy cluster-issues: marshal: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(hierarchy.FormatClusterResult(res))
	os.Exit(0)
}

// runHierarchyApply is `leadkit hierarchy apply --plan <file> [--apply --confirm]
// [--epic X]` — materializes a REVIEWED mapping plan (see internal/hierarchyapply's
// own doc comment). Dry-run default; only --apply --confirm writes; a bare --apply
// refuses (confirmGuard, the same guard audit/init/team install/orchestrate apply/
// queue backfill-type all share).
func runHierarchyApply(args []string) {
	fs := flag.NewFlagSet("hierarchy apply", flag.ContinueOnError)
	planPath := fs.String("plan", "", "path to the mapping plan JSON (cluster-issues --json shape, human-reviewed) (required)")
	epicFilter := fs.String("epic", "", "apply only this epic's plan entries")
	apply := fs.Bool("apply", false, "write the plan (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy apply: %v\n", err)
		os.Exit(2)
	}
	if *planPath == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit hierarchy apply --plan <file> [--apply --confirm] [--epic X]")
		os.Exit(2)
	}

	data, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy apply: read plan: %v\n", err)
		os.Exit(1)
	}
	entries, err := hierarchyapply.ParsePlan(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy apply: %v\n", err)
		os.Exit(1)
	}

	prof := loadProfileOrDefaults()
	storeRoot, issuesRoot := prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot)
	plan, err := hierarchyapply.Compute(entries, *epicFilter, storeRoot, issuesRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy apply: %v\n", err)
		os.Exit(1)
	}

	guard := confirmGuard(*apply, *confirm)
	dryRun := guard != guardMutate
	logPath := prof.Resolve(".leadkit/hierarchy-apply.jsonl")

	n, err := hierarchyapply.Apply(plan, storeRoot, issuesRoot, logPath, dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit hierarchy apply: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(hierarchyapply.FormatPlan(plan, dryRun))

	switch guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would write %d item(s); re-run with `--apply --confirm` to execute.\n", n)
		os.Exit(1)
	default:
		os.Exit(0)
	}
}
