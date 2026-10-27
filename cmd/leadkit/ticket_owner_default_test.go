package main

// Ticket 173, end to end through the real binary: a ticket created the normal way (no
// owner) must be reconcilable by a read-only reviewer. Nothing sets `owner:` after
// creation, so `gate verdict changes-needed` defaults it from the ticket's `type:`;
// `queue assign --owner` records an explicit one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ownerRig struct {
	t     *testing.T
	bin   string
	env   []string
	root  string
	repo  string
	sha   string
	tasks string
	queue string
}

func newOwnerRig(t *testing.T) *ownerRig {
	t.Helper()
	r := &ownerRig{t: t, bin: testBinary(t), root: t.TempDir()}
	profilePath := filepath.Join(r.root, "leadkit.yaml")
	profile := "queue_root: queue\nstore_root: tasks\nenforced_remotes: [based]\nroster:\n  - type: developer\n    count: 1\n"
	if err := os.WriteFile(profilePath, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	r.tasks, r.queue = filepath.Join(r.root, "tasks"), filepath.Join(r.root, "queue")
	for _, d := range []string{r.tasks, "code-review", "critique", "developer"} {
		if !filepath.IsAbs(d) {
			d = filepath.Join(r.queue, d, "pending")
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.repo, r.sha = gitRepoWithOneCommit(t, "git@example.com:org/based.git")
	r.env = []string{"LEADKIT_PROFILE=" + profilePath, "LEADKIT_QUEUE=", "HOME=" + t.TempDir()}
	return r
}

func (r *ownerRig) lk(args ...string) (string, int) {
	r.t.Helper()
	return runCLI(r.t, r.bin, r.env, args...)
}

// newTicket creates a ticket the normal way: `task create` with NO --owner. Returns its
// id and its store path.
func (r *ownerRig) newTicket(typ string) (id, path string) {
	r.t.Helper()
	out, code := r.lk("task", "create", "--title", "t", "--type", typ, "--unparented")
	if code != 0 {
		r.t.Fatalf("task create: exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	id = lines[len(lines)-1]
	matches, _ := filepath.Glob(filepath.Join(r.tasks, id+"-*.md"))
	if len(matches) != 1 {
		r.t.Fatalf("want one store file for ticket %s, got %v", id, matches)
	}
	return id, matches[0]
}

func (r *ownerRig) sendToReview(path string) {
	r.t.Helper()
	if out, code := r.lk("review", "send", "--task", path, "--repo", r.repo); code != 0 {
		r.t.Fatalf("review send: exit %d\n%s", code, out)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCLI_ChangesNeededOnATicketCreatedWithoutAnOwnerReachesTheOwningRole(t *testing.T) {
	r := newOwnerRig(t)
	_, path := r.newTicket("developer")
	if fm := readFile(t, path); strings.Contains(fm, "\nowner:") {
		t.Fatalf("precondition: the ticket must have no owner:\n%s", fm)
	}
	r.sendToReview(path)
	base := filepath.Base(path)
	link := filepath.Join(r.queue, "code-review", "pending", base)

	// The queue/... form a wake message hands a reviewer, WITHOUT the .md extension.
	ref := filepath.Join("queue", "code-review", "pending", strings.TrimSuffix(base, ".md"))
	out, code := r.lk("gate", "verdict", "--ticket", ref, "--verdict", "changes-needed", "--by", "cr-1")
	if code != 0 {
		t.Fatalf("a ticket with no owner: must be reconcilable (defaulting from type:): exit %d\n%s", code, out)
	}
	dest := filepath.Join(r.queue, "developer", "pending", base)
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("rework must land in the owning role's pending lane %s: %v\n%s", dest, err, out)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Errorf("the ticket must leave the reviewer's lane %s", link)
	}
	if _, err := os.Stat(filepath.Join(r.queue, "owner")); err == nil {
		t.Error("no stray owner lane may be minted")
	}
	fm := readFile(t, path)
	if !strings.Contains(fm, "stage: needs_rework") || !strings.Contains(fm, "status: pending") {
		t.Errorf("the ticket must read as rework:\n%s", fm)
	}
}

func TestCLI_ChangesNeededRefusesATypeThatIsNotAKnownRole_NothingRecorded(t *testing.T) {
	r := newOwnerRig(t)
	_, path := r.newTicket("qa-agent") // qa is not in this roster
	r.sendToReview(path)
	link := filepath.Join(r.queue, "code-review", "pending", filepath.Base(path))

	out, code := r.lk("gate", "verdict", "--ticket", link, "--verdict", "changes-needed", "--by", "cr-1")
	if code == 0 || !strings.Contains(out, "type") || !strings.Contains(out, "does not reconcile") {
		t.Fatalf("want a refusal naming the type that was used as owner: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("a refused verdict must leave the ticket where it was: %v", err)
	}
	recs, _ := filepath.Glob(filepath.Join(r.env[2][len("HOME="):], ".claude", "review-gate", "records", "*"))
	if len(recs) != 0 {
		t.Errorf("a refusal must precede any gate record, found %v", recs)
	}
}

// A ticket whose TYPE names a reviewer role (work ABOUT the review system, like #117) must
// not loop back into the lane that bounced it: refused, naming the type and the way out,
// with the ticket left in place and nothing recorded.
func TestCLI_ChangesNeededRefusesATypeThatNamesAGateRole_NoSelfBounce(t *testing.T) {
	for _, typ := range []string{"code-review-agent", "critique-agent"} {
		t.Run(typ, func(t *testing.T) {
			r := newOwnerRig(t)
			_, path := r.newTicket(typ)
			r.sendToReview(path)
			link := filepath.Join(r.queue, "code-review", "pending", filepath.Base(path))

			out, code := r.lk("gate", "verdict", "--ticket", link, "--verdict", "changes-needed", "--by", "cr-1")
			if code == 0 || !strings.Contains(out, typ) || !strings.Contains(out, "explicit owner") || !strings.Contains(out, "--owner developer") {
				t.Fatalf("want a refusal naming type %q and pointing at an explicit owner: exit %d\n%s", typ, code, out)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("a refused verdict must leave the ticket where it was: %v", err)
			}
			recs, _ := filepath.Glob(filepath.Join(r.env[2][len("HOME="):], ".claude", "review-gate", "records", "*"))
			if len(recs) != 0 {
				t.Errorf("a refusal must precede any gate record, found %v", recs)
			}

			// And the way out the message names works: an explicit owner: in the frontmatter.
			fm := readFile(t, path)
			if err := os.WriteFile(path, []byte(strings.Replace(fm, "\n---\n", "\nowner: developer\n---\n", 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			out, code = r.lk("gate", "verdict", "--ticket", link, "--verdict", "changes-needed", "--by", "cr-1")
			if code != 0 {
				t.Fatalf("with an explicit owner the verdict must run: exit %d\n%s", code, out)
			}
			if _, err := os.Lstat(filepath.Join(r.queue, "developer", "pending", filepath.Base(path))); err != nil {
				t.Errorf("rework must land in developer/pending: %v\n%s", err, out)
			}
		})
	}
}

func TestCLI_QueueAssignOwner_RecordsTheCanonicalRole(t *testing.T) {
	r := newOwnerRig(t)
	id, path := r.newTicket("developer")

	// an owner that is no known role is refused with nothing done
	out, code := r.lk("queue", "assign", "--owner", "nobody-1234", id, "developer")
	if code != 2 || !strings.Contains(out, "does not reconcile") {
		t.Fatalf("an unknown --owner must be refused (exit 2): exit %d\n%s", code, out)
	}
	if entries, _ := filepath.Glob(filepath.Join(r.queue, "*", "pending", "*")); len(entries) != 0 {
		t.Errorf("a refused assign must queue nothing, found %v", entries)
	}

	// dry-run reports the owner and writes nothing
	out, code = r.lk("queue", "assign", "--dry-run", "--owner", "developer-agent", id, "developer")
	if code != 0 || !strings.Contains(out, "would set owner: developer") {
		t.Errorf("dry-run must name the owner it would set: exit %d\n%s", code, out)
	}
	if strings.Contains(readFile(t, path), "\nowner:") {
		t.Error("dry-run must not write the owner")
	}

	// the real run writes the CANONICAL role, not the spelling given
	out, code = r.lk("queue", "assign", "--owner", "owner-developer", id, "developer")
	if code != 0 {
		t.Fatalf("queue assign --owner: exit %d\n%s", code, out)
	}
	if fm := readFile(t, path); !strings.Contains(fm, "\nowner: developer\n") {
		t.Errorf("owner: developer must be written to the ticket:\n%s", fm)
	}
	if _, err := os.Stat(filepath.Join(r.queue, "developer", "pending", filepath.Base(path))); err != nil {
		t.Errorf("the ticket must still be queued: %v", err)
	}
}
