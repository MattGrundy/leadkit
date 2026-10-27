package milestone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- injected fakes: never hit real gh/CI/git ---

type fakeTasks struct {
	counts map[string]int
	err    error
}

// OpenTaskCount reports the fixture count and treats any id present in the counts map as
// a KNOWN hierarchy node (so a fixture 0 reads as a genuine MET); an id absent from the
// map is unknown (the mis-keyed-id case → the caller reads Unverified).
func (f fakeTasks) OpenTaskCount(ms string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	n, known := f.counts[ms]
	return n, known, nil
}

type fakePR struct{ m map[string]State }

func (f fakePR) Merged(ref, _ string) (State, error) { return f.m[ref], nil }

type fakeCI struct{ m map[string]State }

func (f fakeCI) Succeeded(name string) (State, error) { return f.m[name], nil }

type fakeGate struct{ m map[string]State }

func (f fakeGate) Ready(ref string) (State, error) { return f.m[ref], nil }

// --- status renders per-condition state + open count, each type via its source ---

func TestEvaluate_RendersPerConditionStateAndOpenCount(t *testing.T) {
	art := Artifact{
		CurrentMilestone: "m1",
		DoD: []Condition{
			{Name: "tasks-closed", Type: TypeTasks},
			{Name: "prs-merged", Type: TypePR, PRs: []string{"20775", "20776"}},
			{Name: "spike-green", Type: TypeGate, Sha: "deadbeef"},
			{Name: "ci-green", Type: TypeCI, Check: "argo/main"},
			{Name: "login-smoke", Type: TypeManual},
		},
	}
	r := Resolvers{
		Tasks: fakeTasks{counts: map[string]int{"m1": 3}},
		PR:    fakePR{m: map[string]State{"20775": Met, "20776": Met}},
		Gate:  fakeGate{m: map[string]State{"deadbeef": Unmet}},
		CI:    fakeCI{m: map[string]State{"argo/main": Unverified}},
	}
	rep := Evaluate(art, true, r)

	if !rep.Declared || rep.CurrentMilestone != "m1" {
		t.Fatalf("Declared/CurrentMilestone = %v/%q", rep.Declared, rep.CurrentMilestone)
	}
	if !rep.OpenTaskCountKnown || rep.OpenTaskCount != 3 {
		t.Errorf("open task count = %d (known=%v), want 3/true", rep.OpenTaskCount, rep.OpenTaskCountKnown)
	}
	want := map[string]State{
		"tasks-closed": Unmet, // 3 open
		"prs-merged":   Met,   // both merged
		"spike-green":  Unmet, // gate not ready
		"ci-green":     Unverified,
		"login-smoke":  Unverified, // manual
	}
	got := map[string]State{}
	for _, c := range rep.Conditions {
		got[c.Name] = c.State
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("condition %s = %s, want %s", name, got[name], w)
		}
	}

	out := FormatStatus(rep)
	for _, sub := range []string{"m1", "open milestone tasks: 3", "tasks-closed", "login-smoke", "manual"} {
		if !strings.Contains(out, sub) {
			t.Errorf("FormatStatus missing %q\n%s", sub, out)
		}
	}
}

// --- check GO only when all met + zero open; BLOCK otherwise, naming blockers ---

func allMetResolvers() Resolvers {
	return Resolvers{
		Tasks: fakeTasks{counts: map[string]int{"m1": 0}},
		PR:    fakePR{m: map[string]State{"20775": Met}},
		Gate:  fakeGate{m: map[string]State{"deadbeef": Met}},
	}
}

func cleanArtifact() Artifact {
	return Artifact{
		CurrentMilestone: "m1",
		DoD: []Condition{
			{Name: "tasks-closed", Type: TypeTasks},
			{Name: "prs-merged", Type: TypePR, PRs: []string{"20775"}},
			{Name: "spike-green", Type: TypeGate, Sha: "deadbeef"},
		},
	}
}

func TestCheck_GO_only_when_all_met_and_zero_open(t *testing.T) {
	rep := Evaluate(cleanArtifact(), true, allMetResolvers())
	if rep.Blocked() {
		t.Fatalf("clean milestone should be GO, blockers=%v", rep.Blockers())
	}
	out := FormatCheck(rep, ModeBlock)
	if !strings.Contains(out, "GO") {
		t.Errorf("FormatCheck should say GO:\n%s", out)
	}
}

func TestCheck_BLOCK_on_open_task_names_it(t *testing.T) {
	r := allMetResolvers()
	r.Tasks = fakeTasks{counts: map[string]int{"m1": 2}}
	rep := Evaluate(cleanArtifact(), true, r)
	if !rep.Blocked() {
		t.Fatal("an open task must BLOCK")
	}
	joined := strings.Join(rep.Blockers(), " | ")
	if !strings.Contains(joined, "2 open milestone task") {
		t.Errorf("blockers must name the open tasks, got %q", joined)
	}
}

func TestCheck_BLOCK_on_unverified_and_unmet_signal_names_them(t *testing.T) {
	r := allMetResolvers()
	r.Gate = fakeGate{m: map[string]State{"deadbeef": Unmet}} // unmet
	r.PR = fakePR{m: map[string]State{"20775": Unverified}}   // unverified
	rep := Evaluate(cleanArtifact(), true, r)
	if !rep.Blocked() {
		t.Fatal("an unmet/unverified signal must BLOCK")
	}
	joined := strings.Join(rep.Blockers(), " | ")
	if !strings.Contains(joined, "spike-green") || !strings.Contains(joined, "prs-merged") {
		t.Errorf("blockers must name the unmet AND unverified conditions, got %q", joined)
	}
}

// --- manual/unverified alone keeps check BLOCKED ---

func TestCheck_Manual_alone_keeps_it_blocked(t *testing.T) {
	art := Artifact{
		CurrentMilestone: "m1",
		DoD: []Condition{
			{Name: "tasks-closed", Type: TypeTasks},
			{Name: "login-smoke", Type: TypeManual},
		},
	}
	r := Resolvers{Tasks: fakeTasks{counts: map[string]int{"m1": 0}}}
	rep := Evaluate(art, true, r)
	if !rep.Blocked() {
		t.Fatal("a manual/unverified condition alone must keep check BLOCKED (can't declare done what you can't verify)")
	}
	if !strings.Contains(strings.Join(rep.Blockers(), " "), "login-smoke") {
		t.Errorf("blockers must name the manual condition: %v", rep.Blockers())
	}
}

// --- absent artifact degrades cleanly (not a crash, not a false GO) ---

func TestEvaluate_AbsentArtifact_degrades_not_false_GO(t *testing.T) {
	rep := Evaluate(Artifact{}, false, Resolvers{})
	if rep.Declared {
		t.Fatal("absent artifact must not read as declared")
	}
	if !rep.Blocked() {
		t.Fatal("absent artifact must NOT be a false GO — Blocked() must be true")
	}
	if !strings.Contains(FormatStatus(rep), "no milestone DoD declared") {
		t.Errorf("status should degrade to the no-milestone notice: %q", FormatStatus(rep))
	}
	if !strings.Contains(FormatCheck(rep, ModeBlock), "no milestone DoD declared") {
		t.Errorf("check should degrade to the no-milestone notice: %q", FormatCheck(rep, ModeBlock))
	}
}

// --- mode resolution order: flag > profile > default-block ---

func TestResolveMode_order(t *testing.T) {
	cases := []struct {
		flag, profile, want string
	}{
		{"", "", ModeBlock},          // default
		{"", "warn", ModeWarn},       // profile
		{"warn", "block", ModeWarn},  // flag wins over profile
		{"block", "warn", ModeBlock}, // flag wins over profile
		{"bogus", "warn", ModeWarn},  // unrecognised flag falls through to profile
		{"", "bogus", ModeBlock},     // unrecognised profile falls through to default
	}
	for _, c := range cases {
		if got := ResolveMode(c.flag, c.profile); got != c.want {
			t.Errorf("ResolveMode(%q,%q) = %q, want %q", c.flag, c.profile, got, c.want)
		}
	}
}

// --- tasks signal reuses the hierarchy frontmatter query (real counter) ---

func writeTask(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTasksSignal_reuses_hierarchy_query(t *testing.T) {
	store := t.TempDir()
	// milestone:==m1, in_progress → open
	writeTask(t, store, "100-a.md", "---\nid: 100\nmilestone: m1\nstatus: in_progress\n---\nwork\n")
	// milestone:==m1 but terminal → not open
	writeTask(t, store, "101-b.md", "---\nid: 101\nmilestone: m1\nstatus: completed\n---\ndone\n")
	// different milestone → ignored
	writeTask(t, store, "102-c.md", "---\nid: 102\nmilestone: m2\nstatus: in_progress\n---\nother\n")

	counter := NewTaskCounter(store, "")
	art := Artifact{CurrentMilestone: "m1", DoD: []Condition{{Name: "tasks-closed", Type: TypeTasks}}}
	rep := Evaluate(art, true, Resolvers{Tasks: counter})

	if rep.OpenTaskCount != 1 {
		t.Errorf("open count = %d, want 1 (only the in_progress m1 task)", rep.OpenTaskCount)
	}
	if rep.Conditions[0].State != Unmet {
		t.Errorf("tasks condition = %s, want unmet (1 open)", rep.Conditions[0].State)
	}

	// Close the open one → the signal graduates to met, gate opens.
	writeTask(t, store, "100-a.md", "---\nid: 100\nmilestone: m1\nstatus: completed\n---\nwork\n")
	rep = Evaluate(art, true, Resolvers{Tasks: counter})
	if rep.OpenTaskCount != 0 || rep.Conditions[0].State != Met || rep.Blocked() {
		t.Errorf("after closing all m1 tasks: open=%d state=%s blocked=%v, want 0/met/false", rep.OpenTaskCount, rep.Conditions[0].State, rep.Blocked())
	}
}

// --- tasks guard: 0-because-done stays MET; 0-because-mis-keyed is UNVERIFIED ---

func tasksCondResult(t *testing.T, counts map[string]int, id string) ConditionResult {
	t.Helper()
	art := Artifact{CurrentMilestone: id, DoD: []Condition{{Name: "tasks-closed", Type: TypeTasks}}}
	rep := Evaluate(art, true, Resolvers{Tasks: fakeTasks{counts: counts}})
	return rep.Conditions[0]
}

func TestTasksSignal_KnownIdAllTerminal_isMet(t *testing.T) {
	// id present in the map with 0 → known genuine zero → MET.
	res := tasksCondResult(t, map[string]int{"123": 0}, "123")
	if res.State != Met {
		t.Fatalf("known id with 0 open tasks = %s (%q), want met", res.State, res.Detail)
	}
}

func TestTasksSignal_MisKeyedId_isUnverifiedNotFalseMet(t *testing.T) {
	// id NOT in the map (unknown) → 0 but not a genuine done → UNVERIFIED, not a silent MET.
	res := tasksCondResult(t, map[string]int{"123": 0}, "124")
	if res.State != Unverified {
		t.Fatalf("mis-keyed id = %s, want unverified (must BLOCK, never false-MET)", res.State)
	}
	if !strings.Contains(res.Detail, "124") || !strings.Contains(res.Detail, "found") {
		t.Errorf("detail should name the not-found id: %q", res.Detail)
	}
}

// --- pr `repo:` hint: the argv carries -R <repo>, empty repo carries none ---

type fakeRunner struct {
	argv []string
	out  string
	err  error
}

func (f *fakeRunner) Run(argv []string) (string, error) {
	f.argv = argv
	return f.out, f.err
}

func argvHas(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		ok := true
		for j, w := range want {
			if argv[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestPRResolver_RepoHint_passesDashR(t *testing.T) {
	fr := &fakeRunner{out: "MERGED"}
	pr := NewGHPRResolver(fr, "")
	st, err := pr.Merged("20775", "acmecorp/web-client")
	if err != nil {
		t.Fatal(err)
	}
	if st != Met {
		t.Errorf("merged state = %s, want met", st)
	}
	if !argvHas(fr.argv, "-R", "acmecorp/web-client") {
		t.Errorf("argv must carry -R <repo>, got %v", fr.argv)
	}
}

func TestPRResolver_EmptyRepo_noDashR_noRegression(t *testing.T) {
	fr := &fakeRunner{out: "MERGED"}
	pr := NewGHPRResolver(fr, "")
	if _, err := pr.Merged("20775", ""); err != nil {
		t.Fatal(err)
	}
	if argvHas(fr.argv, "-R") {
		t.Errorf("empty repo must NOT add -R (cwd-repo back-compat), got %v", fr.argv)
	}
}

func TestPRResolver_StateMapping(t *testing.T) {
	cases := []struct {
		out  string
		err  error
		want State
	}{
		{"MERGED", nil, Met},
		{"OPEN", nil, Unmet},
		{"CLOSED", nil, Unmet},
		{"", errFake, Unverified}, // gh failure (wrong repo/auth/network) → unverified
	}
	for _, c := range cases {
		pr := NewGHPRResolver(&fakeRunner{out: c.out, err: c.err}, "")
		st, _ := pr.Merged("20775", "acmecorp/web-client")
		if st != c.want {
			t.Errorf("out=%q err=%v → %s, want %s", c.out, c.err, st, c.want)
		}
	}
}

var errFake = os.ErrNotExist

// An unresolvable ref names the repo it was looked up in, so a wrong `repo:` is diagnosable.
func TestEvalPR_UnresolvableNamesRepoInDetail(t *testing.T) {
	art := Artifact{
		CurrentMilestone: "m1",
		DoD: []Condition{
			{Name: "extraction", Type: TypePR, Repo: "acmecorp/web-client", PRs: []string{"20775"}},
		},
	}
	rep := Evaluate(art, true, Resolvers{PR: fakePR{m: map[string]State{"20775": Unverified}}})
	res := rep.Conditions[0]
	if res.State != Unverified {
		t.Fatalf("unresolvable pr = %s, want unverified", res.State)
	}
	if !strings.Contains(res.Detail, "acmecorp/web-client") || !strings.Contains(res.Detail, "20775") {
		t.Errorf("detail must name the repo it looked in: %q", res.Detail)
	}
}

// merged in the named repo → Met; unmerged → Unmet.
func TestEvalPR_RepoMergedUnmerged(t *testing.T) {
	mk := func(state State) ConditionResult {
		art := Artifact{DoD: []Condition{{Name: "p", Type: TypePR, Repo: "o/r", PRs: []string{"1"}}}}
		return Evaluate(art, true, Resolvers{PR: fakePR{m: map[string]State{"1": state}}}).Conditions[0]
	}
	if got := mk(Met).State; got != Met {
		t.Errorf("merged → %s, want met", got)
	}
	if got := mk(Unmet).State; got != Unmet {
		t.Errorf("unmerged → %s, want unmet", got)
	}
}

// --- Parse: the repo: field parses on pr AND gate/ci conditions (forward-compat) ---

func TestParse_RepoField_onAllConditionTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "milestones.yaml")
	writeTask(t, dir, "milestones.yaml",
		"current_milestone: m1\ndod:\n"+
			"  - name: p\n    type: pr\n    repo: acmecorp/web-client\n    prs: [\"20775\"]\n"+
			"  - name: g\n    type: gate\n    repo: o/r\n    sha: deadbeef\n"+
			"  - name: c\n    type: ci\n    repo: o/r\n    check: argo/main\n")
	art, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"acmecorp/web-client", "o/r", "o/r"} {
		if art.DoD[i].Repo != want {
			t.Errorf("DoD[%d].Repo = %q, want %q", i, art.DoD[i].Repo, want)
		}
	}
}

// --- Parse: a missing artifact is not-present, not an error ---

func TestParse_MissingIsNotPresentNotError(t *testing.T) {
	_, present, err := Parse(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || present {
		t.Errorf("Parse(missing) = present=%v err=%v, want false/nil", present, err)
	}
}

// --- type: script — a fake ScriptRunner returns canned exit codes; NEVER runs a shell ---

type fakeScript struct {
	exit    int
	out     string
	err     error
	calls   int
	gotArg  string
	gotWD   string
	gotDead bool // whether the ctx carried a deadline (the timeout was threaded)
}

func (f *fakeScript) Run(ctx context.Context, arg, workdir string) (int, string, error) {
	f.calls++
	f.gotArg = arg
	f.gotWD = workdir
	_, f.gotDead = ctx.Deadline()
	return f.exit, f.out, f.err
}

// scriptCond evaluates a single script condition against the given resolvers.
func scriptCond(art Artifact, r Resolvers) ConditionResult {
	rep := Evaluate(art, true, r)
	return rep.Conditions[0]
}

func scriptArt(script string) Artifact {
	return Artifact{CurrentMilestone: "m1", DoD: []Condition{{Name: "spike-green", Type: TypeScript, Script: script}}}
}

// The documented exit-status → state matrix: 0 → met, 1 → unmet, ≥2 / run-error /
// timeout → unverified, and the detail NAMES the code/error (never a false met).
func TestScriptSignal_ExitStatusMatrix(t *testing.T) {
	cases := []struct {
		name      string
		exit      int
		err       error
		wantState State
		detailHas string
	}{
		{"exit0-met", 0, nil, Met, "0"},
		{"exit1-unmet", 1, nil, Unmet, "1"},
		{"exit2-unverified", 2, nil, Unverified, "2"},
		{"run-error-unverified", -1, os.ErrNotExist, Unverified, "could not run"},
		{"timeout-unverified", -1, context.DeadlineExceeded, Unverified, "timed out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeScript{exit: c.exit, err: c.err}
			res := scriptCond(scriptArt("check.sh cmd"), Resolvers{Script: fs, ScriptEnabled: true})
			if res.State != c.wantState {
				t.Fatalf("state = %s, want %s (detail %q)", res.State, c.wantState, res.Detail)
			}
			if !strings.Contains(res.Detail, c.detailHas) {
				t.Errorf("detail %q must name %q for diagnosis", res.Detail, c.detailHas)
			}
			// A never-false-met invariant: only exit 0 may read met.
			if c.exit != 0 && res.State == Met {
				t.Fatalf("a non-0 exit (%d)/error must NEVER read met", c.exit)
			}
		})
	}
}

// The VERDICT is the exit code ONLY — a script that prints "done" on stdout but exits 1
// is unmet; stdout is never treated as success. Output is still captured to the detail.
func TestScriptSignal_VerdictIsExitCodeNotStdout(t *testing.T) {
	fs := &fakeScript{exit: 1, out: "done\nall green\n"}
	res := scriptCond(scriptArt("echo done; exit 1"), Resolvers{Script: fs, ScriptEnabled: true})
	if res.State != Unmet {
		t.Fatalf("stdout 'done' with exit 1 must be unmet (verdict = exit code), got %s", res.State)
	}
	if !strings.Contains(res.Detail, "done") {
		t.Errorf("captured output should be folded into the detail for diagnosis: %q", res.Detail)
	}
}

// enable_script_signals:false → the condition reads unverified ("disabled") and the
// runner is NEVER called (the categorical hard-disable).
func TestScriptSignal_DisabledNeverCallsRunner(t *testing.T) {
	fs := &fakeScript{exit: 0} // would read MET if ever called
	res := scriptCond(scriptArt("anything"), Resolvers{Script: fs, ScriptEnabled: false})
	if res.State != Unverified {
		t.Fatalf("disabled script signal must read unverified, got %s", res.State)
	}
	if !strings.Contains(res.Detail, "disabled") {
		t.Errorf("detail must say the signal is disabled: %q", res.Detail)
	}
	if fs.calls != 0 {
		t.Fatalf("runner must NEVER be called when disabled, got %d calls", fs.calls)
	}
}

// A nil runner (unwired) reads unverified, never a false met.
func TestScriptSignal_NilRunnerUnverified(t *testing.T) {
	res := scriptCond(scriptArt("cmd"), Resolvers{ScriptEnabled: true})
	if res.State != Unverified || !strings.Contains(res.Detail, "no script runner") {
		t.Fatalf("nil runner = %s/%q, want unverified/no-runner", res.State, res.Detail)
	}
}

// A timeout reads unverified — the fake signals it via context.DeadlineExceeded (no real
// clock, no real shell). The ctx must also carry the threaded deadline.
func TestScriptSignal_TimeoutUnverified(t *testing.T) {
	fs := &fakeScript{exit: -1, err: context.DeadlineExceeded}
	res := scriptCond(scriptArt("sleep 999"), Resolvers{Script: fs, ScriptEnabled: true, ScriptTimeout: 30 * 1e9})
	if res.State != Unverified || !strings.Contains(res.Detail, "timed out") {
		t.Fatalf("timeout = %s/%q, want unverified/timed-out", res.State, res.Detail)
	}
	if !fs.gotDead {
		t.Error("a positive ScriptTimeout must be threaded onto the runner's ctx deadline")
	}
}

// Inline command vs a file path both dispatch correctly: the runner receives the raw
// command for an inline value, the RESOLVED ABSOLUTE PATH for an existing file, and the
// project workdir in both cases.
func TestScriptSignal_InlineVsFilePathDispatch(t *testing.T) {
	root := t.TempDir()
	dodDir := filepath.Join(root, ".leadkit", "dod")
	mustMkScriptDir(t, dodDir)
	scriptPath := filepath.Join(dodDir, "spike-green.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}

	// Inline: not an existing file → passed verbatim.
	inlineFS := &fakeScript{exit: 0}
	scriptCond(scriptArt("gh pr checks 123 --required"), Resolvers{
		Script: inlineFS, ScriptEnabled: true, ScriptWorkdir: root, ScriptResolve: resolve,
	})
	if inlineFS.gotArg != "gh pr checks 123 --required" {
		t.Errorf("inline command must dispatch verbatim, got %q", inlineFS.gotArg)
	}
	if inlineFS.gotWD != root {
		t.Errorf("workdir = %q, want project root %q", inlineFS.gotWD, root)
	}

	// File path: resolves to an existing file → the ABSOLUTE path is passed.
	fileFS := &fakeScript{exit: 0}
	scriptCond(scriptArt(".leadkit/dod/spike-green.sh"), Resolvers{
		Script: fileFS, ScriptEnabled: true, ScriptWorkdir: root, ScriptResolve: resolve,
	})
	if fileFS.gotArg != scriptPath {
		t.Errorf("a file-path script must dispatch the resolved abs path, got %q want %q", fileFS.gotArg, scriptPath)
	}
	if fileFS.gotWD != root {
		t.Errorf("workdir = %q, want project root %q", fileFS.gotWD, root)
	}
}

func mustMkScriptDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// The `script:` field parses off a type: script condition.
func TestParse_ScriptField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "milestones.yaml")
	writeTask(t, dir, "milestones.yaml",
		"current_milestone: m1\ndod:\n  - name: spike-green\n    type: script\n    script: \"gh pr checks 123\"\n")
	art, _, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if art.DoD[0].Type != TypeScript || art.DoD[0].Script != "gh pr checks 123" {
		t.Errorf("parsed script condition wrong: %+v", art.DoD[0])
	}
}

func TestParse_RealArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "milestones.yaml")
	writeTask(t, dir, "milestones.yaml", "current_milestone: m1\ndod:\n  - name: t\n    type: tasks\n  - name: p\n    type: pr\n    prs: [\"20775\", \"20776\"]\n")
	art, present, err := Parse(path)
	if err != nil || !present {
		t.Fatalf("Parse real = present=%v err=%v", present, err)
	}
	if art.CurrentMilestone != "m1" || len(art.DoD) != 2 || len(art.DoD[1].PRs) != 2 {
		t.Errorf("parsed artifact wrong: %+v", art)
	}
}
