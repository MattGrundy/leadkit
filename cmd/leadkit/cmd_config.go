package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/install"
)

// runConfig dispatches the config subcommand group (show|reference|init).
func runConfig(args []string) {
	if len(args) >= 1 && args[0] == "reference" {
		runConfigReference(args[1:])
		return
	}
	if len(args) >= 1 && args[0] == "path" {
		cwd, _ := os.Getwd()
		fmt.Print(config.FormatPath(config.ResolveLocations(cwd)))
		return
	}
	if len(args) >= 1 && args[0] == "init" {
		runConfigInit(args[1:])
		return
	}
	if len(args) < 1 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: leadkit config <show [--show-origin] | path | reference [--markdown] | init [--external] [--project <slug>]>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	showOrigin := fs.Bool("show-origin", false, "annotate each key with its layer (user/team/project/default) and lock status")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config show: %v\n", err)
		os.Exit(2)
	}

	if *showOrigin {
		prof, path, origins, err := config.LoadWithOrigin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit config show: %v\n", err)
			os.Exit(1)
		}
		out, err := config.FormatShowOrigin(prof, path, origins)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit config show: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(out)
		printDefaultsHint(path)
		fmt.Print(config.FormatEffectivePushWhitelist(prof, gitOriginRemote("")))
		return
	}

	prof, path, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config show: %v\n", err)
		os.Exit(1)
	}
	out, err := config.FormatShow(prof, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config show: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(out)
	printDefaultsHint(path)
	fmt.Print(config.FormatEffectivePushWhitelist(prof, gitOriginRemote("")))
}

// printDefaultsHint, when no config file resolved (defaults), names the external file
// leadkit would read if created and the slug-fallback caveat.
func printDefaultsHint(path string) {
	if path != "" {
		return
	}
	cwd, _ := os.Getwd()
	if hint := config.ResolveLocations(cwd).DefaultsHint(); hint != "" {
		fmt.Printf("# %s\n", hint)
	}
}

// runConfigInit writes a starter leadkit config (idempotent — never clobbers an existing
// file). --external (the recommended placement for a repo that keeps NO committed config)
// writes ~/.claude/leadkit/config/<slug>.yaml, keyed by the repo's cwd-honest slug;
// without it, an in-repo ./leadkit.yaml at the cwd. --project <slug> overrides the slug
// (defaults to the session's cwd-honest identity slug). The starter mirrors the per-project
// template; authority grants are deliberately absent (owner-direct).
func runConfigInit(args []string) {
	fs := flag.NewFlagSet("config init", flag.ContinueOnError)
	external := fs.Bool("external", false, "write to ~/.claude/leadkit/config/<slug>.yaml (external to the repo) instead of an in-repo ./leadkit.yaml")
	project := fs.String("project", "", "the project slug to key the config on (default: the session's cwd-honest identity slug)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config init: %v\n", err)
		os.Exit(2)
	}

	slug := *project
	if slug == "" {
		slug, _ = sessionIdentitySlug(sessionCwd())
	}
	slug = install.Slugify(slug)
	if slug == "" {
		fmt.Fprintln(os.Stderr, "leadkit config init: could not resolve a project slug (pass --project <slug>)")
		os.Exit(1)
	}

	var target string
	if *external {
		dir := config.ExternalConfigDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit config init: mkdir %s: %v\n", dir, err)
			os.Exit(1)
		}
		target = filepath.Join(dir, slug+".yaml")
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit config init: getwd: %v\n", err)
			os.Exit(1)
		}
		target = filepath.Join(cwd, config.DefaultProfileName)
	}

	// Idempotent: refuse to clobber an existing config, exit 0 (safe to re-run).
	if _, err := os.Stat(target); err == nil {
		fmt.Printf("leadkit config init: %s already exists — leaving it untouched (idempotent).\n", target)
		return
	}

	if err := os.WriteFile(target, []byte(config.StarterConfigFor(slug, *external, projectOriginForInit(*external))), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config init: write %s: %v\n", target, err)
		os.Exit(1)
	}
	kind := "in-repo"
	if *external {
		kind = "external"
	}
	fmt.Printf("wrote %s starter config for %q: %s\n", kind, slug, target)
	if *external {
		fmt.Println("  (relative paths in it anchor to the config dir; the gate still scopes to the repo's own origin)")
	}
}

// runConfigReference emits the schema-level config reference — every profile field,
// its type, default, allowed values, and description. Needs no project (it's derived
// from the schema), so it runs anywhere. Default output is a human table; --markdown
// emits the committed docs/CONFIG-REFERENCE.md form.
func runConfigReference(args []string) {
	fs := flag.NewFlagSet("config reference", flag.ContinueOnError)
	markdown := fs.Bool("markdown", false, "emit markdown (the committed docs/CONFIG-REFERENCE.md form) instead of a human table")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit config reference: %v\n", err)
		os.Exit(2)
	}
	entries := config.Reference()
	if *markdown {
		fmt.Print(config.FormatReferenceMarkdown(entries))
		return
	}
	fmt.Print(config.FormatReference(entries))
}

// projectOriginForInit is the project_origin an external starter config should carry: the
// owner/repo of the repo `config init` runs in, "" when it has none (or the config is in-repo).
func projectOriginForInit(external bool) string {
	if !external {
		return ""
	}
	if cwd := sessionCwd(); cwd != "" {
		return config.ProjectOriginForDir(cwd)
	}
	return ""
}
