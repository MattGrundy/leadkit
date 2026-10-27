package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/install"
	"mgpp.dev/leadkit/internal/onboard"
	"mgpp.dev/leadkit/internal/usage"
)

// runInit is `leadkit init [--apply --confirm] [--settings <path>] [--launch-agents-dir
// <dir>]`: the installer — hooks (settings.json) AND, on macOS, the two OS-level
// LaunchAgents (waker + periodic sweep; see internal/install/daemon.go). DRY-RUN IS
// THE DEFAULT — it computes and prints the FULL plan (both parts), writes nothing,
// and exits 0. Only --apply --confirm together perform the write (see
// internal/install's package docs for the safety model: additive-only, idempotent,
// atomic write + timestamped backup — same shape for both parts).
//
// NOT THIS: the SESSION-local heartbeat (CronCreate, re-armed each session by the
// SessionStart skill) is a completely different mechanism, owned by that skill —
// the sweep LaunchAgent below is the OS-level, persists-across-reboots equivalent,
// not a replacement for it.
//
// --settings/--launch-agents-dir override the targets — mainly so a dry-run (or even
// --apply --confirm) can be sampled against temp copies rather than the live
// settings.json / ~/Library/LaunchAgents; they default to the real locations.
//
// --skip-launchctl is a testing-only escape hatch (see internal/install.ApplyDaemonPlan's
// doc comment): a subprocess test execs a SEPARATE binary, so it can't inject the
// in-process runLaunchctl stub — this flag writes the plist(s) but never calls
// launchctl at all. A real invocation should never need it.
func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write settings.json + install the daemons (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	settingsPath := fs.String("settings", userSettingsPath(), "path to settings.json to target (default: the real user-scope settings.json)")
	launchAgentsDir := fs.String("launch-agents-dir", defaultLaunchAgentsDir(), "directory for the launchd plists (default: ~/Library/LaunchAgents)")
	skillsDir := fs.String("skills-dir", defaultSkillsDir(), "parent dir the leadkit-adopt skill is installed under (default: ~/.claude/skills)")
	skipLaunchctl := fs.Bool("skip-launchctl", false, "write daemon plists but never call launchctl — testing escape hatch only; real usage should never need this")
	daemonsOnly := fs.Bool("daemons-only", false, "install ONLY this project's waker+sweep launchd agents (with --confirm) — does NOT touch settings.json or skills; idempotent; a queue-less/gate-only project installs nothing")
	projectsDir := fs.String("projects-dir", usage.DefaultProjectsDir, "account-wide dir this project's queue/store/issues roots are registered into for the multi-project board (docs/build-tasks/multi-project-board.md)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit init: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()

	// --daemons-only is the narrow, idempotent per-project daemon install adoption runs
	// (docs/build-tasks/adopt-installs-daemons.md): it installs ONLY this project's
	// waker+sweep LaunchAgents and never touches settings.json or the skills, so it's a
	// safe, repeatable step that doesn't rewrite shared user-scope state. It reuses the
	// SAME slug-namespaced plan `--apply` builds. Diverges here (before any settings read
	// or hooks/skill/burn planning) so none of those surfaces is even computed.
	if *daemonsOnly {
		runInitDaemonsOnly(prof, install.ResolveBinaryPath(), *launchAgentsDir, *confirm, *skipLaunchctl)
		return
	}

	existing, err := os.ReadFile(*settingsPath)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "leadkit init: read %s: %v\n", *settingsPath, err)
		os.Exit(1)
	}
	// The absolute binary path — NOT a bare "leadkit" — is what every hook entry's
	// command is built from (docs/build-tasks/init-hook-idempotency.md): PreToolUse
	// hooks run under /bin/sh with a minimal PATH that excludes ~/.local/bin, so a
	// bare command fails command-not-found (the push-guard incident). Reuses the same
	// resolution the daemon plan already relies on for the identical reason (see
	// ResolveBinaryPath's own doc comment) — one PATH-independent binary path for both.
	binaryPath := install.ResolveBinaryPath()
	hooksPlan, err := install.ComputePlan(existing, binaryPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit init: %v\n", err)
		os.Exit(1)
	}

	daemonPlan := install.ComputeDaemonPlan(
		runtime.GOOS, binaryPath,
		prof.Resolve(prof.QueueRoot), prof.Resolve(".leadkit"), prof.Heartbeat.Schedule,
		*launchAgentsDir, prof.ProjectID,
	)

	// The ACCOUNT-LEVEL burn-enforce daemon (docs/build-tasks/burn-auto-enforce.md):
	// account-scoped, so it's a SEPARATE, un-slugged plan (dev.leadkit.burn) — not tied
	// to this project's queue, and every project's init writes the same idempotent
	// plist. It reads real account weekly usage each tick and writes/clears the account
	// pause lock, so the throttle fires without any session running `leadkit burn`. Its
	// cwd + logs live in the account leadkit dir, not this project's.
	accountLeadkitDir := filepath.Join(os.Getenv("HOME"), ".claude", "leadkit")
	burnDaemonPlan := install.ComputeBurnDaemonPlan(
		runtime.GOOS, binaryPath,
		accountLeadkitDir, accountLeadkitDir, *launchAgentsDir,
		int(prof.Burn.EnforceIntervalDur().Seconds()),
	)

	// The leadkit-adopt skill (adoption-onboard deliverable 3) — installed from
	// its EMBEDDED content (internal/onboard.SkillMD), same guard as the hooks/
	// daemon plans above. A machine-wide write, CHECKPOINTED like the rest of
	// init — see ONBOARDING's deployment sequence.
	skillPlan := install.ComputeSkillPlan(*skillsDir, onboard.SkillName, map[string][]byte{"SKILL.md": []byte(onboard.SkillMD)})

	fmt.Printf("target: %s\n", *settingsPath)
	fmt.Print(install.FormatPlan(hooksPlan))
	fmt.Printf("launch agents dir: %s\n", *launchAgentsDir)
	fmt.Print(install.FormatDaemonPlan(daemonPlan))
	if w := daemonPlan.SkippedWarning(); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	fmt.Print(install.FormatDaemonPlan(burnDaemonPlan))
	fmt.Printf("skills dir: %s\n", *skillsDir)
	fmt.Print(install.FormatSkillPlan(skillPlan))

	// The review-gate records directory. doctor reports it CRITICAL when absent, and nothing else
	// creates it before the first verdict is recorded, so a fresh machine could never reach a green
	// doctor. Part of init because the gate hooks init installs are what read and write it.
	recordsDir := prof.Resolve(prof.RecordsDir)
	recordsMissing := false
	if fi, err := os.Stat(recordsDir); err != nil || !fi.IsDir() {
		recordsMissing = true
		fmt.Printf("records dir: %s (will create)\n", recordsDir)
	} else {
		fmt.Printf("records dir: %s (exists)\n", recordsDir)
	}

	// The git pre-push hook: the enforcement layer for a raw `git push` (ticket 171). It is
	// per clone, so it is installed into the repo init runs in, and only there.
	var prePush install.PrePushStatus
	if wd, err := os.Getwd(); err == nil {
		prePush = install.InspectPrePush(wd, binaryPath)
	} else {
		prePush = install.PrePushStatus{State: install.PrePushNoRepo, Detail: err.Error()}
	}
	fmt.Print(install.FormatPrePush(prePush))
	prePushChanged := false
	switch prePush.State {
	case install.PrePushMissing, install.PrePushStale, install.PrePushNotExecutable:
		prePushChanged = true
	}

	changedCount := len(hooksPlan.Additions) + len(hooksPlan.Repairs)
	if prePushChanged {
		changedCount++
	}
	if recordsMissing {
		changedCount++
	}
	for _, s := range daemonPlan.Services {
		if s.Changed {
			changedCount++
		}
	}
	for _, s := range burnDaemonPlan.Services {
		if s.Changed {
			changedCount++
		}
	}
	for _, f := range skillPlan.Files {
		if f.Changed {
			changedCount++
		}
	}

	switch guard := confirmGuard(*apply, *confirm); guard {
	case guardDryRun:
		fmt.Println("(dry-run — nothing written; re-run with --apply --confirm)")
		os.Exit(0)
	case guardRefused:
		fmt.Printf("REFUSED — --apply without --confirm mutates nothing. This would change %d item(s); re-run with `--apply --confirm` to execute.\n", changedCount)
		os.Exit(1)
	}

	// guardMutate from here down.
	if recordsMissing {
		if err := os.MkdirAll(recordsDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: create records dir %s: %v\n", recordsDir, err)
			os.Exit(1)
		}
		fmt.Printf("created records dir %s\n", recordsDir)
	}
	if hooksPlan.Changed {
		backup, err := install.Apply(*settingsPath, hooksPlan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: %v\n", err)
			os.Exit(1)
		}
		if backup != "" {
			fmt.Printf("wrote %s (backup: %s)\n", *settingsPath, backup)
		} else {
			fmt.Printf("wrote %s (no prior file — no backup needed)\n", *settingsPath)
		}
	} else {
		fmt.Println("hooks: nothing to apply — every leadkit hook is already wired")
	}

	if !daemonPlan.Supported {
		fmt.Println(daemonPlan.SkipNote)
	} else {
		results, err := install.ApplyDaemonPlan(daemonPlan, *launchAgentsDir, !*skipLaunchctl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: daemon: %v\n", err)
			os.Exit(1)
		}
		for _, r := range results {
			switch {
			case r.Skipped:
				fmt.Printf("daemon: %s already installed, unchanged — left alone\n", r.Label)
			case r.LoadErr != nil:
				fmt.Fprintf(os.Stderr, "daemon: %s: wrote plist but load failed: %v\n", r.Label, r.LoadErr)
			default:
				note := ""
				if r.BackupPath != "" {
					note = fmt.Sprintf(" (backup: %s)", r.BackupPath)
				}
				fmt.Printf("daemon: %s loaded via `launchctl %s`%s\n", r.Label, r.LoadMethod, note)
			}
		}
	}

	if !burnDaemonPlan.Supported {
		fmt.Println(burnDaemonPlan.SkipNote)
	} else {
		results, err := install.ApplyDaemonPlan(burnDaemonPlan, *launchAgentsDir, !*skipLaunchctl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: burn daemon: %v\n", err)
			os.Exit(1)
		}
		for _, r := range results {
			switch {
			case r.Skipped:
				fmt.Printf("daemon: %s already installed, unchanged — left alone\n", r.Label)
			case r.LoadErr != nil:
				fmt.Fprintf(os.Stderr, "daemon: %s: wrote plist but load failed: %v\n", r.Label, r.LoadErr)
			default:
				note := ""
				if r.BackupPath != "" {
					note = fmt.Sprintf(" (backup: %s)", r.BackupPath)
				}
				fmt.Printf("daemon: %s (account burn-enforce throttle) loaded via `launchctl %s`%s\n", r.Label, r.LoadMethod, note)
			}
		}
	}

	if prePushChanged {
		if _, err := install.ApplyPrePush(prePush); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("pre-push hook: wrote %s\n", prePush.HookPath)
	}

	if skillPlan.AnyChanged() {
		skillRes, err := install.ApplySkillPlan(skillPlan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit init: skill: %v\n", err)
			os.Exit(1)
		}
		for _, name := range skillRes.WrittenFiles {
			note := ""
			if backup, ok := skillRes.BackedUpTo[name]; ok {
				note = fmt.Sprintf(" (backup: %s)", backup)
			}
			fmt.Printf("skill: wrote %s%s\n", filepath.Join(skillPlan.Dir, name), note)
		}
	} else {
		fmt.Println("skill: nothing to apply — leadkit-adopt already matches")
	}

	// Multi-project board (multi-project-board.md): register this project's
	// roots + a display name under its daemon slug (install.ResolveProjectSlug —
	// the SAME key ComputeDaemonPlan just namespaced the daemons under above),
	// so `leadkit serve` in ACCOUNT mode can discover and render this project
	// without it being open. Best-effort — a write failure here never fails
	// init itself, it's the same advisory posture `burn`'s own write already has.
	slug := install.ResolveProjectSlug(prof.ProjectID, prof.Resolve(prof.QueueRoot))
	projectName := prof.ProjectID
	if projectName == "" {
		projectName = filepath.Base(filepath.Dir(prof.Resolve(prof.QueueRoot)))
	}
	if err := usage.WriteProjectEntry(*projectsDir, slug, projectName, prof.Resolve(prof.QueueRoot), prof.Resolve(prof.StoreRoot), prof.Resolve(prof.IssuesRoot), time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit init: could not write project registry entry: %v\n", err)
	} else if slug != "" {
		fmt.Printf("project registry: registered %q (slug %s) in %s\n", projectName, slug, *projectsDir)
	}
	// Last, so it is the thing the adopter reads last rather than a line scrolled past.
	if w := daemonPlan.SkippedWarning(); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
}

// runInitDaemonsOnly is `leadkit init --daemons-only [--confirm]`: the narrow,
// idempotent per-project daemon install adoption runs (docs/build-tasks/
// adopt-installs-daemons.md). It installs ONLY this project's waker+sweep LaunchAgents
// and deliberately NEVER touches settings.json, the leadkit-adopt skill, or the
// account-level burn daemon — so it is safe to run repeatedly (adoption + backfill both
// re-run it) without rewriting shared user-scope state. It reuses the SAME slug-namespaced
// install.ComputeDaemonPlan/ApplyDaemonPlan the full `--apply` path uses (no forked
// template), so the plists it writes are byte-identical to what `--apply` would produce.
//
// Guard: dry-run is the default (prints the plan, writes nothing); --confirm performs the
// write. --skip-launchctl is honored (writes the plists but never calls launchctl — the
// same testing escape hatch --apply exposes).
//
// A queue-LESS / gate-only project (queue_root doesn't exist — the external-config
// gate-only case) installs NOTHING and exits 0: a waker with no lanes to watch is a dead
// daemon. This mirrors doctor's absent-queue INFO posture rather than erroring.
func runInitDaemonsOnly(prof *config.Profile, binaryPath, launchAgentsDir string, confirm, skipLaunchctl bool) {
	queueRoot := prof.Resolve(prof.QueueRoot)
	if fi, err := os.Stat(queueRoot); err != nil || !fi.IsDir() {
		fmt.Printf("no queue at %s — a gate-only project has no lanes to watch; no waker needed\n", queueRoot)
		os.Exit(0)
	}

	daemonPlan := install.ComputeDaemonPlan(
		runtime.GOOS, binaryPath,
		queueRoot, prof.Resolve(".leadkit"), prof.Heartbeat.Schedule,
		launchAgentsDir, prof.ProjectID,
	)

	fmt.Printf("launch agents dir: %s\n", launchAgentsDir)
	fmt.Print(install.FormatDaemonPlan(daemonPlan))

	if !confirm {
		fmt.Println("(dry-run — nothing written; re-run with --daemons-only --confirm to install)")
		os.Exit(0)
	}

	if !daemonPlan.Supported {
		// Non-darwin (systemd is a TODO) — nothing to write; the SkipNote explains why.
		fmt.Println(daemonPlan.SkipNote)
		os.Exit(0)
	}

	results, err := install.ApplyDaemonPlan(daemonPlan, launchAgentsDir, !skipLaunchctl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit init: daemon: %v\n", err)
		os.Exit(1)
	}
	for _, r := range results {
		switch {
		case r.Skipped:
			fmt.Printf("daemon: %s already installed, unchanged — left alone\n", r.Label)
		case r.LoadErr != nil:
			fmt.Fprintf(os.Stderr, "daemon: %s: wrote plist but load failed: %v\n", r.Label, r.LoadErr)
		case r.LoadMethod == "skipped":
			note := ""
			if r.BackupPath != "" {
				note = fmt.Sprintf(" (backup: %s)", r.BackupPath)
			}
			fmt.Printf("daemon: %s plist written — launchctl skipped (--skip-launchctl)%s\n", r.Label, note)
		default:
			note := ""
			if r.BackupPath != "" {
				note = fmt.Sprintf(" (backup: %s)", r.BackupPath)
			}
			fmt.Printf("daemon: %s loaded via `launchctl %s`%s\n", r.Label, r.LoadMethod, note)
		}
	}
	os.Exit(0)
}
