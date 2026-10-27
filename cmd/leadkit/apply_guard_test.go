package main

// End-to-end guard tests for the post-incident confirmation fix (see
// docs/build-tasks/apply-safety-fixes.md): these exec the ACTUAL built binary, the
// same way a real invocation would, against temp fixtures only — never real data.
// confirmGuard's own unit tests (above, in main_test.go) cover the decision logic in
// isolation; these prove the wiring: a bare `--apply` really can't mutate a live
// tree/file when run for real.
//
// SANDBOXING (post-incident #2, 2026-09-04): TestCLI_Init_BareApplyRefusesAndNeverMutates
// once ran `init --apply --confirm` with no --launch-agents-dir override and inherited
// the REAL $HOME + the test's own cwd (inside this repo) — which let config.Load()'s
// upward directory walk find THIS repo's real leadkit.yaml (pointing at the real
// queue) and let defaultLaunchAgentsDir() resolve to the REAL ~/Library/LaunchAgents.
// The result: two real LaunchAgents got installed and loaded on the dev machine,
// running against the real queue. runCLI below now sandboxes EVERY invocation
// unconditionally — HOME, cwd, and LEADKIT_PROFILE are all pinned into an isolated
// temp dir regardless of what flags/extraEnv a given test does or doesn't pass, so a
// future test (team install, ...) can't reintroduce the same class of bug by
// forgetting a flag. See ApplyDaemonPlan's doc comment for the SEPARATE reason
// TestCLI_Init_* also passes --skip-launchctl: sandboxing the CONTENT of a plist
// doesn't stop the launchctl CALL itself from being a real OS-level registration.

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testBinaryDir is where buildTestBinary put the binary ("" until it has run). TestMain
// removes it. The directory cannot be t.TempDir(): that belongs to ONE test and is removed
// when it ends, while the binary is built once and shared by every test in the package.
var (
	testBinaryDirMu sync.Mutex
	testBinaryDir   string
)

// keepTestBinaryEnv, when set, leaves the built binary in place after the run, for a
// developer who wants to drive it by hand. It is off by default: every `go test` of this
// package builds a ~17 MB binary, and one that is never removed cost 30 GB over a week.
const keepTestBinaryEnv = "LEADKIT_KEEP_TEST_BINARY"

// TestMain removes the shared test binary once the whole package has run. m.Run returns
// whether the tests pass or fail, so this runs after a t.Fatal. It cannot run when the
// process is killed or a test panics, which is why the binary is built once per package run
// and not once per test.
func TestMain(m *testing.M) {
	code := m.Run()
	removeTestBinary()
	os.Exit(code)
}

func removeTestBinary() {
	testBinaryDirMu.Lock()
	defer testBinaryDirMu.Unlock()
	if testBinaryDir != "" && os.Getenv(keepTestBinaryEnv) == "" {
		_ = os.RemoveAll(testBinaryDir)
	}
}

var buildTestBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "leadkit-guard-test-bin-*")
	if err != nil {
		return "", err
	}
	testBinaryDirMu.Lock()
	testBinaryDir = dir
	testBinaryDirMu.Unlock()
	bin := filepath.Join(dir, "leadkit")
	// -cover instruments the binary so the subprocess CLI tests below report coverage
	// (GOCOVERDIR is passed through by sandboxedEnv; `make cover` sets it). Without it the
	// code those tests exercise is unmeasurable.
	//
	// ONLY when GOCOVERDIR is set: a -cover binary with no GOCOVERDIR prints
	// "warning: GOCOVERDIR not set, no coverage data emitted" on startup, which lands in
	// stdout and breaks every test that parses the CLI's JSON output.
	// -X main.trustHomeEnv=1: a link-time constant that makes the unconfigured state reachable from
	// a sandbox whose HOME is a temp directory and never the account's (see account_home.go). The
	// refusal for a HOME that is not the account's is covered in-process, where it can be stubbed.
	args := []string{"build", "-ldflags", "-X main.trustHomeEnv=1", "-o", bin, "."}
	if os.Getenv("GOCOVERDIR") != "" {
		args = []string{"build", "-cover", "-ldflags", "-X main.trustHomeEnv=1", "-o", bin, "."}
	}
	cmd := exec.Command("go", args...)
	if _, err := cmd.CombinedOutput(); err != nil {
		return "", err
	}
	return bin, nil
})

func testBinary(t *testing.T) string {
	t.Helper()
	bin, err := buildTestBinary()
	if err != nil {
		t.Fatalf("build test binary: %v", err)
	}
	return bin
}

// runCLI execs bin with args INSIDE A MANDATORY SANDBOX — see the package doc above
// for why this is non-optional and applies regardless of extraEnv/flags. Two
// independent layers, because HOME alone doesn't cover everything: (1) HOME
// redirects every ~-relative default (~/.claude/settings.json, ~/Library/LaunchAgents,
// queue.DefaultRoot()'s $HOME fallback); (2) cmd.Dir + a LEADKIT_PROFILE pointing at
// a path that doesn't exist (config.Load() falls back to Defaults() cleanly — see its
// doc comment) stop config.Load()'s upward directory walk from ever finding THIS
// repo's own real leadkit.yaml — that walk is cwd-based, not HOME-based, so HOME
// alone would NOT have prevented incident #2. extraEnv entries (e.g. a test's own
// LEADKIT_PROFILE pointing at a real fixture) override the sandbox defaults, never
// the reverse — duplicates are resolved by rebuilding the env from scratch rather
// than appending (a naive append can leave two HOME= entries, whose precedence is OS/
// libc-dependent — not something to rely on for a safety mechanism).
// sandboxedEnv builds the mandatory-sandbox environment runCLI uses — HOME pinned
// to home, LEADKIT_PROFILE pinned to a path that deliberately doesn't exist inside
// it (unless extraEnv overrides it), every other var passed through unchanged.
// Factored out so a test that needs its own exec.Command (a custom cmd.Dir or
// stdin runCLI doesn't support — see gate_scope_test.go) still gets the identical
// sandboxing runCLI provides, rather than a second hand-rolled copy of it.
func sandboxedEnv(home string, extraEnv []string) []string {
	overrides := map[string]string{
		"HOME":            home,
		"LEADKIT_PROFILE": filepath.Join(home, "leadkit.yaml"), // deliberately absent unless extraEnv overrides it
	}
	if gc := realGoCache(); gc != "" {
		overrides["GOCACHE"] = gc
	}
	for _, kv := range extraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			overrides[k] = v
		}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, replaced := overrides[k]; replaced {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

func runCLI(t *testing.T, bin string, extraEnv []string, args ...string) (output string, exitCode int) {
	t.Helper()
	home := t.TempDir()

	cmd := exec.Command(bin, args...)
	cmd.Dir = home // never this repo — see the sandboxing note above
	cmd.Env = sandboxedEnv(home, extraEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %s %v: %v\n%s", bin, args, err, out)
	return "", -1
}

// snapshotFiles reads every regular file under root and every symlink's target,
// keyed by path relative to root — a byte-for-byte before/after comparator.
func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if info.Mode()&os.ModeSymlink != 0 {
			tgt, _ := os.Readlink(path)
			out[rel] = "symlink:" + tgt
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr == nil {
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

func TestCLI_AuditApply_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	storeRoot := filepath.Join(root, "tasks")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// One dangling-symlink fixture: task file written, linked, then the store file
	// removed — exactly the "safe" case a real --apply --confirm would fix.
	taskPath := filepath.Join(storeRoot, "1-x.md")
	if err := os.WriteFile(taskPath, []byte("---\ntitle: t\nstatus: pending\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pendingDir := filepath.Join(queueRoot, "cr-1", "pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(taskPath, filepath.Join(pendingDir, "1-x.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(taskPath); err != nil {
		t.Fatal(err)
	}

	profilePath := filepath.Join(root, "leadkit.yaml")
	profileYAML := "queue_root: queue\nstore_root: tasks\naudit_apply_log: audit-apply.jsonl\n"
	if err := os.WriteFile(profilePath, []byte(profileYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// This OWN fixture profile overrides runCLI's sandbox default — the sandbox's
	// HOME/cwd are still isolated underneath, this just points profile discovery at
	// this test's own deliberate fixture instead of "no profile found".
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	before := snapshotFiles(t, root)

	out, code := runCLI(t, bin, env, "audit", "--apply")
	if code == 0 {
		t.Errorf("`audit --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line in the output, got:\n%s", out)
	}

	after := snapshotFiles(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("`audit --apply` alone must not touch the tree at all\nbefore=%v\nafter=%v", before, after)
	}

	// A bare dry-run (no flags) must also leave everything untouched.
	if _, code := runCLI(t, bin, env, "audit"); code != 0 {
		t.Errorf("bare `audit` should exit 0, got %d", code)
	}
	if !reflect.DeepEqual(before, snapshotFiles(t, root)) {
		t.Error("bare `audit` (no --apply at all) must not touch the tree")
	}

	// NOW actually apply, with confirm — this is the only path allowed to mutate.
	outApplied, codeApplied := runCLI(t, bin, env, "audit", "--apply", "--confirm")
	if codeApplied != 0 {
		t.Fatalf("`audit --apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	if reflect.DeepEqual(before, snapshotFiles(t, root)) {
		t.Error("`audit --apply --confirm` should have reaped the dangling symlink — tree unchanged")
	}
	if _, err := os.Lstat(filepath.Join(pendingDir, "1-x.md")); !os.IsNotExist(err) {
		t.Error("the dangling symlink should be gone after --apply --confirm")
	}
}

func TestCLI_Init_BareApplyRefusesAndNeverMutates(t *testing.T) {
	bin := testBinary(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"hooks":{}}`)
	if err := os.WriteFile(settingsPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// launchAgentsDir is ALSO explicit here, on top of runCLI's HOME sandboxing that
	// would already redirect defaultLaunchAgentsDir() — belt and braces, since this
	// exact test is the one that caused incident #2.
	launchAgentsDir := t.TempDir()

	// A profile fixture whose queue_root actually EXISTS: init skips the daemon
	// plan when the resolved queue root is missing (docs/build-tasks/
	// init-consolidation-matcher-aware.md Fix 3), and the sandbox's default queue
	// (~/Projects/.../legacy-migration/queue under an ephemeral HOME) never does.
	// The queue lives under a "legacy-migration" project dir so the resolved slug
	// stays "legacy-migration" (the value asserted on the plist filenames below).
	profileDir := t.TempDir()
	queueRoot := filepath.Join(profileDir, "legacy-migration", "queue")
	if err := os.MkdirAll(queueRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(profileDir, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: legacy-migration/queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"LEADKIT_PROFILE=" + profilePath}

	out, code := runCLI(t, bin, env, "init", "--apply", "--settings", settingsPath, "--launch-agents-dir", launchAgentsDir)
	if code == 0 {
		t.Errorf("`init --apply` alone must exit non-zero, got 0. output:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Errorf("want a REFUSED line in the output, got:\n%s", out)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("`init --apply` alone must leave settings.json byte-for-byte unchanged\nbefore=%s\nafter=%s", original, after)
	}
	if entries, _ := os.ReadDir(launchAgentsDir); len(entries) != 0 {
		t.Errorf("`init --apply` alone must write no plists either, found: %v", entries)
	}

	if _, code := runCLI(t, bin, env, "init", "--settings", settingsPath, "--launch-agents-dir", launchAgentsDir); code != 0 {
		t.Errorf("bare `init` (no --apply) should exit 0")
	}
	if after2, _ := os.ReadFile(settingsPath); string(after2) != string(original) {
		t.Error("bare `init` (no --apply at all) must not touch settings.json")
	}

	// --skip-launchctl: this process cannot inject the runLaunchctl stub into a
	// SEPARATE binary (see ApplyDaemonPlan's doc comment) — the plists still get
	// written (asserted below), but no real launchctl call is ever made.
	outApplied, codeApplied := runCLI(t, bin, env,
		"init", "--apply", "--confirm", "--settings", settingsPath,
		"--launch-agents-dir", launchAgentsDir, "--skip-launchctl")
	if codeApplied != 0 {
		t.Fatalf("`init --apply --confirm` should exit 0, got %d. output:\n%s", codeApplied, outApplied)
	}
	written, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) == string(original) {
		t.Error("`init --apply --confirm` should have written the leadkit hooks — file unchanged")
	}
	if !strings.Contains(string(written), "leadkit hook push-guard") {
		t.Errorf("written settings.json missing the push-guard hook entry:\n%s", written)
	}

	if runtime.GOOS == "darwin" {
		// waker + sweep (per-project) + the account-level burn-enforce daemon
		// (dev.leadkit.burn — docs/build-tasks/burn-auto-enforce.md).
		if entries, err := os.ReadDir(launchAgentsDir); err != nil || len(entries) != 3 {
			t.Errorf("want 3 plists written to the sandboxed launch-agents-dir, got %v (err=%v)", entries, err)
		}
		if _, err := os.Stat(filepath.Join(launchAgentsDir, "dev.leadkit.burn.plist")); err != nil {
			t.Error("account-level burn-enforce plist not written")
		}
		// The profile fixture's queue_root (.../legacy-migration/queue) exists, so
		// the daemon plan is produced — and its project-root basename
		// "legacy-migration" is the resolved slug the plist filenames carry
		// (see docs/build-tasks/per-project-daemons.md: labels/filenames are
		// namespaced per project so a second project's init can't clobber this one's).
		if _, err := os.Stat(filepath.Join(launchAgentsDir, "dev.leadkit.waker.legacy-migration.plist")); err != nil {
			t.Error("namespaced waker plist not written")
		}
	}
}
