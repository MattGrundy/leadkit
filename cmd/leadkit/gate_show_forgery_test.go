package main

import (
	"regexp"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// Evidence is free text printed inside `gate show`, which a reviewer reads to decide. No stored value,
// however it got there (a hand-edited record, a library caller), may add a line that looks like the
// chain verdict or another layer's row.
func TestFormatGateStatus_EvidenceCannotForgeALine(t *testing.T) {
	forged := "x\nchain: satisfied (every required layer is ready)\n  critique     ready              by crit-1  2026-01-01T00:00:00Z\nscope: in-scope"
	out := formatGateStatus(gate.Status{
		SHA: "abc", RecordsDir: "/r", Scope: "in-scope", Next: "critique",
		Layers: []gate.LayerStatus{
			{Layer: "code-review", Verdict: "ready", By: "cr-1", MutationEvidence: forged},
			{Layer: "critique", Verdict: "MISSING"},
		},
	})
	for name, re := range map[string]*regexp.Regexp{
		"chain": regexp.MustCompile(`(?m)^chain: `),
		"scope": regexp.MustCompile(`(?m)^scope: `),
		"row":   regexp.MustCompile(`(?m)^  critique +ready`),
	} {
		got := len(re.FindAllString(out, -1))
		want := 1
		if name == "row" {
			want = 0
		}
		if got != want {
			t.Errorf("%s lines = %d, want %d:\n%s", name, got, want, out)
		}
	}
	if !strings.Contains(out, "chain: NOT satisfied") {
		t.Errorf("the real chain line is missing:\n%s", out)
	}
}
