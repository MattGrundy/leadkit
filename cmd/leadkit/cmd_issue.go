package main

import (
	"flag"
	"fmt"
	"os"

	"mgpp.dev/leadkit/internal/issuecreate"
)

// runIssue is `leadkit issue <create>`.
func runIssue(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit issue <create> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		runIssueCreate(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit issue: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runIssueCreate is `leadkit issue create --title <t> --epic <epic-id> [--priority
// <p>]` — schema-enforced Issue creation (its own id-space, distinct from Task ids;
// see internal/issuecreate's package doc). NOT confirm-guarded — same normal-op
// class as `task create`.
func runIssueCreate(args []string) {
	fs := flag.NewFlagSet("issue create", flag.ContinueOnError)
	title := fs.String("title", "", "the issue's title (required)")
	epic := fs.String("epic", "", "epic id this issue belongs to (required — must exist in milestones.yaml)")
	priority := fs.String("priority", "", "e.g. P0..P3")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit issue create: %v\n", err)
		os.Exit(2)
	}
	if *title == "" || *epic == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit issue create --title <t> --epic <epic-id> [--priority <p>]")
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	res, err := issuecreate.Create(issuecreate.Options{
		Title: *title, Epic: *epic, Priority: *priority,
		IssuesRoot: prof.Resolve(prof.IssuesRoot), StoreRoot: prof.Resolve(prof.StoreRoot),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit issue create: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("created: %s\n", res.Path)
	fmt.Println(res.ID)
	os.Exit(0)
}
