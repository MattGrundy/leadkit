package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// fakePushGitGate is a stub gate.Git for the native-push tests: it reports a fixed origin
// for scope and treats an already-full sha as resolving to itself (normalizeSHA identity).
// HEAD is deliberately NOT served here — the primitive resolves the target's HEAD through
// the INJECTED runGit (fakePushRunner), so a test can prove the sha judged is the TARGET
// clone's, never something the gate's own git returned.
type fakePushGitGate struct {
	remote string
}

func (f fakePushGitGate) RevParse(repo, ref string) (string, bool) { return ref, true }
func (f fakePushGitGate) RemoteURL(repo string) string             { return f.remote }

// pushGitCall is one recorded `git -C <repo> <args…>` invocation.
type pushGitCall struct {
	repo string
	args []string
}

// fakePushRunner records every git call and serves a per-repo HEAD, so a test can inject
// two repos at DIFFERENT HEADs and prove the primitive resolved the right one.
type fakePushRunner struct {
	heads   map[string]string // repo -> HEAD sha (for `rev-parse HEAD`)
	calls   []pushGitCall
	pushErr error
}

func (r *fakePushRunner) run(repo string, args ...string) (string, error) {
	r.calls = append(r.calls, pushGitCall{repo: repo, args: append([]string(nil), args...)})
	if len(args) >= 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		return r.heads[repo], nil
	}
	return "", r.pushErr // a `push` call
}

func (r *fakePushRunner) pushCalls() []pushGitCall {
	var out []pushGitCall
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "push" {
			out = append(out, c)
		}
	}
	return out
}

const (
	pushEnforcedRemote = "git@github.com:acmecorp/widgets.git"
	pushEnforcedSubstr = "acmecorp/widgets"
	pushFullSHA        = "abcdef0123456789abcdef0123456789abcdef01"
)

// writePushRecord writes a passing (both layers ready) review-gate record for sha.
func writePushRecord(t *testing.T, recordsDir, sha string) {
	t.Helper()
	rec := gate.Record{
		SHA: sha,
		Layers: map[string]gate.Layer{
			"code-review": {Verdict: "ready", By: "cr-1", Ts: "2026-01-01T00:00:00Z"},
			"critique":    {Verdict: "ready", By: "crit-1", Ts: "2026-01-01T00:00:00Z"},
		},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordsDir, sha+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pushHarness builds a pushDeps whose gate has a tempdir records dir and reports `remote`
// as the origin of every repo, with the given enforced_remotes list.
func pushHarness(t *testing.T, remote string, enforced []string, heads map[string]string) (pushDeps, *fakePushRunner, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	recordsDir := t.TempDir()
	g := &gate.Gate{RecordsDir: recordsDir, Git: fakePushGitGate{remote: remote}, EnforcedRemotes: enforced}
	runner := &fakePushRunner{heads: heads}
	var out, errb bytes.Buffer
	deps := pushDeps{
		resolveGate: func(string) *gate.Gate { return g },
		runGit:      runner.run,
		stdout:      &out,
		stderr:      &errb,
	}
	return deps, runner, recordsDir, &out, &errb
}

func TestPushRun_GatedSHA_Pushes(t *testing.T) {
	repo := t.TempDir()
	deps, runner, recordsDir, out, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	writePushRecord(t, recordsDir, pushFullSHA)

	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 0 {
		t.Fatalf("gated push should succeed (exit 0), got %d\nstdout: %s", code, out.String())
	}
	pushes := runner.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("want exactly one push call, got %d: %+v", len(pushes), runner.calls)
	}
	got := pushes[0]
	if got.repo != repo {
		t.Errorf("push -C repo = %q, want %q", got.repo, repo)
	}
	wantArgs := []string{"push", "--no-verify", "origin", pushFullSHA + ":refs/heads/feat/x"}
	if strings.Join(got.args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("push args = %v, want %v", got.args, wantArgs)
	}
	if !strings.Contains(out.String(), "gated") {
		t.Errorf("success line should tag [gated], got: %s", out.String())
	}
}

func TestPushRun_Ungated_Refuses_NoPush(t *testing.T) {
	repo := t.TempDir()
	deps, runner, _, _, errb := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	// No record written → ungated.
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 1 {
		t.Fatalf("ungated push should refuse with exit 1, got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("ungated push must NOT call git push, got: %+v", runner.calls)
	}
	if !strings.Contains(errb.String(), "no review record") {
		t.Errorf("refusal should name the missing-record reason, got: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "leadkit gate record") {
		t.Errorf("refusal should carry the how-to-record remediation, got: %s", errb.String())
	}
}

func TestPushRun_OutOfScope_PushesWithNote(t *testing.T) {
	repo := t.TempDir()
	// An enforced list that the origin does NOT match → out of scope.
	deps, runner, _, out, errb := pushHarness(t, "git@github.com:someorg/random.git", []string{pushEnforcedSubstr}, nil)
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 0 {
		t.Fatalf("out-of-scope push should proceed (exit 0), got %d\n%s", code, errb.String())
	}
	if len(runner.pushCalls()) != 1 {
		t.Fatalf("out-of-scope push should still push once, got: %+v", runner.calls)
	}
	if !strings.Contains(errb.String(), "out of scope") {
		t.Errorf("out-of-scope push should print the ungated-by-policy note, got: %s", errb.String())
	}
	if !strings.Contains(out.String(), "out-of-scope") {
		t.Errorf("success line should tag [out-of-scope], got: %s", out.String())
	}
}

func TestPushRun_Unverifiable_FailsClosed(t *testing.T) {
	repo := t.TempDir()
	// Origin unresolvable ("") WITH an enforced list → scopeUnresolved → OutcomeUnverifiable.
	deps, runner, _, _, errb := pushHarness(t, "", []string{pushEnforcedSubstr}, nil)
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 1 {
		t.Fatalf("unverifiable push must FAIL CLOSED (exit 1), got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("unverifiable push must NOT push, got: %+v", runner.calls)
	}
	if !strings.Contains(errb.String(), "fail-closed") {
		t.Errorf("refusal should name the deliberate fail-closed posture, got: %s", errb.String())
	}
}

func TestPushRun_ShortSHA_Rejected(t *testing.T) {
	repo := t.TempDir()
	deps, runner, _, _, errb := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: "abcdef0", remote: "origin"})
	if code != 2 {
		t.Fatalf("a short --sha prefix should be a usage error (exit 2), got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("a rejected short sha must not push, got: %+v", runner.calls)
	}
	if !strings.Contains(errb.String(), "full 40-char sha") {
		t.Errorf("error should explain the full-sha requirement, got: %s", errb.String())
	}
}

// TestPushRun_OmittedSHA_ResolvesTargetHEAD proves the sha judged is the TARGET clone's
// HEAD, not the invoking session's: two repos at DIFFERENT HEADs, a gate record for only
// one. Pushing the gated one succeeds; pushing the other (whose own HEAD is ungated) is
// refused — which can only happen if the primitive resolved EACH repo's OWN HEAD.
func TestPushRun_OmittedSHA_ResolvesTargetHEAD(t *testing.T) {
	repoGated := t.TempDir()
	repoOther := t.TempDir()
	headGated := "1111111111111111111111111111111111111111"
	headOther := "2222222222222222222222222222222222222222"
	heads := map[string]string{repoGated: headGated, repoOther: headOther}
	deps, runner, recordsDir, out, errb := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, heads)
	writePushRecord(t, recordsDir, headGated) // only the gated repo's HEAD has a record

	// The gated repo: HEAD resolves to headGated, which is gated → push succeeds.
	code := pushRun(deps, pushOpts{repo: repoGated, branch: "feat/g", remote: "origin"})
	if code != 0 {
		t.Fatalf("gated-repo HEAD push should succeed, got %d\n%s", code, errb.String())
	}
	pushes := runner.pushCalls()
	if len(pushes) != 1 || pushes[0].args[len(pushes[0].args)-1] != headGated+":refs/heads/feat/g" {
		t.Fatalf("should push the RESOLVED gated HEAD pinned to the branch, got: %+v", runner.calls)
	}
	if !strings.Contains(out.String(), headGated[:12]) {
		t.Errorf("output should name the resolved target HEAD %s, got: %s", headGated[:12], out.String())
	}

	// The other repo: HEAD resolves to headOther, which is UNGATED → refuse, no push.
	runner.calls = nil
	code = pushRun(deps, pushOpts{repo: repoOther, branch: "feat/o", remote: "origin"})
	if code != 1 {
		t.Fatalf("other-repo HEAD (ungated) push should refuse, got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("ungated other-repo HEAD must not push, got: %+v", runner.calls)
	}
}

func TestPushRun_Force_UsesForceWithLease(t *testing.T) {
	repo := t.TempDir()
	deps, runner, recordsDir, _, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	writePushRecord(t, recordsDir, pushFullSHA)
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin", force: true})
	if code != 0 {
		t.Fatalf("forced gated push should succeed, got %d", code)
	}
	pushes := runner.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("want one push, got: %+v", runner.calls)
	}
	var sawLease, sawBare bool
	for _, a := range pushes[0].args {
		if a == "--force-with-lease" {
			sawLease = true
		}
		if a == "--force" {
			sawBare = true
		}
	}
	if !sawLease {
		t.Errorf("force push should use --force-with-lease, got args: %v", pushes[0].args)
	}
	if sawBare {
		t.Errorf("force push must NEVER use bare --force, got args: %v", pushes[0].args)
	}
}

func TestPushRun_DryRun_NoPush(t *testing.T) {
	repo := t.TempDir()
	deps, runner, recordsDir, out, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	writePushRecord(t, recordsDir, pushFullSHA)

	// Gated + dry-run → exit 0, but NO push call.
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin", dryRun: true})
	if code != 0 {
		t.Fatalf("dry-run of a gated push should exit 0, got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("dry-run must NOT push, got: %+v", runner.calls)
	}
	if !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "would push") {
		t.Errorf("dry-run should print the would-push preview, got: %s", out.String())
	}
}

func TestPushRun_DryRun_Ungated_NonZero(t *testing.T) {
	repo := t.TempDir()
	deps, runner, _, _, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	// No record → ungated. A dry-run that WOULD be refused exits non-zero.
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin", dryRun: true})
	if code == 0 {
		t.Fatalf("dry-run of an ungated push should exit non-zero")
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("dry-run must not push, got: %+v", runner.calls)
	}
}

func TestPushRun_MissingArgs_Usage(t *testing.T) {
	deps, runner, _, _, errb := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	if code := pushRun(deps, pushOpts{branch: "feat/x"}); code != 2 {
		t.Errorf("missing --repo should be a usage error (exit 2), got %d", code)
	}
	if code := pushRun(deps, pushOpts{repo: t.TempDir()}); code != 2 {
		t.Errorf("missing --branch should be a usage error (exit 2), got %d", code)
	}
	if len(runner.pushCalls()) != 0 {
		t.Fatalf("usage errors must not push, got: %+v", runner.calls)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Errorf("should print usage, got: %s", errb.String())
	}
}

// TestPushRun_BypassEnv_Allows proves the process-env escape hatch works for the native
// primitive (no command parsing needed — the env IS set for the invoked leadkit process).
func TestPushRun_BypassEnv_Allows(t *testing.T) {
	repo := t.TempDir()
	deps, runner, _, out, _ := pushHarness(t, pushEnforcedRemote, []string{pushEnforcedSubstr}, nil)
	// In scope + no record, but bypassed via the env.
	t.Setenv("REVIEW_GATE_BYPASS", "hotfix in progress")
	code := pushRun(deps, pushOpts{repo: repo, branch: "feat/x", sha: pushFullSHA, remote: "origin"})
	if code != 0 {
		t.Fatalf("env-bypassed push should succeed, got %d", code)
	}
	if len(runner.pushCalls()) != 1 {
		t.Fatalf("bypassed push should push once, got: %+v", runner.calls)
	}
	if !strings.Contains(out.String(), "bypass") {
		t.Errorf("success line should tag [bypass], got: %s", out.String())
	}
}
