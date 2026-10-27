package config

import (
	"strings"
	"testing"
)

// Ticket 240. The lenient read is reached when the strict parse failed, that is on input an attacker
// controls, so every ambiguity must read as the STRICTER of the readings. Each row below names the
// permissive reading it must not produce.

func TestReadLenient_OwnerGuardModeFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want string
	}{
		// What the file says, read exactly.
		"no guards block":                      {"project_id: x\n", GuardModeAdvisory},
		"default advisory":                     {"guards:\n  default_mode: advisory\n", GuardModeAdvisory},
		"default enforce":                      {"guards:\n  default_mode: enforce\n", GuardModeEnforce},
		"default unset (empty string)":         {"guards:\n  default_mode: ''\n", GuardModeAdvisory},
		"owner_guard enforce over advisory":    {"guards:\n  default_mode: advisory\n  owner_guard:\n    mode: enforce\n", GuardModeEnforce},
		"owner_guard advisory over enforce":    {"guards:\n  default_mode: enforce\n  owner_guard:\n    mode: advisory\n", GuardModeAdvisory},
		"owner_guard without a mode: default":  {"guards:\n  default_mode: enforce\n  owner_guard: {}\n", GuardModeEnforce},
		"another guard's mode does not matter": {"guards:\n  push_gate:\n    mode: enforce\n", GuardModeAdvisory},
		// Cannot read confidently: enforce, never advisory.
		"guards is a scalar":                 {"guards: advisory\n", GuardModeEnforce},
		"guards is a list":                   {"guards: [advisory]\n", GuardModeEnforce},
		"guards is null":                     {"guards:\n", GuardModeEnforce},
		"guards spelled with a capital":      {"Guards:\n  default_mode: advisory\n", GuardModeEnforce},
		"default_mode wrong case value":      {"guards:\n  default_mode: Advisory\n", GuardModeEnforce},
		"default_mode with a trailing blank": {"guards:\n  default_mode: 'advisory '\n", GuardModeEnforce},
		"default_mode not a string":          {"guards:\n  default_mode: 3\n", GuardModeEnforce},
		"default_mode a list":                {"guards:\n  default_mode: [advisory]\n", GuardModeEnforce},
		"default_mode a lookalike":           {"guards:\n  Default-Mode: advisory\n", GuardModeEnforce},
		"owner_guard a scalar":               {"guards:\n  owner_guard: advisory\n", GuardModeEnforce},
		"owner_guard mode a list":            {"guards:\n  owner_guard:\n    mode: [advisory]\n", GuardModeEnforce},
		"owner_guard mode unknown":           {"guards:\n  owner_guard:\n    mode: bogus\n", GuardModeEnforce},
		"owner_guard mode a lookalike":       {"guards:\n  owner_guard:\n    Mode: advisory\n", GuardModeEnforce},
		"owner_guard a lookalike":            {"guards:\n  default_mode: advisory\n  Owner-Guard:\n    mode: advisory\n", GuardModeEnforce},
		// Two readings of the same thing: the stricter one.
		"exact advisory beside a lookalike enforce": {"guards:\n  owner_guard:\n    mode: advisory\n  Owner_Guard:\n    mode: enforce\n", GuardModeEnforce},
		"advisory default, enforce lookalike":       {"guards:\n  default_mode: advisory\n  DEFAULT_MODE: enforce\n", GuardModeEnforce},
	} {
		l, ok := ReadLenient([]byte(tc.yaml))
		if !ok {
			t.Errorf("%s: the generic read failed on %q", name, tc.yaml)
			continue
		}
		if l.OwnerGuardMode != tc.want {
			t.Errorf("%s: owner-guard mode = %q, want %q for %q", name, l.OwnerGuardMode, tc.want, tc.yaml)
		}
	}
}

func TestReadLenient_OwnersFileCountsAsSetUnlessPositivelyAbsent(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want bool
	}{
		"absent":                     {"project_id: x\n", false},
		"empty string is unset":      {"owners_file: ''\n", false},
		"a path":                     {"owners_file: ./owners.yaml\n", true},
		"a list":                     {"owners_file: [a, b]\n", true},
		"a map":                      {"owners_file: {a: b}\n", true},
		"null":                       {"owners_file:\n", true},
		"a number":                   {"owners_file: 3\n", true},
		"a lookalike key":            {"Owners_File: ./o.yaml\n", true},
		"a hyphenated lookalike":     {"owners-file: ./o.yaml\n", true},
		"exact empty, lookalike set": {"owners_file: ''\nOWNERS_FILE: ./o.yaml\n", true},
	} {
		l, ok := ReadLenient([]byte(tc.yaml))
		if !ok {
			t.Errorf("%s: the generic read failed", name)
			continue
		}
		if l.OwnersFileSet != tc.want {
			t.Errorf("%s: OwnersFileSet = %v, want %v for %q", name, l.OwnersFileSet, tc.want, tc.yaml)
		}
	}
}

func TestReadLenient_StoreRoot(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml       string
		root       string
		unreadable bool
	}{
		"absent":          {"project_id: x\n", "", false},
		"a path":          {"store_root: ./t\n", "./t", false},
		"a list":          {"store_root: [a]\n", "", true},
		"null":            {"store_root:\n", "", true},
		"a lookalike key": {"Store-Root: ./t\n", "", true},
	} {
		l, ok := ReadLenient([]byte(tc.yaml))
		if !ok || l.StoreRoot != tc.root || l.StoreRootUnreadable != tc.unreadable {
			t.Errorf("%s: %+v ok=%v, want root %q unreadable %v", name, l, ok, tc.root, tc.unreadable)
		}
	}
}

// A file the generic read also rejects is not read at all: the caller refuses.
func TestReadLenient_AFileTheGenericReadRejectsIsNotRead(t *testing.T) {
	for name, y := range map[string]string{
		"a syntax error":                  "guards: [\n",
		"a merge conflict marker":         "<<<<<<< HEAD\nguards:\n  default_mode: advisory\n=======\nguards:\n  default_mode: enforce\n>>>>>>> b\n",
		"a duplicated key":                "guards:\n  default_mode: advisory\n  default_mode: enforce\n",
		"a duplicated top-level guards":   "guards:\n  default_mode: enforce\nguards:\n  default_mode: advisory\n",
		"nothing":                         "",
		"only a comment":                  "# nothing\n",
		"a scalar document":               "just a string\n",
		"a tab where the grammar forbids": "guards:\n\tdefault_mode: advisory\n",
	} {
		if l, ok := ReadLenient([]byte(y)); ok {
			t.Errorf("%s: read as %+v, want it refused", name, l)
		}
	}
}

// The crafted file the lead asked to be pinned: the strict parse fails (a schema error elsewhere) and the
// guards block is ambiguous. It must not come out as advisory.
func TestReadLenient_ACraftedFileThatDefeatsTheStrictParseDoesNotYieldAdvisory(t *testing.T) {
	crafted := []string{
		// strict fails on queue_root; guards reads advisory only through a lookalike
		"queue_root: [1]\nGuards:\n  default_mode: advisory\n",
		// strict fails on the owner_guard shape; the scalar is not a mode the schema allows
		"guards:\n  default_mode: advisory\n  owner_guard: advisory\nowners_file: ./o.yaml\n",
		// an anchor/merge that makes two readings
		"base: &b {default_mode: advisory}\nguards:\n  <<: *b\n  default_mode: enforce\n",
		// a mode written with a trailing blank, quoted, beside a schema error
		"roster: x\nguards:\n  default_mode: \"advisory \"\n",
		// a flow mapping with the real key and a lookalike
		"guards: {default_mode: advisory, default-mode: enforce}\nstore_root: [x]\n",
	}
	for _, y := range crafted {
		if !strings.Contains(y, "guards") && !strings.Contains(y, "Guards") {
			t.Fatalf("fixture error: %q", y)
		}
		l, ok := ReadLenient([]byte(y))
		if !ok {
			continue // not read at all: refused, which is also not advisory
		}
		if l.OwnerGuardMode != GuardModeEnforce {
			t.Errorf("a crafted file read as %q (want enforce, or not read): %q", l.OwnerGuardMode, y)
		}
	}
}

func TestLenient_StandInCarriesTheFactsAndTheStrictestWhenNothingIsKnown(t *testing.T) {
	root := t.TempDir()
	s := Lenient{OwnerGuardMode: GuardModeEnforce, OwnersFileSet: true, StoreRoot: "./t"}.StandIn(root)
	if got := s.EffectiveGuardMode(GuardOwnerGuard); got != GuardModeEnforce || s.OwnersFile == "" || s.Resolve(s.StoreRoot) != root+"/t" {
		t.Errorf("stand-in = mode %q owners %q store %q", got, s.OwnersFile, s.Resolve(s.StoreRoot))
	}
	a := Lenient{OwnerGuardMode: GuardModeAdvisory}.StandIn(root)
	if a.EffectiveGuardMode(GuardOwnerGuard) != GuardModeAdvisory || a.OwnersFile != "" {
		t.Errorf("an advisory stand-in with no owners_file: mode %q owners %q", a.EffectiveGuardMode(GuardOwnerGuard), a.OwnersFile)
	}
	h := UnreadableStandIn(root)
	if h.EffectiveGuardMode(GuardOwnerGuard) != GuardModeEnforce || h.OwnersFile == "" || h.Resolve(h.StoreRoot) != "/" {
		t.Errorf("the strictest stand-in = mode %q owners %q store %q", h.EffectiveGuardMode(GuardOwnerGuard), h.OwnersFile, h.Resolve(h.StoreRoot))
	}
	d := Lenient{OwnerGuardMode: GuardModeAdvisory, StoreRootUnreadable: true}.StandIn(root)
	if d.Resolve(d.StoreRoot) != "/" {
		t.Errorf("an unreadable store_root must put every file in scope, got %q", d.Resolve(d.StoreRoot))
	}
}
