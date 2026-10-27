package milestone

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/hierarchy"
	"mgpp.dev/leadkit/internal/prcreate"
)

// This file holds the REAL signal resolvers the CLI wires. Every one degrades to
// Unverified (never a false Met) on any uncertainty, so an unreachable store / offline
// forge / unrecorded gate keeps the idle-gate closed rather than falsely opening it.
// Tests never construct these — they inject fakes for the interfaces in milestone.go.

// taskCounter is the real `tasks` signal: it REUSES internal/hierarchy's rollup query
// (OpenTasksForMilestone → LoadSubtasks/LoadIssues/Parse), never a forked reader. It
// needs issuesRoot as well as storeRoot so a task tagged only by its parent Issue rolls
// up through that Issue to the milestone/epic.
type taskCounter struct{ storeRoot, issuesRoot string }

// NewTaskCounter returns the real hierarchy-backed task counter over storeRoot (tasks/*.md
// + <storeRoot>/milestones.yaml) and issuesRoot (the Issue level).
func NewTaskCounter(storeRoot, issuesRoot string) TaskCounter {
	return taskCounter{storeRoot: storeRoot, issuesRoot: issuesRoot}
}

func (t taskCounter) OpenTaskCount(milestoneID string) (int, bool, error) {
	return hierarchy.OpenTasksForMilestone(t.storeRoot, t.issuesRoot, milestoneID)
}

// gateResolver is the real `gate` signal: it REUSES internal/gate records. A named
// sha/branch is met only when the record exists AND every RequiredLayer verdict is
// "ready"; a record present but not fully ready is Unmet; no record (or a read error)
// is Unverified — the gate hasn't been recorded, so done can't be confirmed.
type gateResolver struct {
	g    *gate.Gate
	repo string
}

// NewGateResolver returns the real gate-record-backed resolver. repo is the git repo
// dir used to rev-parse a branch ref to its sha (the record key); "" uses the process
// cwd, matching internal/gate's own convention.
func NewGateResolver(g *gate.Gate, repo string) GateResolver {
	return gateResolver{g: g, repo: repo}
}

func (gr gateResolver) Ready(ref string) (State, error) {
	sha := ref
	if gr.g != nil && gr.g.Git != nil {
		if full, ok := gr.g.Git.RevParse(gr.repo, ref); ok {
			sha = full
		}
	}
	rec, err := gr.g.ReadRecord(sha)
	if err != nil {
		// A missing record means "not recorded yet" → unverified, never Unmet: the
		// gate simply hasn't run, distinct from ran-and-failed.
		return Unverified, nil
	}
	if len(gate.Missing(rec, gr.g.Layers())) > 0 {
		return Unmet, nil
	}
	return Met, nil
}

// ghPRResolver is the real `pr` signal: it REUSES internal/prcreate's forge-CLI Runner
// seam (ExecRunner shells to gh) so it's read-only and, in tests, injectable. Any gh
// failure (offline, not authed, unknown ref) is Unverified — never a false Met.
type ghPRResolver struct {
	runner prcreate.Runner
	repo   string // fallback gh -R <owner/name> when a condition names no repo; "" lets gh infer from cwd
}

// NewGHPRResolver returns the real gh-backed PR resolver. Pass prcreate.ExecRunner{} for
// the real forge; a fake Runner in tests. repo is a fallback used only when a `pr`
// condition names no repo of its own (today the CLI passes "").
func NewGHPRResolver(runner prcreate.Runner, repo string) PRResolver {
	return ghPRResolver{runner: runner, repo: repo}
}

// Merged shells `gh pr view <ref> [-R <repo>] --json state`. The per-condition repo (from
// the DoD Condition's `repo:`) wins; the resolver's own repo is the fallback; empty means
// no -R, so gh resolves against the cwd repo (today's back-compat behavior).
func (pr ghPRResolver) Merged(ref, repo string) (State, error) {
	r := repo
	if r == "" {
		r = pr.repo
	}
	argv := []string{"gh", "pr", "view", ref}
	if r != "" {
		argv = append(argv, "-R", r)
	}
	argv = append(argv, "--json", "state", "-q", ".state")
	out, err := pr.runner.Run(argv)
	if err != nil {
		return Unverified, nil
	}
	switch strings.TrimSpace(strings.ToUpper(out)) {
	case "MERGED":
		return Met, nil
	case "OPEN", "CLOSED":
		return Unmet, nil
	default:
		return Unverified, nil
	}
}

// execScriptRunner is the REAL `type: script` runner. It EXECUTES arbitrary shell — a
// lead+user-authored command from milestones.yaml, at the invoking user's privileges and
// with the invoking environment — so it lives behind the milestone.enable_script_signals
// opt-in (evalScript never calls it when disabled). The verdict is the EXIT CODE ONLY:
// a clean run (any exit code) returns err==nil with the code; a could-not-run (binary not
// found, non-executable) returns a non-timeout err; a run that exceeds the ctx deadline
// returns context.DeadlineExceeded. Combined stdout+stderr is captured for the detail but
// NEVER interpreted as success. Tests never construct this — they inject a fake.
type execScriptRunner struct{}

// NewScriptRunner returns the real exec-backed script runner.
func NewScriptRunner() ScriptRunner { return execScriptRunner{} }

func (execScriptRunner) Run(ctx context.Context, scriptOrPath, workdir string) (int, string, error) {
	// A value that names an existing regular file is exec'd as that file; anything else
	// is an inline command run via `bash -c`. evalScript hands over the already-resolved
	// absolute path for the file case, so this stat is cwd-independent.
	var cmd *exec.Cmd
	if info, err := os.Stat(scriptOrPath); err == nil && info.Mode().IsRegular() {
		cmd = exec.CommandContext(ctx, scriptOrPath)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", scriptOrPath)
	}
	cmd.Dir = workdir
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	// A timeout (ctx deadline) is surfaced explicitly so evalScript reads it as unverified,
	// distinct from a real run error.
	if ctx.Err() == context.DeadlineExceeded {
		return -1, string(out), context.DeadlineExceeded
	}
	if err != nil {
		// A non-zero exit is NOT a run error — extract the code and report success-of-run
		// so the exit→state mapping (1 → unmet, ≥2 → unverified) applies.
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out), nil
		}
		// Could not start/find the command (not found, non-executable) → a run error.
		return -1, string(out), err
	}
	return 0, string(out), nil
}
