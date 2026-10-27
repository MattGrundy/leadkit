package gate

import (
	"fmt"
	"reflect"
	"testing"
)

// equivDepth is how many records long the walk goes. 36 distinct records per step makes this a
// little under two million decisions, a few seconds.
const equivDepth = 4

const equivBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type equivStep struct {
	layer, verdict, by string
	base               bool
}

func (s equivStep) String() string {
	b := ""
	if s.base {
		b = "+base"
	}
	return fmt.Sprintf("%s/%s/%s%s", s.layer, s.verdict, s.by, b)
}

func equivSteps() []equivStep {
	var out []equivStep
	for _, layer := range []string{"code-review", "critique"} {
		for _, verdict := range []string{"ready", "changes-needed"} {
			for _, by := range []string{"cr-1", "cr-2", "crit-1", "crit-2", "aud-1", "aud-2"} {
				for _, base := range []bool{false, true} {
					if base && verdict != "ready" {
						continue // a base qualifies a ready only; Record refuses it before the decision core
					}
					out = append(out, equivStep{layer, verdict, by, base})
				}
			}
		}
	}
	return out
}

func cloneRecord(r Record) Record {
	c := Record{SHA: r.SHA}
	if r.Layers != nil {
		c.Layers = make(map[string]Layer, len(r.Layers))
		for k, v := range r.Layers {
			c.Layers[k] = v
		}
	}
	return c
}

// stripTs removes the clock, the only thing two runs of the same decision can differ in.
func stripTs(r Record) Record {
	c := cloneRecord(r)
	for k, v := range c.Layers {
		v.Ts = ""
		c.Layers[k] = v
	}
	return c
}

// walkEquivalence feeds every sequence of up to depth records, drawn from equivSteps, through the
// frozen pre-166 logic and the current decision core. It returns how many decisions it compared and
// a description of the first place they differ ("" when none does): one refuses what the other
// accepts, or they accept and produce different records.
func walkEquivalence(g *Gate, depth int) (checked int, diff string) {
	steps := equivSteps()
	var walk func(prefix []equivStep, rec Record, existed bool) bool
	walk = func(prefix []equivStep, rec Record, existed bool) bool {
		if len(prefix) == depth {
			return true
		}
		for _, st := range steps {
			opts := RecordOpts{SHA: testSHA, Layer: st.layer, Verdict: st.verdict, By: st.by}
			baseSHA := ""
			if st.base {
				opts.Base = "origin/main"
				baseSHA = equivBase
			}
			old, oldErr := legacyRecord(cloneRecord(rec), existed, opts, testSHA, st.by, baseSHA)
			got, gotErr := g.decide(cloneRecord(rec), existed, opts, testSHA, st.by, baseSHA)
			checked++
			seq := append(append([]equivStep(nil), prefix...), st)
			if (oldErr == nil) != (gotErr == nil) {
				diff = fmt.Sprintf("sequence %v: the fixed pair gave %v, the chain gave %v", seq, oldErr, gotErr)
				return false
			}
			if oldErr == nil && !reflect.DeepEqual(stripTs(old), stripTs(got)) {
				diff = fmt.Sprintf("sequence %v: records differ\n fixed pair: %+v\n chain:      %+v", seq, stripTs(old), stripTs(got))
				return false
			}
			next, nextExisted := rec, existed
			if oldErr == nil {
				next, nextExisted = old, true
			}
			// a refusal leaves the record as it was; keep walking from there too
			if !walk(seq, next, nextExisted) {
				return false
			}
		}
		return true
	}
	walk(nil, Record{}, false)
	return checked, diff
}

// The safety property of ticket 166: with the default chain [code-review, critique] the generalised
// Record decides exactly as the fixed pair does. Every sequence of up to equivDepth records over
// both layers, both verdicts, a base or none, and recorders of the code-review role, the critique
// role and a role that is neither, is accepted and refused identically and leaves identical records.
func TestRecord_DefaultChainBehavesExactlyAsTheFixedPairDid(t *testing.T) {
	n, diff := walkEquivalence(&Gate{}, equivDepth)
	if diff != "" {
		t.Fatal(diff)
	}
	if n < 1000000 {
		t.Fatalf("only %d decisions compared; the walk is not covering the space it claims to", n)
	}
	// Naming the default chain explicitly changes nothing.
	if _, diff := walkEquivalence(&Gate{Chain: []string{"code-review", "critique"}}, 2); diff != "" {
		t.Fatal(diff)
	}
}

// The comparison must be able to see a difference at all: an oracle and a subject that cannot
// disagree make the property above vacuous. Chains that are not the fixed pair must be caught.
func TestRecord_EquivalenceWalkNoticesADifferingChain(t *testing.T) {
	for _, chain := range [][]string{
		{"critique"},                // critique does not wait for code-review
		{"critique", "code-review"}, // the order reversed
	} {
		if _, diff := walkEquivalence(&Gate{Chain: chain}, 2); diff == "" {
			t.Errorf("chain %v was not distinguished from the fixed pair", chain)
		}
	}
}
