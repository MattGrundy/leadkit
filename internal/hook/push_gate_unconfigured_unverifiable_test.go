package hook

import (
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// An unverifiable verdict (the gate could not confirm the repo's scope, e.g. its origin cannot be
// read) is not "no profile governs", and the unconfigured seam is not asked about it: advisory
// allows it silently as it always did, enforce refuses it, and neither reports the state.
func TestPushGateUnconfigured_NotAnAnswerForAnUnverifiableVerdict(t *testing.T) {
	repo := t.TempDir()
	g := &gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{}, head: gateTestSHA}, // no readable origin
		EnforcedRemotes: []string{enforcedSubstr},
	}
	var asked []string
	for _, mode := range []GuardMode{ModeAdvisory, ModeEnforce} {
		cfg := PushGateConfig{
			ResolveGate:  staticGate(g),
			Cwd:          repo,
			Mode:         func(string) GuardMode { return mode },
			Unconfigured: func(r string) string { asked = append(asked, r); return unconfiguredNoteText },
		}
		res := dispatchPush(t, cfg, "git push")
		if mode == ModeEnforce {
			assertDeny(t, res, "enforce, unverifiable")
		} else {
			assertAllow(t, res, "advisory, unverifiable")
		}
	}
	if len(asked) != 0 {
		t.Errorf("the unconfigured seam was asked about an unverifiable verdict: %v", asked)
	}
}
