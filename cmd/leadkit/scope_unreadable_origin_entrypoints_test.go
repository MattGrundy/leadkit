package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 228, round 2. The first fix made the push-gate hook and `leadkit push` refuse a profiled repo
// whose remote is not named origin. The same repo still passed `gate check` and the git pre-push hook's
// `gate check-push` (both read only the ok-bool), and the refusals blamed the repo being "out of scope" and
// told the user to push "from the real repo", which is the attribution-to-policy ticket 197 removed. These
// run the real binary over the repo shapes the ticket names, per entry point.

type originRepo struct {
	dir  string
	head string
}

// mkOriginRepo builds a git repo whose only remote is called remoteName, with the given committed profile
// ("" for none).
func mkOriginRepo(t *testing.T, profile, remoteName string) originRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "noorigin")
	initRepoWithOrigin(t, dir, "")
	if remoteName != "" {
		gitRun(t, dir, "remote", "add", remoteName, "https://github.com/acme/noorigin.git")
	}
	if profile != "" {
		if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "profile")
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return originRepo{dir: dir, head: strings.TrimSpace(string(out))}
}

// runOriginCLI runs the binary in dir with a sandboxed HOME, stdin and extra env, returning the combined output
// and the exit code. REVIEW_GATE_BYPASS is cleared unless extra sets it.
func runOriginCLI(t *testing.T, bin, dir, stdin string, extra []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(sandboxedEnv(t.TempDir(), nil), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "REVIEW_GATE_BYPASS=")
	cmd.Env = append(cmd.Env, extra...)
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v", args, err)
	return "", -1
}

// originHookDeny runs `hook push-gate` for command from dir and returns the deny reason ("" for an allow).
func originHookDeny(t *testing.T, bin, dir, command string, extra ...string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"cwd": dir, "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	out, code := runOriginCLI(t, bin, dir, string(payload), extra, "hook", "push-gate")
	if code != 0 {
		t.Fatalf("hook push-gate exited %d: %s", code, out)
	}
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var res struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if res.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("unexpected hook output: %s", out)
	}
	return res.HookSpecificOutput.PermissionDecisionReason
}

const (
	enforceProfile  = "guards:\n  default_mode: enforce\n"
	advisoryProfile = "project_id: x\n"
)

// The refusal of a profiled repo whose origin cannot be read names THAT cause and its fix. It does not call
// the repo out of scope (a statement about policy, which this is not), and it does not tell the user to push
// "from the real repo", which is advice for a different failure.
func TestCLI_UnreadableOriginRefusalNamesItsOwnCauseAndFix(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	enforce := mkOriginRepo(t, enforceProfile, "github")

	check := func(entry, text string) {
		t.Helper()
		if !strings.Contains(text, "git remote rename <name> origin") {
			t.Errorf("%s: the refusal must give the fix, `git remote rename <name> origin`:\n%s", entry, text)
		}
		if !strings.Contains(text, "no remote named `origin`") {
			t.Errorf("%s: the refusal must name the cause (no remote named origin):\n%s", entry, text)
		}
		for _, wrong := range []string{"out of scope", "real repo", "resolvable HEAD"} {
			if strings.Contains(text, wrong) {
				t.Errorf("%s: the refusal says %q, which is the wrong cause or advice:\n%s", entry, wrong, text)
			}
		}
	}

	check("hook push-gate", originHookDeny(t, bin, enforce.dir, "git push github main"))

	out, code := runOriginCLI(t, bin, enforce.dir, "", nil, "gate", "check")
	if code != 1 || !strings.Contains(out, "BLOCK") {
		t.Fatalf("gate check must BLOCK (exit 1) in enforce mode, got exit %d:\n%s", code, out)
	}
	check("gate check", out)

	out, code = runOriginCLI(t, bin, enforce.dir, "", nil, "push", "--repo", enforce.dir, "--branch", "main", "--remote", "github", "--dry-run")
	if code == 0 {
		t.Fatalf("leadkit push must refuse, got exit 0:\n%s", out)
	}
	check("leadkit push", out)
}

// The matrix the ticket names, per entry point: enforce refuses, advisory allows with a message that says the
// scope is unresolved, an unprofiled repo and a repo whose remote IS named origin are unchanged.
func TestCLI_UnreadableOriginEveryEntryPoint(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)

	enforce := mkOriginRepo(t, enforceProfile, "github")
	enforceNoRemote := mkOriginRepo(t, enforceProfile, "")
	advisory := mkOriginRepo(t, advisoryProfile, "github")
	unprofiled := mkOriginRepo(t, "", "github")
	named := mkOriginRepo(t, enforceProfile, "origin")

	refs := func(r originRepo) string {
		return "refs/heads/main " + r.head + " refs/heads/main 0000000000000000000000000000000000000000\n"
	}
	cases := []struct {
		name       string
		repo       originRepo
		wantBlock  bool
		wantInGate string // substring of `gate check`'s output
		notInGate  string // substring that must NOT be in it
	}{
		{"enforce, remote github: refused", enforce, true, "no remote named `origin`", "out of scope"},
		{"enforce, no remote at all: refused", enforceNoRemote, true, "no remote named `origin`", "out of scope"},
		{"advisory, remote github: allowed, says unresolved", advisory, false, "scope unresolved", "out of scope"},
		{"unprofiled, remote github: allowed, out of scope", unprofiled, false, "out of scope", "scope unresolved"},
		{"enforce, remote named origin: judged on its record", named, true, "no review record", "scope unresolved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantCode := 0
			if tc.wantBlock {
				wantCode = 1
			}
			out, code := runOriginCLI(t, bin, tc.repo.dir, "", nil, "gate", "check")
			if code != wantCode {
				t.Errorf("gate check exit = %d, want %d:\n%s", code, wantCode, out)
			}
			if !strings.Contains(out, tc.wantInGate) || strings.Contains(out, tc.notInGate) {
				t.Errorf("gate check output must contain %q and not %q:\n%s", tc.wantInGate, tc.notInGate, out)
			}
			out, code = runOriginCLI(t, bin, tc.repo.dir, refs(tc.repo), nil, "gate", "check-push")
			if code != wantCode {
				t.Errorf("gate check-push exit = %d, want %d:\n%s", code, wantCode, out)
			}
		})
	}
}

// REVIEW_GATE_BYPASS is the escape hatch the refusal names, and in enforce mode it must lift this refusal:
// the gate read the scope before the bypass, so a repo whose remote is not named origin had no way out but
// renaming the remote.
func TestCLI_UnreadableOriginBypassLiftsTheRefusal(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	repo := mkOriginRepo(t, enforceProfile, "github")

	if originHookDeny(t, bin, repo.dir, "git push github main") == "" {
		t.Fatal("control: without the bypass the push is refused")
	}
	if d := originHookDeny(t, bin, repo.dir, "git push github main", "REVIEW_GATE_BYPASS=hotfix"); d != "" {
		t.Errorf("REVIEW_GATE_BYPASS must lift the refusal in enforce mode, got: %s", d)
	}
	if out, code := runOriginCLI(t, bin, repo.dir, "", []string{"REVIEW_GATE_BYPASS=hotfix"}, "gate", "check"); code != 0 || !strings.Contains(out, "BYPASS") {
		t.Errorf("gate check with the bypass: exit %d, want 0 and a BYPASS note:\n%s", code, out)
	}
}

// `gate show` is where the reader learns what an unresolved scope means for `gate check`: the statement is
// the user-facing description of the behaviour, and it differs from the out-of-scope one.
func TestCLI_GateShowExplainsAnUnresolvedScope(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	unresolved := mkOriginRepo(t, enforceProfile, "github")
	out, code := runOriginCLI(t, bin, unresolved.dir, "", nil, "gate", "show", "--sha", unresolved.head)
	if code != 0 || !strings.Contains(out, "scope: unresolved") || !strings.Contains(out, "refuses it in enforce mode") {
		t.Errorf("gate show must say an unresolved scope is refused in enforce mode (exit %d):\n%s", code, out)
	}
	unprofiled := mkOriginRepo(t, "", "github")
	out, code = runOriginCLI(t, bin, unprofiled.dir, "", nil, "gate", "show", "--sha", unprofiled.head)
	if code != 0 || !strings.Contains(out, "scope: out-of-scope") || !strings.Contains(out, "allows an ungoverned repo") || strings.Contains(out, "refuses it in enforce mode") {
		t.Errorf("an unprofiled repo keeps the out-of-scope statement (exit %d):\n%s", code, out)
	}
}

// The pre-push hook's footer was written for a missing review ("Record the missing layers ..."). For an
// unreadable origin that sends the reader to review a commit whose problem is a remote name, so the footer is
// the fix instead. A repo whose origin IS readable but unreviewed keeps the review footer.
func TestCLI_CheckPushFooterNamesTheFixForAnUnreadableOrigin(t *testing.T) {
	requireGit(t)
	bin := testBinary(t)
	refs := func(r originRepo) string {
		return "refs/heads/main " + r.head + " refs/heads/main 0000000000000000000000000000000000000000\n"
	}

	unreadable := mkOriginRepo(t, enforceProfile, "github")
	out, code := runOriginCLI(t, bin, unreadable.dir, refs(unreadable), nil, "gate", "check-push")
	if code != 1 {
		t.Fatalf("gate check-push must refuse (exit 1), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "git remote rename <name> origin") || !strings.Contains(out, "REVIEW_GATE_BYPASS") {
		t.Errorf("the footer must give the fix and the override:\n%s", out)
	}
	if strings.Contains(out, "Record the missing layers") {
		t.Errorf("the review footer is the wrong advice for an unreadable origin:\n%s", out)
	}

	named := mkOriginRepo(t, enforceProfile, "origin")
	out, code = runOriginCLI(t, bin, named.dir, refs(named), nil, "gate", "check-push")
	if code != 1 || !strings.Contains(out, "Record the missing layers") || strings.Contains(out, "git remote rename") {
		t.Errorf("a readable origin with no record keeps the review footer (exit %d):\n%s", code, out)
	}
}
