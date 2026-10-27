package audit

import (
	"strings"
	"testing"
)

func TestFormatReport_SummaryLineCounts(t *testing.T) {
	findings := []Finding{
		{Category: CategoryOrphanedActive, Severity: SeverityWarn, ID: "1", Detail: "d1"},
		{Category: CategoryOrphanedActive, Severity: SeverityWarn, ID: "2", Detail: "d2"},
		{Category: CategoryDanglingSymlink, Severity: SeverityWarn, ID: "3.md", Detail: "d3"},
		{Category: CategoryDuplicateID, Severity: SeverityInfo, ID: "4", Detail: "d4"},
	}
	out := FormatReport(findings, nil)

	want := "audit: 4 findings — 2 orphaned-active, 1 dangling, 0 drift, 1 dup-id, 0 noncanonical, 0 session-misuse"
	if !strings.Contains(out, want) {
		t.Errorf("report summary missing/wrong.\ngot:\n%s\nwant substring:\n%s", out, want)
	}
	for _, id := range []string{"1", "2", "3.md", "4"} {
		if !strings.Contains(out, id) {
			t.Errorf("report is missing finding id %q:\n%s", id, out)
		}
	}
}

func TestFormatReport_EmptyIsStillWellFormed(t *testing.T) {
	out := FormatReport(nil, nil)
	if !strings.Contains(out, "audit: 0 findings") {
		t.Errorf("empty report = %q, want it to say 0 findings", out)
	}
}

func TestFormatReport_IncludesSessionCounts(t *testing.T) {
	out := FormatReport(nil, map[string]SessionCounts{
		"session-abc": {Total: 5, Gate: 2, Misuse: 1},
	})
	if !strings.Contains(out, "session-abc") || !strings.Contains(out, "total=5") {
		t.Errorf("report missing session counts:\n%s", out)
	}
}
