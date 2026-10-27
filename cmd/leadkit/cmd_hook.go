package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mgpp.dev/leadkit/internal/audit"
	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// runHook is `leadkit hook <name>`: the portable PreToolUse guard hooks. FAILS OPEN
// (silent, exit 0) on a missing name, a stdin read error, or an unrecognized name —
// this is the process boundary around internal/hook's own fail-open contract.
func runHook(args []string) {
	if len(args) < 1 {
		os.Exit(0)
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Println("usage: leadkit hook <name>")
		fmt.Println("names:")
		for _, n := range hook.Names {
			fmt.Printf("  %s\n", n)
		}
		os.Exit(0)
	}

	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}

	// Fix 3: install this invocation's resolution base from the PreToolUse payload's own
	// `cwd` BEFORE building the per-target resolvers, so a truly-bare `git push` and a
	// RELATIVE `cd`/`-C` target resolve against WHERE THE TOOL CALL RUNS, not os.Getwd()
	// of this hook subprocess. A payload with no cwd (older client / a non-payload stdin)
	// leaves the override nil → the process-cwd default (no regression). Best-effort: a
	// parse failure just leaves the default in place; Dispatch re-parses and fails open.
	if payloadCwd := parsePayloadCwd(stdin); payloadCwd != "" {
		b := hookResolveBase{cwd: payloadCwd, home: os.Getenv("HOME")}
		resolveBaseOverride = &b
	}

	prof := loadProfileOrDefaults()

	// AUTO-REGISTER presence (docs/build-tasks/burn-auto-enforce.md): every guard-hook
	// invocation is a PreToolUse fire on a real tool call (Bash/Agent/SendMessage), so
	// this is the cheapest machine-wide place to refresh THIS session's active-session
	// heartbeat — a live session becomes visible WITHOUT it running `leadkit burn`.
	// Deliberately a PRESENCE write (no N): a reviewer/watcher/poller session is free
	// infra, so it must never inflate the governor's account_n; the declared workstream
	// registry (written only by `leadkit burn`) still decides n. Best-effort and
	// SILENT — a presence write must never delay, fail, or add noise to the guard hook
	// it piggy-backs on.
	refreshPresence(prof)

	cfg := hook.Config{
		QueueBase: filepath.Base(prof.QueueRoot),
		LogPath:   prof.Resolve(prof.TelemetryLog),
		Workish: audit.WorkishConfig{
			DescriptionLengthThreshold: prof.SessionTaskWorkish.DescriptionLengthThreshold,
			Markers:                    prof.SessionTaskWorkish.Markers,
		},
		PushGuard:      buildPushGuardConfig(prof),
		PushGate:       buildPushGateConfig(),
		BriefLengthMax: prof.BriefLengthMax,
		Profile:        prof,
		Cwd:            hookCwd(),
		TaskStatus:     buildTaskStatusConfig(),
		OwnerGuard:     buildOwnerGuardConfig(prof),
		Milestone: hook.MilestoneBackstopConfig{
			// The backstop asks "would `milestone check` block right now?" — evaluated
			// with the real resolvers, lazily (only when a SendMessage actually reads as
			// a work-at-rest claim). Any evaluation trouble degrades to not-blocked
			// (fail open — the backstop never warns spuriously).
			Evaluate: func() (bool, string) {
				rep := loadMilestoneReport(prof)
				// The SOFT backstop only warns when there IS a declared milestone that
				// is blocking — a project with no milestone artifact declared is simply
				// unaffected (no anchor to signal against), unlike the HARD `check`
				// which treats absent-as-not-a-false-GO.
				if !rep.Declared {
					return false, ""
				}
				return rep.Blocked(), rep.Summary()
			},
		},
		RoutingInbox: hook.RoutingInboxConfig{
			// The self-surfacing seam: on this session's activity, does its project have
			// open (non-terminal) routing items to surface (deduped, once per window)?
			// routing.SurfaceOnce owns the inbox read + dedupe + fail-open, so any trouble
			// (unresolvable slug/session, unreadable store) degrades to a silent allow —
			// a durable item reaches an already-live recipient WITHOUT a SendMessage.
			Surface: func() (string, bool) {
				slug, sessionID := sessionSlugAndID(prof)
				return routing.SurfaceOnce(resolveRoutingDir(""), slug, sessionID, routing.DefaultSurfaceDedupeWindow, time.Now().UTC())
			},
		},
	}

	res := hook.Dispatch(args[0], stdin, cfg)
	if res.Stdout != "" {
		fmt.Print(res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	os.Exit(res.ExitCode)
}

// refreshPresence best-effort refreshes this session's active-session presence
// heartbeat (docs/build-tasks/burn-auto-enforce.md) with a fresh updated_at. Keyed by
// the session's cwd-honest IDENTITY slug (sessionIdentitySlug — its OWN git-repo/cwd,
// never an inherited env/ancestor config) plus a per-instance session id
// (resolveSessionID: $LEADKIT_SESSION_ID → $CLAUDE_CODE_SESSION_ID → slug), so two live
// instances register DISTINCT presence files instead of clobbering one. Also records the
// HARNESS ADDRESSING HANDLE (messaging socket + harness UUID from env) so DeliveryHint
// can name a real SendMessage target. SILENT by design: any error is swallowed —
// presence is pure liveness telemetry and must never disturb the guard hook that calls
// this. The prof arg is retained for signature stability with the guard-hook call site;
// identity intentionally does NOT read the policy profile.
func refreshPresence(prof *config.Profile) {
	slug, _ := sessionIdentitySlug(sessionCwd())
	if slug == "" {
		return
	}
	sessionID, _ := resolveSessionID(slug)
	_ = usage.WritePresenceEntryWithHarness(
		usage.DefaultPresenceDir, slug, sessionID,
		os.Getenv(messagingSocketEnvVar), os.Getenv(harnessSessionEnvVar),
		time.Now().UTC(),
	)
}
