package prcreate

import (
	"strings"
	"testing"
)

// The control for every refusal below: the same request with no passthrough IS allowed, so the
// refusals are about the passthrough and not some other part of the decision.
func decisions(extra []string) map[string]Decision {
	e := allowedEdit()
	e.Req.Extra = extra
	m := mergeableInput()
	m.Req.Extra = extra
	return map[string]Decision{"edit": DecideEdit(e), "merge": DecideMerge(m)}
}

func createDecision(extra []string) Decision {
	in := allowedInput()
	in.Req.Extra = extra
	return Decide(in)
}

func TestPassthrough_ControlNoExtraIsAllowedOnEveryVerb(t *testing.T) {
	for verb, d := range decisions(nil) {
		if !d.Allowed {
			t.Errorf("control: pr %s with no passthrough must be allowed, got: %s", verb, d.Reason)
		}
	}
	if d := createDecision(nil); !d.Allowed {
		t.Errorf("control: pr create with no passthrough must be allowed, got: %s", d.Reason)
	}
}

// Each of these reaches a different part of what a trailing flag can change, and each must be
// refused on every verb with no argv produced.
func TestPassthrough_EverythingIsRefusedOnEveryVerb(t *testing.T) {
	for _, extra := range [][]string{
		{"-R", "victim/other"},        // a second repo: the last --repo wins, so the call lands outside the governed repo
		{"--repo", "victim/other"},    // the same, long form
		{"--repo=victim/other"},       // and the = form
		{"--admin"},                   // merge past branch protection
		{"--attach", "/etc/passwd"},   // the edit verb uploads a local file
		{"--add-reviewer", "someone"}, // scope creep
		{"--add-label=x"},
		{"--body", "replaced"}, // overrides the governed --body-file
		{"--base", "main"},     // a retarget outside the narrowing check
		{"-B", "main"},
		// pflag also takes the value attached to a shorthand, so a denylist of spellings is never complete:
		// -Bmain parses as base=main and was a retarget with no ancestry check.
		{"-Bmain"},
		{"-B=main"},
		{"-Rvictim/other"},
		{"-tnew-title"},
		{"-bnew-body"},
		{"-Fnew-body.md"},
		{"-mV1"},
		{"--target-branch", "main"},
		{"--merge"}, // overrides the composed squash
		{"--delete-branch"},
		{"99"}, // a bare word: a second positional the forge CLI may interpret
		{"--"},
		{"--some-flag-the-forge-cli-adds-next-year"},
		{"--title", "ok", "--admin"}, // an innocent flag does not launder a dangerous one after it
	} {
		all := decisions(extra)
		all["create"] = createDecision(extra)
		for verb, d := range all {
			if d.Allowed {
				t.Errorf("pr %s must refuse passthrough %v", verb, extra)
				continue
			}
			if !strings.Contains(d.Reason, "allowlist") {
				t.Errorf("pr %s refusing %v must say why (allowlist): %q", verb, extra, d.Reason)
			}
			if len(d.Argv) != 0 {
				t.Errorf("pr %s refusing %v still produced an argv: %v", verb, extra, d.Argv)
			}
		}
	}
}

func TestPassthrough_ARefusedPassthroughNeverReachesTheForge(t *testing.T) {
	fr := &fakeRunner{}
	in := allowedEdit()
	in.Req.Extra = []string{"-R", "victim/other"}
	if d, _, _ := ExecuteEdit(in, false, fr); d.Allowed || len(fr.calls) != 0 {
		t.Fatalf("a smuggled -R must be refused and the forge never called: allowed=%v calls=%v", d.Allowed, fr.calls)
	}
	m := mergeableInput()
	m.Req.Extra = []string{"--admin"}
	if d, _, _ := ExecuteMerge(m, false, fr); d.Allowed || len(fr.calls) != 0 {
		t.Fatalf("--admin must be refused and the forge never called: allowed=%v calls=%v", d.Allowed, fr.calls)
	}
}

// The authority check still comes first: an ungoverned repo is told so, not told about its flags.
func TestPassthrough_GovernanceStillSpeaksFirst(t *testing.T) {
	in := allowedEdit()
	in.HasAuthority = false
	in.Req.Extra = []string{"--admin"}
	if d := DecideEdit(in); d.Allowed || !strings.Contains(d.Reason, "no authority") {
		t.Errorf("an ungoverned repo must get the authority refusal: %q", d.Reason)
	}
}

// Tripwire: the allowlist is empty on purpose. Admitting a flag is a decision, and it needs a test
// showing the governance decision still holds with the flag present, so this fails until one exists.
func TestPassthrough_TheAllowlistIsEmptyUntilAFlagEarnsItsPlace(t *testing.T) {
	for verb, flags := range allowedPassthrough {
		if len(flags) != 0 {
			t.Errorf("pr %s allows %v: add a test that the repo, base and merge options still cannot be overridden with it present, then update this assertion", verb, flags)
		}
	}
	for _, verb := range []string{"create", "edit", "merge"} {
		if _, ok := allowedPassthrough[verb]; !ok {
			t.Errorf("verb %q has no allowlist entry; refusePassthrough would refuse everything by accident", verb)
		}
	}
}

func TestRefusePassthrough_JudgesTheFlagNameNotTheValue(t *testing.T) {
	allowedPassthrough["probe"] = map[string]bool{"--ok": true}
	defer delete(allowedPassthrough, "probe")
	if r := refusePassthrough("probe", []string{"--ok", "--ok=1"}); r != "" {
		t.Errorf("an allowed flag, bare or with =value, must pass: %q", r)
	}
	for _, bad := range [][]string{{"--ok", "positional"}, {"--ok", "--nope"}, {"--nope=--ok"}, {"ok"}} {
		if refusePassthrough("probe", bad) == "" {
			t.Errorf("%v must be refused", bad)
		}
	}
}
