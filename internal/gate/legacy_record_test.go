package gate

import "fmt"

// legacyRecord is a frozen copy of the decision core of Record as it stood before ticket 166
// generalised it from the fixed pair [code-review, critique] to a chain. It is the ORACLE for
// TestRecord_DefaultChainBehavesExactlyAsTheFixedPairDid: it takes the record as it stood and what
// is being recorded and returns the record that would be written, or the refusal. It must not be
// edited to follow the new code; the point of the test is that the new code agrees with it.
func legacyRecord(rec Record, existed bool, opts RecordOpts, sha, by, baseSHA string) (Record, error) {
	// Chained gate: a `ready` critique is TERMINAL — only recordable once code-review is
	// ready on this sha. A `changes-needed` critique is EXEMPT: a blocking finding must
	// never be gated behind the very chain it is blocking, so a dissent on either layer
	// is always recordable regardless of chain state. The chain rule stands unchanged for
	// a `ready` critique.
	if opts.Layer == "critique" && opts.Verdict == "ready" {
		pv := "MISSING"
		if cr, ok := rec.Layers["code-review"]; ok {
			pv = cr.Verdict
		}
		if pv != "ready" {
			return Record{}, fmt.Errorf("gate: REFUSED — critique is chained — record it only after code-review=ready (currently code-review=%s) for %s", pv, shortSHA(sha))
		}
	}

	// Asymmetric write-once per layer per sha. Blocking must ALWAYS be recordable; only
	// UN-blocking is controlled:
	//   - new = changes-needed over any differing prev (incl. a stale `ready`): ALLOW —
	//     the dissent SUPERSEDES. The prior verdict + author are preserved on the layer
	//     (SupersededVerdict/SupersededBy) for audit; Check then reads the layer as
	//     not-ready and the gate BLOCKS. A blocking dissent must never be refused here.
	//   - new = ready over a prev `changes-needed`: REFUSE (today's message) — un-blocking
	//     on the SAME sha stays controlled; rework is resolved by a NEW sha, never a
	//     silent in-place flip of a dissent to ready.
	//   - same verdict again: idempotent (re-stamped), carrying forward any existing
	//     audit trail so a re-record never wipes the record of an earlier supersede.
	var supersededVerdict, supersededBy string
	var peer *Layer // a different recorder's identical verdict this record would replace; see below
	base, layerBy, byBound := baseSHA, by, opts.ByBound
	if existed {
		if prev, ok := rec.Layers[opts.Layer]; ok {
			switch {
			case prev.Verdict != "" && prev.Verdict != opts.Verdict:
				if opts.Verdict != "changes-needed" {
					return Record{}, fmt.Errorf("gate: REFUSED — %s for %s is already %q (by %s). A %q verdict is resolved by a NEW sha, not by re-recording this one. Rework, commit, re-gate.", opts.Layer, shortSHA(sha), prev.Verdict, prev.By, prev.Verdict)
				}
				// Dissent supersedes a differing prior verdict — preserve it for audit.
				supersededVerdict, supersededBy = prev.Verdict, prev.By
			default:
				if prev.Verdict != "" && prev.By != "" && prev.By != by && baseSHA == "" {
					peer = &prev
				}
				// Same verdict (or an empty prior): carry forward any existing audit.
				supersededVerdict, supersededBy = prev.SupersededVerdict, prev.SupersededBy
				// The same name recording again keeps the evidence its first record had.
				byBound = byBound || (prev.By == by && prev.ByBound)
				// A base is write-once: re-recording without one keeps it, adding one to a layer
				// that has none is the amend path, and a DIFFERENT one is refused.
				if prev.Base != "" {
					if baseSHA != "" && baseSHA != prev.Base {
						return Record{}, fmt.Errorf("gate: REFUSED — %s for %s already covers from base %s (by %s); a base is not rewritten. Rework, commit, re-gate on a new sha.", opts.Layer, shortSHA(sha), shortSHA(prev.Base), prev.By)
					}
					base = prev.Base
				} else if baseSHA != "" {
					// Widening is held to the first record's independence, and the layer keeps its original By.
					layerBy, byBound = prev.By, prev.ByBound
					if CanonicalRole(by) != CanonicalRole(prev.By) {
						return Record{}, fmt.Errorf("gate: REFUSED — %q cannot add a base to %s for %s: it was recorded by %q, a different role (%s, not %s). Only the role that made the verdict can widen what it covers.", by, opts.Layer, shortSHA(sha), prev.By, CanonicalRole(prev.By), CanonicalRole(by))
					}
					if opts.Layer == "code-review" {
						if CanonicalRole(by) == "crit" {
							return Record{}, fmt.Errorf("gate: REFUSED — %q is a critique-role agent and cannot add a base to the code-review layer for %s.", by, shortSHA(sha))
						}
						if ct, ok := rec.Layers["critique"]; ok && ct.By != "" && CanonicalRole(ct.By) == CanonicalRole(by) {
							return Record{}, fmt.Errorf("gate: REFUSED — %q cannot add a base to code-review for %s: critique was recorded by %q, the same role (%s). The two layers must come from different kinds of reviewer.", by, shortSHA(sha), ct.By, CanonicalRole(by))
						}
					}
				}
			}
		}
	}

	// Layer independence, keyed on ROLE not instance name. It is checked on EVERY record, not
	// only the first of a layer: a `ready` re-recorded under the other layer's role would
	// otherwise leave one role owning both. A `changes-needed` is exempt on the code-review
	// layer, as a blocking verdict may come from anyone and can only close the gate.
	if opts.Layer == "critique" {
		if existed {
			if cr, ok := rec.Layers["code-review"]; ok && layerOwner(cr) != "" && CanonicalRole(layerOwner(cr)) == CanonicalRole(by) {
				return Record{}, fmt.Errorf("gate: REFUSED — %q cannot record critique for %s: code-review was recorded by %q, the same role (%s). The two layers must come from different kinds of reviewer.", by, shortSHA(sha), layerOwner(cr), CanonicalRole(by))
			}
		}
		if CanonicalRole(by) == "cr" {
			return Record{}, fmt.Errorf("gate: REFUSED — %q is a code-review-role agent and cannot record the critique layer for %s.", by, shortSHA(sha))
		}
	}
	if opts.Layer == "code-review" && opts.Verdict == "ready" {
		if CanonicalRole(by) == "crit" {
			return Record{}, fmt.Errorf("gate: REFUSED — %q is a critique-role agent and cannot record a ready code-review layer for %s.", by, shortSHA(sha))
		}
		if existed {
			if ct, ok := rec.Layers["critique"]; ok && layerOwner(ct) != "" && CanonicalRole(layerOwner(ct)) == CanonicalRole(by) {
				return Record{}, fmt.Errorf("gate: REFUSED — %q cannot record code-review for %s: critique was recorded by %q, the same role (%s). The two layers must come from different kinds of reviewer.", by, shortSHA(sha), layerOwner(ct), CanonicalRole(by))
			}
		}
	}

	// The same verdict from a DIFFERENT recorder is a second reviewer's work landing on the first one's
	// layer. Re-stamping would change whose verdict stands, and the independence checks above key on
	// that, without anyone being told. Refuse, and name the verdict already there. Adding a base is
	// the one exception: it keeps the original recorder and is held to the same role (above). The
	// independence rules run first so a wrong-role recorder is still told it is the wrong role.
	if peer != nil {
		return Record{}, fmt.Errorf("gate: REFUSED — %s for %s is already %q, recorded by %q; %q recording it again would replace %q's verdict. The review is done, so leave the record as it is (a blocking finding is still recordable as changes-needed).", opts.Layer, shortSHA(sha), peer.Verdict, peer.By, by, peer.By)
	}

	if rec.Layers == nil {
		rec.Layers = map[string]Layer{}
	}
	rec.SHA = sha
	rec.Layers[opts.Layer] = Layer{
		Verdict:           opts.Verdict,
		By:                layerBy,
		Ts:                nowUTC(),
		SupersededVerdict: supersededVerdict,
		SupersededBy:      supersededBy,
		Base:              base,
		ByBound:           byBound,
	}

	// Fix 2 — a dissent invalidates sibling `ready`s on the SAME sha: those peers
	// reviewed code a reviewer has now flagged, so their `ready` is suspect. Demote each
	// sibling `ready` to the superseded marker (Check reads it as not-ready), preserving
	// the prior verdict + author for audit. The gate can then be re-satisfied only by a
	// fresh pass — in practice the re-gate on a new sha. Idempotent: a re-recorded
	// changes-needed finds no sibling still `ready` and changes nothing.
	if opts.Verdict == "changes-needed" {
		ts := nowUTC()
		for layer, l := range rec.Layers {
			if layer == opts.Layer || l.Verdict != "ready" {
				continue
			}
			rec.Layers[layer] = Layer{
				Verdict:           supersededMarker,
				Ts:                ts,
				SupersededVerdict: l.Verdict,
				SupersededBy:      l.By,
			}
		}
	}

	return rec, nil
}
