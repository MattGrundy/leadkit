package doctor

import (
	"fmt"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/forge"
	"mgpp.dev/leadkit/internal/redact"
)

// forgeRunner is how forgeBranchProtectionCheck reaches the forge: the real gh/glab in
// the CLI, a fake in tests. Package-level so Run's already-long signature does not grow.
var forgeRunner forge.Runner = forge.ExecRunner{}

// forgeBranchProtectionCheck (Layer 0, docs/design/agent-sandboxing-and-enforcement.md)
// VERIFIES, against the forge, that the repo's protected branches are protected
// server-side.
//
// It reads only (gh/glab GETs). It reports three things and never blurs them:
//
//	VERIFIED        the forge affirmatively reported the required protection  -> OK, Info
//	NOT PROTECTED   the forge says it is absent, weak, or unavailable on the plan -> Warn
//	CANNOT VERIFY   leadkit could not read the state (gh/glab missing or unauthenticated,
//	                no access, a ruleset it does not inspect, unknown forge)    -> Warn
//
// CANNOT VERIFY is never reported as VERIFIED, and is worded as "treat as unprotected".
// The one OK-without-asking case is no origin remote at all, where there is nothing to
// verify and the detail says it did not check.
func forgeBranchProtectionCheck(prof *config.Profile, git Git) Check {
	const name = "forge-branch-protection"
	if git == nil {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked (no git seam given)"}
	}
	origin := git.RemoteURL("")
	if origin == "" {
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked: no origin remote resolved here, so there is no forge to verify"}
	}
	_, spec, _ := config.MatchAuthorityKey(origin, prof.Authority, false)

	cannot := func(why string) Check {
		return Check{Name: name, OK: false, Severity: SeverityWarn,
			Detail: "CANNOT VERIFY forge-side branch protection for " + redact.Origin(origin) + ": " + why +
				" — treat the branches as UNPROTECTED until verified; guards that run on the client are advisory"}
	}
	target, err := forge.ResolveTarget(origin, spec)
	if err != nil {
		return cannot(err.Error())
	}
	branches, err := forge.Branches(forgeRunner, target, spec)
	if err != nil {
		return cannot(err.Error())
	}

	var lines []string
	var notProtected, unenforceable, unverified bool
	for _, b := range branches {
		st := forge.Inspect(forgeRunner, target, b)
		switch st.State {
		case forge.StateVerified:
			lines = append(lines, fmt.Sprintf("%s: VERIFIED — %s", b, st.Detail))
		case forge.StateUnknown:
			unverified = true
			lines = append(lines, fmt.Sprintf("%s: CANNOT VERIFY — %s", b, st.Detail))
		default:
			notProtected = true
			unenforceable = unenforceable || st.State == forge.StateUnenforceable
			lines = append(lines, fmt.Sprintf("%s: NOT PROTECTED (%s) — %s", b, st.State, st.Detail))
		}
	}
	where := fmt.Sprintf("%s %s", target.Kind, target.Repo)
	summary := strings.Join(lines, "; ")
	if !notProtected && !unverified {
		return Check{Name: name, OK: true, Severity: SeverityInfo,
			Detail: fmt.Sprintf("server-side protection VERIFIED on %s — %s", where, summary)}
	}
	var tail string
	if notProtected {
		tail = " Protected branches rely only on client-side guards — a client that bypasses the hook can still push."
		if unenforceable {
			tail += " This forge plan cannot enforce branch protection here (make the repository public, move it, or upgrade the plan); `leadkit forge protect` cannot help until then."
		} else {
			tail += " Enable forge protection: `leadkit forge protect` (needs an authority.<repo> entry)."
		}
	}
	if unverified {
		tail += " Branches that could not be verified must be treated as unprotected."
	}
	return Check{Name: name, OK: false, Severity: SeverityWarn,
		Detail: fmt.Sprintf("%s: %s.%s", where, summary, tail)}
}
