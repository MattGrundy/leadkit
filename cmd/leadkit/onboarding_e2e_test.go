package main

// End-to-end walk of the onboarding path in docs/ADOPTING-LEADKIT.md, asserted rather than
// documented: a throwaway HOME and a throwaway project, the real built binary, and a count of
// how many steps and how many seconds it takes to get from nothing to `doctor` with zero
// criticals.
//
// WHAT IS AUTOMATED: writing leadkit.yaml, `team scaffold`, `init`, `team install`, `doctor`, and
// enqueueing the first task with the command the scaffold printed, plus the PRECONDITIONS an owner
// needs for standing up: its definition is installed, its lane exists, and `queue wait` blocks
// instead of erroring.
//
// WHAT IS NOT, ON PURPOSE: standing up the owners (`team standup`) needs live agents. Nothing here
// spawns one, pretends to, or counts the precondition checks as agent coverage. A green result of
// this test means "the mechanical path and the preconditions hold", never "an owner picked up work".
//
// THE ARMS, each named for what it actually proves.
//   - "clone": the binary built from this checkout, with a Go toolchain on PATH.
//   - "no-go-on-path": the same binary on a PATH with no Go and no leadkit source anywhere the
//     process could find it. It proves the commands need nothing from a source tree at runtime;
//     it does NOT prove the installer works.
//   - "installer": the binary is delivered by the repository's own install.sh, run against a
//     fixture release (a stub gh serving an archive and a SHA256SUMS), the way
//     scripts/test-install.sh drives it. This is the arm that covers the real binary-only entry.
//
// SANDBOX. Every command runs with an explicit, minimal environment: HOME is a temp dir, PATH is
// fixed, nothing is inherited except GOCOVERDIR (see newOnboardWalk), and `init` always passes --skip-launchctl. Nothing reads the
// developer's real ~/.claude, installs a launchd agent, or touches the real records_dir (the
// same discipline as apply_guard_test.go's runCLI).
//
// WHAT "DOCUMENTED" MEANS. A step counts as documented only if docs/ADOPTING-LEADKIT.md, the file
// `leadkit onboard` prints, actually contains its command. The walk reads the doc; it is not a
// hand-set boolean. A doc that stops saying `leadkit init --apply --confirm` turns that step
// undocumented and fails the test, and a doc that never said it could not hide a missing step
// behind a constant. Every place the path does not hold is a GAP, and any gap fails: the walk
// still applies the smallest workaround afterwards so later steps are measured, but nothing is
// left on an exemption list.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// adoptingDocNeedles are the commands the walk runs, and the exact text the doc has to contain
// for each to count as documented.
const (
	needleInit    = "leadkit init --apply --confirm"
	needleScaffld = "leadkit team scaffold --apply --confirm"
	needleInstall = "leadkit team install --apply --confirm"
	needleDoctor  = "leadkit doctor"
	needleStandup = "leadkit team standup --scaffold --confirm"
	needleTask    = "leadkit task create"
)

// printedOneLiner pulls the command the scaffold tells the adopter to run to enqueue their first
// task ("enqueue the first task: `...`"), or "" when it is not printed.
func printedOneLiner(scaffoldOut string) string {
	const marker = "enqueue the first task: `"
	i := strings.Index(scaffoldOut, marker)
	if i < 0 {
		return ""
	}
	rest := scaffoldOut[i+len(marker):]
	if j := strings.Index(rest, "`"); j >= 0 {
		return rest[:j]
	}
	return ""
}

// walkStep is one thing the adopter has to do, or one thing the harness checked.
type walkStep struct {
	name       string
	documented bool // listed in docs/ADOPTING-LEADKIT.md, or implied by a stated prerequisite
	verify     bool // the harness checking something (a doctor re-run, a blocking probe), not something the adopter does
	dur        time.Duration
	exit       int
}

type onboardWalk struct {
	t     *testing.T
	bin   string // the binary under test, installed at home/.claude/leadkit-bin
	home  string
	proj  string
	env   []string
	steps []walkStep
	gaps  map[string]string
	doc   string // docs/ADOPTING-LEADKIT.md, as `leadkit onboard` prints it
}

// docHas reports whether the adopter doc contains this command text.
// Whitespace is collapsed first: the doc wraps a long command across lines, and that must not
// make a documented step look undocumented.
func (w *onboardWalk) docHas(needle string) bool {
	return strings.Contains(strings.Join(strings.Fields(w.doc), " "), needle)
}

func newOnboardWalk(t *testing.T, arm string) *onboardWalk {
	t.Helper()
	home := t.TempDir()
	proj := filepath.Join(t.TempDir(), "widget")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	root := repoRootFromCwd(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "ADOPTING-LEADKIT.md"))
	if err != nil {
		t.Fatalf("the walk reads the adopter doc to decide what is documented: %v", err)
	}

	built := testBinary(t)
	bin := filepath.Join(home, ".claude", "leadkit-bin")
	path := "/usr/bin:/bin"
	switch arm {
	case "installer":
		installViaInstallSh(t, root, home, built)
	default:
		data, err := os.ReadFile(built)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if arm == "clone" {
		goBin, err := exec.LookPath("go")
		if err != nil {
			t.Skip("clone arm needs a Go toolchain on PATH")
		}
		path = filepath.Dir(goBin) + ":" + path
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + path,
		"TMPDIR=" + home,
		// Prerequisite 2 in the doc: Agent Teams on, "in your shell or a settings.json env block".
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"CLAUDE_CODE_SUBAGENT_MODEL=sonnet",
	}
	// The one inherited variable: under `make cover` the -cover binary warns on stderr when GOCOVERDIR is missing, and the walk parses stderr.
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		env = append(env, "GOCOVERDIR="+dir)
	}
	w := &onboardWalk{t: t, bin: bin, home: home, proj: proj, gaps: map[string]string{}, doc: string(doc), env: env}
	// The project is a git repo with an origin, like any real adopter's.
	w.sh("git", "init", "-q", ".")
	w.sh("git", "remote", "add", "origin", "git@github.com:acme/widget.git")
	return w
}

// sh runs a non-leadkit setup command, uncounted.
func (w *onboardWalk) sh(name string, args ...string) {
	w.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = w.proj, w.env
	if out, err := cmd.CombinedOutput(); err != nil {
		w.t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// lk runs the binary as a step and returns its combined output and exit code.
func (w *onboardWalk) lk(documented bool, args ...string) (string, int) {
	w.t.Helper()
	return w.lkTimeout(documented, false, 60*time.Second, args...)
}

func (w *onboardWalk) lkTimeout(documented, verify bool, limit time.Duration, args ...string) (string, int) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.bin, args...)
	cmd.Dir, cmd.Env = w.proj, w.env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			code = -2 // still running when the limit hit
		case errors.As(err, &ee):
			code = ee.ExitCode()
		default:
			w.t.Fatalf("exec leadkit %v: %v", args, err)
		}
	}
	w.steps = append(w.steps, walkStep{name: "leadkit " + strings.Join(args, " "), documented: documented, verify: verify, dur: time.Since(start), exit: code})
	return string(out), code
}

// manual records a step the adopter does by hand (no leadkit command involved).
func (w *onboardWalk) manual(documented bool, name string, fn func()) {
	w.t.Helper()
	start := time.Now()
	fn()
	w.steps = append(w.steps, walkStep{name: name, documented: documented, dur: time.Since(start)})
}

func (w *onboardWalk) gap(id, detail string) {
	w.t.Helper()
	if _, dup := w.gaps[id]; !dup {
		w.gaps[id] = detail
	}
	w.t.Logf("GAP %s: %s", id, detail)
}

type doctorReport struct {
	Checks []struct {
		Name     string `json:"name"`
		OK       bool   `json:"ok"`
		Severity string `json:"severity"`
	} `json:"checks"`
}

// doctor runs `leadkit doctor --json` and returns the failing critical check names and the failing
// hook:* check names.
func (w *onboardWalk) doctor() (criticals, hooks []string) {
	w.t.Helper()
	// A verification, not an adopter action: the doc's own step is "run doctor", but re-running it
	// to confirm a workaround is the harness looking, and must not inflate the step count.
	out, _ := w.lkTimeout(true, true, 60*time.Second, "doctor", "--json") // exit 1 on a critical is expected, the JSON is the signal
	var r doctorReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		w.t.Fatalf("doctor --json did not produce JSON: %v\n%s", err, out)
	}
	if len(r.Checks) == 0 {
		w.t.Fatal("doctor reported no checks at all: a green result over nothing proves nothing")
	}
	for _, c := range r.Checks {
		if c.OK {
			continue
		}
		if c.Severity == "critical" {
			criticals = append(criticals, c.Name)
		}
		if strings.HasPrefix(c.Name, "hook:") {
			hooks = append(hooks, c.Name)
		}
	}
	sort.Strings(criticals)
	return criticals, hooks
}

func (w *onboardWalk) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(w.proj, rel))
	return err == nil
}

// docStep runs one command the path needs. If the adopter doc does not contain it the step is
// UNDOCUMENTED: that is a gap (the adopter would have to discover it), recorded, and the step still
// runs so the rest of the walk is measured.
func (w *onboardWalk) docStep(needle string, args ...string) (string, int) {
	w.t.Helper()
	documented := w.docHas(needle)
	if !documented {
		w.gap("undocumented: "+needle, fmt.Sprintf("the path needs `%s` and docs/ADOPTING-LEADKIT.md never says to run it", needle))
	}
	out, code := w.lk(documented, args...)
	if code != 0 {
		w.t.Fatalf("leadkit %s exited %d:\n%s", strings.Join(args, " "), code, out)
	}
	return out, code
}

// walk is the whole path, in the order docs/ADOPTING-LEADKIT.md gives it.
func (w *onboardWalk) walk() {
	t := w.t
	t.Helper()

	// Write leadkit.yaml (piece a: queue_root, roles_path, roster, push_whitelist).
	w.manual(true, "write leadkit.yaml", func() {
		profile := "project_id: widget\nqueue_root: ./queue\npush_whitelist: [feat/*, fix/*, chore/*]\nroster:\n  - type: developer\n    count: 1\n"
		if err := os.WriteFile(filepath.Join(w.proj, "leadkit.yaml"), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	// The order is part of the contract: init installs the waker and sweep only for a queue that
	// exists, and scaffold is what creates it. The doc has to say scaffold first, or an adopter
	// following it exactly ends with no daemons and owners that are never woken.
	flat := strings.Join(strings.Fields(w.doc), " ")
	si, ii := strings.Index(flat, needleScaffld), strings.Index(flat, needleInit)
	if si >= 0 && ii >= 0 && si > ii {
		w.gap("documented order skips the daemons", "docs/ADOPTING-LEADKIT.md runs `init` before `team scaffold`; init skips the waker and sweep when the queue does not exist yet")
	}

	// team scaffold: the queue lanes, ROLES, and the task store.
	scaffoldOut, _ := w.docStep(needleScaffld, "team", "scaffold", "--apply", "--confirm")
	for _, lane := range []string{"developer", "orchestrator", "finalise"} {
		for _, st := range []string{"pending", "in-progress", "done"} {
			if !w.exists(filepath.Join("queue", lane, st)) {
				t.Errorf("scaffold did not create queue/%s/%s", lane, st)
			}
		}
	}
	roles, _ := os.ReadFile(filepath.Join(w.proj, "queue", "ROLES"))
	for _, row := range []string{"developer\tdeveloper", "orchestrator\torchestrator", "finalise\tfinalise"} {
		if !strings.Contains(string(roles), row) {
			t.Errorf("ROLES has no %q row:\n%s", row, roles)
		}
	}

	// init: the guard hooks, the daemons, the adopt skill, and the review-gate records dir.
	initOut, _ := w.docStep(needleInit, "init", "--apply", "--confirm", "--skip-launchctl")
	if _, hooks := w.doctor(); len(hooks) > 0 {
		t.Errorf("hooks still failing after init --apply --confirm: %v", hooks)
	}
	if strings.Contains(initOut, "were NOT installed") {
		t.Errorf("init in the documented order must not skip the daemons:\n%s", initOut)
	}
	// By NAME: the account-level burn plist always exists, so "some dev.leadkit plist" cannot fail.
	if runtime.GOOS == "darwin" {
		for _, label := range []string{"dev.leadkit.waker.widget", "dev.leadkit.sweep.widget"} {
			if !fileExists(filepath.Join(w.home, "Library", "LaunchAgents", label+".plist")) {
				t.Errorf("after the documented path there is no %s plist: the project's owners would never be woken", label)
			}
		}
	}

	// team install: the owners' and reviewers' definitions.
	w.docStep(needleInstall, "team", "install", "--apply", "--confirm")
	for _, def := range []string{"developer", "code-review-agent", "critique-agent", "finalise", "orchestrator"} {
		if !fileExists(filepath.Join(w.home, ".claude", "agents", def+".md")) {
			t.Errorf("owner definition %s.md is not installed after team install", def)
		}
	}

	// doctor: zero criticals. This is the documented step the others exist to satisfy.
	documented := w.docHas(needleDoctor)
	if !documented {
		w.gap("undocumented: "+needleDoctor, "the doc never tells the adopter to run doctor")
	}
	out, _ := w.lk(documented, "doctor", "--json")
	var r doctorReport
	if err := json.Unmarshal([]byte(out), &r); err != nil || len(r.Checks) == 0 {
		t.Fatalf("doctor --json did not produce checks: %v\n%s", err, out)
	}
	var criticals []string
	for _, c := range r.Checks {
		if !c.OK && c.Severity == "critical" {
			criticals = append(criticals, c.Name)
		}
	}
	if len(criticals) > 0 {
		w.gap("doctor-red-after-the-documented-steps", fmt.Sprintf("doctor has %d critical(s) after every documented step: %v", len(criticals), criticals))
		// The smallest workaround, counted as undocumented actions, so the rest is still measured.
		for _, c := range criticals {
			switch c {
			case "store_root":
				w.manual(false, "mkdir <project>/tasks  (store_root)", func() { onboardMkdir(t, filepath.Join(w.proj, "tasks")) })
			case "records_dir":
				w.manual(false, "mkdir ~/.claude/review-gate/records  (records_dir)", func() {
					onboardMkdir(t, filepath.Join(w.home, ".claude", "review-gate", "records"))
				})
			}
		}
		if c, _ := w.doctor(); len(c) > 0 {
			t.Fatalf("doctor still has criticals after the workaround: %v", c)
		}
	}

	// `onboard --check` is the command that tells the adopter what is next. A verification, not an
	// adopter action. With doctor green it must send them on to the owners, not back to a step
	// they have just completed.
	if chk, _ := w.lkTimeout(true, true, 60*time.Second, "onboard", "--check"); strings.Contains(chk, "scaffold --apply") || !strings.Contains(chk, "team standup") {
		w.gap("onboard-check-contradicts-the-doc", "after the documented steps `onboard --check` says:\n"+chk)
	}

	// standup --scaffold: the review-gate lanes a project needs beyond its roster.
	w.docStep(needleStandup, "team", "standup", "--scaffold", "--confirm")
	for _, lane := range []string{"code-review", "critique"} {
		if !w.exists(filepath.Join("queue", lane, "pending")) {
			t.Errorf("standup --scaffold did not create the %s lane", lane)
		}
	}

	// Enqueue the first task using the one-liner the scaffold printed, run exactly as printed with
	// its placeholders filled the way an adopter would.
	oneLiner := printedOneLiner(scaffoldOut)
	if oneLiner == "" {
		t.Fatalf("the scaffold no longer prints a first-task one-liner; there is nothing to run:\n%s", scaffoldOut)
	}
	filled := strings.NewReplacer("<what to do>", "first task").Replace(oneLiner)
	fields := strings.Fields(strings.ReplaceAll(filled, `"first task"`, "first-task"))
	if len(fields) < 2 || fields[0] != "leadkit" {
		t.Fatalf("the printed one-liner is not a leadkit command: %q", oneLiner)
	}
	if !w.docHas(needleTask) {
		w.gap("undocumented: "+needleTask, "the doc never says how a first task is created, though the scaffold prints the command")
	}
	laneHas := func() []string {
		m, _ := filepath.Glob(filepath.Join(w.proj, "queue", "developer", "pending", "*.md"))
		return m
	}
	out, code := w.lk(w.docHas(needleTask), fields[1:]...)
	if code != 0 || len(laneHas()) != 1 {
		w.gap("first-task-one-liner", fmt.Sprintf("running the printed %q exited %d and left %d ticket(s) in queue/developer/pending: %s",
			filled, code, len(laneHas()), strings.TrimSpace(firstLine(out))))
	}

	// An owner's first action, `queue wait` on an idle lane, must BLOCK rather than error.
	_, code = w.lkTimeout(true, true, 2*time.Second, "queue", "wait", filepath.Join(w.proj, "queue", "orchestrator"))
	if code != -2 {
		t.Errorf("`queue wait` on an idle lane returned (exit %d) instead of blocking", code)
	}

	// Final: the number.
	if c, _ := w.doctor(); len(c) > 0 {
		t.Errorf("the walk ended with doctor criticals: %v", c)
	}
}

// installViaInstallSh delivers the built binary the way an adopter without Go gets it: the
// repository's install.sh, run against a fixture release. A stub gh serves the archive and a
// SHA256SUMS (the same shape scripts/test-install.sh uses), so no network and no real gh are
// involved, and the installer's own checks (checksum, atomic replace, the INT/TERM trap) all run.
func installViaInstallSh(t *testing.T, root, home, builtBinary string) {
	t.Helper()
	osName := map[string]string{"darwin": "darwin", "linux": "linux"}[runtime.GOOS]
	arch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		t.Skipf("install.sh supports darwin/linux on arm64/amd64, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const tag = "v9.9.9"
	fix := t.TempDir()
	pkg := fmt.Sprintf("leadkit_%s_%s_%s", tag, osName, arch)
	if err := os.MkdirAll(filepath.Join(fix, "pkg", pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(builtBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fix, "pkg", pkg, "leadkit"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := pkg + ".tar.gz"
	if out, err := exec.Command("tar", "-C", filepath.Join(fix, "pkg"), "-czf", filepath.Join(fix, archive), pkg).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	tarball, err := os.ReadFile(filepath.Join(fix, archive))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tarball)
	if err := os.WriteFile(filepath.Join(fix, "SHA256SUMS"), []byte(fmt.Sprintf("%x  %s\n", sum, archive)), 0o644); err != nil {
		t.Fatal(err)
	}

	stubs := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1 $2\" in\n  \"auth status\") exit 0 ;;\n  \"release view\") echo " + tag + "; exit 0 ;;\n" +
		"  \"release download\")\n    dir=\"\"; pats=\"\"\n    while [ $# -gt 0 ]; do\n      case \"$1\" in\n        --dir) dir=\"$2\"; shift ;;\n        --pattern) pats=\"$pats $2\"; shift ;;\n      esac\n      shift\n    done\n" +
		"    for p in $pats; do [ -f \"$FIXTURE_DIR/$p\" ] && cp \"$FIXTURE_DIR/$p\" \"$dir/$p\"; done\n    exit 0 ;;\nesac\necho \"stub gh: unexpected: $*\" >&2; exit 99\n"
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", filepath.Join(root, "install.sh"), tag)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "TMPDIR=" + home, "PATH=" + stubs + ":/usr/bin:/bin", "FIXTURE_DIR=" + fix}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	bin := filepath.Join(home, ".claude", "leadkit-bin")
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("install.sh left no executable at %s: %v", bin, err)
	}
	if out, err := exec.Command(bin, "version").CombinedOutput(); err != nil {
		t.Fatalf("the installed binary does not run: %v\n%s", err, out)
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func onboardMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// undocumentedActions counts what the adopter has to do that the doc does not say. Verifications
// (doctor re-runs, the blocking probe) are the harness looking, never an adopter action.
func (w *onboardWalk) undocumentedActions() int {
	n := 0
	for _, s := range w.steps {
		if !s.verify && !s.documented {
			n++
		}
	}
	return n
}

func (w *onboardWalk) summary() string {
	var documented, extra, verifications int
	var actionTime, verifyTime time.Duration
	var b strings.Builder
	for i, s := range w.steps {
		kind := "documented"
		switch {
		case s.verify:
			verifications++
			kind = "verification"
		case s.documented:
			documented++
		default:
			extra++
			kind = "NOT DOCUMENTED"
		}
		switch {
		case !s.verify:
			actionTime += s.dur
		case !strings.HasPrefix(s.name, "leadkit queue wait"): // the 2s blocking probe is deliberate and would drown the signal
			verifyTime += s.dur
		}
		fmt.Fprintf(&b, "  %2d. [%s] %s (%s, exit %d)\n", i+1, kind, s.name, s.dur.Round(time.Millisecond), s.exit)
	}
	fmt.Fprintf(&b, "  => %d adopter action(s) to doctor-green: %d documented, %d NOT documented (+ %d verification(s)); leadkit time %s in actions + %s in verifications (the 2s blocking probe excluded); %d gap(s)",
		documented+extra, documented, extra, verifications, actionTime.Round(time.Millisecond), verifyTime.Round(time.Millisecond), len(w.gaps))
	return b.String()
}

func TestOnboarding_CloneToDoctorGreen(t *testing.T) {
	for _, arm := range []string{"clone", "no-go-on-path", "installer"} {
		t.Run(arm, func(t *testing.T) {
			w := newOnboardWalk(t, arm)
			w.walk()
			t.Logf("onboarding walk (%s):\n%s", arm, w.summary())

			if len(w.gaps) > 0 {
				ids := make([]string, 0, len(w.gaps))
				for id := range w.gaps {
					ids = append(ids, id)
				}
				sort.Strings(ids)
				t.Errorf("the documented path has %d gap(s): %v", len(ids), ids)
			}
			if got := w.undocumentedActions(); got != 0 {
				t.Errorf("the adopter has to do %d thing(s) the doc does not say", got)
			}
		})
	}
}
