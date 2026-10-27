// Package decisionrec is `leadkit decision record|list` — a human-in-the-loop
// decision, recorded durably (docs/HIERARCHY-AND-SYNC.md's uniform state layer).
// The audit trail AND the "don't re-litigate a settled decision" memory: a decision
// the user already made (PVC wipe approved, brain promotion authorised, merge-to-
// master is user-only) is ON RECORD, so the lead/orchestrator read it instead of
// re-deriving or re-asking.
//
// Append-only: List never mutates. Record is a normal op, not confirm-guarded — but
// every test/dogfood run MUST target a temp log, never the real one (same
// discipline as spikerec/taskcreate's own safety notes).
package decisionrec

import (
	"fmt"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/recordlog"
)

// FormatRecords renders records as `decision list`'s human-readable report — one
// line per record, oldest first (List/ReadAll already preserve on-disk append
// order).
func FormatRecords(records []Record) string {
	var b strings.Builder
	for _, r := range records {
		fmt.Fprintf(&b, "%s  by=%-12s  what=%q", r.TS, r.By, r.What)
		if r.Authorizes != "" {
			fmt.Fprintf(&b, "  authorizes=%q", r.Authorizes)
		}
		if r.Ref != "" {
			fmt.Fprintf(&b, "  ref=%s", r.Ref)
		}
		if r.Scope != "" {
			fmt.Fprintf(&b, "  scope=%s", r.Scope)
		}
		if r.Context != "" {
			fmt.Fprintf(&b, "  context=%q", r.Context)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d record(s)\n", len(records))
	return b.String()
}

// Record is one HiL decision.
type Record struct {
	TS         string `json:"ts"` // RFC3339, UTC
	What       string `json:"what"`
	By         string `json:"by"`
	Authorizes string `json:"authorizes,omitempty"`
	Context    string `json:"context,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// nowFunc is the clock Append's ts uses — injectable so tests can assert an exact
// timestamp.
var nowFunc = time.Now

// Append validates and appends one decision to logPath. --what and --by are
// required — a decision with no content or no attributed human isn't a record of
// anything.
func Append(logPath, what, by, authorizes, context, ref, scope string) error {
	if strings.TrimSpace(what) == "" {
		return fmt.Errorf("decisionrec: --what is required")
	}
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("decisionrec: --by is required")
	}
	rec := Record{
		TS: nowFunc().UTC().Format(time.RFC3339), What: what, By: by,
		Authorizes: authorizes, Context: context, Ref: ref, Scope: scope,
	}
	return recordlog.Append(logPath, rec)
}

// List reads logPath, filtered by by/ref (exact match) and/or since (a decision's
// ts must be on or after since — RFC3339 or a bare "2006-01-02" date are both
// accepted). Empty filters match everything. A missing log returns an empty slice,
// not an error.
func List(logPath, byFilter, refFilter, sinceFilter string) ([]Record, error) {
	all, err := recordlog.ReadAll[Record](logPath)
	if err != nil {
		return nil, err
	}
	var since time.Time
	if sinceFilter != "" {
		var perr error
		since, perr = parseSince(sinceFilter)
		if perr != nil {
			return nil, fmt.Errorf("decisionrec: --since %q: %w", sinceFilter, perr)
		}
	}

	var out []Record
	for _, r := range all {
		if byFilter != "" && r.By != byFilter {
			continue
		}
		if refFilter != "" && r.Ref != refFilter {
			continue
		}
		if sinceFilter != "" {
			ts, err := time.Parse(time.RFC3339, r.TS)
			if err != nil || ts.Before(since) {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// parseSince accepts either a full RFC3339 timestamp or a bare "2006-01-02" date
// (midnight UTC) — the friendlier form for a `--since` flag typed by hand.
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
