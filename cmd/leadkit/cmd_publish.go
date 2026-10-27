package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"mgpp.dev/leadkit/internal/forge"
	"mgpp.dev/leadkit/internal/publish"
	"mgpp.dev/leadkit/internal/scrubterms"
)

const publishUsage = "usage: leadkit publish <check|prepare> [flags]"

// runPublish is `leadkit publish`, the committed form of the internal-to-public check.
func runPublish(args []string) {
	os.Exit(publishCmd(args, publishDeps{
		git: forge.ExecRunner{Timeout: 5 * time.Minute},
		gh:  forge.ExecRunner{},
		now: time.Now,
	}, os.Stdout, os.Stderr))
}

// publishDeps are the seams publishCmd reads through, so tests drive it without a live gh.
type publishDeps struct {
	git, gh forge.Runner
	now     func() time.Time
}

// publishCmd is `leadkit publish check|prepare`.
//
//	check    verify the tree at --ref against the scrub terms and the exclusion manifest; write nothing
//	prepare  check, then build the public repository as one new commit (--out, the first release)
//	         or make a fresh clone of it equal the source in one new commit (--base), and re-check inside it
//
// There is deliberately no verb or flag that creates a repository, adds a remote or sends a
// branch anywhere: publishing is owner-direct (tickets 198, 267, 276), and publish.Guard refuses
// it besides.
//
// Exit 0: clean (check) or prepared (prepare). 1: a refusal or a finding, nothing is ready.
// 2: usage.
func publishCmd(args []string, d publishDeps, stdout, stderr io.Writer) int {
	if len(args) < 1 || (args[0] != "check" && args[0] != "prepare") {
		fmt.Fprintln(stderr, publishUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("publish "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o publish.Options
	fs.StringVar(&o.Repo, "repo", ".", "the source checkout")
	fs.StringVar(&o.Ref, "ref", "HEAD", "the commit to publish")
	fs.StringVar(&o.Manifest, "manifest", publish.DefaultManifest, "the exclusion manifest, as a path within the tree")
	fs.BoolVar(&o.RequireCleanHistory, "require-clean-history", false,
		"fail when the source history's messages or identities match (off by default: a private history is expected to)")
	termsFile := fs.String("terms-file", "", "the scrub term file (default $"+scrubterms.EnvVar+", else the standard path)")
	termsRepo := fs.String("terms-repo", "", "owner/name that holds the "+scrubterms.VarName+" GitHub variable (default: gh infers it from the working directory)")
	if verb == "prepare" {
		fs.StringVar(&o.Out, "out", "", "a new directory to build the public repository in (the first release)")
		fs.StringVar(&o.Base, "base", "", "a fresh clone of the existing public repository: one new commit that makes its tree equal the source tree")
		fs.StringVar(&o.Message, "message", "", "the commit message (default: generated)")
		fs.StringVar(&o.AuthorName, "author-name", "", "the commit's author and committer name (default: a generic one)")
		fs.StringVar(&o.AuthorEmail, "author-email", "", "the commit's author and committer address (default: a generic one)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, publishUsage)
		return 2
	}

	deps := publish.Deps{
		Git: d.git, GH: d.gh, Now: d.now,
		Terms: scrubterms.ResolveOptions{Path: *termsFile, Repo: *termsRepo},
	}
	var rep publish.Report
	var err error
	if verb == "check" {
		rep, err = publish.Check(o, deps)
	} else {
		rep, err = publish.Prepare(o, deps)
	}
	if err != nil {
		fmt.Fprintf(stderr, "leadkit publish %s: REFUSED — %v\n", verb, err)
		return 1
	}
	rep.Write(stdout)
	if !rep.OK() {
		return 1
	}
	return 0
}
