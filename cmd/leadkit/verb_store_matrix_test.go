package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The store half of the verb matrix (ticket 105): the verbs that take a TICKET or a queue PATH
// rather than a repo. A repo has a profile that governs it (verb_state_matrix_test.go); a ticket
// belongs to a STORE, and the verb has to decide which one the argument names.
//
// Two stores, session (S) and other (O), each with its own queue/ and tasks/. The verb always runs
// from S, with its profile discovered from the cwd. The argument forms are the ways a caller names
// a ticket: through a lane link that resolves, through one that was routed away, by bare name, by a
// tasks/ path, through a symlinked spelling of the queue.
//
// The state that needed a third answer (ticket 86, round 3) is the routed-away lane path in O: it
// belongs to store O by its queue prefix and the ticket is absent from the lane. The verb must
// neither write into S's same-numbered ticket nor report success.

type storeWorld struct {
	root, session, other, alias string
}

const ticketBody = "---\ntitle: %s\nstatus: pending\n---\nbody\n"

func newStoreWorld(t *testing.T) storeWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := storeWorld{root: root, session: filepath.Join(root, "S"), other: filepath.Join(root, "O"), alias: filepath.Join(root, "alias-S")}
	mk := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{w.session, w.other} {
		for _, d := range []string{"tasks", "queue/developer/pending", "queue/developer/in-progress", "queue/code-review/pending"} {
			mk(filepath.Join(p, d))
		}
		if err := os.WriteFile(filepath.Join(p, "leadkit.yaml"), []byte("queue_root: ./queue\nstore_root: ./tasks\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// ticket creates tasks/<base> in store, and a lane link to it unless routedAway.
	ticket := func(store, base string, linked bool) {
		if err := os.WriteFile(filepath.Join(store, "tasks", base), []byte(fmt.Sprintf(ticketBody, base)), 0o644); err != nil {
			t.Fatal(err)
		}
		if linked {
			if err := os.Symlink(filepath.Join("..", "..", "..", "tasks", base), filepath.Join(store, "queue", "developer", "pending", base)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ticket(w.session, "5-a.md", true)
	ticket(w.session, "6-c.md", false) // routed away from every lane
	ticket(w.other, "7-b.md", true)
	ticket(w.other, "8-d.md", false) // routed away from every lane
	if err := os.Symlink(w.session, w.alias); err != nil {
		t.Fatal(err)
	}
	// A clean git repo for `review send`'s preconditions.
	repo := filepath.Join(root, "repo")
	mk(repo)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return w
}

// run execs the binary from dir with the profile discovered from it.
func (w storeWorld) run(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = sandboxedEnv(filepath.Join(w.root, "home"), []string{"LEADKIT_PROFILE=", "LEADKIT_QUEUE="})
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("exec %v: %v\n%s", args, err, out)
	return "", -1
}

// storeForm is one way of naming a ticket, and what each verb does with it.
type storeForm struct {
	name string
	arg  func(w storeWorld) string
	// want is keyed by verb: what it wrote or planned, or "refused:<cause>" with nothing written.
	want map[string]string
}

func in(store func(w storeWorld) string, rel ...string) func(w storeWorld) string {
	return func(w storeWorld) string { return filepath.Join(append([]string{store(w)}, rel...)...) }
}

func sessionDir(w storeWorld) string { return w.session }
func otherDir(w storeWorld) string   { return w.other }
func aliasDir(w storeWorld) string   { return w.alias }
func lit(s string) func(w storeWorld) string {
	return func(storeWorld) string { return s }
}

var storeForms = []storeForm{
	{"session lane path, absolute", in(sessionDir, "queue/developer/pending/5-a.md"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "plan:S/5-a.md"}},
	{"session lane path, relative to the cwd", lit("queue/developer/pending/5-a.md"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "refused:ticket-not-found"}},
	{"session lane path through a symlinked spelling of S", in(aliasDir, "queue/developer/pending/5-a.md"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "plan:S/5-a.md"}},
	{"session tasks/ path", in(sessionDir, "tasks/5-a.md"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "plan:S/5-a.md"}},
	{"bare name, in S", lit("5-a"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "plan:S/5-a.md"}},
	{"bare name with .md, in S", lit("5-a.md"), map[string]string{"task note": "wrote:S/5-a.md", "review send": "plan:S/5-a.md"}},
	{"bare name, in S but routed away from every lane", lit("6-c"), map[string]string{"task note": "wrote:S/6-c.md", "review send": "plan:S/6-c.md"}},
	{"session lane path that was routed away", in(sessionDir, "queue/developer/pending/6-c.md"), map[string]string{"task note": "wrote:S/6-c.md", "review send": "refused:ticket-not-found"}},
	{"other store's lane path, resolves", in(otherDir, "queue/developer/pending/7-b.md"), map[string]string{"task note": "wrote:O/7-b.md", "review send": "refused:not-under-tasks-dir"}},
	{"other store's lane path, routed away", in(otherDir, "queue/developer/pending/8-d.md"), map[string]string{"task note": "refused:outside-session-dirs", "review send": "refused:ticket-not-found"}},
	{"other store's tasks/ path", in(otherDir, "tasks/7-b.md"), map[string]string{"task note": "refused:outside-session-dirs", "review send": "refused:not-under-tasks-dir"}},
	{"bare name that exists only in O", lit("7-b"), map[string]string{"task note": "refused:no-such-ticket-in-tasks", "review send": "refused:ticket-not-found"}},
	{"bare name that exists nowhere", lit("99-nope"), map[string]string{"task note": "refused:no-such-ticket-in-tasks", "review send": "refused:ticket-not-found"}},
}

var storeMarkerSeq int

// refusalReason names the cause of a refusal, so a form pinned as refused is refused for the reason
// it is meant to be and not for some other one.
func refusalReason(out string) string {
	for _, r := range []struct{ class, text string }{
		{"outside-session-dirs", "outside this session's queue"},
		{"no-such-ticket-in-tasks", "no ticket"},
		{"ticket-not-found", "ticket not found"},
		{"not-under-tasks-dir", "is not under the tasks dir"},
	} {
		if strings.Contains(out, r.text) {
			return r.class
		}
	}
	return "other(" + stateFirstLine(out) + ")"
}

// observeStoreVerb runs one verb on one argument form and reports what it did to the stores.
func observeStoreVerb(t *testing.T, w storeWorld, bin, verb, arg string) string {
	t.Helper()
	storeMarkerSeq++
	marker := fmt.Sprintf("MARK-%d", storeMarkerSeq) // unique per call: the stores are shared across the forms
	switch verb {
	case "task note":
		out, code := w.run(t, bin, w.session, "task", "note", "--task", arg, "--by", "matrix", "--text", marker)
		for _, f := range []struct{ label, path string }{
			{"S/5-a.md", filepath.Join(w.session, "tasks", "5-a.md")}, {"S/6-c.md", filepath.Join(w.session, "tasks", "6-c.md")},
			{"O/7-b.md", filepath.Join(w.other, "tasks", "7-b.md")}, {"O/8-d.md", filepath.Join(w.other, "tasks", "8-d.md")},
		} {
			if b, _ := os.ReadFile(f.path); strings.Contains(string(b), marker) {
				if code != 0 {
					return fmt.Sprintf("ERROR: wrote %s but exited %d", f.label, code)
				}
				return "wrote:" + f.label
			}
		}
		if code == 0 {
			return "ERROR: exit 0 and no ticket has the note"
		}
		return "refused:" + refusalReason(out)
	case "review send":
		out, code := w.run(t, bin, w.session, "review", "send", "--task", arg, "--repo", filepath.Join(w.root, "repo"), "--dry-run")
		if code != 0 {
			return "refused:" + refusalReason(out)
		}
		m := strings.SplitN(out, "would symlink ", 2)
		if len(m) != 2 {
			return "ERROR: exit 0 with no plan: " + stateFirstLine(out)
		}
		link := strings.Fields(m[1])[0]
		store := "?"
		switch {
		case strings.HasPrefix(link, w.session+string(os.PathSeparator)):
			store = "S"
		case strings.HasPrefix(link, w.other+string(os.PathSeparator)):
			store = "O"
		}
		return "plan:" + store + "/" + filepath.Base(link)
	}
	t.Fatalf("unknown store verb %q", verb)
	return ""
}

func TestVerbStoreMatrix(t *testing.T) {
	bin := testBinary(t)
	for _, verb := range []string{"task note", "review send"} {
		w := newStoreWorld(t)
		for _, f := range storeForms {
			t.Run(verb+"/"+f.name, func(t *testing.T) {
				want, ok := f.want[verb]
				if !ok {
					t.Fatalf("form %q has no expectation for %s", f.name, verb)
				}
				if got := observeStoreVerb(t, w, bin, verb, f.arg(w)); got != want {
					t.Errorf("%s with %s (%s): got %q, want %q", verb, f.name, f.arg(w), got, want)
				}
			})
		}
	}
}

// The same verbs, varying the SESSION's own profile rather than the argument. A store verb resolves
// its queue and tasks directories from the profile found at the cwd, so the profile's state decides
// which store it touches, and an unreadable profile reads exactly like an absent one: both fall back
// to the built-in defaults, so the verb acts on ./tasks and says nothing.
//
// The fixture has a real store (real-tasks/, what a valid profile points at) and a decoy at the
// default location (tasks/), each holding a ticket 5-a.md. Which one the note lands in is the answer.
type sessionState struct {
	name, profile, want, gap, desired string
}

var sessionStates = []sessionState{
	{name: "governed", profile: "queue_root: ./queue\nstore_root: ./real-tasks\n", want: "wrote:real"},
	// No profile is the ordinary ungoverned case: the defaults are the right answer.
	{name: "ungoverned", profile: "", want: "wrote:decoy"},
	{name: "unreadable", profile: "queue_root: [unclosed\n", want: "wrote:decoy", gap: "105-A",
		desired: "refuse, naming the profile that would not load: the verb writes into whichever ticket the defaults find"},
}

func TestVerbStoreMatrix_SessionProfileStates(t *testing.T) {
	bin := testBinary(t)
	for _, st := range sessionStates {
		t.Run("task note/"+st.name, func(t *testing.T) {
			w := newStoreWorld(t)
			dir := filepath.Join(w.root, "S2")
			for _, d := range []string{"real-tasks", "tasks", "queue/developer/pending", "queue/code-review/pending"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, d := range []string{"real-tasks", "tasks"} {
				if err := os.WriteFile(filepath.Join(dir, d, "5-a.md"), []byte(fmt.Sprintf(ticketBody, d)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if st.profile != "" {
				if err := os.WriteFile(filepath.Join(dir, "leadkit.yaml"), []byte(st.profile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, code := w.run(t, bin, dir, "task", "note", "--task", "5-a", "--by", "matrix", "--text", "SESSION-MARK")
			got := "refused"
			for _, d := range []struct{ label, name string }{{"real", "real-tasks"}, {"decoy", "tasks"}} {
				if b, _ := os.ReadFile(filepath.Join(dir, d.name, "5-a.md")); strings.Contains(string(b), "SESSION-MARK") {
					got = "wrote:" + d.label
				}
			}
			if code != 0 && got != "refused" {
				got = fmt.Sprintf("ERROR: wrote but exited %d: %s", code, stateFirstLine(out))
			}
			if got == st.want {
				return
			}
			if st.gap != "" {
				t.Errorf("task note with a %s session profile now gives %q, but this row pins the known gap %s (%q, should be: %s).\n"+
					"If this is the fix, set want to the new answer and drop the gap.", st.name, got, st.gap, st.want, st.desired)
				return
			}
			t.Errorf("task note with a %s session profile: got %q, want %q", st.name, got, st.want)
		})
	}
}

// `queue move` and `queue assign` take their stores from the session, and an argument that names
// the other store must be refused rather than acted on in the wrong one. They are dry runs: the
// answer is whether the plan is made.
func TestVerbStoreMatrix_QueueVerbs(t *testing.T) {
	bin := testBinary(t)
	w := newStoreWorld(t)
	plan := func(args ...string) string {
		out, code := w.run(t, bin, w.session, args...)
		if code == 0 && strings.Contains(out, "DRY RUN") {
			return "plan"
		}
		if code == 0 {
			return "ERROR: exit 0 with no plan: " + stateFirstLine(out)
		}
		return "refused: " + stateFirstLine(out)
	}
	sess := func(rel string) string { return filepath.Join(w.session, rel) }
	oth := func(rel string) string { return filepath.Join(w.other, rel) }
	ali := func(rel string) string { return filepath.Join(w.alias, rel) }
	const pending, inprog = "queue/developer/pending/5-a.md", "queue/developer/in-progress/"
	for _, c := range []struct {
		name string
		got  func() string
		want string
	}{
		{"move: session lane path to a session state dir", func() string { return plan("queue", "move", "--dry-run", sess(pending), sess(inprog)) }, "plan"},
		{"move: relative to the cwd", func() string { return plan("queue", "move", "--dry-run", pending, inprog) }, "plan"},
		{"move: through a symlinked spelling of S", func() string { return plan("queue", "move", "--dry-run", ali(pending), ali(inprog)) }, "plan"},
		{"move: the other store's lane path", func() string {
			return plan("queue", "move", "--dry-run", oth("queue/developer/pending/7-b.md"), oth(inprog))
		}, "source outside queue root"},
		{"move: a session ticket into the other store's queue", func() string { return plan("queue", "move", "--dry-run", sess(pending), oth(inprog)) }, "dest outside queue root"},
		{"move: the other store's ticket into the session's queue", func() string {
			return plan("queue", "move", "--dry-run", oth("queue/developer/pending/7-b.md"), sess(inprog))
		}, "source outside queue root"},
		{"assign: a ticket number in the session's store", func() string { return plan("queue", "assign", "--dry-run", "6", "code-review") }, "plan"},
		{"assign: a ticket number only the other store has", func() string { return plan("queue", "assign", "--dry-run", "8", "code-review") }, "found 0"},
		{"assign: a ticket number nobody has", func() string { return plan("queue", "assign", "--dry-run", "99", "code-review") }, "found 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.got()
			// A refusal is pinned by its REASON, so a move that is refused for some other cause (a bad
			// state directory, an unknown role) cannot stand in for the cross-store refusal.
			if got != c.want && !(c.want != "plan" && strings.HasPrefix(got, "refused:") && strings.Contains(got, c.want)) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
