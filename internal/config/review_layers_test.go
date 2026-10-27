package config

import (
	"strings"
	"testing"
)

func TestReviewRoles(t *testing.T) {
	p := &Profile{Roster: []RosterSpec{
		{Type: "developer", Count: 1},
		{Type: "security", Count: 1, ReviewLayer: true},
		{Type: "security", Count: 2, ReviewLayer: true}, // listed twice: still one layer
		{Type: "", ReviewLayer: true},
		{Type: "perf", ReviewLayer: true},
	}}
	if got := p.ReviewRoles(); strings.Join(got, ",") != "security,perf" {
		t.Errorf("ReviewRoles = %v, want [security perf]: only flagged roles, once each, in roster order", got)
	}
	if got := (&Profile{}).ReviewRoles(); got != nil {
		t.Errorf("no roster declares no review roles, got %v", got)
	}
}

// required_gates names layers, and a layer is only real if the project has it. Validate names what is
// wrong and what exists, so a gate nothing can satisfy is reported at the profile, not discovered as a
// refusal at the PR.
func TestValidate_RequiredGatesMustNameReviewLayers(t *testing.T) {
	auth := func(gates ...string) map[string]AuthoritySpec {
		return map[string]AuthoritySpec{"acme/widgets": {RequiredGates: gates}}
	}
	find := func(ws []string, sub string) bool {
		for _, w := range ws {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}

	if ws := (&Profile{Authority: auth("code-review", "critique")}).Validate(); find(ws, "required_gates") {
		t.Errorf("the built-in pair is valid: %v", ws)
	}
	if ws := (&Profile{Authority: auth()}).Validate(); find(ws, "required_gates") {
		t.Errorf("no required_gates is valid: %v", ws)
	}
	declared := &Profile{Roster: []RosterSpec{{Type: "security", ReviewLayer: true}}, Authority: auth("code-review", "critique", "security")}
	if ws := declared.Validate(); find(ws, "required_gates") {
		t.Errorf("a roster role declared review_layer is a valid layer: %v", ws)
	}

	ws := (&Profile{Authority: auth("code-review", "security")}).Validate()
	if !find(ws, `authority.acme/widgets.required_gates: "security" is not a review layer`) || !find(ws, "valid layers: code-review critique") {
		t.Errorf("an undeclared layer must be reported with the valid ones named: %v", ws)
	}
	// Declared on the roster but NOT flagged: a work role is not a review layer.
	unflagged := &Profile{Roster: []RosterSpec{{Type: "security"}}, Authority: auth("security")}
	if ws := unflagged.Validate(); !find(ws, `"security" is not a review layer`) {
		t.Errorf("a roster role that is not review_layer is not a layer: %v", ws)
	}
	if ws := (&Profile{Authority: auth("critique", "critique")}).Validate(); !find(ws, "listed twice") {
		t.Errorf("a repeated layer must be reported: %v", ws)
	}
	// finalise and orchestrator never record a verdict.
	for _, fixed := range []string{"finalise", "orchestrator"} {
		ws := (&Profile{Roster: []RosterSpec{{Type: fixed, ReviewLayer: true}}}).Validate()
		if !find(ws, fixed+" cannot be a review_layer") {
			t.Errorf("%s as a review layer must be reported: %v", fixed, ws)
		}
	}
}
