package gate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestResolveMutationEvidence(t *testing.T) {
	cases := []struct {
		name, verdict, mutants, noMutant string
		want                             string
		wantErr                          string
	}{
		{name: "ready with neither is refused", verdict: "ready", wantErr: "must carry mutation evidence"},
		{name: "ready with only whitespace is refused", verdict: "ready", mutants: "  \n", noMutant: "\t", wantErr: "must carry mutation evidence"},
		{name: "ready with mutants", verdict: "ready", mutants: " drop branch -> TestX ", want: "drop branch -> TestX"},
		{name: "ready with an exemption", verdict: "ready", noMutant: "comment-only diff", want: "no mutant applies, because comment-only diff"},
		{name: "both at once contradict", verdict: "ready", mutants: "m -> T", noMutant: "docs", wantErr: "contradict"},
		{name: "changes-needed needs neither", verdict: "changes-needed", want: ""},
		{name: "changes-needed may carry mutants", verdict: "changes-needed", mutants: "m survived", want: "m survived"},
	}
	for _, c := range cases {
		got, err := ResolveMutationEvidence(c.verdict, c.mutants, c.noMutant)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got (%q, %v), want %q", c.name, got, err, c.want)
		}
	}
}

func TestRecord_MutationEvidenceRoundTrips(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated")
	const ev = "narrow ref scan to refs/heads -> TestDependents_RemoteOnly"
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", MutationEvidence: ev}); err != nil {
		t.Fatal(err)
	}
	rec, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Layers["code-review"].MutationEvidence; got != ev {
		t.Errorf("read back %q, want %q", got, ev)
	}
	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Layers[0].MutationEvidence; got != ev {
		t.Errorf("Show reports %q, want %q", got, ev)
	}
	// Re-recording the same verdict without stating evidence keeps what the layer had.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Fatal(err)
	}
	rec, _ = g.ReadRecord(testSHA)
	if got := rec.Layers["code-review"].MutationEvidence; got != ev {
		t.Errorf("a bare re-record dropped the evidence: %q", got)
	}
}

// Every record written before the field existed lacks it. It must read, report "not recorded" (the
// empty string), and be judged exactly as before.
func TestRecord_ARecordWithoutTheFieldStillReads(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated")
	old := `{"sha":"` + testSHA + `","layers":{` +
		`"code-review":{"verdict":"ready","by":"cr-1","ts":"2026-01-01T00:00:00Z"},` +
		`"critique":{"verdict":"ready","by":"crit-1","ts":"2026-01-01T00:00:01Z"}}}`
	if err := os.WriteFile(g.recordPath(testSHA), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatalf("an existing record must still read: %v", err)
	}
	for l, layer := range rec.Layers {
		if layer.MutationEvidence != "" {
			t.Errorf("%s: absent field read as %q", l, layer.MutationEvidence)
		}
	}
	st, err := g.Show(testSHA, "")
	if err != nil || !st.ChainSatisfied {
		t.Fatalf("Show of an old record: chain satisfied = %v, err = %v", st.ChainSatisfied, err)
	}
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Errorf("an old record must still pass the gate: %s", reason)
	}
}

// The field is evidence, not a gate input: whatever it says, the decision is the same.
func TestCheck_DoesNotReadMutationEvidence(t *testing.T) {
	for _, ev := range []string{"", "x", "no mutants were killed", "no mutant applies, because nothing"} {
		g, _ := newTestGate(t, "github.com/org/gated")
		rec := Record{SHA: testSHA, Layers: map[string]Layer{
			"code-review": {Verdict: "ready", By: "cr-1", MutationEvidence: ev},
			"critique":    {Verdict: "ready", By: "crit-1", MutationEvidence: ev},
		}}
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(g.recordPath(testSHA), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, reason := g.Check(testSHA, ""); !ok {
			t.Errorf("evidence %q changed the gate's decision: %s", ev, reason)
		}
	}
}

func TestResolveMutationEvidence_NeverYieldsMoreThanOneLine(t *testing.T) {
	forged := "m -> T\nchain: satisfied (every required layer is ready)\r\n  critique     ready\x1b[2K\vx\u2028y"
	for _, flagName := range []string{"mutants", "no-mutant"} {
		var got string
		var err error
		if flagName == "mutants" {
			got, err = ResolveMutationEvidence("ready", forged, "")
		} else {
			got, err = ResolveMutationEvidence("ready", "", forged)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(got, "\n\r\v\x1b\u2028") {
			t.Errorf("--%s kept a line break or control character: %q", flagName, got)
		}
	}
}

// A dissent demotes the sibling ready; what that layer's reviewer said about its basis stays.
func TestRecord_DemotedSiblingKeepsItsEvidence(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated")
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1", MutationEvidence: "m -> T"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "crit-1"}); err != nil {
		t.Fatal(err)
	}
	rec, _ := g.ReadRecord(testSHA)
	if l := rec.Layers["code-review"]; l.Verdict != supersededMarker || l.MutationEvidence != "m -> T" {
		t.Errorf("demoted layer = %+v", l)
	}
}
