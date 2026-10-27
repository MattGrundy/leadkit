package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureSrc = `package p

func Big(n int) bool {
	if n > 10 {
		return true
	}
	return false
}

func Untested(a, b string) bool { return a == b }
`

const weakTest = `package p

import "testing"

func TestBig(t *testing.T) {
	if !Big(100) || Big(0) {
		t.Fatal("wrong")
	}
}
`

// The boundary is what the weak test never touches, and what a strong one must.
const strongTest = weakTest + `
func TestBigBoundary(t *testing.T) {
	if Big(10) || !Big(11) {
		t.Fatal("wrong at the boundary")
	}
}

func TestUntested(t *testing.T) {
	if !Untested("a", "a") || Untested("a", "b") {
		t.Fatal("wrong")
	}
}
`

// An importer of p whose tests do not pin p's decisions.
const importerSrc = `package q

import "example.com/m/p"

func Both() bool { return p.Big(11) }
`

const importerTest = `package q

import "testing"

func TestBoth(t *testing.T) {
	if !Both() {
		t.Fatal("wrong")
	}
}
`

func fixture(t *testing.T, test string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":         "module example.com/m\n\ngo 1.21\n",
		"p/p.go":         fixtureSrc,
		"p/p_test.go":    test,
		"p/accepted.txt": "",
		"q/q.go":         importerSrc,
		"q/q_test.go":    importerTest,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGenerate_MutantsParseAndDiffer(t *testing.T) {
	root := fixture(t, weakTest)
	ms, err := GeneratePackage(root, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no mutants generated")
	}
	orig, _ := os.ReadFile(filepath.Join(root, "p/p.go"))
	ops := map[string]bool{}
	for _, m := range ms {
		ops[m.Op] = true
		got := m.Source(orig)
		if bytes.Equal(got, orig) {
			t.Errorf("%s changes nothing", m.Key())
		}
		if _, err := parser.ParseFile(token.NewFileSet(), "p.go", got, 0); err != nil {
			t.Errorf("%s does not parse: %v", m.Key(), err)
		}
	}
	for _, op := range []string{"operator", "negate-condition", "flip-bool"} {
		if !ops[op] {
			t.Errorf("no %s mutant from the fixture", op)
		}
	}
}

func TestKey_SurvivesCodeMovingAboveItButNotChangingIt(t *testing.T) {
	root := fixture(t, weakTest)
	before, _ := GeneratePackage(root, "p")
	moved := "package p\n\n// a new comment and\n// another\n" + strings.TrimPrefix(fixtureSrc, "package p\n")
	if err := os.WriteFile(filepath.Join(root, "p/p.go"), []byte(moved), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := GeneratePackage(root, "p")
	keys := func(ms []Mutant) []string {
		var k []string
		for _, m := range ms {
			k = append(k, m.Key())
		}
		return k
	}
	if !reflect.DeepEqual(keys(before), keys(after)) {
		t.Errorf("keys moved with the line numbers:\n%v\n%v", keys(before), keys(after))
	}
	if before[0].Line == after[0].Line {
		t.Error("the fixture edit was meant to move the lines")
	}
}

func run(t *testing.T, root string) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	ok, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 2, AcceptedFile: "p/accepted.txt", Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return ok, out.String()
}

func TestRun_AWeakTestLeavesASurvivorAndAStrongOneKillsIt(t *testing.T) {
	ok, out := run(t, fixture(t, weakTest))
	if ok {
		t.Fatalf("a boundary no test pins must fail the run:\n%s", out)
	}
	if !strings.Contains(out, `operator: ">" -> ">="`) {
		t.Errorf("the surviving boundary mutant must be named:\n%s", out)
	}
	if !strings.Contains(out, "Untested") && !strings.Contains(out, "not-covered") {
		t.Errorf("uncovered code must be reported apart from survivors:\n%s", out)
	}

	ok, out = run(t, fixture(t, strongTest))
	if !ok {
		t.Fatalf("a test that pins the boundary must pass:\n%s", out)
	}
}

func TestRun_AcceptedSurvivorPassesAndAStaleEntryFails(t *testing.T) {
	root := fixture(t, weakTest)
	ms, _ := GeneratePackage(root, "p")
	var boundary Mutant
	for _, m := range ms {
		if m.Op == "operator" && m.Before == ">" && m.After == ">=" {
			boundary = m
		}
	}
	if boundary.Key() == "" {
		t.Fatal("fixture has no > to >= mutant")
	}
	acc := filepath.Join(root, "p/accepted.txt")
	// Everything the weak test misses, accepted with a reason: the run passes.
	_, out := run(t, root)
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "key: ") {
			lines = append(lines, strings.TrimPrefix(strings.TrimSpace(l), "key: ")+" -- fixture: equivalent by construction")
		}
	}
	if len(lines) == 0 {
		t.Fatalf("expected survivors to accept:\n%s", out)
	}
	if err := os.WriteFile(acc, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, out := run(t, root); !ok {
		t.Fatalf("accepted survivors must not fail the run:\n%s", out)
	}
	// An entry matching nothing is stale.
	if err := os.WriteFile(acc, []byte(strings.Join(lines, "\n")+"\nnot a mutant -- gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, out := run(t, root); ok || !strings.Contains(out, "match no surviving mutant") {
		t.Fatalf("a stale accepted entry must fail:\nok=%v\n%s", ok, out)
	}
}

func TestLoadAccepted_RequiresAReason(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(p, []byte("# comment\n\nsome key #0 -- because\nbare key #0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAccepted(p); err == nil {
		t.Error("an entry with no reason must be rejected")
	}
	if err := os.WriteFile(p, []byte("some key #0 -- because\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadAccepted(p)
	if err != nil || m["some key #0"] != "because" {
		t.Errorf("got %v, %v", m, err)
	}
}

func TestRun_RedBaselineIsAnErrorNotAScore(t *testing.T) {
	root := fixture(t, "package p\n\nimport \"testing\"\n\nfunc TestRed(t *testing.T) { t.Fatal(\"red\") }\n")
	var out bytes.Buffer
	if _, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 1, AcceptedFile: "p/accepted.txt", Out: &out}); err == nil {
		t.Error("mutating against failing tests proves nothing and must be an error")
	}
}

func TestParseProfile(t *testing.T) {
	into := map[string]map[int]bool{}
	prof := "mode: set\nmgpp.dev/leadkit/internal/hook/a.go:10.2,12.3 2 1\nmgpp.dev/leadkit/internal/hook/a.go:20.2,21.3 1 0\n"
	if err := parseProfile(strings.NewReader(prof), "mgpp.dev/leadkit", into); err != nil {
		t.Fatal(err)
	}
	got := into["internal/hook/a.go"]
	if !got[10] || !got[11] || !got[12] || got[20] {
		t.Errorf("covered lines = %v", got)
	}
}

// fakeTester stands in for `go test` so the harness's own refusals can be driven without running
// mutants. The canary's unbuildable file is the mutant whose Op is canaryOp.
type fakeTester struct {
	mu     sync.Mutex
	canary Status
	base   Status // outcome of the unmutated importer run; Survived (passing) when unset
	narrow Status // outcome when only the mutated package's tests run
	wide   Status // outcome when an importer's tests run
	calls  [][]string
}

func (f *fakeTester) test(_ Options, _ string, _ int, m Mutant, targets []string) (Status, string) {
	f.mu.Lock()
	f.calls = append(f.calls, targets)
	f.mu.Unlock()
	if m.Op == baselineOp {
		if f.base == "" {
			return Survived, ""
		}
		return f.base, ""
	}
	if m.Op == canaryOp {
		return f.canary, ""
	}
	if len(targets) == 1 && targets[0] == "./p" {
		return f.narrow, ""
	}
	return f.wide, ""
}

func (f *fakeTester) wideCalls() int {
	n := 0
	for _, c := range f.calls {
		if len(c) != 1 || c[0] != "./p" {
			n++
		}
	}
	return n
}

func runFake(t *testing.T, root string, f *fakeTester, wide bool, accepted string) (bool, string, error) {
	t.Helper()
	var out bytes.Buffer
	ok, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 1, Wide: wide, AcceptedFile: accepted, Out: &out, test: f.test})
	return ok, out.String(), err
}

func TestClassify(t *testing.T) {
	boom := errors.New("exit status 1")
	for name, c := range map[string]struct {
		out    string
		err    error
		ctxErr error
		want   Status
	}{
		"pass":            {"ok\tp\n", nil, nil, Survived},
		"assertion":       {"--- FAIL: TestX\nFAIL\tp\t0.1s\n", boom, nil, Killed},
		"test timeout":    {"panic: test timed out after 1m30s\nFAIL\tp\t90.0s\n", boom, nil, TimedOut},
		"killed by clock": {"", boom, context.DeadlineExceeded, TimedOut},
		// A panic, a failing subtest, and a package result line with a duration each show a test failing.
		"panic":   {"panic: boom\n\ngoroutine 1 [running]:\n", boom, nil, Killed},
		"subtest": {"    --- FAIL: TestX/y (0.00s)\nFAIL\n", boom, nil, Killed},
		"a real failure beside a package that did not build": {"--- FAIL: TestX (0.00s)\nFAIL\tp\t0.1s\nFAIL\tq [build failed]\n", boom, nil, Killed},
		// None of these shows a test failing: the environment broke, and that is not a kill.
		"build failed":      {"# p\np.go:3:2: undefined: x\nFAIL\tp [build failed]\n", boom, nil, Broken},
		"setup failed":      {"# p\nno required module provides package q\nFAIL\tp [setup failed]\n", boom, nil, Broken},
		"rejected flag":     {"go: invalid value \"bogus\" for flag -vet (from $GOFLAGS): nope\nusage: go test\n", boom, nil, Broken},
		"no output":         {"", boom, nil, Broken},
		"killed by the OS":  {"signal: killed\nFAIL\tp\t3.2s\n", boom, nil, Broken},
		"link out of space": {"/usr/local/go/pkg/tool/link: write p.test: no space left on device\nFAIL\tp [build failed]\n", boom, nil, Broken},
		// The disk filled while the tests ran: t.TempDir fails and the tests print --- FAIL for it.
		"test run out of space": {"--- FAIL: TestX (0.00s)\n    x_test.go:9: TempDir: mkdir /tmp/T: no space left on device\nFAIL\nFAIL\tp\t0.1s\n", boom, nil, Broken},
		"panic out of space":    {"panic: write /tmp/f: no space left on device\nFAIL\tp\t0.1s\n", boom, nil, Broken},
		// The duration line is the only sign of these kills: no test is named when the process exits.
		"exit without a failing test": {"exit status 1\nFAIL\tp\t0.2s\nFAIL\n", boom, nil, Killed},
		"two packages, one exited":    {"ok\tq\t0.1s\nexit status 1\nFAIL\tp\t0.2s\nFAIL\n", boom, nil, Killed},
		// A failing test that prints an unindented "signal: killed" itself is still a failing test.
		"a failing test that prints signal: killed": {"signal: killed\n--- FAIL: TestX (0.01s)\nFAIL\tp\t0.1s\n", boom, nil, Killed},
		"a panic that prints signal: killed":        {"signal: killed\npanic: boom\nFAIL\tp\t0.1s\n", boom, nil, Killed},
		// These count only at the start of a line: a log line that mentions them is not a failure.
		"panic mentioned mid-line":    {"note: recovered from panic: x\n", boom, nil, Broken},
		"duration line, wrong prefix": {"xFAIL\tp\t0.1s\n", boom, nil, Broken},
	} {
		if got, _ := classify([]byte(c.out), c.err, c.ctxErr); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestRun_RefusesWhenTheCanaryFileStillBuilds(t *testing.T) {
	f := &fakeTester{canary: Survived, narrow: Killed, wide: Killed}
	_, _, err := runFake(t, fixture(t, strongTest), f, true, "p/accepted.txt")
	if err == nil || !strings.Contains(err.Error(), "overlay did not take effect") {
		t.Fatalf("a mutant mechanism that cannot break the build must be refused, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("no mutant may run once the canary fails, got %d runs", len(f.calls))
	}
}

func TestRun_RefusesCoverageThatMatchesNoMutant(t *testing.T) {
	f := &fakeTester{canary: Invalid, narrow: Killed, wide: Killed}
	var out bytes.Buffer
	_, err := Run(Options{Root: fixture(t, strongTest), Pkgs: []string{"p"}, Jobs: 1, AcceptedFile: "p/accepted.txt", Out: &out, test: f.test,
		cover: func(Options) (map[string]map[int]bool, error) {
			return map[string]map[int]bool{"elsewhere/p.go": {1: true}}, nil
		}})
	if err == nil || !strings.Contains(err.Error(), "coverage file names did not match") {
		t.Fatalf("coverage naming no mutated file must be an error, not a clean report: %v", err)
	}
}

func TestRun_AnImporterThatFailsOnASurvivorKillsIt(t *testing.T) {
	root := fixture(t, weakTest)
	f := &fakeTester{canary: Invalid, narrow: Survived, wide: Killed}
	ok, out, err := runFake(t, root, f, true, "p/accepted.txt")
	if err != nil || !ok {
		t.Fatalf("a mutant an importer's tests fail on is killed: ok=%v err=%v\n%s", ok, err, out)
	}
	if f.wideCalls() == 0 {
		t.Error("survivors must be re-run against importers")
	}
	f = &fakeTester{canary: Invalid, narrow: Survived, wide: Survived}
	if ok, out, _ := runFake(t, root, f, true, "p/accepted.txt"); ok {
		t.Errorf("a survivor no importer kills must fail:\n%s", out)
	}
	f = &fakeTester{canary: Invalid, narrow: Survived, wide: Killed}
	ok, _, _ = runFake(t, root, f, false, "p/accepted.txt")
	if ok || f.wideCalls() != 0 {
		t.Errorf("without -wide survivors stay survivors and importers are not run: ok=%v wide calls=%d", ok, f.wideCalls())
	}
}

func TestRun_CodeNoTestExecutesFailsUnlessAnImporterPinsIt(t *testing.T) {
	root := fixture(t, weakTest) // Untested is not covered by p's own tests
	f := &fakeTester{canary: Invalid, narrow: Killed, wide: Survived}
	ok, out, err := runFake(t, root, f, true, "p/accepted.txt")
	if err != nil || ok || !strings.Contains(out, "Untested") {
		t.Fatalf("an uncovered line nothing kills is a failure that names it: ok=%v err=%v\n%s", ok, err, out)
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 5 && f[0] == "p" && f[4] == "0" {
			t.Errorf("the uncovered mutants must be counted as not-covered: %q", l)
		}
	}
	f = &fakeTester{canary: Invalid, narrow: Killed, wide: Killed}
	if ok, out, _ := runFake(t, root, f, true, "p/accepted.txt"); !ok {
		t.Errorf("an importer that fails on an uncovered line pins it:\n%s", out)
	}
	f = &fakeTester{canary: Invalid, narrow: Killed, wide: Killed}
	if ok, _, _ := runFake(t, root, f, false, "p/accepted.txt"); ok {
		t.Error("with no importer pass, an uncovered line has no guard and must fail")
	}
}

// seqTester answers each mutant from a per-mutant script, one entry per call, and records how
// many calls were in flight together, so a test can prove a re-run happened ALONE.
type seqTester struct {
	mu       sync.Mutex
	script   func(call int, m Mutant) Status // call is the 1-based call number for this mutant
	limits   map[string][]time.Duration      // the timeout each call was given, per mutant key
	calls    map[string]int
	inflight int
	maxIn    int
	order    []string // "<key>#<call>" in start order
	soloRuns []string // calls that started while another call was running
}

func (f *seqTester) test(o Options, _ string, _ int, m Mutant, targets []string) (Status, string) {
	if m.Op == canaryOp {
		return Invalid, "" // the canary
	}
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
		f.limits = map[string][]time.Duration{}
	}
	f.limits[m.Key()] = append(f.limits[m.Key()], timeoutFor(o))
	f.calls[m.Key()]++
	n := f.calls[m.Key()]
	f.inflight++
	if f.inflight > f.maxIn {
		f.maxIn = f.inflight
	}
	tag := fmt.Sprintf("%s#%d", m.Key(), n)
	if f.inflight > 1 {
		f.soloRuns = append(f.soloRuns, tag)
	}
	f.order = append(f.order, tag)
	f.mu.Unlock()
	time.Sleep(5 * time.Millisecond) // long enough for an overlapping call to be seen
	st := f.script(n, m)
	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	return st, ""
}

func runSeq(t *testing.T, root string, f *seqTester, jobs int) (bool, string, error) {
	t.Helper()
	return runSeqWith(t, root, f, Options{Jobs: jobs})
}

func runSeqWith(t *testing.T, root string, f *seqTester, o Options) (bool, string, error) {
	t.Helper()
	var out bytes.Buffer
	o.Root, o.Pkgs, o.AcceptedFile, o.Out, o.test = root, []string{"p"}, "p/accepted.txt", &out, f.test
	ok, err := Run(o)
	return ok, out.String(), err
}

// A timeout under load is inconclusive; the isolated re-run is what decides.
func TestRun_ALoadTimeoutIsSettledByAnIsolatedRerun(t *testing.T) {
	root := fixture(t, strongTest)

	// timed out under load, killed when alone: an ordinary kill
	f := &seqTester{script: func(call int, _ Mutant) Status {
		if call == 1 {
			return TimedOut
		}
		return Killed
	}}
	if ok, out, err := runSeq(t, root, f, 4); err != nil || !ok || strings.Contains(out, "hung") && strings.Contains(out, "NOTE") {
		t.Fatalf("a load timeout that is killed when alone is a kill: ok=%v err=%v\n%s", ok, err, out)
	}

	// timed out under load, SURVIVED when alone: load must not hide a survivor
	f = &seqTester{script: func(call int, _ Mutant) Status {
		if call == 1 {
			return TimedOut
		}
		return Survived
	}}
	if ok, out, _ := runSeq(t, root, f, 4); ok || !strings.Contains(out, "surviving mutant") {
		t.Fatalf("a mutant that survives its isolated re-run is a survivor, however it first behaved:\n%s", out)
	}
}

// Hanging both times means the mutation makes the code hang, which is a detection. The run
// passes, the score counts them as kills, and they are LISTED, so nobody has to accept anything.
func TestRun_AMutantThatHangsAloneToo_IsKilledByTheHang(t *testing.T) {
	f := &seqTester{script: func(int, Mutant) Status { return TimedOut }}
	ok, out, err := runSeq(t, fixture(t, strongTest), f, 2)
	if err != nil || !ok {
		t.Fatalf("a deterministic hang must not keep the run red: ok=%v err=%v\n%s", ok, err, out)
	}
	if !strings.Contains(out, "mutant(s) hung") || !strings.Contains(out, "Counted as kills") {
		t.Errorf("hung mutants must be listed:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if fs := strings.Fields(l); len(fs) > 7 && fs[0] == "p" && fs[len(fs)-1] != "100%" {
			t.Errorf("hung mutants count as kills, so the score is 100%%: %q", l)
		}
	}
	// Every mutant ran exactly twice: once under load, once alone. Not a third time.
	for key, n := range f.calls {
		if n != 2 {
			t.Errorf("%s ran %d times, want exactly 2 (one loaded, one alone)", key, n)
		}
	}
}

// "Alone" is the point: the re-runs must be sequential and must come after every parallel run.
func TestRun_TheRerunHappensAloneAfterTheParallelPhase(t *testing.T) {
	f := &seqTester{script: func(call int, _ Mutant) Status {
		if call == 1 {
			return TimedOut
		}
		return Killed
	}}
	if _, _, err := runSeq(t, fixture(t, strongTest), f, 4); err != nil {
		t.Fatal(err)
	}
	firstRerun := -1
	for i, tag := range f.order {
		if strings.HasSuffix(tag, "#2") {
			firstRerun = i
			break
		}
	}
	if firstRerun < 0 {
		t.Fatal("no re-run happened")
	}
	for _, tag := range f.order[firstRerun:] {
		if strings.HasSuffix(tag, "#1") {
			t.Errorf("a first run (%s) started after a re-run began: the re-runs must follow the whole parallel phase", tag)
		}
	}
	for _, tag := range f.soloRuns {
		if strings.HasSuffix(tag, "#2") {
			t.Errorf("re-run %s overlapped another run: it was not alone", tag)
		}
	}
}

// The real thing, no fake: a mutant that really does hang (&& turned into || leaves `!found` true
// forever), run through go test with a short timeout. It must time out, time out again alone, and
// come out as a kill instead of keeping the run red.
func TestRun_ARealHangIsKilledByTheHang(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n",
		"p/p.go": `package p

// Scan stops at the first hit, or at n.
func Scan(n, hit int) int {
	i, found := 0, false
	for i < n && !found {
		if i == hit {
			found = true
		}
		i++
	}
	return i
}
`,
		"p/p_test.go":    "package p\n\nimport \"testing\"\n\nfunc TestScan(t *testing.T) {\n\tif Scan(5, -1) != 5 || Scan(5, 2) != 3 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
		"p/accepted.txt": "",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	ok, err := Run(Options{Root: root, Pkgs: []string{"p"}, Jobs: 2, AcceptedFile: "p/accepted.txt", Out: &out, Timeout: 3 * time.Second, Only: "p.go"})
	if err != nil || !ok {
		t.Fatalf("a mutant that makes the code hang is detected by the hang; ok=%v err=%v\n%s", ok, err, out.String())
	}
	if !strings.Contains(out.String(), "mutant(s) hung") || !strings.Contains(out.String(), `operator: "&&" -> "||"`) {
		t.Errorf("the && -> || mutant must be reported as hung:\n%s", out.String())
	}
}

// "Slow" must not be called "hung": the re-run gets a LONGER limit, and a suite that merely takes
// longer than the first limit comes back as its real answer. A deterministic hang survives any limit.
func TestRun_TheRerunGetsALongerLimit_SoASlowSuiteIsNotCalledHung(t *testing.T) {
	slow := &slowTester{}
	var out bytes.Buffer
	ok, err := Run(Options{Root: fixture(t, strongTest), Pkgs: []string{"p"}, Jobs: 2, AcceptedFile: "p/accepted.txt", Out: &out, test: slow.test})
	if err != nil {
		t.Fatal(err)
	}
	if ok || !strings.Contains(out.String(), "surviving mutant") {
		t.Errorf("a suite that finishes under the longer limit and passes is a SURVIVOR, not a hang:\n%s", out.String())
	}
	if strings.Contains(out.String(), "mutant(s) hung") {
		t.Errorf("slow was called hung:\n%s", out.String())
	}
	if slow.rerunLimit < settleFactor*testTimeout {
		t.Errorf("the re-run limit was %s, want at least %dx the first (%s)", slow.rerunLimit, settleFactor, testTimeout)
	}
}

// slowTester times out at the first limit and finishes (surviving) at any longer one.
type slowTester struct {
	mu         sync.Mutex
	rerunLimit time.Duration
}

func (s *slowTester) test(o Options, _ string, _ int, m Mutant, _ []string) (Status, string) {
	if m.Op == canaryOp {
		return Invalid, ""
	}
	if timeoutFor(o) <= testTimeout {
		return TimedOut, ""
	}
	s.mu.Lock()
	s.rerunLimit = timeoutFor(o)
	s.mu.Unlock()
	return Survived, ""
}

// Past the settle cap the rest stay unsettled and FAIL: that many timeouts is an overloaded
// machine, and re-running all of them would spend hours confirming it.
func TestRun_PastTheSettleCapTimeoutsStayUnsettledAndFail(t *testing.T) {
	f := &seqTester{script: func(int, Mutant) Status { return TimedOut }}
	ok, out, err := runSeqWith(t, fixture(t, strongTest), f, Options{Jobs: 2, MaxSettle: 2})
	if err != nil || ok {
		t.Fatalf("unsettled timeouts must fail the run: ok=%v err=%v\n%s", ok, err, out)
	}
	if !strings.Contains(out, "NOT settled") || !strings.Contains(out, "settle cap is 2") {
		t.Errorf("the failure must say why and name the cap:\n%s", out)
	}
	reruns := 0
	for _, n := range f.calls {
		if n == 2 {
			reruns++
		}
	}
	if reruns != 2 {
		t.Errorf("exactly the cap (2) mutants may be re-run, got %d", reruns)
	}
	for key, limits := range f.limits {
		if len(limits) == 2 && limits[1] != limits[0]*settleFactor {
			t.Errorf("%s: the re-run limit %s is not %dx the first %s", key, limits[1], settleFactor, limits[0])
		}
	}
}

// Only a TIMEOUT earns a re-run; a kill or a survivor is already an answer.
func TestRun_OnlyATimeoutIsRerun(t *testing.T) {
	f := &seqTester{script: func(int, Mutant) Status { return Killed }}
	if ok, out, err := runSeq(t, fixture(t, strongTest), f, 2); err != nil || !ok {
		t.Fatalf("ok=%v err=%v\n%s", ok, err, out)
	}
	for key, n := range f.calls {
		if n != 1 {
			t.Errorf("%s ran %d times; a kill is final", key, n)
		}
	}
}

// If a timed-out mutant ever reached the report unsettled, it must read as a failure, not clean.
func TestReport_AnUnsettledTimeoutIsNotClean(t *testing.T) {
	var out bytes.Buffer
	ok, err := report(Options{Out: &out, AcceptedFile: "none.txt", Root: t.TempDir()}, []Result{
		{Mutant: Mutant{File: "p/p.go", Func: "F", Op: "operator", Before: "a", After: "b"}, Status: TimedOut},
	})
	if err != nil || ok {
		t.Errorf("an unsettled timeout must fail the report: ok=%v err=%v\n%s", ok, err, out.String())
	}
	if !strings.Contains(out.String(), "NOT settled") || strings.Contains(out.String(), "surviving mutant") {
		t.Errorf("an unsettled timeout must get its own message, not be filed as a survivor:\n%s", out.String())
	}
}

func TestRun_ARedImporterIsAnErrorNotAVacuousPass(t *testing.T) {
	root := fixture(t, strongTest)
	f := &fakeTester{canary: Invalid, base: Killed, narrow: Survived, wide: Killed}
	ok, _, err := runFake(t, root, f, true, "p/accepted.txt")
	if err == nil || ok || !strings.Contains(err.Error(), "do not pass unmutated") {
		t.Fatalf("importers that fail unmutated must be an error: ok=%v err=%v", ok, err)
	}
	f = &fakeTester{canary: Invalid, base: Killed, narrow: Killed, wide: Killed}
	if _, _, err := runFake(t, root, f, false, "p/accepted.txt"); err != nil {
		t.Errorf("without -wide the importers are not run, so they cannot refuse: %v", err)
	}
}

func TestRemovable_LockAndWaitCallsAreNotMutated(t *testing.T) {
	src := "package p\n\nimport \"sync\"\n\nfunc f(mu *sync.Mutex, wg *sync.WaitGroup) {\n\tmu.Lock()\n\tmu.Unlock()\n\twg.Wait()\n\twg.Done()\n\tg()\n}\n\nfunc g() {}\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ms, err := Generate(filepath.Join(dir, "p.go"), "p.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Before != "g()" {
		t.Errorf("only g() is a removable call, got %+v", ms)
	}
}

func TestRun_StaleCheckOnlyJudgesEntriesForPackagesThatRan(t *testing.T) {
	root := fixture(t, strongTest)
	acc := filepath.Join(root, "p/accepted.txt")
	f := &fakeTester{canary: Invalid, narrow: Killed, wide: Killed}
	if err := os.WriteFile(acc, []byte("other/pkg/x.go: F: operator: \">\" => \">=\" #0 -- a package this run skipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, out, err := runFake(t, root, f, true, "p/accepted.txt"); err != nil || !ok {
		t.Errorf("an entry for a package that was not run is not stale: ok=%v err=%v\n%s", ok, err, out)
	}
	if err := os.WriteFile(acc, []byte("p/p.go: Gone: operator: \">\" => \">=\" #0 -- the code left\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, out, _ := runFake(t, root, f, true, "p/accepted.txt"); ok || !strings.Contains(out, "match no surviving mutant") {
		t.Errorf("an entry for a package that ran and matched nothing is stale:\n%s", out)
	}
}

func TestAcceptedInScope(t *testing.T) {
	o := Options{Pkgs: []string{"internal/gate", "./internal/hook/"}}
	for name, c := range map[string]struct {
		key, only string
		want      bool
	}{
		"package that ran":           {"internal/gate/a.go: F: operator: \"a\" => \"b\" #0", "", true},
		"cleaned package that ran":   {"internal/hook/a.go: F: operator: \"a\" => \"b\" #0", "", true},
		"package that did not run":   {"internal/queue/a.go: F: operator: \"a\" => \"b\" #0", "", false},
		"file outside -only":         {"internal/gate/a.go: F: operator: \"a\" => \"b\" #0", "b.go", false},
		"file inside -only":          {"internal/gate/a.go: F: operator: \"a\" => \"b\" #0", "a.go", true},
		"unparseable, full run":      {"not a mutant", "", true},
		"unparseable, narrowed run":  {"not a mutant", "a.go", false},
		"prefix of a package is not": {"internal/gate2/a.go: F: operator: \"a\" => \"b\" #0", "", false},
	} {
		o.Only = c.only
		if got := acceptedInScope(c.key, o); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

// manyMutants is a fixture whose package has n comparisons, each exercised by its own test, so a
// run generates at least n mutants that all reach the tester.
func manyMutants(t *testing.T, n int) string {
	t.Helper()
	var src, test strings.Builder
	src.WriteString("package p\n\n")
	test.WriteString("package p\n\nimport \"testing\"\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&src, "func F%d(a, b int) bool { return a > b }\n", i)
		fmt.Fprintf(&test, "func TestF%d(t *testing.T) {\n\tif !F%d(2, 1) || F%d(1, 2) {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n", i, i, i)
	}
	root := fixture(t, test.String())
	if err := os.WriteFile(filepath.Join(root, "p/p.go"), []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// main.go never sets MaxSettle, so the cap production runs under is defaultMaxSettle. Every other cap
// test passes one explicitly, which would leave the default free to become unlimited unnoticed.
func TestRun_TheDefaultSettleCapIsWhatProductionUses(t *testing.T) {
	// The cap is the worst-case cost of a run (that many multiplied timeouts, back to back), so its
	// value is a decision and is stated here as a literal: a test derived from the constant would pass
	// whatever it was changed to. Changing it means updating this and the cost in main.go's doc.
	if defaultMaxSettle != 20 {
		t.Fatalf("defaultMaxSettle is %d; the documented bound is 20 (see main.go)", defaultMaxSettle)
	}
	root := manyMutants(t, defaultMaxSettle+5)
	f := &seqTester{script: func(int, Mutant) Status { return TimedOut }}
	ok, out, err := runSeqWith(t, root, f, Options{Jobs: 4}) // MaxSettle deliberately unset
	if err != nil || ok {
		t.Fatalf("unsettled timeouts must fail the run: ok=%v err=%v", ok, err)
	}
	first, reruns := 0, 0
	for _, n := range f.calls {
		first++
		if n == 2 {
			reruns++
		}
	}
	if first <= defaultMaxSettle {
		t.Fatalf("fixture too small to test the cap: only %d mutants reached the tester", first)
	}
	if reruns != defaultMaxSettle {
		t.Errorf("with MaxSettle unset exactly defaultMaxSettle (%d) may be re-run, got %d of %d", defaultMaxSettle, reruns, first)
	}
	if want := first - defaultMaxSettle; !strings.Contains(out, fmt.Sprintf("%d mutant(s) timed out and were NOT settled", want)) {
		t.Errorf("exactly the %d beyond the cap must be reported unsettled:\n%s", want, out)
	}
	if !strings.Contains(out, fmt.Sprintf("settle cap is %d", defaultMaxSettle)) {
		t.Errorf("the report must name the default cap:\n%s", out)
	}
}

// The overlay is keyed by the resolved root, so a PWD that reaches it through a symlink must not
// make the go command name files by the symlinked path.
func TestOverlayCanary_HoldsWhenPWDReachesTheRootThroughASymlink(t *testing.T) {
	real, err := filepath.EvalSymlinks(fixture(t, strongTest))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("PWD", link)
	if err := overlayCanary(Options{Root: real, Pkgs: []string{"p"}, test: testMutant}); err != nil {
		t.Fatalf("the overlay must apply through a symlinked PWD: %v", err)
	}
}

func TestCoveredLines_ConcurrentRunsDoNotShareAProfile(t *testing.T) {
	a, cleanA, err := coverProfilePath("p")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanA()
	b, cleanB, err := coverProfilePath("p")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanB()
	if a == b {
		t.Fatalf("two runs share the profile path %s", a)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		root, err := filepath.EvalSymlinks(fixture(t, strongTest))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = coveredLines(Options{Root: root, Pkgs: []string{"p"}})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("concurrent coveredLines: %v", err)
		}
	}
}
