package main

// End to end through the real binary: a reviewer can set a base and see it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// baseRepo is a repo with two commits: the base a reviewer reads from, and the one reviewed.
func baseRepo(t *testing.T) (repo, first, second string) {
	t.Helper()
	repo, first = gitRepoWithOneCommit(t, "git@example.com:org/based.git")
	if err := os.WriteFile(filepath.Join(repo, "two.md"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "two"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, first, strings.TrimSpace(string(out))
}

func TestCLI_GateRecordBase_IsStoredAndGateShowReportsIt(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo, first, second := baseRepo(t)
	recordsDir := filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = home
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatalf("exec %v: %v", args, err)
		}
		return string(out), 0
	}

	// code-review recorded WITH a base (a ref, resolved to a sha); critique WITHOUT one.
	if out, code := run("gate", "record", "--repo", repo, "--sha", second, "--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", first); code != 0 {
		t.Fatalf("record --base: exit %d\n%s", code, out)
	}
	if out, code := run("gate", "record", "--repo", repo, "--sha", second, "--layer", "critique", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "crit-1"); code != 0 {
		t.Fatalf("record critique: exit %d\n%s", code, out)
	}

	raw, err := os.ReadFile(filepath.Join(recordsDir, second+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"base": "`+first+`"`) != 1 {
		t.Errorf("exactly the code-review layer carries the base, as a full sha:\n%s", raw)
	}

	out, code := run("gate", "show", "--repo", repo, "--sha", second)
	if code != 0 {
		t.Fatalf("gate show: exit %d\n%s", code, out)
	}
	want := "(covers " + first[:12] + "..." + second[:12] + ")"
	if !strings.Contains(out, want) {
		t.Errorf("gate show must say what the layer covers (%q):\n%s", want, out)
	}
	if strings.Count(out, "(covers") != 1 {
		t.Errorf("only the layer that recorded a base says it covers anything:\n%s", out)
	}
	js, code := run("gate", "show", "--repo", repo, "--sha", second, "--json")
	if code != 0 || !strings.Contains(js, `"base": "`+first+`"`) {
		t.Errorf("--json reports the base: exit %d\n%s", code, js)
	}

	// refusals reach the user as exit 2, and write nothing
	if out, code := run("gate", "record", "--repo", repo, "--sha", second, "--layer", "code-review", "--verdict", "changes-needed", "--by", "cr-2", "--base", first); code != 2 || !strings.Contains(out, "ready verdict only") {
		t.Errorf("a base on a dissent is refused: exit %d\n%s", code, out)
	}
	if out, code := run("gate", "record", "--repo", repo, "--sha", second, "--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", "no-such-ref"); code != 2 || !strings.Contains(out, "does not resolve") {
		t.Errorf("an unresolvable base is refused: exit %d\n%s", code, out)
	}
	if out, code := run("gate", "record", "--repo", repo, "--sha", second, "--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", second); code != 2 || !strings.Contains(out, "not rewritten") {
		t.Errorf("a different base is refused: exit %d\n%s", code, out)
	}
}

func TestCLI_GateVerdictBase_ReachesTheRecord(t *testing.T) {
	bin := testBinary(t)
	projRoot := t.TempDir()
	profilePath := filepath.Join(projRoot, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nenforced_remotes: [based]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(projRoot, "tasks")
	crPending := filepath.Join(projRoot, "queue", "code-review", "pending")
	critPending := filepath.Join(projRoot, "queue", "critique", "pending")
	for _, d := range []string{tasksDir, crPending, critPending} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo, first, second := baseRepo(t)
	ticketPath := filepath.Join(tasksDir, "9-example.md")
	ticket := "---\ntitle: t\nstatus: in_review\nowner: dev-owner\nrepo: " + repo + "\nsha: " + second + "\nstage: code-review\ngate: true\n---\n\n# body\n"
	if err := os.WriteFile(ticketPath, []byte(ticket), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(crPending, "9-example.md")
	if err := os.Symlink(ticketPath, link); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := []string{"LEADKIT_PROFILE=" + profilePath, "LEADKIT_QUEUE=", "HOME=" + home}

	// the dry-run plan says what would be recorded, base included, and writes nothing
	out, code := runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", first, "--dry-run")
	if code != 0 || !strings.Contains(out, "base="+first) {
		t.Errorf("the dry-run plan names the base: exit %d\n%s", code, out)
	}
	recordPath := filepath.Join(home, ".claude", "review-gate", "records", second+".json")
	if _, err := os.Stat(recordPath); err == nil {
		t.Fatal("a dry-run writes no record")
	}

	// the dry-run refuses what the real run refuses
	out, code = runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "changes-needed", "--by", "cr-1", "--base", first, "--dry-run")
	if code == 0 || !strings.Contains(out, "ready verdict only") {
		t.Errorf("a dry-run with --base on changes-needed must refuse: exit %d\n%s", code, out)
	}
	out, code = runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", "no-such-ref", "--dry-run")
	if code == 0 || !strings.Contains(out, "does not resolve") {
		t.Errorf("a dry-run with an unresolvable --base must refuse: exit %d\n%s", code, out)
	}

	out, code = runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--no-mutant", "test fixture", "--by", "cr-1", "--base", first)
	if code != 0 {
		t.Fatalf("gate verdict --base: exit %d\n%s", code, out)
	}
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("record not written: %v", err)
	}
	if !strings.Contains(string(raw), `"base": "`+first+`"`) {
		t.Errorf("gate verdict must carry --base into the record:\n%s", raw)
	}
}

// A role other than the one that made a verdict must not widen it.
func TestCLI_GateRecordBase_AnotherRoleCannotWidenALayer(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo, first, second := baseRepo(t)
	recordsDir := filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := sandboxedEnv(home, []string{"LEADKIT_PROFILE=" + profile})
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = home
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatalf("exec %v: %v", args, err)
		}
		return string(out), 0
	}
	rec := func(layer, by string, extra ...string) (string, int) {
		args := []string{"gate", "record", "--repo", repo, "--sha", second, "--layer", layer, "--verdict", "ready", "--no-mutant", "test fixture", "--by", by}
		return run(append(args, extra...)...)
	}

	if out, code := rec("code-review", "cr-1"); code != 0 {
		t.Fatalf("code-review: exit %d\n%s", code, out)
	}
	if out, code := rec("critique", "crit-1"); code != 0 {
		t.Fatalf("critique: exit %d\n%s", code, out)
	}

	out, code := rec("code-review", "crit-2", "--base", first)
	if code != 2 || !strings.Contains(out, "REFUSED") {
		t.Errorf("the critique role widening code-review must be refused: exit %d\n%s", code, out)
	}
	show, code := run("gate", "show", "--repo", repo, "--sha", second)
	if code != 0 {
		t.Fatalf("gate show: exit %d\n%s", code, show)
	}
	if strings.Contains(show, "by crit-2") || strings.Contains(show, "(covers") {
		t.Errorf("the refused amend must leave code-review under cr-1 with no coverage:\n%s", show)
	}
	if !regexp.MustCompile(`code-review\s+ready\s+by cr-1\b`).MatchString(show) {
		t.Errorf("code-review must still stand under cr-1:\n%s", show)
	}

	// the role that made the verdict can still add the base
	if out, code := rec("code-review", "cr-2", "--base", first); code != 0 {
		t.Fatalf("a same-role amend is allowed: exit %d\n%s", code, out)
	}
	show, _ = run("gate", "show", "--repo", repo, "--sha", second)
	if !regexp.MustCompile(`code-review\s+ready\s+by cr-1\b.*\(covers ` + first[:12] + `\.\.\.`).MatchString(show) {
		t.Errorf("the same-role amend should be recorded and shown:\n%s", show)
	}
}
