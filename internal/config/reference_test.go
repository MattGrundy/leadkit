package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// clearProfileEnv unsets the env vars that would perturb Defaults()-derived defaults,
// so the reference (and the drift check) is deterministic regardless of the caller's
// shell — the committed doc reflects a clean environment.
func clearProfileEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LEADKIT_QUEUE", "LEADKIT_PROFILE", "LEADKIT_SESSION_PRIORITY"} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}
}

// entryByPath finds a reference entry by its yaml path.
func entryByPath(t *testing.T, entries []ReferenceEntry, path string) ReferenceEntry {
	t.Helper()
	for _, e := range entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no reference entry for path %q", path)
	return ReferenceEntry{}
}

// TestReference_EveryFieldHasMetadata is the ANTI-DRIFT guarantee: a reflective walk
// of the Profile struct (nested blocks + map/slice value structs included) asserts
// every yaml-tagged field has a fieldMeta entry with a description. A new/changed
// config field with no metadata FAILS here until documented.
func TestReference_EveryFieldHasMetadata(t *testing.T) {
	if missing := MissingMetadata(); len(missing) > 0 {
		t.Fatalf("config fields with no reference metadata (add them to fieldMeta in internal/config/reference.go): %s",
			strings.Join(missing, ", "))
	}
}

// TestReference_WalksNestedAndMapStructs makes the reflective coverage explicit: the
// walk must reach a top-level scalar, a nested-block field, a map-value struct field
// (AuthoritySpec), and a slice-of-struct field (RosterSpec).
func TestReference_WalksNestedAndMapStructs(t *testing.T) {
	entries := Reference()
	for _, want := range []string{
		"queue_root",                   // top-level scalar
		"burn.session_reserve",         // nested block
		"authority.<repo>.can_open_pr", // map-value struct (AuthoritySpec)
		"roster[].type",                // slice-of-struct (RosterSpec)
		"diagnostics.self_repair",      // nested block, enum
		"tests.<repo>.test_command",    // map-value struct (TestSpec)
	} {
		entryByPath(t, entries, want) // fails if absent
	}
}

// TestReference_KnownFieldWithDefaultAndAllowed checks a known field renders its
// default and allowed values.
func TestReference_KnownFieldWithDefaultAndAllowed(t *testing.T) {
	clearProfileEnv(t)
	entries := Reference()

	idle := entryByPath(t, entries, "milestone.idle_gate_mode")
	if idle.Default != DefaultIdleGateMode {
		t.Errorf("idle_gate_mode default = %q, want %q", idle.Default, DefaultIdleGateMode)
	}
	if got := strings.Join(idle.Enum, "|"); got != "block|warn" {
		t.Errorf("idle_gate_mode allowed = %q, want block|warn", got)
	}

	// A scalar default derived reflectively from Defaults().
	bl := entryByPath(t, entries, "brief_length_max")
	if bl.Default != "1400" {
		t.Errorf("brief_length_max default = %q, want 1400", bl.Default)
	}
}

// TestReference_AllowedValuesCorrectness pins the enum-like fields' allowed values.
func TestReference_AllowedValuesCorrectness(t *testing.T) {
	entries := Reference()
	cases := map[string]string{
		"milestone.idle_gate_mode":     "block|warn",
		"diagnostics.self_repair":      "report|command|auto",
		"authority.<repo>.can_open_pr": "true|false|human",
		"authority.<repo>.can_merge":   "true|false|human",
		"authority.<repo>.forge":       "github|gitlab",
		"workspace_model":              "worktrees|clones",
	}
	for path, want := range cases {
		e := entryByPath(t, entries, path)
		if got := strings.Join(e.Enum, "|"); got != want {
			t.Errorf("%s allowed = %q, want %q", path, got, want)
		}
	}
}

// TestReference_HumanTableShowsEnumOptions checks the default (human) output shows an
// enum field's options.
func TestReference_HumanTableShowsEnumOptions(t *testing.T) {
	clearProfileEnv(t)
	out := FormatReference(Reference())
	for _, want := range []string{
		"idle_gate_mode", "block|warn",
		"can_open_pr", "true|false|human",
		"self_repair", "report|command|auto",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human reference output missing %q", want)
		}
	}
}

// TestReferenceMarkdown_StableAndValid checks the markdown is deterministic and
// structurally valid (header, one table header + separator per block group).
func TestReferenceMarkdown_StableAndValid(t *testing.T) {
	clearProfileEnv(t)
	a := FormatReferenceMarkdown(Reference())
	b := FormatReferenceMarkdown(Reference())
	if a != b {
		t.Fatal("markdown reference is not deterministic (two renders differ)")
	}
	if !strings.HasPrefix(a, "# leadkit config reference") {
		t.Error("markdown reference missing top-level heading")
	}
	headers := strings.Count(a, "| Field | Type | Default | Allowed | Description |")
	seps := strings.Count(a, "| --- | --- | --- | --- | --- |")
	if headers == 0 || headers != seps {
		t.Errorf("markdown table headers=%d separators=%d (want equal and >0)", headers, seps)
	}
	// Every data row must have the 5-column pipe shape.
	for _, line := range strings.Split(a, "\n") {
		if strings.HasPrefix(line, "| `") { // a data row starts with a backticked field path
			if strings.Count(line, "|") < 6 {
				t.Errorf("malformed table row (too few columns): %q", line)
			}
		}
	}
}

// TestCommittedConfigReferenceInSync is the DRIFT CHECK: the committed
// docs/CONFIG-REFERENCE.md must byte-match the generator's current output. A field
// added/changed without regenerating (go generate ./internal/config) fails here.
func TestCommittedConfigReferenceInSync(t *testing.T) {
	clearProfileEnv(t)
	const rel = "../../docs/CONFIG-REFERENCE.md"
	committed, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read committed reference: %v", err)
	}
	want := FormatReferenceMarkdown(Reference())
	if string(committed) != want {
		t.Fatalf("docs/CONFIG-REFERENCE.md is out of sync with the config schema.\n" +
			"Regenerate it with:  go generate ./internal/config")
	}
}

// A map default is rendered from the map, whose iteration order is random. The reference is committed and
// diffed by CI, so the rendering must be identical every time: many keys, rendered repeatedly, so removing
// the sort fails every run.
func TestDefaultString_MapIsRenderedInKeyOrder(t *testing.T) {
	m := map[string]string{}
	var parts []string
	for i := 0; i < 12; i++ {
		k := fmt.Sprintf("key-%02d", 11-i)
		m[k] = fmt.Sprintf("v%d", 11-i)
	}
	for i := 0; i < 12; i++ {
		parts = append(parts, fmt.Sprintf("key-%02d=v%d", i, i))
	}
	want := strings.Join(parts, ", ")
	for run := 0; run < 64; run++ {
		if got := defaultString(reflect.ValueOf(m), reflect.TypeOf(m)); got != want {
			t.Fatalf("run %d: %q, want %q", run, got, want)
		}
	}
}
