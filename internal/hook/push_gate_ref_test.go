package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// The raw-git push gate must judge the commit(s) the push PUBLISHES — the
// ref named on the command line — never the HEAD of whichever checkout the session
// happens to sit in. These tests put the session's project on one sha and push another.

const (
	shaGated   = "1111111111111111111111111111111111111111" // has a full passing record
	shaUngated = "2222222222222222222222222222222222222222" // no record
	shaGated2  = "3333333333333333333333333333333333333333" // has a full passing record
)

// refPushEnv is a session sitting in repo whose HEAD is sessionHead, with branches
// feat/ungated -> shaUngated, feat/gated -> shaGated2 and feat/gated-b -> shaGated, and
// passing records for shaGated and shaGated2 only. The mode is advisory (nil Mode).
func refPushEnv(t *testing.T, sessionHead string) (PushGateConfig, string) {
	t.Helper()
	repo := t.TempDir()
	recordsDir := t.TempDir()
	writeFullRecord(t, recordsDir, shaGated)
	writeFullRecord(t, recordsDir, shaGated2)
	g := &gate.Gate{
		RecordsDir: recordsDir,
		Git: fakeGateGit{
			remotes: map[string]string{repo: enforcedRemote},
			head:    sessionHead,
			refs: map[string]string{
				"feat/ungated": shaUngated,
				"feat/gated":   shaGated2,
				"feat/gated-b": shaGated,
			},
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	return PushGateConfig{ResolveGate: staticGate(g), Cwd: repo}, repo
}

// Fail-open case: the session's project is on a GATED
// sha, the pushed branch's head is UNGATED. Judging the session HEAD would allow it.
func TestPushGate_SessionOnGatedSha_PushOfUngatedBranch_IsRefused(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	res := PushGate(bashInput("git push origin feat/ungated"), cfg)
	assertDeny(t, res, "ungated branch pushed from a session sitting on a gated sha")
	if !strings.Contains(res.Stdout, shaUngated[:12]) {
		t.Errorf("the deny must name the PUSHED sha %s, got:\n%s", shaUngated[:12], res.Stdout)
	}
	if strings.Contains(res.Stdout, shaGated[:12]) {
		t.Errorf("the deny must not mention the session HEAD %s, got:\n%s", shaGated[:12], res.Stdout)
	}
}

// False-block case: the session's project is on an UNGATED sha, the
// pushed branch's head is gated and ready. Must be allowed.
func TestPushGate_SessionOnUngatedSha_PushOfGatedBranch_IsAllowed(t *testing.T) {
	cfg, _ := refPushEnv(t, shaUngated)
	assertAllow(t, PushGate(bashInput("git push origin feat/gated"), cfg), "gated branch pushed from a session sitting on an ungated sha")
}

// Every refspec spelling resolves its SOURCE side, not the session HEAD.
func TestPushGate_RefspecForms_JudgeTheSource(t *testing.T) {
	forms := []struct {
		name, cmd string
		wantDeny  bool
	}{
		{"bare branch", "git push origin feat/ungated", true},
		{"full ref", "git push origin refs/heads/feat/ungated", true},
		{"src:dst", "git push origin feat/ungated:feat/other", true},
		{"forced src:dst", "git push origin +feat/ungated:feat/other", true},
		{"flags before the remote", "git push -u --force-with-lease origin feat/ungated", true},
		{"-C target", "git -C REPO push origin feat/ungated", true},
		{"gated source onto an ungated name", "git push origin feat/gated:feat/ungated", false},
		{"gated source", "git push origin feat/gated-b", false},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			// The session HEAD is always the opposite of the pushed ref's state.
			head := shaGated
			if !f.wantDeny {
				head = shaUngated
			}
			cfg, repo := refPushEnv(t, head)
			cmd := strings.Replace(f.cmd, "REPO", repo, 1)
			res := PushGate(bashInput(cmd), cfg)
			if f.wantDeny {
				assertDeny(t, res, f.cmd)
			} else {
				assertAllow(t, res, f.cmd)
			}
		})
	}
}

// A push of several refs is only as gated as its least-gated ref.
func TestPushGate_MultipleRefs_OneUngated_IsRefused(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	res := PushGate(bashInput("git push origin feat/gated feat/ungated"), cfg)
	assertDeny(t, res, "one gated + one ungated ref")
	if !strings.Contains(res.Stdout, shaUngated[:12]) {
		t.Errorf("deny should name the ungated ref's sha, got:\n%s", res.Stdout)
	}
	assertAllow(t, PushGate(bashInput("git push origin feat/gated feat/gated-b"), cfg), "two gated refs")
}

// A bare push publishes the repo's current branch, so its HEAD IS the pushed ref.
func TestPushGate_BarePush_JudgesTheCurrentBranch(t *testing.T) {
	cfg, _ := refPushEnv(t, shaUngated)
	assertDeny(t, PushGate(bashInput("git push"), cfg), "bare push on an ungated HEAD")
	assertDeny(t, PushGate(bashInput("git push origin"), cfg), "remote-only push on an ungated HEAD")
	cfg, _ = refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push"), cfg), "bare push on a gated HEAD")
}

// If the pushed commits cannot be determined the push is REFUSED — never judged by
// falling back to the session HEAD, which is gated here and would otherwise let it by.
func TestPushGate_UndeterminablePush_IsRefusedNotJudgedByHead(t *testing.T) {
	cmds := map[string]string{
		"unresolvable ref":   "git push origin no/such-branch",
		"wildcard refspec":   "git push origin refs/heads/*:refs/heads/*",
		"--all":              "git push --all origin",
		"--mirror":           "git push --mirror origin",
		"--tags":             "git push --tags origin",
		"one bad of several": "git push origin feat/gated no/such-branch",
	}
	for name, cmd := range cmds {
		t.Run(name, func(t *testing.T) {
			cfg, _ := refPushEnv(t, shaGated)
			res := PushGate(bashInput(cmd), cfg)
			assertDeny(t, res, cmd)
			if !strings.Contains(res.Stdout, "could not determine") {
				t.Errorf("deny should say the commits could not be determined, got:\n%s", res.Stdout)
			}
			// Refused in enforce mode too.
			cfg.Mode = modeResolver(ModeEnforce)
			assertDeny(t, PushGate(bashInput(cmd), cfg), cmd+" (enforce)")
		})
	}
}

// An undeterminable push is still subject to scope and the documented bypass: the
// refusal is the gate's own ungated verdict, not a harsher one.
func TestPushGate_UndeterminablePush_HonoursScopeAndBypass(t *testing.T) {
	cfg, repo := refPushEnv(t, shaGated)
	cfg.ResolveGate = staticGate(&gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{repo: "git@github.com:other/unrelated.git"}, head: shaGated},
		EnforcedRemotes: []string{enforcedSubstr},
	})
	assertAllow(t, PushGate(bashInput("git push --all origin"), cfg), "out of scope")

	cfg, _ = refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="hotfix" git push --all origin`), cfg), "inline bypass")
}

// A push that only deletes refs publishes no commit, so there is nothing to gate.
func TestPushGate_DeleteOnlyPush_IsAllowed(t *testing.T) {
	for _, cmd := range []string{
		"git push origin :feat/old",
		"git push origin --delete feat/old",
		"git push origin -d feat/old",
	} {
		cfg, _ := refPushEnv(t, shaUngated)
		assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
	}
	// ...but a delete alongside a real push does not whitewash the push.
	cfg, _ := refPushEnv(t, shaGated)
	assertDeny(t, PushGate(bashInput("git push origin :feat/old feat/ungated"), cfg), "delete + ungated push")
}

// End to end with real git: the main checkout sits on a gated branch, and
// a different branch of the same repo (what an agent's worktree holds) is not gated.
func TestPushGate_RealGit_MainCheckoutGated_OtherBranchUngated(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(main, "init", "-q", "-b", "chore/main-branch")
	run(main, "remote", "add", "origin", enforcedRemote)
	write(filepath.Join(main, "a"))
	run(main, "add", "a")
	run(main, "commit", "-q", "-m", "base")
	run(main, "branch", "fix/ungated")
	wt := filepath.Join(t.TempDir(), "wt")
	run(main, "worktree", "add", "-q", wt, "fix/ungated")
	write(filepath.Join(wt, "b"))
	run(wt, "add", "b")
	run(wt, "commit", "-q", "-m", "work")

	mainHead := run(main, "rev-parse", "HEAD")
	ungated := run(main, "rev-parse", "fix/ungated")
	if mainHead == ungated {
		t.Fatal("test setup: the worktree branch should have moved off the main HEAD")
	}

	recordsDir := t.TempDir()
	writeFullRecord(t, recordsDir, mainHead) // the session project's own HEAD is fully gated
	g := &gate.Gate{RecordsDir: recordsDir, Git: realGit{}, EnforcedRemotes: []string{enforcedSubstr}}
	cfg := PushGateConfig{ResolveGate: staticGate(g), Cwd: main}

	assertDeny(t, PushGate(bashInput("git push origin fix/ungated"), cfg), "ungated worktree branch, session on a gated sha")
	// The same decision when the push is run from inside the worktree itself.
	assertDeny(t, PushGate(bashInput("cd "+wt+" && git push origin fix/ungated"), cfg), "same push from the worktree")

	writeFullRecord(t, recordsDir, ungated)
	assertAllow(t, PushGate(bashInput("git push origin fix/ungated"), cfg), "once the pushed head is gated")
}

// realGit is a gate.Git over the real git binary (gate's own is unexported).
type realGit struct{}

func (realGit) RevParse(repo, ref string) (string, bool) {
	args := []string{"rev-parse", ref}
	if repo != "" {
		args = append([]string{"-C", repo}, args...)
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func (realGit) RemoteURL(repo string) string {
	out, err := exec.Command("git", "-C", repo, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
