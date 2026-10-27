package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 223, 197's Case C. HOME is mislocated, so a repo governed only by its external config cannot
// be found, and a healthy profile for some OTHER project is in effect via $LEADKIT_PROFILE. The profile in
// effect does not mention the repo, which read as "out of scope" and planned the push. A repo that
// evidently is a leadkit project (it has a .leadkit/ directory) is refused instead; one with no sign of
// leadkit is allowed.
func TestCLIPush_CaseC_AHealthyUnrelatedProfileCannotVouchForARepoThatIsALeadkitProject(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\n")
	unrelated := filepath.Join(m.session.dir, "leadkit.yaml")
	home := t.TempDir() // a HOME holding no leadkit config: "mislocated"

	governed := m.unprofiled.dir
	if err := os.MkdirAll(filepath.Join(governed, ".leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	stranger := cliProject(t, t.TempDir(), "stranger", "git@example.com:org/stranger.git", "")

	push := func(repo string, env ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, "push", "--repo", repo, "--branch", "feat/x", "--dry-run")
		cmd.Dir = home
		cmd.Env = sandboxedEnv(home, append([]string{"LEADKIT_PROFILE=" + unrelated, "REVIEW_GATE_BYPASS="}, env...))
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}

	// The exact Case C invocation, against a repo that is a leadkit project: REFUSED, naming the sign,
	// the path searched and the HOME, and not attributing it to policy.
	out, code := push(governed)
	if code != 1 {
		t.Fatalf("Case C must refuse (exit %d):\n%s", code, out)
	}
	for _, want := range []string{"cannot be known", ".leadkit directory", "leadkit project", "HOME=" + `"` + home + `"`, "external config"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "out of scope") || strings.Contains(out, "by policy") {
		t.Errorf("it must not be reported as a policy or as out of scope:\n%s", out)
	}

	// Controls. A repo with no sign of leadkit stays allowed, with only the existing note.
	out, code = push(stranger.dir)
	if code != 0 || !strings.Contains(out, "out of scope of the profile in effect") || strings.Contains(out, "cannot be known") {
		t.Errorf("a repo with no .leadkit/ must be allowed (exit %d):\n%s", code, out)
	}

	// Bypass: the env form overrides the refusal, loudly...
	out, code = push(governed, "REVIEW_GATE_BYPASS=hotfix")
	if code != 0 {
		t.Errorf("REVIEW_GATE_BYPASS must still override (exit %d):\n%s", code, out)
	}
	// ...and so does the BYPASS file in the records dir the gate reads.
	recs := filepath.Join(home, ".claude", "review-gate", "records")
	if err := os.MkdirAll(recs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recs, "BYPASS"), []byte("deliberate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code = push(governed); code != 0 {
		t.Errorf("a BYPASS file must still override (exit %d):\n%s", code, out)
	}
	_ = os.Remove(filepath.Join(recs, "BYPASS"))

	// Once the repo's own profile is findable it is governed by it, not refused as unfindable.
	cfgDir := filepath.Join(home, ".claude", "leadkit", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "unprofiled.yaml"), []byte("queue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code = push(governed); strings.Contains(out, "cannot be known") {
		t.Errorf("a repo whose external config is found must not be refused as unfindable (exit %d):\n%s", code, out)
	}
}

// pushNoProfile is the seam: with the target unfindable and the session's profile healthy, only the
// target's own leadkit sign changes the answer.
func TestPushNoProfile_OnlyTheTargetsOwnSignOverridesAHealthySession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session := filepath.Join(t.TempDir(), "leadkit.yaml")
	if err := os.WriteFile(session, []byte("queue_root: ./queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", session)
	repo := t.TempDir()

	if why := pushNoProfile(repo); why != "" {
		t.Fatalf("no sign of leadkit and a healthy session profile is a decision, not a problem: %q", why)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".leadkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	why := pushNoProfile(repo)
	if !strings.Contains(why, ".leadkit directory") || !strings.Contains(why, "HOME=") {
		t.Errorf("a .leadkit/ directory with no profile found must be a problem naming the search and HOME: %q", why)
	}
	// The answer is hookProfileProblem's, not a second check's.
	if want := hookProfileProblem(repo); why != want {
		t.Errorf("push must share push-gate's signal:\n got  %q\n want %q", why, want)
	}
}
