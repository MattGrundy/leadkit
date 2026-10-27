package doctor

import (
	"os"

	"mgpp.dev/leadkit/internal/install"
)

// PrePushHookCheck (ticket 171) reports whether this clone's git pre-push hook, the
// enforcement layer for a raw `git push`, will actually run. An unwired guard is
// indistinguishable from one that passes, so it never reports a hook it cannot show is
// wired as fine. Never critical (a clone without it is a degraded layer, not a broken
// install), but a WARN in every state except installed.
//
//	installed      the hook is leadkit's, executable, git reads that directory and its binary exists
//	NOT installed  no hook, a foreign hook, a stale or non-executable one: the push is not gated at the git level
//	redirected     core.hooksPath sends git to another directory, so a hook in .git/hooks never runs
//
// The hook is per clone and `--no-verify`, `core.hooksPath` and deleting the file still switch
// it off; it narrows the ways to bypass the gate to that short list and does not close them.
func PrePushHookCheck(st install.PrePushStatus) Check {
	const name = "pre-push-hook"
	warn := func(detail, fix string) Check {
		return Check{Name: name, OK: false, Severity: SeverityWarn, Detail: detail + ". " + fix}
	}
	switch st.State {
	case install.PrePushNoRepo:
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "not checked: " + st.Detail + " (the hook is per clone; run doctor inside the repo)"}
	case install.PrePushInstalled:
		if fi, err := os.Stat(st.Binary); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return warn("installed at "+st.HookPath+" but the binary it runs, "+st.Binary+", is missing or not executable, so it REFUSES every push in this clone",
				"Re-run `leadkit init --apply --confirm` from this repo.")
		}
		return Check{Name: name, OK: true, Severity: SeverityInfo, Detail: "installed: " + st.Detail + ". It is per clone, and --no-verify, core.hooksPath or deleting it still switch it off"}
	case install.PrePushRedirected:
		return warn("NOT WIRED, redirected: "+st.Detail+"; a raw `git push` here is not gated at the git level",
			"Put the hook in that directory (`exec <leadkit> gate check-push \"$@\"`) or unset core.hooksPath for this repo.")
	case install.PrePushForeign:
		return warn("NOT WIRED: "+st.Detail+", so leadkit's gate does not run on a raw `git push`",
			"Add `exec <leadkit> gate check-push \"$@\"` to it, or remove it and run `leadkit init --apply --confirm`.")
	case install.PrePushStale, install.PrePushNotExecutable:
		return warn("NOT WIRED: "+st.Detail, "Run `leadkit init --apply --confirm` from this repo.")
	default:
		return warn("NOT installed: "+st.Detail+"; a raw `git push` here is gated only by the text hook, which cannot read every spelling",
			"Run `leadkit init --apply --confirm` from this repo (the hook is per clone).")
	}
}
