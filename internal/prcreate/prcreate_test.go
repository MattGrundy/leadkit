package prcreate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
)

// fakeRunner captures every argv it is asked to run and NEVER execs a real forge —
// this is what guarantees the tests never invoke gh/glab or reach GitHub/GitLab.
type fakeRunner struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeRunner) Run(argv []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	return f.out, f.err
}

// baseReq is the standard PR-create request the tests reuse.
func baseReq() Request {
	return Request{Base: "develop", Head: "feat/x", Title: "T", BodyFile: "/tmp/b.md"}
}

// allowedInput is a governed, gated, github origin with can_open_pr:true — the happy path.
func allowedInput() DecideInput {
	return DecideInput{
		OriginURL:    "git@github.com:acmecorp/widgets.git",
		AuthKey:      "acmecorp/widgets",
		Spec:         config.AuthoritySpec{CanOpenPR: config.BoolOrHuman{Allow: true}},
		HasAuthority: true,
		Req:          baseReq(),
		HeadSHA:      "abcdef1234567890",
		GateOK:       true,
		LineageOK:    true,
		GateReason:   "all layers ready",
	}
}

func TestCanOpenPRTrueGated_Allowed_RunnerInvokedOnce(t *testing.T) {
	in := allowedInput()
	fr := &fakeRunner{}
	d, _, err := Execute(in, false, fr)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("want Allowed, got refused: %s", d.Reason)
	}
	want := []string{"gh", "pr", "create", "--repo", "acmecorp/widgets", "--base", "develop",
		"--head", "feat/x", "--title", "T", "--body-file", "/tmp/b.md"}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, want)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("want runner invoked exactly once, got %d calls", len(fr.calls))
	}
	if !reflect.DeepEqual(fr.calls[0], want) {
		t.Fatalf("runner argv mismatch:\n got %v\nwant %v", fr.calls[0], want)
	}
}

func TestPRDraftOnly_ForcesDraft_EvenWithoutFlag(t *testing.T) {
	in := allowedInput()
	in.Spec.PRDraftOnly = true
	in.Req.Draft = false // caller did NOT ask for draft
	fr := &fakeRunner{}
	d, _, _ := Execute(in, false, fr)
	if !d.Allowed {
		t.Fatalf("want Allowed, got: %s", d.Reason)
	}
	if !d.Draft {
		t.Fatalf("pr_draft_only must force Draft=true")
	}
	if !containsArg(d.Argv, "--draft") {
		t.Fatalf("pr_draft_only must force --draft into argv, got %v", d.Argv)
	}
	if len(fr.calls) != 1 || !containsArg(fr.calls[0], "--draft") {
		t.Fatalf("runner must be invoked once with --draft, got %v", fr.calls)
	}
}

func TestCanOpenPRFalse_Refused_RunnerNeverInvoked(t *testing.T) {
	in := allowedInput()
	in.Spec.CanOpenPR = config.BoolOrHuman{Allow: false}
	fr := &fakeRunner{}
	d, _, _ := Execute(in, false, fr)
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

func TestCanOpenPRHuman_Refused_EscalationMessage(t *testing.T) {
	in := allowedInput()
	in.Spec.CanOpenPR = config.BoolOrHuman{Human: true}
	fr := &fakeRunner{}
	d, _, _ := Execute(in, false, fr)
	if d.Allowed {
		t.Fatalf("can_open_pr:human must refuse")
	}
	if !strings.Contains(d.Reason, "requires a human") || !strings.Contains(d.Reason, "escalates") {
		t.Fatalf("want human-specific escalation message, got %q", d.Reason)
	}
	// Distinct from the false wording.
	if strings.Contains(d.Reason, "not wired") {
		t.Fatalf("human message must differ from the false message, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked, got %d calls", len(fr.calls))
	}
}

func TestNoAuthorityMatch_Refused_DefaultDeny(t *testing.T) {
	in := allowedInput()
	in.HasAuthority = false
	in.AuthKey = ""
	fr := &fakeRunner{}
	d, _, _ := Execute(in, false, fr)
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

func TestRequiredGatesUnsatisfied_Refused_ThenUserApprovedAllows(t *testing.T) {
	fr := &fakeRunner{}

	// Not gated, no --user-approved → refuse with the gate reason.
	in := allowedInput()
	in.GateOK = false
	in.GateReason = "layers not ready: critique=MISSING"
	d, _, _ := Execute(in, false, fr)
	if d.Allowed {
		t.Fatalf("ungated head must refuse without --user-approved")
	}
	if !strings.Contains(d.Reason, "not gated") || !strings.Contains(d.Reason, "critique=MISSING") {
		t.Fatalf("refusal must carry the gate reason, got %q", d.Reason)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must not run on a gate refusal, got %d", len(fr.calls))
	}

	// Same ungated head, WITH --user-approved → allowed (human substitute), runner runs.
	in.UserApproved = true
	d2, _, _ := Execute(in, false, fr)
	if !d2.Allowed {
		t.Fatalf("--user-approved must substitute for the gate, got: %s", d2.Reason)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner must run once once approved, got %d", len(fr.calls))
	}
}

func TestRequiredGatesSatisfied_Allowed(t *testing.T) {
	in := allowedInput() // GateOK true
	fr := &fakeRunner{}
	d, _, _ := Execute(in, false, fr)
	if !d.Allowed {
		t.Fatalf("gated head must be allowed, got: %s", d.Reason)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("runner must run once, got %d", len(fr.calls))
	}
}

func TestDryRun_PlanOnly_RunnerNeverInvoked(t *testing.T) {
	in := allowedInput()
	fr := &fakeRunner{}
	d, _, _ := Execute(in, true, fr) // dryRun=true
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

func TestOwnerNameDerivation_SSHandHTTPS(t *testing.T) {
	cases := []struct {
		url       string
		wantHost  string
		wantOwner string
	}{
		{"git@github.com:acmecorp/widgets.git", "github.com", "acmecorp/widgets"},
		{"https://github.com/acmecorp/widgets.git", "github.com", "acmecorp/widgets"},
		{"https://github.com/acmecorp/widgets", "github.com", "acmecorp/widgets"},
		{"ssh://git@gitlab.example.com/grp/sub/proj.git", "gitlab.example.com", "grp/sub/proj"},
	}
	for _, c := range cases {
		host, owner := ParseOrigin(c.url)
		if host != c.wantHost || owner != c.wantOwner {
			t.Errorf("ParseOrigin(%q) = (%q,%q); want (%q,%q)", c.url, host, owner, c.wantHost, c.wantOwner)
		}
		if OwnerName(c.url) != c.wantOwner {
			t.Errorf("OwnerName(%q) = %q; want %q", c.url, OwnerName(c.url), c.wantOwner)
		}
	}
}

func TestForgeSelection_GitHubVsGitLab_SameRequest(t *testing.T) {
	// Same governed request; only the origin host differs → different forge argv.
	gh := allowedInput()
	gh.OriginURL = "git@github.com:acmecorp/widgets.git"
	dGH := Decide(gh)
	if !dGH.Allowed || dGH.Argv[0] != "gh" {
		t.Fatalf("github origin should compose a gh argv, got %v (%s)", dGH.Argv, dGH.Reason)
	}
	wantGH := []string{"gh", "pr", "create", "--repo", "acmecorp/widgets", "--base", "develop",
		"--head", "feat/x", "--title", "T", "--body-file", "/tmp/b.md"}
	if !reflect.DeepEqual(dGH.Argv, wantGH) {
		t.Fatalf("gh argv:\n got %v\nwant %v", dGH.Argv, wantGH)
	}

	gl := allowedInput()
	gl.OriginURL = "git@gitlab.com:acmecorp/widgets.git"
	gl.AuthKey = "acmecorp/widgets"
	dGL := Decide(gl)
	if !dGL.Allowed || dGL.Argv[0] != "glab" {
		t.Fatalf("gitlab origin should compose a glab argv, got %v (%s)", dGL.Argv, dGL.Reason)
	}
	wantGL := []string{"glab", "mr", "create", "--source-branch", "feat/x", "--target-branch", "develop",
		"--title", "T", "--description", "/tmp/b.md"}
	if !reflect.DeepEqual(dGL.Argv, wantGL) {
		t.Fatalf("glab argv:\n got %v\nwant %v", dGL.Argv, wantGL)
	}
}

func TestForgeSelection_UnknownHost_RefusesUnlessForgeOverride(t *testing.T) {
	// Unknown self-hosted host, no override → refuse ("cannot determine forge").
	unknown := allowedInput()
	unknown.OriginURL = "git@git.internal.example:acmecorp/widgets.git"
	d := Decide(unknown)
	if d.Allowed {
		t.Fatalf("unknown host with no forge override must refuse")
	}
	if !strings.Contains(d.Reason, "cannot determine forge") {
		t.Fatalf("want 'cannot determine forge', got %q", d.Reason)
	}

	// Same host WITH authority.<repo>.forge: gitlab → glab argv.
	withOverride := unknown
	withOverride.Spec = config.AuthoritySpec{CanOpenPR: config.BoolOrHuman{Allow: true}, Forge: "gitlab"}
	d2 := Decide(withOverride)
	if !d2.Allowed || d2.Argv[0] != "glab" {
		t.Fatalf("forge:gitlab override must compose a glab argv, got %v (%s)", d2.Argv, d2.Reason)
	}
}

// fakeGit is a stub gate.Git so the gate test never touches a real repo.
type fakeGit struct {
	sha    string
	remote string
}

func (f fakeGit) RevParse(repo, ref string) (string, bool) { return f.sha, f.sha != "" }
func (f fakeGit) RemoteURL(repo string) string             { return f.remote }

// TestGateBeforePR_RealGateFakeGit exercises the gate-before-PR flow end to end through
// a REAL gate.Gate (fake gate.Git + a t.TempDir records dir) feeding GateReady, then
// Decide — ungated refuses, a recorded HEAD is allowed, and --user-approved bypasses.
func TestGateBeforePR_RealGateFakeGit(t *testing.T) {
	dir := t.TempDir()
	headSHA := "0123456789abcdef0123456789abcdef01234567"
	g := &gate.Gate{
		RecordsDir:      dir,
		Git:             fakeGit{sha: headSHA, remote: "git@github.com:acmecorp/widgets.git"},
		EnforcedRemotes: []string{"acmecorp/widgets"}, // governed repo is in-scope for the gate
	}

	// Ungated → GateReady false → Decide refuses.
	ok, reason := GateReady(g, headSHA, "", gate.RequiredLayers)
	if ok {
		t.Fatalf("ungated head must not be ready")
	}
	in := allowedInput()
	in.HeadSHA, in.GateOK, in.GateReason = headSHA, ok, reason
	in.LineageOK = true
	if d := Decide(in); d.Allowed {
		t.Fatalf("ungated head must refuse, got allowed")
	}

	// Record both required layers ready → GateReady true → Decide allows.
	if _, err := g.Record(gate.RecordOpts{SHA: headSHA, Layer: "code-review", Verdict: "ready", By: "cr"}); err != nil {
		t.Fatalf("record code-review: %v", err)
	}
	if _, err := g.Record(gate.RecordOpts{SHA: headSHA, Layer: "critique", Verdict: "ready", By: "crit"}); err != nil {
		t.Fatalf("record critique: %v", err)
	}
	ok2, reason2 := GateReady(g, headSHA, "", gate.RequiredLayers)
	if !ok2 {
		t.Fatalf("both layers ready should pass the gate, got: %s", reason2)
	}
	in.GateOK, in.GateReason = ok2, reason2
	if d := Decide(in); !d.Allowed {
		t.Fatalf("gated head must be allowed, got: %s", d.Reason)
	}

	// Sanity: a real record file was written under the tempdir, nothing real was touched.
	if _, err := os.Stat(filepath.Join(dir, headSHA+".json")); err != nil {
		t.Fatalf("expected a record file in the tempdir: %v", err)
	}
}

func TestResolveAuthority_SubstringMatch(t *testing.T) {
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"acmecorp/widgets": {CanOpenPR: config.BoolOrHuman{Allow: true}},
	}}
	key, _, ok := ResolveAuthority(prof, "git@github.com:acmecorp/widgets.git")
	if !ok || key != "acmecorp/widgets" {
		t.Fatalf("want match on acmecorp/widgets, got key=%q ok=%v", key, ok)
	}
	if _, _, ok := ResolveAuthority(prof, "git@github.com:other/repo.git"); ok {
		t.Fatalf("non-matching origin must be default-deny")
	}
}

// TestResolveAuthority_LongestKeyWinsDeterministic: pr create/merge must resolve the
// SAME deterministic entry the push-guard's shared matcher does — when two keys match
// one origin, the longer (more-specific) wins, stably across runs. Guards against the
// old map-random first-match.
func TestResolveAuthority_LongestKeyWinsDeterministic(t *testing.T) {
	origin := "git@github.com:acmecorp/shared-infra.git"
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"infra":                {MergeGrant: "short"},
		"acmecorp/shared-infra": {MergeGrant: "specific"},
	}}
	for i := 0; i < 100; i++ {
		key, spec, ok := ResolveAuthority(prof, origin)
		if !ok || key != "acmecorp/shared-infra" || spec.MergeGrant != "specific" {
			t.Fatalf("run %d: got key=%q grant=%q ok=%v, want the longest key", i, key, spec.MergeGrant, ok)
		}
	}
	// Same result as the shared matcher itself (single source of truth).
	wantKey, _, _ := config.MatchAuthorityKey(origin, prof.Authority, false)
	if wantKey != "acmecorp/shared-infra" {
		t.Fatalf("shared matcher disagrees: %q", wantKey)
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
