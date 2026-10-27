package decisionrec

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// FormatRecords is what a human reads to avoid re-litigating a settled decision, so the
// properties that matter are: every record appears, order is preserved, optional fields
// appear only when set, a value survives intact however awkward, and the count is the
// real count.
//
// These tests read the report back through parseReport rather than looking for fragments
// of it, so they fail when the report stops meaning the right thing and not when someone
// rewords it. parseReport's own contract is small and is the one thing a format change
// has to preserve or consciously update: a line is a bare timestamp followed by key=value
// fields, a value is either a bare token or a Go-quoted string, and a quoted string may
// contain anything, including spaces, '=' and other keys.

type reportLine struct {
	ts     string
	fields map[string]string
}

// parseReport splits a FormatRecords report into one parsed line per record plus the
// trailing count, which it reads as the first integer on the last non-empty line.
func parseReport(t *testing.T, out string) (lines []reportLine, count int) {
	t.Helper()
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := ""
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.TrimSpace(rows[i]) != "" {
			last = rows[i]
			rows = rows[:i]
			break
		}
	}
	n, err := strconv.Atoi(regexp.MustCompile(`\d+`).FindString(last))
	if err != nil {
		t.Fatalf("the report must end with a numeric count, last line %q", last)
	}
	for _, row := range rows {
		if strings.TrimSpace(row) == "" {
			continue
		}
		lines = append(lines, parseLine(t, row))
	}
	return lines, n
}

func parseLine(t *testing.T, row string) reportLine {
	t.Helper()
	l := reportLine{fields: map[string]string{}}
	rest := row
	skip := func() { rest = strings.TrimLeftFunc(rest, unicode.IsSpace) }
	for skip(); rest != ""; skip() {
		end := strings.IndexFunc(rest, func(r rune) bool { return r == '=' || unicode.IsSpace(r) })
		if end < 0 {
			end = len(rest)
		}
		word := rest[:end]
		if end == len(rest) || rest[end] != '=' {
			if l.ts != "" {
				t.Fatalf("unexpected bare token %q after the timestamp in %q", word, row)
			}
			l.ts, rest = word, rest[end:]
			continue
		}
		rest = rest[end+1:]
		var val string
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				t.Fatalf("field %q has an unterminated quoted value in %q", word, row)
			}
			val, _ = strconv.Unquote(q)
			rest = rest[len(q):]
		} else {
			e := strings.IndexFunc(rest, unicode.IsSpace)
			if e < 0 {
				e = len(rest)
			}
			val, rest = rest[:e], rest[e:]
		}
		if _, dup := l.fields[word]; dup {
			t.Fatalf("field %q appears twice in %q: a value leaked across a field boundary", word, row)
		}
		l.fields[word] = val
	}
	return l
}

func TestFormatRecords_OptionalFieldsOnlyWhenSet(t *testing.T) {
	minimal := Record{TS: "2026-01-02T03:04:05Z", By: "matt", What: "wipe approved"}
	full := Record{
		TS: "2026-01-03T00:00:00Z", By: "matt", What: "promotion authorised",
		Authorizes: "brain-promote", Ref: "task-41", Scope: "project-x",
		Context: "agreed on the call",
	}

	lines, _ := parseReport(t, FormatRecords([]Record{minimal}))
	if len(lines) != 1 {
		t.Fatalf("want one line, got %d", len(lines))
	}
	want := map[string]string{"by": "matt", "what": "wipe approved"}
	if !reflect.DeepEqual(lines[0].fields, want) {
		t.Errorf("a minimal record must carry only its required fields: got %v, want %v", lines[0].fields, want)
	}
	if lines[0].ts != minimal.TS {
		t.Errorf("timestamp = %q, want %q", lines[0].ts, minimal.TS)
	}

	lines, _ = parseReport(t, FormatRecords([]Record{full}))
	want = map[string]string{
		"by": "matt", "what": "promotion authorised", "authorizes": "brain-promote",
		"ref": "task-41", "scope": "project-x", "context": "agreed on the call",
	}
	if !reflect.DeepEqual(lines[0].fields, want) {
		t.Errorf("a full record must carry every field: got %v, want %v", lines[0].fields, want)
	}
}

// What a decision says must come back exactly as written, whatever it contains. If a value
// can bleed into the next field, a reader — and anything parsing the report — cannot tell
// where `what` ends. The awkward values are the ones that would: spaces, quotes, '=', and
// text that looks like another field.
func TestFormatRecords_ValuesSurviveARoundTrip(t *testing.T) {
	for _, v := range []string{
		"merge to master is user-only",
		`approved "the wipe" on the call`,
		"set ref=oops and scope=elsewhere",
		`backslash \ and tab	and unicode é`,
		"trailing space ",
	} {
		rec := Record{TS: "t", By: "matt", What: v, Context: v, Authorizes: v, Ref: "r", Scope: "s"}
		lines, count := parseReport(t, FormatRecords([]Record{rec}))
		if len(lines) != 1 || count != 1 {
			t.Fatalf("%q: want one line and a count of 1, got %d lines, count %d", v, len(lines), count)
		}
		for _, field := range []string{"what", "context", "authorizes"} {
			if got := lines[0].fields[field]; got != v {
				t.Errorf("%s: %q came back as %q", field, v, got)
			}
		}
		// The injected look-alikes must not have become fields of their own.
		if lines[0].fields["ref"] != "r" || lines[0].fields["scope"] != "s" {
			t.Errorf("%q leaked into a neighbouring field: %v", v, lines[0].fields)
		}
	}
}

// Order is the audit trail: List preserves on-disk append order and the report must not
// reorder it. And the count must be the number of records, including zero.
func TestFormatRecords_PreservesOrderAndCounts(t *testing.T) {
	recs := []Record{
		{TS: "1", By: "a", What: "first"},
		{TS: "2", By: "b", What: "second"},
		{TS: "3", By: "c", What: "third"},
	}
	lines, count := parseReport(t, FormatRecords(recs))
	var got []string
	for _, l := range lines {
		got = append(got, l.fields["what"])
	}
	if want := []string{"first", "second", "third"}; !reflect.DeepEqual(got, want) {
		t.Errorf("append order not preserved: got %v, want %v", got, want)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	lines, count = parseReport(t, FormatRecords(nil))
	if len(lines) != 0 || count != 0 {
		t.Errorf("no records must be zero lines and a count of 0, got %d lines, count %d", len(lines), count)
	}
}
