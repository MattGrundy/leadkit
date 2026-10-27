package gate

import (
	"strings"
	"testing"
)

// Ticket 150. Role independence was checked on the first record of a layer and never again, so a
// ready layer could be re-recorded under the other layer's role and one role end up owning both.

func bothReady(t *testing.T) *Gate {
	t.Helper()
	g, _ := newTestGate(t, "")
	for _, r := range []RecordOpts{
		{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"},
		{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func TestRecord_ReadyCodeReviewUnderTheCritiqueRoleIsRefused(t *testing.T) {
	g := bothReady(t)
	before := layerOf(t, g, "code-review")
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "crit-1"})
	if err == nil || !strings.Contains(err.Error(), "critique") {
		t.Fatalf("the critique role re-recording code-review must be refused, got %v", err)
	}
	if after := layerOf(t, g, "code-review"); after != before {
		t.Errorf("a refused re-record changed the layer: %+v -> %+v", before, after)
	}
	// Another instance of the critique role, and a fresh sha's FIRST record, are refused alike.
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "crit-7"}); err == nil {
		t.Error("any critique-role instance must be refused")
	}
	g2, _ := newTestGate(t, "")
	if _, err := g2.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "critique-agent"}); err == nil {
		t.Error("a critique-role agent must not make the first code-review record either")
	}
}

// Independence follows the ROLE the critique layer was recorded under, whatever it is named.
func TestRecord_ReadyCodeReviewUnderTheRoleThatRecordedCritiqueIsRefused(t *testing.T) {
	g, _ := newTestGate(t, "")
	for _, r := range []RecordOpts{
		{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"},
		{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "auditor-1"},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "auditor-2"}); err == nil || !strings.Contains(err.Error(), "same role") {
		t.Fatalf("the role that recorded critique must be refused code-review, got %v", err)
	}
}

// The check outlives a dissent: a sibling demoted by changes-needed keeps no By, only SupersededBy,
// and the role that owned it still owns it.
func TestRecord_IndependenceSurvivesADemotedSibling(t *testing.T) {
	g, _ := newTestGate(t, "")
	for _, r := range []RecordOpts{
		{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "auditor-1"},
		{SHA: testSHA, Layer: "critique", Verdict: "ready", By: "crit-1"},
		// the critique dissents, which demotes code-review's ready to a superseded marker
		{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "crit-1"},
	} {
		if _, err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	if cr := layerOf(t, g, "code-review"); cr.By != "" || cr.SupersededBy != "auditor-1" {
		t.Fatalf("fixture: want code-review demoted with SupersededBy auditor-1, got %+v", cr)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "critique", Verdict: "changes-needed", By: "auditor-2"}); err == nil || !strings.Contains(err.Error(), "same role") {
		t.Errorf("the role that owned the demoted code-review layer must not record critique, got %v", err)
	}
}

// What must keep working: the same reviewer re-recording its own verdict, and a blocking verdict from
// anyone (including the sibling role). A different instance of the role that owns the layer is NOT in
// this list: its identical verdict would replace the first reviewer's (ticket 222).
func TestRecord_LegitimateReRecordsStillWork(t *testing.T) {
	g := bothReady(t)
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "ready", By: "cr-1"}); err != nil {
		t.Errorf("a reviewer re-recording its own ready layer: %v", err)
	}
	if _, err := g.Record(RecordOpts{SHA: testSHA, Layer: "code-review", Verdict: "changes-needed", By: "crit-1"}); err != nil {
		t.Errorf("a blocking code-review verdict is recordable by anyone, including the critique role: %v", err)
	}
	if got := layerOf(t, g, "code-review").Verdict; got != "changes-needed" {
		t.Errorf("the dissent must stand, got %q", got)
	}
}
