package review

import (
	"os"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// A dry run is the only verdict path that never reaches Record, so the independence refusal has to
// come from the pre-flight or it does not come at all: a plan that promises what the real run refuses.
func TestGateVerdict_DryRunRefusesWhatTheRealRunRefusesOnIndependence(t *testing.T) {
	rv, g, link, repo := setupVerdictFixture(t)
	g.Chain = []string{"code-review", "critique", "security"}
	g.ReviewRoles = []string{"security"}
	if _, err := g.Record(gate.RecordOpts{SHA: verdictTestSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	critLink := link + ".crit"
	if err := os.Rename(link, critLink); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ layer, by, want string }{
		{"critique", "cr-2", "same role"},
		{"critique", "cr-1", "same role"},
		{"security", "cr-3", "same role"},
	} {
		for _, dry := range []bool{true, false} {
			_, err := rv.GateVerdict(g, VerdictOpts{Ticket: critLink, Verdict: "ready", By: c.by, Layer: c.layer, DryRun: dry})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "gate-verdict: refuse") {
				t.Errorf("%s by %s (dry=%v): %v, want a gate-verdict refusal containing %q", c.layer, c.by, dry, err, c.want)
			}
		}
	}
}

// ...and with nothing recorded yet, the role family alone is enough to refuse.
func TestGateVerdict_DryRunRefusesAnEarlierLayersRoleBeforeAnythingIsRecorded(t *testing.T) {
	rv, g, link, _ := setupVerdictFixture(t)
	_, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-5", Layer: "critique", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "code-review-role agent") {
		t.Errorf("a code-review recorder on the critique layer: %v", err)
	}
}
