package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"mgpp.dev/leadkit/internal/lock"
)

// runLock is `leadkit lock [status|wait] [--reason <r>]`: dispatches to the status/
// wait subcommands, or — with no subcommand — a manual Set (the lead pausing the team
// by hand, same as burn's automatic throttle lock but set_by="lead").
func runLock(args []string) {
	if len(args) >= 1 && args[0] == "status" {
		runLockStatus(args[1:])
		return
	}
	if len(args) >= 1 && args[0] == "wait" {
		runLockWait(args[1:])
		return
	}

	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	reason := fs.String("reason", "manually locked by the lead", "reason recorded in the lock metadata")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit lock: %v\n", err)
		os.Exit(2)
	}

	prof := loadProfileOrDefaults()
	lockPath := prof.Resolve(prof.Throttle.LockFile)
	if err := orphanRuntimeDir(prof, lockPath); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit lock: %v\n", err)
		os.Exit(1)
	}
	meta := lock.Meta{
		Reason: *reason, SetBy: "lead", PausedAt: time.Now().UTC(),
		PausePct: prof.Throttle.PausePct, ResumePct: prof.Throttle.ResumePct,
	}
	if err := lock.Set(lockPath, meta); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit lock: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("locked: %s\n", lockPath)
}

// runUnlock is `leadkit unlock`: manual Clear.
func runUnlock(args []string) {
	prof := loadProfileOrDefaults()
	lockPath := prof.Resolve(prof.Throttle.LockFile)
	if err := lock.Clear(lockPath); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit unlock: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("unlocked")
}

// runLockStatus is `leadkit lock status`. Reports the per-project lock AND, separately,
// the account-level burn-enforce lock (docs/build-tasks/burn-auto-enforce.md) — a
// session can be paused by either.
func runLockStatus(args []string) {
	prof := loadProfileOrDefaults()
	printLockStatus := func(label, path string) {
		locked, meta, err := lock.Status(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit lock status: %s: %v\n", label, err)
			return
		}
		if !locked {
			fmt.Printf("%s: unlocked (%s)\n", label, path)
			return
		}
		fmt.Printf("%s: locked reason=%q set_by=%s paused_at=%s pause_pct=%.1f resume_pct=%.1f\n",
			label, meta.Reason, meta.SetBy, meta.PausedAt.Format(time.RFC3339), meta.PausePct, meta.ResumePct)
	}
	printLockStatus("project", prof.Resolve(prof.Throttle.LockFile))
	printLockStatus("account", lock.DefaultAccountLockPath)
}

// runLockWait is `leadkit lock wait [timeout-seconds]`: blocks token-free until the
// lock clears. Part of the token-free pause protocol (see internal/lock's package
// doc): an agent that gets LOCKED back from `queue wait` should run this next, then
// loop back to `queue wait` on UNLOCKED.
func runLockWait(args []string) {
	timeout := 3600 * time.Second
	if len(args) >= 1 {
		secs, err := strconv.Atoi(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit lock wait: invalid timeout-seconds %q: %v\n", args[0], err)
			os.Exit(2)
		}
		timeout = time.Duration(secs) * time.Second
	}

	prof := loadProfileOrDefaults()
	lockPath := prof.Resolve(prof.Throttle.LockFile)
	// Wait until BOTH the per-project lock AND the account-level burn-enforce lock are
	// clear — a session paused by either must stay paused until every lock holding it is
	// gone (docs/build-tasks/burn-auto-enforce.md).
	if lock.WaitAll([]string{lockPath, lock.DefaultAccountLockPath}, timeout) {
		fmt.Println("UNLOCKED")
	} else {
		fmt.Println("TIMEOUT: re-arm")
	}
	os.Exit(0)
}
