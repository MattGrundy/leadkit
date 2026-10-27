package main

import (
	"flag"
	"fmt"
	"os"

	"mgpp.dev/leadkit/internal/review"
)

// runReview dispatches the review subcommand group (send), the Go equivalent of
// send-to-review.sh.
func runReview(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit review <send> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "send":
		runReviewSend(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit review: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runReviewSend is `send-to-review`: move a ticket into the code-review reviewer's
// pending inbox. Exits 1 on refusal (nothing moved), 0 on success.
func runReviewSend(args []string) {
	fs := flag.NewFlagSet("review send", flag.ContinueOnError)
	task := fs.String("task", "", "ticket: absolute path, or a bare name/name.md under tasks/")
	repo := fs.String("repo", "", "clone (absolute path) to gate preconditions against")
	owner := fs.String("owner", "", "owner to set on the ticket frontmatter (optional)")
	dryRun := fs.Bool("dry-run", false, "print the plan (reviewer, frontmatter changes, stale entries, target symlink) and mutate nothing")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit review send: %v\n", err)
		os.Exit(2)
	}
	if *task == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit review send --task <t> --repo <clone-abs> [--owner <name>] [--dry-run]")
		os.Exit(2)
	}
	queueRoot, storeRoot := resolveQueueAndStore(loadProfileOrDefaults())
	rv := &review.Reviewer{QueueRoot: queueRoot, TasksDir: storeRoot}
	res, err := rv.SendToReview(*task, *repo, *owner, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !*dryRun {
		fmt.Printf("send-to-review: %s -> %s's pending (sha=%s)\n", res.Base, res.Reviewer, res.SHA)
	}
}
