package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/review"
)

// runGate dispatches the gate subcommand group (record|check|show|check-push|verdict), the
// Go equivalent of review-gate.sh + gate-verdict.sh.
func runGate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit gate <record|check|show|check-push|verdict> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "record":
		runGateRecord(args[1:])
	case "check":
		runGateCheck(args[1:])
	case "show":
		runGateShow(args[1:])
	case "check-push":
		runGateCheckPush(args[1:])
	case "verdict":
		runGateVerdict(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit gate: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

const (
	mutantsUsage = "mutation evidence, free text: each mutant you applied and the test that failed for it. A ready needs this or --no-mutant. " +
		"A mutant that does not compile kills nothing, and a run without -count=1 can read as dead from the test cache. " +
		"Stored beside the verdict as evidence; the gate never reads it, and it is only as honest as --by"
	noMutantUsage = "ready only, instead of --mutants: why no mutant applies (a comment-only or docs-only diff has none). Stored as \"no mutant applies, because <text>\""
)

// runGateVerdict is `gate-verdict`: record a layer verdict AND reconcile the ticket's
// frontmatter/queue location to match, as one action. Exit codes: 0 on success or a
// no-op (already fully gated); 1 when critique was recorded but the gate still isn't
// passing (guard working, ticket left in place); 2 on any pre-record refusal.
func runGateVerdict(args []string) {
	fs := flag.NewFlagSet("gate verdict", flag.ContinueOnError)
	ticket := fs.String("ticket", "", "the ticket's current queue location (queue/<agent>/<status>/<base>)")
	verdict := fs.String("verdict", "", "verdict: ready|changes-needed")
	by := fs.String("by", "", "recording agent/role name (default: $CLAUDE_AGENT_NAME when set, else claude; when it is set, any other name is refused)")
	next := fs.String("next", "", "live queue-dir name for the next layer (default: the 'critique' role's live name)")
	layer := fs.String("layer", "", "layer: code-review|critique (default: derived from the existing record)")
	base := fs.String("base", "", "ready only: the commit the review read FROM, so the layer vouches for <base>...<sha> (see `gate record --base`)")
	mutants := fs.String("mutants", "", mutantsUsage)
	noMutant := fs.String("no-mutant", "", noMutantUsage)
	dryRun := fs.Bool("dry-run", false, "print the plan (derived layer, the record that would be written, frontmatter changes, destination) and write NOTHING")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate verdict: %v\n", err)
		os.Exit(2)
	}
	if *ticket == "" || *verdict == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit gate verdict --ticket <p> --verdict ready|changes-needed [--by <n>] [--next <a>] [--layer <L>] [--dry-run]")
		os.Exit(2)
	}

	evidence, err := gate.ResolveMutationEvidence(*verdict, *mutants, *noMutant)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	who, bound, err := gate.ResolveBy(*by, os.Getenv("CLAUDE_AGENT_NAME"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	*by = who
	warnUnboundReviewer(os.Stderr, who, bound)

	// ONE loaded profile for both the Reviewer (ticket/queue resolution) and the
	// Gate (scope) — so the two can never disagree about which project they're in
	// (docs/build-tasks/profile-store-resolution.md).
	prof := loadProfileOrDefaults()
	queueRoot, storeRoot := resolveQueueAndStore(prof)
	rv := &review.Reviewer{QueueRoot: queueRoot, TasksDir: storeRoot}
	// The gate that judges the ticket's repo is that repo's own, resolved once the
	// ticket's `repo:` is known (GateForRepo) — newGate(prof) scopes by the SESSION's profile
	// and is only the fallback for a caller that supplies no resolver.
	res, err := rv.GateVerdict(newGate(prof), review.VerdictOpts{
		Ticket: *ticket, Verdict: *verdict, By: *by, ByBound: bound, Next: *next, Layer: *layer, Base: *base, MutationEvidence: evidence, DryRun: *dryRun,
		Roster: rosterTypes(prof), LaneAliases: prof.LaneAliases,
		GateForRepo: reviewGateResolver(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	note := ""
	if !res.NoOp && !res.DryRun {
		note = byNote(bound) // only when a verdict was actually recorded
	}
	fmt.Println(res.Message + note)
	if res.GuardFailed {
		os.Exit(1)
	}
	os.Exit(0)
}

// runGateRecord is `review-gate record`: record one layer verdict. Exits 2 on refusal
// (matches review-gate.sh's die()), 0 on success.
func runGateRecord(args []string) {
	fs := flag.NewFlagSet("gate record", flag.ContinueOnError)
	sha := fs.String("sha", "", "sha to record against (default: HEAD)")
	layer := fs.String("layer", "", "layer: code-review|critique")
	verdict := fs.String("verdict", "", "verdict: ready|changes-needed")
	by := fs.String("by", "", "recording agent/role name (default: $CLAUDE_AGENT_NAME when set, else claude; when it is set, any other name is refused)")
	repo := fs.String("repo", "", "repo path (default: process cwd)")
	base := fs.String("base", "", "ready only: the commit your review read FROM (e.g. origin/main for a review of the whole stack); "+
		"the layer then vouches for <base>...<sha>, so a stacked PR's lineage check needs no record on the branches inside it. "+
		"Omit it and the layer covers only <sha>. Re-recording the same ready with --base adds one to a layer that has none, but only as the role that recorded it; a base is never rewritten")
	mutants := fs.String("mutants", "", mutantsUsage)
	noMutant := fs.String("no-mutant", "", noMutantUsage)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate record: %v\n", err)
		os.Exit(2)
	}
	evidence, err := gate.ResolveMutationEvidence(*verdict, *mutants, *noMutant)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	who, bound, err := gate.ResolveBy(*by, os.Getenv("CLAUDE_AGENT_NAME"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	*by = who
	warnUnboundReviewer(os.Stderr, who, bound)
	// The record lands in the TARGET repo's records_dir (the one push-gate and gate check
	// read), not the session's, which is why this resolves the target rather than gate.New().
	g := reviewGateResolver()(*repo)
	resolvedSHA, err := g.Record(gate.RecordOpts{SHA: *sha, Layer: *layer, Verdict: *verdict, By: *by, ByBound: bound, Repo: *repo, Base: *base, MutationEvidence: evidence})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	fmt.Printf("review-gate: recorded %s=%s for %s (by %s)%s\n", *layer, *verdict, resolvedSHA, *by, byNote(bound))
}

// runGateCheck is `review-gate check`: report whether a sha passes the gate. Exits 1
// on BLOCK, 0 on allow/OK.
func runGateCheck(args []string) {
	fs := flag.NewFlagSet("gate check", flag.ContinueOnError)
	sha := fs.String("sha", "", "sha to check (default: HEAD)")
	repo := fs.String("repo", "", "repo path (default: process cwd)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate check: %v\n", err)
		os.Exit(2)
	}
	g := reviewGateResolver()(*repo)
	// The same rule the push-gate hook has: a push the gate cannot confirm is allowed in advisory
	// mode and refused in enforce mode, by the TARGET repo's own push-gate mode.
	g.RefuseUnverifiable = resolveTargetGuardModeDir(*repo, config.GuardPushGate).Enforcing()
	ok, reason := g.Check(*sha, *repo)
	if ok {
		fmt.Fprintf(os.Stderr, "review-gate: OK — %s\n", reason)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "review-gate: BLOCK — %s\n", reason)
	if gate.OriginUnreadable(reason) {
		fmt.Fprintf(os.Stderr, "review-gate: %s, or set REVIEW_GATE_BYPASS=\"reason\".\n", gate.OriginUnreadableFix)
	}
	os.Exit(1)
}

// runGateShow is `gate show`: print what each required layer has recorded for a sha. Read-only,
// never fail-open, and never creates a record: the only other verb that reveals whether a layer
// has recorded is `gate record`, which writes one. Exit 0 whenever the read succeeded (the chain
// being unsatisfied is an answer, not an error), 2 when it could not be read.
func runGateShow(args []string) {
	fs := flag.NewFlagSet("gate show", flag.ContinueOnError)
	sha := fs.String("sha", "", "sha to show (default: the repo's HEAD)")
	repo := fs.String("repo", "", "repo path (default: process cwd)")
	asJSON := fs.Bool("json", false, "emit the status as JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate show: %v\n", err)
		os.Exit(2)
	}
	// Read from the TARGET repo's records_dir, the one `gate record` writes to.
	g := reviewGateResolver()(*repo)
	st, err := g.Show(*sha, *repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	if *asJSON {
		out, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Print(formatGateStatus(st))
}

// warnUnboundReviewer notes, without refusing, that a reviewer-role name is self-asserted because
// $CLAUDE_AGENT_NAME is unset. Reviewer-pool consumers that share (or lack) an identity file their
// claim and their record under whatever each call was told, so the names drift apart and the
// independence guard then fires on the mismatch rather than on a real collision.
func warnUnboundReviewer(w io.Writer, by string, bound bool) {
	if bound {
		return
	}
	if r := gate.CanonicalRole(by); r != "cr" && r != "crit" {
		return
	}
	fmt.Fprintf(w, "leadkit: note: %q is a reviewer role but $CLAUDE_AGENT_NAME is unset, so the name is self-asserted. Start each reviewer-pool consumer with its own distinct CLAUDE_AGENT_NAME (e.g. cr-1, crit-1) so its claim, record and verdict all carry one name.\n", by)
}

// byNote says, after a record, whether the name it was recorded under was checked against anything.
func byNote(bound bool) string {
	if bound {
		return " [identity: $CLAUDE_AGENT_NAME]"
	}
	return " [name self-asserted: $CLAUDE_AGENT_NAME is unset]"
}

// formatGateStatus renders a gate.Status for a human.
func formatGateStatus(st gate.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "review gate for %s\nrecords: %s\n", st.SHA, st.RecordsDir)
	for _, l := range st.Layers {
		fmt.Fprintf(&b, "  %-12s %-17s", l.Layer, l.Verdict)
		if l.By != "" {
			fmt.Fprintf(&b, " by %s  %s", l.By, l.Ts)
			if !l.ByBound {
				b.WriteString("  (name self-asserted)")
			}
		}
		if l.Base != "" {
			fmt.Fprintf(&b, "  (covers %.12s...%.12s)", l.Base, st.SHA)
		}
		if l.SupersededVerdict != "" {
			fmt.Fprintf(&b, "  (superseded %s by %s)", l.SupersededVerdict, l.SupersededBy)
		}
		b.WriteString("\n")
		if l.Verdict != "MISSING" {
			if l.MutationEvidence == "" {
				b.WriteString("               mutation evidence: not recorded\n")
			} else {
				fmt.Fprintf(&b, "               mutation evidence: %s\n", gate.OneLine(l.MutationEvidence))
			}
		}
	}
	if st.ChainSatisfied {
		b.WriteString("chain: satisfied (every required layer is ready)\n")
	} else {
		fmt.Fprintf(&b, "chain: NOT satisfied; next to record: %s\n", st.Next)
	}
	fmt.Fprintf(&b, "scope: %s", st.Scope)
	switch st.Scope {
	case "out-of-scope":
		b.WriteString(" (`gate check` allows an ungoverned repo whatever is recorded; this is the record, not the decision)")
	case "unresolved":
		b.WriteString(" (the gate cannot confirm this repo's scope: `gate check` allows it in advisory mode and refuses it in enforce mode, whatever is recorded; this is the record, not the decision)")
	}
	b.WriteString("\n")
	return b.String()
}

// runGateCheckPush is `review-gate check-push`: the git pre-push hook entry point,
// reading the push protocol lines from stdin. Exits 1 if any ref is blocked.
func runGateCheckPush(args []string) {
	fs := flag.NewFlagSet("gate check-push", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate check-push: %v\n", err)
		os.Exit(2)
	}
	// A git pre-push hook runs in the repo being pushed, so the target is the cwd; resolve
	// it as the target (not via $LEADKIT_PROFILE, which names the SESSION's profile).
	g := targetGateResolver()("")
	// As in `gate check`: the hook is the git-level layer, so a push the gate cannot confirm is
	// refused where the target's push-gate guard is enforcing.
	g.RefuseUnverifiable = resolveTargetGuardModeDir("", config.GuardPushGate).Enforcing()
	v, err := g.CheckPushVerdict(os.Stdin, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit gate check-push: %v\n", err)
		os.Exit(2)
	}
	if !v.OK {
		fmt.Fprintf(os.Stderr, "leadkit pre-push: push refused by the review gate.\n")
		for _, r := range v.Reasons {
			fmt.Fprintf(os.Stderr, "  %s\n", r)
		}
		// An unreadable origin is not a missing review: "record the missing layers" would send the
		// reader to review a commit whose real problem is a remote name.
		unreadable := false
		for _, r := range v.Reasons {
			unreadable = unreadable || gate.OriginUnreadable(r)
		}
		if unreadable {
			fmt.Fprintf(os.Stderr, "%s, or set REVIEW_GATE_BYPASS=\"reason\" for a deliberate ungated push.\n", gate.OriginUnreadableFix)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Record the missing layers with `leadkit gate record --layer <code-review|critique> --verdict ready --mutants \"<mutant -> failing test>\"` (or `--no-mutant \"<why>\"`), or set REVIEW_GATE_BYPASS=\"reason\" for a deliberate ungated push.\n")
		os.Exit(1)
	}
	// A verdict of "out of scope" exempts the repo only if a profile was READ to reach it. When the
	// profile that governs this clone could not be (HOME changed, a profile that will not parse, a
	// .leadkit/ directory with no profile found), the built-in default stood in and governs
	// nothing; that is an unknown, not an exemption (ticket 204, the same seam the text hook uses).
	if v.OutOfScope {
		if cwd, err := os.Getwd(); err == nil {
			if why := hookProfileProblem(cwd); why != "" {
				if _, oc, _ := g.CheckUnreadableProfile(why); oc == gate.OutcomeUngated {
					fmt.Fprintf(os.Stderr, "leadkit pre-push: push refused: whether %s is gated cannot be known, because %s.\n"+
						"The built-in default that stands in for an unreadable profile governs nothing, and that is an unknown, not a decision to exempt the repo (fail-closed). "+
						"Fix the environment (HOME, the working directory), repair or add the profile (a leadkit.yaml), or remove the .leadkit/ directory if this is not a leadkit project; "+
						"or set REVIEW_GATE_BYPASS=\"reason\" for a deliberate ungated push.\n", filepath.Base(cwd), why)
					os.Exit(1)
				}
			} else if note := hookUnconfigured(cwd); note != "" {
				// A .leadkit/ directory and no profile anywhere (ticket 302): there is no gate to judge this
				// push against, so it is allowed, and the state is reported on this decision rather than
				// left for the operator to infer from the silence.
				fmt.Fprintln(os.Stderr, hook.UnconfiguredWarning(cwd, note))
			}
		}
	}
	os.Exit(0)
}
