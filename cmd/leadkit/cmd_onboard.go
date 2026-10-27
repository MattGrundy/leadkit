package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/doctor"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/onboard"
	"mgpp.dev/leadkit/internal/usage"
)

// runOnboard is `leadkit onboard [--check [--json]]` — the self-serve adoption
// engine (docs/build-tasks/adoption-onboard.md). No flags: prints
// docs/ADOPTING-LEADKIT.md's content verbatim (embedded — see internal/onboard),
// so an adopter's binary needs no repo checkout. --check: a project-tailored,
// read-only state probe (the leadkit-adopt skill's --json contract) — REUSES
// config.Load, install.ResolveProjectSlug, and doctor.Run rather than
// reimplementing any of their checks.
func runOnboard(args []string) {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	check := fs.Bool("check", false, "print a project-tailored state probe instead of the guide")
	jsonOut := fs.Bool("json", false, "with --check, emit the CheckResult as JSON (the leadkit-adopt skill's contract)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit onboard: %v\n", err)
		os.Exit(2)
	}

	if !*check {
		fmt.Print(onboard.GuideMD)
		os.Exit(0)
	}

	prof, profPath, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit onboard: %v\n", err)
		d := config.Defaults()
		prof, profPath = &d, ""
	}
	slug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))

	regHost, regSession, _ := registryIdentity(prof)
	report := doctor.Run(prof, profPath, Version, standardSettingsPaths(), defaultLaunchAgentsDir(), usage.DefaultWorkstreamsDir, doctor.RealGit(), usage.DefaultPriorityCeilingPath, usage.DefaultProjectsDir, usage.DefaultPresenceDir, usage.DefaultRoutingDir, prof.Resolve(prof.RegistryRepo), regHost, regSession, defaultTranscriptsDir())
	doctorOK := report.AllCriticalPass()
	doctorSummary := "all critical checks pass"
	if !doctorOK {
		critFail := 0
		for _, c := range report.Checks {
			if !c.OK && c.Severity == doctor.SeverityCritical {
				critFail++
			}
		}
		doctorSummary = fmt.Sprintf("%d critical check(s) failing", critFail)
	}

	result := onboard.BuildCheckResult(profPath != "", profPath, slug, slug != "", doctorOK, doctorSummary)

	if *jsonOut {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit onboard: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(0)
	}
	fmt.Print(onboard.FormatCheck(result))
	os.Exit(0)
}
