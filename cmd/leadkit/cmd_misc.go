package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/decisionrec"
	"mgpp.dev/leadkit/internal/gh"
	"mgpp.dev/leadkit/internal/hierarchy"
	"mgpp.dev/leadkit/internal/linearapi"
	"mgpp.dev/leadkit/internal/spikerec"
	"mgpp.dev/leadkit/internal/state"
	syncpkg "mgpp.dev/leadkit/internal/sync"
	"mgpp.dev/leadkit/internal/telemetry"
	"mgpp.dev/leadkit/internal/testrun"
)

// runGet is `leadkit get <id> [--json]` — resolve an id at ANY taxonomy level
// (milestone/epic from milestones.yaml, or an issues/ or tasks/ file) and print its
// own fields + rollup + children. READ-ONLY, uniform — replaces the ad-hoc grep the
// lead/orchestrator used to run over the store directly.
func runGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the Node as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit get: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit get [--json] <id>")
		os.Exit(2)
	}

	mr, subtasks, err := loadHierarchyRollup(loadProfileOrDefaults())
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit get: %v\n", err)
		os.Exit(1)
	}
	node, ok := hierarchy.Get(mr, subtasks, fs.Arg(0))
	if !ok {
		fmt.Fprintf(os.Stderr, "leadkit get: no node found for id %q\n", fs.Arg(0))
		os.Exit(1)
	}

	if *jsonOut {
		data, err := json.Marshal(node)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit get: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(hierarchy.FormatNode(node))
	os.Exit(0)
}

// runList is `leadkit list <milestone|epic|issue|task> [--epic X] [--milestone Y]
// [--status Z] [--json]` — READ-ONLY, uniform; see runGet's own doc comment.
func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	epicFilter := fs.String("epic", "", "filter to this epic id (issue/task level)")
	milestoneFilter := fs.String("milestone", "", "filter to this milestone id")
	statusFilter := fs.String("status", "", "filter to this exact status/state (case-insensitive)")
	jsonOut := fs.Bool("json", false, "emit the Node list as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit list: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit list [--epic X] [--milestone Y] [--status Z] [--json] <milestone|epic|issue|task>")
		os.Exit(2)
	}
	level := fs.Arg(0)
	switch level {
	case "milestone", "epic", "issue", "task":
	default:
		fmt.Fprintf(os.Stderr, "leadkit list: unknown level %q (want milestone|epic|issue|task)\n", level)
		os.Exit(2)
	}

	mr, subtasks, err := loadHierarchyRollup(loadProfileOrDefaults())
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit list: %v\n", err)
		os.Exit(1)
	}
	nodes := hierarchy.List(mr, subtasks, level, *milestoneFilter, *epicFilter, *statusFilter)

	if *jsonOut {
		data, err := json.Marshal(nodes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit list: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(hierarchy.FormatNodeList(nodes))
	os.Exit(0)
}

// runTree is `leadkit tree [--milestone X] [--epic Y]` — hierarchy show's tree view
// scoped to a subtree; READ-ONLY.
func runTree(args []string) {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	milestoneFilter := fs.String("milestone", "", "scope to this milestone id")
	epicFilter := fs.String("epic", "", "scope to this epic id")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit tree: %v\n", err)
		os.Exit(2)
	}

	mr, _, err := loadHierarchyRollup(loadProfileOrDefaults())
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit tree: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(hierarchy.FormatTree(mr, *milestoneFilter, *epicFilter))
	os.Exit(0)
}

// runSpike is `leadkit spike <record|show>`.
func runSpike(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit spike <record|show> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "record":
		runSpikeRecord(args[1:])
	case "show":
		runSpikeShow(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit spike: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runSpikeRecord is `leadkit spike record --ref <sha|pr> --verdict green|red
// [--issue <id>] [--verified <text>] [--link <url>] [--note <text>]` — appends one
// record to spikes_log. NOT confirm-guarded (a normal op, like `task create`); every
// test/dogfood run MUST target a temp log — see internal/spikerec's own doc comment.
func runSpikeRecord(args []string) {
	fs := flag.NewFlagSet("spike record", flag.ContinueOnError)
	ref := fs.String("ref", "", "the sha/PR this spike ran against (required)")
	verdict := fs.String("verdict", "", "green or red (required)")
	issue := fs.String("issue", "", "the Issue/Task id this spike verifies")
	verified := fs.String("verified", "", "what was verified, in prose")
	link := fs.String("link", "", "a URL for the run (Argo UI, PR, ...)")
	note := fs.String("note", "", "free-text note")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit spike record: %v\n", err)
		os.Exit(2)
	}
	if *ref == "" || *verdict == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit spike record --ref <sha|pr> --verdict green|red [--issue <id>] [--verified <text>] [--link <url>] [--note <text>]")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	if err := orphanRuntimeDir(prof, prof.Resolve(prof.SpikesLog)); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit spike record: %v\n", err)
		os.Exit(2)
	}
	if err := spikerec.Append(prof.Resolve(prof.SpikesLog), *ref, *verdict, *issue, *verified, *link, *note); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit spike record: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("recorded")
	os.Exit(0)
}

// runSpikeShow is `leadkit spike show [--issue <id>] [--ref <x>] [--json]` — READ-ONLY.
func runSpikeShow(args []string) {
	fs := flag.NewFlagSet("spike show", flag.ContinueOnError)
	issue := fs.String("issue", "", "filter to this Issue/Task id")
	ref := fs.String("ref", "", "filter to this ref (exact match)")
	jsonOut := fs.Bool("json", false, "emit the records as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit spike show: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	records, err := spikerec.Show(prof.Resolve(prof.SpikesLog), *issue, *ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit spike show: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, err := json.Marshal(records)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit spike show: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(spikerec.FormatRecords(records))
	os.Exit(0)
}

// runDecision is `leadkit decision <record|list>`.
func runDecision(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit decision <record|list> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "record":
		runDecisionRecord(args[1:])
	case "list":
		runDecisionList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit decision: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runDecisionRecord is `leadkit decision record --what <text> --by <name>
// [--authorizes <text>] [--context <text>] [--ref <id|url>] [--scope <text>]` —
// appends one record to decisions_log. NOT confirm-guarded; every test/dogfood run
// MUST target a temp log — see internal/decisionrec's own doc comment.
func runDecisionRecord(args []string) {
	fs := flag.NewFlagSet("decision record", flag.ContinueOnError)
	what := fs.String("what", "", "what was decided (required)")
	by := fs.String("by", "", "who decided it (required)")
	authorizes := fs.String("authorizes", "", "what this decision authorizes going forward")
	context := fs.String("context", "", "why — free text")
	ref := fs.String("ref", "", "an id/URL this decision relates to")
	scope := fs.String("scope", "", "how broadly this decision applies")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit decision record: %v\n", err)
		os.Exit(2)
	}
	if *what == "" || *by == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit decision record --what <text> --by <name> [--authorizes <text>] [--context <text>] [--ref <id|url>] [--scope <text>]")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	if err := orphanRuntimeDir(prof, prof.Resolve(prof.DecisionsLog)); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit decision record: %v\n", err)
		os.Exit(2)
	}
	if err := decisionrec.Append(prof.Resolve(prof.DecisionsLog), *what, *by, *authorizes, *context, *ref, *scope); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit decision record: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("recorded")
	os.Exit(0)
}

// runDecisionList is `leadkit decision list [--by <name>] [--ref <x>] [--since
// <date>] [--json]` — READ-ONLY.
func runDecisionList(args []string) {
	fs := flag.NewFlagSet("decision list", flag.ContinueOnError)
	by := fs.String("by", "", "filter to this author (exact match)")
	ref := fs.String("ref", "", "filter to this ref (exact match)")
	since := fs.String("since", "", "filter to decisions on/after this date (RFC3339 or YYYY-MM-DD)")
	jsonOut := fs.Bool("json", false, "emit the records as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit decision list: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	records, err := decisionrec.List(prof.Resolve(prof.DecisionsLog), *by, *ref, *since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit decision list: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, err := json.Marshal(records)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit decision list: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(decisionrec.FormatRecords(records))
	os.Exit(0)
}

// runSync is `leadkit sync <plan|record-id>` — the pluggable output-sync layer
// (see internal/sync's own package doc). leadkit computes the plan and writes
// back created ids; Linear's ops are applied by an agent (or the opt-in direct
// executor), GitHub's in process through gh.
func runSync(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit sync <plan|run|record-id> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "plan":
		runSyncPlan(args[1:])
	case "record-id":
		runSyncRecordID(args[1:])
	case "run":
		runSyncRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit sync: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// resolveSyncOutput looks up name in sync.Registry, exiting 2 with the known-name
// list on a miss — the same usage-error convention as every other flag validation
// in this CLI.
func resolveSyncOutput(name string) syncpkg.Output {
	out, ok := syncpkg.Registry[name]
	if !ok {
		known := make([]string, 0, len(syncpkg.Registry))
		for k := range syncpkg.Registry {
			known = append(known, k)
		}
		fmt.Fprintf(os.Stderr, "leadkit sync: unknown --output %q (known: %s)\n", name, strings.Join(known, ", "))
		os.Exit(2)
	}
	return out
}

// syncOutputName resolves the --output name for `sync plan`/`sync run`: the flag,
// else the profile's single configured output, exiting 2 (usage) otherwise.
func syncOutputName(cmd string, prof *config.Profile, flagVal string) string {
	name := flagVal
	if name == "" {
		switch len(prof.Outputs) {
		case 0:
			fmt.Fprintf(os.Stderr, "usage: leadkit %s --output <name> [--json] (no --output given, and the profile's outputs: is empty)\n", cmd)
			os.Exit(2)
		case 1:
			name = prof.Outputs[0]
		default:
			fmt.Fprintf(os.Stderr, "leadkit %s: --output is required (the profile configures multiple: %s)\n", cmd, strings.Join(prof.Outputs, ", "))
			os.Exit(2)
		}
	}
	return name
}

// runSyncPlan is `leadkit sync plan --output <name> [--json]` — READ-ONLY: computes
// the canonical tree (hierarchy apply's populated Issue/Task mapping) and emits the
// ordered []Op an agent should execute via that tracker's own MCP.
func runSyncPlan(args []string) {
	fs := flag.NewFlagSet("sync plan", flag.ContinueOnError)
	output := fs.String("output", "", "the Output adapter to plan for, e.g. linear (default: the profile's merged outputs: — see layered config's team-policy key)")
	jsonOut := fs.Bool("json", false, "emit the []Op as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync plan: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	name := syncOutputName("sync plan", prof, *output)
	adapter := resolveSyncOutput(name)

	mr, subtasks, err := loadHierarchyRollup(prof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync plan: %v\n", err)
		os.Exit(1)
	}
	model := syncpkg.BuildModelFor(adapter, mr, subtasks)
	ops, err := adapter.Plan(model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync plan: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, err := json.Marshal(ops)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit sync plan: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(syncpkg.FormatOps(ops))
	os.Exit(0)
}

// runSyncRun is `leadkit sync run --output <name> [--mode mcp|direct]` — the
// EXECUTION step. The plan it executes is exactly `sync plan`'s (mode-agnostic);
// only who applies it differs. Mode: --mode > linear.sync_mode > env default
// (LINEAR_API_KEY present -> direct, else mcp). MCP mode executes nothing itself —
// it points at the sync-executor agent, exactly as before. Direct mode applies
// the plan against the Linear GraphQL API; the key is read at runtime from env
// LINEAR_API_KEY or the keychain and never printed, logged, or stored.
func runSyncRun(args []string) {
	fs := flag.NewFlagSet("sync run", flag.ContinueOnError)
	output := fs.String("output", "", "the Output adapter to execute, e.g. linear (default: the profile's single outputs: entry)")
	modeFlag := fs.String("mode", "", "force the execution mode: mcp or direct (default: linear.sync_mode, else direct iff LINEAR_API_KEY is set)")
	repo := fs.String("repo", "", "github only: the owner/name repo to mirror tickets into (required; refused unless the forge reports it private)")
	apply := fs.Bool("apply", false, "github only: actually create/update issues; without it the run is a dry run that writes nothing")
	withBodies := fs.Bool("with-bodies", false, "github only: ALSO publish ticket bodies (default: title + metadata only; a published body cannot be unpublished)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()
	name := syncOutputName("sync run", prof, *output)
	adapter := resolveSyncOutput(name)

	if name == "github" {
		if *modeFlag != "" {
			fmt.Fprintln(os.Stderr, "leadkit sync run: --mode is a Linear flag; the github output always executes in process through gh")
			os.Exit(2)
		}
		runSyncRunGitHub(prof, adapter, *repo, *apply, *withBodies)
		return
	}
	if *repo != "" || *apply || *withBodies {
		fmt.Fprintf(os.Stderr, "leadkit sync run: --repo, --apply and --with-bodies are github-only flags (output is %q)\n", name)
		os.Exit(2)
	}

	mode, err := linearapi.ResolveMode(*modeFlag, prof.Linear.SyncMode, strings.TrimSpace(os.Getenv(linearapi.EnvKey)) != "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(2)
	}
	if mode == linearapi.ModeMCP {
		fmt.Println("mode=mcp: leadkit does not execute in this mode — wake the sync-executor agent to run `sync plan` and apply it via the Linear MCP (or use --mode direct with LINEAR_API_KEY).")
		os.Exit(0)
	}
	if name != "linear" {
		fmt.Fprintf(os.Stderr, "leadkit sync run: direct mode only supports --output linear (got %q)\n", name)
		os.Exit(2)
	}
	key, err := linearapi.ResolveKey(os.Getenv, linearapi.SecurityKeychain, prof.KeychainService)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}

	mr, subtasks, err := loadHierarchyRollup(prof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}
	model := syncpkg.BuildModelFor(adapter, mr, subtasks)
	ops, err := adapter.Plan(model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}
	storeRoot, issuesRoot := prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot)
	de := &syncpkg.DirectExecutor{
		C: linearapi.New(key, nil),
		Cfg: syncpkg.DirectConfig{
			TeamKey: prof.Linear.TeamID, RootInitiative: prof.Linear.RootInitiative,
			ClosedState: prof.Linear.ClosedState, DefaultState: prof.Linear.DefaultState,
		},
		Record: func(output, local, id string) error {
			return syncpkg.RecordID(storeRoot, issuesRoot, output, local, id)
		},
	}
	res, err := de.Execute(context.Background(), model, ops)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v (so far: %d created, %d updated, %d closed)\n", err, res.Created, res.Updated, res.Closed)
		os.Exit(1)
	}
	fmt.Printf("mode=direct: %d created, %d updated, %d closed\n", res.Created, res.Updated, res.Closed)
	os.Exit(0)
}

// runSyncRunGitHub is `sync run --output github --repo owner/name [--apply] [--with-bodies]`.
// It executes in process through gh (see github_exec.go for why there is no agent half)
// and is a DRY RUN unless --apply is given: the remote is read, nothing is created,
// changed or recorded. Ticket bodies are never read or sent unless --with-bodies.
func runSyncRunGitHub(prof *config.Profile, adapter syncpkg.Output, repo string, apply, withBodies bool) {
	if !syncpkg.ValidRepo(repo) {
		fmt.Fprintln(os.Stderr, "usage: leadkit sync run --output github --repo <owner/name> [--apply] [--with-bodies]")
		os.Exit(2)
	}
	mr, subtasks, err := loadHierarchyRollup(prof)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}
	model := syncpkg.BuildModelFor(adapter, mr, subtasks)
	ops, err := adapter.Plan(model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}
	storeRoot, issuesRoot := prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot)
	ge := &syncpkg.GitHubExecutor{
		Repo: repo, Run: syncpkg.ExecGH, Apply: apply, Pace: time.Second,
		Record: func(output, local, id string) error {
			return syncpkg.RecordID(storeRoot, issuesRoot, output, local, id)
		},
	}
	bodies := "title + metadata only"
	if withBodies {
		bodies = "BODIES INCLUDED"
		ge.Body = func(local string) (string, error) { return syncpkg.TicketBody(storeRoot, issuesRoot, local) }
	}
	mode := "dry run (nothing written; pass --apply)"
	if apply {
		mode = "APPLY"
	}
	fmt.Printf("github %s: %d ticket(s), %s, %s\n\n", repo, len(ops), bodies, mode)
	res, err := ge.Execute(context.Background(), model, ops)
	fmt.Print(syncpkg.FormatGitHubResult(res))
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync run: %v\n", err)
		os.Exit(1)
	}
	if len(res.Problems()) > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

// runSyncRecordID is `leadkit sync record-id --output <name> --local <id>
// --tracker-id <id>` — writes the agent-created tracker id back into that Issue/
// Task's `trackers.<output>` frontmatter, so the next `sync plan` sees it as an
// update, never a duplicate create. NOT confirm-guarded (writes one field on one
// file, same normal-op class as `task create`/`queue assign`); every test/dogfood
// run MUST target a temp store.
func runSyncRecordID(args []string) {
	fs := flag.NewFlagSet("sync record-id", flag.ContinueOnError)
	output := fs.String("output", "", "the Output this id belongs to, e.g. linear (required)")
	local := fs.String("local", "", "the local Issue/Task id (required)")
	trackerID := fs.String("tracker-id", "", "the id the tracker assigned on create (required)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync record-id: %v\n", err)
		os.Exit(2)
	}
	if *output == "" || *local == "" || *trackerID == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit sync record-id --output <name> --local <id> --tracker-id <id>")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	if err := syncpkg.RecordID(prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot), *output, *local, *trackerID); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit sync record-id: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("recorded")
	os.Exit(0)
}

// runTestCmd is `leadkit test <run>`.
func runTestCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit test <run> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		runTestRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit test: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runTestRun is `leadkit test run --repo <clone> --sha <sha> [--affected]
// [--json]` — see internal/testrun's own package doc. Read-only w.r.t. the task
// store; exit code mirrors the verdict (0 green, non-zero red) so a calling
// agent/script can branch on it directly.
func runTestRun(args []string) {
	fs := flag.NewFlagSet("test run", flag.ContinueOnError)
	repo := fs.String("repo", "", "path to the repo clone (required)")
	sha := fs.String("sha", "", "the committed sha to test (required) — refuses if HEAD doesn't match")
	affected := fs.Bool("affected", true, "try the affected-tests subset first; falls back to the full suite when no mechanism is available")
	jsonOut := fs.Bool("json", false, "emit {sha, verdict, failing} as JSON instead of the human report")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit test run: %v\n", err)
		os.Exit(2)
	}
	if *repo == "" || *sha == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit test run --repo <clone> --sha <sha> [--affected] [--json]")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	testCmd, affectedCmd := resolveTestOverrides(prof, *repo)

	v, err := testrun.Run(testrun.Options{
		Repo: *repo, SHA: *sha, Affected: *affected,
		TestCommand: testCmd, AffectedCommand: affectedCmd,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit test run: %v\n", err)
		os.Exit(2)
	}

	if *jsonOut {
		data, err := json.Marshal(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit test run: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		fmt.Print(testrun.FormatVerdict(v))
	}

	if v.Verdict == "green" {
		os.Exit(0)
	}
	os.Exit(1)
}

// resolveTestOverrides resolves repo's Profile.Tests override, if any — keyed the
// same way Authority is: a repo's origin remote URL contains the map key as a
// substring. No match (or no remote) means no override — internal/testrun's own
// inference takes over.
func resolveTestOverrides(prof *config.Profile, repo string) (testCommand, affectedCommand string) {
	remote := gitOriginRemote(repo)
	if remote == "" {
		return "", ""
	}
	for key, spec := range prof.Tests {
		if key != "" && strings.Contains(remote, key) {
			return spec.TestCommand, spec.AffectedCommand
		}
	}
	return "", ""
}

// runGoal dispatches the goal subcommand group (show|ensure).
func runGoal(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit goal <show|ensure>")
		os.Exit(2)
	}
	prof, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit goal: %v\n", err)
		os.Exit(1)
	}
	path := prof.Resolve(prof.GoalFile)

	switch args[0] {
	case "show":
		g, err := state.LoadGoal(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("no goal file yet at %s — run `leadkit goal ensure`\n", path)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "leadkit goal show: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(state.FormatGoal(g))
	case "ensure":
		if err := orphanRuntimeDir(prof, path); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit goal ensure: %v\n", err)
			os.Exit(1)
		}
		_, created, err := state.EnsureGoal(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit goal ensure: %v\n", err)
			os.Exit(1)
		}
		if created {
			fmt.Printf("wrote a new goal stub at %s — edit it to set the real north star\n", path)
		} else {
			fmt.Printf("goal already exists at %s (untouched)\n", path)
		}
	default:
		fmt.Fprintf(os.Stderr, "leadkit goal: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runState dispatches the state subcommand group (show) — the Handover file.
func runState(args []string) {
	if len(args) < 1 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: leadkit state show")
		os.Exit(2)
	}
	prof, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit state: %v\n", err)
		os.Exit(1)
	}
	path := prof.Resolve(prof.HandoverFile)

	h, err := state.LoadHandover(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("no handover file yet at %s\n", path)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "leadkit state show: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(state.FormatHandoverSummary(h))
}

// runMergeMsg is compose-squash-merge-message.sh: print the squash-merge commit
// subject/body for a PR, in the exact wire format the merge wrapper parses.
func runMergeMsg(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: leadkit merge-msg <repo> <pr>")
		os.Exit(2)
	}
	subject, body, err := gh.Compose(args[0], args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit merge-msg: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(gh.FormatMergeMessage(subject, body))
}

// runSkipCI is check-commit-skip-ci.sh: exit 1 + a stderr warning when sha's commit
// message carries "[skip ci]", exit 0 silent when clean.
func runSkipCI(args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: leadkit skip-ci <repo> <sha>")
		os.Exit(2)
	}
	repo, sha := args[0], args[1]
	found, err := gh.HasSkipCI(repo, sha)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit skip-ci: %v\n", err)
		os.Exit(1)
	}
	if found {
		fmt.Fprintln(os.Stderr, gh.FormatSkipCIMessage(sha))
		os.Exit(1)
	}
	os.Exit(0)
}

// runRate is burn-rate.sh: report local tool-call telemetry volume over a trailing
// window. Always exits 0 — a read-only advisory readout, never a gate.
func runRate(args []string) {
	fs := flag.NewFlagSet("rate", flag.ContinueOnError)
	logPath := fs.String("log", telemetry.DefaultLogPath, "path to the tool-usage.jsonl telemetry log")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit rate: %v\n", err)
		os.Exit(0)
	}

	windowMinutes := 60
	if fs.NArg() >= 1 {
		m, err := strconv.Atoi(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit rate: invalid window_minutes %q: %v\n", fs.Arg(0), err)
			os.Exit(0)
		}
		windowMinutes = m
	}

	res, err := telemetry.Rate(*logPath, time.Duration(windowMinutes)*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit rate: %v\n", err)
		os.Exit(0)
	}

	var top strings.Builder
	for _, tc := range res.Top {
		fmt.Fprintf(&top, "%s:%d ", tc.Tool, tc.Count)
	}
	fmt.Printf("BURN(%dm): %d ops (~%d/hr) · %d agent-spawns (~%d/hr) · %d bash · top[ %s]\n",
		res.WindowMinutes, res.Total, res.PerHr, res.Agents, res.AgentsHr, res.Bash, top.String())

	if res.PerHr > 400 {
		fmt.Printf("BURN: ⚠ op-rate ~%d/hr high — check for a loop/spawn-storm\n", res.PerHr)
	}
	if res.AgentsHr > 40 {
		fmt.Printf("BURN: ⚠ agent-spawn ~%d/hr high — check fan-out\n", res.AgentsHr)
	}

	os.Exit(0)
}
