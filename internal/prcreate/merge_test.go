package prcreate

import (
	"reflect"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// mergeableInput is the happy path: a governed github origin with can_merge:true, a
// standing merge_grant, readable+OPEN+not-draft+MERGEABLE+CLEAN metadata, no skip-ci
// HEAD, CI green, review-gate 2/2 ready, and a composed squash message. Individual tests
// mutate ONE field to prove each guard.
// gatedHead is the full sha the fixtures' ladder judged; the merge must be pinned to it.
const gatedHead = "abcdef1234567890abcdef1234567890abcdef12"

func mergeableInput() DecideMergeInput {
	return DecideMergeInput{
		OriginURL:    "git@github.com:acmecorp/widgets.git",
		AuthKey:      "acmecorp/widgets",
		Spec:         config.AuthoritySpec{CanMerge: config.BoolOrHuman{Allow: true}, MergeGrant: "INC-42"},
		HasAuthority: true,
		Req:          MergeRequest{PRNumber: "77", Method: "squash"},
		MetaOK:       true,
		State: PRState{
			State: "OPEN", IsDraft: false, Mergeable: "MERGEABLE",
			MergeStateStatus: "CLEAN", HeadSHA: gatedHead, Title: "T",
		},
		SkipCIChecked: true,
		HeadSkipCI:    false,
		CIGreen:       true,
		GateOK:        true,
		LineageOK:     true,
		DependentsOK:  true,
		GateReason:    "all layers ready",
		ComposeOK:     true,
		Subject:       "T (#77)",
		Body:          "* fix: the thing",
	}
}

func wantSquashArgv() []string {
	return []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets",
		"--squash", "--subject", "T (#77)", "--body", "* fix: the thing\n\nLeadkit-Merge: pr-merge", "--match-head-commit", gatedHead}
}

func TestDecideMerge_HappyPath_AllowedWithComposedSquashArgv(t *testing.T) {
	d := DecideMerge(mergeableInput())
	if !d.Allowed {
		t.Fatalf("want Allowed, got refused: %s", d.Reason)
	}
	if !reflect.DeepEqual(d.Argv, wantSquashArgv()) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, wantSquashArgv())
	}
}

func TestDecideMerge_NoAuthority_DefaultDeny(t *testing.T) {
	in := mergeableInput()
	in.HasAuthority = false
	d := DecideMerge(in)
	if d.Allowed || d.Argv != nil {
		t.Fatalf("want default-deny refusal, got %+v", d)
	}
	if !strings.Contains(d.Reason, "no authority") {
		t.Errorf("reason = %q, want the default-deny message", d.Reason)
	}
}

func TestDecideMerge_CanMergeFalse_Refused(t *testing.T) {
	in := mergeableInput()
	in.Spec.CanMerge = config.BoolOrHuman{Allow: false}
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused when can_merge is false")
	}
	if !strings.Contains(d.Reason, "can_merge is false") {
		t.Errorf("reason = %q, want the false-specific message", d.Reason)
	}
}

func TestDecideMerge_CanMergeHuman_EscalationDistinctFromFalse(t *testing.T) {
	in := mergeableInput()
	in.Spec.CanMerge = config.BoolOrHuman{Human: true}
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused when can_merge is human")
	}
	if !strings.Contains(d.Reason, "requires a human") {
		t.Errorf("reason = %q, want the human escalation message (distinct from false)", d.Reason)
	}
}

func TestDecideMerge_NoGrantNoUserApproved_Refused(t *testing.T) {
	in := mergeableInput()
	in.Spec.MergeGrant = ""
	in.UserApproved = false
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused with an empty merge_grant and no --user-approved")
	}
	if !strings.Contains(d.Reason, "merge_grant is empty") {
		t.Errorf("reason = %q, want the proceed-authority message", d.Reason)
	}
}

func TestDecideMerge_NoGrantButUserApproved_Proceeds(t *testing.T) {
	in := mergeableInput()
	in.Spec.MergeGrant = ""
	in.UserApproved = true
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed: --user-approved satisfies the proceed authority; got %s", d.Reason)
	}
}

func TestDecideMerge_MetaFetchFailure_FailsClosed(t *testing.T) {
	in := mergeableInput()
	in.MetaOK = false
	in.MetaReason = "cannot fetch PR"
	// Even with a grant AND --user-approved, an unreadable PR must fail closed.
	in.UserApproved = true
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want fail-closed refusal on an unreadable PR fetch")
	}
	if !strings.Contains(d.Reason, "cannot read PR state") || !strings.Contains(d.Reason, "Fail-closed") {
		t.Errorf("reason = %q, want the fail-closed message", d.Reason)
	}
}

// The OPEN/not-draft/MERGEABLE/CLEAN/skip-ci/CI-green conditions must be refused even
// WITH --user-approved — a human vouches for review, never for these.
func TestDecideMerge_StateGuards_NotSubstitutableByUserApproved(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*DecideMergeInput)
		wantsub string
	}{
		{"not open", func(in *DecideMergeInput) { in.State.State = "MERGED" }, "state=MERGED"},
		{"draft", func(in *DecideMergeInput) { in.State.IsDraft = true }, "is a draft"},
		{"not mergeable", func(in *DecideMergeInput) { in.State.Mergeable = "CONFLICTING" }, "mergeable=CONFLICTING"},
		{"not clean", func(in *DecideMergeInput) { in.State.MergeStateStatus = "BLOCKED" }, "mergeStateStatus=BLOCKED"},
		{"skip-ci HEAD", func(in *DecideMergeInput) { in.HeadSkipCI = true }, "skip-ci directive"},
		{"ci not green", func(in *DecideMergeInput) { in.CIGreen = false; in.CIReason = "1 failing" }, "not all green"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := mergeableInput()
			in.UserApproved = true // the substitute must NOT rescue these
			c.mutate(&in)
			d := DecideMerge(in)
			if d.Allowed {
				t.Fatalf("want refused (%s) even with --user-approved", c.name)
			}
			if !strings.Contains(d.Reason, c.wantsub) {
				t.Errorf("reason = %q, want substring %q", d.Reason, c.wantsub)
			}
		})
	}
}

func TestDecideMerge_SkipCICheckUnresolved_FailsClosed(t *testing.T) {
	in := mergeableInput()
	in.SkipCIChecked = false
	in.SkipCIReason = "commit unreadable"
	in.UserApproved = true
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want fail-closed when the skip-ci HEAD check couldn't resolve")
	}
	if !strings.Contains(d.Reason, "Fail-closed") {
		t.Errorf("reason = %q, want a fail-closed message", d.Reason)
	}
}

// The review-gate 2/2 is the ONLY condition --user-approved substitutes.
func TestDecideMerge_GateNotReady_RefusedWithoutUserApproved(t *testing.T) {
	in := mergeableInput()
	in.GateOK = false
	in.GateReason = "layers not ready: critique=MISSING"
	in.UserApproved = false
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused when the review-gate is not 2/2 and no --user-approved")
	}
	if !strings.Contains(d.Reason, "not gated") {
		t.Errorf("reason = %q, want the gate refusal", d.Reason)
	}
}

func TestDecideMerge_GateNotReady_UserApprovedSubstitutes(t *testing.T) {
	in := mergeableInput()
	in.GateOK = false
	in.GateReason = "layers not ready: critique=MISSING"
	in.UserApproved = true
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed: --user-approved substitutes the review-gate; got %s", d.Reason)
	}
}

// merge_grant enables autonomy but does NOT bypass the review-gate: on a grant alone
// (no --user-approved), an un-gated PR is still refused.
func TestDecideMerge_GrantDoesNotBypassReviewGate(t *testing.T) {
	in := mergeableInput()
	in.Spec.MergeGrant = "INC-42"
	in.UserApproved = false
	in.GateOK = false
	in.GateReason = "layers not ready"
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused: a merge_grant is autonomy, not a review-gate bypass")
	}
	if !strings.Contains(d.Reason, "not gated") {
		t.Errorf("reason = %q, want the gate refusal", d.Reason)
	}
}

func TestDecideMerge_SquashComposeFailure_FailsClosed(t *testing.T) {
	in := mergeableInput()
	in.ComposeOK = false
	in.ComposeReason = "cannot fetch PR commits"
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want fail-closed when the squash compose failed (skip-ci strip guard)")
	}
	if !strings.Contains(d.Reason, "skip-ci directive") {
		t.Errorf("reason = %q, want the compose fail-closed message naming the strip guard", d.Reason)
	}
}

// A non-squash method needs no compose — a compose failure is irrelevant to --merge.
func TestDecideMerge_MergeMethod_NoSubjectBody_ComposeIrrelevant(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "merge"
	in.ComposeOK = false // would fail-close a squash; must not affect --merge
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed for --merge; got %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets", "--merge", "--body", "Leadkit-Merge: pr-merge", "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, want)
	}
}

func TestDecideMerge_RebaseMethod_Argv(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "rebase"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed for --rebase; got %s", d.Reason)
	}
	want := []string{"gh", "pr", "merge", "77", "--repo", "acmecorp/widgets", "--rebase", "--match-head-commit", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, want)
	}
}

func TestDecideMerge_EmptyMethodDefaultsToSquash(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "" // default → squash
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed; got %s", d.Reason)
	}
	if !reflect.DeepEqual(d.Argv, wantSquashArgv()) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", d.Argv, wantSquashArgv())
	}
}

func TestDecideMerge_UnknownMethod_Refused(t *testing.T) {
	in := mergeableInput()
	in.Req.Method = "fast-forward"
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused for an unknown merge method")
	}
	if !strings.Contains(d.Reason, "unknown merge method") {
		t.Errorf("reason = %q, want the unknown-method message", d.Reason)
	}
}

// `--admin` is refused, never appended to the merge argv: the forge's merge command with --admin
// merges past the protections the gate steps exist to respect.
func TestDecideMerge_RefusesAnyPassthrough_AdminIncluded(t *testing.T) {
	in := mergeableInput()
	in.Req.Extra = []string{"--admin"}
	d := DecideMerge(in)
	if d.Allowed || !strings.Contains(d.Reason, "allowlist") || !strings.Contains(d.Reason, "--admin") {
		t.Fatalf("--admin must be refused as off the allowlist: allowed=%v %q", d.Allowed, d.Reason)
	}
	if len(d.Argv) != 0 {
		t.Errorf("a refused merge must carry no argv, got %v", d.Argv)
	}
}

func TestDecideMerge_GitLabForge_MergeArgv(t *testing.T) {
	in := mergeableInput()
	in.OriginURL = "git@gitlab.com:acmecorp/widgets.git"
	in.AuthKey = "acmecorp/widgets"
	d := DecideMerge(in)
	if !d.Allowed {
		t.Fatalf("want allowed for a gitlab origin; got %s", d.Reason)
	}
	want := []string{"glab", "mr", "merge", "77", "--repo", "acmecorp/widgets", "--squash", "--message", "T (#77)\n\nLeadkit-Merge: pr-merge", "--sha", gatedHead}
	if !reflect.DeepEqual(d.Argv, want) {
		t.Fatalf("gitlab argv mismatch:\n got %v\nwant %v", d.Argv, want)
	}
}

func TestDecideMerge_UnknownHost_Refused(t *testing.T) {
	in := mergeableInput()
	in.OriginURL = "git@git.internal.example:acmecorp/widgets.git" // neither github nor gitlab, no forge override
	d := DecideMerge(in)
	if d.Allowed {
		t.Fatal("want refused for an unresolvable forge host (never guess a forge)")
	}
	if !strings.Contains(d.Reason, "cannot determine forge") {
		t.Errorf("reason = %q, want the unresolvable-forge message", d.Reason)
	}
}

// ExecuteMerge orchestration: allowed → runner invoked once with the argv; refusal and
// dry-run → runner NEVER invoked (a merge is irreversible; neither must reach the forge).
func TestExecuteMerge_Allowed_RunnerInvokedOnce(t *testing.T) {
	fr := &fakeRunner{out: "merged"}
	d, out, err := ExecuteMerge(mergeableInput(), false, fr)
	if err != nil || !d.Allowed {
		t.Fatalf("want allowed no-err; got allowed=%v err=%v", d.Allowed, err)
	}
	if out != "merged" {
		t.Errorf("out = %q, want %q", out, "merged")
	}
	if len(fr.calls) != 1 {
		t.Fatalf("want runner invoked exactly once, got %d", len(fr.calls))
	}
	if !reflect.DeepEqual(fr.calls[0], wantSquashArgv()) {
		t.Fatalf("runner argv mismatch:\n got %v\nwant %v", fr.calls[0], wantSquashArgv())
	}
}

func TestExecuteMerge_Refusal_RunnerNeverInvoked(t *testing.T) {
	in := mergeableInput()
	in.HasAuthority = false
	fr := &fakeRunner{}
	d, _, _ := ExecuteMerge(in, false, fr)
	if d.Allowed {
		t.Fatal("want refused")
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner must NEVER be invoked on a refusal, got %d calls", len(fr.calls))
	}
}

func TestExecuteMerge_DryRun_RunnerNeverInvoked(t *testing.T) {
	fr := &fakeRunner{}
	d, out, err := ExecuteMerge(mergeableInput(), true, fr)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !d.Allowed {
		t.Fatal("want the allowed decision surfaced even on dry-run")
	}
	if out != "" || len(fr.calls) != 0 {
		t.Fatalf("dry-run must NEVER invoke the runner, got out=%q calls=%d", out, len(fr.calls))
	}
}

// Ticket 241. The ladder judges ONE head (skip-ci, CI, the gate, the lineage, the compose), and the
// merge then ran with nothing tying it to that head: a push to the branch in between landed commits
// nobody gated. The argv of every allowed merge, on both forges and for every method, carries the sha
// the ladder judged, in the forge's own flag.
func TestDecideMerge_EveryAllowedMergeIsPinnedToTheHeadTheLadderJudged(t *testing.T) {
	for _, forge := range []struct{ name, origin, flag string }{
		{"github", "git@github.com:acmecorp/widgets.git", "--match-head-commit"},
		{"gitlab", "git@gitlab.com:acmecorp/widgets.git", "--sha"},
	} {
		for _, method := range []string{"squash", "merge", "rebase"} {
			t.Run(forge.name+"/"+method, func(t *testing.T) {
				in := mergeableInput()
				in.OriginURL, in.Req.Method = forge.origin, method
				d := DecideMerge(in)
				if !d.Allowed {
					t.Fatalf("refused: %s", d.Reason)
				}
				at := indexOf(d.Argv, forge.flag)
				if at < 0 || at+1 >= len(d.Argv) || d.Argv[at+1] != gatedHead {
					t.Errorf("argv %v does not pin the merge to %s with %s", d.Argv, gatedHead, forge.flag)
				}
				if n := countOf(d.Argv, gatedHead); n != 1 {
					t.Errorf("the head sha appears %d times in %v, want exactly once", n, d.Argv)
				}
			})
		}
	}
}

// The pin is the head the ladder judged and nothing else: a different head in the input is a different
// argv, and a caller cannot override it through the passthrough.
func TestDecideMerge_ThePinFollowsTheJudgedHeadAndCannotBePassedThrough(t *testing.T) {
	in := mergeableInput()
	in.State.HeadSHA = "0123456789abcdef0123456789abcdef01234567"
	d := DecideMerge(in)
	if !d.Allowed || countOf(d.Argv, in.State.HeadSHA) != 1 || countOf(d.Argv, gatedHead) != 0 {
		t.Fatalf("pin must follow the judged head: %+v", d)
	}
	in = mergeableInput()
	in.Req.Extra = []string{"--match-head-commit", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
	if d := DecideMerge(in); d.Allowed {
		t.Errorf("a passthrough --match-head-commit must be refused, not appended after the pin: %v", d.Argv)
	}
}

// A head that cannot be named in full cannot be pinned, and an unpinned merge is the defect. Not
// substitutable by --user-approved: a human vouches for review, not for which commits will land.
func TestDecideMerge_AHeadThatIsNotAFullShaIsRefused(t *testing.T) {
	for _, head := range []string{"", "abcdef1", "abcdef1234567890", gatedHead + "0", "zzzzzz1234567890abcdef1234567890abcdef12", " " + gatedHead[1:]} {
		for _, approved := range []bool{false, true} {
			in := mergeableInput()
			in.State.HeadSHA, in.UserApproved = head, approved
			d := DecideMerge(in)
			if d.Allowed || d.Argv != nil || !strings.Contains(d.Reason, "cannot be pinned") {
				t.Errorf("head %q (user-approved=%v): want a refusal saying it cannot be pinned, got %+v", head, approved, d)
			}
		}
	}
	for _, head := range []string{gatedHead, strings.ToUpper(gatedHead), gatedHead + gatedHead[:24]} {
		in := mergeableInput()
		in.State.HeadSHA = head
		if d := DecideMerge(in); !d.Allowed {
			t.Errorf("a full sha %q must be accepted: %s", head, d.Reason)
		}
	}
}

func indexOf(ss []string, x string) int {
	for i, s := range ss {
		if s == x {
			return i
		}
	}
	return -1
}

func countOf(ss []string, x string) int {
	n := 0
	for _, s := range ss {
		if s == x {
			n++
		}
	}
	return n
}
