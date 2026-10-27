// Package scrubterms loads the list of terms the repo's scrub ratchets forbid.
//
// The list is deliberately NOT in the committed tree. A file that spells out "these names must
// never appear in this repo" is itself a place those names appear, so publishing the repo would
// publish the list. It lives in a file outside the checkout instead.
//
// What happens when the file is absent depends on the caller, and the two differ on purpose:
//   - The real-tree ratchets (the source, test-file and prose scans in cmd/leadkit) call Require
//     and SKIP with an explicit message. A public clone runs green without ever holding the terms.
//   - Tests that have committed generic checks of their own call Optional. They keep those
//     checks and ADD the specific names when the file exists, so without it they check strictly
//     less; Optional logs that it did so rather than saying nothing (visible under go test -v).
//
// The same mechanism is the adopter-facing ratchet: point LEADKIT_SCRUB_TERMS at a file listing
// YOUR organisation's names and the ratchets enforce them in your fork.
//
// The bound, stated so a green run is not read as more than it is: a ratchet proves the tree
// contains none of the terms IT WAS GIVEN. A leak whose name is not on the list passes, and the
// list is the owner's to maintain. Adding two internal names to the list once found a file that a
// shorter list had reported clean.
//
// What belongs on the list is names that must never ship: an employer, an internal system or
// script, a machine path. It is not the maintainer's public identity (the copyright holder, the
// install path); a ratchet that fires on the project's own name gets switched off. Prefer the
// path form of a handle (/users/<name>) to the bare handle.
//
// File format, one entry per line:
//
//	// a comment
//	acmecorp           a term, matched case-insensitively as a substring everywhere
//	src: #42           a term matched in shipping source and prose only, not in _test.go files
//
// Blank lines are ignored. The "src:" scope exists for terms that tests legitimately reuse as
// placeholders (a bare ticket number), where scanning test files would flag the placeholder.
//
// Resolve (used by `leadkit publish`) is stricter than Parse and Load, which the ratchets use: it
// requires every entry to use only letters, digits, space and . _ / # @ : + - (single spaces, no
// leading "- ") and an exactly-spelt "src:" prefix, because it cannot tell a term that matches
// nothing from one that is clean. The ratchets still accept more, so a list can be valid for them
// and refused by publish; that difference is deliberate for now.
package scrubterms

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnvVar names the environment variable that overrides the default location.
const EnvVar = "LEADKIT_SCRUB_TERMS"

// srcPrefix scopes a term to shipping source and prose.
const srcPrefix = "src:"

// Terms is a parsed term list.
type Terms struct {
	// All are the terms to match everywhere, lowercased.
	All []string
	// SourceOnly are the terms to match everywhere EXCEPT in _test.go files, lowercased.
	SourceOnly []string
}

// Everywhere returns every term, for scanning shipping source and prose.
func (t Terms) Everywhere() []string {
	return append(append([]string{}, t.All...), t.SourceOnly...)
}

// ForTests returns the terms that apply to _test.go files.
func (t Terms) ForTests() []string { return append([]string{}, t.All...) }

// Path returns where the term file is expected: $LEADKIT_SCRUB_TERMS when set, otherwise
// ~/.claude/leadkit/config/scrub-terms.txt.
func Path() string {
	if p := os.Getenv(EnvVar); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "leadkit", "config", "scrub-terms.txt")
}

// Parse reads a term list. It returns an error for an empty list, so a file that exists but
// holds nothing cannot pass for a clean ratchet.
func Parse(data string) (Terms, error) {
	var t Terms
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, srcPrefix); ok {
			if rest = strings.ToLower(strings.TrimSpace(rest)); rest != "" {
				t.SourceOnly = append(t.SourceOnly, rest)
			}
			continue
		}
		t.All = append(t.All, strings.ToLower(line))
	}
	if len(t.All)+len(t.SourceOnly) == 0 {
		return Terms{}, errors.New("no terms in the file")
	}
	return t, nil
}

// Load reads the term file. found is false when the file does not exist, which is the normal
// state of a public clone; any other failure (unreadable, empty) is an error, because a ratchet
// must not quietly stand down on a file that is present but broken.
func Load() (terms Terms, found bool, err error) {
	return LoadFrom(Path())
}

// LoadFrom is Load for an explicit path.
func LoadFrom(path string) (terms Terms, found bool, err error) {
	if path == "" {
		return Terms{}, false, nil
	}
	data, rerr := os.ReadFile(path)
	if errors.Is(rerr, fs.ErrNotExist) {
		return Terms{}, false, nil
	}
	if rerr != nil {
		return Terms{}, true, fmt.Errorf("read scrub terms %s: %w", path, rerr)
	}
	t, perr := Parse(string(data))
	if perr != nil {
		return Terms{}, true, fmt.Errorf("scrub terms %s: %w", path, perr)
	}
	return t, true, nil
}

// T is the part of *testing.T the helpers need. Declared here so this shipping package does not
// import "testing" into the binary.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// SkipMessage is the explanation a skipped ratchet prints.
func SkipMessage(path string) string {
	return fmt.Sprintf("no scrub term file at %s (set %s to point at one); the terms are kept out "+
		"of the committed tree on purpose, so a clone without the file cannot run this ratchet", path, EnvVar)
}

// Require returns the terms, skipping the test with SkipMessage when the file is absent and
// failing it when the file is present but unusable.
func Require(t T) Terms {
	t.Helper()
	terms, found, err := Load()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !found {
		t.Skipf("%s", SkipMessage(Path()))
	}
	return terms
}

// Optional returns the terms, or the zero Terms when the file is absent, in which case it logs
// that the caller is checking only its committed needles. For tests that have a committed generic
// check and merely add the specific names when they are available; it does not skip, because that
// would drop the generic check too.
func Optional(t T) Terms {
	t.Helper()
	terms, found, err := Load()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !found {
		t.Logf("scrub terms not checked here: %s", SkipMessage(Path()))
	}
	return terms
}
