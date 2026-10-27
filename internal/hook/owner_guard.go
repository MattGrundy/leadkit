package hook

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/owners"
)

// owner-guard: the thin PreToolUse wrapper around internal/owners.Decide (the pure,
// fully-tested decision core; see that package's doc for the ported decision order and
// the fail-open posture). This wrapper only translates a hook Input + the resolved
// policy/caller/bypass into an owners.Input, runs Decide, appends a best-effort audit
// line, and maps the Decision onto a Result. All the policy comes from the PROFILE
// (owners_file) — cmd/leadkit builds OwnerGuardConfig; see runHook.
//
// WIRING (a later, reviewed `leadkit init`): PreToolUse(Edit|Write|MultiEdit|
// NotebookEdit|Bash) → `leadkit hook owner-guard`. This subcommand touches no settings.

// OwnerGuardConfig is owner-guard's resolved inputs. The ownership Policy is resolved
// PER-TARGET via ResolvePolicy — from the PROJECT that owns the edited file, not the
// session-ambient profile — so a project that defines an owners_file has its boundary
// enforced even from a foreign session, while a project (or a file) under no owners
// policy stays disabled/allow-unowned exactly as today. WHO is acting (Caller) is
// correctly the session; WHAT policy governs is the target project's. ResolvePolicy
// returning loaded == false (no owners_file, or its file absent/unparseable, or no
// resolver wired) DISABLES the boundary system — Decide is still called (with a nil
// policy) so the disabled path is one code path, not a special case here. The guard
// stays PURE — it loads no profiles; cmd/leadkit injects ResolvePolicy (see
// resolveTargetOwnersPolicy). Home/Pwd feed the home-relative REL (Pwd also the policy-
// resolution target for a Bash command that names no single edit file); Caller/Bypass
// are resolved upstream from the acting session; AuditLogPath == "" disables the log.
type OwnerGuardConfig struct {
	ResolvePolicy func(targetPath string) (policy *owners.Policy, loaded bool)
	Caller        string
	Lead          string
	Bypass        bool
	Home          string
	Pwd           string
	AuditLogPath  string

	// Enforcement resolves owner-guard's mode for the TARGET file's project (advisory |
	// enforce) AND whether an owners policy is CONFIGURED there (owners_file set). It flips
	// ONLY the unverifiable path: a policy that is CONFIGURED but could not be loaded
	// (owners_file set, but its file is absent/unreadable/unparseable → ResolvePolicy
	// loaded=false). advisory leaves the boundary DISABLED (allow-unowned, today);
	// enforce DENIES (can't confirm ownership → block). A project that defines NO
	// owners_file (ownersConfigured=false) has no boundary and is unaffected in BOTH modes
	// — that's a clear no-policy allow, not an unverifiable condition. Optional: nil (every
	// existing test, an unconfigured adopter) means advisory + the disabled-allow path,
	// unchanged. See docs/build-tasks/configurable-guard-enforcement.md.
	Enforcement func(targetPath string) (mode GuardMode, ownersConfigured bool)

	// ProfileProblem says what is wrong with the profile that governs the TARGET file, when
	// something is (ticket 240). Optional: nil means nothing is, and every existing caller is
	// unchanged. The policy and mode ResolvePolicy and Enforcement return are already the safe ones
	// for each case; this carries the REASON, the two exceptions that let a broken profile be
	// repaired, and the verdict for a profile nothing could be read from.
	ProfileProblem func(targetPath string) OwnerProfileProblem
}

// OwnerProfileProblem is the state of a governing profile that did not simply load.
//
//   - Mismatch: the profile names a different origin than this clone. That is not "cannot tell" but
//     "not this repo's profile" (a fork), so there is no boundary and the edit is allowed, with a note.
//   - Unreadable: the profile exists and did not load. Enforcement then reports what could be read
//     of it, fail-closed, so the existing owners-policy-unreadable rule applies; Why names it.
//
// A profile nothing could be read from is reported as Unreadable too: Enforcement then returns the
// strictest reading (enforce, owners_file set), so it is refused in both modes by the same rule.
//
// File is the profile file: an edit of it is allowed so it can be repaired, as is any edit while the
// project's bypass sentinel is set.
type OwnerProfileProblem struct {
	Mismatch, Unreadable bool
	File, Why            string
}

// OwnerGuard runs the ownership boundary decision for an Edit/Write/MultiEdit/
// NotebookEdit/Bash tool call.
func OwnerGuard(in Input, cfg OwnerGuardConfig) Result {
	// Resolve the policy that GOVERNS the target of this call from the TARGET file's own
	// project. For the edit family that target is the edit path; for a Bash command (no
	// single edit path) fall back to the session cwd — the same project whose policy
	// governed a Bash command before this fix, so Bash behavior is unchanged.
	target := in.ToolInput.FilePath
	if target == "" {
		target = cfg.Pwd
	}
	var policy *owners.Policy
	loaded := false
	if cfg.ResolvePolicy != nil {
		policy, loaded = cfg.ResolvePolicy(target)
	}

	mode := ModeAdvisory
	ownersConfigured := false
	if cfg.Enforcement != nil {
		mode, ownersConfigured = cfg.Enforcement(target)
	}

	var prob OwnerProfileProblem
	if cfg.ProfileProblem != nil {
		prob = cfg.ProfileProblem(target)
	}
	// Two ways out that cost nothing and are how a broken profile gets repaired: the project's
	// bypass sentinel, and an edit of the profile file itself.
	recovering := cfg.Bypass || (prob.File != "" && in.ToolInput.FilePath != "" && filepath.Clean(in.ToolInput.FilePath) == filepath.Clean(prob.File))

	if !loaded {
		// The boundary policy did not load. Distinguish TWO cases:
		//   - owners_file NOT configured for this project → no boundary exists → allow
		//     (clear no-policy, both modes).
		//   - owners_file IS configured but its file is absent/unreadable/unparseable →
		//     UNVERIFIABLE. advisory allows (disabled, today); enforce DENIES (fail-closed:
		//     a policy we can't read can't clear the caller, so treat as unsafe).
		if mode.Enforcing() && ownersConfigured && !recovering {
			appendOwnerAudit(cfg.AuditLogPath, cfg.Caller, in.ToolName, "DENY-UNVERIFIABLE owners policy unreadable")
			if prob.Unreadable {
				return Result{Stderr: ownerGuardProfileReason(prob.Why, mode) + "\n", ExitCode: 2}
			}
			return Result{Stderr: ownerGuardUnverifiableReason(mode) + "\n", ExitCode: 2}
		}
		policy = nil // disabled boundary → allow-all, via Decide's nil-policy path
	}

	d := owners.Decide(owners.Input{
		Tool:     in.ToolName,
		FilePath: in.ToolInput.FilePath,
		Command:  in.ToolInput.Command,
		Caller:   cfg.Caller,
		Lead:     cfg.Lead,
		Bypass:   cfg.Bypass,
		Home:     cfg.Home,
		Pwd:      cfg.Pwd,
		Policy:   policy,
		Resolve:  owners.ResolvePath,
	})

	// Audit only a decision that actually consulted the policy — a disabled system or
	// a no-candidate fail-open isn't a boundary event worth a line.
	if loaded && !strings.HasPrefix(d.Reason, "ALLOW no candidate") {
		appendOwnerAudit(cfg.AuditLogPath, cfg.Caller, in.ToolName, d.Reason)
	}

	if !d.Allow {
		return Result{Stderr: d.Reason + ownerGuardModeNote(mode) + "\n", ExitCode: 2}
	}
	if prob.Mismatch {
		// Allowed, and said: a fork or a same-named clone is not governed by the profile that names
		// another origin, which is different from a profile that could not be read.
		return Result{Stderr: "owner-guard: " + prob.Why + "\n"}
	}
	return allow
}

// ownerGuardProfileReason is the deny for a governing profile that did not load: the project's
// owners boundary cannot be read from it, so the edit is refused rather than treated as unowned. The
// way out is to repair the profile (editing it is allowed), or the bypass sentinel.
func ownerGuardProfileReason(why string, mode GuardMode) string {
	return "owner-guard: " + why + ", so this project's ownership boundary cannot be read and the edit is refused (fail-closed). Repair the profile (an edit of the profile file itself is allowed), or set .leadkit/bypass to override (lead-only)." + ownerGuardModeNote(mode)
}

// ownerGuardModeNote is the trailing posture clause appended to an owner-guard deny so
// logs/doctor (and the agent) see whether the deny was advisory or enforcing. A positive
// ownership violation denies in BOTH modes (that's the guard's job); the note reports the
// resolved posture.
func ownerGuardModeNote(mode GuardMode) string {
	return " [owner-guard mode: " + mode.String() + "]"
}

// ownerGuardUnverifiableReason is the enforce-mode fail-CLOSED deny for a configured-but-
// unreadable owners policy — advisory would have left the boundary disabled (allow). A
// policy we can't read can't confirm the caller owns the path, so the action is treated
// as unsafe.
func ownerGuardUnverifiableReason(mode GuardMode) string {
	return "owner-guard: the owners policy is configured (owners_file) but could not be read/parsed — ownership cannot be verified, and this project's guards are ENFORCING, so the edit is denied (fail-closed). Fix or remove owners_file, or set enforce→advisory for this project to restore fail-open." + ownerGuardModeNote(mode)
}

// ownerAuditMaxLines / ownerAuditTrimTo mirror the shell's ~5000-line trim-to-4000
// rotation, applied at write time.
const (
	ownerAuditMaxLines = 5000
	ownerAuditTrimTo   = 4000
)

// appendOwnerAudit appends one tab-separated decision line (ts, caller, tool, verdict)
// to path, best-effort — ANY failure is swallowed (a logging failure must never affect
// the decision). It trims the log to ownerAuditTrimTo lines when it exceeds
// ownerAuditMaxLines, the same rotation the shell does at hook entry.
func appendOwnerAudit(path, caller, tool, verdict string) {
	if path == "" {
		return
	}
	trimOwnerAudit(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf("%s\t%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), caller, tool, verdict)
	_, _ = f.WriteString(line)
}

// trimOwnerAudit keeps only the last ownerAuditTrimTo lines when path exceeds
// ownerAuditMaxLines. Best-effort: any error leaves the file untouched.
func trimOwnerAudit(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	f.Close()
	if err := sc.Err(); err != nil || len(lines) <= ownerAuditMaxLines {
		return
	}
	kept := lines[len(lines)-ownerAuditTrimTo:]
	_ = os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}
