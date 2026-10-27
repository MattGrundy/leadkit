package gate

import (
	"reflect"
	"strings"
	"testing"
)

// threeLayerGate is a repo whose grant requires [code-review, critique, security], with security a
// review role the project declared.
func threeLayerGate(t *testing.T) *Gate {
	t.Helper()
	g, _ := newTestGate(t, "github.com/org/gated")
	g.Chain = []string{"code-review", "critique", "security"}
	g.ReviewRoles = []string{"security"}
	return g
}

func rec(t *testing.T, g *Gate, layer, verdict, by string) error {
	t.Helper()
	_, err := g.Record(RecordOpts{SHA: testSHA, Layer: layer, Verdict: verdict, By: by})
	return err
}

func TestVocabularyAndValidateChain(t *testing.T) {
	if got := Vocabulary(nil); !reflect.DeepEqual(got, []string{"code-review", "critique"}) {
		t.Errorf("the built-in vocabulary = %v", got)
	}
	if got := Vocabulary([]string{"security", "critique", "", "security"}); !reflect.DeepEqual(got, []string{"code-review", "critique", "security"}) {
		t.Errorf("extras are appended once, built-ins not repeated, blanks dropped: %v", got)
	}
	vocab := Vocabulary([]string{"security"})
	for _, ok := range [][]string{nil, {"code-review"}, {"security", "code-review"}, {"code-review", "critique", "security"}} {
		if err := ValidateChain(ok, vocab); err != nil {
			t.Errorf("ValidateChain(%v): %v", ok, err)
		}
	}
	for names, want := range map[string]string{
		"code-review,nope":          `"nope" is not a review layer`,
		"code-review,code-review":   "listed twice",
		"security-agent":            "valid layers: code-review critique security",
		"code-review,,critique":     "empty layer name",
		"code-review,  ,critique":   "empty layer name",
		"outcome-verifier,critique": `"outcome-verifier" is not a review layer`,
	} {
		err := ValidateChain(strings.Split(names, ","), vocab)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateChain(%q) = %v, want it to contain %q", names, err, want)
		}
	}
}

func TestMissing(t *testing.T) {
	r := Record{Layers: map[string]Layer{
		"code-review": {Verdict: "ready"},
		"critique":    {Verdict: supersededMarker},
	}}
	if got := Missing(r, []string{"code-review", "critique", "security"}); !reflect.DeepEqual(got, []string{"critique=" + supersededMarker, "security=MISSING"}) {
		t.Errorf("Missing = %v", got)
	}
	if got := Missing(r, []string{"code-review"}); got != nil {
		t.Errorf("a satisfied chain has nothing missing, got %v", got)
	}
	if got := Missing(Record{}, nil); got != nil {
		t.Errorf("an empty chain is trivially satisfied, got %v", got)
	}
}

// A layer the project declared is recordable end to end, and one it did not is refused with the
// layers that exist named. This is the unsatisfiable-gate trap of ticket 166: before it, a third layer
// could be required but never recorded.
func TestRecord_ADeclaredReviewRoleIsRecordableAndAnUnknownOneIsRefused(t *testing.T) {
	g := threeLayerGate(t)
	for _, step := range []struct{ layer, by string }{{"code-review", "cr-1"}, {"critique", "crit-1"}, {"security", "sec-1"}} {
		if err := rec(t, g, step.layer, "ready", step.by); err != nil {
			t.Fatalf("%s by %s: %v", step.layer, step.by, err)
		}
	}
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Fatalf("all three layers ready must satisfy the three-layer gate: %s", reason)
	}

	err := rec(t, g, "outcome", "ready", "o-1")
	if err == nil || !strings.Contains(err.Error(), "unknown layer") || !strings.Contains(err.Error(), "code-review critique security") {
		t.Errorf("an undeclared layer must be refused naming the valid ones: %v", err)
	}
	// Without the declaration the same name is unknown: the roster is the only thing that makes it a layer.
	plain, _ := newTestGate(t, "github.com/org/gated")
	if err := rec(t, plain, "security", "ready", "sec-1"); err == nil || !strings.Contains(err.Error(), "unknown layer") {
		t.Errorf("security without a review_layer declaration: %v", err)
	}
}

func TestRecord_ListOrderIsChainOrder(t *testing.T) {
	g := threeLayerGate(t)
	for _, c := range []struct{ layer, by, wantAfter string }{
		{"critique", "crit-1", "code-review"},
		{"security", "sec-1", "code-review"}, // the FIRST layer missing is named
	} {
		err := rec(t, g, c.layer, "ready", c.by)
		if err == nil || !strings.Contains(err.Error(), "chained") || !strings.Contains(err.Error(), "only after "+c.wantAfter+"=ready") {
			t.Errorf("%s ready before its predecessors: %v", c.layer, err)
		}
	}
	if err := rec(t, g, "code-review", "ready", "cr-1"); err != nil {
		t.Fatal(err)
	}
	if err := rec(t, g, "security", "ready", "sec-1"); err == nil || !strings.Contains(err.Error(), "only after critique=ready") {
		t.Errorf("security ready with critique still missing: %v", err)
	}
	// A blocking finding is never gated behind the chain it blocks, on any layer.
	if err := rec(t, g, "security", "changes-needed", "sec-1"); err != nil {
		t.Errorf("a dissent on the last layer must be recordable at any point: %v", err)
	}
	// The order is the list's, not the vocabulary's: reversed, security comes first.
	rev, _ := newTestGate(t, "github.com/org/gated")
	rev.Chain = []string{"security", "code-review"}
	rev.ReviewRoles = []string{"security"}
	if err := rec(t, rev, "code-review", "ready", "cr-1"); err == nil || !strings.Contains(err.Error(), "only after security=ready") {
		t.Errorf("code-review after security in the list: %v", err)
	}
	if err := rec(t, rev, "security", "ready", "sec-1"); err != nil {
		t.Errorf("security first: %v", err)
	}
	if err := rec(t, rev, "code-review", "ready", "cr-1"); err != nil {
		t.Errorf("code-review once security is ready: %v", err)
	}
}

// A layer the repo does not require is valid but unordered: nothing waits on it and it waits on nothing.
func TestRecord_ALayerOutsideTheChainIsUnordered(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated")
	g.Chain = []string{"code-review"}
	if err := rec(t, g, "critique", "ready", "crit-1"); err != nil {
		t.Errorf("critique is not in this repo's chain, so it does not wait for code-review: %v", err)
	}
	if ok, _ := g.Check(testSHA, ""); ok {
		t.Error("the chain [code-review] is not satisfied by a critique")
	}
	if err := rec(t, g, "code-review", "ready", "cr-1"); err != nil {
		t.Fatal(err)
	}
	if ok, reason := g.Check(testSHA, ""); !ok {
		t.Errorf("code-review alone satisfies [code-review]: %s", reason)
	}
}

// A layer the chain does not list is unordered, but it is held to the same independence from every layer
// that does gate, on every verdict: it is placed AFTER them, so a recorder of any gating layer's role is
// refused on it even for a changes-needed. Placed first it would owe nothing to anyone and a code-review
// agent could record the security layer.
func TestRecord_AnUnrequiredLayerIsHeldIndependentFromTheLayersThatGate(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated") // the default chain: [code-review, critique]
	g.ReviewRoles = []string{"security"}           // declared, so a valid layer, and not required
	for _, c := range []struct{ by, verdict, want string }{
		{"cr-1", "changes-needed", "code-review-role agent"},
		{"crit-1", "changes-needed", "critique-role agent"},
		{"cr-1", "ready", "code-review-role agent"},
	} {
		if err := rec(t, g, "security", c.verdict, c.by); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s recording security %s: %v, want a refusal naming the %s", c.by, c.verdict, err, c.want)
		}
		if err := g.CheckIndependence(Record{}, false, "security", c.verdict, c.by, testSHA); err == nil {
			t.Errorf("CheckIndependence (gate-verdict's pre-flight) let %s record security %s", c.by, c.verdict)
		}
	}
	if err := rec(t, g, "security", "changes-needed", "security-agent-1"); err != nil {
		t.Errorf("the layer's own role may record it: %v", err)
	}
}

// No two layers of a chain may share a role, for every pair and not only code-review and critique.
func TestRecord_NoTwoLayersOfAChainShareARole(t *testing.T) {
	g := threeLayerGate(t)
	if err := rec(t, g, "code-review", "ready", "cr-1"); err != nil {
		t.Fatal(err)
	}
	if err := rec(t, g, "critique", "ready", "crit-1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ layer, by, want string }{
		{"security", "cr-2", "same role"},              // the code-review role
		{"security", "crit-2", "same role"},            // the critique role
		{"security", "code-review-agent", "same role"}, // its spelling as an agent
		{"security", "cr", "same role"},                // the bare family
	} {
		if err := rec(t, g, c.layer, "ready", c.by); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s recorded by %s: %v", c.layer, c.by, err)
		}
	}
	// ...and the other way: the security role cannot take an earlier layer, with or without a record.
	g2 := threeLayerGate(t)
	for _, by := range []string{"security-agent", "security-agent-2", "security"} {
		err := rec(t, g2, "code-review", "ready", by)
		if err == nil || !strings.Contains(err.Error(), "security-role agent") {
			t.Errorf("code-review ready by %s: %v", by, err)
		}
	}
	// A dissent from a later layer's role stays recordable on an earlier layer: it can only close the gate.
	if err := rec(t, g2, "code-review", "changes-needed", "security-agent-1"); err != nil {
		t.Errorf("a later layer's role dissenting on an earlier layer: %v", err)
	}
	// An earlier layer's role may not record a later layer at all, not even to dissent.
	g3 := threeLayerGate(t)
	if err := rec(t, g3, "security", "changes-needed", "cr-1"); err == nil || !strings.Contains(err.Error(), "code-review-role agent") {
		t.Errorf("code-review's role dissenting on security: %v", err)
	}
	// A layer demoted by a sibling's dissent keeps its owner, so independence does not lapse with it.
	g4 := threeLayerGate(t)
	for _, s := range []struct{ layer, verdict, by string }{
		{"code-review", "ready", "auditor-1"},
		{"critique", "ready", "crit-1"},
		{"critique", "changes-needed", "crit-1"}, // demotes code-review's ready
	} {
		if err := rec(t, g4, s.layer, s.verdict, s.by); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec(t, g4, "security", "changes-needed", "auditor-2"); err == nil || !strings.Contains(err.Error(), "same role") {
		t.Errorf("the role that owned the demoted code-review must not record security: %v", err)
	}
}

// The pre-flight gate-verdict runs and Record cannot disagree, because both ask the same function.
func TestCheckIndependenceAgreesWithRecord(t *testing.T) {
	g := threeLayerGate(t)
	if err := rec(t, g, "code-review", "ready", "cr-1"); err != nil {
		t.Fatal(err)
	}
	r, err := g.ReadRecord(testSHA)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ layer, verdict, by string }{
		{"critique", "ready", "cr-2"}, {"critique", "ready", "crit-1"}, {"security", "ready", "cr-9"},
		{"security", "ready", "sec-1"}, {"code-review", "ready", "security-agent"}, {"code-review", "changes-needed", "security-agent"},
	} {
		pre := g.CheckIndependence(r, true, c.layer, c.verdict, c.by, testSHA)
		_, recErr := g.decide(r, true, RecordOpts{Layer: c.layer, Verdict: c.verdict, By: c.by}, testSHA, c.by, "")
		independence := recErr != nil && !strings.Contains(recErr.Error(), "chained") && !strings.Contains(recErr.Error(), "already")
		if (pre != nil) != independence {
			t.Errorf("%v: pre-flight %v, Record %v", c, pre, recErr)
		}
	}
}

func TestShowAndCheckFollowTheChain(t *testing.T) {
	g := threeLayerGate(t)
	if err := rec(t, g, "code-review", "ready", "cr-1"); err != nil {
		t.Fatal(err)
	}
	st, err := g.Show(testSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range st.Layers {
		names = append(names, l.Layer)
	}
	if !reflect.DeepEqual(names, []string{"code-review", "critique", "security"}) || st.Next != "critique" || st.ChainSatisfied {
		t.Errorf("Show = layers %v next %q satisfied %v", names, st.Next, st.ChainSatisfied)
	}
	ok, reason := g.Check(testSHA, "")
	if ok || !strings.Contains(reason, "critique=MISSING") || !strings.Contains(reason, "security=MISSING") {
		t.Errorf("Check = %v %q, want both missing layers named", ok, reason)
	}
	// The default gate is unchanged by any of it.
	def, _ := newTestGate(t, "github.com/org/gated")
	if !reflect.DeepEqual(def.Layers(), RequiredLayers) {
		t.Errorf("a gate with no chain requires %v", def.Layers())
	}
}

// The chain rule is "every layer before it", not "the layer just before it". A record in which the
// middle layer is ready and the first is not (a stale or hand-edited record) must still hold the last
// layer back, naming the first one that is missing.
func TestRecord_AReadyWaitsForEveryEarlierLayerNotJustTheAdjacentOne(t *testing.T) {
	g := threeLayerGate(t)
	if err := g.writeRecord(testSHA, Record{SHA: testSHA, Layers: map[string]Layer{
		"critique": {Verdict: "ready", By: "crit-1"},
	}}); err != nil {
		t.Fatal(err)
	}
	err := rec(t, g, "security", "ready", "sec-1")
	if err == nil || !strings.Contains(err.Error(), "only after code-review=ready (currently code-review=MISSING)") {
		t.Errorf("security with code-review missing and critique ready: %v", err)
	}
}

// Only a declared review role extends the vocabulary. A chain that names a layer nobody declared does not
// make it recordable: the resolver validates a chain against the vocabulary before it sets one, and
// the gate must not widen the vocabulary to fit whatever chain it is handed.
func TestVocabularyIsBuiltInsPlusDeclaredRolesAndNeverTheChain(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/gated")
	g.Chain = []string{"security"}
	if got := g.Vocabulary(); !reflect.DeepEqual(got, []string{"code-review", "critique"}) {
		t.Errorf("Vocabulary = %v, want only the built-in layers", got)
	}
	if err := rec(t, g, "security", "ready", "sec-1"); err == nil || !strings.Contains(err.Error(), "unknown layer") {
		t.Errorf("a chain naming an undeclared layer made it recordable: %v", err)
	}
	g.ReviewRoles = []string{"security"}
	if got := g.Vocabulary(); !reflect.DeepEqual(got, []string{"code-review", "critique", "security"}) {
		t.Errorf("Vocabulary = %v", got)
	}
}
