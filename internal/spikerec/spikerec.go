// Package spikerec is `leadkit spike record|show` — a spike/dispatch run's
// verified result, recorded durably and keyed to the Issue/Task it verifies
// (docs/HIERARCHY-AND-SYNC.md's uniform state layer). Was a spike green on sha X,
// what did it prove — on disk, not only in a session transcript.
//
// Append-only: Show never mutates. Record is a normal op (like `task create`), not
// confirm-guarded — but every test/dogfood run MUST target a temp log, never the
// real one (same discipline as taskcreate/issuecreate's own safety notes).
package spikerec

import (
	"fmt"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/recordlog"
)

// FormatRecords renders records as `spike show`'s human-readable report — one line
// per record, oldest first (Show/ReadAll already preserve on-disk append order).
func FormatRecords(records []Record) string {
	var b strings.Builder
	for _, r := range records {
		fmt.Fprintf(&b, "%s  ref=%s  verdict=%-5s", r.TS, r.Ref, r.Verdict)
		if r.Issue != "" {
			fmt.Fprintf(&b, "  issue=%s", r.Issue)
		}
		if r.Verified != "" {
			fmt.Fprintf(&b, "  verified=%q", r.Verified)
		}
		if r.Link != "" {
			fmt.Fprintf(&b, "  link=%s", r.Link)
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "  note=%q", r.Note)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d record(s)\n", len(records))
	return b.String()
}

// Verdict values Record accepts.
const (
	VerdictGreen = "green"
	VerdictRed   = "red"
)

// Record is one spike/dispatch run's recorded result.
type Record struct {
	TS       string `json:"ts"` // RFC3339, UTC
	Ref      string `json:"ref"`
	Verdict  string `json:"verdict"` // "green" | "red"
	Issue    string `json:"issue,omitempty"`
	Verified string `json:"verified,omitempty"`
	Link     string `json:"link,omitempty"`
	Note     string `json:"note,omitempty"`
}

// nowFunc is the clock Append's ts uses — injectable so tests can assert an exact
// timestamp.
var nowFunc = time.Now

// Append validates and appends one spike result to logPath.
func Append(logPath, ref, verdict, issue, verified, link, note string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("spikerec: --ref is required")
	}
	if verdict != VerdictGreen && verdict != VerdictRed {
		return fmt.Errorf("spikerec: --verdict must be %q or %q, got %q", VerdictGreen, VerdictRed, verdict)
	}
	rec := Record{
		TS: nowFunc().UTC().Format(time.RFC3339), Ref: ref, Verdict: verdict,
		Issue: issue, Verified: verified, Link: link, Note: note,
	}
	return recordlog.Append(logPath, rec)
}

// Show reads logPath, filtered by issue and/or ref (exact match; empty = no
// filter). A missing log returns an empty slice, not an error.
func Show(logPath, issueFilter, refFilter string) ([]Record, error) {
	all, err := recordlog.ReadAll[Record](logPath)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range all {
		if issueFilter != "" && r.Issue != issueFilter {
			continue
		}
		if refFilter != "" && r.Ref != refFilter {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
