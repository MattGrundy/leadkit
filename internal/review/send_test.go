package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sampleTicket = "---\ntitle: test ticket\nstatus: backlog\nowner: someone\npriority: P2\n---\n\n# body\n"

// initGitRepo creates a throwaway git repo (never the real project clones) with one
// commit carrying commitMsg, and returns its path.
func initGitRepo(t *testing.T, commitMsg string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", commitMsg)
	return repo
}

// setupReviewFixture builds a throwaway queue/tasks tree (never the real one) with
// the code-review ROLE lane already live at queue/code-review/ — role-keyed lanes:
// there is no queue/ROLES file here at all, proving SendToReview needs none to
// route into the role's own lane.
func setupReviewFixture(t *testing.T) *Reviewer {
	t.Helper()
	root := t.TempDir()
	queueRoot := filepath.Join(root, "queue")
	tasksDir := filepath.Join(root, "tasks")
	for _, d := range []string{tasksDir, filepath.Join(queueRoot, "code-review", "pending"), filepath.Join(queueRoot, "code-review", "in-progress")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Reviewer{QueueRoot: queueRoot, TasksDir: tasksDir}
}

func writeTicket(t *testing.T, rv *Reviewer, base, content string) string {
	t.Helper()
	path := filepath.Join(rv.TasksDir, base)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSendToReview_DryRunMutatesNothing(t *testing.T) {
	rv := setupReviewFixture(t)
	ticketPath := writeTicket(t, rv, "42-example.md", sampleTicket)
	repo := initGitRepo(t, "a normal commit")

	before, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}

	res, err := rv.SendToReview("42-example.md", repo, "new-owner", true)
	if err != nil {
		t.Fatalf("SendToReview (dry-run): %v", err)
	}
	if !res.DryRun || res.Reviewer != "code-review" || len(res.SHA) != 40 {
		t.Errorf("res = %+v, want the plan filled in (DryRun=true Reviewer=code-review a full sha)", res)
	}

	after, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("dry-run must not modify the ticket frontmatter:\nbefore: %s\nafter:  %s", before, after)
	}
	link := filepath.Join(rv.QueueRoot, "code-review", "pending", "42-example.md")
	if _, err := os.Lstat(link); err == nil {
		t.Error("dry-run must not create the queue symlink")
	}
}

func TestSendToReview_HappyPath(t *testing.T) {
	rv := setupReviewFixture(t)
	ticketPath := writeTicket(t, rv, "42-example.md", sampleTicket)
	repo := initGitRepo(t, "a normal commit")

	res, err := rv.SendToReview("42-example.md", repo, "new-owner", false)
	if err != nil {
		t.Fatalf("SendToReview: %v", err)
	}
	if res.Reviewer != "code-review" {
		t.Errorf("Reviewer = %q, want code-review", res.Reviewer)
	}
	if len(res.SHA) != 40 {
		t.Errorf("SHA = %q, want a full 40-char sha", res.SHA)
	}

	fm, err := ReadFrontmatter(ticketPath)
	if err != nil {
		t.Fatalf("ReadFrontmatter: %v", err)
	}
	for k, want := range map[string]string{
		"stage": "code-review", "status": "in_review", "gate": "true",
		"owner": "new-owner", "repo": repo, "sha": res.SHA,
		"title": "test ticket", // an untouched key must be preserved
	} {
		if fm[k] != want {
			t.Errorf("frontmatter[%q] = %q, want %q (full: %+v)", k, fm[k], want, fm)
		}
	}

	link := filepath.Join(rv.QueueRoot, "code-review", "pending", "42-example.md")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("queue entry is not a symlink")
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture's TasksDir sits at QueueRoot's sibling "tasks" (the
	// legacy-migration-shaped layout) — filepath.Rel-computed relative target
	// happens to equal the old hardcoded "../../../tasks" depth for exactly that
	// shape; see TestSendToReview_NonSiblingLayoutSymlinkStillResolves for a
	// layout where the two diverge.
	wantTarget := filepath.Join("..", "..", "..", "tasks", "42-example.md")
	if target != wantTarget {
		t.Errorf("symlink target = %q, want %q", target, wantTarget)
	}
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve queue link: %v", err)
	}
	wantReal, _ := filepath.EvalSymlinks(ticketPath)
	if real != wantReal {
		t.Errorf("resolved queue link = %s, want %s", real, wantReal)
	}
}

// TestSendToReview_NonSiblingLayoutSymlinkStillResolves is the profile-store-
// resolution regression: QueueRoot and TasksDir need NOT be siblings under one
// parent (an adopter's own queue_root/store_root may sit anywhere) — the
// created symlink must still resolve to the real ticket regardless of the
// relative depth between the two, proving the link target is computed for
// real (filepath.Rel) rather than assuming the legacy-migration shape.
func TestSendToReview_NonSiblingLayoutSymlinkStillResolves(t *testing.T) {
	root := t.TempDir()
	// Deliberately NOT siblings: tasksDir is two levels deeper than queueRoot's
	// parent, under a completely different subtree name.
	queueRoot := filepath.Join(root, "a", "b", "queue")
	tasksDir := filepath.Join(root, "somewhere", "else", "entirely", "tasks")
	for _, d := range []string{tasksDir, filepath.Join(queueRoot, "code-review", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A decoy ROLES file mapping code-review to a totally different live instance —
	// role-keyed lanes: SendToReview must ignore it entirely and route into the
	// role's own lane regardless.
	if err := os.WriteFile(filepath.Join(queueRoot, "ROLES"), []byte("code-review\tcr-9-decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rv := &Reviewer{QueueRoot: queueRoot, TasksDir: tasksDir}
	ticketPath := writeTicket(t, rv, "9-example.md", sampleTicket)
	repo := initGitRepo(t, "a normal commit")

	if _, err := rv.SendToReview("9-example.md", repo, "", false); err != nil {
		t.Fatalf("SendToReview: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(queueRoot, "cr-9-decoy")); err == nil {
		t.Error("must not have touched the decoy ROLES-mapped instance dir at all")
	}

	link := filepath.Join(queueRoot, "code-review", "pending", "9-example.md")
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve queue link: %v", err)
	}
	wantReal, _ := filepath.EvalSymlinks(ticketPath)
	if real != wantReal {
		t.Errorf("resolved queue link = %s, want %s — a hardcoded-depth symlink would resolve to the wrong (nonexistent) path here", real, wantReal)
	}
}

func TestSendToReview_RefusesDirtyRepo(t *testing.T) {
	rv := setupReviewFixture(t)
	writeTicket(t, rv, "1-x.md", sampleTicket)
	repo := initGitRepo(t, "clean commit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := rv.SendToReview("1-x.md", repo, "", false); err == nil {
		t.Fatal("want refusal for uncommitted tracked changes")
	}
}

func TestSendToReview_RefusesSkipCiCommitMessage(t *testing.T) {
	rv := setupReviewFixture(t)
	writeTicket(t, rv, "1-x.md", sampleTicket)
	repo := initGitRepo(t, "chore: resync [skip ci]")

	if _, err := rv.SendToReview("1-x.md", repo, "", false); err == nil {
		t.Fatal("want refusal for a HEAD commit message containing skip ci")
	}
}

func TestSendToReview_RefusesMissingReviewerPendingDir(t *testing.T) {
	rv := setupReviewFixture(t)
	if err := os.RemoveAll(filepath.Join(rv.QueueRoot, "code-review", "pending")); err != nil {
		t.Fatal(err)
	}
	writeTicket(t, rv, "1-x.md", sampleTicket)
	repo := initGitRepo(t, "clean commit")

	if _, err := rv.SendToReview("1-x.md", repo, "", false); err == nil {
		t.Fatal("want refusal when the reviewer's pending dir doesn't exist")
	}
}

func TestSendToReview_RefusesUnresolvableTicket(t *testing.T) {
	rv := setupReviewFixture(t)
	repo := initGitRepo(t, "clean commit")
	if _, err := rv.SendToReview("does-not-exist.md", repo, "", false); err == nil {
		t.Fatal("want refusal for a ticket that doesn't resolve to a real file")
	}
}

func TestSendToReview_RemovesStaleEntriesExceptReviewerPending(t *testing.T) {
	rv := setupReviewFixture(t)
	base := "1-x.md"
	ticketPath := writeTicket(t, rv, base, sampleTicket)

	// A stale entry under a DIFFERENT agent's pending must be removed.
	if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "some-other-agent", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	staleLink := filepath.Join(rv.QueueRoot, "some-other-agent", "pending", base)
	if err := os.Symlink(ticketPath, staleLink); err != nil {
		t.Fatal(err)
	}

	// A stale entry under the REVIEWER's own in-progress must also be removed —
	// only its pending is excepted.
	inProgLink := filepath.Join(rv.QueueRoot, "code-review", "in-progress", base)
	if err := os.Symlink(ticketPath, inProgLink); err != nil {
		t.Fatal(err)
	}

	repo := initGitRepo(t, "clean commit")
	if _, err := rv.SendToReview(base, repo, "", false); err != nil {
		t.Fatalf("SendToReview: %v", err)
	}

	if _, err := os.Lstat(staleLink); err == nil {
		t.Error("stale entry under another agent's pending should have been removed")
	}
	if _, err := os.Lstat(inProgLink); err == nil {
		t.Error("stale entry under the reviewer's own in-progress should have been removed")
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "code-review", "pending", base)); err != nil {
		t.Error("the reviewer's own pending entry (the new link) should exist")
	}
}

func TestSendToReview_ResolvesAnAbsoluteQueueSymlinkPath(t *testing.T) {
	rv := setupReviewFixture(t)
	base := "1-x.md"
	ticketPath := writeTicket(t, rv, base, sampleTicket)
	if err := os.MkdirAll(filepath.Join(rv.QueueRoot, "dev-1", "pending"), 0o755); err != nil {
		t.Fatal(err)
	}
	queueSymlink := filepath.Join(rv.QueueRoot, "dev-1", "pending", base)
	rel, err := filepath.Rel(filepath.Dir(queueSymlink), ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, queueSymlink); err != nil {
		t.Fatal(err)
	}

	repo := initGitRepo(t, "clean commit")
	res, err := rv.SendToReview(queueSymlink, repo, "", false)
	if err != nil {
		t.Fatalf("SendToReview: %v", err)
	}
	if res.Base != base {
		t.Errorf("Base = %q, want %q", res.Base, base)
	}
}

// The queue and the store reached through two spellings of one place (macOS /var and /private/var):
// the link SendToReview creates must resolve, or the ticket is stranded behind a dangling link and
// gate verdict then reports "ticket not found".
func TestSendToReview_LinkResolvesWhenQueueAndStoreUseDifferentSpellings(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(base, "a", "b", "real")
	alias := filepath.Join(base, "alias")
	for _, d := range []string{filepath.Join(realRoot, "tasks"), filepath.Join(realRoot, "queue", "code-review", "pending")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	rv := &Reviewer{QueueRoot: filepath.Join(alias, "queue"), TasksDir: filepath.Join(realRoot, "tasks")}
	writeTicket(t, rv, "9-x.md", sampleTicket)

	res, err := rv.SendToReview("9-x.md", initGitRepo(t, "a normal commit"), "", false)
	if err != nil {
		t.Fatalf("SendToReview: %v", err)
	}
	if _, err := os.Stat(res.QueueLink); err != nil {
		t.Fatalf("the link SendToReview made dangles: %v", err)
	}
}

// The two safety nets around the link cannot be reached with a correct helper, so each is driven by
// forcing a dangling target through the package seam.
func TestSendToReview_RefusesBeforeChangingAnythingWhenTheLinkWouldDangle(t *testing.T) {
	rv := setupReviewFixture(t)
	ticket := writeTicket(t, rv, "9-x.md", sampleTicket)
	before, _ := os.ReadFile(ticket)
	stale := filepath.Join(rv.QueueRoot, "code-review", "in-progress", "9-x.md")
	if err := os.Symlink(ticket, stale); err != nil {
		t.Fatal(err)
	}
	orig := linkTargetFor
	defer func() { linkTargetFor = orig }()
	linkTargetFor = func(_, _ string) string { return "../../../nowhere/9-x.md" }

	_, err := rv.SendToReview("9-x.md", initGitRepo(t, "a normal commit"), "", false)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a link that would dangle must be refused up front: %v", err)
	}
	if after, _ := os.ReadFile(ticket); string(after) != string(before) {
		t.Error("the refusal still rewrote the ticket's frontmatter")
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Error("the refusal still removed the existing queue entry")
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "code-review", "pending", "9-x.md")); !os.IsNotExist(err) {
		t.Error("the refusal still created a link")
	}
}

func TestSendToReview_RemovesALinkThatDoesNotResolveAfterItIsCreated(t *testing.T) {
	rv := setupReviewFixture(t)
	writeTicket(t, rv, "9-x.md", sampleTicket)
	origT, origR := linkTargetFor, linkResolves
	defer func() { linkTargetFor, linkResolves = origT, origR }()
	linkTargetFor = func(_, _ string) string { return "../../../nowhere/9-x.md" }
	linkResolves = func(_, _ string) bool { return true } // the pre-check is fooled, so the post-check must catch it

	_, err := rv.SendToReview("9-x.md", initGitRepo(t, "a normal commit"), "", false)
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("a link that dangles after creation must be reported: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rv.QueueRoot, "code-review", "pending", "9-x.md")); !os.IsNotExist(err) {
		t.Error("the dangling link was left behind")
	}
}

// A dry run writes nothing, so it can run the check and must not promise a link the real run refuses.
func TestSendToReview_DryRunRefusesWhatTheRealRunWouldRefuse(t *testing.T) {
	rv := setupReviewFixture(t)
	writeTicket(t, rv, "9-x.md", sampleTicket)
	orig := linkTargetFor
	defer func() { linkTargetFor = orig }()
	linkTargetFor = func(_, _ string) string { return "../../../nowhere/9-x.md" }
	if _, err := rv.SendToReview("9-x.md", initGitRepo(t, "a normal commit"), "", true); err == nil {
		t.Error("a dry run must refuse a link that would dangle instead of printing 'would symlink'")
	}
}
