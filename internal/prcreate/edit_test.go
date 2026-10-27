package prcreate

import (
	"reflect"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// allowedEdit is a governed github origin with can_open_pr:true, editing PR 42 with a
// new title AND body — the happy path.
func allowedEdit() DecideEditInput {
	return DecideEditInput{
		OriginURL:    "git@github.com:acmecorp/widgets.git",
		AuthKey:      "acmecorp/widgets",
		Spec:         config.AuthoritySpec{CanOpenPR: config.BoolOrHuman{Allow: true}},
		HasAuthority: true,
		Req: EditRequest{
			PRNumber: "42",
			Title:    "New title", TitleSet: true,
			BodyFile: "/tmp/b.md", BodySet: true,
		},
	}
}

func TestEdit_HappyPath_Allowed_RunnerInvokedOnce(t *testing.T) {
	in := allowedEdit()
	fr := &fakeRunner{}
	d, _, err := ExecuteEdit(in, false, fr)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("want Allowed, got refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "edit", "42", "--repo", "acmecorp/widgets",
		"--title", "New title", "--body-file", "/tmp/b.md"}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, want)
	}
	if len(fr.calls) != 1 || !reflect.DeepEqual(fr.calls[0], want) {
		t.Fatalf("runner must be invoked once with the argv, got %v", fr.calls)
	}
}

func TestEdit_NoAuthorityMatch_Refused_DefaultDeny(t *testing.T) {
	in := allowedEdit()
	in.HasAuthority = false
	in.AuthKey = ""
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if d.Allowed {
		t.Fatalf("no authority match must refuse (default-deny)")
	}
	if !strings.Contains(d.Reason, "no authority") {
		t.Fatalf("want no-authority message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked for an ungoverned repo, got %d calls", len(fr.calls))
	}
}

func TestEdit_NoOp_NeitherFieldSet_Refused(t *testing.T) {
	in := allowedEdit()
	in.Req.TitleSet, in.Req.BodySet = false, false
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if d.Allowed {
		t.Fatalf("a no-op edit (neither field) must refuse")
	}
	if !strings.Contains(d.Reason, "nothing to edit") {
		t.Fatalf("want no-op message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked on a no-op, got %d calls", len(fr.calls))
	}
}

func TestEdit_CanOpenPRFalse_Refused(t *testing.T) {
	in := allowedEdit()
	in.Spec.CanOpenPR = config.BoolOrHuman{Allow: false}
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if d.Allowed {
		t.Fatalf("can_open_pr:false must refuse")
	}
	if !strings.Contains(d.Reason, "not wired") {
		t.Fatalf("want false-specific 'not wired' message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked on refusal, got %d calls", len(fr.calls))
	}
}

func TestEdit_CanOpenPRHuman_Refused_EscalationMessage(t *testing.T) {
	in := allowedEdit()
	in.Spec.CanOpenPR = config.BoolOrHuman{Human: true}
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if d.Allowed {
		t.Fatalf("can_open_pr:human must refuse")
	}
	if !strings.Contains(d.Reason, "requires a human") || !strings.Contains(d.Reason, "escalates") {
		t.Fatalf("want human-specific escalation message, got %q", d.Reason)
	}
	if strings.Contains(d.Reason, "not wired") {
		t.Fatalf("human message must differ from the false message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked, got %d calls", len(fr.calls))
	}
}

func TestEdit_OwnerPRsOnly_Unconfirmed_FailsClosed(t *testing.T) {
	in := allowedEdit()
	in.Spec.OwnerPRsOnly = true
	in.OwnerConfirmed = false // lookup failed / offline / a different author
	in.OwnerReason = "lookup failed, offline, or a different author"
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if d.Allowed {
		t.Fatalf("owner_prs_only with an unconfirmed owner must FAIL CLOSED (refuse)")
	}
	if !strings.Contains(d.Reason, "owner_prs_only") || !strings.Contains(d.Reason, "Fail-closed") {
		t.Fatalf("want a fail-closed owner_prs_only message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked on a fail-closed owner refusal, got %d calls", len(fr.calls))
	}
}

func TestEdit_OwnerPRsOnly_Confirmed_Allowed(t *testing.T) {
	in := allowedEdit()
	in.Spec.OwnerPRsOnly = true
	in.OwnerConfirmed = true
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, false, fr)
	if !d.Allowed {
		t.Fatalf("owner_prs_only with a confirmed owner must be allowed, got: %s", d.Reason)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner must run once, got %d", len(fr.calls))
	}
}

func TestEdit_OmittedField_NotBlanked(t *testing.T) {
	// Only --title set → argv carries --title and NOT --body-file (body untouched).
	titleOnly := allowedEdit()
	titleOnly.Req.BodySet = false
	titleOnly.Req.BodyFile = ""
	d := DecideEdit(titleOnly)
	if !d.Allowed {
		t.Fatalf("title-only edit must be allowed, got: %s", d.Reason)
	}
	if containsArg(d.Argv, "--body-file") {
		t.Fatalf("an omitted --body-file must NOT be sent (would blank the body), got %v", d.Argv)
	}
	if !containsArg(d.Argv, "--title") {
		t.Fatalf("the provided --title must be sent, got %v", d.Argv)
	}

	// Only --body-file set → argv carries --body-file and NOT --title (title untouched).
	bodyOnly := allowedEdit()
	bodyOnly.Req.TitleSet = false
	bodyOnly.Req.Title = ""
	d2 := DecideEdit(bodyOnly)
	if !d2.Allowed {
		t.Fatalf("body-only edit must be allowed, got: %s", d2.Reason)
	}
	if containsArg(d2.Argv, "--title") {
		t.Fatalf("an omitted --title must NOT be sent (would blank the title), got %v", d2.Argv)
	}
	if !containsArg(d2.Argv, "--body-file") {
		t.Fatalf("the provided --body-file must be sent, got %v", d2.Argv)
	}
}

func TestEdit_NeverUndraftReadyOrMerge(t *testing.T) {
	// No caller input can make the edit path emit a draft-state or merge flag.
	in := allowedEdit()
	in.Spec.PRDraftOnly = true // even a draft-only repo must not add a draft flag on EDIT
	in.Req.Extra = nil
	d := DecideEdit(in)
	if !d.Allowed {
		t.Fatalf("want allowed, got: %s", d.Reason)
	}
	for _, forbidden := range []string{"--draft", "--ready", "--undraft", "merge", "--mark-ready"} {
		if containsArg(d.Argv, forbidden) {
			t.Fatalf("edit argv must NEVER contain %q, got %v", forbidden, d.Argv)
		}
	}
}

func TestEdit_ForgeSelection_GitHubVsGitLab_SameRequest(t *testing.T) {
	gh := allowedEdit()
	dGH := DecideEdit(gh)
	wantGH := []string{"gh", "pr", "edit", "42", "--repo", "acmecorp/widgets",
		"--title", "New title", "--body-file", "/tmp/b.md"}
	if !dGH.Allowed || !reflect.DeepEqual(dGH.Argv, wantGH) {
		t.Fatalf("gh edit argv:\n got %v\nwant %v (%s)", dGH.Argv, wantGH, dGH.Reason)
	}

	gl := allowedEdit()
	gl.OriginURL = "git@gitlab.com:acmecorp/widgets.git"
	dGL := DecideEdit(gl)
	wantGL := []string{"glab", "mr", "update", "42", "--repo", "acmecorp/widgets",
		"--title", "New title", "--description", "/tmp/b.md"}
	if !dGL.Allowed || !reflect.DeepEqual(dGL.Argv, wantGL) {
		t.Fatalf("glab update argv:\n got %v\nwant %v (%s)", dGL.Argv, wantGL, dGL.Reason)
	}
}

func TestEdit_ForgeSelection_UnknownHost_RefusesUnlessOverride(t *testing.T) {
	unknown := allowedEdit()
	unknown.OriginURL = "git@git.internal.example:acmecorp/widgets.git"
	d := DecideEdit(unknown)
	if d.Allowed {
		t.Fatalf("unknown host with no forge override must refuse")
	}
	if !strings.Contains(d.Reason, "cannot determine forge") {
		t.Fatalf("want 'cannot determine forge', got %q", d.Reason)
	}

	withOverride := unknown
	withOverride.Spec = config.AuthoritySpec{CanOpenPR: config.BoolOrHuman{Allow: true}, Forge: "gitlab"}
	d2 := DecideEdit(withOverride)
	if !d2.Allowed || d2.Argv[0] != "glab" {
		t.Fatalf("forge:gitlab override must compose a glab argv, got %v (%s)", d2.Argv, d2.Reason)
	}
}

func TestEdit_DryRun_PlanOnly_RunnerNeverInvoked(t *testing.T) {
	in := allowedEdit()
	fr := &fakeRunner{}
	d, _, _ := ExecuteEdit(in, true, fr)
	if !d.Allowed {
		t.Fatalf("dry-run of an allowed decision should still resolve Allowed, got: %s", d.Reason)
	}
	if len(d.Argv) == 0 {
		t.Fatalf("dry-run must still compute the argv to display")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("dry-run must NEVER invoke the runner, got %d calls", len(fr.calls))
	}
}

func TestForgeName_ReusesForgeFor(t *testing.T) {
	n, err := ForgeName("git@github.com:acmecorp/widgets.git", config.AuthoritySpec{})
	if err != nil || n != "github" {
		t.Fatalf("github origin → (%q,%v), want github", n, err)
	}
	n, err = ForgeName("git@gitlab.com:acmecorp/widgets.git", config.AuthoritySpec{})
	if err != nil || n != "gitlab" {
		t.Fatalf("gitlab origin → (%q,%v), want gitlab", n, err)
	}
	if _, err := ForgeName("git@git.internal.example:acmecorp/widgets.git", config.AuthoritySpec{}); err == nil {
		t.Fatalf("unknown host with no override must error")
	}
}
