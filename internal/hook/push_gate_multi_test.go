package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// The raw-git push gate must judge EVERY push in a command line, not just the first: one
// verdict for the line, decided by its worst constituent. refPushEnv's feat/gated and
// feat/gated-b have passing records; feat/ungated has none.

// The separators lexShell splits on. push-guard shares parsePushCommands, so this table is
// also the pin that the two hooks agree on how many pushes a line carries.
var pushSeparators = []struct{ name, sep string }{
	{"and", " && "},
	{"semicolon", "; "},
	{"or", " || "},
	{"pipe", " | "},
	{"background", " & "},
	{"newline", "\n"},
	{"unspaced and", "&&"},
	{"unspaced semicolon", ";"},
}

func TestParsePushCommands_FindsEveryPushAcrossSeparators(t *testing.T) {
	for _, s := range pushSeparators {
		t.Run(s.name, func(t *testing.T) {
			p := parsePushCommands("git push origin feat/gated" + s.sep + "git push origin feat/ungated")
			inv := p.Invocations
			if p.State != pushFound || len(inv) != 2 {
				t.Fatalf("want 2 pushes, got state=%v %v", p.State, inv)
			}
			if got := strings.Join(inv[1].args, " "); got != "origin feat/ungated" {
				t.Errorf("second push args = %q", got)
			}
		})
	}
}

// The fail-open: a gated push FIRST, an ungated push SECOND. Judging only the first lets
// the whole line through.
func TestPushGate_GatedThenUngated_IsRefusedNamingTheUngatedOne(t *testing.T) {
	for _, s := range pushSeparators {
		t.Run(s.name, func(t *testing.T) {
			cfg, _ := refPushEnv(t, shaGated)
			cmd := "git push origin feat/gated" + s.sep + "git push origin feat/ungated"
			res := PushGate(bashInput(cmd), cfg)
			assertDeny(t, res, cmd)
			if !strings.Contains(res.Stdout, shaUngated[:12]) {
				t.Errorf("deny must name the ungated sha %s, got:\n%s", shaUngated[:12], res.Stdout)
			}
			if !strings.Contains(res.Stdout, "git push origin feat/ungated") {
				t.Errorf("deny must name the ungated push command, got:\n%s", res.Stdout)
			}
			if strings.Contains(res.Stdout, shaGated2[:12]) {
				t.Errorf("deny must not blame the gated push %s, got:\n%s", shaGated2[:12], res.Stdout)
			}
		})
	}
}

func TestPushGate_UngatedThenGated_IsRefused(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	res := PushGate(bashInput("git push origin feat/ungated && git push origin feat/gated"), cfg)
	assertDeny(t, res, "ungated first, gated second")
	if !strings.Contains(res.Stdout, shaUngated[:12]) {
		t.Errorf("deny must name the ungated sha, got:\n%s", res.Stdout)
	}
}

func TestPushGate_TwoGatedPushes_AreAllowed(t *testing.T) {
	for _, s := range pushSeparators {
		t.Run(s.name, func(t *testing.T) {
			cfg, _ := refPushEnv(t, shaUngated)
			cmd := "git push origin feat/gated" + s.sep + "git push origin feat/gated-b"
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
		})
	}
}

// A later push that cannot be named is refused, not skipped; so is a later bare push on an
// ungated HEAD.
func TestPushGate_LaterPushUndeterminedOrBare_IsRefused(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	res := PushGate(bashInput("git push origin feat/gated && git push origin no/such-branch"), cfg)
	assertDeny(t, res, "second push names an unresolvable ref")
	if !strings.Contains(res.Stdout, "could not determine") || !strings.Contains(res.Stdout, "no/such-branch") {
		t.Errorf("deny should say the second push could not be determined, got:\n%s", res.Stdout)
	}
	cfg, _ = refPushEnv(t, shaUngated)
	assertDeny(t, PushGate(bashInput("git push origin feat/gated && git push"), cfg), "second push is a bare push on an ungated HEAD")
	cfg, _ = refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("git push origin feat/gated && git push"), cfg), "second push is a bare push on a gated HEAD")
}

// Each push resolves its OWN target repo: a `cd <repo> &&` carry belongs to the push it
// precedes, so the repo is not the one the earlier push targeted.
func TestPushGate_EachPushResolvesItsOwnRepo(t *testing.T) {
	cfg, repo := refPushEnv(t, shaGated)
	var seen []string
	inner := cfg.ResolveGate
	cfg.ResolveGate = func(r string) *gate.Gate { seen = append(seen, r); return inner(r) }
	other := t.TempDir()
	cmd := "cd " + repo + " && git push origin feat/gated && cd " + other + " && git push origin feat/gated-b"
	assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
	if len(seen) != 2 || seen[0] != repo || seen[1] != other {
		t.Errorf("gate resolved for repos %v, want [%s %s]", seen, repo, other)
	}
}

// An inline REVIEW_GATE_BYPASS assignment sets the variable for the ONE command it prefixes
// — the first push. It must not whitewash a later push on the same line.
func TestPushGate_InlineBypassCoversOnlyTheFirstPush(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="hotfix" git push origin feat/ungated`), cfg),
		"bypass on a lone ungated push")
	cmd := `REVIEW_GATE_BYPASS="hotfix" git push origin feat/gated && git push origin feat/ungated`
	res := PushGate(bashInput(cmd), cfg)
	assertDeny(t, res, cmd)
	if !strings.Contains(res.Stdout, "git push origin feat/ungated") {
		t.Errorf("deny must name the un-bypassed push, got:\n%s", res.Stdout)
	}
	assertDeny(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="hotfix" true && git push origin feat/ungated`), cfg),
		"bypass assigned to an earlier, non-push command")
}
