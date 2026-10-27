package review

import "testing"

// The verdict path hands the recorder's evidence to the gate: a bound name must land on the layer.
func TestGateVerdict_StoresWhetherTheNameWasBoundToAnIdentity(t *testing.T) {
	for _, bound := range []bool{true, false} {
		rv, g, link, _ := setupVerdictFixture(t)
		if _, err := rv.GateVerdict(g, VerdictOpts{Ticket: link, Verdict: "ready", By: "cr-owner-5", ByBound: bound}); err != nil {
			t.Fatal(err)
		}
		rec, err := g.ReadRecord(verdictTestSHA)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.Layers["code-review"].ByBound; got != bound {
			t.Errorf("ByBound = %v on the layer, want %v", got, bound)
		}
	}
}
