// Package review folds send-to-review.sh and gate-verdict.sh into leadkit: moving a
// ticket into the code-review/critique pipeline, and recording+reconciling a verdict.
//
// Both operate on the SAME queue overlay and tasks/ store leadkit/internal/queue
// already defines. ROLE-KEYED LANES (docs/build-tasks/role-keyed-lanes.md): the
// queue DESTINATION is always the canonical role's own lane (queue/code-review/,
// queue/critique/, queue/<owner-role>/, ...) — neither SendToReview nor GateVerdict
// resolves a role to a live instance dir via queue/ROLES any more. That file (and
// leadkit/internal/roles.Resolve) still exists for other consumers (e.g. `leadkit
// team capture`'s inbox-manifest snapshot), just not for this package's routing.
package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/queue"
)

// codeReviewRole is the fixed canonical role lane send-to-review always routes
// into — role-keyed-lanes.md: the LANE is the canonical role, not a resolved live
// instance. There is exactly one code-review lane in the whole operating model,
// so this needs no resolver — it's simply always "code-review".
const codeReviewRole = "code-review"

// Reviewer bundles the queue/tasks locations SendToReview operates over, so tests can
// point them at t.TempDir() fixtures instead of the real queue/tasks/ROLES. The zero
// value is unusable for the real thing — use New().
type Reviewer struct {
	QueueRoot string // .../queue
	TasksDir  string // .../tasks
}

// New returns a Reviewer wired to the real queue root and its sibling tasks/ dir.
func New() *Reviewer {
	root := queue.DefaultRoot()
	return &Reviewer{QueueRoot: root, TasksDir: filepath.Join(filepath.Dir(root), "tasks")}
}

// Result is what SendToReview did (or, if dryRun, would have done).
type Result struct {
	Base      string // ticket filename, e.g. "123-foo.md"
	Reviewer  string // the code-review ROLE lane — always "code-review" (codeReviewRole)
	SHA       string // full HEAD sha recorded onto the ticket
	QueueLink string // the queue/<reviewer>/pending/<base> symlink created (or would-be)
	DryRun    bool
}

// SendToReview is send-to-review.sh: moves task into the code-review reviewer's
// pending inbox. Every precondition is checked BEFORE anything moves — a refusal
// leaves the ticket and queue untouched. dryRun runs every precondition/resolution
// step unchanged, prints the resolved reviewer, the frontmatter fields that WOULD
// change, the stale entries that WOULD be removed, and the target symlink — then
// mutates nothing (no frontmatter write, no removal, no symlink).
func (rv *Reviewer) SendToReview(task, repo, owner string, dryRun bool) (Result, error) {
	base, canonicalPath, err := rv.resolveTicket(task)
	if err != nil {
		return Result{}, err
	}

	sha, err := checkSendPreconditions(repo)
	if err != nil {
		return Result{}, err
	}

	// Role-keyed lanes: the destination is ALWAYS the code-review role's own lane —
	// no queue/ROLES role->instance lookup. (Previously this resolved "code-review"
	// through roles.Resolve to whatever live instance dir (e.g. "cr-1") currently
	// served it; that indirection is gone from this routing path entirely — see
	// docs/build-tasks/role-keyed-lanes.md.)
	reviewer := codeReviewRole
	reviewerPending := filepath.Join(rv.QueueRoot, reviewer, "pending")
	if fi, err := os.Stat(reviewerPending); err != nil || !fi.IsDir() {
		return Result{}, fmt.Errorf("send-to-review: refuse — reviewer %q's pending dir does not exist: %s", reviewer, reviewerPending)
	}

	updates := []KV{
		{"sha", sha},
		{"repo", repo},
		{"stage", "code-review"},
		{"status", "in_review"},
		{"gate", "true"},
	}
	if owner != "" {
		updates = append(updates, KV{"owner", owner})
	}

	// A reviewer holding a live claim on this ticket is mid-review of the sha already sent. Sending it
	// again hands it to a second reviewer and puts a new sha under the first one's feet.
	if held, ok := queue.ClaimedByOther(filepath.Join(rv.QueueRoot, reviewer, "in-progress", base), "", time.Now()); ok {
		return Result{}, fmt.Errorf("send-to-review: refuse — %s is in %s's in-progress and %s. Re-sending now would give a second reviewer the same ticket. Wait for that review's verdict, or if the reviewer is gone `leadkit queue unlink` the entry", base, reviewer, held.String())
	}

	link := filepath.Join(reviewerPending, base)
	// The relative symlink target from link's directory to the canonical tasks/ file, computed
	// rather than a hardcoded "go up 3": that shortcut only held when TasksDir sat exactly at
	// QueueRoot's sibling "tasks", and a profile can put them anywhere. queue.RelativeLinkTarget
	// compares the symlink-RESOLVED spellings, because Rel between two spellings of one place
	// (macOS /var vs /private/var) produces a target that does not resolve.
	rel := linkTargetFor(reviewerPending, canonicalPath)

	// Refuse BEFORE mutating anything if the link we are about to make would dangle: a ticket
	// whose frontmatter says in_review and whose only lane entry resolves to nothing is lost. This
	// runs for a dry run too (it writes nothing), so a dry run cannot promise a link the real run
	// refuses.
	if !linkResolves(reviewerPending, rel) {
		return Result{}, fmt.Errorf("send-to-review: refuse — a link %s -> %s would not resolve to %s; nothing was changed", link, rel, canonicalPath)
	}

	if dryRun {
		stale, err := rv.listStaleQueueEntries(base, reviewer)
		if err != nil {
			return Result{}, fmt.Errorf("send-to-review: %w", err)
		}
		var changes []string
		for _, kv := range updates {
			changes = append(changes, kv.Key+"="+kv.Value)
		}
		fmt.Printf("DRY RUN: send-to-review: would set frontmatter %s\n", strings.Join(changes, ","))
		if len(stale) == 0 {
			fmt.Println("DRY RUN: send-to-review: no stale queue entries to remove")
		} else {
			for _, s := range stale {
				fmt.Printf("DRY RUN: send-to-review: would remove stale entry %s\n", s)
			}
		}
		fmt.Printf("DRY RUN: send-to-review: would symlink %s -> %s (nothing done)\n", link, rel)
		return Result{Base: base, Reviewer: reviewer, SHA: sha, QueueLink: link, DryRun: true}, nil
	}

	if err := UpdateFrontmatter(canonicalPath, updates); err != nil {
		return Result{}, fmt.Errorf("send-to-review: %w", err)
	}

	if err := rv.removeStaleQueueEntries(base, reviewer); err != nil {
		return Result{}, fmt.Errorf("send-to-review: %w", err)
	}

	_ = os.Remove(link) // clear any stale link at the destination itself (ln -sf semantics)
	if err := os.Symlink(rel, link); err != nil {
		return Result{}, fmt.Errorf("send-to-review: symlink %s into %s's pending: %w", base, reviewer, err)
	}
	if _, err := os.Stat(link); err != nil {
		_ = os.Remove(link)
		return Result{}, fmt.Errorf("send-to-review: the link %s -> %s does not resolve, so it was removed; the ticket is in no lane: %w", link, rel, err)
	}

	return Result{Base: base, Reviewer: reviewer, SHA: sha, QueueLink: link}, nil
}

// linkTargetFor and linkResolves are variables only so a test can force a dangling target and prove
// the two safety nets around it fire (the pre-mutation refusal and the post-create removal); with
// the helper right, no real input can make the link dangle.
var (
	linkTargetFor = queue.RelativeLinkTarget
	linkResolves  = queue.LinkResolves
)

// resolveTicket resolves task — an absolute path (typically a queue symlink), or a
// bare ticket name / name.md under TasksDir — to its canonical tasks/<base>. Queue
// entries are symlinks; EvalSymlinks follows to the real file rather than the entry.
func (rv *Reviewer) resolveTicket(task string) (base, canonicalPath string, err error) {
	p := task
	if !filepath.IsAbs(p) {
		name := p
		if !strings.HasSuffix(name, ".md") {
			name += ".md"
		}
		p = filepath.Join(rv.TasksDir, name)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", "", fmt.Errorf("send-to-review: refuse — ticket not found: %s", task)
	}
	base = filepath.Base(real)
	canonicalPath = filepath.Join(rv.TasksDir, base)
	if _, err := os.Stat(canonicalPath); err != nil {
		return "", "", fmt.Errorf("send-to-review: refuse — resolved ticket %s is not under the tasks dir %s", real, rv.TasksDir)
	}
	return base, canonicalPath, nil
}

// removeStaleQueueEntries drops any queue entry named base under */pending and
// */in-progress, except exceptReviewer's own pending — SendToReview (re)creates that
// one itself right after, so clearing it here would just be undone.
func (rv *Reviewer) removeStaleQueueEntries(base, exceptReviewer string) error {
	stale, err := rv.listStaleQueueEntries(base, exceptReviewer)
	if err != nil {
		return err
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove stale entry %s: %w", p, err)
		}
	}
	return nil
}

// listStaleQueueEntries finds (without removing) every queue entry named base under
// */pending and */in-progress, except exceptReviewer's own pending — the same
// exception removeStaleQueueEntries applies, since SendToReview (re)creates that one
// itself right after. Shared by the real removal and the --dry-run preview.
func (rv *Reviewer) listStaleQueueEntries(base, exceptReviewer string) ([]string, error) {
	agents, err := os.ReadDir(rv.QueueRoot)
	if err != nil {
		return nil, fmt.Errorf("list queue root %s: %w", rv.QueueRoot, err)
	}
	var stale []string
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		agent := a.Name()
		for _, status := range []string{"pending", "in-progress"} {
			if agent == exceptReviewer && status == "pending" {
				continue
			}
			p := filepath.Join(rv.QueueRoot, agent, status, base)
			if _, lerr := os.Lstat(p); lerr == nil {
				stale = append(stale, p)
			}
		}
	}
	return stale, nil
}

// checkSendPreconditions runs send-to-review.sh's precondition checks against repo —
// a clean git repo, HEAD resolving to a full sha, and a commit message without
// "skip ci" — and returns the resolved HEAD sha. Each failure refuses with a reason;
// nothing about the ticket or queue is touched by this function.
func checkSendPreconditions(repo string) (string, error) {
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return "", fmt.Errorf("send-to-review: refuse — %s is not a git repo (no .git)", repo)
	}
	dirty, err := hasUncommittedTrackedChanges(repo)
	if err != nil {
		return "", fmt.Errorf("send-to-review: refuse — could not read git status in %s: %w", repo, err)
	}
	if dirty {
		return "", fmt.Errorf("send-to-review: refuse — uncommitted tracked changes in %s", repo)
	}
	sha, err := headSHA(repo)
	if err != nil || len(sha) != 40 {
		return "", fmt.Errorf("send-to-review: refuse — HEAD does not resolve to a full sha in %s", repo)
	}
	msg, err := headCommitMessage(repo)
	if err != nil {
		return "", fmt.Errorf("send-to-review: refuse — could not read HEAD commit message in %s: %w", repo, err)
	}
	if strings.Contains(strings.ToLower(msg), "skip ci") {
		return "", fmt.Errorf("send-to-review: refuse — HEAD commit message in %s contains 'skip ci'", repo)
	}
	return sha, nil
}

// hasUncommittedTrackedChanges is `git status --porcelain` with untracked ("??")
// lines excluded — send-to-review.sh only cares about TRACKED changes.
func hasUncommittedTrackedChanges(repo string) (bool, error) {
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "??") {
			continue
		}
		return true, nil
	}
	return false, nil
}

func headSHA(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func headCommitMessage(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
