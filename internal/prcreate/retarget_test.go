package prcreate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func retargetInput(cur string, ancestor bool) DecideEditInput {
	in := allowedEdit()
	in.Req = EditRequest{PRNumber: "4", Base: "main", BaseSet: true}
	in.BaseCheck = BaseCheck{Resolved: true, CurrentBase: cur, Ancestor: ancestor}
	in.LeadOK = true
	in.ConfirmAtUse = func(RetargetFacts) error { return nil }
	return in
}

// The refusal is the part that proves the guard: a retarget whose current base is NOT merged into
// the new one changes what a merge does, and must not reach the forge.
func TestRetarget_RefusedWhenTheCurrentBaseIsNotMergedIntoTheNewOne(t *testing.T) {
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(retargetInput("feat/stack-below", false), false, fr)
	if d.Allowed {
		t.Fatal("a retarget whose current base is not an ancestor of the new base must be REFUSED")
	}
	for _, want := range []string{"retarget of PR #4 refused", `"feat/stack-below" is NOT merged into "main"`, "escalate"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason lacks %q: %s", want, d.Reason)
		}
	}
	if len(fr.calls) != 0 {
		t.Errorf("the forge must never be called for a refused retarget, got %v", fr.calls)
	}
}

func TestRetarget_AllowedWhenStrictlyNarrowing(t *testing.T) {
	fr := &fakeRunner{}
	d, _, err := ExecuteEdit(retargetInput("test/coverage-and-unit-gaps", true), false, fr)
	if err != nil || !d.Allowed {
		t.Fatalf("a retarget whose current base is already merged into the new one must be allowed: %v %s", err, d.Reason)
	}
	want := []string{"gh", "pr", "edit", "4", "--repo", "acmecorp/widgets", "--base", "main"}
	if !reflect.DeepEqual(d.Argv, want) || len(fr.calls) != 1 || !reflect.DeepEqual(fr.calls[0], want) {
		t.Errorf("argv = %v, calls = %v, want %v once", d.Argv, fr.calls, want)
	}
	if !strings.Contains(d.Reason, "base") {
		t.Errorf("the allow reason should name the base edit: %s", d.Reason)
	}
}

func TestRetarget_UnverifiedEvidenceIsARefusalNeverAnAllow(t *testing.T) {
	in := retargetInput("x", true)
	in.BaseCheck = BaseCheck{Resolved: false, Reason: "gh pr view: exit status 1"}
	d := DecideEdit(in)
	if d.Allowed || !strings.Contains(d.Reason, "Fail-closed") || !strings.Contains(d.Reason, "gh pr view") {
		t.Errorf("unresolved evidence must refuse fail-closed, naming why: allowed=%v %s", d.Allowed, d.Reason)
	}
	// Ancestor=true is NOT enough on its own: Resolved gates it.
	in.BaseCheck = BaseCheck{Resolved: false, Ancestor: true, CurrentBase: "x"}
	if DecideEdit(in).Allowed {
		t.Error("Ancestor without Resolved must not allow")
	}
}

func TestRetarget_NoOpAndEmptyBaseAreRefused(t *testing.T) {
	if d := DecideEdit(retargetInput("main", true)); d.Allowed || !strings.Contains(d.Reason, "already the base") {
		t.Errorf("retargeting onto the current base is a no-op: %v %s", d.Allowed, d.Reason)
	}
	in := retargetInput("a", true)
	in.Req.Base = "  "
	if d := DecideEdit(in); d.Allowed {
		t.Error("an empty --base must be refused")
	}
}

func TestRetarget_CannotBeSmuggledThroughThePassthrough(t *testing.T) {
	for _, flag := range []string{"--base", "-B", "--target-branch", "--base=main"} {
		in := allowedEdit()
		in.Req.Extra = []string{flag, "main"}
		if d := DecideEdit(in); d.Allowed || !strings.Contains(d.Reason, "allowlist") {
			t.Errorf("%q in the passthrough must be refused: allowed=%v %s", flag, d.Allowed, d.Reason)
		}
	}
	in := retargetInput("a", true)
	in.Req.Extra = []string{"--base", "evil"}
	if DecideEdit(in).Allowed {
		t.Error("a governed --base plus a smuggled one must be refused")
	}
}

func TestRetarget_GovernanceStillApplies(t *testing.T) {
	in := retargetInput("a", true)
	in.HasAuthority = false
	if DecideEdit(in).Allowed {
		t.Error("an ungoverned repo must never retarget")
	}
	in = retargetInput("a", true)
	in.Spec.CanOpenPR.Allow, in.Spec.CanOpenPR.Human = false, true
	if DecideEdit(in).Allowed {
		t.Error("can_open_pr: human must refuse a retarget")
	}
	in = retargetInput("a", true)
	in.Spec.OwnerPRsOnly = true
	if DecideEdit(in).Allowed {
		t.Error("owner_prs_only with unconfirmed ownership must refuse a retarget")
	}
}

func TestRetarget_GitLabUsesItsOwnFlag(t *testing.T) {
	in := retargetInput("a", true)
	in.OriginURL = "git@gitlab.com:acmecorp/widgets.git"
	d := DecideEdit(in)
	if !d.Allowed || !containsArg(d.Argv, "--target-branch") || containsArg(d.Argv, "--base") {
		t.Errorf("gitlab argv = %v (%s)", d.Argv, d.Reason)
	}
}

func TestRetarget_BaseOnlyEditIsNotANoOp(t *testing.T) {
	in := allowedEdit()
	in.Req = EditRequest{PRNumber: "4"}
	if d := DecideEdit(in); d.Allowed || !strings.Contains(d.Reason, "nothing to edit") {
		t.Errorf("an edit with nothing set must still be refused: %s", d.Reason)
	}
}

// --- ResolveBaseCheck, with a fake probe ----------------------------------------------------------

type fakeProbe struct {
	cur       string
	curErr    error
	refErr    error
	refreshed []string
	anc       bool
	ancErr    error
	askedFor  [2]string
}

func (f *fakeProbe) Refresh(_ string, b ...string) error {
	f.refreshed = append(f.refreshed, b...)
	return f.refErr
}
func (f *fakeProbe) CurrentBase(_, _, _ string) (string, error) { return f.cur, f.curErr }
func (f *fakeProbe) IsAncestor(_, a, d string) (bool, error) {
	f.askedFor = [2]string{a, d}
	return f.anc, f.ancErr
}

func TestResolveBaseCheck(t *testing.T) {
	p := &fakeProbe{cur: "feat/below", anc: true}
	bc := ResolveBaseCheck(p, "github", "", "4", "main")
	if !bc.Resolved || !bc.Ancestor || bc.CurrentBase != "feat/below" {
		t.Errorf("got %+v", bc)
	}
	if !reflect.DeepEqual(p.refreshed, []string{"feat/below", "main"}) {
		t.Errorf("both branches must be refreshed before ancestry is judged, refreshed %v", p.refreshed)
	}
	if p.askedFor != [2]string{"origin/feat/below", "origin/main"} {
		t.Errorf("ancestry must be judged on the REMOTE-TRACKING refs, asked %v", p.askedFor)
	}
	if bc := ResolveBaseCheck(&fakeProbe{cur: "feat/below", anc: false}, "github", "", "4", "main"); !bc.Resolved || bc.Ancestor {
		t.Errorf("a not-merged base resolves to Ancestor=false: %+v", bc)
	}
	for name, probe := range map[string]*fakeProbe{
		"forge lookup fails": {curErr: errors.New("offline")},
		"empty answer":       {cur: ""},
		"ref missing":        {cur: "a", ancErr: errors.New("origin/a is not a ref in this clone")},
		"refresh fails":      {cur: "a", anc: true, refErr: errors.New("could not resolve host")},
	} {
		if bc := ResolveBaseCheck(probe, "github", "", "4", "main"); bc.Resolved || bc.Reason == "" {
			t.Errorf("%s must be unresolved with a reason: %+v", name, bc)
		}
	}
	if bc := ResolveBaseCheck(&fakeProbe{cur: "a", anc: true}, "gitlab", "", "4", "main"); bc.Resolved {
		t.Error("only github is verifiable; gitlab must be unresolved (fail-closed)")
	}
}

// --- ExecProbe.IsAncestor against a real git repo -------------------------------------------------

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestExecProbe_IsAncestor_RealGit(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	commit := func(msg string) string {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-qm", msg)
		return gitIn(t, dir, "rev-parse", "HEAD")
	}
	root := commit("root")
	below := commit("stack base")   // main will contain this
	main := commit("main moved on") // descendant of below
	gitIn(t, dir, "checkout", "-q", "-b", "side", root)
	side := commit("diverged") // NOT an ancestor of main
	for ref, sha := range map[string]string{"origin/below": below, "origin/main": main, "origin/side": side} {
		gitIn(t, dir, "update-ref", "refs/remotes/"+ref, sha)
	}

	p := ExecProbe{}
	if ok, err := p.IsAncestor(dir, "origin/below", "origin/main"); err != nil || !ok {
		t.Errorf("a merged base is an ancestor: %v %v", ok, err)
	}
	if ok, err := p.IsAncestor(dir, "origin/side", "origin/main"); err != nil || ok {
		t.Errorf("a diverged branch is NOT an ancestor (and that is an answer, not an error): %v %v", ok, err)
	}
	if ok, err := p.IsAncestor(dir, "origin/main", "origin/below"); err != nil || ok {
		t.Errorf("the reverse direction would WIDEN the diff and must read false: %v %v", ok, err)
	}
	if _, err := p.IsAncestor(dir, "origin/nope", "origin/main"); err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("a missing ref is an error that says how to fix it, not false: %v", err)
	}
}

// Stale remote-tracking refs after a force-push can still say "ancestor". With a real origin, the
// ancestry judged through ResolveBaseCheck must follow the forge, not the clone's last fetch.
func TestExecProbe_Refresh_AForcePushedBaseIsNoLongerAnAncestor(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	gitIn(t, base, "init", "-q", "--bare", origin)
	work := filepath.Join(base, "work")
	gitIn(t, base, "clone", "-q", origin, work)
	commit := func(msg string) {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, work, "add", "-A")
		gitIn(t, work, "commit", "-qm", msg)
	}
	commit("root")
	gitIn(t, work, "branch", "-M", "main")
	gitIn(t, work, "push", "-q", "origin", "main")
	gitIn(t, work, "checkout", "-q", "-b", "below")
	commit("stack base")
	gitIn(t, work, "push", "-q", "origin", "below")
	gitIn(t, work, "checkout", "-q", "main")
	gitIn(t, work, "merge", "-q", "--no-ff", "-m", "merge below", "below")
	gitIn(t, work, "push", "-q", "origin", "main")
	gitIn(t, work, "fetch", "-q", "origin")

	p := ExecProbe{}
	if bc := ResolveBaseCheck(&execBase{ExecProbe: p, base: "below"}, "github", work, "4", "main"); !bc.Resolved || !bc.Ancestor {
		t.Fatalf("precondition: a merged base is an ancestor: %+v", bc)
	}

	// Someone force-pushes `below` to a commit main does not contain; THIS clone has not fetched.
	other := filepath.Join(base, "other")
	gitIn(t, base, "clone", "-q", origin, other)
	gitIn(t, other, "checkout", "-q", "-B", "below", "origin/main")
	if err := os.WriteFile(filepath.Join(other, "f"), []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, other, "add", "-A")
	gitIn(t, other, "commit", "-qm", "rewritten base")
	gitIn(t, other, "push", "-q", "-f", "origin", "below")

	if ok, err := p.IsAncestor(work, "origin/below", "origin/main"); err != nil || !ok {
		t.Fatalf("precondition: the stale ref still claims ancestor (that is the hazard): %v %v", ok, err)
	}
	bc := ResolveBaseCheck(&execBase{ExecProbe: p, base: "below"}, "github", work, "4", "main")
	if !bc.Resolved || bc.Ancestor {
		t.Errorf("after the force-push the base is NOT merged into main, and the check must see it: %+v", bc)
	}

	// An unreachable origin refuses (fail-closed) instead of judging on old refs.
	gitIn(t, work, "remote", "set-url", "origin", filepath.Join(base, "gone.git"))
	if bc := ResolveBaseCheck(&execBase{ExecProbe: p, base: "below"}, "github", work, "4", "main"); bc.Resolved {
		t.Errorf("a failed refresh must leave the evidence unresolved: %+v", bc)
	}
}

// execBase is the real probe with the forge lookup (gh) replaced by a fixed answer.
type execBase struct {
	ExecProbe
	base string
}

func (e *execBase) CurrentBase(_, _, _ string) (string, error) { return e.base, nil }

// WHO may retarget and WHETHER THE OWNER CONFIRMS are independent of each other and of the ancestry
// check: each missing condition refuses on its own, and the zero value of each refuses too.
func TestRetarget_RoleAndConfirmationAreEachRequired(t *testing.T) {
	if d := DecideEdit(retargetInput("below", true)); !d.Allowed {
		t.Fatalf("control: with lead, a way to confirm, and ancestry the retarget is allowed: %s", d.Reason)
	}

	notLead := retargetInput("below", true)
	notLead.LeadOK, notLead.LeadReason = false, `this session is registered as "ic", not lead`
	if d := DecideEdit(notLead); d.Allowed || !strings.Contains(d.Reason, "only a lead may retarget") || !strings.Contains(d.Reason, `"ic"`) {
		t.Errorf("a non-lead must be refused, naming the role: allowed=%v %s", d.Allowed, d.Reason)
	}

	noWayToAsk := retargetInput("below", true)
	noWayToAsk.ConfirmAtUse = nil
	if d := DecideEdit(noWayToAsk); d.Allowed || !strings.Contains(d.Reason, "no way to ask the owner") {
		t.Errorf("a retarget that cannot ask the owner must be refused: allowed=%v %s", d.Allowed, d.Reason)
	}

	if d := DecideEdit(retargetInput("below", false)); d.Allowed || !strings.Contains(d.Reason, "NOT merged") {
		t.Errorf("ancestry is still required: %v %s", d.Allowed, d.Reason)
	}

	if d := DecideEdit(allowedEdit()); !d.Allowed {
		t.Errorf("a title/body edit is gated on neither role nor confirmation: %s", d.Reason)
	}
}

// The confirmation is asked once, with the facts the owner is confirming, after every other
// check, and only on the path that is about to call the forge.
func TestExecuteEdit_TheOwnerIsAskedAtUseAndOnlyThen(t *testing.T) {
	var asked []RetargetFacts
	answer := error(nil)
	fixture := func(cur string, ancestor bool) DecideEditInput {
		in := retargetInput(cur, ancestor)
		asked = nil
		in.ConfirmAtUse = func(f RetargetFacts) error { asked = append(asked, f); return answer }
		return in
	}

	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(fixture("below", true), false, fr)
	want := RetargetFacts{Repo: "acmecorp/widgets", PR: "4", From: "below", To: "main"}
	if !d.Allowed || len(fr.calls) != 1 || len(asked) != 1 || asked[0] != want {
		t.Fatalf("an allowed retarget asks once with %+v then runs: allowed=%v asked=%v calls=%v", want, d.Allowed, asked, fr.calls)
	}

	for name, run := range map[string]func() (Decision, int){
		"dry run": func() (Decision, int) {
			f := &fakeRunner{}
			d, _, _ := ExecuteEdit(fixture("below", true), true, f)
			return d, len(f.calls)
		},
		"ancestry fails": func() (Decision, int) {
			f := &fakeRunner{}
			d, _, _ := ExecuteEdit(fixture("diverged", false), false, f)
			return d, len(f.calls)
		},
		"not a lead": func() (Decision, int) {
			in := fixture("below", true)
			in.LeadOK = false
			f := &fakeRunner{}
			d, _, _ := ExecuteEdit(in, false, f)
			return d, len(f.calls)
		},
		"title only": func() (Decision, int) {
			in := allowedEdit()
			in.ConfirmAtUse = func(f RetargetFacts) error { asked = append(asked, f); return nil }
			asked = nil
			f := &fakeRunner{}
			d, _, _ := ExecuteEdit(in, false, f)
			if !d.Allowed {
				t.Fatalf("control: %s", d.Reason)
			}
			return Decision{}, len(f.calls) - 1 // one call is expected here
		},
	} {
		_, calls := run()
		if len(asked) != 0 {
			t.Errorf("%s: the owner must not be asked, was asked %v", name, asked)
		}
		if calls != 0 {
			t.Errorf("%s: the forge was called %d time(s)", name, calls)
		}
	}

	answer = errors.New("the typed number did not match")
	fr = &fakeRunner{}
	d, _, err := ExecuteEdit(fixture("below", true), false, fr)
	if d.Allowed || err != nil || len(fr.calls) != 0 || !strings.Contains(d.Reason, "typed number did not match") {
		t.Fatalf("a declined confirmation must stop the forge call and say why: allowed=%v err=%v calls=%v reason=%q", d.Allowed, err, fr.calls, d.Reason)
	}
}
