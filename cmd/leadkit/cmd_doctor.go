package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/doctor"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/usage"
)

// runDoctor is `leadkit doctor [--json]`: the preflight self-check. Exit 0 iff every
// CRITICAL check passes; non-zero otherwise. Read-only — see internal/doctor's
// package doc.
func runDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit the Report as JSON instead of the ✓/✗ checklist")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit doctor: %v\n", err)
		os.Exit(2)
	}

	prof, profPath, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit doctor: %v\n", err)
		d := config.Defaults()
		prof, profPath = &d, ""
	}

	regHost, regSession, _ := registryIdentity(prof)
	report := doctor.Run(prof, profPath, Version, standardSettingsPaths(), defaultLaunchAgentsDir(), usage.DefaultWorkstreamsDir, doctor.RealGit(), usage.DefaultPriorityCeilingPath, usage.DefaultProjectsDir, usage.DefaultPresenceDir, usage.DefaultRoutingDir, prof.Resolve(prof.RegistryRepo), regHost, regSession, defaultTranscriptsDir())

	// The session-identity check is wired here (not inside doctor.Run) because IDENTITY
	// resolution lives in cmd/leadkit — it reuses the git-toplevel helper. Appended
	// after Run: it is never critical (fail-open WARN/Info), so it can't affect exit
	// code, only surface an env/ancestor shadowing the session's real project.
	idSlug, idSource := sessionIdentitySlug(sessionCwd())
	policySlug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	report.Checks = append(report.Checks, doctor.SessionIdentityCheck(idSlug, idSource, policySlug))

	// The git pre-push hook is per clone, so it is inspected from the session's cwd here.
	report.Checks = append(report.Checks, doctor.PrePushHookCheck(install.InspectPrePush(sessionCwd(), install.ResolveBinaryPath())))

	if *jsonOut {
		data, err := json.Marshal(report)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit doctor: marshal report: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		fmt.Print(doctor.FormatReport(report))
	}

	if !report.AllCriticalPass() {
		os.Exit(1)
	}
	os.Exit(0)
}
