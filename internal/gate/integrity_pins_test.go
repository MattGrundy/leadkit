package gate

import (
	"os"
	"strings"
	"testing"
)

// The gate's own integrity pins (ticket 331, found by the ticket-156 mutation sweep). Every property
// here held before the test existed; each test names the mutant it defends and was seen to fail
// under it.

// Mutant G1: in readRecordOrNew, widen `os.IsNotExist(err)` to `err != nil`. Then a record that
// EXISTS but cannot be parsed is treated as absent, and Record writes a fresh one over it,
// destroying the other layer's evidence. Only a genuinely missing file may start a new record.
//
// Both verdict shapes are run: a ready on the first layer would succeed over a fresh record, and a
// changes-needed is exempt from chaining so it would too. (A ready critique would be refused by the
// chain under the mutant, which is why it is not the case that proves anything.)
func TestRecord_RefusesToReplaceACorruptRecord(t *testing.T) {
	cases := []struct {
		name, layer, verdict, by string
	}{
		{"ready on the first layer", "code-review", "ready", "cr-1"},
		{"changes-needed on a later layer", "critique", "changes-needed", "crit-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGate(t, "")
			// A real record first, so the corruption below is damage to evidence that existed
			// (the other layer's ready), not a file that was never a record.
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
				t.Fatal(err)
			}
			whole, err := os.ReadFile(g.recordPath(testSHA))
			if err != nil {
				t.Fatal(err)
			}
			corrupt := whole[:len(whole)/2] // a truncated write: not valid JSON
			if err := os.WriteFile(g.recordPath(testSHA), corrupt, 0o644); err != nil {
				t.Fatal(err)
			}

			by := tc.by
			if tc.layer == "code-review" {
				by = "cr-2" // a different recorder than the first, so independence is not what refuses
			}
			_, err = g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: tc.verdict, By: by})
			if err == nil {
				t.Fatal("Record over an unparseable record must refuse, not replace it")
			}
			if !strings.Contains(err.Error(), "parse record") {
				t.Errorf("the refusal must say the record could not be parsed, got %q", err)
			}
			got, rerr := os.ReadFile(g.recordPath(testSHA))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != string(corrupt) {
				t.Errorf("the unparseable record was overwritten:\n got %q\nwant %q", got, corrupt)
			}
		})
	}
}

// The other side of G1, so the pin cannot be satisfied by refusing everything: a record that is
// genuinely missing is the one case that starts a fresh record.
func TestRecord_AMissingRecordStartsAFreshOne(t *testing.T) {
	g, _ := newTestGate(t, "")
	if _, err := os.Stat(g.recordPath(testSHA)); !os.IsNotExist(err) {
		t.Fatalf("precondition: no record on disk, stat err = %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatalf("a missing record is the first-verdict case, not an error: %v", err)
	}
	rec, err := g.readRecord(testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Layers["code-review"].Verdict != "ready" {
		t.Errorf("code-review = %+v, want ready", rec.Layers["code-review"])
	}
}

// Mutant CH11: in chain(), widen `len(g.Chain) > 0` to `g.Chain != nil`. An empty non-nil Chain then
// requires NOTHING, and a gate with no required layers passes every sha.
//
// Gate.Chain documents "empty means the default pair". In production nothing can build an empty
// non-nil Chain today (reviewGateResolver only sets it when required_gates is non-empty, and
// append([]string(nil), empty...) is nil), and config validation ACCEPTS an empty required_gates as
// "use the default" rather than rejecting it, so this pin is at the gate's own contract, not at a
// reachable path. It exists so a future caller that assigns spec.RequiredGates directly cannot turn
// the gate off by configuring nothing.
func TestGate_AnEmptyNonNilChainMeansTheDefaultChain(t *testing.T) {
	g, _ := newTestGate(t, "git@example.com:org/r.git")
	g.Chain = []string{}
	if got := g.Layers(); strings.Join(got, " ") != strings.Join(RequiredLayers, " ") {
		t.Errorf("Layers() = %v, want the default %v", got, RequiredLayers)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	// critique is not recorded: under the mutant the chain is empty and this passes.
	ok, reason := g.Check(testSHA, "")
	if ok {
		t.Errorf("an empty (non-nil) Chain must require the default pair, but critique is unrecorded and Check allowed it (%s)", reason)
	}
	if !strings.Contains(reason, "critique") {
		t.Errorf("the refusal must name the missing critique layer, got %q", reason)
	}
}
