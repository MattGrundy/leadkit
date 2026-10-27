package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"mgpp.dev/leadkit/internal/gh"
	"mgpp.dev/leadkit/internal/hook"
	"mgpp.dev/leadkit/internal/prcreate"
	"mgpp.dev/leadkit/internal/redact"
	"mgpp.dev/leadkit/internal/ticketlink"
)

// runPR is `leadkit pr <create|edit|merge>` — the governed PR command group. Every
// subcommand reuses internal/prcreate's authority resolver + Runner + gate seam; `merge`
// (the merge-wrapper fold) is the highest-risk verb — default-deny, fail-closed, and the
// only path that can perform an irreversible outward-facing forge write.
func runPR(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit pr <create|edit|merge> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		runPRCreate(args[1:])
	case "edit":
		runPREdit(args[1:])
	case "merge":
		runPRMerge(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "leadkit pr: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runPRCreate is `leadkit pr create --base <b> --head <branch> --title <t> --body-file
// <path> [--repo <dir>] [--draft] [--user-approved] [--dry-run]` — the governed replacement for the shell PR-create wrapper. It resolves the
// target repo's authority.<repo> policy from the loaded profile and ENFORCES it
// (default-deny) before shelling out to the forge CLI: no authority match → refuse;
// can_open_pr false/human → refuse; pr_draft_only → force --draft; required_gates
// unsatisfied → refuse unless --user-approved (an asserted human approval that the CLI
// records but cannot verify, not an enforced check). Exit 0 on success/dry-run, 1 on a
// governance refusal or a forge failure, 2 on a usage error, 3 when the PR opened but the ticket named
// by --task could not be updated (see recordCreatedPR).
func runPRCreate(args []string) {
	fs := flag.NewFlagSet("pr create", flag.ContinueOnError)
	base := fs.String("base", "", "target branch (required)")
	head := fs.String("head", "", "source branch (required)")
	title := fs.String("title", "", "PR/MR title (required)")
	bodyFile := fs.String("body-file", "", "path to the PR/MR body file (required)")
	repo := fs.String("repo", "", "local clone whose origin identifies the authority.<repo> entry (default: cwd)")
	draft := fs.Bool("draft", false, "open as draft (pr_draft_only forces this on regardless)")
	// NOT a control: this is a plain bool an agent can pass as easily as a human can,
	// so it asserts a human approval rather than proving one. Its value is the audit
	// record — the recordlog line says the open was authorised by an assertion instead
	// of a satisfied required_gates. The controls that cannot be self-granted are the
	// authority.<repo> policy and the PreToolUse hooks.
	userApproved := fs.Bool("user-approved", false, "assert that a human approved opening despite required_gates being unsatisfied (recorded in the audit line; not itself a check)")
	dryRun := fs.Bool("dry-run", false, "print the fully-resolved plan (authority verdict, draft decision, gate status, exact argv) and exit WITHOUT executing")
	task := fs.String("task", "", "the ticket this PR is for (id, name or path): once created, its URL is recorded on the ticket as trackers.pr, which is how a later merge finds the ticket again")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr create: %v\n", err)
		os.Exit(2)
	}
	if *base == "" || *head == "" || *title == "" || *bodyFile == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit pr create --base <b> --head <branch> --title <t> --body-file <path> [--task <ticket>] [--repo <dir>] [--draft] [--user-approved] [--dry-run]")
		os.Exit(2)
	}
	extra := append([]string(nil), fs.Args()...) // anything after `--` is handed to the decision, which refuses it (the passthrough allowlist is empty)

	prof := loadProfileOrDefaults()
	repoDir := *repo
	origin := gitOriginRemote(repoDir)

	// The ticket is resolved BEFORE the PR is opened: one that could not be written afterwards is
	// refused now, not discovered with the PR already public.
	var ticket ticketlink.Ref
	if strings.TrimSpace(*task) != "" {
		_, storeRoot := resolveQueueAndStore(prof)
		var tErr error
		if ticket, tErr = ticketlink.Resolve(*task, storeRoot); tErr == nil {
			tErr = ticketlink.PreflightPR(ticket)
		}
		if tErr != nil {
			fmt.Fprintf(os.Stderr, "leadkit pr create: REFUSED — --task %q: %v (no PR was opened)\n", *task, tErr)
			os.Exit(1)
		}
	}

	key, spec, hasAuthority := prcreate.ResolveAuthority(prof, origin)
	profileMissing := ""
	if !hasAuthority {
		profileMissing = profileMissingNote()
	}

	// The head branch's HEAD sha — what the gate is checked against ("gate BEFORE PR").
	headSHA, _ := gitRevParse(repoDir, *head)

	// Reuse gate.Gate.Check (scope/bypass/HEAD semantics) via the prcreate seam. The
	// gate is scoped to THIS governed repo (its resolved authority key), so a governed
	// repo that isn't in the project's default enforced_remotes still gets gated —
	// PR-create's scope IS the authority entry, not the push-gate's remote list.
	g := prGate(repoDir, prof, key)
	gateOK, gateReason := true, "no gate required"
	if hasAuthority {
		gateOK, gateReason = prcreate.GateReady(g, headSHA, repoDir, spec.RequiredGates)
		if why := requiredGatesProblem(prof, spec); why != "" {
			gateOK, gateReason = false, why
		}
	}

	// The gate above judges the head's own sha. The lineage check looks DOWN: every branch the head
	// is built on must be gated too, or a gated tip carries arbitrary ungated history through.
	lineageOK, lineageReason := true, "no gate required"
	if hasAuthority {
		lineageOK, lineageReason = prcreate.CheckLineage(prcreate.ExecLineageProbe{}, g, repoDir, headSHA, *base, spec.RequiredGates)
		if why := requiredGatesProblem(prof, spec); why != "" {
			lineageOK, lineageReason = false, why
		}
	}

	in := prcreate.DecideInput{
		OriginURL:      origin,
		AuthKey:        key,
		Spec:           spec,
		HasAuthority:   hasAuthority,
		ProfileMissing: profileMissing,
		Req: prcreate.Request{
			Base: *base, Head: *head, Title: *title, BodyFile: *bodyFile,
			Draft: *draft, Extra: extra,
		},
		UserApproved:  *userApproved,
		HeadSHA:       headSHA,
		GateOK:        gateOK,
		GateReason:    gateReason,
		LineageOK:     lineageOK,
		LineageReason: lineageReason,
	}

	decision, out, err := prcreate.Execute(in, *dryRun, prcreate.ExecRunner{})

	if *dryRun {
		fmt.Println("leadkit pr create — plan (dry-run; nothing executed):")
		fmt.Printf("  origin:       %s\n", redact.Origin(origin))
		if hasAuthority {
			fmt.Printf("  authority:    %s\n", key)
		} else {
			if profileMissing != "" {
				label := "profile NOT FOUND"
				if strings.Contains(profileMissing, "was not accepted") {
					label = "profile NOT ACCEPTED"
				}
				fmt.Printf("  authority:    (%s — default-deny; %s)\n", label, profileMissing)
			} else {
				fmt.Printf("  authority:    (no match in the profile — default-deny)\n")
			}
		}
		fmt.Printf("  head sha:     %s\n", headSHA)
		fmt.Printf("  gate:         ok=%v (%s)\n", gateOK, gateReason)
		fmt.Printf("  lineage:      ok=%v (%s)\n", lineageOK, lineageReason)
		fmt.Printf("  draft:        %v\n", decision.Draft)
		if ticket.ID != "" {
			fmt.Printf("  ticket:       %s (the PR's URL is recorded as trackers.%s once created)\n", ticket.ID, ticketlink.PRTracker)
		} else {
			fmt.Printf("  ticket:       (none — pass --task so the ticket records this PR)\n")
		}
		fmt.Printf("  verdict:      %s\n", decision.Reason)
		if decision.Allowed {
			fmt.Printf("  would run:    %s\n", strings.Join(decision.Argv, " "))
		} else {
			fmt.Printf("  would run:    (nothing — refused)\n")
		}
		os.Exit(0)
	}

	if !decision.Allowed {
		fmt.Fprintf(os.Stderr, "leadkit pr create: REFUSED — %s\n", decision.Reason)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr create: forge failed: %v\n%s", err, out)
		os.Exit(1)
	}
	if strings.TrimSpace(out) != "" {
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
	}
	fmt.Printf("leadkit pr create: %s\n", decision.Reason)
	if ticket.ID == "" {
		fmt.Println("leadkit pr create: no --task: no ticket records this PR, so a later merge cannot find the ticket and status can drift")
		os.Exit(0)
	}
	os.Exit(recordCreatedPR(os.Stdout, os.Stderr, ticket, out))
}

// recordCreatedPR is the second half of `pr create --task`: it reads the new PR's URL from the forge
// CLI's output and records it as trackers.pr on the ticket. It returns the process exit code. The
// PR is open and cannot be un-opened, so a failure here exits 3, not the 1 of a refusal, and says
// so: "PR opened, ticket not linked" must not be read as "nothing happened".
func recordCreatedPR(out, errOut io.Writer, ticket ticketlink.Ref, forgeOutput string) int {
	url := ticketlink.ExtractPRURL(forgeOutput)
	if url == "" {
		fmt.Fprintf(errOut, "leadkit pr create: the PR WAS OPENED but ticket %s was NOT updated: no PR URL in the forge's output, so there is nothing to record\n", ticket.ID)
		return 3
	}
	ch, err := ticketlink.RecordPR(ticket, url)
	if err != nil {
		fmt.Fprintf(errOut, "leadkit pr create: the PR WAS OPENED (%s) but ticket %s was NOT updated: %v\n", url, ticket.ID, err)
		fmt.Fprintf(errOut, "leadkit pr create: set it by hand: trackers: {pr: %s}, in %s\n", url, ticket.Path)
		return 3
	}
	if ch.OldPR != "" && ch.OldPR != ch.NewPR {
		fmt.Fprintf(out, "leadkit pr create: ticket %s: trackers.pr %s -> %s (a ticket holds the latest PR; the earlier one is replaced)\n", ticket.ID, ch.OldPR, ch.NewPR)
	} else {
		fmt.Fprintf(out, "leadkit pr create: ticket %s: trackers.pr = %s (status unchanged: %s)\n", ticket.ID, ch.NewPR, orNone(ch.Status))
	}
	return 0
}

// runPREdit is `leadkit pr edit <pr-number> [--title <t>] [--body-file <path>] [--repo
// <dir>] [--dry-run]` — the governed amend path, a SIBLING of
// pr create (NOT an overload). It resolves the target repo's authority.<repo> policy and
// ENFORCES it (default-deny) before shelling out to the forge CLI: no authority match →
// refuse; a no-op edit (neither --title nor --body-file) → refuse; can_open_pr
// false/human → refuse; owner_prs_only → the PR must be confirmed as authored by the
// authenticated forge user (fail-CLOSED: an unresolvable/errored lookup refuses). An
// omitted --title/--body-file is NOT sent (never blanks that field), and the edit path
// can NEVER undraft/ready/merge a PR. There is NO required_gates check (a metadata edit
// introduces no new sha). Exit 0 on success/dry-run, 1 on refusal/forge failure, 2 on a
// usage error.
func runPREdit(args []string) {
	// The PR number is the first positional argument (mirrors `gh pr edit <n>` and the
	// the legacy shell wrapper's <pr-number>); flags follow it.
	var prNumber string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		prNumber = rest[0]
		rest = rest[1:]
	}

	fs := flag.NewFlagSet("pr edit", flag.ContinueOnError)
	title := fs.String("title", "", "new PR/MR title (omitted → title left unchanged, never blanked)")
	bodyFile := fs.String("body-file", "", "path to the new PR/MR body file (omitted → body left unchanged, never blanked)")
	base := fs.String("base", "", "retarget the PR onto this branch — ONLY when the PR's current base is already merged into it (a strictly narrowing change); anything else is refused")
	repo := fs.String("repo", "", "local clone whose origin identifies the authority.<repo> entry (default: cwd)")
	dryRun := fs.Bool("dry-run", false, "print the fully-resolved plan (authority verdict, owner check, exact argv) and exit WITHOUT executing")
	if err := fs.Parse(rest); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr edit: %v\n", err)
		os.Exit(2)
	}
	if prNumber == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit pr edit <pr-number> [--title <t>] [--body-file <path>] [--base <branch>] [--repo <dir>] [--dry-run]")
		os.Exit(2)
	}

	// An omitted flag must NOT blank the field — track which were actually provided so
	// only those reach the forge argv (a no-op with neither is refused by DecideEdit).
	titleSet, bodySet, baseSet := false, false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "title":
			titleSet = true
		case "body-file":
			bodySet = true
		case "base":
			baseSet = true
		}
	})
	extra := append([]string(nil), fs.Args()...) // anything after `--` is handed to the decision, which refuses it (the passthrough allowlist is empty)

	prof := loadProfileOrDefaults()
	repoDir := *repo
	origin := gitOriginRemote(repoDir)

	key, spec, hasAuthority := prcreate.ResolveAuthority(prof, origin)
	profileMissing := ""
	if !hasAuthority {
		profileMissing = profileMissingNote()
	}

	// owner_prs_only: resolve ownership up front via the REUSED internal/hook author
	// lookup seam and feed the result into the pure DecideEdit (fail-closed — an
	// unresolvable forge or a failed lookup is NOT confirmed). Only consulted when the
	// policy is actually set, and only worth doing once we know the repo is governed.
	ownerConfirmed, ownerReason := true, "owner_prs_only not set"
	if hasAuthority && spec.OwnerPRsOnly && (titleSet || bodySet || baseSet) {
		forgeName, ferr := prcreate.ForgeName(origin, spec)
		if ferr != nil {
			ownerConfirmed, ownerReason = false, ferr.Error()
		} else {
			ownerConfirmed, ownerReason = hook.ConfirmPRAuthor(forgeName, repoDir, "", prNumber)
		}
	}

	// A retarget is judged on evidence gathered here (the forge's current base, git ancestry) and
	// fed to the pure DecideEdit; only for a governed repo, so an ungoverned one never reaches the forge.
	var baseCheck prcreate.BaseCheck
	if hasAuthority && baseSet {
		if forgeName, ferr := prcreate.ForgeName(origin, spec); ferr != nil {
			baseCheck = prcreate.BaseCheck{Reason: ferr.Error()}
		} else {
			baseCheck = prcreate.ResolveBaseCheck(prcreate.ExecProbe{}, forgeName, repoDir, prNumber, *base)
		}
	}

	// WHO may retarget: a registered live lead, read from the identity registry. The owner's
	// confirmation is asked later, by ExecuteEdit, at the moment of use.
	leadOK, leadReason := false, ""
	if hasAuthority && baseSet {
		leadOK, leadReason = callerIsLiveLead(prof)
	}

	in := prcreate.DecideEditInput{
		OriginURL:      origin,
		AuthKey:        key,
		Spec:           spec,
		HasAuthority:   hasAuthority,
		ProfileMissing: profileMissing,
		Req: prcreate.EditRequest{
			PRNumber: prNumber,
			Title:    *title, TitleSet: titleSet,
			BodyFile: *bodyFile, BodySet: bodySet,
			Base: *base, BaseSet: baseSet,
			Extra: extra,
		},
		OwnerConfirmed: ownerConfirmed,
		OwnerReason:    ownerReason,
		BaseCheck:      baseCheck,
		LeadOK:         leadOK,
		LeadReason:     leadReason,
		ConfirmAtUse:   confirmRetargetAtTerminal,
	}

	decision, out, err := prcreate.ExecuteEdit(in, *dryRun, prcreate.ExecRunner{})

	if *dryRun {
		fmt.Println("leadkit pr edit — plan (dry-run; nothing executed):")
		fmt.Printf("  origin:       %s\n", redact.Origin(origin))
		if hasAuthority {
			fmt.Printf("  authority:    %s\n", key)
		} else {
			fmt.Printf("  authority:    (no match — default-deny)\n")
		}
		fmt.Printf("  pr number:    %s\n", prNumber)
		fmt.Printf("  title set:    %v\n", titleSet)
		fmt.Printf("  body set:     %v\n", bodySet)
		if baseSet {
			fmt.Printf("  retarget:     %s -> %s (current base ancestor of new: %v, resolved: %v)\n", baseCheck.CurrentBase, *base, baseCheck.Ancestor, baseCheck.Resolved)
			fmt.Printf("  lead:         ok=%v (%s)\n", leadOK, leadReason)
			fmt.Printf("  confirm:      the owner is asked at a terminal when this runs, and must type %s\n", prNumber)
		}
		if hasAuthority && spec.OwnerPRsOnly {
			fmt.Printf("  owner check:  ok=%v (%s)\n", ownerConfirmed, ownerReason)
		} else {
			fmt.Printf("  owner check:  (owner_prs_only not set)\n")
		}
		fmt.Printf("  verdict:      %s\n", decision.Reason)
		if decision.Allowed {
			fmt.Printf("  would run:    %s\n", strings.Join(decision.Argv, " "))
		} else {
			fmt.Printf("  would run:    (nothing — refused)\n")
		}
		os.Exit(0)
	}

	if !decision.Allowed {
		fmt.Fprintf(os.Stderr, "leadkit pr edit: REFUSED — %s\n", decision.Reason)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr edit: forge failed: %v\n%s", err, out)
		os.Exit(1)
	}
	if strings.TrimSpace(out) != "" {
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
	}
	fmt.Printf("leadkit pr edit: %s\n", decision.Reason)
	os.Exit(0)
}

// runPRMerge is `leadkit pr merge <pr-number> [--squash|--merge|--rebase]
// [--user-approved] [--repo <dir>] [--dry-run]` — the governed
// fold of the shell merge wrapper, a SIBLING of pr create/edit (NOT an overload). It is the
// HIGHEST-RISK verb: a merge is irreversible + outward-facing, so it is default-DENY and
// fails CLOSED on any unreadable input. The CLI resolves ALL I/O (PR metadata, the
// skip-ci HEAD check, the CI-green check, the review-gate read, and — for squash — the
// skip-ci-stripped squash message) and feeds it into the PURE prcreate.DecideMerge; the
// forge runner is invoked only for an ALLOWED, non-dry-run decision. Governance order:
// no authority match → refuse; can_merge false/human → refuse; empty merge_grant AND no
// --user-approved → refuse; then the exact merge ladder (OPEN + not-draft +
// MERGEABLE + CLEAN → no skip-ci HEAD → CI all green → review-gate 2/2). --user-approved
// substitutes for the review-gate 2/2 ONLY — never for OPEN/CLEAN/skip-ci/CI-green. Exit
// 0 on success/dry-run, 1 on refusal/forge failure, 2 on a usage error, 3 when the PR merged but the
// ticket it is linked to (found by its trackers.pr, or named by --task) could not be updated (see
// linkMergedTicket).
func runPRMerge(args []string) {
	fs := flag.NewFlagSet("pr merge", flag.ContinueOnError)
	pr := fs.String("pr", "", "the PR/MR number to merge (required)")
	methodFlag := fs.String("method", "", "merge method: squash|merge|rebase (default squash, or merge when branches are stacked on the PR; an explicit squash or rebase is refused then)")
	// Same caveat as pr create: an assertion, not a check (see the comment there). It
	// narrows the gate ladder by one rung and names itself in the merge_gate audit line;
	// it cannot relax OPEN/CLEAN/skip-ci/CI-green, and can_merge: human still refuses.
	userApproved := fs.Bool("user-approved", false, "assert a human approval in place of the review-gate 2/2 ONLY — never for OPEN/CLEAN/skip-ci/CI-green")
	repo := fs.String("repo", "", "local clone whose origin identifies the authority.<repo> entry (default: cwd)")
	dryRun := fs.Bool("dry-run", false, "print the fully-resolved plan (governance verdict, PR state, gate, exact argv) and exit WITHOUT merging")
	task := fs.String("task", "", "override: the ticket this merge is for (id, name or path). By default it is found as the ticket recording this PR's URL (`pr create --task`). The merge commit gets a `Ticket: <id>` trailer, and once merged the ticket records the sha it landed as; its status is never changed")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr merge: %v\n", err)
		os.Exit(2)
	}
	if *pr == "" {
		fmt.Fprintln(os.Stderr, "usage: leadkit pr merge --pr <number> [--task <ticket>] [--method squash|merge|rebase] [--user-approved] [--repo <dir>] [--dry-run]")
		os.Exit(2)
	}
	prNumber := *pr
	method := strings.ToLower(strings.TrimSpace(*methodFlag))
	if method != "" && method != "squash" && method != "merge" && method != "rebase" {
		fmt.Fprintf(os.Stderr, "leadkit pr merge: unknown --method %q (want squash|merge|rebase)\n", *methodFlag)
		os.Exit(2)
	}
	extra := append([]string(nil), fs.Args()...) // anything after `--` is handed to the decision, which refuses it (the passthrough allowlist is empty)

	prof := loadProfileOrDefaults()
	repoDir := *repo
	origin := gitOriginRemote(repoDir)
	key, spec, hasAuthority := prcreate.ResolveAuthority(prof, origin)
	profileMissing := ""
	if !hasAuthority {
		profileMissing = profileMissingNote()
	}
	ownerName := prcreate.OwnerName(origin)

	// The ticket is resolved BEFORE the merge, which is irreversible: a ticket that could not be
	// written afterwards is refused now, not discovered with the PR already merged. An explicit
	// --task is the override; without it the ticket is the one that recorded this PR (see below).
	var ticket ticketlink.Ref
	ticketFrom := ""
	_, storeRoot := resolveQueueAndStore(prof)
	if strings.TrimSpace(*task) != "" {
		var tErr error
		if ticket, tErr = ticketlink.Resolve(*task, storeRoot); tErr == nil {
			tErr = ticketlink.PreflightPR(ticket)
		}
		if tErr != nil {
			fmt.Fprintf(os.Stderr, "leadkit pr merge: REFUSED — --task %q: %v (nothing was merged)\n", *task, tErr)
			os.Exit(1)
		}
		ticketFrom = "--task"
	}

	var stack *prcreate.Dependents
	in := prcreate.DecideMergeInput{
		OriginURL:      origin,
		AuthKey:        key,
		Spec:           spec,
		HasAuthority:   hasAuthority,
		ProfileMissing: profileMissing,
		Req:            prcreate.MergeRequest{PRNumber: prNumber, Method: method, Extra: extra, Ticket: ticket.ID},
		UserApproved:   *userApproved,
	}

	// Resolve ALL forge I/O up front — ONLY for a governed repo (never touch the forge
	// for an ungoverned one; DecideMerge default-denies that anyway). Every read carries
	// a success flag so an unreadable input fails CLOSED in the pure resolver.
	if hasAuthority {
		meta, mErr := gh.FetchPRMeta(ownerName, prNumber)
		if mErr != nil {
			in.MetaReason = mErr.Error()
		} else {
			in.MetaOK = true
			in.State = prcreate.PRState{
				State:            meta.State,
				IsDraft:          meta.IsDraft,
				Mergeable:        meta.Mergeable,
				MergeStateStatus: meta.MergeStateStatus,
				HeadSHA:          meta.HeadRefOID,
				Title:            meta.Title,
				BaseRefName:      meta.BaseRefName,
				URL:              meta.URL,
			}
			// skip-ci HEAD check (fail-closed on an unreadable HEAD).
			if skip, sErr := gh.HeadCarriesSkipCI(ownerName, meta.HeadRefOID); sErr != nil {
				in.SkipCIReason = sErr.Error()
			} else {
				in.SkipCIChecked, in.HeadSkipCI = true, skip
			}
			// CI checks all green (`gh pr checks` exit 0).
			green, ciOut, _ := gh.CIChecksGreen(ownerName, prNumber)
			in.CIGreen = green
			if !green {
				in.CIReason = ciSnippet(ciOut)
			}
			// review-gate 2/2 (code-review+critique) on the PR HEAD sha — reuse the
			// prcreate GateReady seam, scoped to THIS governed repo's authority key.
			g := prGate(repoDir, prof, key)
			in.GateOK, in.GateReason = prcreate.GateReady(g, meta.HeadRefOID, repoDir, spec.RequiredGates)
			if why := requiredGatesProblem(prof, spec); why != "" {
				in.GateOK, in.GateReason = false, why
			}
			// ...and the commits beneath that sha: a merge lands all of them, and the gate on the head says
			// nothing about them. An unreadable base is refused, never skipped.
			in.LineageOK, in.LineageReason = prcreate.CheckLineage(prcreate.ExecLineageProbe{}, g, repoDir, meta.HeadRefOID, meta.BaseRefName, spec.RequiredGates)
			if why := requiredGatesProblem(prof, spec); why != "" {
				in.LineageOK, in.LineageReason = false, why
			}
			// ...and the branches stacked on it: a squash or rebase rewrites every commit in base..head, so
			// each such branch would be left conflicting. Unreadable is not "none".
			if dep, dErr := (prcreate.ExecDependentProbe{}).Dependents(repoDir, meta.HeadRefOID, meta.HeadRefName, meta.BaseRefName); dErr != nil {
				in.DependentsReason = dErr.Error()
			} else {
				in.DependentsOK, in.Dependents = true, dep.Branches
				stack = &dep
			}
			// squash message compose (squash, or no method asked for) — CLI-composed + skip-ci-stripped,
			// fail-closed if the compose fails (never delegate the strip to gh's default).
			if method == "" || method == "squash" {
				if subject, body, cErr := gh.ComposeMerge(ownerName, prNumber); cErr != nil {
					in.ComposeReason = cErr.Error()
				} else {
					in.ComposeOK, in.Subject, in.Body = true, subject, body
				}
			}
		}
	}

	// The tickets that recorded this PR's URL when it was created. An exact match on the whole URL, so
	// the lookup is not a guess; it fails CLOSED. Two claimants refuse the merge (a link to the wrong
	// ticket is worse than none), and so does a claimant that cannot be written, or a store that cannot
	// be read: the check exists for exactly the stores that are not tidy.
	if in.MetaOK && in.State.URL != "" {
		claims, fErr := ticketlink.FindByPR(storeRoot, in.State.URL)
		if fErr != nil {
			fmt.Fprintf(os.Stderr, "leadkit pr merge: REFUSED — could not look for the ticket recording %s: %v (nothing was merged)\n", in.State.URL, fErr)
			os.Exit(1)
		}
		if ticket.ID == "" {
			switch {
			case len(claims) > 1:
				var ids []string
				for _, c := range claims {
					ids = append(ids, c.ID)
				}
				fmt.Fprintf(os.Stderr, "leadkit pr merge: REFUSED — tickets %s all record %s as their PR; pass --task to say which one this merge is for (nothing was merged)\n", strings.Join(ids, ", "), in.State.URL)
				os.Exit(1)
			case len(claims) == 1:
				c := claims[0]
				pErr := c.Err
				if pErr == nil {
					pErr = ticketlink.PreflightPR(c.Ref)
				}
				if pErr != nil {
					fmt.Fprintf(os.Stderr, "leadkit pr merge: REFUSED — ticket %s records this PR but %v (nothing was merged)\n", c.ID, pErr)
					os.Exit(1)
				}
				ticket, ticketFrom = c.Ref, "trackers.pr"
				in.Req.Ticket = ticket.ID
			}
		} else {
			// An override that names a ticket other than the one(s) recording this PR leaves two claimants
			// once it records trackers.pr, and the NEXT merge of this PR would refuse.
			for _, c := range claims {
				if c.ID != ticket.ID {
					fmt.Fprintf(os.Stderr, "leadkit pr merge: warning: ticket %s already records this PR; --task %s will record it on %s too, and a second merge of this PR will then refuse until one is removed\n", c.ID, ticket.ID, ticket.ID)
				}
			}
		}
	}

	decision, out, err := prcreate.ExecuteMerge(in, *dryRun, prcreate.ExecRunner{})

	if *dryRun {
		fmt.Println("leadkit pr merge — plan (dry-run; nothing merged):")
		fmt.Printf("  origin:       %s\n", redact.Origin(origin))
		if hasAuthority {
			fmt.Printf("  authority:    %s\n", key)
		} else {
			fmt.Printf("  authority:    (no match — default-deny)\n")
		}
		fmt.Printf("  pr number:    %s\n", prNumber)
		if ticket.ID != "" {
			fmt.Printf("  ticket:       %s, from %s (trailers %q; once merged the ticket records sha = the merge commit; its status is not changed)\n", ticket.ID, ticketFrom, ticketlink.TrailerBlock(ticket.ID))
		} else {
			fmt.Printf("  ticket:       (none: no ticket records this PR's URL, and no --task; the merge is still marked %q)\n", ticketlink.Marker())
		}
		chosen, methodNote, _ := prcreate.ResolveMergeMethod(in)
		fmt.Printf("  method:       %s\n", chosen)
		if methodNote != "" {
			fmt.Printf("  method note:  %s\n", methodNote)
		}
		if hasAuthority {
			fmt.Printf("  can_merge:    allow=%v human=%v\n", spec.CanMerge.Allow, spec.CanMerge.Human)
			grant := strings.TrimSpace(spec.MergeGrant)
			if grant == "" {
				grant = "(empty)"
			}
			fmt.Printf("  merge_grant:  %s (user-approved=%v)\n", grant, *userApproved)
			if in.MetaOK {
				fmt.Printf("  pr state:     state=%s draft=%v mergeable=%s mergeStateStatus=%s\n",
					in.State.State, in.State.IsDraft, in.State.Mergeable, in.State.MergeStateStatus)
				fmt.Printf("  head sha:     %s\n", in.State.HeadSHA)
				fmt.Printf("  skip-ci HEAD: checked=%v carries=%v\n", in.SkipCIChecked, in.HeadSkipCI)
				fmt.Printf("  ci green:     %v\n", in.CIGreen)
				fmt.Printf("  review-gate:  %s\n", formatMergeGateLine(in))
				fmt.Printf("  stacked:      %s\n", formatStackedLine(in, stack))
				if chosen == "squash" {
					fmt.Printf("  squash msg:   composed=%v\n", in.ComposeOK)
				}
			} else {
				fmt.Printf("  pr state:     UNREADABLE — %s (fail-closed)\n", reasonOrUnread(in.MetaReason))
			}
		}
		fmt.Printf("  verdict:      %s\n", decision.Reason)
		if decision.Allowed {
			fmt.Printf("  would run:    %s\n", strings.Join(decision.Argv, " "))
		} else {
			fmt.Printf("  would run:    (nothing — refused)\n")
		}
		os.Exit(0)
	}

	if !decision.Allowed {
		fmt.Fprintf(os.Stderr, "leadkit pr merge: REFUSED — %s\n", decision.Reason)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit pr merge: forge failed: %v\n%s", err, out)
		os.Exit(1)
	}
	if strings.TrimSpace(out) != "" {
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
	}
	// Surface the review-gate basis on the ACTUAL merge too — every REAL merge visibly
	// records WHAT authorized it (a live 2/2 or a --user-approved substitution), not only
	// the dry-run plan. Allowed path only; a refusal already printed its reason above.
	fmt.Printf("leadkit pr merge: review-gate: %s for HEAD %s\n", formatMergeGateLine(in), in.State.HeadSHA)
	fmt.Printf("leadkit pr merge: %s\n", decision.Reason)
	if ticket.ID == "" {
		fmt.Println("leadkit pr merge: linked to no ticket (none records this PR, and no --task): nothing ties a ticket to the commit that satisfied it (the merge is still marked " + ticketlink.Marker() + ")")
		os.Exit(0)
	}
	os.Exit(linkMergedTicket(os.Stdout, os.Stderr, ticket, ownerName, prNumber, in.State.HeadSHA, in.State.URL))
}

// linkMergedTicket is the second half of a ticket-linked `pr merge`, run once the forge has merged:
// it reads which commit the PR landed as, checks the marker and Ticket lines actually rode on it, and
// records that commit on the ticket. It never changes the ticket's status: merged is a fact, complete
// is a decision. It returns the process exit code. The merge cannot be undone, so a failure here is
// reported as exactly that, and exits 3 rather than the 1 of a refusal: the caller must not read
// "PR merged, ticket not linked" as "nothing happened".
func linkMergedTicket(out, errOut io.Writer, ticket ticketlink.Ref, ownerName, prNumber, reviewedSHA, prURL string) int {
	unlinked := func(why string) int {
		fmt.Fprintf(errOut, "leadkit pr merge: PR #%s IS MERGED but ticket %s was NOT updated: %s\n", prNumber, ticket.ID, why)
		fmt.Fprintf(errOut, "leadkit pr merge: set it by hand: sha: <the merge commit>, in %s\n", ticket.Path)
		return 3
	}
	landed, err := gh.FetchMergeCommit(ownerName, prNumber)
	if err != nil {
		return unlinked("cannot read which commit it merged as: " + err.Error())
	}
	if landed.State != "MERGED" || landed.OID == "" {
		return unlinked(fmt.Sprintf("the forge reports state=%q with no merge commit yet (a merge queue?), so there is no sha to record", landed.State))
	}
	if msg, mErr := gh.CommitMessage(ownerName, landed.OID); mErr != nil {
		fmt.Fprintf(errOut, "leadkit pr merge: warning: could not read merge commit %s to confirm its trailers: %v\n", landed.OID, mErr)
	} else if !ticketlink.HasTrailer(msg, ticket.ID) || !ticketlink.HasMarker(msg) {
		fmt.Fprintf(errOut, "leadkit pr merge: warning: merge commit %s is missing %q or %q, so `git log --grep` will not find it\n", landed.OID, ticketlink.Marker(), ticketlink.Trailer(ticket.ID))
	}
	ch, err := ticketlink.RecordMerge(ticket, landed.OID, reviewedSHA, prURL)
	if err != nil {
		return unlinked(err.Error())
	}
	fmt.Fprintf(out, "leadkit pr merge: ticket %s: sha %s -> %s, trackers.pr = %s; status unchanged (%s): closing the ticket is a separate decision\n", ticket.ID, orNone(ch.OldSHA), ch.NewSHA, orNone(ch.NewPR), orNone(ch.Status))
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// formatStackedLine renders what the stack check found, beside the method it bears on. A squash or
// rebase leaves every branch listed here conflicting, so the plan says so whatever method was chosen.
func formatStackedLine(in prcreate.DecideMergeInput, d *prcreate.Dependents) string {
	if !in.DependentsOK {
		return fmt.Sprintf("UNKNOWN — %s (a squash or rebase is refused until this resolves)", reasonOrUnread(in.DependentsReason))
	}
	if len(in.Dependents) == 0 {
		return "none among the refs this clone has fetched"
	}
	anchor := ""
	if d != nil && d.Anchor != "" {
		anchor = fmt.Sprintf(", measured from the oldest of its %d commit(s), %.8s", d.Commits, d.Anchor)
	}
	return fmt.Sprintf("%d branch(es) carry its commits%s; a squash or rebase would leave them conflicting: %s",
		len(in.Dependents), anchor, strings.Join(in.Dependents, ", "))
}

// ciSnippet trims `gh pr checks` output to a compact one-ish-line diagnostic for a
// not-green refusal reason (the full table would bloat the message).
func ciSnippet(out string) string {
	s := strings.TrimSpace(out)
	if s == "" {
		return "gh pr checks did not exit clean"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return strings.ReplaceAll(s, "\n", " | ")
}

// formatMergeGateLine renders the review-gate verdict for a merge — the SINGLE pure
// formatter shared by the dry-run plan and the ACTUAL (non-dry-run) allowed-merge basis
// line, so both output paths name the code-review+critique gate identically and a live
// merge is never ambiguous about WHAT authorized it. When --user-approved substituted for
// the gate it says SUBSTITUTED and still shows the underlying auto-gate ok value (so a
// human substitution is never mistaken for a live 2/2); otherwise it shows the auto-gate
// ok=<v> with its reason. No I/O — directly unit-testable.
func formatMergeGateLine(in prcreate.DecideMergeInput) string {
	if in.UserApproved {
		return fmt.Sprintf("SUBSTITUTED by --user-approved (auto-gate ok=%v (%s))",
			in.GateOK, gateReasonOr(in.GateReason))
	}
	return fmt.Sprintf("ok=%v (%s)", in.GateOK, gateReasonOr(in.GateReason))
}

// gateReasonOr renders an empty gate reason (e.g. a green gate that reported no detail)
// as a descriptive fallback so the review-gate line never dangles an empty clause.
func gateReasonOr(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "review-gate 2/2 (code-review+critique)"
	}
	return reason
}

// reasonOrUnread renders an empty metadata-fetch reason as a generic fallback in the
// dry-run plan.
func reasonOrUnread(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "forge metadata fetch failed"
	}
	return reason
}

// gitRevParse is a read-only `git -C <repo> rev-parse <ref>` — "" and false on any
// failure (unknown ref, no repo, git not found), the same non-fatal posture
// gitOriginRemote takes. Callers pass the result to the gate as the head sha.
func gitRevParse(repo, ref string) (string, bool) {
	args := []string{"rev-parse", ref}
	if repo != "" {
		args = append([]string{"-C", repo}, args...)
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
