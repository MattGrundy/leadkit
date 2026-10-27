package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/hook"
)

// `leadkit push` is the native, GATED push primitive — the leadkit equivalent of the
// bash ci-push.sh wrapper it replaces. It runs the review-gate check ITSELF, in-process,
// before the push, and then passes --no-verify, so the git pre-push hook does not re-judge a
// sha this command has just gated. That hook (installed by `leadkit init`) is the enforcement
// layer for a RAW `git push`, the path that does not come through here. This is that
// wrapper, folded into the binary:
//
//	leadkit push --repo <abs-clone> --branch <branch> [--sha <full-40>] [--force] [--remote origin] [--dry-run]
//
// KEY PROPERTIES (see docs/build-tasks/leadkit-push-native.md):
//   - The gate is resolved PER-TARGET-REPO (resolveTargetGate — the SAME per-target
//     resolver the push-gate hook uses), so whether a push is gated is decided by the
//     TARGET repo's own governance (scope + records_dir), never the invoking session's
//     ambient profile. The acting session's enforced_remotes are UNIONED in ADD-ONLY,
//     exactly as buildPushGateConfig does for the hook.
//   - The sha judged is ALWAYS the TARGET clone's, explicitly. An explicit --sha is used
//     verbatim (and MUST be the full 40-char sha — a short prefix collides across clones
//     and mis-keys the gate record, so it is refused). An omitted --sha resolves
//     `git -C <repo> rev-parse HEAD` for the TARGET repo — never the invoking session's
//     cwd HEAD. This is the core fix ci-push.sh's required --sha and the push-gate
//     sha-resolution hardening both address.
//   - It FAILS CLOSED on an unverifiable gate (a bad repo, an unresolvable HEAD, an
//     unresolvable scope): this is a deliberate, user-invoked push of a NAMED sha — if we
//     cannot confirm it is gated, we refuse. This is DIFFERENT from the push-gate hook's
//     advisory fail-OPEN: the hook is a best-effort backstop on an ordinary `git push`
//     the user did not route through leadkit, so it must not block work it cannot judge;
//     here the user explicitly asked leadkit to push a specific sha, so an unverifiable
//     result is a refusal, not a pass.
//   - The push uses `--force-with-lease` (never bare `--force`) so a force push can never
//     clobber a ref that moved under us. It pushes the resolved full sha PINNED to the
//     branch ref (`<sha>:refs/heads/<branch>`), matching ci-push.sh — so the exact gated
//     commit is what is published, not whatever the local branch happens to point at
//     (the #1242 "push HEAD publishes whatever is checked out" class of bug the --sha
//     pinning exists to close). `--no-verify` is passed deliberately: the gate has
//     ALREADY been enforced in-process on the resolved full sha before this command runs,
//     so the git pre-push hook would only judge the same sha twice for THIS command. That
//     does not make the hook redundant in general: it is what gates a raw `git push`. The
//     in-process gate cannot be skipped by --no-verify (it is not a git hook).
//   - REVIEW_GATE_BYPASS works for `leadkit push` via the ordinary process env
//     (gate.CheckWithOutcome reads os.Getenv): `REVIEW_GATE_BYPASS="reason" leadkit push …`
//     sets it for the invoked leadkit process, so no command parsing is needed here (that
//     parsing is only needed for the HOOK path, where the inline assignment sets the var
//     for the pushed subprocess, not the hook).

// pushOpts are the parsed `leadkit push` flags.
type pushOpts struct {
	repo   string
	branch string
	sha    string // empty → resolve the target repo's HEAD
	remote string
	force  bool
	dryRun bool
}

// pushDeps are pushRun's injected collaborators, so the whole command is unit-testable
// without a real git binary, network, or on-disk profile — the runBurn/runInit
// testability direction.
//
//   - resolveGate builds the review-gate that GOVERNS a target repo (the real path wires
//     resolveTargetGate + the session-remote union; tests inject a fake-git gate).
//   - runGit runs `git -C <repo> <args…>` and returns trimmed combined output + error
//     (tests inject a recorder that captures the exact args and returns canned output).
//   - stdout/stderr are the injected writers (tests capture them).
type pushDeps struct {
	resolveGate func(repo string) *gate.Gate
	// noProfile answers whether any leadkit profile could be found for repo (its own, or the
	// acting session's): "" when one can, else what was searched. A repo the gate calls out of
	// scope is only genuinely out of scope if a profile was read and did not govern it; with
	// none read, "out of scope" is just the built-in default talking, and the push is refused.
	// nil means not wired (unit tests that exercise only the gate outcomes).
	noProfile func(repo string) string
	// unconfigured answers whether repo is in the unconfigured state (hookUnconfigured): the text
	// of it, or "" when it is not. nil means not wired.
	unconfigured func(repo string) string
	runGit       func(repo string, args ...string) (string, error)
	stdout       io.Writer
	stderr       io.Writer
}

// runPush is the thin CLI entry point: parse flags, wire the real collaborators, run the
// pure pushRun, exit with its code.
func runPush(args []string) {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	repo := fs.String("repo", "", "absolute path to the target clone to push from (REQUIRED)")
	branch := fs.String("branch", "", "branch to push to on the remote (REQUIRED)")
	sha := fs.String("sha", "", "full 40-char sha to gate + push (default: the target repo's HEAD)")
	remote := fs.String("remote", "origin", "remote to push to")
	force := fs.Bool("force", false, "force the push with --force-with-lease (never a bare --force)")
	dryRun := fs.Bool("dry-run", false, "resolve + gate-check + print the exact push that WOULD run, push nothing")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit push: %v\n", err)
		os.Exit(2)
	}

	// The gate is resolved PER-TARGET-REPO exactly as the push-gate hook does
	// (targetGateResolver: resolveTargetGate + the acting session's enforced_remotes
	// unioned in ADD-ONLY), so a session that declares enforced_remotes still gates a push
	// to an UNPROFILED clone of one of those repos, while the target's own gating decision
	// stands on its own.
	deps := pushDeps{
		resolveGate:  targetGateResolver(),
		noProfile:    pushNoProfile,
		unconfigured: hookUnconfigured,
		runGit: func(repoPath string, gitArgs ...string) (string, error) {
			full := gitArgs
			if repoPath != "" {
				full = append([]string{"-C", repoPath}, gitArgs...)
			}
			out, err := exec.Command("git", full...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
		stdout: os.Stdout,
		stderr: os.Stderr,
	}

	os.Exit(pushRun(deps, pushOpts{
		repo: *repo, branch: *branch, sha: *sha, remote: *remote, force: *force, dryRun: *dryRun,
	}))
}

// pushNoProfile answers whether any leadkit profile could be found for repoPath: "" when one can,
// else what was searched. Three findings are kept apart: the target's own profile was found
// (governed), the target has none and nothing says it is a leadkit project while the session's
// profile was read (a decision: out of scope), and everything else (unknown, so refused).
func pushNoProfile(repoPath string) string {
	target := targetProfileState(repoPath)
	if target.Found {
		return ""
	}
	if target.Refused {
		// The target's own profile exists and was not accepted. Defaults govern in its
		// place, so nothing here says the repo is exempt; the session's profile cannot
		// vouch for a repo whose own profile was turned away.
		return target.Why
	}
	// Not found for the target. A healthy profile elsewhere in the session cannot vouch that the
	// repo is ungoverned when the repo itself says it is a leadkit project (a .leadkit/
	// directory): that is a governed repo whose profile is looking in the wrong place, the
	// same signal push-gate uses. hookProfileProblem is the one place that decides it.
	if why := hookProfileProblem(repoPath); why != "" {
		return why
	}
	// Unconfigured (ticket 302): a .leadkit/ directory and no profile anywhere, under a HOME that was
	// searched. The session's profile has nothing to say about it either way, and it is not an
	// unknown to refuse; pushRun reports the state (deps.unconfigured) instead of staying silent.
	if hookUnconfigured(repoPath) != "" {
		return ""
	}
	if session := sessionProfileState(); !session.Found {
		if session.Why == target.Why {
			return target.Why
		}
		return target.Why + "; for the session: " + session.Why
	}
	return ""
}

// pushRun is the PURE core: it resolves the sha, gates it against the TARGET repo's own
// review-gate, and (unless --dry-run) pushes the resolved full sha pinned to the branch
// ref. It performs NO I/O beyond the injected deps + os.Stat(repo) (the same existence
// check gate.CheckWithOutcome does). Exit codes: 0 success (or a --dry-run that would
// push); 1 a gate REFUSAL (ungated, or an unverifiable fail-closed) or a push/HEAD-resolve
// failure; 2 a usage/argument error.
func pushRun(deps pushDeps, opts pushOpts) int {
	if opts.repo == "" || opts.branch == "" {
		fmt.Fprintln(deps.stderr, "usage: leadkit push --repo <abs-clone> --branch <branch> [--sha <full-40>] [--force] [--remote origin] [--dry-run]")
		return 2
	}
	remote := opts.remote
	if remote == "" {
		remote = "origin"
	}

	// Resolve the sha to gate + push — ALWAYS the TARGET repo's, explicitly.
	sha := opts.sha
	if sha != "" {
		// An explicit --sha must be the FULL 40-char sha: a short prefix collides across
		// clones and mis-keys the gate record, so we refuse it rather than resolve it
		// against one clone and push it in another.
		if !isFullSHA(sha) {
			fmt.Fprintf(deps.stderr, "leadkit push: --sha must be the full 40-char sha, got %q (a short prefix collides across clones — pass the full sha)\n", sha)
			return 2
		}
	} else {
		resolved, err := deps.runGit(opts.repo, "rev-parse", "HEAD")
		if err != nil || resolved == "" {
			// Fail CLOSED: we cannot push a sha we cannot resolve for the target.
			fmt.Fprintf(deps.stderr, "leadkit push: could not resolve HEAD in %q — refusing (fail-closed): %v\n", opts.repo, err)
			return 1
		}
		sha = resolved
	}

	// Gate the exact (repo, sha) against the TARGET repo's own governing review-gate.
	g := deps.resolveGate(opts.repo)
	if g == nil {
		// Defensive: no gate to check against → fail CLOSED (unlike the hook's fail-open),
		// because this is an explicit user-invoked push we cannot prove is gated.
		fmt.Fprintf(deps.stderr, "leadkit push: no review-gate resolved for %q — refusing (fail-closed)\n", opts.repo)
		return 1
	}
	ok, outcome, reason := g.CheckWithOutcome(sha, opts.repo)

	switch outcome {
	case gate.OutcomeGated:
		// Clear pass (all layers ready, or an explicit BYPASS) — proceed.
	case gate.OutcomeOutOfScope:
		// "Out of scope" is a finding about a profile, so it needs one. With none found the
		// built-in default governs nothing, and that is an unknown, not a decision: refuse.
		if deps.noProfile != nil {
			if why := deps.noProfile(opts.repo); why != "" {
				// The same escape hatch the hook honours for this state: the env var and the records dir's
				// BYPASS file, through the one gate method that reads both.
				if bypassed, _, bp := g.CheckUnreadableProfile(why); bypassed {
					fmt.Fprintf(deps.stderr, "leadkit push: %s is not known to be gated (%s) — %s, pushing ungated\n", filepath.Base(opts.repo), why, bp)
					break
				}
				fmt.Fprintf(deps.stderr, "leadkit push: refusing — whether %s is gated cannot be known, because %s. This is not a policy that exempts the repo (fail-closed). Fix the environment (HOME, the working directory) or the profile, or set REVIEW_GATE_BYPASS=\"reason\" for a deliberate ungated push.\n", filepath.Base(opts.repo), why)
				return 1
			}
		}
		// No profile exists for the repo (ticket 302): there is no gate to judge the push against, so it
		// goes ahead, and the state is stated, never left to the silence.
		if deps.unconfigured != nil {
			if note := deps.unconfigured(opts.repo); note != "" {
				fmt.Fprintln(deps.stderr, "leadkit push: "+hook.UnconfiguredWarning(opts.repo, note))
				break
			}
		}
		// A profile was read and does not govern this repo — proceed, but say so plainly.
		fmt.Fprintf(deps.stderr, "leadkit push: %s is out of scope of the profile in effect — pushing ungated\n", filepath.Base(opts.repo))
	case gate.OutcomeUngated:
		fmt.Fprintln(deps.stderr, pushRefusalReason("Review-gate not satisfied", reason, g.RecordsDir))
		return 1
	case gate.OutcomeUnverifiable:
		// Fail CLOSED — deliberately unlike the push-gate hook's advisory fail-open. This
		// is an explicit push of a NAMED sha: if we cannot confirm it is gated, we refuse.
		if gate.OriginUnreadable(reason) {
			// The cause is known and it is the remote's name; "an explicit push of a named sha" would
			// be the wrong thing to tell the user. The BYPASS escape hatch was read ahead of this.
			fmt.Fprintf(deps.stderr, "leadkit push: refusing — %s (fail-closed). %s, or set REVIEW_GATE_BYPASS=\"reason\" for a deliberate ungated push.\n", strings.TrimSuffix(reason, " — allow"), gate.OriginUnreadableFix)
			return 1
		}
		fmt.Fprintf(deps.stderr, "leadkit push: could not confirm this push is gated (%s) — refusing (fail-closed). This is an explicit push of a named sha, so an unverifiable gate is a refusal (unlike the advisory push-gate hook).\n", strings.TrimSuffix(reason, " — allow"))
		return 1
	default:
		if !ok {
			fmt.Fprintln(deps.stderr, pushRefusalReason("Review-gate not satisfied", reason, g.RecordsDir))
			return 1
		}
	}

	status := pushStatus(outcome, reason)
	refspec := sha + ":refs/heads/" + opts.branch
	pushArgs := []string{"push"}
	if opts.force {
		pushArgs = append(pushArgs, "--force-with-lease")
	}
	// --no-verify is deliberate: the gate has ALREADY run in-process on the resolved full
	// sha above, so the pre-push hook would re-judge it. The hook is not redundant for a raw
	// push; it is only skipped for this governed one.
	pushArgs = append(pushArgs, "--no-verify", remote, refspec)

	if opts.dryRun {
		fmt.Fprintf(deps.stdout, "leadkit push (dry-run): would push git -C %s %s  [%s %s]\n",
			opts.repo, strings.Join(pushArgs, " "), shortSHA12(sha), status)
		return 0
	}

	out, err := deps.runGit(opts.repo, pushArgs...)
	if err != nil {
		if out != "" {
			fmt.Fprintln(deps.stderr, out)
		}
		fmt.Fprintf(deps.stderr, "leadkit push: push failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(deps.stdout, "leadkit push: %s %s %s -> %s [%s]\n",
		filepath.Base(opts.repo), opts.branch, shortSHA12(sha), remote, status)
	return 0
}

// pushStatus is the compact gated/bypass/out-of-scope tag for the output line.
func pushStatus(outcome gate.CheckOutcome, reason string) string {
	switch outcome {
	case gate.OutcomeOutOfScope:
		return "out-of-scope"
	case gate.OutcomeGated:
		if strings.Contains(reason, "BYPASS") {
			return "bypass"
		}
		return "gated"
	default:
		return "gated"
	}
}

// pushRefusalReason is the ungated-refusal message: the gate's own reason plus how to
// record the missing layers and the BYPASS escape hatch (mirrors the push-gate hook's
// deny wording, since a lead migrating off ci-push.sh sees the same guidance either way).
func pushRefusalReason(prefix, reason, recordsDir string) string {
	bypassFile := "the records dir's BYPASS file"
	if recordsDir != "" {
		bypassFile = filepath.Join(recordsDir, "BYPASS")
	}
	return "leadkit push: BLOCKED — " + prefix + ". " + reason +
		"  Record layers with: `leadkit gate record --layer <code-review|critique> --verdict ready --mutants \"<mutant -> failing test>\"` (or `--no-mutant \"<why>\"`)." +
		"  Override: set `REVIEW_GATE_BYPASS=\"reason\"` or create " + bypassFile + "."
}

// isFullSHA reports whether s is exactly a 40-char lowercase-or-uppercase hex string —
// the full git object name a gate record is keyed by (a short prefix is refused).
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// shortSHA12 is the 12-char short sha for human output (matches ci-push.sh's ${SHA:0:12}).
func shortSHA12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
