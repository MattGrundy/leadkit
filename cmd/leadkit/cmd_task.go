package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mgpp.dev/leadkit/internal/review"
	"mgpp.dev/leadkit/internal/taskcreate"
)

// runTask is `leadkit task <create>`.
func runTask(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit task <create|note> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		runTaskCreate(args[1:])
	case "note":
		runTaskNote(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit task: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runTaskCreate is `leadkit task create` — the single sanctioned task-creation
// primitive. Schema-enforced: --type AND --issue are both required (an Issue is the
// taxonomy's Task parent — Epic -> Issue -> Task), so a future task is born typed +
// parented and the type/parent backfills stay one-time. --unparented is the explicit
// escape hatch for the rare orphan-task edge case. NOT confirm-guarded: creating one
// task is a normal workflow op (like `queue move`/`queue assign`), not a bulk
// mutation. Exits 2 on a validation refusal (bad --type, missing/dangling --issue,
// dangling --milestone), matching the rest of this CLI's usage-error convention;
// prints the created id LAST, alone, so a script/agent can capture it with something
// like `... | tail -1`.
func runTaskCreate(args []string) {
	fs := flag.NewFlagSet("task create", flag.ContinueOnError)
	title := fs.String("title", "", "the task's subject/title (required)")
	typ := fs.String("type", "", "canonical agent type this task needs (required — one of the agent-definition basenames in this project's team dir, or the binary's embedded set when it has none)")
	issue := fs.String("issue", "", "this task's Issue parent id (required unless --unparented)")
	unparented := fs.Bool("unparented", false, "explicit escape hatch: allow creation with no --issue (edge cases only)")
	milestone := fs.String("milestone", "", "milestone id (validated only once milestones.yaml has milestones)")
	priority := fs.String("priority", "", "e.g. P0..P3")
	owner := fs.String("owner", "", "owner to set (optional — most tasks start unowned)")
	assign := fs.String("assign", "", "role name to symlink the new task into (via queue.Assign)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit task create: %v\n", err)
		os.Exit(2)
	}
	if *title == "" || *typ == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit task create --title <t> --type <canonical> (--issue <id> | --unparented) [--milestone <id>] [--priority <p>] [--owner <name>] [--assign <role>]")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	res, err := taskcreate.Create(taskcreate.Options{
		Title: *title, Type: *typ, Parent: *issue, Unparented: *unparented, Milestone: *milestone,
		Priority: *priority, Owner: *owner, Assign: *assign, RosterTypes: rosterTypes(prof),
		StoreRoot: prof.Resolve(prof.StoreRoot), QueueRoot: prof.Resolve(prof.QueueRoot),
		RolesPath: prof.Resolve(prof.RolesPath), AgentsDir: filepath.Join(prof.Resolve(prof.Team.TeamDir), "agents"),
		IssuesRoot: prof.Resolve(prof.IssuesRoot),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit task create: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("created: %s\n", res.Path)
	if res.Lane != "" {
		fmt.Printf("assigned: %s\n", res.Lane)
	}
	fmt.Println(res.ID)
	os.Exit(0)
}

// runTaskNote is `leadkit task note`: the sanctioned way to put a review note in a ticket. /tasks/
// is gitignored, so the ticket file IS the durable record of a review, and a note appended with a
// shell redirect to a lane path the ticket has since been routed away from is silently lost.
// This resolves the canonical tasks/ file, appends under the ticket lock, and fails loudly.
func runTaskNote(args []string) {
	fs := flag.NewFlagSet("task note", flag.ContinueOnError)
	task := fs.String("task", "", "the ticket: its lane path, tasks/ path, or bare name (required)")
	by := fs.String("by", "", "who is writing the note, e.g. your role (required)")
	text := fs.String("text", "", "the note (default: read stdin)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit task note: %v\n", err)
		os.Exit(2)
	}
	if *task == "" || *by == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit task note --task <ticket> --by <who> [--text <note> | <stdin]")
		os.Exit(2)
	}
	body := *text
	if body == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit task note: read stdin: %v\n", err)
			os.Exit(2)
		}
		body = string(data)
	}
	prof := loadProfileOrDefaults()
	queueRoot, storeRoot := resolveQueueAndStore(prof)
	rv := &review.Reviewer{QueueRoot: queueRoot, TasksDir: storeRoot}
	res, err := rv.AppendNote(*task, *by, body, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	fmt.Printf("noted: %s (+%d bytes)\n", res.Path, res.Bytes)
}
