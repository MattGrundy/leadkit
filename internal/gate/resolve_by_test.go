package gate

import (
	"strings"
	"testing"
)

func TestResolveBy(t *testing.T) {
	for _, c := range []struct {
		name, asked, identity string
		wantBy                string
		wantBound             bool
		wantErr               string
	}{
		{"nothing at all keeps the historical default", "", "", "claude", false, ""},
		{"no identity: the flag is self-asserted", "cr-owner-5", "", "cr-owner-5", false, ""},
		{"identity, no flag: the identity is the name", "", "cr-owner-5", "cr-owner-5", true, ""},
		{"identity and the same flag", "cr-owner-5", "cr-owner-5", "cr-owner-5", true, ""},
		{"surrounding whitespace is not a different name", " cr-owner-5 ", "cr-owner-5\n", "cr-owner-5", true, ""},
		{"identity and a different flag is refused", "crit-owner-1", "cr-owner-5", "", false, "cr-owner-5"},
		{"claiming to be the sibling role is the case that matters", "crit-1", "cr-1", "", false, "crit-1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			by, bound, err := ResolveBy(c.asked, c.identity)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "REFUSED") {
					t.Fatalf("want a refusal naming %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil || by != c.wantBy || bound != c.wantBound {
				t.Errorf("ResolveBy(%q, %q) = %q, %v, %v; want %q, %v", c.asked, c.identity, by, bound, err, c.wantBy, c.wantBound)
			}
		})
	}
}

// ByBound is the record's only evidence of how a name was obtained, so it must be written when the
// recorder says so, kept when the same name records again, and not forged by a different recorder
// widening a layer it does not own.
func TestRecord_ByBoundIsStoredAndCarriedHonestly(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", ByBound: true}); err != nil {
		t.Fatal(err)
	}
	if !layerOf(t, g, "code-review").ByBound {
		t.Fatal("a bound record did not store by_bound")
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	if layerOf(t, g, "critique").ByBound {
		t.Error("a self-asserted record claims to be bound")
	}

	// The same name recording again without the evidence does not erase what the first record had.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	if !layerOf(t, g, "code-review").ByBound {
		t.Error("re-recording under the same name dropped the evidence")
	}

	// A peer of the same role adding a base keeps the ORIGINAL recorder's name and its evidence, not
	// its own: a bound peer must not launder an unbound layer, nor an unbound one strip a bound layer.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-2", ByBound: true, Base: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g, "critique"); l.By != "crit-1" || l.ByBound {
		t.Errorf("widening by a peer rewrote attribution or evidence: %+v", l)
	}
}

// The evidence belongs to the name it was gathered for. A bound layer widened by a same-role peer that is
// itself unbound keeps both the original name and the evidence, and a DIFFERENT name re-recording the same
// verdict over a bound layer gets none of it: a self-asserted name must never read as identity-checked.
func TestRecord_ByBoundStaysWithTheNameItWasGatheredFor(t *testing.T) {
	g := baseGate(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", ByBound: true}); err != nil {
		t.Fatal(err)
	}
	// widening by an unbound peer of the same role
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2", Base: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g, "code-review"); l.By != "cr-1" || !l.ByBound || l.Base != testBase {
		t.Errorf("an unbound peer widening a bound layer must leave its name and evidence alone: %+v", l)
	}

	// a different name recording the same verdict over a bound layer is refused (a second recorder
	// does not replace the first), and the layer keeps its name and its evidence
	g2 := baseGate(t)
	if _, err := g2.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", ByBound: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := g2.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2"}); err == nil {
		t.Fatal("a different recorder's identical verdict replaced the first reviewer's")
	}
	if l := layerOf(t, g2, "code-review"); l.By != "cr-1" || !l.ByBound {
		t.Errorf("a refused record must leave the bound layer alone: %+v", l)
	}
	// a ready entry that names nobody (a hand-edited or old record) is not a first reviewer's verdict to
	// protect, so a name may take it, but it carries none of the entry's evidence
	g4 := baseGate(t)
	if err := g4.writeRecord(testSHA, Record{SHA: testSHA, Layers: map[string]Layer{"code-review": {Verdict: "ready", ByBound: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g4.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2"}); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g4, "code-review"); l.By != "cr-2" || l.ByBound {
		t.Errorf("a name taking an anonymous entry must not inherit its evidence: %+v", l)
	}
	// and a layer whose prior entry carries no verdict is replaced outright, evidence included.
	g3 := baseGate(t)
	if err := g3.writeRecord(testSHA, Record{SHA: testSHA, Layers: map[string]Layer{"code-review": {By: "cr-1", ByBound: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g3.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-2"}); err != nil {
		t.Fatal(err)
	}
	if l := layerOf(t, g3, "code-review"); l.By != "cr-2" || l.ByBound {
		t.Errorf("a different name must not inherit another name's evidence: %+v", l)
	}
}
