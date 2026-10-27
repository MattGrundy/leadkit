// Command mutate runs a small mutation harness over the packages whose tests are the guard: for
// every decision in them (a comparison, a condition, a returned bool, a bare call) it makes the
// opposite change, runs the tests, and reports the changes the tests did not notice.
//
// A surviving mutant in a guard means a rule could be inverted and nothing would go red. That is a
// defect, so this FAILS when a survivor is not on tools/mutate/accepted.txt. It is not in
// `make check`: it takes minutes, not seconds.
//
// Every mutant ends in exactly one of: killed, survived, not-covered, hung, INVALID, NOT-APPLIED.
// The last two ran no test and are in no score, and either FAILS the run:
//
//   - INVALID: the mutant does not compile. A build failure is indistinguishable from a test
//     failure by exit status, so the mutated package is built first and `go test` is never the
//     judge of that.
//   - NOT-APPLIED: the edit did not take effect on the build (it changes nothing, it was cut from
//     bytes the file does not hold, or the build excludes the file), so what ran was unmutated
//     code, which passes and would read as a survivor.
//
// Before any mutant runs the unmutated tests must pass, and a file that cannot compile must break
// the build (the canary), or the run is refused. Nothing writes to the tree: a mutant is an
// overlay file in a temp dir, so pointing this at any checkout leaves it byte-identical.
//
// A test run that times out under load is inconclusive, so it is re-run once, alone, with the
// timeout multiplied (settleFactor). Still running after that, the mutation really does hang, and
// the suite not finishing is the detection: counted as a kill and listed as hung. COST: each
// timeout is one more full multiplied timeout (settleFactor x testTimeout), sequentially, so at most
// defaultMaxSettle are re-run; more than that means the machine was overloaded, and the rest fail
// the run as unsettled instead of burning an afternoon confirming it. The values are in run.go and
// a test pins the cap production uses.
//
//	go run ./tools/mutate [-pkgs a,b] [-j N] [-list] [-accepted file] [-wide]
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// defaultPkgs are the packages whose tests ARE the guard and gate.
var defaultPkgs = []string{"internal/hook", "internal/gate", "internal/queue", "internal/owners"}

func main() {
	pkgs := flag.String("pkgs", strings.Join(defaultPkgs, ","), "comma-separated package directories to mutate")
	jobs := flag.Int("j", max(1, runtime.NumCPU()/2), "mutants tested in parallel")
	list := flag.Bool("list", false, "only list the mutants, run nothing")
	accepted := flag.String("accepted", "tools/mutate/accepted.txt", "survivors that are accepted, with a reason each")
	wide := flag.Bool("wide", true, "re-test survivors, and lines no test of the package executes, against every package that imports it")
	only := flag.String("only", "", "only mutants whose file contains this substring")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(2)
	}
	opts := Options{Root: root, Pkgs: splitList(*pkgs), Jobs: *jobs, ListOnly: *list, Wide: *wide, Only: *only, AcceptedFile: *accepted, Out: os.Stdout}
	ok, err := Run(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
