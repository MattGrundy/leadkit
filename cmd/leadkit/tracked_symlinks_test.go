package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ticket 119. Three commits captured symlinks named queue and tasks, pointing at absolute paths in
// a home directory: .gitignore listed `/queue/` and `/tasks/`, a trailing slash matches
// directories only, and git sees a symlink as a file, so `git add -A` took them. Two pins:
// no ignore rule may be blind to a symlink, and no tracked symlink may point at an absolute path.

// gitIn runs git in dir with identity and config pinned, so the host's settings change nothing.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// blindIgnoreRules returns the rules of a .gitignore that end in a slash.
func blindIgnoreRules(gitignore string) []string {
	var out []string
	for _, line := range strings.Split(gitignore, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if strings.HasSuffix(line, "/") {
			out = append(out, line)
		}
	}
	return out
}

func TestGitignore_NoRuleIsBlindToASymlink(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRootFromCwd(t), ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if blind := blindIgnoreRules(string(data)); len(blind) > 0 {
		t.Errorf("these .gitignore rules end in a slash, so they ignore a directory but not a symlink of the same name: %v", blind)
	}

	// The original reproduction, behaviourally: the repo's own .gitignore, a symlink named for
	// each local-state directory it lists, and git deciding.
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"queue", "tasks"} {
		if err := os.Symlink(t.TempDir(), filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	if status := gitIn(t, repo, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, "queue") || strings.Contains(status, "tasks") {
		t.Errorf("a symlink named queue or tasks is offered to `git add -A`:\n%s", status)
	}
}

func TestBlindIgnoreRules_FindsASlashRuleAndNothingElse(t *testing.T) {
	got := blindIgnoreRules("# comment/\n/queue/\n/tasks\n!keep/\n\nbin/\n*.log\n")
	if strings.Join(got, ",") != "/queue/,bin/" {
		t.Errorf("blindIgnoreRules = %v, want [/queue/ bin/]", got)
	}
}

// trackedAbsoluteSymlinks lists the tracked symlinks of the repo at dir whose target is absolute.
func trackedAbsoluteSymlinks(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(gitIn(t, dir, "ls-files", "-s"), "\n") {
		// "<mode> <object> <stage>\t<path>"
		meta, path, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(meta, "120000 ") {
			continue
		}
		target := gitIn(t, dir, "cat-file", "blob", strings.Fields(meta)[1])
		if filepath.IsAbs(target) {
			found = append(found, path+" -> "+target)
		}
	}
	return found
}

func TestRepo_TracksNoSymlinkToAnAbsolutePath(t *testing.T) {
	root := repoRootFromCwd(t)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("not a git checkout")
	}
	if found := trackedAbsoluteSymlinks(t, root); len(found) > 0 {
		t.Errorf("tracked symlinks point at absolute paths (a machine's home directory leaks into the repository):\n%s", strings.Join(found, "\n"))
	}
}

func TestTrackedAbsoluteSymlinks_FlagsOnlyTheAbsoluteOne(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	if err := os.Symlink("/Users/someone/project/queue", filepath.Join(repo, "queue")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("docs/readme.md", filepath.Join(repo, "readme")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "plain.txt"), []byte("/Users/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "x")
	got := trackedAbsoluteSymlinks(t, repo)
	if len(got) != 1 || !strings.HasPrefix(got[0], "queue -> /Users/someone/project/queue") {
		t.Errorf("want only the absolute symlink, got %v", got)
	}
}
