package doctor

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Git abstracts the two git-touching calls the branch-whitelist check needs — the
// current branch and the origin remote URL — so that check is testable without
// shelling out to a real git repository. Mirrors internal/gate's own Git seam
// (RevParse/RemoteURL), specialised to what doctor needs: a plain "current branch"
// read rather than a general ref resolution.
type Git interface {
	// CurrentBranch resolves repo's checked-out branch name ("" repo = process cwd).
	// ok is false on any failure (no repo, detached HEAD reads as ok=true with branch
	// "HEAD" — callers distinguish that themselves, matching push-guard's own
	// convention).
	CurrentBranch(repo string) (branch string, ok bool)
	// RemoteURL returns repo's origin remote URL, or "" if it can't be determined.
	RemoteURL(repo string) string
}

// execGit is the real Git: shells out to the git binary.
type execGit struct{}

// RealGit returns the real Git implementation (shells out to git) — what the actual
// `leadkit doctor` CLI wires in; tests inject their own stub instead.
func RealGit() Git { return execGit{} }

func (execGit) CurrentBranch(repo string) (string, bool) {
	out, err := runGit(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", false
	}
	return out, true
}

func (execGit) RemoteURL(repo string) string {
	out, err := runGit(repo, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return out
}

// lastCommitTime returns the committer timestamp of repo's most recent commit (HEAD)
// — the "new commits in the window" deliverable signal the productivity watcher
// correlates against activity. repo "" means the process cwd (same convention as the
// branch-whitelist check's git reads). ok is false on any failure (no repo, no
// commits, unparseable output) — the watcher then treats git as contributing no
// deliverable signal, never a false productive reading.
func lastCommitTime(repo string) (time.Time, bool) {
	out, err := runGit(repo, "log", "-1", "--format=%ct")
	if err != nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

func runGit(repo string, args ...string) (string, error) {
	full := args
	if repo != "" {
		full = append([]string{"-C", repo}, args...)
	}
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
