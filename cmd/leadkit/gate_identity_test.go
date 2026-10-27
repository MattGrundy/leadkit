package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateAs runs the real binary with CLAUDE_AGENT_NAME set to identity ("" leaves it unset).
func gateAs(t *testing.T, bin, home, profile, identity string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = home
	extra := []string{"LEADKIT_PROFILE=" + profile}
	if identity != "" {
		extra = append(extra, "CLAUDE_AGENT_NAME="+identity)
	}
	cmd.Env = sandboxedEnv(home, extra)
	if identity == "" {
		// the sandbox inherits the parent environment; make sure the variable is genuinely absent
		var env []string
		for _, kv := range cmd.Env {
			if !strings.HasPrefix(kv, "CLAUDE_AGENT_NAME=") {
				env = append(env, kv)
			}
		}
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v", args, err)
	return "", -1
}

// TestCLI_GateRecord_ByIsBoundToTheSessionsIdentity drives the verb through the real binary. With
// $CLAUDE_AGENT_NAME set, the name a verdict is recorded under is that identity: omitted -by uses it, and
// a different one is refused with nothing written, which is how an agent started as a code reviewer is
// stopped from casually recording as the critique role. Unset, the self-asserted name stands
// and says so.
func TestCLI_GateRecord_ByIsBoundToTheSessionsIdentity(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/bound.git")
	recordsDir := filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := scopeTestFakeSHA
	record := func(identity string, extra ...string) (string, int) {
		args := append([]string{"gate", "record", "--repo", repo, "--sha", sha, "--verdict", "ready", "--no-mutant", "test fixture"}, extra...)
		return gateAs(t, bin, home, profile, identity, args...)
	}
	show := func() string {
		out, code := gateAs(t, bin, home, profile, "", "gate", "show", "--repo", repo, "--sha", sha)
		if code != 0 {
			t.Fatalf("gate show (exit %d):\n%s", code, out)
		}
		return out
	}

	// A different name than the identity: refused, naming both, and nothing is written.
	out, code := record("cr-owner-5", "--layer", "critique", "--by", "crit-owner-1")
	if code != 2 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "cr-owner-5") || !strings.Contains(out, "crit-owner-1") {
		t.Fatalf("a mismatched -by must be refused naming both names (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(recordsDir); err == nil {
		entries, _ := os.ReadDir(recordsDir)
		if len(entries) != 0 {
			t.Errorf("a refused record wrote %v", entries)
		}
	}

	// No -by: the identity is the name, and the record says the name was bound.
	out, code = record("cr-owner-5", "--layer", "code-review")
	if code != 0 || !strings.Contains(out, "(by cr-owner-5)") || !strings.Contains(out, "identity: $CLAUDE_AGENT_NAME") {
		t.Fatalf("recording as the identity (exit %d):\n%s", code, out)
	}
	if s := show(); !strings.Contains(s, "by cr-owner-5") || strings.Contains(s, "self-asserted") {
		t.Errorf("gate show must present a bound name as such:\n%s", s)
	}

	// Repeating the identity as -by is fine.
	if out, code = record("crit-owner-1", "--layer", "critique", "--by", "crit-owner-1"); code != 0 {
		t.Fatalf("-by equal to the identity (exit %d):\n%s", code, out)
	}

	// Unset: the flag stands as before, and both the record line and gate show say it was not checked.
	out, code = gateAs(t, bin, home, profile, "", "gate", "record", "--repo", repo, "--sha", sha, "--layer", "code-review", "--verdict", "changes-needed", "--by", "anyone")
	if code != 0 || !strings.Contains(out, "(by anyone)") || !strings.Contains(out, "self-asserted") {
		t.Fatalf("with no identity the flag is accepted and flagged (exit %d):\n%s", code, out)
	}
	s := show()
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(line, "by anyone") && !strings.Contains(line, "(name self-asserted)"):
			t.Errorf("the self-asserted layer's own line must be marked: %q", line)
		case strings.Contains(line, "by crit-owner-1") && strings.Contains(line, "self-asserted"):
			t.Errorf("the bound critique layer's line must NOT be marked: %q", line)
		}
	}
	if !strings.Contains(s, "by anyone") || !strings.Contains(s, "by crit-owner-1") {
		t.Errorf("fixture: both layers should show:\n%s", s)
	}

	// gate verdict binds the same way, before it reads the ticket or touches the queue.
	out, code = gateAs(t, bin, home, profile, "cr-owner-5", "gate", "verdict", "--ticket", "queue/code-review/pending/none.md", "--verdict", "ready", "--no-mutant", "test fixture", "--by", "crit-owner-1")
	if code != 2 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "crit-owner-1") {
		t.Errorf("gate verdict must refuse a name other than the identity (exit %d):\n%s", code, out)
	}
}

// gate verdict carries the identity into the record the same way: omitted -by is the identity and the
// layer on disk says the name was bound, which is what a later reader (finalise) can check.
func TestCLI_GateVerdict_StoresTheBoundNameInTheRecord(t *testing.T) {
	bin := testBinary(t)
	projRoot := t.TempDir()
	profilePath := filepath.Join(projRoot, "leadkit.yaml")
	if err := os.WriteFile(profilePath, []byte("queue_root: queue\nstore_root: tasks\nenforced_remotes: [based]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(projRoot, "tasks")
	crPending := filepath.Join(projRoot, "queue", "code-review", "pending")
	for _, d := range []string{tasksDir, crPending, filepath.Join(projRoot, "queue", "critique", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo, _, second := baseRepo(t)
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
	env := []string{"LEADKIT_PROFILE=" + profilePath, "LEADKIT_QUEUE=", "HOME=" + home, "CLAUDE_AGENT_NAME=cr-owner-7"}

	out, code := runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--no-mutant", "test fixture", "--dry-run")
	if code != 0 || strings.Contains(out, "identity: $CLAUDE_AGENT_NAME") {
		t.Fatalf("a dry run records nothing, so it says nothing about how a name was obtained (exit %d):\n%s", code, out)
	}
	out, code = runCLI(t, bin, env, "gate", "verdict", "--ticket", link, "--verdict", "ready", "--mutants", "verdict-path mutant killed by TestVerdictPath")
	if code != 0 || !strings.Contains(out, "identity: $CLAUDE_AGENT_NAME") {
		t.Fatalf("gate verdict (exit %d):\n%s", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "review-gate", "records", second+".json"))
	if err != nil {
		t.Fatalf("record not written: %v", err)
	}
	for _, want := range []string{`"by": "cr-owner-7"`, `"by_bound": true`, `"mutation_evidence": "verdict-path mutant killed by TestVerdictPath"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the record lacks %s:\n%s", want, raw)
		}
	}
}

// unboundNote is the stderr line warnUnboundReviewer prints. It is NOT byNote's "self-asserted" (that is the
// post-record text), so a test asserting this string cannot pass on the wrong output.
const unboundNote = "is a reviewer role but $CLAUDE_AGENT_NAME is unset"

// TestCLI_Gate_WarnsOnAnUnboundReviewerName pins the warning at BOTH gate call sites, by mutation: delete
// the warnUnboundReviewer call from runGateRecord or runGateVerdict and the matching case here fails. The
// warning goes out before anything else is read, so gate verdict is driven at a ticket that does not exist.
func TestCLI_Gate_WarnsOnAnUnboundReviewerName(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	repo := gitRepoNoCommits(t, "git@example.com:org/warn.git")
	recordsDir := filepath.Join(t.TempDir(), "records")
	profile := filepath.Join(repo, "leadkit.yaml")
	if err := os.WriteFile(profile, []byte("queue_root: queue\nstore_root: tasks\nrecords_dir: "+recordsDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := func(identity string, extra ...string) (string, int) {
		args := append([]string{"gate", "record", "--repo", repo, "--sha", scopeTestFakeSHA, "--layer", "code-review", "--verdict", "ready", "--no-mutant", "test fixture"}, extra...)
		return gateAs(t, bin, home, profile, identity, args...)
	}
	verdict := func(identity string, extra ...string) (string, int) {
		args := append([]string{"gate", "verdict", "--ticket", "queue/code-review/pending/none.md", "--verdict", "ready", "--no-mutant", "test fixture"}, extra...)
		return gateAs(t, bin, home, profile, identity, args...)
	}

	for _, c := range []struct {
		name string
		run  func(identity string, extra ...string) (string, int)
	}{{"gate record", record}, {"gate verdict", verdict}} {
		// Unbound reviewer-role name: noted, and quoted back.
		out, _ := c.run("", "--by", "cr-x")
		if !strings.Contains(out, unboundNote) || !strings.Contains(out, `"cr-x"`) {
			t.Errorf("%s: an unbound -by cr-x must emit the warning:\n%s", c.name, out)
		}
		// Bound, whether -by is omitted or repeated: silent.
		for _, extra := range [][]string{nil, {"--by", "cr-x"}} {
			if out, _ := c.run("cr-x", extra...); strings.Contains(out, unboundNote) {
				t.Errorf("%s: a bound name (-by %v) must not warn:\n%s", c.name, extra, out)
			}
		}
		// Not a reviewer role: nothing to warn about.
		if out, _ := c.run("", "--by", "dev-x"); strings.Contains(out, unboundNote) {
			t.Errorf("%s: a non-reviewer name must not warn:\n%s", c.name, out)
		}
		// The unset default is "claude", also silent. This pins today's gap on purpose (ticket 309 owns it),
		// so closing it is a visible change to this line rather than an accident.
		if out, _ := c.run(""); strings.Contains(out, unboundNote) {
			t.Errorf("%s: the unset default name \"claude\" does not warn today (ticket 309):\n%s", c.name, out)
		}
	}
}
