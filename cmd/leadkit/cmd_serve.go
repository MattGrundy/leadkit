package main

import (
	"flag"
	"fmt"
	"os"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/roles"
	"mgpp.dev/leadkit/internal/usage"
	"mgpp.dev/leadkit/internal/viewer"
)

// serveAccountMode decides serve's mode from its parsed flags
// (docs/build-tasks/multi-project-board.md). An explicit -queue is the
// strongest, most specific signal and is what the hard constraint's
// byte-identical guarantee is scoped to: single-project mode is exactly
// today's behavior — and this function is the ONLY thing standing between a
// bare `leadkit serve` and single-project mode, so a caller auditing "does
// -queue ever fall through to account mode" only has to read this one
// function, not runServe's whole body. -account/-all can still force account
// mode even alongside -queue (an explicit ask beats the default path either
// way); with neither queue flag given, serve defaults to ACCOUNT mode per the
// spec ("no -queue ... => ACCOUNT mode").
func serveAccountMode(queue string, account, all bool) bool {
	if account || all {
		return true
	}
	return queue == ""
}

// runServe starts the read-only local task-queue viewer, including /health (the
// audit dash — audit-2). Flags override the profile; unset ones default from it.
//
// Two modes (multi-project-board.md): SINGLE-project (an explicit -queue,
// unless -account/-all also forces account mode) renders exactly what serve
// has always rendered — same Server, same routes, and critically NEVER reads
// or writes the account-wide project registry (internal/usage's
// ProjectEntry/ReadProjectRegistry/WriteProjectEntry are not referenced
// anywhere on this path). ACCOUNT mode (no -queue, or an explicit
// -account/-all) reads that registry and renders the project-switcher +
// every registered project's own board/tree/store/health via
// viewer.AccountServer — a router around the exact same per-project Server
// type, not a reimplementation.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	queueFlag := fs.String("queue", "", "path to the task queue overlay root (default: the profile's queue_root; omit for ACCOUNT mode)")
	storeFlag := fs.String("store", "", "path to the canonical flat tasks/ store (default: the profile's store_root, if it exists)")
	issuesFlag := fs.String("issues", "", "path to the issues/ dir, taxonomy's Issue level (default: the profile's issues_root, if it exists)")
	addr := fs.String("addr", "127.0.0.1:7777", "listen address host:port")
	accountFlag := fs.Bool("account", false, "ACCOUNT mode: aggregate every registered project instead of one (docs/build-tasks/multi-project-board.md)")
	allFlag := fs.Bool("all", false, "alias for -account")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "account-wide project registry ACCOUNT mode reads (single-project mode never touches this, regardless of this flag)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit serve: %v\n", err)
		os.Exit(2)
	}

	if serveAccountMode(*queueFlag, *accountFlag, *allFlag) {
		runServeAccount(*projectsDir, *addr)
		return
	}

	prof, _, err := config.Load()
	if err != nil {
		d := config.Defaults()
		prof = &d
	}

	queue := *queueFlag
	if queue == "" {
		queue = prof.Resolve(prof.QueueRoot)
	}
	store := *storeFlag
	if store == "" {
		if cand := prof.Resolve(prof.StoreRoot); cand != "" {
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				store = cand
			}
		}
	}

	issues := *issuesFlag
	if issues == "" {
		if cand := prof.Resolve(prof.IssuesRoot); cand != "" {
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				issues = cand
			}
		}
	}

	var rosterTypes []string
	for _, r := range prof.Roster {
		if r.Type != "" {
			rosterTypes = append(rosterTypes, r.Type)
		}
	}

	s := &viewer.Server{
		Root: queue, Store: store,
		SessionTasksRoot:  prof.Resolve(prof.SessionTasksRoot),
		CanonicalStatuses: prof.CanonicalStatuses,
		WorkishThreshold:  prof.SessionTaskWorkish.DescriptionLengthThreshold,
		WorkishMarkers:    prof.SessionTaskWorkish.Markers,
		PushViolationsLog: prof.Resolve(prof.PushViolationsLog),
		IssuesRoot:        issues,
		// KnownRoles (role-keyed-lanes.md): folds a legacy/instance queue lane's
		// board chip to its canonical role, same resolver send/verdict/assign use.
		KnownRoles: roles.KnownRoles(rosterTypes),
	}
	// Board RENDER surface (self-surfacing-diagnostics.md, Part 3): /health renders the
	// sweep's findings — wired only when diagnostics.surface_board is on. Off leaves the
	// field "" so the section renders nothing (the config-driven toggle).
	if prof.Diagnostics.SurfaceBoard {
		s.SweepStatusFile = prof.Resolve(prof.SweepStatusFile)
	}
	if err := viewer.ListenAndServe(s, *addr); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit serve: %v\n", err)
		os.Exit(1)
	}
}

// runServeAccount starts the ACCOUNT-mode board (multi-project-board.md) —
// projectsDir is read-only here; ACCOUNT mode never writes to the registry
// either (the board is read-only, full stop — burn/init are what register a
// project's roots).
func runServeAccount(projectsDir, addr string) {
	a := &viewer.AccountServer{ProjectsDir: projectsDir}
	if err := viewer.ListenAndServeAccount(a, addr); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit serve: %v\n", err)
		os.Exit(1)
	}
}
