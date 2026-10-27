// Package testrun is `leadkit test run` — a uniform "run the relevant suites on
// a committed sha, FRESH, and report a distilled verdict" helper. It supports the
// test-runner agent (team/agents/test-runner.md), a green-gate between an owner
// and the review pool: standardizes WHICH suites run (affected-first when
// available, else an inferred fresh full run — never relying on a cached
// "reported green" that the -count=1/fresh-run requirement exists specifically to
// rule out) and distills raw test-runner output into green/red + failing names.
//
// Read-only w.r.t. the task store — it runs tests inside the target repo clone
// and writes nothing to tasks/queue. It never calls a tracker/git/gh mutating
// command; the only external process it runs is the test suite itself (and a
// `git rev-parse` to verify the checked-out sha).
package testrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Verdict is one `test run` outcome.
type Verdict struct {
	SHA     string   `json:"sha"`
	Verdict string   `json:"verdict"` // "green" | "red"
	Command string   `json:"command,omitempty"`
	Failing []string `json:"failing,omitempty"`
}

// Options is one `test run` invocation's inputs.
type Options struct {
	Repo string // path to the repo clone (required)
	SHA  string // the committed sha to test (required) — refuses if HEAD != SHA

	// Affected, when true (the CLI's own default), TRIES the affected-tests
	// subset first — AffectedCommand if set, else a repo-local tokensave index if
	// one already exists (see hasTokensaveIndex's own doc comment for why this
	// package never triggers a FRESH tokensave index as a side effect). No
	// mechanism available is not an error: it silently falls through to the full,
	// inferred suite, matching the spec's "default on IF available" framing.
	Affected bool

	// TestCommand/AffectedCommand are profile per-repo overrides (`leadkit test
	// run`'s CLI wrapper resolves these from Profile.Tests before calling Run) —
	// set, they win outright over any inference.
	TestCommand     string
	AffectedCommand string

	// Timeout bounds the actual test-suite run (not the git rev-parse check).
	// Zero means DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout is how long Run waits for the suite before reporting "timed out"
// as a red verdict.
const DefaultTimeout = 15 * time.Minute

// runFunc runs command (via `sh -c`, so any shell syntax — pipes, flags, && — just
// works uniformly) inside dir, returning its combined stdout+stderr, its exit code
// (0 = success), and a Go error ONLY for a genuine exec-level failure (the shell
// itself couldn't start, or a timeout) — a non-zero exit from a real test run is a
// normal red verdict, never an error. Overridable so tests never execute a real
// test suite (or a real git command) — see testrun_test.go.
var runFunc = func(dir string, timeout time.Duration, command string) (output string, exitCode int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), -1, fmt.Errorf("timed out after %s", timeout)
	}
	if runErr == nil {
		return string(out), 0, nil
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), -1, runErr
}

// Run verifies opts.Repo's checked-out HEAD matches opts.SHA, picks the suite
// command (profile override > affected > inferred), runs it fresh, and distills
// the result. A sha mismatch is a hard refusal (a Go error) — the runner must
// test the enqueued sha, never whatever happens to be checked out, so this never
// silently runs the wrong commit's tests.
func Run(opts Options) (Verdict, error) {
	head, _, err := runFunc(opts.Repo, 10*time.Second, "git rev-parse HEAD")
	if err != nil {
		return Verdict{}, fmt.Errorf("testrun: git rev-parse HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if head != opts.SHA {
		return Verdict{}, fmt.Errorf("testrun: refusing — %s HEAD is %s, not the requested sha %s (must test the enqueued sha, not whatever's checked out)",
			opts.Repo, shortSHA(head), shortSHA(opts.SHA))
	}

	command, err := selectCommand(opts)
	if err != nil {
		return Verdict{}, err
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	output, exitCode, runErr := runFunc(opts.Repo, timeout, command)
	if runErr != nil {
		return Verdict{SHA: opts.SHA, Verdict: "red", Command: command, Failing: []string{"(runner error: " + runErr.Error() + ")"}}, nil
	}
	if exitCode == 0 {
		return Verdict{SHA: opts.SHA, Verdict: "green", Command: command}, nil
	}
	return Verdict{SHA: opts.SHA, Verdict: "red", Command: command, Failing: parseFailing(output)}, nil
}

// selectCommand picks the suite command: TestCommand override wins outright;
// else, if Affected, try AffectedCommand override or a pre-existing tokensave
// index; else infer from the repo's own files (go.mod -> a fresh `go test`,
// package.json -> its package manager's test script).
func selectCommand(opts Options) (string, error) {
	if opts.TestCommand != "" {
		return opts.TestCommand, nil
	}
	if opts.Affected {
		if opts.AffectedCommand != "" {
			return opts.AffectedCommand, nil
		}
		if cmd, ok := affectedCommand(opts.Repo); ok {
			return cmd, nil
		}
		// No affected mechanism available — not an error, fall through to the
		// full inferred suite (the spec's "default on IF available" framing).
	}
	return inferCommand(opts.Repo)
}

// affectedCommand returns tokensave's own affected-tests invocation, but ONLY
// when a tokensave index ALREADY exists for this repo. It deliberately never
// runs a bare `tokensave` invocation that could trigger tokensave's own
// interactive "no index found — create one now?" auto-init prompt: that command
// was observed (during this feature's own development) to silently create a
// `.tokensave/` dir AND rewrite the repo's `.gitignore` as a side effect of a
// single innocuous invocation — exactly the kind of unrelated mutation a
// read-only test-run helper must never risk causing in someone else's clone.
func affectedCommand(repo string) (string, bool) {
	if _, err := exec.LookPath("tokensave"); err != nil {
		return "", false
	}
	if !hasTokensaveIndex(repo) {
		return "", false
	}
	return "tokensave tool run_affected_tests", true
}

// hasTokensaveIndex reports whether repo already has a local tokensave index —
// see affectedCommand's own doc comment for why this gate exists.
func hasTokensaveIndex(repo string) bool {
	info, err := os.Stat(filepath.Join(repo, ".tokensave"))
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// inferCommand infers the standard suite command from the repo's own files:
// go.mod -> a FRESH `go test ./... -count=1` (-count=1 forces a real run, no
// cache staleness — the exact "reported-green-off-cache" failure mode this
// package exists to rule out); package.json -> its package manager's own `test`
// script (pnpm/yarn/npm, picked from whichever lockfile is present).
func inferCommand(repo string) (string, error) {
	if fileExists(filepath.Join(repo, "go.mod")) {
		return "go test ./... -count=1", nil
	}
	if fileExists(filepath.Join(repo, "package.json")) {
		return packageManagerTestCommand(repo), nil
	}
	return "", fmt.Errorf("testrun: could not infer a test command for %s (no go.mod or package.json, and no test_command override set)", repo)
}

func packageManagerTestCommand(repo string) string {
	switch {
	case fileExists(filepath.Join(repo, "pnpm-lock.yaml")):
		return "pnpm test"
	case fileExists(filepath.Join(repo, "yarn.lock")):
		return "yarn test"
	default:
		return "npm test"
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
