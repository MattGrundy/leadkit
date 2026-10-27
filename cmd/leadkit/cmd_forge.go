package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/forge"
	"mgpp.dev/leadkit/internal/prcreate"
)

// runForge is `leadkit forge <protect>` — Layer 0, forge-side branch protection
// (docs/design/agent-sandboxing-and-enforcement.md).
func runForge(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: leadkit forge <protect> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "protect":
		os.Exit(forgeProtect(args[1:], forgeDeps{
			prof:   loadProfileOrDefaults,
			origin: gitOriginRemote,
			runner: forge.ExecRunner{},
		}, os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "leadkit forge: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// forgeDeps are the seams forgeProtect reads through, so tests drive it without a real
// profile, git checkout, or gh/glab.
type forgeDeps struct {
	prof   func() *config.Profile
	origin func(repoDir string) string
	runner forge.Runner
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// forgeProtect is `leadkit forge protect [--repo <dir>] [--branch <b>]... [--require-check
// <ctx>]... [--apply --confirm]`. It makes each named branch (default: the authority
// entry's protected_branches, else the repo's default branch) require a pull/merge
// request, reject force-push and deletion, and bind admins.
//
// This is an outward-facing forge WRITE: default-deny (an authority.<repo> entry must
// match the origin), dry-run by default, and only `--apply --confirm` together writes; a
// bare --apply refuses (confirmGuard). A write that would remove any existing protection
// is refused even under --confirm. A 2xx from the write is not proof: the read-back must
// show the branch verified and nothing it had before gone.
//
// Exit 0: every branch verified or (dry-run) planned. 1: a refusal, a failed write, or a
// branch that did not verify after the write. 2: usage.
func forgeProtect(args []string, d forgeDeps, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("forge protect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "local clone whose origin identifies the authority.<repo> entry (default: cwd)")
	var branches, checks listFlag
	fs.Var(&branches, "branch", "exact branch to protect (repeatable; default: authority.<repo>.protected_branches, else the repo's default branch)")
	fs.Var(&checks, "require-check", "status-check context that must pass before merge (repeatable; GitHub only)")
	apply := fs.Bool("apply", false, "write the protection (requires --confirm too; default: dry-run)")
	confirm := fs.Bool("confirm", false, "required alongside --apply to actually write — --apply alone always refuses")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: leadkit forge protect [--repo <dir>] [--branch <b>]... [--require-check <ctx>]... [--apply --confirm]")
		return 2
	}

	prof := d.prof()
	origin := d.origin(*repo)
	key, spec, hasAuthority := prcreate.ResolveAuthority(prof, origin)

	// Authority first, before any forge I/O: an ungoverned repo is never even read.
	if !hasAuthority {
		dec := forge.Decide(forge.DecideInput{OriginURL: origin})
		fmt.Fprintf(stderr, "leadkit forge protect: REFUSED — %s\n", dec.Reason)
		return 1
	}
	target, terr := forge.ResolveTarget(origin, spec)
	if terr != nil {
		dec := forge.Decide(forge.DecideInput{OriginURL: origin, AuthKey: key, HasAuthority: true, TargetErr: terr})
		fmt.Fprintf(stderr, "leadkit forge protect: REFUSED — %s\n", dec.Reason)
		return 1
	}

	if len(branches) > 0 {
		spec.ProtectedBranches = branches
	}
	names, err := forge.Branches(d.runner, target, spec)
	if err != nil {
		fmt.Fprintf(stderr, "leadkit forge protect: REFUSED — %v\n", err)
		return 1
	}

	guard := confirmGuard(*apply, *confirm)
	fmt.Fprintf(stdout, "leadkit forge protect — %s %s%s\n", target.Kind, target.Repo, map[bool]string{true: "", false: " (dry-run; nothing written)"}[guard == guardMutate])
	fmt.Fprintf(stdout, "  authority: %s\n", key)

	failed := false
	pending := 0
	for _, b := range names {
		cur := forge.Inspect(d.runner, target, b)
		dec := forge.Decide(forge.DecideInput{
			OriginURL: origin, AuthKey: key, HasAuthority: true,
			Target: target, Current: cur, Opts: forge.Options{RequireChecks: checks},
		})
		fmt.Fprintf(stdout, "  [%s] %s\n", cur.State, dec.Reason)
		printPlan(stdout, dec.Plan)
		if !dec.Allowed {
			failed = true
			continue
		}
		if dec.Noop {
			continue
		}
		pending++
		fmt.Fprintf(stdout, "    audit trail, would run: %s   (JSON body on stdin: %s)\n", strings.Join(dec.Call.Argv, " "), dec.Call.Body)
		if guard != guardMutate {
			continue
		}
		if err := forge.Apply(d.runner, *dec.Call); err != nil {
			fmt.Fprintf(stdout, "    WRITE FAILED: %v\n", err)
			failed = true
			continue
		}
		after := forge.Inspect(d.runner, target, b)
		if problems := forge.ReadBackProblems(cur, after); len(problems) > 0 {
			fmt.Fprintf(stdout, "    wrote, but the read-back disagrees with the plan:\n")
			for _, p := range problems {
				fmt.Fprintf(stdout, "      - %s\n", p)
			}
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "    applied and verified server-side: %s\n", after.Detail)
	}

	switch {
	case failed:
		fmt.Fprintln(stdout, "FAILED — see above; protection is NOT confirmed for every branch.")
		return 1
	case guard == guardRefused:
		fmt.Fprintf(stdout, "REFUSED — --apply without --confirm mutates nothing. This would change %d branch(es); re-run with `--apply --confirm` to execute.\n", pending)
		return 1
	case guard == guardDryRun && pending > 0:
		fmt.Fprintln(stdout, "(dry-run — nothing written; re-run with --apply --confirm)")
	}
	return 0
}

// printPlan renders a Plan for a person: the branch's protection now, then every change
// with removals first, then one summary line that is loud when anything would be lost.
// The JSON body is printed separately as an audit trail; it is not the warning.
func printPlan(w io.Writer, p *forge.Plan) {
	if p == nil {
		return
	}
	fmt.Fprintf(w, "    protected by now: %s\n", p.Current)
	if len(p.Changes) == 0 && p.Why == "" {
		return
	}
	for _, c := range p.Changes {
		fmt.Fprintf(w, "      %s\n", c)
	}
	fmt.Fprintf(w, "    %s\n", p.Summary())
	if p.Why != "" {
		fmt.Fprintf(w, "    why: %s\n", p.Why)
	}
}
