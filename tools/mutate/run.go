package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is what became of one mutant.
type Status string

const (
	Killed     Status = "killed"      // it built and the tests failed: the change was noticed
	Survived   Status = "survived"    // it built and the tests passed: nothing pins this decision
	Invalid    Status = "invalid"     // the mutant did not compile. It ran no tests, so it is neither killed nor survived, and the run fails
	NotApplied Status = "not-applied" // the edit did not take effect on the build (a no-op, stale offsets, a file the build excludes). It tested nothing, and the run fails
	Broken     Status = "broken"      // the mutant built, but `go test` failed without any test failing (a bad GOFLAGS, a full disk while linking, an OOM kill, a package that would not load). Nothing was judged, and the run fails
	NotCovered Status = "not-covered" // no test in any package executes the line, or none that does fails on it
	TimedOut   Status = "timed-out"   // the test run hit its timeout: neither a kill nor a survivor
	Hung       Status = "hung"        // timed out, then timed out AGAIN when re-run alone: the mutation makes the code hang, and the suite not finishing is the detection
)

// Options configures a run.
type Options struct {
	Root         string
	Pkgs         []string // package dirs relative to Root
	Jobs         int
	ListOnly     bool
	Wide         bool
	Only         string
	AcceptedFile string
	Out          io.Writer
	Timeout      time.Duration // per `go test` run; 0 means testTimeout
	MaxSettle    int           // most timeouts re-run alone; 0 means defaultMaxSettle

	// tmpdir, when set, is the TMPDIR of every go command run for ONE mutant (see privateTmp).
	tmpdir string

	// Seams for tests of the harness's own refusals; nil means the real thing.
	test  func(o Options, tmp string, id int, m Mutant, targets []string) (Status, string)
	cover func(o Options) (map[string]map[int]bool, error)
	built func(o Options) (map[string]bool, error)
}

// Result pairs a mutant with its outcome.
type Result struct {
	Mutant Mutant
	Status Status
	By     string // for a kill: the package whose tests noticed. For invalid and not-applied: why
}

const testTimeout = 90 * time.Second

// settleFactor multiplies the timeout for the isolated re-run. "Alone" only removes this run's own
// parallelism; other processes on the machine are uncontrolled, and an importer's suite that is
// merely slow (cmd/leadkit takes 17-48s quiet) can exceed the first limit twice. A deterministic
// hang survives any limit and a slow suite does not, so a mutant is called hung only if it is
// STILL running after this many times as long.
const settleFactor = 3

// defaultMaxSettle caps how many timeouts are re-run. Each costs one more full (multiplied)
// timeout, sequentially, so the cost grows with every hang. More timeouts than this means the
// machine was overloaded, not that this many mutants hang, and re-running them all would only
// spend an afternoon confirming it: the rest stay unsettled and fail the run.
const defaultMaxSettle = 20

func timeoutFor(o Options) time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return testTimeout
}

// Run mutates every package, prints the report, and returns false when a survivor is not accepted
// (or an accepted entry matches nothing), or when any mutant was invalid, not applied or broken:
// none of those was judged by a test, so a clean report over them would be a result nobody earned.
//
// Nothing here writes to the tree being mutated. A mutant is a replacement file in a temp dir,
// handed to the go command as a build overlay, so pointing Root at any checkout, including the
// one you are working in, leaves it byte-identical (a test pins that).
func Run(o Options) (bool, error) {
	// The overlay is keyed by the path Go itself resolves; a symlinked root (macOS /var, /tmp)
	// would silently match nothing and every mutant would "survive".
	root, err := filepath.EvalSymlinks(o.Root)
	if err != nil {
		return false, err
	}
	o.Root = root
	var all []Mutant
	for _, p := range o.Pkgs {
		ms, err := GeneratePackage(o.Root, p)
		if err != nil {
			return false, err
		}
		for _, m := range ms {
			if o.Only == "" || strings.Contains(m.File, o.Only) {
				all = append(all, m)
			}
		}
	}
	if o.ListOnly {
		for _, m := range all {
			fmt.Fprintf(o.Out, "%s:%d %s\n", m.File, m.Line, m.Key())
		}
		fmt.Fprintf(o.Out, "%d mutants\n", len(all))
		return true, nil
	}

	if o.test == nil {
		o.test = testMutant
	}
	if o.cover == nil {
		o.cover = coveredLines
	}
	if o.built == nil {
		o.built = builtFiles
	}
	imps, err := importers(o)
	if err != nil {
		return false, err
	}
	// The baseline comes first: a suite that already fails kills every mutant for free, so nothing
	// below may run a mutant until the unmutated tests are known to pass.
	covered, err := o.cover(o)
	if err != nil {
		return false, err
	}
	if err := canary(o, imps); err != nil {
		return false, err
	}
	built, err := o.built(o)
	if err != nil {
		return false, err
	}
	// A coverage map that matches no mutant means the file names did not line up, and every mutant
	// would be reported not-covered and never run: a green result that tested nothing.
	if len(all) > 0 {
		any := false
		for _, m := range all {
			if covered[m.File][m.Line] {
				any = true
				break
			}
		}
		if !any {
			return false, fmt.Errorf("no mutant lies on a line the tests execute; coverage file names did not match %q", all[0].File)
		}
	}
	tmp, err := os.MkdirTemp("", "mutate-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)

	results := make([]Result, len(all))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, o.Jobs))
	var mu sync.Mutex
	done := 0
	for i, m := range all {
		results[i] = Result{Mutant: m}
		if !built[m.File] {
			// The go command never compiles this file here, so the edit cannot reach any test.
			results[i].Status = NotApplied
			results[i].By = "the file is excluded from this build (build constraints or GOOS/GOARCH), so the edit cannot take effect"
			continue
		}
		isCovered := covered[m.File][m.Line]
		wide := imps[filepath.Dir(m.File)]
		if !isCovered && (!o.Wide || len(wide) == 0) {
			results[i].Status = NotCovered
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, m Mutant) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i].Status, results[i].By = evaluate(o, tmp, i, m, isCovered, wide)
			mu.Lock()
			done++
			if done%100 == 0 {
				fmt.Fprintf(os.Stderr, "mutate: %d mutants tested\n", done)
			}
			mu.Unlock()
		}(i, m)
	}
	wg.Wait()

	// A timeout under parallel load is inconclusive: the machine may have been too busy, and
	// counting it as a kill would let load hide a survivor. So each is re-run ONCE, ALONE (this
	// phase is sequential, after every parallel run has finished). If it times out again the
	// mutation really does make the code hang: an infinite loop introduced by a mutant is detected
	// by the suite not finishing, which is a kill, not an inconclusive result. Anything else the
	// re-run says (killed, survived, not viable) replaces the loaded answer.
	maxSettle := o.MaxSettle
	if maxSettle <= 0 {
		maxSettle = defaultMaxSettle
	}
	settled := 0
	rerun := o
	rerun.Timeout = timeoutFor(o) * settleFactor
	for i := range results {
		if results[i].Status != TimedOut {
			continue
		}
		if settled >= maxSettle {
			continue // left TimedOut: the report names it as unsettled
		}
		settled++
		m := results[i].Mutant
		st, by := evaluate(rerun, tmp, i, m, covered[m.File][m.Line], imps[filepath.Dir(m.File)])
		if st == TimedOut {
			st = Hung
		}
		results[i].Status, results[i].By = st, by
	}
	return report(o, results)
}

// evaluate runs one mutant against the narrowest tests that could kill it, widening to the
// importers' tests when the package's own do not. A line the package's own tests never execute can
// still be pinned from an importer, so only importers are tried for it; if none fails on it,
// nothing guards the line.
func evaluate(o Options, tmp string, i int, m Mutant, isCovered bool, wide []string) (Status, string) {
	targets := []string{"./" + filepath.Dir(m.File)}
	if !isCovered {
		targets = wide
	}
	st, by := o.test(o, tmp, i, m, targets)
	if st == Survived && isCovered && o.Wide && len(wide) > 0 {
		st, by = o.test(o, tmp, i, m, wide)
	}
	if st == Survived && !isCovered {
		st = NotCovered
	}
	return st, by
}

// baselineOp marks the unmutated run of the importer targets.
const baselineOp = "baseline"

// canary proves the harness before any result is believed. A file replaced by one that cannot
// compile must break the build, or mutants are not being applied and every one would survive. And
// with -wide, the importer packages must pass unmutated, or classify would count their own
// failure as a kill of every survivor.
func canary(o Options, imps map[string][]string) error {
	if err := overlayCanary(o); err != nil {
		return err
	}
	if !o.Wide {
		return nil
	}
	seen := map[string]bool{}
	var targets []string
	for _, p := range o.Pkgs {
		for _, t := range imps[p] {
			if !seen[t] {
				seen[t] = true
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sort.Strings(targets)
	file := ""
	for _, p := range o.Pkgs {
		if ms, err := GeneratePackage(o.Root, p); err == nil && len(ms) > 0 {
			file = ms[0].File
			break
		}
	}
	if file == "" {
		return nil
	}
	tmp, err := os.MkdirTemp("", "mutate-baseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// One run of every importer's suite at once, unlike a mutant's single narrow target, so it gets
	// the same longer limit a settling re-run does: a slow machine must not read as a red baseline.
	slow := o
	slow.Timeout = timeoutFor(o) * settleFactor
	st, _ := o.test(slow, tmp, 0, Mutant{File: file, Op: baselineOp}, targets)
	if st != Survived {
		return fmt.Errorf("the importer packages %s do not pass unmutated (status %q), so a failure there would read as a kill; fix them or run with -wide=false", strings.Join(targets, " "), st)
	}
	return nil
}

func overlayCanary(o Options) error {
	for _, p := range o.Pkgs {
		ms, err := GeneratePackage(o.Root, p)
		if err != nil {
			return err
		}
		if len(ms) == 0 {
			continue
		}
		tmp, err := os.MkdirTemp("", "mutate-canary-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		orig, err := os.ReadFile(filepath.Join(o.Root, filepath.FromSlash(ms[0].File)))
		if err != nil {
			return err
		}
		broken := Mutant{File: ms[0].File, Op: "canary", Before: string(orig), After: "package broken\nthis does not compile\n", start: 0, end: len(orig)}
		st, _ := o.test(o, tmp, 0, broken, []string{"./" + p})
		if st != Invalid {
			return fmt.Errorf("the overlay did not take effect for %s (a file that cannot compile still built, status %q); refusing to report results", p, st)
		}
	}
	return nil
}

// canaryOp marks the harness's own proof-of-mechanism mutant: a file replaced by one that cannot
// compile.
const canaryOp = "canary"

// testMutant applies one mutant through a build overlay (so the tree is never written to, and
// mutants run in parallel), proves it was applied, proves it compiles, and only then runs the
// targets' tests. GOFLAGS carries the overlay so that tests which `go build` the real binary see
// the mutant too.
//
// The order is the point. A mutant that does not compile runs no test, so it must be called
// invalid BEFORE `go test` can fail on it: a failure there reads as a kill. And an edit that did
// not change the file runs the unmutated suite, which passes, so it must be refused before that
// pass reads as a survivor.
func testMutant(o Options, tmp string, id int, m Mutant, targets []string) (Status, string) {
	if m.Op == baselineOp {
		return inPrivateTmp(o, tmp, id, func(o Options) (Status, string) { return runTests(o, "", targets) })
	}
	overlay, st, why := apply(o, tmp, id, m)
	if st != "" {
		return st, why
	}
	return inPrivateTmp(o, tmp, id, func(o Options) (Status, string) {
		if ok, out := compiles(o, overlay, filepath.Dir(m.File)); !ok {
			return Invalid, out
		}
		return runTests(o, overlay, targets)
	})
}

// inPrivateTmp runs f with o's go commands in a TMPDIR of their own, removed when f returns.
func inPrivateTmp(o Options, tmp string, id int, f func(Options) (Status, string)) (Status, string) {
	priv, cleanup, err := privateTmp(tmp, id)
	if err != nil {
		return Broken, "cannot make the mutant its own TMPDIR: " + err.Error()
	}
	defer cleanup()
	o.tmpdir = priv
	return f(o)
}

// apply writes the mutated file and the overlay that points the build at it, and PROVES the edit:
// the mutation must differ from the original and must have been cut from the bytes the generator
// saw, and the files it writes must be written. It returns a non-empty status (NotApplied) with the
// reason when any of that fails. A tool that edits files and does not check it did is how a
// no-op passes for a survivor on a platform where `sed -i` behaves differently.
func apply(o Options, tmp string, id int, m Mutant) (overlay string, st Status, why string) {
	abs := filepath.Join(o.Root, filepath.FromSlash(m.File))
	orig, err := os.ReadFile(abs)
	if err != nil {
		return "", NotApplied, "cannot read the target: " + err.Error()
	}
	if m.start < 0 || m.end < m.start || m.end > len(orig) || string(orig[m.start:m.end]) != m.Before {
		return "", NotApplied, "the file no longer holds " + fmt.Sprintf("%q", m.Before) + " where the mutant was generated: the offsets are stale and the edit would land in the wrong place"
	}
	src := m.Source(orig)
	if sha256.Sum256(src) == sha256.Sum256(orig) {
		return "", NotApplied, "the edit leaves the file byte-identical, so it tests nothing"
	}
	mutated := filepath.Join(tmp, fmt.Sprintf("m%d.go", id))
	if err := os.WriteFile(mutated, src, 0o644); err != nil {
		return "", NotApplied, "cannot write the mutated file: " + err.Error()
	}
	overlay = filepath.Join(tmp, fmt.Sprintf("m%d.json", id))
	js, _ := json.Marshal(map[string]map[string]string{"Replace": {abs: mutated}})
	if err := os.WriteFile(overlay, js, 0o644); err != nil {
		return "", NotApplied, "cannot write the overlay: " + err.Error()
	}
	return overlay, "", ""
}

// privateTmp makes the TMPDIR one mutant's go commands run in, and the cleanup that removes it
// the moment that mutant is judged. Every test a mutant runs writes temp files, and some leak them
// (the CLI tests built a 17 MB binary into one per run, ticket 184; ticket 179 was the same class).
// A mutation run is thousands of test runs, so those leaks multiplied until the disk was full, and
// a full disk makes the rest of the run read as broken. Nothing a mutant's tests leave can outlive
// that mutant, whoever forgot to clean it up. Inside tmp, which the run removes at its end, so a
// run cut short still leaves one directory and not thousands.
func privateTmp(tmp string, id int) (dir string, cleanup func(), err error) {
	dir = filepath.Join(tmp, fmt.Sprintf("tmp%d", id))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// overlayEnv is the environment of every go command run against a mutant. PWD must be the resolved
// root: the overlay is keyed by resolved paths.
func overlayEnv(o Options, overlay string) []string {
	env := append(os.Environ(), "PWD="+o.Root)
	if o.tmpdir != "" {
		env = append(env, "TMPDIR="+o.tmpdir)
	}
	if overlay != "" {
		env = append(env, "GOFLAGS="+strings.TrimSpace(os.Getenv("GOFLAGS")+" -overlay="+overlay))
	}
	return env
}

// compiles is the compile check: it builds the mutated package under the overlay and reports
// whether the mutant is buildable, with the compiler's own words when it is not. Mutants only edit
// function bodies of non-test files, so any compile error they cause (an orphaned variable, an
// import left unused) is in the mutated package itself, which this finds.
//
// This is the ONLY thing that makes a mutant invalid. `go test` is never asked: its failure on a
// mutant that does not build is indistinguishable, by exit status, from a kill.
func compiles(o Options, overlay, pkgDir string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutFor(o)+30*time.Second)
	defer cancel()
	return compilesWithin(ctx, o, overlay, pkgDir)
}

// compilesWithin is compiles under a caller's deadline. A build the deadline cut short, or one the
// go command never started, leaves no compiler output; the reason must still say what happened, or
// the report prints an invalid mutant with no diagnosis.
func compilesWithin(ctx context.Context, o Options, overlay, pkgDir string) (bool, string) {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", os.DevNull, "./"+pkgDir)
	cmd.Dir = o.Root
	cmd.Env = overlayEnv(o, overlay)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, ""
	}
	msg := strings.TrimSpace(string(out))
	switch {
	case ctx.Err() != nil:
		msg = "the build did not finish within its limit (" + err.Error() + ")\n" + msg
	case msg == "":
		msg = err.Error()
	}
	return false, strings.TrimSpace(msg)
}

func runTests(o Options, overlay string, targets []string) (Status, string) {
	limit := timeoutFor(o)
	ctx, cancel := context.WithTimeout(context.Background(), limit+30*time.Second)
	defer cancel()
	args := append([]string{"test", "-count=1", "-vet=off", "-failfast", "-timeout", limit.String()}, targets...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = o.Root
	cmd.Env = overlayEnv(o, overlay)
	out, err := cmd.CombinedOutput()
	return classify(out, err, ctx.Err())
}

// classify turns one `go test` run into a status. It is only ever given a build the compile check
// has passed (or the unmutated baseline), so the build is not in question here. What is in question
// is whether the tests ran and failed: a non-zero exit also comes from a GOFLAGS the go command
// rejects, a full disk while linking the test binary, an OOM kill, or a package that will not load,
// and none of those says anything about the tests. So Killed needs the output to show a test
// failing (testFailed), and any other failure is Broken, which is not counted and fails the run.
// A timeout is its own status: the run was cut short, so the tests never got to say whether they
// notice the mutant, and counting it as a kill would let machine load hide a survivor.
//
// The output is read here, at the test step, to tell a failing test from a broken environment. It
// is never asked whether the BUILD failed: compiles does that with a real `go build`.
func classify(out []byte, err error, ctxErr error) (Status, string) {
	if err == nil {
		return Survived, ""
	}
	if ctxErr != nil || bytes.Contains(out, []byte("panic: test timed out")) {
		return TimedOut, ""
	}
	if testFailed(out) {
		return Killed, firstFailingPackage(out)
	}
	return Broken, "go test exited with " + err.Error() + " but no test failed:\n" + strings.TrimSpace(string(out))
}

// testFailed reports whether the output of a failed `go test` shows a test failing: a failing test
// or subtest (--- FAIL), a panic, or a package result line with a duration (FAIL<tab>pkg<tab>0.1s).
// A package that did not build or set up prints that line WITHOUT a duration ("FAIL<tab>pkg
// [build failed]"), which is what tells the two apart. The duration line alone is a real kill: a
// test that calls os.Exit or log.Fatal, or a TestMain that fails, prints only "exit status 1" and
// that line.
//
// Two things in the output say the machine failed, not the mutant. One is a full disk ("no space
// left on device"), which makes t.TempDir fail so that the tests print --- FAIL for it, and it
// beats everything. The other is an OS-killed test binary ("signal: killed"), which also prints
// the duration line. A test that really fails can print an unindented "signal: killed" of its
// own, so --- FAIL and panic: win over that line; a binary the OS killed prints neither.
//
// NOT COVERED: a package that really fails beside one that hits a full disk is Broken, not
// Killed, and so is a test that legitimately prints "no space left on device" (none in this repo
// does). Both fail the run loudly, which is the cheaper way to be wrong.
func testFailed(out []byte) bool {
	if bytes.Contains(out, []byte("no space left on device")) {
		return false
	}
	named, duration, osKilled := false, false, false
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case l == "signal: killed":
			osKilled = true
		case strings.HasPrefix(strings.TrimLeft(l, " \t"), "--- FAIL"), strings.HasPrefix(l, "panic: "):
			named = true
		case strings.HasPrefix(l, "FAIL\t") && len(strings.Split(l, "\t")) >= 3:
			duration = true
		}
	}
	return named || (duration && !osKilled)
}

// builtFiles maps each non-test file of the mutated packages to whether the go command compiles
// it on this platform. A mutant in a file it does not compile cannot reach any test.
func builtFiles(o Options) (map[string]bool, error) {
	built := map[string]bool{}
	for _, p := range o.Pkgs {
		cmd := exec.Command("go", "list", "-f", `{{range .GoFiles}}{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{.}}{{"\n"}}{{end}}`, "./"+p)
		cmd.Dir = o.Root
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list %s: %w", p, err)
		}
		for _, name := range strings.Fields(string(out)) {
			built[filepath.ToSlash(filepath.Join(p, name))] = true
		}
	}
	return built, nil
}

func firstFailingPackage(out []byte) string {
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "FAIL\t") {
			if f := strings.Fields(l); len(f) > 1 {
				return f[1]
			}
		}
	}
	return ""
}

// coveredLines runs each package's tests once, unmutated, and returns file -> line -> executed.
// A package whose tests already fail is an error: a mutation run over a red baseline means nothing.
func coveredLines(o Options) (map[string]map[int]bool, error) {
	module, err := modulePath(o.Root)
	if err != nil {
		return nil, err
	}
	covered := map[string]map[int]bool{}
	for _, p := range o.Pkgs {
		prof, cleanup, err := coverProfilePath(p)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		cmd := exec.Command("go", "test", "-count=1", "-coverprofile="+prof, "./"+p)
		cmd.Dir = o.Root
		// The profile's directory is removed with the profile, so what the tests leave goes too.
		cmd.Env = append(os.Environ(), "PWD="+o.Root, "TMPDIR="+filepath.Dir(prof))
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("baseline tests for %s fail, so there is nothing to mutate against:\n%s", p, out)
		}
		f, err := os.Open(prof)
		if err != nil {
			return nil, err
		}
		err = parseProfile(f, module, covered)
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return covered, nil
}

// coverProfilePath returns a profile path no other run shares, and its cleanup.
func coverProfilePath(pkg string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "mutate-cover-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, strings.ReplaceAll(pkg, "/", "-")+".out"), func() { os.RemoveAll(dir) }, nil
}

func modulePath(root string) (string, error) {
	cmd := exec.Command("go", "list", "-m")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseProfile reads a Go coverprofile: "mod/path/file.go:l.c,l.c stmts count".
func parseProfile(r io.Reader, module string, into map[string]map[int]bool) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return fmt.Errorf("bad coverprofile line %q", line)
		}
		file, rest := line[:colon], strings.Fields(line[colon+1:])
		if len(rest) != 3 {
			return fmt.Errorf("bad coverprofile line %q", line)
		}
		count, err := strconv.Atoi(rest[2])
		if err != nil || count == 0 {
			continue
		}
		span := strings.Split(rest[0], ",")
		if len(span) != 2 {
			return fmt.Errorf("bad coverprofile span %q", line)
		}
		from, err1 := strconv.Atoi(strings.SplitN(span[0], ".", 2)[0])
		to, err2 := strconv.Atoi(strings.SplitN(span[1], ".", 2)[0])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad coverprofile span %q", line)
		}
		rel := strings.TrimPrefix(file, module+"/")
		if into[rel] == nil {
			into[rel] = map[int]bool{}
		}
		for l := from; l <= to; l++ {
			into[rel][l] = true
		}
	}
	return sc.Err()
}

// importers maps each mutated package dir to the test targets that exercise it from OUTSIDE:
// every package in the module that depends on it. cmd/leadkit's tests drive the real binary, so
// a decision the package's own tests miss can still be pinned there.
func importers(o Options) (map[string][]string, error) {
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Deps \" \"}}", "./...")
	cmd.Dir = o.Root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	module, err := modulePath(o.Root)
	if err != nil {
		return nil, err
	}
	deps := map[string]map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		d := map[string]bool{}
		for _, x := range f[1:] {
			d[x] = true
		}
		deps[f[0]] = d
	}
	res := map[string][]string{}
	for _, p := range o.Pkgs {
		full := module + "/" + p
		for pkg, d := range deps {
			if d[full] {
				res[p] = append(res[p], "./"+strings.TrimPrefix(pkg, module+"/"))
			}
		}
		sort.Strings(res[p])
	}
	return res, nil
}

// LoadAccepted reads "<key> -- <reason>" lines. A line with no reason is an error: the reason is
// the point of the list.
func LoadAccepted(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		key, reason, ok := strings.Cut(l, " -- ")
		if !ok || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: an accepted survivor needs a reason after ' -- '", path, i+1)
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(reason)
	}
	return out, nil
}

func report(o Options, results []Result) (bool, error) {
	accepted, err := LoadAccepted(filepath.Join(o.Root, o.AcceptedFile))
	if err != nil {
		return false, err
	}
	type tally struct{ killed, survived, invalid, acceptedInvalid, notApplied, notCovered, hung, acceptedN int }
	byPkg := map[string]*tally{}
	var unaccepted, hung, unsettled, invalid, acceptedInvalid, notApplied, broken []Result
	live := map[string]bool{}
	for _, r := range results {
		pkg := filepath.Dir(r.Mutant.File)
		t := byPkg[pkg]
		if t == nil {
			t = &tally{}
			byPkg[pkg] = t
		}
		switch r.Status {
		case Killed:
			t.killed++
		case Invalid:
			t.invalid++
			live[r.Mutant.Key()] = true
			if _, ok := accepted[r.Mutant.Key()]; ok {
				// Still counted as invalid, in no score and never as a kill. Accepting says only that
				// this operator cannot produce a buildable mutant here, and why.
				t.acceptedInvalid++
				acceptedInvalid = append(acceptedInvalid, r)
			} else {
				invalid = append(invalid, r)
			}
		case NotApplied:
			t.notApplied++
			notApplied = append(notApplied, r)
		case Broken:
			broken = append(broken, r)
		case TimedOut:
			// Only past the settle cap (or if something skipped the re-run): nothing judged this
			// mutant, so it must never read as clean.
			unsettled = append(unsettled, r)
		case Hung:
			t.hung++
			hung = append(hung, r)
		case NotCovered, Survived:
			if r.Status == NotCovered {
				t.notCovered++
			} else {
				t.survived++
			}
			live[r.Mutant.Key()] = true
			if _, ok := accepted[r.Mutant.Key()]; ok {
				t.acceptedN++
			} else {
				unaccepted = append(unaccepted, r)
			}
		default:
			// A mutant with no recognised outcome would otherwise be dropped from every column and
			// from the score, which is the silent uncounting this report exists to prevent.
			return false, fmt.Errorf("%s:%d has outcome %q, which the report does not know how to count", r.Mutant.File, r.Mutant.Line, r.Status)
		}
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	fmt.Fprintf(o.Out, "%-18s %7s %9s %8s %11s %9s %8s %12s %6s\n", "package", "killed", "survived", "accepted", "not-covered", "hung", "invalid", "not-applied", "score")
	for _, p := range pkgs {
		t := byPkg[p]
		score := "n/a"
		if k := t.killed + t.hung; k+t.survived > 0 {
			score = fmt.Sprintf("%.0f%%", 100*float64(k)/float64(k+t.survived))
		}
		fmt.Fprintf(o.Out, "%-18s %7d %9d %8d %11d %9d %8d %12d %6s\n", p, t.killed, t.survived, t.acceptedN, t.notCovered, t.hung, t.invalid, t.notApplied, score)
	}
	fmt.Fprintln(o.Out, "score = (killed + hung) / (killed + hung + survived), over the lines the tests execute. not-covered mutants (no test fails on them, in the package or an importer) are survivors for acceptance; accepted counts both. invalid and not-applied mutants ran no test and are in no score")
	{
		var n [8]int
		for _, r := range results {
			switch r.Status {
			case Killed:
				n[0]++
			case Survived:
				n[1]++
			case NotCovered:
				n[2]++
			case Hung:
				n[3]++
			case TimedOut:
				n[4]++
			case Invalid:
				n[5]++
			case NotApplied:
				n[6]++
			case Broken:
				n[7]++
			}
		}
		fmt.Fprintf(o.Out, "%d mutants = %d killed + %d survived + %d not-covered + %d hung + %d timed-out + %d INVALID + %d NOT-APPLIED + %d BROKEN\n", len(results), n[0], n[1], n[2], n[3], n[4], n[5], n[6], n[7])
	}

	ok := true
	if len(invalid) > 0 {
		ok = false
		fmt.Fprintf(o.Out, "\nFAIL: %d INVALID mutant(s): the build failed, so they ran no test. They are NOT counted as killed, "+
			"survived, or in the score; they were never judged. A compiler error is a defect in the mutation operator; "+
			"anything else (a full disk, a broken toolchain) is the environment. Fix it, or accept the mutant on %s with a reason:\n", len(invalid), o.AcceptedFile)
		for _, r := range invalid {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n%s", r.Mutant.File, r.Mutant.Line, describe(r.Mutant), indent(r.By, "      "))
		}
	}
	if len(acceptedInvalid) > 0 {
		fmt.Fprintf(o.Out, "\nNOTE: %d INVALID mutant(s) accepted on %s: they do not compile, ran no test, and are in no score:\n", len(acceptedInvalid), o.AcceptedFile)
		for _, r := range acceptedInvalid {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n", r.Mutant.File, r.Mutant.Line, describe(r.Mutant))
		}
	}
	if len(notApplied) > 0 {
		ok = false
		fmt.Fprintf(o.Out, "\nFAIL: %d NOT-APPLIED mutant(s): the edit did not take effect on the build, so what ran was the unmutated code. "+
			"They are NOT counted as killed or survived, or in the score:\n", len(notApplied))
		for _, r := range notApplied {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n%s", r.Mutant.File, r.Mutant.Line, describe(r.Mutant), indent(r.By, "      "))
		}
	}
	if len(broken) > 0 {
		ok = false
		fmt.Fprintf(o.Out, "\nFAIL: %d BROKEN mutant(s): `go test` failed but no test did, so the failure is the environment's (a rejected GOFLAGS, a full disk, an OOM kill, a package that would not load), not the tests'. "+
			"They are NOT counted as killed or survived, or in the score. Fix the environment and re-run:\n", len(broken))
		for _, r := range broken {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n%s", r.Mutant.File, r.Mutant.Line, describe(r.Mutant), indent(r.By, "      "))
		}
	}
	if len(hung) > 0 {
		// Informational, not a failure: counted as kills in the score above.
		fmt.Fprintf(o.Out, "\nNOTE: %d mutant(s) hung: they timed out under load and again when re-run alone, so the mutation makes the code hang and the suite not finishing is what detects it. Counted as kills:\n", len(hung))
		for _, r := range hung {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n", r.Mutant.File, r.Mutant.Line, describe(r.Mutant))
		}
	}
	if len(unsettled) > 0 {
		ok = false
		fmt.Fprintf(o.Out, "\nFAIL: %d mutant(s) timed out and were NOT settled by an isolated re-run (the settle cap is %d, or the re-run was skipped). "+
			"That many timeouts means the machine was overloaded, not that this many mutants hang; no result was reached for them. Re-run with a lower -j, or narrow with -only:\n", len(unsettled), settleCap(o))
		for _, r := range unsettled {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n    key: %s\n", r.Mutant.File, r.Mutant.Line, describe(r.Mutant), r.Mutant.Key())
		}
	}
	if len(unaccepted) > 0 {
		ok = false
		fmt.Fprintf(o.Out, "\nFAIL: %d surviving mutant(s) not on %s. Each is a decision no test pins:\n", len(unaccepted), o.AcceptedFile)
		for _, r := range unaccepted {
			fmt.Fprintf(o.Out, "  %s:%d  %s\n    key: %s\n", r.Mutant.File, r.Mutant.Line, describe(r.Mutant), r.Mutant.Key())
		}
		fmt.Fprintln(o.Out, "Add a test that fails on the change, or, if the change is equivalent, accept it with a reason.")
	}
	{
		var stale []string
		for k := range accepted {
			if !live[k] && acceptedInScope(k, o) {
				stale = append(stale, k)
			}
		}
		sort.Strings(stale)
		if len(stale) > 0 {
			ok = false
			fmt.Fprintf(o.Out, "\nFAIL: %d accepted entr(ies) match no surviving mutant (the code changed, or a test now kills it); remove them:\n", len(stale))
			for _, k := range stale {
				fmt.Fprintf(o.Out, "  %s\n", k)
			}
		}
	}
	return ok, nil
}

// acceptedInScope reports whether this run could have produced the accepted entry's mutant, so an
// entry for a package or file that was not run is not called stale. An entry naming no file can
// never match, so only a full run judges it.
func acceptedInScope(key string, o Options) bool {
	file, _, ok := strings.Cut(key, ": ")
	if !ok {
		return o.Only == ""
	}
	if o.Only != "" && !strings.Contains(file, o.Only) {
		return false
	}
	dir := path.Dir(file)
	for _, p := range o.Pkgs {
		if path.Clean(filepath.ToSlash(p)) == dir {
			return true
		}
	}
	return false
}

func settleCap(o Options) int {
	if o.MaxSettle > 0 {
		return o.MaxSettle
	}
	return defaultMaxSettle
}

// indent prefixes every line of s, and ends in a newline; empty s yields nothing.
func indent(s, prefix string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = append(lines[:6], "...")
	}
	return prefix + strings.Join(lines, "\n"+prefix) + "\n"
}

// describe renders the change, cutting a long one (a removed multi-line call) so a report line
// stays one line; the full text is in the key.
func describe(m Mutant) string {
	return fmt.Sprintf("%s: %q -> %q", m.Op, clip(m.Before), clip(m.After))
}

func clip(s string) string {
	const limit = 100
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
