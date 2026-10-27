package typeinfer

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgentDef(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testCanonical(t *testing.T) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"ci-infra-agent", "devops-agent", "code-review-agent", "critique-agent", "lead"} {
		writeAgentDef(t, dir, n)
	}
	return LoadCanonical(dir)
}

func TestLoadCanonical_ReadsBasenamesFromDisk(t *testing.T) {
	c := testCanonical(t)
	for _, want := range []string{"ci-infra-agent", "devops-agent", "code-review-agent", "critique-agent", "lead"} {
		if !c[want] {
			t.Errorf("canonical set missing %q: %v", want, c)
		}
	}
}

func TestLoadCanonical_MissingDirYieldsEmptySetNotError(t *testing.T) {
	c := LoadCanonical(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(c) != 0 {
		t.Errorf("want an empty set, got %v", c)
	}
}

func TestMatch_ExactCanonicalName(t *testing.T) {
	c := testCanonical(t)
	got, ok := Match("ci-infra-agent", c)
	if !ok || got != "ci-infra-agent" {
		t.Errorf("Match(ci-infra-agent) = (%q, %v), want (ci-infra-agent, true)", got, ok)
	}
}

func TestMatch_ShortFormGetsAgentSuffixAppended(t *testing.T) {
	c := testCanonical(t)
	for _, raw := range []string{"ci-infra", "CI-Infra", "ci_infra", "ci infra"} {
		got, ok := Match(raw, c)
		if !ok || got != "ci-infra-agent" {
			t.Errorf("Match(%q) = (%q, %v), want (ci-infra-agent, true)", raw, got, ok)
		}
	}
}

func TestMatch_BareCanonicalNameWithNoAgentSuffix(t *testing.T) {
	c := testCanonical(t)
	got, ok := Match("lead", c)
	if !ok || got != "lead" {
		t.Errorf("Match(lead) = (%q, %v), want (lead, true)", got, ok)
	}
}

func TestMatch_UnknownNameFails(t *testing.T) {
	c := testCanonical(t)
	for _, raw := range []string{"", "null", "fix-973", "owner-479a"} {
		if _, ok := Match(raw, c); ok {
			t.Errorf("Match(%q) should not match anything, but it did", raw)
		}
	}
}

func TestResolve_ExplicitTypeWins(t *testing.T) {
	c := testCanonical(t)
	typ, source, ok := Resolve("devops-agent", "ci-infra-agent", "cr-1", nil, c)
	if !ok || typ != "devops-agent" || source != "type" {
		t.Errorf("Resolve = (%q, %q, %v), want (devops-agent, type, true) — explicit type must win over owner", typ, source, ok)
	}
}

func TestResolve_NonCanonicalExplicitTypeFallsThroughToOwner(t *testing.T) {
	c := testCanonical(t)
	typ, source, ok := Resolve("bogus-type", "ci-infra", "", nil, c)
	if !ok || typ != "ci-infra-agent" || source != "owner" {
		t.Errorf("Resolve = (%q, %q, %v), want (ci-infra-agent, owner, true)", typ, source, ok)
	}
}

func TestResolve_FallsBackToOwner(t *testing.T) {
	c := testCanonical(t)
	typ, source, ok := Resolve("", "devops", "", nil, c)
	if !ok || typ != "devops-agent" || source != "owner" {
		t.Errorf("Resolve = (%q, %q, %v), want (devops-agent, owner, true)", typ, source, ok)
	}
}

func TestResolve_FallsBackToLaneDirectMatch(t *testing.T) {
	c := testCanonical(t)
	typ, source, ok := Resolve("", "unassigned", "devops", nil, c)
	if !ok || typ != "devops-agent" || source != "lane" {
		t.Errorf("Resolve = (%q, %q, %v), want (devops-agent, lane, true)", typ, source, ok)
	}
}

func TestResolve_FallsBackToLaneViaRolesReverseLookup(t *testing.T) {
	c := testCanonical(t)
	laneToRole := map[string]string{"cr-1": "code-review"}
	typ, source, ok := Resolve("", "unassigned", "cr-1", laneToRole, c)
	if !ok || typ != "code-review-agent" || source != "lane" {
		t.Errorf("Resolve = (%q, %q, %v), want (code-review-agent, lane, true)", typ, source, ok)
	}
}

func TestResolve_UnresolvableIsManual(t *testing.T) {
	c := testCanonical(t)
	cases := []struct{ owner, lane string }{
		{"null", ""}, {"unassigned", ""}, {"fix-973", ""}, {"", ""},
	}
	for _, tc := range cases {
		typ, source, ok := Resolve("", tc.owner, tc.lane, nil, c)
		if ok {
			t.Errorf("Resolve(owner=%q, lane=%q) = (%q, %q, true), want ok=false (manual)", tc.owner, tc.lane, typ, source)
		}
	}
}

// LoadCanonicalOrEmbedded exists because untracking a captured team snapshot would
// otherwise make `task create` reject EVERY --type against an empty set. These two tests
// pin both halves: the fallback fires when there is nothing on disk, and a project's own
// definitions still win when there are. Without the second, the fallback could shadow a
// project's real type set and nothing would say so.
func TestLoadCanonicalOrEmbedded_FallsBackWhenDirMissing(t *testing.T) {
	c := LoadCanonicalOrEmbedded(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(c) == 0 {
		t.Fatal("want the embedded set as a fallback, got an empty set — `task create` would reject every type")
	}
	// Spot-check roles the embedded set is required to carry; a fallback that returned some
	// unrelated set would still be non-empty.
	for _, want := range []string{"lead", "code-review-agent", "critique-agent", "developer"} {
		if !c[want] {
			t.Errorf("embedded fallback missing %q: %v", want, c)
		}
	}
}

func TestLoadCanonicalOrEmbedded_ProjectDefsWinOverEmbedded(t *testing.T) {
	dir := t.TempDir()
	// A project declaring exactly one, deliberately unusual, type.
	if err := os.WriteFile(filepath.Join(dir, "only-local-role.md"), []byte("---\nname: only-local-role\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := LoadCanonicalOrEmbedded(dir)
	if !c["only-local-role"] {
		t.Errorf("project's own type missing: %v", c)
	}
	// The embedded set must NOT be merged in: a project that declares its roster has
	// declared it exhaustively, and silently adding the embedded set would let a task be filed
	// against a role the project does not staff.
	if c["lead"] || c["critique-agent"] {
		t.Errorf("embedded set leaked into a project that declares its own roster: %v", c)
	}
	if len(c) != 1 {
		t.Errorf("want exactly the project's 1 type, got %d: %v", len(c), c)
	}
}
