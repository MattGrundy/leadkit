package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mgpp.dev/leadkit/internal/burnreport"
)

// `leadkit burn-report` is a READ-ONLY spend reporter over Claude Code session
// transcripts. It is DISTINCT from `leadkit burn` (the live throttle). The aggregation
// core lives in internal/burnreport (shared with `leadkit doctor`); this file is the
// thin CLI: flag parsing + rendering.
//
//	leadkit burn-report [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--projects-dir DIR]
//	                    [--tools] [--json] [--by project|model|day]

// burnReportOpts are the parsed flags.
type burnReportOpts struct {
	since, until time.Time
	projectsDir  string
	tools, json  bool
	by           string
}

func runBurnReport(args []string) {
	fs := flag.NewFlagSet("burn-report", flag.ContinueOnError)
	since := fs.String("since", "", "first day to include, YYYY-MM-DD (default: 7 days ago, UTC)")
	until := fs.String("until", "", "last day to include, YYYY-MM-DD (default: no upper bound)")
	dir := fs.String("projects-dir", "", "Claude Code projects dir (default: ~/.claude/projects)")
	tools := fs.Bool("tools", false, "also print the Opus IC-vs-orchestration tool-mix per project")
	asJSON := fs.Bool("json", false, "machine-readable output")
	by := fs.String("by", "", "single rollup: project|model|day (default: all three)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	o := burnReportOpts{projectsDir: *dir, tools: *tools, json: *asJSON, by: *by}
	if o.projectsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn-report: %v\n", err)
			os.Exit(1)
		}
		o.projectsDir = filepath.Join(home, ".claude", "projects")
	}
	o.since = time.Now().UTC().AddDate(0, 0, -7)
	var err error
	if *since != "" {
		if o.since, err = time.Parse(time.DateOnly, *since); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn-report: bad --since %q (want YYYY-MM-DD)\n", *since)
			os.Exit(2)
		}
	}
	if *until != "" {
		if o.until, err = time.Parse(time.DateOnly, *until); err != nil {
			fmt.Fprintf(os.Stderr, "leadkit burn-report: bad --until %q (want YYYY-MM-DD)\n", *until)
			os.Exit(2)
		}
	}
	if o.by != "" && o.by != "project" && o.by != "model" && o.by != "day" {
		fmt.Fprintf(os.Stderr, "leadkit burn-report: bad --by %q (want project|model|day)\n", o.by)
		os.Exit(2)
	}
	if st, serr := os.Stat(o.projectsDir); serr != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "leadkit burn-report: projects dir %q not readable\n", o.projectsDir)
		os.Exit(1)
	}
	rep := burnreport.Aggregate(burnreport.WalkTranscripts(o.projectsDir, o.tools), burnreport.DefaultPrices, o.since, o.until, o.tools)
	if err := renderBurnReport(os.Stdout, rep, o); err != nil {
		fmt.Fprintf(os.Stderr, "leadkit burn-report: %v\n", err)
		os.Exit(1)
	}
}

// defaultTranscriptsDir is Claude Code's transcripts root (~/.claude/projects); "" when
// the home dir is unknowable (doctor then skips its recent-spend line).
func defaultTranscriptsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func renderBurnReport(w io.Writer, rep burnreport.Report, o burnReportOpts) error {
	if o.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprintf(w, "since %s — est $%.2f (list-price proxy)\n", rep.Since, rep.TotalCost)
	fmt.Fprintf(w, "(%s)\n", rep.Caveat)
	table := func(title string, rows []burnreport.Row, opus bool) {
		fmt.Fprintf(w, "\n%s\n", title)
		for _, r := range rows {
			fmt.Fprintf(w, "  %-34s $%10.2f  %14d tok  %5.1f%%", r.Key, r.Cost, r.Tokens, r.Pct)
			if opus {
				fmt.Fprintf(w, "  opus %5.1f%%", r.OpusPct)
			}
			fmt.Fprintln(w)
		}
	}
	if o.by == "" || o.by == "model" {
		table("BY MODEL", rep.ByModel, false)
	}
	if o.by == "" || o.by == "project" {
		table("BY PROJECT", rep.ByProject, true)
	}
	if o.by == "" || o.by == "day" {
		fmt.Fprintf(w, "\nBY DAY\n")
		for _, r := range rep.ByDay {
			fmt.Fprintf(w, "  %-12s $%10.2f\n", r.Key, r.Cost)
		}
	}
	if o.tools {
		fmt.Fprintf(w, "\nOPUS TOOL-MIX (IC = Edit/Write/MultiEdit/NotebookEdit/Bash; ORCH = Task/Agent/SendMessage)\n")
		for _, m := range rep.ToolMix {
			fmt.Fprintf(w, "  %-34s %7d calls  IC %5.1f%%  ORCH %5.1f%%\n", m.Project, m.Calls, m.ICPct, m.OrchPct)
		}
	}
	return nil
}
