package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 197, the PR side, through the real binary: "no authority matched" has three different
// causes, and none of them may tell an agent to add an authority entry.
func TestCLIPRCreate_DryRun_TellsTheThreeCausesOfNoAuthorityApart(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\n")
	home := t.TempDir()
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	prCreate := func(cwd, repo string) string {
		out, _ := atCwd(t, bin, home, cwd, "pr", "create", "--dry-run", "--base", "main", "--head", "feat/x", "--title", "t", "--body-file", body, "--repo", repo)
		return out
	}
	authorityLine := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "authority:") {
				return l
			}
		}
		t.Fatalf("no authority line in:\n%s", out)
		return ""
	}

	// (a) no profile anywhere: an environment problem, with the search and the HOME named.
	out := prCreate(m.neutral, m.unprofiled.dir)
	line := authorityLine(out)
	if !strings.Contains(line, "profile NOT FOUND") || !strings.Contains(line, "HOME=") || !strings.Contains(line, "in-repo leadkit.yaml") {
		t.Errorf("(a) want 'profile NOT FOUND' naming HOME and the searched path, got: %s", line)
	}

	// (b) a profile that was read and has no entry for the origin: a policy fact, said as one.
	out = prCreate(m.session.dir, m.session.dir)
	line = authorityLine(out)
	if !strings.Contains(line, "no match in the profile") || strings.Contains(out, "NOT FOUND") {
		t.Errorf("(b) want 'no match in the profile' and no 'NOT FOUND', got:\n%s", out)
	}
	if !strings.Contains(out, "ask the repo owner") {
		t.Errorf("(b) the verdict must send the reader to the repo owner, got:\n%s", out)
	}

	// (c) a profile that exists and will not load, whose error quotes a URL with a credential.
	broken := cliProject(t, t.TempDir(), "brokenproj", "git@example.com:org/brokenproj.git",
		"brief_length_max: https://user:supersecrettoken@example.com/x\n")
	out = prCreate(broken.dir, broken.dir)
	line = authorityLine(out)
	if !strings.Contains(line, "profile NOT ACCEPTED") || !strings.Contains(line, "was not accepted") || strings.Contains(out, "NOT FOUND") {
		t.Errorf("(c) a refused profile is neither missing nor a policy: want 'was not accepted', got:\n%s", out)
	}
	if strings.Contains(out, "environment problem") || !strings.Contains(out, "fix it or the repo's origin") {
		t.Errorf("(c) a refused profile is not an environment problem: it must say to fix the profile or the origin, got:\n%s", out)
	}
	// (The YAML library truncates the scalar it quotes, so this cannot fail today; redaction itself
	// is pinned by TestRedactCredentials. It guards the day a loader error quotes more.)
	if strings.Contains(out, "supersecrettoken") {
		t.Errorf("(c) the credential in the loader's error must not be printed:\n%s", out)
	}

	// None of the three advises writing a grant.
	for _, o := range []string{prCreate(m.neutral, m.unprofiled.dir), prCreate(m.session.dir, m.session.dir), out} {
		if strings.Contains(strings.ToLower(o), "add an authority entry to") || strings.Contains(strings.ToLower(o), "add an authority entry yourself") {
			t.Errorf("a refusal must not advise adding an authority entry:\n%s", o)
		}
	}
}

// A remote can carry a credential. No output of push or of any pr verb, dry-run included, in any
// profile state, may print it: the origin line, the verdict, and every refusal built from it.
func TestCLI_NoOutputOfPushOrPRPrintsACredentialFromTheRemote(t *testing.T) {
	bin := testBinary(t)
	m := newCLIMatrix(t, "queue_root: ./queue\nstore_root: ./tasks\n")
	home := t.TempDir()
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const secret = "SECRETTOK123"
	remote := "https://deploy:" + secret + "@github.com/org/leaky.git"
	unprofiled := cliProject(t, t.TempDir(), "leaky", remote, "")
	broken := cliProject(t, t.TempDir(), "leakybroken", "https://deploy:p@s"+secret+"@github.com/org/leakybroken.git", "queue_root: [unclosed\n")

	shown := map[string]bool{} // per verb: did any state print the (redacted) origin?
	for _, c := range []struct{ name, cwd, repo string }{
		{"no profile anywhere", m.neutral, unprofiled.dir},
		{"profile read, no entry", m.session.dir, unprofiled.dir},
		{"profile refused", m.session.dir, broken.dir},
	} {
		for _, args := range [][]string{
			{"push", "--repo", c.repo, "--branch", "feat/x", "--dry-run"},
			{"pr", "create", "--dry-run", "--base", "main", "--head", "feat/x", "--title", "t", "--body-file", body, "--repo", c.repo},
			{"pr", "edit", "1", "--title", "t", "--dry-run", "--repo", c.repo}, // the number is positional
			{"pr", "merge", "--pr", "1", "--dry-run", "--repo", c.repo},
		} {
			out, _ := atCwd(t, bin, home, c.cwd, args...)
			if strings.Contains(out, secret) || strings.Contains(out, "deploy:") {
				t.Errorf("%s / %s leaks the remote's credential:\n%s", c.name, strings.Join(args[:2], " "), out)
			}
			verb := strings.Join(args[:2], " ")
			shown[verb] = shown[verb] || strings.Contains(out, "github.com/org/leaky")
		}
	}
	// Not vacuous, per verb: each pr verb prints the (redacted) origin in at least one state, so a
	// mis-called invocation (a usage line and nothing else) fails here instead of passing silently.
	// `push` prints no origin at all; its output is still scanned above.
	for _, verb := range []string{"pr create", "pr edit", "pr merge"} {
		if !shown[verb] {
			t.Errorf("`leadkit %s` never printed the origin in any state, so its leak check proves nothing (wrong invocation?)", verb)
		}
	}
}
