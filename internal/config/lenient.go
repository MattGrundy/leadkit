package config

import (
	"errors"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// IsOriginMismatch reports whether err is the loader declining a config because its project_origin
// names a different repository than the clone it was found for. That is a determinate answer ("this
// profile is not for this repo"), not a failure to read the profile: a fork works this way, and ticket
// 136 decided that such a config withholds its grants and does not refuse.
func IsOriginMismatch(err error) bool {
	var m *originMismatchError
	return errors.As(err, &m)
}

// Lenient is what can still be read, fail-closed, from a profile that did not load: the two
// enforcement facts the file guards need when the strict parse is lost (ticket 240).
//
// A lenient read is reached exactly when the strict parse FAILED, that is on input that may have been
// crafted to defeat it, so every ambiguity resolves to the STRICTER reading and never to the one that
// disarms a guard:
//
//   - owner-guard's mode is "advisory" only when it is positively read as advisory from the exact
//     schema path (guards.owner_guard.mode, else guards.default_mode, else an absent guards block). A
//     value that is not a string, not one of the two modes, a key spelled like the real one but not
//     equal to it, or a block of the wrong shape is "enforce".
//   - owners_file counts as set whenever the key is present with anything but an empty string, or a
//     key spelled like it is.
//   - store_root unreadable means the evidence rule applies to every task file.
//
// A file the generic YAML parse also rejects (a syntax error, a duplicated key, a merge-conflict
// marker) is not read at all: ReadLenient reports false and the caller refuses.
type Lenient struct {
	// OwnerGuardMode is "enforce" or "advisory".
	OwnerGuardMode string
	// OwnersFileSet is true when the profile declares (or may declare) an owners_file.
	OwnersFileSet bool
	// StoreRoot is the store_root as written ("" when absent, meaning the default).
	StoreRoot string
	// StoreRootUnreadable is true when store_root is present but cannot be read as a string.
	StoreRootUnreadable bool
}

// unreadableOwnersFile is the owners_file of a stand-in profile: a path that cannot be read, so the
// guard's existing "owners_file set but unreadable" rule applies.
const unreadableOwnersFile = ".leadkit/owners-file-unreadable-because-the-profile-did-not-load"

// foldKey normalises a key for the "spelled like it" test: case, `-` and spaces.
func foldKey(k string) string {
	return strings.ToLower(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(k)))
}

// lookalike reports whether m holds a key that is spelled like want without being want.
func lookalike(m map[string]any, want string) bool {
	for k := range m {
		if k != want && foldKey(k) == want {
			return true
		}
	}
	return false
}

// ReadLenient reads the Lenient facts from the raw profile data. ok is false when even the generic
// read fails, or the data holds no mapping.
func ReadLenient(data []byte) (l Lenient, ok bool) {
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil || m == nil {
		return Lenient{}, false
	}
	l.OwnerGuardMode = lenientOwnerMode(m)

	if lookalike(m, "owners_file") {
		l.OwnersFileSet = true
	} else if v, has := m["owners_file"]; has {
		s, isStr := v.(string)
		l.OwnersFileSet = !isStr || strings.TrimSpace(s) != ""
	}

	if lookalike(m, "store_root") {
		l.StoreRootUnreadable = true
	} else if v, has := m["store_root"]; has {
		if s, isStr := v.(string); isStr {
			l.StoreRoot = s
		} else {
			l.StoreRootUnreadable = true
		}
	}
	return l, true
}

// lenientOwnerMode is owner-guard's mode read fail-closed (see Lenient).
func lenientOwnerMode(m map[string]any) string {
	if lookalike(m, "guards") {
		return GuardModeEnforce
	}
	gv, has := m["guards"]
	if !has {
		return GuardModeAdvisory
	}
	g, isMap := gv.(map[string]any)
	if !isMap {
		return GuardModeEnforce
	}
	if lookalike(g, "owner_guard") || lookalike(g, "default_mode") {
		return GuardModeEnforce
	}
	if ov, has := g["owner_guard"]; has {
		og, isMap := ov.(map[string]any)
		if !isMap || lookalike(og, "mode") {
			return GuardModeEnforce
		}
		if mv, has := og["mode"]; has {
			mode, ok := lenientMode(mv)
			if !ok {
				return GuardModeEnforce
			}
			if mode != "" {
				return mode
			}
		}
	}
	dv, has := g["default_mode"]
	if !has {
		return GuardModeAdvisory
	}
	mode, ok := lenientMode(dv)
	if !ok {
		return GuardModeEnforce
	}
	if mode == "" {
		return GuardModeAdvisory
	}
	return mode
}

// lenientMode reads one mode value: "enforce" or "advisory" exactly, "" for the empty string (unset),
// ok=false for anything else.
func lenientMode(v any) (mode string, ok bool) {
	s, isStr := v.(string)
	if !isStr {
		return "", false
	}
	switch s {
	case GuardModeEnforce, GuardModeAdvisory, "":
		return s, true
	}
	return "", false
}

// StandIn is the profile the file guards act on when the real one did not load but could be read
// leniently: the built-in defaults with the facts above applied. root is the directory the profile's
// relative paths anchor to.
func (l Lenient) StandIn(root string) Profile {
	d := Defaults()
	d.root = root
	d.Guards.OwnerGuard.Mode = l.OwnerGuardMode
	if l.OwnersFileSet {
		d.OwnersFile = unreadableOwnersFile
	}
	switch {
	case l.StoreRootUnreadable:
		d.StoreRoot = string(filepath.Separator)
	case l.StoreRoot != "":
		d.StoreRoot = l.StoreRoot
	}
	return d
}

// UnreadableStandIn is the profile for a governing file that did not load and could not be read
// leniently either: nothing about it is known, so it is the strictest one the guards can act on.
func UnreadableStandIn(root string) Profile {
	return Lenient{OwnerGuardMode: GuardModeEnforce, OwnersFileSet: true, StoreRootUnreadable: true}.StandIn(root)
}
