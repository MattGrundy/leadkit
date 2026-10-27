package prcreate

// merge.go adds the governed `leadkit pr merge` path — the fold of the legacy
// the shell merge wrapper into leadkit, as a SIBLING of pr create / pr edit (NOT an
// overload of Decide/DecideEdit). It mirrors their design exactly: a PURE DecideMerge
// returning the same Decision struct (no I/O), an injected Runner (ExecuteMerge), and
// the forge abstraction reused unchanged (a new MergeArgv method).
//
// A merge is the HIGHEST-RISK verb in the kit — IRREVERSIBLE and outward-facing — so
// this path is adversarially default-DENY and fails CLOSED on any unreadable input. All
// I/O (PR metadata, the skip-ci HEAD check, the CI-green check, the squash-message
// compose, the review-gate read) is resolved by the CLI and fed in as already-resolved
// values + success flags; DecideMerge itself performs NO I/O, so every governance
// branch is directly unit-testable and dry-run/refusal can NEVER reach the runner.
//
// The legacy the merge wrapper condition ladder is preserved EXACTLY (PR OPEN + not draft +
// mergeable == MERGEABLE + mergeStateStatus == CLEAN → no skip-ci HEAD → CI all green →
// review-gate 2/2), with leadkit's authority governance (default-deny → can_merge →
// merge_grant/--user-approved) layered in FRONT of it. Crucially:
//   - --user-approved substitutes for the review-gate 2/2 ONLY. The OPEN/not-draft/
//     MERGEABLE/CLEAN/no-skip-ci/CI-green conditions are NEVER substitutable by it — a
//     human vouches for review quality, not for a PR being open, clean, or CI-green.
//   - merge_grant OR --user-approved is REQUIRED to proceed at all; merge_grant enables
//     AUTONOMY (no per-merge human) but does NOT bypass the review-gate — the auto-gate
//     2/2 must still be green when merging on a grant alone.
//   - the squash body is composed + skip-ci-stripped by the CLI (internal/gh.Compose)
//     and passed in; DecideMerge refuses (fail-closed) if that compose did not succeed,
//     so the #727/#728 skip-ci-in-a-constituent-commit guard is never delegated to the
//     forge's default squash composition.

import (
	"fmt"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/ticketlink"
)

// MergeRequest is the uniform, forge-agnostic PR-merge request as the CLI expresses it.
type MergeRequest struct {
	PRNumber string   // the PR/MR number to merge
	Method   string   // "squash" (default) | "merge" | "rebase"
	Extra    []string // passthrough forge flags, appended after the composed flags
	// Ticket is the id of the ticket this merge satisfies ("" for none). When set the merge commit
	// carries a `Ticket: <id>` trailer, which only a squash or merge commit can: a rebase writes no
	// commit of its own, so DecideMerge refuses that combination rather than merge unlinked.
	Ticket string
}

// PRState is the forge PR metadata the merge gate reads, resolved by the CLI (from
// `gh pr view --json ...`) and fed into DecideMerge. Only meaningful when MetaOK.
type PRState struct {
	State            string // OPEN | CLOSED | MERGED
	IsDraft          bool
	Mergeable        string // MERGEABLE | CONFLICTING | UNKNOWN
	MergeStateStatus string // CLEAN | BLOCKED | DIRTY | ...
	HeadSHA          string // headRefOid — what the skip-ci + review-gate checks key on
	Title            string
	BaseRefName      string // the branch the PR targets; the lineage check measures against it
	URL              string // the PR's full URL; how the ticket recording it is found
}

// DecideMergeInput is everything DecideMerge needs — all already-resolved, NO I/O. The
// CLI resolves origin, authority, the PR metadata, the skip-ci HEAD check, the CI-green
// check, the review-gate read, and (for squash) the composed message, then DecideMerge
// applies the governance ladder deterministically. Every I/O result carries a success
// flag so an unreadable input fails CLOSED (refuse) rather than being read as "fine".
type DecideMergeInput struct {
	OriginURL      string               // the target repo's origin remote URL
	AuthKey        string               // the matched authority.<repo> key (for messages)
	Spec           config.AuthoritySpec // the resolved authority policy
	HasAuthority   bool                 // did any authority key match the origin?
	ProfileMissing string               // no profile was found at all: see noAuthorityReason
	Req            MergeRequest         // the composed PR-merge request
	UserApproved   bool                 // explicit human substitute for the review-gate 2/2

	// --- forge PR metadata (fail-closed on an unreadable fetch) ---
	MetaOK     bool    // did the forge PR-metadata fetch succeed?
	MetaReason string  // why the fetch failed (shown on the fail-closed refusal)
	State      PRState // the fetched metadata (only meaningful when MetaOK)

	// --- skip-ci HEAD check (fail-closed if it couldn't resolve) ---
	SkipCIChecked bool   // did the skip-ci HEAD check resolve at all?
	HeadSkipCI    bool   // does the PR HEAD commit carry a [skip ci] / [ci skip] directive?
	SkipCIReason  string // why the check couldn't resolve (shown on the fail-closed refusal)

	// --- CI-green check (any non-green is a refusal; NOT substitutable) ---
	CIGreen  bool   // did `gh pr checks` come back all-green (exit 0)?
	CIReason string // the not-green detail (shown on refusal)

	// --- review-gate 2/2 (the ONLY thing --user-approved substitutes) ---
	GateOK     bool   // review-gate 2/2 (code-review+critique) ready for State.HeadSHA?
	GateReason string // the gate's reason (shown when it did not pass)
	// LineageOK says every branch the PR head is built on is itself gated. No default: unresolved
	// is refused. See prcreate.CheckLineage.
	LineageOK     bool
	LineageReason string

	// --- stacked branches (squash and rebase only; fail-closed). See ResolveMergeMethod. ---
	DependentsOK     bool     // did the stack check resolve? No default: unresolved is refused for a squash or rebase
	DependentsReason string   // why it did not (shown on the refusal)
	Dependents       []string // branches carrying commits this PR would merge, other than its own

	// --- squash-message compose (squash method only; fail-closed) ---
	ComposeOK     bool   // did the squash-message compose succeed?
	Subject       string // composed squash subject (skip-ci stripped by internal/gh)
	Body          string // composed squash body (skip-ci stripped by internal/gh)
	ComposeReason string // why the compose failed (shown on the fail-closed refusal)
}

// DecideMerge is the PURE governance resolver for `leadkit pr merge` — no I/O, fully
// unit-testable. It enforces, IN ORDER (default-DENY throughout, and fail-CLOSED on any
// unreadable input):
//  1. no authority entry matched the origin → REFUSE (never merge an ungoverned repo).
//  2. can_merge: true → continue; false → refuse ("not wired"); "human" → refuse with a
//     DISTINCT escalation message.
//  3. proceed authority: merge_grant OR --user-approved is REQUIRED — an empty grant
//     with no --user-approved → REFUSE (a merge needs a standing grant or a human).
//  4. forge-metadata fetch FAILURE → REFUSE (fail-closed; never merge on unreadable
//     state). [the merge wrapper: `die "cannot fetch PR"`]
//  5. PR must be OPEN, 6. not draft, 7. mergeable == MERGEABLE, 8. mergeStateStatus ==
//     CLEAN — the merge-wrapper ladder, EXACT. NONE of these is substitutable by
//     --user-approved.
//  9. skip-ci HEAD: an unresolved check → REFUSE (fail-closed); a skip-ci HEAD → REFUSE.
//     NOT substitutable by --user-approved.
//  10. CI checks all green → else REFUSE. NOT substitutable by --user-approved.
//  11. review-gate 2/2 → must hold UNLESS --user-approved (the ONLY condition a human
//     substitutes; the gate is NEVER silently skipped without it).
//  12. only after all pass: validate the method, refuse a squash or rebase that would orphan stacked
//     branches (default to a merge commit when none was asked for), require a successful squash compose
//     (squash only, fail-closed), select the forge, and compose the exact argv.
func DecideMerge(in DecideMergeInput) Decision {
	// 1. default-deny: an ungoverned repo never reaches the forge.
	if !in.HasAuthority {
		return mergeRefuse("%s", noAuthorityReason("PR-merge", in.OriginURL, in.ProfileMissing))
	}

	// The passthrough is an allowlist, and nothing is on it: `--admin` would merge past the very
	// protections the gate steps below exist to respect. See allowedPassthrough.
	if reason := refusePassthrough("merge", in.Req.Extra); reason != "" {
		return mergeRefuse("%s", reason)
	}

	// 2. can_merge — human is distinct from false (escalate vs not-yet-wired).
	if in.Spec.CanMerge.Human {
		return mergeRefuse(
			"merging in %s requires a human; the lead escalates to the user", in.AuthKey)
	}
	if !in.Spec.CanMerge.Allow {
		return mergeRefuse(
			"can_merge is false for %s — not wired for autonomous PR-merge", in.AuthKey)
	}

	// 3. proceed authority: merge_grant OR --user-approved. An empty grant with no human
	//    approval → refuse. merge_grant enables autonomy but (see step 11) does NOT
	//    bypass the review-gate.
	if strings.TrimSpace(in.Spec.MergeGrant) == "" && !in.UserApproved {
		return mergeRefuse(
			"PR #%s merge denied for %s — merge_grant is empty and --user-approved was not passed; a merge needs a standing merge_grant or an explicit human approval",
			in.Req.PRNumber, in.AuthKey)
	}

	// 4. fail-closed: never merge on PR state we couldn't read.
	if !in.MetaOK {
		return mergeRefuse(
			"PR #%s merge denied for %s — cannot read PR state: %s. Fail-closed: never merge on unreadable forge metadata.",
			in.Req.PRNumber, in.AuthKey, reasonOr(in.MetaReason, "forge metadata fetch failed"))
	}

	// 5-8. the merge wrapper mergeability ladder — EXACT, and NEVER substitutable by
	//      --user-approved (a human vouches for review, not for a PR being open/clean).
	if in.State.State != "OPEN" {
		return mergeRefuse("PR #%s state=%s (need OPEN)", in.Req.PRNumber, orUnknown(in.State.State))
	}
	if in.State.IsDraft {
		return mergeRefuse("PR #%s is a draft", in.Req.PRNumber)
	}
	if in.State.Mergeable != "MERGEABLE" {
		return mergeRefuse("PR #%s mergeable=%s (need MERGEABLE)", in.Req.PRNumber, orUnknown(in.State.Mergeable))
	}
	if in.State.MergeStateStatus != "CLEAN" {
		return mergeRefuse("PR #%s mergeStateStatus=%s (need CLEAN)", in.Req.PRNumber, orUnknown(in.State.MergeStateStatus))
	}

	// 9. skip-ci HEAD — an unresolved check fails closed; a skip-ci HEAD refuses. A
	//    [skip ci] / [ci skip] HEAD suppresses the WHOLE push/PR check-suite, so the PR
	//    silently gets ZERO CI while mergeStateStatus still reads CLEAN (cost #727/#728
	//    their runs). NOT substitutable by --user-approved.
	if !in.SkipCIChecked {
		return mergeRefuse(
			"PR #%s merge denied — cannot determine whether HEAD %s carries a skip-ci directive: %s. Fail-closed.",
			in.Req.PRNumber, shortSHA(in.State.HeadSHA), reasonOr(in.SkipCIReason, "skip-ci HEAD check failed"))
	}
	if in.HeadSkipCI {
		return mergeRefuse(
			"PR #%s HEAD %s carries a skip-ci directive — GitHub creates ZERO checks for this push/PR event; amend/reorder so a skip-ci commit isn't HEAD, then re-push before merging",
			in.Req.PRNumber, shortSHA(in.State.HeadSHA))
	}

	// 10. CI checks all green — NOT substitutable by --user-approved.
	if !in.CIGreen {
		return mergeRefuse(
			"PR #%s checks are not all green: %s", in.Req.PRNumber, reasonOr(in.CIReason, "gh pr checks did not exit clean"))
	}

	// 11. review-gate 2/2 — the ONLY condition --user-approved substitutes. NEVER skip
	//     it silently: absent --user-approved, the auto-gate MUST be green (this holds
	//     even when merging on a merge_grant — the grant is autonomy, not a review bypass).
	if !in.UserApproved && !in.GateOK {
		return mergeRefuse(
			"PR #%s HEAD %s not gated: %s. Re-gate (code-review+critique both ready), or pass --user-approved if a human reviewed it.",
			in.Req.PRNumber, shortSHA(in.State.HeadSHA), reasonOr(in.GateReason, "no review-gate 2/2 record"))
	}

	// 11b. lineage — the gate on HEAD says nothing about the commits beneath it, and a merge lands
	//      them all. Substitutable by --user-approved exactly as the gate is, because it IS the gate.
	if !in.UserApproved && !in.LineageOK {
		return mergeRefuse(
			"PR #%s HEAD %s is gated but its lineage is not: %s. Gate what lies beneath, or pass --user-approved if a human reviewed it.",
			in.Req.PRNumber, shortSHA(in.State.HeadSHA), reasonOrUnchecked(in.LineageReason))
	}

	// 12. build the forge argv. Validate the method, and (squash only) require the
	//     skip-ci-stripping compose to have succeeded — the #727/#728 guard is enforced
	//     here, never delegated to the forge's default squash composition.
	method := normalizeMergeMethod(in.Req.Method)
	if method != "squash" && method != "merge" && method != "rebase" {
		return mergeRefuse(
			"PR #%s merge denied — unknown merge method %q (want squash|merge|rebase)", in.Req.PRNumber, in.Req.Method)
	}
	// 12a. a squash or rebase orphans every branch stacked on the PR. NOT substitutable by
	//      --user-approved: a human vouches for review, not for breaking the branches beneath it.
	method, methodNote, refusal := ResolveMergeMethod(in)
	if refusal != "" {
		return mergeRefuse("%s", refusal)
	}
	if method == "squash" && !in.ComposeOK {
		return mergeRefuse(
			"PR #%s merge denied — could not compose the squash message: %s. Fail-closed (the squash body MUST strip any skip-ci directive riding in a constituent commit).",
			in.Req.PRNumber, reasonOr(in.ComposeReason, "squash-message compose failed"))
	}

	// 12b. the merge is pinned to the head the ladder judged, so that head must be known in full. A
	//      PR whose head could not be named cannot be pinned, and an unpinned merge is the gap the
	//      pin exists to close. NOT substitutable by --user-approved.
	if !isFullSHA(in.State.HeadSHA) {
		return mergeRefuse(
			"PR #%s merge denied — the head sha %q is not a full commit sha, so the merge cannot be pinned to the head the checks judged. Fail-closed.",
			in.Req.PRNumber, in.State.HeadSHA)
	}

	f, err := forgeFor(in.OriginURL, in.Spec)
	if err != nil {
		return Decision{Allowed: false, Reason: err.Error()}
	}

	// 12c. the trailers. Every merge this verb makes is marked `Leadkit-Merge: pr-merge`, so a merge
	//      commit on the base branch without it was made some other way; `Ticket: <id>` is added when
	//      a ticket is named. The link between a ticket and the commit that satisfied it is written
	//      here, into the message of the commit the forge is about to create. A ticket asked for and
	//      not possible is a refusal, not a merge that quietly drops the link. A merge that cannot
	//      carry the marker either (a rebase writes no commit; glab is not told a merge commit's
	//      message) still proceeds when no ticket is asked for, unmarked.
	ticket := strings.TrimSpace(in.Req.Ticket)
	body := ""
	switch {
	case method == "rebase":
		if ticket != "" {
			return mergeRefuse(
				"PR #%s merge denied — --task %s needs a squash or merge commit to carry its Ticket trailer, and a rebase writes no commit of its own. Use --method squash or merge.",
				in.Req.PRNumber, ticket)
		}
	case method == "merge" && f.Name() != "github":
		if ticket != "" {
			return mergeRefuse(
				"PR #%s merge denied — --task %s: %s cannot be told the message of a merge commit, so the Ticket trailer cannot ride on one. Use --method squash.",
				in.Req.PRNumber, ticket, f.Name())
		}
	case method == "squash":
		body = ticketlink.WithTrailers(in.Body, ticket)
	default:
		body = ticketlink.TrailerBlock(ticket)
	}

	argv := f.MergeArgv(mergeReq{
		OwnerName: OwnerName(in.OriginURL),
		Number:    in.Req.PRNumber,
		Method:    method,
		Subject:   in.Subject,
		Body:      body,
		Ticket:    strings.TrimSpace(in.Req.Ticket),
		HeadSHA:   in.State.HeadSHA,
		Extra:     in.Req.Extra,
	})
	return Decision{Allowed: true, Argv: argv, Reason: fmt.Sprintf(
		"%s: merge allowed for %s (PR #%s, %s%s)%s", f.Name(), in.AuthKey, in.Req.PRNumber, method, approvalNote(in), noteSuffix(methodNote))}
}

// ExecuteMerge renders or executes a merge Decision — the single orchestration point
// both the CLI and the tests drive, mirroring Execute/ExecuteEdit. When the decision is
// refused OR dryRun is set, the runner is NEVER called (so a merge — the irreversible
// action — cannot happen on a refusal or a dry-run).
func ExecuteMerge(in DecideMergeInput, dryRun bool, runner Runner) (Decision, string, error) {
	d := DecideMerge(in)
	if !d.Allowed || dryRun {
		return d, "", nil
	}
	out, err := runner.Run(d.Argv)
	return d, out, err
}

// mergeReq is the forge-facing merge request — MergeRequest plus the derived owner/name
// and the composed squash subject/body.
type mergeReq struct {
	OwnerName string
	Number    string
	Method    string // squash | merge | rebase (already validated)
	Subject   string // squash only
	Body      string // squash: the composed body plus the trailer block; merge: only the trailer block
	Ticket    string // "" for none; the id whose trailer Body (or glab's message) carries
	// HeadSHA is the full sha of the head the ladder judged. Every forge argv carries it as the
	// head the merge must match, so a push to the branch after the checks fails the merge instead of
	// landing commits nobody gated.
	HeadSHA string
	Extra   []string
}

// MergeArgv composes `gh pr merge <n> --repo <owner/name> <--squash|--merge|--rebase>
// [--subject <s> --body <b>] --match-head-commit <sha> [extra]`. The pin is what makes the gate's claim
// ("this sha was reviewed") true of what lands: without it the merge takes whatever the branch points at
// when the command runs, seconds after the ladder judged a different head. For squash it passes the CLI-composed,
// skip-ci-stripped subject/body explicitly — NEVER letting gh fall through to its own
// default composition (which would concatenate every constituent commit verbatim, so a
// dist commit's "[skip ci]" would land on the merge commit and suppress post-merge CI).
func (githubForge) MergeArgv(r mergeReq) []string {
	argv := []string{"gh", "pr", "merge", r.Number, "--repo", r.OwnerName}
	switch r.Method {
	case "squash":
		argv = append(argv, "--squash", "--subject", r.Subject, "--body", r.Body)
	case "merge":
		argv = append(argv, "--merge")
		if r.Body != "" {
			argv = append(argv, "--body", r.Body) // only ever the trailer block; see DecideMerge 12c
		}
	case "rebase":
		argv = append(argv, "--rebase")
	}
	argv = append(argv, "--match-head-commit", r.HeadSHA)
	return append(argv, r.Extra...)
}

// MergeArgv composes `glab mr merge <n> --repo <owner/name> ... --sha <head>`; glab's --sha is the
// head the merge request must still have, the same pin as gh's --match-head-commit. This path has
// never run against a live GitLab; the argv shape is what is pinned. GitLab's merge verb is
// `mr merge`; it takes the squash commit message via --message (no separate subject/body
// on the wire). the merge wrapper was GitHub-only, so this GitLab path carries the same
// never-run-for-real caveat as CreateArgv/EditArgv — the injectable seam and argv shape
// are what matter for governance + tests. The subject is still the CLI-composed,
// skip-ci-stripped one, so the strip guard holds on either forge.
func (gitlabForge) MergeArgv(r mergeReq) []string {
	argv := []string{"glab", "mr", "merge", r.Number, "--repo", r.OwnerName}
	switch r.Method {
	case "squash":
		message := r.Subject + "\n\n" + ticketlink.TrailerBlock(r.Ticket) // the trailers ride in the one message glab takes
		argv = append(argv, "--squash", "--message", message)
	case "merge":
		argv = append(argv, "--merge")
	case "rebase":
		argv = append(argv, "--rebase")
	}
	argv = append(argv, "--sha", r.HeadSHA)
	return append(argv, r.Extra...)
}

// normalizeMergeMethod defaults an empty method to "squash" (the merge wrapper's default) and
// otherwise lower-cases/trims it for the switch; an unrecognised value is left as-is so
// DecideMerge can refuse it explicitly.
func normalizeMergeMethod(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" {
		return "squash"
	}
	return m
}

// mergeRefuse builds a refused Decision (Argv nil) with a formatted reason.
func mergeRefuse(format string, a ...interface{}) Decision {
	return Decision{Allowed: false, Reason: fmt.Sprintf(format, a...)}
}

// reasonOr returns reason when non-blank, else fallback — so a missing detail never
// leaves a refusal message with a dangling empty clause.
func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

// orUnknown renders an empty forge field as "(unknown)" in a refusal message.
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unknown)"
	}
	return s
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " — " + note
}

// approvalNote annotates the allow summary with WHY the merge was permitted to proceed —
// an explicit human approval, or a standing grant.
func approvalNote(in DecideMergeInput) string {
	if in.UserApproved {
		return ", --user-approved"
	}
	if g := strings.TrimSpace(in.Spec.MergeGrant); g != "" {
		return ", grant=" + g
	}
	return ""
}

// isFullSHA reports whether s is a full commit id: 40 hex digits (sha1) or 64 (sha256).
func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
