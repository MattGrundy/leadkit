package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
)

// The guard matrix (guard_target_matrix_test.go) pins how the HOOK guards resolve a target.
// These cover the CLI verbs: --repo locates the record and the sha, but the SCOPE decision
// must follow the target's own profile, not the one found by walking up from the process
// cwd, so the same sha and --repo must answer the same wherever the caller stands.
//
// They run the real binary with LEADKIT_PROFILE="" (so the profile is discovered from the
// cwd, as in a real session; runCLI's sandbox otherwise pins it to a file that does not
// exist, which would hide the very dependence under test) and vary ONLY the cwd.

type cliRepo struct {
	dir, sha string
}

// cliProject makes a git repo with an origin and one commit, plus (when profile != "") its
// own leadkit.yaml. The profile is committed to nothing; it only has to be on disk.
func cliProject(t *testing.T, parent, name, origin, profile string) cliRepo {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if origin != "" { // "" is a repo whose origin cannot be read: it has no origin remote at all
		git("remote", "add", "origin", origin)
	}
	if profile != "" {
		if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f.txt")
	git("commit", "-q", "-m", "init")
	return cliRepo{dir: dir, sha: git("rev-parse", "HEAD")}
}

// cliMatrix is the fixture: a SESSION project whose policy names some other repo, a TARGET
// project whose own policy gates its own origin, an UNPROFILED repo with an unrelated
// origin, and a neutral directory under no project at all.
type cliMatrix struct {
	session, target, unprofiled cliRepo
	neutral                     string
}

func newCLIMatrix(t *testing.T, sessionProfile string) cliMatrix {
	t.Helper()
	root := t.TempDir()
	m := cliMatrix{
		session:    cliProject(t, root, "sessionproj", "git@example.com:org/sessionproj.git", sessionProfile),
		target:     cliProject(t, root, "targetproj", "git@example.com:org/targetproj.git", "queue_root: ./queue\nstore_root: ./tasks\nrecords_dir: ./recs\n"),
		unprofiled: cliProject(t, root, "unprofiled", "git@example.com:org/unprofiled.git", ""),
		neutral:    filepath.Join(root, "neutral"),
	}
	if err := os.MkdirAll(m.neutral, 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

// atCwd runs the binary with cwd = dir and the profile discovered from it. The HOME is
// shared across calls so a record written by one call is visible to the next.
func atCwd(t *testing.T, bin, home, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = sandboxedEnv(home, []string{"LEADKIT_PROFILE="})
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

// The reproduction from the ticket: the session's own profile names OTHER repos, the target's
// own profile gates itself. The answer about the target must not depend on where we stand.
func TestCLIVerbs_GateCheck_ScopeFollowsTheTargetNotTheCwd(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\n")
	home := t.TempDir()

	cwds := map[string]string{"session project": m.session.dir, "target project": m.target.dir, "neutral dir": m.neutral}
	for name, cwd := range cwds {
		t.Run("in-scope target, no record, from "+name, func(t *testing.T) {
			out, code := atCwd(t, bin, home, cwd, "gate", "check", "--repo", m.target.dir, "--sha", m.target.sha)
			if code != 1 || !strings.Contains(out, "BLOCK") {
				t.Errorf("the target's own profile gates it, so with no record this must BLOCK from every cwd; got exit %d:\n%s", code, out)
			}
		})
		// CONTROL: without this, a verb that returned BLOCK for everything would pass above.
		t.Run("out-of-scope target, from "+name, func(t *testing.T) {
			out, code := atCwd(t, bin, home, cwd, "gate", "check", "--repo", m.unprofiled.dir, "--sha", m.unprofiled.sha)
			if code != 0 || !strings.Contains(out, "out of scope") {
				t.Errorf("an unprofiled repo no profile names is out of scope from every cwd; got exit %d:\n%s", code, out)
			}
		})
	}
}

// The ONE intended cwd-dependence: the session's enforced_remotes can only ADD scope. A
// session that declares a repo gates it even when the repo is unprofiled; a session that
// does not, does not. It is asserted so that removing the cwd-dependence cannot also remove
// this intended one.
func TestCLIVerbs_GateCheck_SessionRemotesOnlyEverWidenScope(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [unprofiled]\n")
	home := t.TempDir()

	out, code := atCwd(t, bin, home, m.session.dir, "gate", "check", "--repo", m.unprofiled.dir, "--sha", m.unprofiled.sha)
	if code != 1 {
		t.Errorf("the session declares this repo, so it is gated from the session: want BLOCK, got exit %d:\n%s", code, out)
	}
	out, code = atCwd(t, bin, home, m.neutral, "gate", "check", "--repo", m.unprofiled.dir, "--sha", m.unprofiled.sha)
	if code != 0 {
		t.Errorf("a session that does not declare it leaves it out of scope: want OK, got exit %d:\n%s", code, out)
	}
	// And the union never shrinks the target's own scope: the target is still gated here,
	// although this session's list does not name it.
	out, code = atCwd(t, bin, home, m.session.dir, "gate", "check", "--repo", m.target.dir, "--sha", m.target.sha)
	if code != 1 {
		t.Errorf("the session's list must not take the target out of its own scope: want BLOCK, got exit %d:\n%s", code, out)
	}
}

// check-push is a git hook: it runs in the repo being pushed, and the session's
// $LEADKIT_PROFILE (set for the whole session) must not decide that repo's scope.
func TestCLIVerbs_GateCheckPush_ScopeFollowsTheRepoNotLeadkitProfile(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\n")
	sessionProfile := filepath.Join(m.session.dir, "leadkit.yaml")

	stdin := "refs/heads/main " + m.target.sha + " refs/heads/main 0000000000000000000000000000000000000000\n"
	cmd := exec.Command(bin, "gate", "check-push")
	cmd.Dir = m.target.dir // git runs the hook in the repo being pushed
	cmd.Env = sandboxedEnv(t.TempDir(), []string{"LEADKIT_PROFILE=" + sessionProfile})
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	ee, isExit := err.(*exec.ExitError)
	if !isExit || ee.ExitCode() != 1 {
		t.Errorf("pushing the target, which its own profile gates, with no record must be denied even with the session's profile in $LEADKIT_PROFILE; err=%v\n%s", err, out)
	}
}

// `gate record` and `gate check` must agree on WHERE a repo's records live: the target's
// own records_dir, which is where push-gate reads. Record from the session's cwd, then
// check from the neutral dir: the second call must find what the first wrote.
func TestCLIVerbs_GateRecord_WritesWhereTheTargetsGateReads(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\n")
	home := t.TempDir()

	for _, r := range [][]string{{"code-review", "ready", "cr-agent"}, {"critique", "ready", "crit-agent"}} {
		out, code := atCwd(t, bin, home, m.session.dir, "gate", "record", "--repo", m.target.dir, "--sha", m.target.sha,
			"--layer", r[0], "--verdict", r[1], "--no-mutant", "test fixture", "--by", r[2])
		if code != 0 {
			t.Fatalf("record %s: exit %d\n%s", r[0], code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(m.target.dir, "recs", m.target.sha+".json")); err != nil {
		t.Errorf("the record must land in the TARGET's records_dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "review-gate", "records", m.target.sha+".json")); err == nil {
		t.Errorf("the record must not land in the session's default records dir")
	}
	for name, cwd := range map[string]string{"session": m.session.dir, "neutral": m.neutral, "target": m.target.dir} {
		out, code := atCwd(t, bin, home, cwd, "gate", "check", "--repo", m.target.dir, "--sha", m.target.sha)
		if code != 0 || !strings.Contains(out, "all layers ready") {
			t.Errorf("from the %s cwd, check must find the record that record wrote; got exit %d:\n%s", name, code, out)
		}
	}
}

// recordBothLayers records code-review and critique ready for sha in repo, from cwd, through
// `gate record` — the verb that decides where a repo's records are written.
func recordBothLayers(t *testing.T, bin, home, cwd string, repo cliRepo) {
	t.Helper()
	for _, r := range [][]string{{"code-review", "cr-agent"}, {"critique", "crit-agent"}} {
		out, code := atCwd(t, bin, home, cwd, "gate", "record", "--repo", repo.dir, "--sha", repo.sha,
			"--layer", r[0], "--verdict", "ready", "--no-mutant", "test fixture", "--by", r[1])
		if code != 0 {
			t.Fatalf("record %s: exit %d\n%s", r[0], code, out)
		}
	}
}

// `gate record` writes to the target's own records_dir, so every verb that READS a gate must
// look there too. The target here sets records_dir (./recs), which differs from the
// central default every un-resolved reader looks in.
//
// pr create --dry-run runs the real gate read (prcreate.GateReady) and prints its verdict, so
// the binary can be asked without a forge. Its scope is the session's authority entry for the
// target, so the session profile carries one; records_dir comes from the TARGET.
func TestCLIVerbs_PrCreate_ReadsTheRecordGateRecordWrote(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nauthority:\n  targetproj:\n    can_open_pr: true\n")
	home := t.TempDir()
	dryRun := func(cwd string) string {
		t.Helper()
		out, code := atCwd(t, bin, home, cwd, "pr", "create", "--dry-run", "--repo", m.target.dir,
			"--base", "main", "--head", m.target.sha, "--title", "t", "--body-file", "/dev/null")
		if code != 0 {
			t.Fatalf("pr create --dry-run from %s: exit %d\n%s", cwd, code, out)
		}
		return out
	}

	// CONTROL: nothing recorded yet, so the gate must say so. Without this, a gate line that
	// always read ok=true would pass below.
	if out := dryRun(m.session.dir); !strings.Contains(out, "gate:         ok=false") || !strings.Contains(out, "no review record") {
		t.Fatalf("before any record the gate must report no review record:\n%s", out)
	}

	recordBothLayers(t, bin, home, m.session.dir, m.target)
	if _, err := os.Stat(filepath.Join(m.target.dir, "recs", m.target.sha+".json")); err != nil {
		t.Fatalf("precondition: the record lands in the target's records_dir: %v", err)
	}
	// Only the session's cwd: the authority entry is the SESSION's grant and is read from the
	// session's profile, so from another cwd there is no authority and no gate to ask about.
	if out := dryRun(m.session.dir); !strings.Contains(out, "gate:         ok=true") || !strings.Contains(out, "all layers ready") {
		t.Errorf("pr create must find the record `gate record` wrote in the target's records_dir:\n%s", out)
	}
}

// The `pr create`/`pr merge` gate is one constructor (prGate), because pr merge's gate read
// sits behind `gh` and cannot be driven from a test binary. This pins its two halves
// separately: records_dir and project root follow the TARGET, scope follows the session's
// list plus the authority key (never the target's own).
func TestPrGate_RecordsFollowTheTargetScopeFollowsTheAuthority(t *testing.T) {
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\n")
	sess := &config.Profile{EnforcedRemotes: []string{"some-other-repo"}}

	g := prGate(m.target.dir, sess, "targetproj")
	targetReal, err := filepath.EvalSymlinks(m.target.dir) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(targetReal, "recs"); g.RecordsDir != want {
		t.Errorf("RecordsDir = %q, want the target profile's %q", g.RecordsDir, want)
	}
	if got := strings.Join(g.EnforcedRemotes, ","); got != "some-other-repo,targetproj" {
		t.Errorf("EnforcedRemotes = %q, want the session's list plus the authority key", got)
	}

	// An unprofiled target has no records_dir of its own: the central default stays.
	if g := prGate(m.unprofiled.dir, sess, "unprofiled"); g.RecordsDir != gate.DefaultRecordsDir() {
		t.Errorf("unprofiled RecordsDir = %q, want the default %q", g.RecordsDir, gate.DefaultRecordsDir())
	}
}

// `gate verdict` takes its repo from the TICKET, not from a flag, so the wiring that makes it
// judge that repo with that repo's own gate is a closure (GateForRepo) the review package
// cannot see being dropped, so only the binary can pin it. These run the binary with a
// ticket whose repo: is the target and a session whose own policy and records_dir are
// different, and observe WHERE the record lands and WHICH scope answered.
func TestCLIVerbs_GateVerdict_JudgesTheTicketsRepoWithItsOwnGate(t *testing.T) {
	bin := testBinary(t)

	newTicket := func(t *testing.T, m cliMatrix) string {
		t.Helper()
		base := "7-verdict.md"
		for _, d := range []string{"tasks", "queue/code-review/in-progress", "queue/critique/pending"} {
			if err := os.MkdirAll(filepath.Join(m.session.dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + m.target.dir + "\nsha: " + m.target.sha + "\nstage: code-review\ngate: true\n---\n\nbody\n"
		if err := os.WriteFile(filepath.Join(m.session.dir, "tasks", base), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(m.session.dir, "queue", "code-review", "in-progress", base)
		if err := os.Symlink(filepath.Join("..", "..", "..", "tasks", base), link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	const sessionProfile = "queue_root: ./queue\nstore_root: ./tasks\nenforced_remotes: [some-other-repo]\nrecords_dir: ./session-recs\n"

	for _, tc := range []struct {
		name string
		args []string // appended to the verb; with no --layer the layer is derived from a gate check
	}{
		{"layer given", []string{"--layer", "code-review"}},
		// Without --layer the verb first asks the gate whether the sha is already gated. The
		// session's policy names some other repo, so a session-scoped gate says "out of scope"
		// and records nothing; the target's own gate says there is no record.
		{"layer derived from the target's scope", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newCLIMatrix(t, sessionProfile)
			home := t.TempDir()
			ticket := newTicket(t, m)
			args := append([]string{"gate", "verdict", "--ticket", ticket, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1"}, tc.args...)
			out, code := atCwd(t, bin, home, m.session.dir, args...)
			if code != 0 {
				t.Fatalf("gate verdict: exit %d\n%s", code, out)
			}
			if strings.Contains(out, "already gated") || strings.Contains(out, "out of scope") {
				t.Errorf("the ticket's repo is gated by its own profile; the session's scope must not decide it:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(m.target.dir, "recs", m.target.sha+".json")); err != nil {
				t.Errorf("the record must land in the TARGET's records_dir: %v\n%s", err, out)
			}
			for _, wrong := range []string{
				filepath.Join(m.session.dir, "session-recs", m.target.sha+".json"),
				filepath.Join(home, ".claude", "review-gate", "records", m.target.sha+".json"),
			} {
				if _, err := os.Stat(wrong); err == nil {
					t.Errorf("the record must not land in %s", wrong)
				}
			}
		})
	}
}

// A project's milestone `gate` condition reads records through the profile's own
// records_dir, where `gate record` writes them; reading the central default would report a
// gated SHA as "unverified" and keep the milestone closed.
func TestCLIVerbs_MilestoneGate_ReadsTheProfilesRecordsDir(t *testing.T) {
	bin := testBinary(t)
	root := t.TempDir()
	proj := cliProject(t, root, "msproj", "git@example.com:org/msproj.git",
		"queue_root: ./queue\nstore_root: ./tasks\nrecords_dir: ./recs\nmilestones_file: ./milestones.yaml\n")
	artifact := "current_milestone: m1\ndod:\n  - name: gated\n    type: gate\n    sha: " + proj.sha + "\n"
	if err := os.WriteFile(filepath.Join(proj.dir, "milestones.yaml"), []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	// CONTROL: nothing recorded -> not met.
	if out, _ := atCwd(t, bin, home, proj.dir, "milestone", "status"); !strings.Contains(out, "unverified") {
		t.Fatalf("before any record the gate condition must read unverified:\n%s", out)
	}
	recordBothLayers(t, bin, home, proj.dir, proj)
	out, _ := atCwd(t, bin, home, proj.dir, "milestone", "status")
	if !strings.Contains(out, "met        [gate]") || strings.Contains(out, "unverified") {
		t.Errorf("milestone must find the record in the profile's records_dir:\n%s", out)
	}
}

// `pr merge` builds its gate behind a `gh` call, which a test binary cannot satisfy, so the
// site is not reachable from the binary. Pin it at the source: both PR verbs must build their
// gate with prGate. A bare gate.New() there reads the central default records dir and
// reports "no review record" for a SHA that is gated, for any target with its own records_dir.
func TestPrVerbs_BuildTheirGateWithPrGate(t *testing.T) {
	src, err := os.ReadFile("cmd_pr.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "prGate("); n != 2 {
		t.Errorf("cmd_pr.go calls prGate %d times, want 2 (pr create and pr merge)", n)
	}
	if strings.Contains(string(src), "gate.New()") {
		t.Error("cmd_pr.go builds a gate with gate.New(); use prGate so records_dir follows the target")
	}
}
