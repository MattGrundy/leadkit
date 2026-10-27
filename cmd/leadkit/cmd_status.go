package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"mgpp.dev/leadkit/internal/milestone"
	"mgpp.dev/leadkit/internal/queue"
)

// runStatus is `leadkit status` — the operator's self-service check-in surface
// (docs/build-tasks/self-surfacing-diagnostics.md, Part 3, the doctor/status PULL). It
// reads the sweep's findings artifact and surfaces stalls/broken-links WITH the
// remediation, so a lead-less adopter sees the problem AND the fix whenever they check
// in (and via SessionStart if they wire it). Read-only, always exit 0 — findings are
// reportable state, not a failure (same health semantics as the sweep). The
// diagnostics.surface_doctor toggle gates the diagnostics block; the milestone
// re-anchor is always shown.
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit status: %v\n", err)
		os.Exit(2)
	}
	prof := loadProfileOrDefaults()

	if prof.Diagnostics.SurfaceDoctor {
		path := prof.Resolve(prof.SweepStatusFile)
		status, present, rerr := queue.ReadSweepStatus(path)
		if rerr != nil {
			// A present-but-unreadable artifact degrades to "no findings" rather than
			// failing the check-in — never a false alarm.
			fmt.Printf("--- daemon diagnostics ---\nsweep findings artifact %s is unreadable (%v) — re-run `leadkit queue sweep`\n", path, rerr)
		} else {
			fmt.Print(queue.FormatStatusSurface(path, status, present, prof.Diagnostics.SelfRepair, time.Hour, time.Now()))
		}
	} else {
		fmt.Print("--- daemon diagnostics ---\nsurfacing disabled (diagnostics.surface_doctor=false)\n")
	}

	fmt.Print("--- milestone re-anchor ---\n")
	fmt.Print(milestone.FormatStatus(loadMilestoneReport(prof)))
	os.Exit(0)
}
