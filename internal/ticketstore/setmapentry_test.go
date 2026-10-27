package ticketstore

import (
	"strings"
	"testing"
)

const prURL = "https://github.com/MattGrundy/leadkit/pull/110"

func setEntry(t *testing.T, text, mapKey, key, value string) (string, error) {
	t.Helper()
	p := put(t, t.TempDir(), "7-t.md", text)
	_, err := Update(p, func(d *Document) error { return d.SetMapEntry(mapKey, key, value) })
	return read(t, p), err
}

func TestSetMapEntry_FillsTheEmptyFlowMapInPlace(t *testing.T) {
	got, err := setEntry(t, base, "trackers", "pr", prURL)
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nid: 7\ntitle: t\nstatus: pending\ntrackers: {pr: " + prURL + "}\n---\n\n# body\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	doc := Parse("x", got)
	if v, ok := doc.Lookup("trackers", "pr"); !ok || v != prURL || len(doc.Findings) > 0 {
		t.Errorf("must read back as written: %q %v %v", v, ok, doc.Findings)
	}
}

func TestSetMapEntry_KeepsOtherEntriesAndReplacesOnlyItsOwn(t *testing.T) {
	text := "---\nid: 7\ntitle: t\nstatus: pending\ntrackers: {linear: ABC-123, pr: https://old/pull/1}\n---\n"
	got, err := setEntry(t, text, "trackers", "pr", prURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "trackers: {linear: ABC-123, pr: "+prURL+"}\n") {
		t.Errorf("got %q", got)
	}
	got, err = setEntry(t, text, "trackers", "github", "o/r#4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "linear: ABC-123, pr: https://old/pull/1, github: o/r#4}") {
		t.Errorf("got %q", got)
	}
}

func TestSetMapEntry_CreatesTheMapWhenAbsent(t *testing.T) {
	got, err := setEntry(t, "---\nid: 7\ntitle: t\nstatus: pending\n---\nb\n", "trackers", "pr", prURL)
	if err != nil || !strings.Contains(got, "status: pending\ntrackers: {pr: "+prURL+"}\n---\nb\n") {
		t.Errorf("%q %v", got, err)
	}
}

func TestSetMapEntry_QuotesAValueTheFlowFormCouldNotHold(t *testing.T) {
	got, err := setEntry(t, base, "trackers", "pr", "a, b}")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := Parse("x", got).Lookup("trackers", "pr"); !ok || v != "a, b}" {
		t.Errorf("got %q -> %q", got, v)
	}
}

func TestSetMapEntry_RefusesWhatItCannotRewriteSafely(t *testing.T) {
	block := "---\nid: 7\ntitle: t\nstatus: pending\ntrackers:\n  linear: ABC-1\n---\n"
	for name, tc := range map[string]struct{ text, mapKey, key, value string }{
		"block map":     {block, "trackers", "pr", prURL},
		"scalar":        {"---\nid: 7\ntitle: t\nstatus: pending\ntrackers: nope\n---\n", "trackers", "pr", prURL},
		"multiline":     {base, "trackers", "pr", "a\nb"},
		"bad entry key": {base, "trackers", "no good", "x"},
		"bad map key":   {base, "9 trackers", "pr", "x"},
	} {
		got, err := setEntry(t, tc.text, tc.mapKey, tc.key, tc.value)
		if err == nil {
			t.Errorf("%s: want a refusal, got %q", name, got)
		}
		if got != tc.text {
			t.Errorf("%s: a refused write must leave the file alone: %q", name, got)
		}
	}
}
