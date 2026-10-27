package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/audit"
	"mgpp.dev/leadkit/internal/config"
)

// userSettingsPath is the user-scope settings.json leadkit init targets — the same
// location Claude Code itself reads first. $HOME missing (never happens in practice)
// degrades to a relative path rather than panicking.
func userSettingsPath() string {
	home := os.Getenv("HOME")
	return filepath.Join(home, ".claude", "settings.json")
}

// defaultLaunchAgentsDir is the real macOS user-scope LaunchAgents directory leadkit
// init targets by default.
func defaultLaunchAgentsDir() string {
	home := os.Getenv("HOME")
	return filepath.Join(home, "Library", "LaunchAgents")
}

// defaultSkillsDir is the real user-scope skills directory `leadkit init` installs
// the leadkit-adopt skill under by default — ~/.claude/skills/<name>, one level up
// from onboard.SkillName's own dir (install.ComputeSkillPlan joins skillName back
// on).
func defaultSkillsDir() string {
	home := os.Getenv("HOME")
	return filepath.Join(home, ".claude", "skills")
}

// standardSettingsPaths are the settings.json locations doctor's hook-wiring check
// reads: user scope, then project scope, then project-local scope — the same
// precedence order Claude Code itself applies them in. A leadkit hook wired in ANY
// of these counts as installed (hookwire.MergeHooks unions them).
func standardSettingsPaths() []string {
	paths := []string{userSettingsPath()}
	if cwd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(cwd, ".claude", "settings.json"))
		paths = append(paths, filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	return paths
}

// runAudit is `leadkit audit`: the read-only audit engine. Always exits 0 — it's a
// report, never a gate.
func runAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	lens := fs.String("lens", "all", "disk|session|all")
	sessionID := fs.String("session", "", "limit the session lens to one session id")
	jsonOut := fs.Bool("json", false, "emit the []Finding as JSON instead of the human report")
	apply := fs.Bool("apply", false, "actually perform the 3 safe fixes below (default: preview only, writes nothing)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually mutate — --apply alone always refuses")
	fixFlag := fs.String("fix", "dangling,noncanonical,done-drift", "comma-separated subset to preview/fix: dangling,noncanonical,done-drift")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit audit: %v\n", err)
		os.Exit(0)
	}

	prof, _, err := config.Load()
	if err != nil {
		d := config.Defaults()
		prof = &d
	}

	// --apply/--fix engage a SEPARATE mode (the safe-cleanup tool) — a bare
	// `leadkit audit` with neither flag is completely unchanged: today's full
	// read-only findings report, exactly as before this task.
	applyModeRequested := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "apply" || f.Name == "fix" || f.Name == "confirm" {
			applyModeRequested = true
		}
	})
	if applyModeRequested {
		runAuditApply(prof, *apply, *confirm, *fixFlag)
		return
	}

	var findings []audit.Finding
	var sessionCounts map[string]audit.SessionCounts

	if *lens == "disk" || *lens == "all" {
		f, err := audit.DiskLens(prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.CanonicalStatuses)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit audit: disk lens: %v\n", err)
		}
		findings = append(findings, f...)
	}
	if *lens == "session" || *lens == "all" {
		f, counts, err := audit.SessionLens(prof.Resolve(prof.SessionTasksRoot), *sessionID, audit.WorkishConfig{
			DescriptionLengthThreshold: prof.SessionTaskWorkish.DescriptionLengthThreshold,
			Markers:                    prof.SessionTaskWorkish.Markers,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit audit: session lens: %v\n", err)
		}
		findings = append(findings, f...)
		sessionCounts = counts
	}

	if *jsonOut {
		data, err := json.Marshal(findings)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit audit: marshal findings: %v\n", err)
			os.Exit(0)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}

	fmt.Print(audit.FormatReport(findings, sessionCounts))
	os.Exit(0)
}

// runAuditApply is `leadkit audit --apply [--confirm] [--fix ...]`: the safe-cleanup
// tool. DRY-RUN IS THE DEFAULT — no --apply just previews. See internal/audit's
// package doc (apply.go) for exactly what the three categories do and don't touch,
// and why a second run after a clean one is a no-op by construction.
//
// CONFIRMATION GUARD (post-incident, see docs/build-tasks/apply-safety-fixes.md):
// mutating requires BOTH doApply AND confirmed. --apply alone (the exact footgun
// that caused the 2026-09-04 incident — a bare Go bool flag parses true with no
// value) computes and prints the FULL plan, same as a real run would, but refuses to
// write anything and exits non-zero so a script/agent notices the no-op.
func runAuditApply(prof *config.Profile, doApply, confirmed bool, fixFlag string) {
	guard := confirmGuard(doApply, confirmed)
	opts := audit.ApplyOptions{
		Aliases:           prof.StatusAliases,
		CanonicalStatuses: prof.CanonicalStatuses,
		DryRun:            guard != guardMutate,
		LogPath:           prof.Resolve(prof.AuditApplyLog),
	}
	for _, cat := range strings.Split(fixFlag, ",") {
		switch strings.TrimSpace(cat) {
		case "dangling":
			opts.Dangling = true
		case "noncanonical":
			opts.Noncanonical = true
		case "done-drift":
			opts.DoneDrift = true
		case "":
			// a trailing/leading comma — ignore
		default:
			fmt.Fprintf(os.Stderr, "leadkit audit --apply: unknown --fix category %q (want dangling,noncanonical,done-drift)\n", cat)
			os.Exit(2)
		}
	}

	res, err := audit.Apply(prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit audit --apply: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(audit.FormatApplyResult(res))

	switch guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", len(res.Fixes))
		os.Exit(1)
	}
	os.Exit(0)
}
