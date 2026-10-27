package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestMatchAuthorityKey_LongestWinsDeterministic is the core determinism guarantee:
// when two keys both substring-match one origin, the LONGER (more-specific) key wins,
// deterministically — not a map-iteration coin flip. Runs the same map many times to
// catch any residual randomness.
func TestMatchAuthorityKey_LongestWinsDeterministic(t *testing.T) {
	origin := "git@github.com:acmecorp/shared-infra.git"
	auth := map[string]AuthoritySpec{
		"infra":                 {PushWhitelist: []string{"chore/*"}},
		"acmecorp/shared-infra": {PushWhitelist: []string{"feat/*", "mg/*"}},
	}
	for i := 0; i < 200; i++ {
		key, spec, ok := MatchAuthorityKey(origin, auth, false)
		if !ok || key != "acmecorp/shared-infra" {
			t.Fatalf("run %d: got key=%q ok=%v, want the longest key to win", i, key, ok)
		}
		if !reflect.DeepEqual(spec.PushWhitelist, []string{"feat/*", "mg/*"}) {
			t.Fatalf("run %d: got spec %+v, want the longest key's spec", i, spec)
		}
	}
}

// TestMatchAuthorityKey_LexicalTiebreak: equal-length matching keys resolve by lexical
// order (smaller wins), stably.
func TestMatchAuthorityKey_LexicalTiebreak(t *testing.T) {
	origin := "host/aaa-bbb"
	auth := map[string]AuthoritySpec{
		"aaa": {},
		"bbb": {},
	}
	for i := 0; i < 50; i++ {
		key, _, ok := MatchAuthorityKey(origin, auth, false)
		if !ok || key != "aaa" {
			t.Fatalf("run %d: got key=%q ok=%v, want lexical-smaller 'aaa'", i, key, ok)
		}
	}
}

func TestMatchAuthorityKey_SingleAndNoMatch(t *testing.T) {
	auth := map[string]AuthoritySpec{"acmecorp/widgets": {CanPush: true}}
	key, _, ok := MatchAuthorityKey("git@github.com:acmecorp/widgets.git", auth, false)
	if !ok || key != "acmecorp/widgets" {
		t.Fatalf("single match: got key=%q ok=%v, want acmecorp/widgets", key, ok)
	}
	if _, _, ok := MatchAuthorityKey("git@github.com:other/repo.git", auth, false); ok {
		t.Fatalf("no match must return ok=false")
	}
	if _, _, ok := MatchAuthorityKey("", auth, false); ok {
		t.Fatalf("empty origin must return ok=false")
	}
	if _, _, ok := MatchAuthorityKey("git@github.com:acmecorp/widgets.git", nil, false); ok {
		t.Fatalf("nil map must return ok=false")
	}
}

// TestMatchAuthorityKey_RequirePushWhitelistFilter: the push-guard filter skips entries
// with no push_whitelist, even when they are the longest match.
func TestMatchAuthorityKey_RequirePushWhitelistFilter(t *testing.T) {
	origin := "git@github.com:acmecorp/shared-infra.git"
	auth := map[string]AuthoritySpec{
		"infra":                 {PushWhitelist: []string{"chore/*"}},
		"acmecorp/shared-infra": {CanPush: true}, // longer, but NO push_whitelist
	}
	// Unfiltered: the longer key wins.
	if key, _, ok := MatchAuthorityKey(origin, auth, false); !ok || key != "acmecorp/shared-infra" {
		t.Fatalf("unfiltered: got key=%q ok=%v, want longest", key, ok)
	}
	// Filtered (push-guard): the longer key has no push_whitelist, so the shorter
	// whitelisted key is the effective override.
	if key, spec, ok := MatchAuthorityKey(origin, auth, true); !ok || key != "infra" ||
		!reflect.DeepEqual(spec.PushWhitelist, []string{"chore/*"}) {
		t.Fatalf("filtered: got key=%q ok=%v spec=%+v, want 'infra'/[chore/*]", key, ok, spec)
	}
}

func TestMatchingAuthorityKeys_SortedLongestFirst(t *testing.T) {
	origin := "git@github.com:acmecorp/shared-infra.git"
	auth := map[string]AuthoritySpec{
		"infra":                 {},
		"acmecorp/shared-infra": {},
		"acmecorp":              {},
		"unrelated":             {},
	}
	got := MatchingAuthorityKeys(origin, auth)
	want := []string{"acmecorp/shared-infra", "acmecorp", "infra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchingAuthorityKeys = %v, want %v", got, want)
	}
	if MatchingAuthorityKeys("", auth) != nil {
		t.Fatalf("empty origin must return nil")
	}
}

func TestAuthorityKeyBoundaryAligned(t *testing.T) {
	cases := []struct {
		origin, key string
		want        bool
	}{
		{"git@github.com:acmecorp/platform-infra.git", "platform-infra", true},          // /...(.)
		{"git@github.com:acmecorp/platform-infra.git", "acmecorp/platform-infra", true}, // /...(.)
		{"git@github.com:acmecorp/shared-infra.git", "infra", false},                    // mid-word
		{"git@github.com:acmecorp/shared-infra.git", "shared-infra", true},
		{"git@github.com:acmecorp/widgets", "widgets", true}, // end-of-string
		{"host/aaa-bbb", "aaa", false},                       // followed by '-'
		{"host/aaa", "aaa", true},                            // preceded by '/', end
	}
	for _, c := range cases {
		if got := AuthorityKeyBoundaryAligned(c.origin, c.key); got != c.want {
			t.Errorf("AuthorityKeyBoundaryAligned(%q,%q) = %v, want %v", c.origin, c.key, got, c.want)
		}
	}
}

// TestEffectivePushWhitelist_AmbientProfile reproduces the ambient-profile report: an
// authority.someinfra.push_whitelist:[chore/*] that substring-matches the
// shared-infra origin REPLACES the mg/*-bearing default; config show must SEE
// the override + which key set it.
func TestEffectivePushWhitelist_AmbientProfile(t *testing.T) {
	prof := &Profile{
		PushWhitelist: []string{"feat/*", "fix/*", "chore/*", "mg/*", "user/*"},
		Authority: map[string]AuthoritySpec{
			// `infra` collides mid-word inside `shared-infra` — the ambient-profile shape.
			"infra": {PushWhitelist: []string{"chore/*"}},
		},
	}
	origin := "git@github.com:acmecorp/shared-infra.git"
	wl, key := EffectivePushWhitelist(prof, origin)
	if key != "infra" || !reflect.DeepEqual(wl, []string{"chore/*"}) {
		t.Fatalf("EffectivePushWhitelist = %v (from %q), want [chore/*] from infra", wl, key)
	}
	line := FormatEffectivePushWhitelist(prof, origin)
	for _, want := range []string{"effective push_whitelist", origin, "[chore/*]", "authority.infra.push_whitelist", "overrides the default"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}

func TestEffectivePushWhitelist_DefaultAndNoOrigin(t *testing.T) {
	prof := &Profile{
		PushWhitelist: []string{"feat/*", "mg/*"},
		Authority:     map[string]AuthoritySpec{"someinfra": {PushWhitelist: []string{"chore/*"}}},
	}
	// Origin resolves but no authority override matches -> default, "no override".
	wl, key := EffectivePushWhitelist(prof, "git@github.com:acmecorp/unrelated.git")
	if key != "" || !reflect.DeepEqual(wl, []string{"feat/*", "mg/*"}) {
		t.Fatalf("no-override origin: got %v/%q, want the default and no key", wl, key)
	}
	dline := FormatEffectivePushWhitelist(prof, "git@github.com:acmecorp/unrelated.git")
	if !strings.Contains(dline, "default (no authority.<repo> override matches)") || !strings.Contains(dline, "[feat/*,mg/*]") {
		t.Errorf("default line %q missing default marker / whitelist", dline)
	}
	// No origin at all -> default + "no origin resolved".
	nline := FormatEffectivePushWhitelist(prof, "")
	if !strings.Contains(nline, "no origin resolved") || !strings.Contains(nline, "[feat/*,mg/*]") {
		t.Errorf("no-origin line %q missing marker / whitelist", nline)
	}
}

// The collision scan lists every matching key, and builds the list from a map. Three keys can come back in
// the right order by chance, so this uses many and repeats: passing without the sort is not something a
// run will see.
func TestMatchingAuthorityKeys_OrderDoesNotDependOnMapIteration(t *testing.T) {
	origin := "git@github.com:acmecorp/shared-infra.git"
	auth := map[string]AuthoritySpec{"unrelated": {}, "zzz-not-in-origin": {}}
	// same length ties are broken lexically, longer keys first
	want := []string{"acmecorp/shared-infra", "github.com:acmecorp", "acmecorp/shared", "shared-infra", "github.com", "acmecorp", "git@github", "shared", "infra", "acme", "git", "com"}
	for _, k := range want {
		auth[k] = AuthoritySpec{}
	}
	for run := 0; run < 64; run++ {
		got := MatchingAuthorityKeys(origin, auth)
		if len(got) != len(want) {
			t.Fatalf("run %d: got %v", run, got)
		}
		for i := 1; i < len(got); i++ {
			if len(got[i-1]) < len(got[i]) || (len(got[i-1]) == len(got[i]) && got[i-1] > got[i]) {
				t.Fatalf("run %d: not longest-first with a lexical tiebreak: %v", run, got)
			}
		}
	}
}
