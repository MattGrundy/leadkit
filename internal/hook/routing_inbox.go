package hook

// routing-inbox is the SELF-SURFACING PreToolUse hook for the account-level routing
// store (docs/build-tasks/routing-substrate.md). It is how a DURABLE routing item reaches
// an already-live recipient WITHOUT a SendMessage: on a session's activity, if that
// session's project has open (non-terminal) routing items, it emits a ONE-LINE surface
// pointing at `leadkit routing inbox`. It is the same lightweight surfacing pattern as
// milestone-backstop — a warn-only, exit-0 stderr line, never a block.
//
// FAILS OPEN like every guard: a nil Surface seam (unresolvable identity / disabled), no
// open items, or any read trouble inside Surface → silent allow. It is also DEDUPED
// (once per session per window, handled inside the Surface seam) so it never nags on
// every tool call. It never inspects the tool — activity of any kind is the trigger; the
// settings.json matcher decides which tool events fire it.

// RoutingInboxConfig carries the routing-inbox surfacing seam. Surface reports whether
// there is a one-line surface to emit right now (and returns it), handling its own inbox
// read, dedupe, and fail-open (any trouble → ok=false). nil Surface = disabled (the hook
// never surfaces), so a session with no resolvable slug/session or an unreadable store is
// simply unaffected.
type RoutingInboxConfig struct {
	Surface func() (line string, ok bool)
}

// RoutingInbox is the self-surfacing hook. Returns a one-line stderr surface (exit 0 —
// NEVER a block) when the seam says there are open routing items and the dedupe window
// has elapsed; silent allow otherwise.
func RoutingInbox(in Input, cfg RoutingInboxConfig) Result {
	if cfg.Surface == nil {
		return allow
	}
	line, ok := cfg.Surface()
	if !ok || line == "" {
		return allow
	}
	return Result{Stderr: line + "\n", ExitCode: 0}
}
