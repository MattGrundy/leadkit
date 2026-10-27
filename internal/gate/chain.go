package gate

import (
	"fmt"
	"strings"
)

// The layer vocabulary and the chain.
//
// A LAYER is a review role's verdict on a sha, and its name is the role's name: the built-in review
// roles are code-review and critique, and a project can declare more (a roster entry with
// review_layer: true). A role is review-capable because the project SAYS it is, in one place, the
// roster; nothing infers it from a name prefix. The CHAIN is the ordered list of layers a repo
// requires (authority.<repo>.required_gates; RequiredLayers when unset), and list order is chain
// order: a layer can be recorded ready only once every layer before it is ready.
//
// For the default chain [code-review, critique] every rule below reduces to what the fixed pair
// always did; TestRecord_DefaultChainBehavesExactlyAsTheFixedPairDid holds the two together.

// Vocabulary is every layer name a record may carry: the built-in review layers followed by the
// extra review roles, each once, in that order.
func Vocabulary(extra []string) []string {
	out := append([]string(nil), RequiredLayers...)
	for _, r := range extra {
		if r != "" && !containsStr(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// ValidateChain says whether names is a usable chain under vocab: non-empty names, each in vocab,
// none repeated. The error names the valid layers, so a config that asks for one that does not exist
// is told what does.
func ValidateChain(names, vocab []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		switch {
		case n == "" || strings.TrimSpace(n) == "":
			return fmt.Errorf("an empty layer name (valid layers: %s)", strings.Join(vocab, " "))
		case !containsStr(vocab, n):
			return fmt.Errorf("%q is not a review layer (valid layers: %s; a project adds one by declaring a roster role with review_layer: true)", n, strings.Join(vocab, " "))
		case seen[n]:
			return fmt.Errorf("layer %q is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// Missing lists the layers of chain that are not ready in rec, each as "layer=verdict" (MISSING when
// the layer has no entry), in chain order. It is the one place "is this record complete" is decided:
// Check, the PR gate, the lineage check, the orchestrator and verdict's dry-run all ask it.
func Missing(rec Record, chain []string) []string {
	var missing []string
	for _, layer := range chain {
		v := "MISSING"
		if l, ok := rec.Layers[layer]; ok {
			v = l.Verdict
		}
		if v != "ready" {
			missing = append(missing, layer+"="+v)
		}
	}
	return missing
}

// Chain is the ordered layers this gate requires: Gate.Chain when the repo's grant names one, else
// RequiredLayers.
func (g *Gate) chain() []string {
	if len(g.Chain) > 0 {
		return g.Chain
	}
	return RequiredLayers
}

// Layers is the chain this gate requires, for callers that report or route by it.
func (g *Gate) Layers() []string { return g.chain() }

// Vocabulary is every layer name this gate accepts on a record.
func (g *Gate) Vocabulary() []string {
	return Vocabulary(g.ReviewRoles)
}

// ValidateChain checks a required_gates list against this gate's vocabulary.
func (g *Gate) ValidateChain(names []string) error {
	return ValidateChain(names, g.Vocabulary())
}

// isLayerRole reports whether role (a CanonicalRole) is the role family that owns layer: code-review's
// is cr, critique's crit, and a layer named for a project role (security) is owned by that role's
// agents, named either security-N or security-agent-N.
func isLayerRole(role, layer string) bool {
	return role == CanonicalRole(layer) || role == CanonicalRole(layer+"-agent")
}

// order is the chain with layer placed in it. idx is layer's position. A layer the chain does not
// list (a valid layer this repo does not require) is placed after all of them and inChain is false:
// it is held to the same independence from every layer that does gate, but it is not ordered, because
// nothing waits on it and it waits on nothing.
func (g *Gate) order(layer string) (order []string, idx int, inChain bool) {
	order = append([]string(nil), g.chain()...)
	for i, l := range order {
		if l == layer {
			return order, i, true
		}
	}
	return append(order, layer), len(order), false
}

// chained refuses a ready verdict on a layer whose predecessors in the chain are not all ready. A
// changes-needed is exempt: a blocking finding must never be gated behind the chain it is blocking.
func chained(rec Record, order []string, idx int, inChain bool, layer, verdict, sha string) error {
	if verdict != "ready" || !inChain {
		return nil
	}
	for _, prev := range order[:idx] {
		pv := "MISSING"
		if l, ok := rec.Layers[prev]; ok {
			pv = l.Verdict
		}
		if pv != "ready" {
			return fmt.Errorf("gate: REFUSED — %s is chained — record it only after %s=ready (currently %s=%s) for %s", layer, prev, prev, pv, shortSHA(sha))
		}
	}
	return nil
}

// independent refuses a recorder whose role is one of the other layers'. No two layers of a chain
// may share a role (CanonicalRole of the recorded name; a layer demoted by a dissent still counts as
// owned by who recorded it, see layerOwner). The rule is asymmetric because blocking must stay
// possible for anyone: a layer BEFORE this one in the chain forbids its role family and its
// recorders' roles for every verdict, and a layer AFTER it does so for a ready only, since a dissent
// can only close the gate. Without a record on disk (existed false) no layer has an owner, but the
// role-family checks still apply. Adding a base to a layer already recorded is a ready record by the
// same role, so it passes through this check like any other.
func independent(rec Record, existed bool, order []string, idx int, layer, verdict, by, sha string) error {
	role := CanonicalRole(by)
	sameRole := func(other string) error {
		if !existed {
			return nil
		}
		l, ok := rec.Layers[other]
		if !ok {
			return nil
		}
		owner := layerOwner(l)
		if owner == "" || CanonicalRole(owner) != role {
			return nil
		}
		return fmt.Errorf("gate: REFUSED — %q cannot record %s for %s: %s was recorded by %q, the same role (%s). The two layers must come from different kinds of reviewer.", by, layer, shortSHA(sha), other, owner, role)
	}
	for _, other := range order[:idx] {
		if err := sameRole(other); err != nil {
			return err
		}
		if isLayerRole(role, other) {
			return fmt.Errorf("gate: REFUSED — %q is a %s-role agent and cannot record the %s layer for %s.", by, other, layer, shortSHA(sha))
		}
	}
	if verdict != "ready" {
		return nil
	}
	for _, other := range order[idx+1:] {
		if isLayerRole(role, other) {
			return fmt.Errorf("gate: REFUSED — %q is a %s-role agent and cannot record a ready %s layer for %s.", by, other, layer, shortSHA(sha))
		}
		if err := sameRole(other); err != nil {
			return err
		}
	}
	return nil
}

// CheckIndependence is the pre-flight form of the independence rule: whether by may record verdict on
// layer given rec, the record as it stands (existed false when none is on disk). gate-verdict calls
// it before it touches anything, so that and Record cannot disagree about who may record what.
func (g *Gate) CheckIndependence(rec Record, existed bool, layer, verdict, by, sha string) error {
	order, idx, _ := g.order(layer)
	return independent(rec, existed, order, idx, layer, verdict, by, sha)
}
