package gate

import (
	"strings"
	"testing"
)

// Two reviewers of one role reach the same sha and both say ready. The second record must be refused,
// naming the verdict it would replace, and leave the record untouched: accepting it would re-stamp the
// first reviewer's By and timestamp, and nobody could then say whose verdict was on disk.
func TestRecord_ASecondReviewersIdenticalVerdictIsRefusedAndTheRecordUntouched(t *testing.T) {
	for _, tc := range []struct {
		name, layer, verdict, first, second string
		prepare                             func(t *testing.T, g *Gate)
	}{
		{"ready code-review by a peer of the same role", "code-review", "ready", "cr-1", "cr-2", nil},
		{"changes-needed code-review by a peer", "code-review", "changes-needed", "cr-1", "cr-2", nil},
		{"ready critique by a peer", "critique", "ready", "crit-1", "crit-2", func(t *testing.T, g *Gate) {
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGate(t, "")
			if tc.prepare != nil {
				tc.prepare(t, g)
			}
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: tc.verdict, By: tc.first}); err != nil {
				t.Fatal(err)
			}
			before := layerOf(t, g, tc.layer)

			_, err := g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: tc.verdict, By: tc.second})
			if err == nil {
				t.Fatalf("%s recording the same %s over %s's was accepted", tc.second, tc.verdict, tc.first)
			}
			for _, want := range []string{"REFUSED", tc.first, tc.second, tc.verdict} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
			if after := layerOf(t, g, tc.layer); after != before {
				t.Errorf("a refused record changed the layer: %+v -> %+v", before, after)
			}

			// Controls: the same recorder again is idempotent, and a blocking verdict from the peer is
			// still recordable, because blocking must never be refused.
			if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: tc.verdict, By: tc.first}); err != nil {
				t.Errorf("the original recorder re-recording: %v", err)
			}
			if tc.verdict == "ready" {
				if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: tc.layer, Verdict: "changes-needed", By: tc.second}); err != nil {
					t.Errorf("a peer's dissent over a ready must stay recordable: %v", err)
				}
				if l := layerOf(t, g, tc.layer); l.Verdict != "changes-needed" || l.SupersededBy != tc.first {
					t.Errorf("the dissent must stand and name whose ready it replaced: %+v", l)
				}
			}
		})
	}
}
