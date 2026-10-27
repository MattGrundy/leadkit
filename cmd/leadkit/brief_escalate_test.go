package main

// Ticket 280: `brief escalate` / `brief route`, the UP/DOWN half of design §2.1. Four
// sessions (a domain lead, the overarching lead, a third project, a bystander) act through
// the real binary against one shared routing store, each with its own un-spoofable slug.
// The assertions that matter are the negative ones: briefs that were NOT escalated are
// byte-identical afterwards, and no task queue is written outside the acting project -
// a view or a verb that touched everything would pass a weak test as easily as a correct one.

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/routing"
	"mgpp.dev/leadkit/internal/usage"
)

// treeSnapshot is every file under root mapped to its content hash. A project's root holds
// its profile, tasks, queue, issues and team dir, so equality of two snapshots means nothing
// in that project was written, created or removed.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			snap[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		snap[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// idAfter extracts the first field after a line prefix, e.g. `brief escalated: <id> -> ...`.
func idAfter(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.Fields(strings.TrimPrefix(line, prefix))[0]
		}
	}
	t.Fatalf("no %q line in output:\n%s", prefix, out)
	return ""
}

// briefFile is the on-disk envelope bytes of a brief, for byte-identity assertions.
func briefFile(t *testing.T, routingDir string, env routing.Envelope) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(routingDir, "brief", env.ToSlug, env.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type escalationRig struct {
	bin                         string
	domain, hub, third, bystand string // profile paths
	routingDir                  string
	rd, pd                      []string // routing-dir / projects+presence-dir flags
	store                       *routing.Store
}

func newEscalationRig(t *testing.T) *escalationRig {
	t.Helper()
	r := &escalationRig{
		bin:    testBinary(t),
		domain: briefProject(t, "domain"), hub: briefProject(t, "hub"),
		third: briefProject(t, "third"), bystand: briefProject(t, "bystander"),
		routingDir: t.TempDir(),
	}
	projectsDir := t.TempDir()
	// Each project is registered with its REAL queue/store/issues dirs under its snapshotted
	// root. A write into another project resolves that project through this registry, so
	// registering placeholder paths would send such a write somewhere no snapshot looks.
	for slug, prof := range map[string]string{"domain": r.domain, "hub": r.hub, "third": r.third, "bystander": r.bystand} {
		root := filepath.Dir(prof)
		if err := usage.WriteProjectEntry(projectsDir, slug, slug,
			filepath.Join(root, "queue"), filepath.Join(root, "tasks"), filepath.Join(root, "issues"), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	r.rd = []string{"--routing-dir", r.routingDir}
	r.pd = []string{"--projects-dir", projectsDir, "--presence-dir", t.TempDir()}
	r.store = routing.Open(r.routingDir)
	return r
}

func (r *escalationRig) run(t *testing.T, prof string, args ...string) (string, int) {
	t.Helper()
	args = append(args, r.rd...)
	// Only the verbs that validate a recipient take the registry dirs.
	if args[1] == "escalate" || args[1] == "route" || args[1] == "send" {
		args = append(args, r.pd...)
	}
	return runBriefAs(t, r.bin, prof, args...)
}

func (r *escalationRig) must(t *testing.T, prof string, args ...string) string {
	t.Helper()
	out, code := r.run(t, prof, args...)
	if code != 0 {
		t.Fatalf("%v must succeed, got %d:\n%s", args, code, out)
	}
	return out
}

func (r *escalationRig) env(t *testing.T, id string) routing.Envelope {
	t.Helper()
	e, err := r.store.Locate("brief", id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// send files a plain brief from -> to through the rig's registry, as `brief send`.
func (r *escalationRig) send(t *testing.T, from, to, subject string) string {
	t.Helper()
	out := r.must(t, from, "brief", "send", "--to", to, "--subject", subject, "--body-file", bodyFile(t, "b"))
	return sendBriefID(t, out)
}

func (r *escalationRig) escalate(t *testing.T, subject string) string {
	t.Helper()
	out := r.must(t, r.domain, "brief", "escalate", "--to", "hub", "--target", "third",
		"--subject", subject, "--body-file", bodyFile(t, "we need the third project to cut a release"))
	return idAfter(t, out, "brief escalated: ")
}

// TestCLI_BriefEscalateRoute_EndToEnd: domain escalates, hub routes down to third, third
// does the work, hub confirms, and domain learns the outcome on the brief it sent. Alongside
// it sit briefs that must NOT be affected, and every project root, which must not be written.
func TestCLI_BriefEscalateRoute_EndToEnd(t *testing.T) {
	r := newEscalationRig(t)

	// Snapshot every project root BEFORE anything is filed, so escalate's own writes are inside
	// the window too.
	roots := map[string]string{"domain": r.domain, "hub": r.hub, "third": r.third, "bystander": r.bystand}
	before := map[string]map[string]string{}
	for n, p := range roots {
		before[n] = treeSnapshot(t, filepath.Dir(p))
	}

	// Bystanders in the same store: a plain brief awaiting its recipient, a plain brief that
	// will run its whole normal lifecycle, and an escalated brief nobody routes.
	idPlainSent := r.send(t, r.domain, "hub", "plain, untouched")
	idPlainFull := r.send(t, r.hub, "third", "plain, full lifecycle")
	idUnrouted := r.escalate(t, "escalated but never routed")
	idB := r.escalate(t, "need a release cut in third")

	untouched := map[string][]byte{}
	for name, id := range map[string]string{"plain-sent": idPlainSent, "unrouted-escalation": idUnrouted} {
		untouched[name] = briefFile(t, r.routingDir, r.env(t, id))
	}

	// Escalated: addressed UP to the hub, carrying the requester's context and target.
	b := r.env(t, idB)
	if b.Status != "escalated" || b.ToSlug != "hub" || b.FromSlug != "domain" {
		t.Fatalf("escalate must file an %q brief domain->hub, got %+v", "escalated", b)
	}
	body, _ := r.store.ReadBody(b)
	for _, want := range []string{"Suggested target project: third", "cut a release"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the escalated body must carry %q for the router, got:\n%s", want, body)
		}
	}

	// An escalated brief is routed, not worked: the router cannot ack it.
	if out, code := r.run(t, r.hub, "brief", "ack", idB); code == 0 || !strings.Contains(out, "ROUTED") {
		t.Fatalf("acking an escalated brief must be refused with the route hint (code=%d):\n%s", code, out)
	}
	// Only the recipient of the escalation may route it - not the requester, not the
	// target, not a bystander; none of them acquire authority by claiming to be on top.
	for name, prof := range map[string]string{"requester": r.domain, "target": r.third, "bystander": r.bystand} {
		if out, code := r.run(t, prof, "brief", "route", idB, "--to", "third"); code == 0 || !strings.Contains(out, "REFUSED") {
			t.Errorf("%s must be refused from routing (code=%d):\n%s", name, code, out)
		}
	}
	// The router cannot route it back to the requester, to itself, or to nowhere.
	for _, to := range []string{"domain", "hub"} {
		if _, code := r.run(t, r.hub, "brief", "route", idB, "--to", to); code == 0 {
			t.Errorf("routing to %q must be refused", to)
		}
	}
	if _, code := r.run(t, r.hub, "brief", "route", idB, "--to", "no-such-project"); code == 0 {
		t.Error("routing to an unknown project must be refused")
	}
	if r.env(t, idB).Status != "escalated" {
		t.Fatalf("refused routes must leave the brief escalated, got %q", r.env(t, idB).Status)
	}

	// Route DOWN: a NEW child brief hub->third; the original is parked at the hub.
	r.must(t, r.hub, "brief", "route", idB, "--to", "third", "--note", "third owns releases")
	var childIDs []string
	all, _ := r.store.List(routing.InboxFilter{Kind: "brief"})
	for _, e := range all {
		if e.Parent == idB {
			childIDs = append(childIDs, e.ID)
		}
	}
	if len(childIDs) != 1 {
		t.Fatalf("routing must file exactly one child of %s, got %v", idB, childIDs)
	}
	c := r.env(t, childIDs[0])
	if c.ToSlug != "third" || c.FromSlug != "hub" || c.Status != "sent" || c.ID == idB {
		t.Fatalf("the child must be a NEW sent brief hub->third, got %+v", c)
	}
	if got := r.env(t, idB); got.Status != "routed" || got.ToSlug != "hub" || got.Terminal() {
		t.Fatalf("the original must stay at the hub, routed and non-terminal, got %+v", got)
	}
	cb, _ := r.store.ReadBody(c)
	for _, want := range []string{"requested by domain", "cut a release", idB} {
		if !strings.Contains(string(cb), want) {
			t.Errorf("the child body must show its true origin (%q), got:\n%s", want, cb)
		}
	}
	// A routed brief cannot be routed a second time.
	if _, code := r.run(t, r.hub, "brief", "route", idB, "--to", "bystander"); code == 0 {
		t.Error("a routed brief must not be routable again")
	}
	// Both ends can read the other hop.
	if show := r.must(t, r.domain, "brief", "show", idB); !strings.Contains(show, "routed to third as "+c.ID) {
		t.Errorf("the requester's show must name the child hop:\n%s", show)
	}
	if show := r.must(t, r.third, "brief", "show", c.ID); !strings.Contains(show, "requested by domain") {
		t.Errorf("the target's show must name the requester:\n%s", show)
	}

	// Escalate and route have run: the target, the likeliest victim of a cross-project write,
	// must be exactly as it started. Only then does it work the brief; its --as-task is the one
	// legitimate queue write, in ITS OWN queue.
	if !sameSnapshot(before["third"], treeSnapshot(t, filepath.Dir(r.third))) {
		t.Error("escalate/route wrote into the target's project before the target did anything")
	}
	r.must(t, r.third, "brief", "ack", c.ID, "--as-task", "--task-type", "devops-agent")
	afterAck := treeSnapshot(t, filepath.Dir(r.third))
	if sameSnapshot(before["third"], afterAck) {
		t.Error("control: the target's own ack --as-task should have written its own queue")
	}
	// Until the router relays, the requester has no outcome to confirm.
	if _, code := r.run(t, r.domain, "brief", "confirm", idB); code == 0 {
		t.Fatal("the requester must not be able to confirm before an outcome is relayed")
	}
	r.must(t, r.third, "brief", "outcome", c.ID, "--body-file", bodyFile(t, "release v9 cut"))
	if got := r.env(t, idB).Status; got != "routed" {
		t.Fatalf("the target's outcome alone must not reach the requester; the router verifies first, got %q", got)
	}
	// The router (the child's sender) verifies; that is what relays the outcome UP.
	if cout := r.must(t, r.hub, "brief", "confirm", c.ID); !strings.Contains(cout, "outcome relayed") {
		t.Fatalf("confirming a routed child must relay its outcome:\n%s", cout)
	}
	if got := r.env(t, idB); got.Status != "outcome" {
		t.Fatalf("the original must now carry the outcome, got %q", got.Status)
	}
	// The ORIGINATING lead learns the outcome, and closes the loop it opened.
	show := r.must(t, r.domain, "brief", "show", idB)
	for _, want := range []string{"release v9 cut", "executed by third", "outcome-relayed"} {
		if !strings.Contains(show, want) {
			t.Errorf("the requester must see %q on the brief it sent:\n%s", want, show)
		}
	}
	r.must(t, r.domain, "brief", "confirm", idB)
	if got := r.env(t, idB).Status; got != "confirmed" {
		t.Fatalf("the requester confirms its own brief, got %q", got)
	}

	// Briefs NOT escalated are unaffected, down to the byte; so is a plain lifecycle.
	for name, id := range map[string]string{"plain-sent": idPlainSent, "unrouted-escalation": idUnrouted} {
		if !bytes.Equal(untouched[name], briefFile(t, r.routingDir, r.env(t, id))) {
			t.Errorf("%s brief was modified by an unrelated escalation/route", name)
		}
	}
	r.must(t, r.third, "brief", "ack", idPlainFull)
	r.must(t, r.third, "brief", "outcome", idPlainFull, "--body-file", bodyFile(t, "plain done"))
	if cout := r.must(t, r.hub, "brief", "confirm", idPlainFull); strings.Contains(cout, "relay") {
		t.Errorf("confirming a brief that was never routed must not mention relaying:\n%s", cout)
	}
	if got := r.env(t, idUnrouted); got.Status != "escalated" {
		t.Errorf("an unrouted escalation must still be escalated, got %q", got.Status)
	}
	if got := r.env(t, idPlainSent).Status; got != "sent" {
		t.Errorf("a plain brief must still be sent, got %q", got)
	}

	// No task queue outside the acting project was written. The target gained nothing beyond
	// its own ack (the control above proves the snapshot can see a queue write at all), and
	// the other three are exactly as they started.
	if !sameSnapshot(afterAck, treeSnapshot(t, filepath.Dir(r.third))) {
		t.Error("the target's project changed after its own ack: outcome/confirm/relay must not write a queue")
	}
	for _, n := range []string{"domain", "hub", "bystander"} {
		if !sameSnapshot(before[n], treeSnapshot(t, filepath.Dir(roots[n]))) {
			t.Errorf("project %s was written by escalate/route/relay; only briefs may move between projects", n)
		}
	}
	// ... and the routing store holds only briefs.
	top, _ := os.ReadDir(r.routingDir)
	var kinds []string
	for _, e := range top {
		kinds = append(kinds, e.Name())
	}
	sort.Strings(kinds)
	if strings.Join(kinds, ",") != "brief" {
		t.Errorf("the routing store must hold only the brief kind, got %v", kinds)
	}
}

// TestCLI_BriefRoute_RelayGuards: the relay writes only a brief still waiting on it, and a
// routed brief the router cannot place goes back to the requester rather than hanging.
func TestCLI_BriefRoute_RelayGuards(t *testing.T) {
	r := newEscalationRig(t)
	route := func(subject string) (parent, child string) {
		id := r.escalate(t, subject)
		r.must(t, r.hub, "brief", "route", id, "--to", "third")
		all, _ := r.store.List(routing.InboxFilter{Kind: "brief"})
		for _, e := range all {
			if e.Parent == id {
				return id, e.ID
			}
		}
		t.Fatal("no child filed")
		return "", ""
	}
	finish := func(child string) {
		r.must(t, r.third, "brief", "ack", child)
		r.must(t, r.third, "brief", "outcome", child, "--body-file", bodyFile(t, "done"))
	}

	// The requester withdrew while the child was in flight: the confirm stands, the relay
	// is skipped, and the withdrawn brief is not resurrected.
	p1, c1 := route("withdrawn mid-flight")
	r.must(t, r.domain, "brief", "withdraw", p1)
	finish(c1)
	out := r.must(t, r.hub, "brief", "confirm", c1)
	if !strings.Contains(out, "not relayed") || r.env(t, p1).Status != "withdrawn" {
		t.Errorf("a withdrawn original must not be relayed onto (status=%q):\n%s", r.env(t, p1).Status, out)
	}

	// The router declines a routed brief (the child came to nothing): the requester hears.
	p2, _ := route("cannot be placed")
	r.must(t, r.hub, "brief", "decline", p2, "--note", "third refused it")
	if got := r.env(t, p2).Status; got != "declined" {
		t.Errorf("a routed brief may be declined by the router, got %q", got)
	}

	// An escalated brief the router cannot place is declined directly.
	p3 := r.escalate(t, "declined outright")
	r.must(t, r.hub, "brief", "decline", p3, "--note", "no project owns this")
	if got := r.env(t, p3).Status; got != "declined" {
		t.Errorf("an escalated brief may be declined by the router, got %q", got)
	}

	// A child forged at the store level - Parent pointing at a brief routed to hub, but sent
	// by someone who is NOT its router - must not relay onto it. The CLI never builds such a
	// child; the guard is what stops a hand-written Parent from becoming a back door.
	p4, _ := route("forged child target")
	forged, err := r.store.Create(routing.CreateParams{
		Kind: "brief", FromSlug: "bystander", ToSlug: "third", Subject: "forged",
		Status: "sent", Parent: p4, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	finish(forged.ID)
	out = r.must(t, r.bystand, "brief", "confirm", forged.ID)
	if got := r.env(t, p4).Status; got != "routed" || !strings.Contains(out, "not relayed") {
		t.Errorf("a bystander's confirm must not relay onto hub's routed brief (status=%q):\n%s", got, out)
	}

	// A plain sent brief cannot be routed: route is only for escalations.
	plain := r.send(t, r.domain, "hub", "plain")
	if out, code := r.run(t, r.hub, "brief", "route", plain, "--to", "third"); code == 0 || !strings.Contains(out, "only an \"escalated\" brief") {
		t.Errorf("routing a non-escalated brief must be refused (code=%d):\n%s", code, out)
	}
}

// TestCLI_BriefEscalate_Validation: escalate refuses what would strand or launder a brief,
// and a refused escalate files nothing.
func TestCLI_BriefEscalate_Validation(t *testing.T) {
	r := newEscalationRig(t)
	cases := map[string][]string{
		"unknown router":   {"--to", "ghost", "--target", "third"},
		"unknown target":   {"--to", "hub", "--target", "ghost"},
		"router is self":   {"--to", "domain", "--target", "third"},
		"target is self":   {"--to", "hub", "--target", "domain"},
		"target is router": {"--to", "hub", "--target", "hub"},
		"missing target":   {"--to", "hub"},
		"missing router":   {"--target", "third"},
	}
	for name, flags := range cases {
		args := append([]string{"brief", "escalate", "--subject", "x"}, flags...)
		if out, code := r.run(t, r.domain, args...); code == 0 {
			t.Errorf("%s must be refused:\n%s", name, out)
		}
	}
	if all, _ := r.store.List(routing.InboxFilter{}); len(all) != 0 {
		t.Errorf("refused escalations must file nothing, got %d item(s)", len(all))
	}
}

// TestCLI_BriefRoute_FailedFilingRestoresEscalation: if the child cannot be filed, the
// escalated brief goes back to `escalated` (with the failure on its history) and can be
// routed again, rather than being stranded as `routed` with no child.
func TestCLI_BriefRoute_FailedFilingRestoresEscalation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	r := newEscalationRig(t)
	id := r.escalate(t, "cannot be filed")
	// The brief's own dir (brief/hub) stays writable, so the status flip succeeds; creating
	// brief/third under the now read-only brief/ dir fails.
	briefDir := filepath.Join(r.routingDir, "brief")
	if err := os.Chmod(briefDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(briefDir, 0o755) })

	out, code := r.run(t, r.hub, "brief", "route", id, "--to", "third")
	if code == 0 || !strings.Contains(out, "filing the brief to third failed") {
		t.Fatalf("a failed filing must exit non-zero and say so (code=%d):\n%s", code, out)
	}
	env := r.env(t, id)
	if env.Status != "escalated" {
		t.Fatalf("the escalated brief must be restored, got %q", env.Status)
	}
	if last := env.History[len(env.History)-1]; last.Action != "route-failed" {
		t.Errorf("the failure must be on the brief's history, got %+v", last)
	}
	if kids := derivedFrom(r.store, id); len(kids) != 0 {
		t.Errorf("no child may exist after a failed filing, got %d", len(kids))
	}
	// And it can be routed once the store is writable again.
	if err := os.Chmod(briefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.must(t, r.hub, "brief", "route", id, "--to", "third")
}
