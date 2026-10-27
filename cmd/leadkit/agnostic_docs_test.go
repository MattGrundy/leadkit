package main

// Doc-side half of the project-agnostic guard (docs/build-tasks/project-agnostic-defaults.md §4).
//
// TestSource_IsProjectAgnostic covers shipping Go. Prose was never covered, which made the
// scrub reversible: a doc could be genericised in one pass and regress in the next with
// nothing failing. This file closes that by scanning *.md / *.yaml / *.yml for the same
// denylist, behind a RATCHET rather than a clean-tree assertion: much of the prose is still
// dirty (workstream 6 in docs/BACKLOG.md), so demanding a clean tree today would just be a
// permanently red test. The allowlist below is the live count; a number written here would be
// wrong from the next scrub onwards with nothing to catch it.
//
// The ratchet has two halves, and both must hold:
//   - A file NOT on the allowlist must be clean. Reintroducing a token fails the build.
//   - A path ON the allowlist must still be dirty. Finishing the scrub on an allowlisted
//     path fails the build too, telling you to delete the entry. That is what makes the
//     list shrink instead of rotting into a permanent exemption.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/commentscan"
	"mgpp.dev/leadkit/internal/publish"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// agnosticDocAllowlist is the set of prose paths KNOWN to still name adopter specifics and
// not yet scrubbed. Entries ending in "/" are directory prefixes; the rest are exact
// repo-relative paths. Every entry is a debt, not a permission: shrink this list, never
// grow it. Adding a path here to make a failing build green reverses the ratchet.
var agnosticDocAllowlist = []string{
	// The per-change spec trail. Densest surface, and the standing decision in
	// workstream 6 is whether these belong in a public repo at all.
	"docs/build-tasks/",
	// Docs an adopter is meant to read.
	"ONBOARDING.md",
	"docs/ADOPTION-BRIEF.md",
	"docs/BACKLOG.md",
	"docs/HIERARCHY-AND-SYNC.md",
	"docs/OPERATING-MODEL.md",
	"docs/PORTABLE-OPERATING-KIT.md",
	"docs/design/cross-project-coordination.md",
}

// agnosticDocExtensions are the prose file types the ratchet scans. ".example" is the adopter's
// starting profile (leadkit.yaml.example): path.Ext reads the last dot, so ".yaml" never matched it
// and the one file an adopter copies verbatim was unguarded.
var agnosticDocExtensions = []string{".md", ".yaml", ".yml", ".example"}

// docAllowlisted reports whether rel (a repo-relative, slash-separated path) is covered by
// the allowlist, either as an exact file or under a directory prefix.
func docAllowlisted(rel string, allowlist []string) bool {
	for _, entry := range allowlist {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(rel, entry) {
				return true
			}
			continue
		}
		if rel == entry {
			return true
		}
	}
	return false
}

// scanDocsForTokens lists the tracked prose files under root and returns one "<relpath>:<line>: <token>" finding per
// denylisted token occurrence in a prose file, case-insensitively. It does NOT apply the
// allowlist: callers decide what to do with each finding, which is what lets one caller
// assert "not allowlisted ⇒ clean" and the other assert "allowlisted ⇒ still dirty".
func scanDocsForTokens(t *testing.T, root string, denylist []string) []string {
	t.Helper()
	lowered := make([]string, len(denylist))
	for i, d := range denylist {
		lowered[i] = strings.ToLower(d)
	}
	var findings []string
	files, err := commentscan.Files(root, agnosticDocExtensions...)
	if err != nil {
		t.Fatalf("list %s: %v", root, err)
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			low := strings.ToLower(line)
			for _, tok := range lowered {
				if strings.Contains(low, tok) {
					findings = append(findings, rel+":"+itoa(lineNo+1)+": "+tok)
				}
			}
		}
	}
	return findings
}

// TestDocs_ScrubIsRatcheted is the forward half: any prose file NOT on the allowlist must
// be free of adopter specifics, so a scrubbed doc cannot quietly regress. Green proves the prose
// lacks the terms in the scrub term file, nothing more: a leak not on the list passes.
func TestDocs_ScrubIsRatcheted(t *testing.T) {
	terms := scrubterms.Require(t).Everywhere()
	repoRoot := repoRootFromCwd(t)
	var leaked []string
	for _, f := range scanDocsForTokens(t, repoRoot, terms) {
		rel := f[:strings.Index(f, ":")]
		if !docAllowlisted(rel, agnosticDocAllowlist) {
			leaked = append(leaked, f)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("prose outside the scrub allowlist names adopter specifics — %d finding(s).\n"+
			"Scrub the file; do NOT add it to agnosticDocAllowlist, which only records debt that predates this guard:\n%s",
			len(leaked), strings.Join(leaked, "\n"))
	}
}

// allowlistVerdict sorts agnosticDocAllowlist entries by what the tree can say about them.
type allowlistVerdict struct {
	stale      []string // present and covered by no dirty file: the ratchet says delete
	notChecked []string // absent, and the exclusion manifest lists them: publish removed them
	missing    []string // absent and NOT listed: deleted or renamed, so the entry is dead
}

// classifyAllowlist stats each entry under root. A present entry is stale when no dirty file is
// under it. An absent entry is tolerated only when manifest lists it, because
// `leadkit publish prepare` removes exactly those docs from the public tree; any other absence is
// a deleted or renamed path whose entry must go, so it is reported rather than excused. Anything
// short of "present" or "does not exist" (a stat error) is returned as an error.
func classifyAllowlist(root string, dirty map[string]bool, allowlist, manifest []string) (allowlistVerdict, error) {
	var v allowlistVerdict
	for _, entry := range allowlist {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(entry, "/"))))
		if errors.Is(err, fs.ErrNotExist) {
			if publish.Excluded(manifest, entry) {
				v.notChecked = append(v.notChecked, entry)
			} else {
				v.missing = append(v.missing, entry)
			}
			continue
		}
		if err != nil {
			return v, fmt.Errorf("stat allowlist entry %q: %w", entry, err)
		}
		covered := false
		for rel := range dirty {
			if docAllowlisted(rel, []string{entry}) {
				covered = true
				break
			}
		}
		if !covered {
			v.stale = append(v.stale, entry)
		}
	}
	return v, nil
}

// TestDocs_AllowlistHasNoStaleEntries is the reverse half, and the reason this is a ratchet
// rather than an exemption list: once a path is genuinely clean its entry must go, so the
// list can only shrink. An entry for a path that does not exist is not clean; it is tolerated
// only if docs/publish-exclude.txt lists it (the public tree), and fails otherwise.
func TestDocs_AllowlistHasNoStaleEntries(t *testing.T) {
	terms := scrubterms.Require(t).Everywhere()
	repoRoot := repoRootFromCwd(t)
	manifest, err := publish.LoadManifest(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	dirty := make(map[string]bool)
	for _, f := range scanDocsForTokens(t, repoRoot, terms) {
		dirty[f[:strings.Index(f, ":")]] = true
	}
	v, err := classifyAllowlist(repoRoot, dirty, agnosticDocAllowlist, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range v.stale {
		t.Errorf("agnosticDocAllowlist entry %q is clean now — delete the entry so the guard starts enforcing it", entry)
	}
	for _, entry := range v.missing {
		t.Errorf("agnosticDocAllowlist entry %q names a path that does not exist and is not in %s — the file was deleted or renamed, so delete or update the entry", entry, publish.DefaultManifest)
	}
	if len(v.notChecked) > 0 {
		t.Logf("NOT CHECKED: %d agnosticDocAllowlist entr(ies) absent from this tree and listed in %s (expected in a prepared public tree): %v", len(v.notChecked), publish.DefaultManifest, v.notChecked)
	}
}

// TestDocs_AllowlistRatchetSeparatesAbsentFromClean proves classifyAllowlist discriminates every
// state against a fixture: present-dirty is kept, present-clean is stale (the ratchet's whole
// purpose), absent-and-listed is not checked, absent-and-unlisted is missing, and an entry that
// cannot be statted is an error rather than a skip.
func TestDocs_AllowlistRatchetSeparatesAbsentFromClean(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"dirty.md":      "names widget-ci\n",
		"clean.md":      "nothing here\n",
		"cleandir/a.md": "nothing here\n",
	} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirty := make(map[string]bool)
	for _, f := range scanDocsForTokens(t, dir, fixtureDenylist) {
		dirty[f[:strings.Index(f, ":")]] = true
	}
	manifest := []string{"gone.md", "gonedir"}
	allow := []string{"dirty.md", "clean.md", "cleandir/", "gone.md", "gonedir/", "renamed.md", "renameddir/"}
	v, err := classifyAllowlist(dir, dirty, allow, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"clean.md", "cleandir/"}; !equalStrings(v.stale, want) {
		t.Errorf("stale = %v, want %v (present and clean must still be reported)", v.stale, want)
	}
	if want := []string{"gone.md", "gonedir/"}; !equalStrings(v.notChecked, want) {
		t.Errorf("notChecked = %v, want %v (absent and excluded by the manifest)", v.notChecked, want)
	}
	if want := []string{"renamed.md", "renameddir/"}; !equalStrings(v.missing, want) {
		t.Errorf("missing = %v, want %v (absent and not excluded must not be excused)", v.missing, want)
	}

	// A path through a regular file is neither present nor absent: it must surface as an error even
	// when the manifest lists it.
	if _, err := classifyAllowlist(dir, dirty, []string{"clean.md/child.md"}, []string{"clean.md/child.md"}); err == nil {
		t.Error("an entry that cannot be statted must be an error, not a skip")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDocs_RatchetCatchesReintroduction is the meta-proof: the scanner and the allowlist
// logic genuinely discriminate, so a green ratchet means "clean", not "inert". Asserted
// against a temp fixture, because the real tree cannot exhibit the failing case.
func TestDocs_RatchetCatchesReintroduction(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A doc outside the allowlist that reintroduces a token: must be caught.
	write("README.md", "# leadkit\n\nGate the widget-ci remote.\n")
	// The adopter's starting profile is a ".example": it ends in neither ".yaml" nor ".md", and was not
	// scanned until the extension was listed.
	write("leadkit.yaml.example", "project_id: widget-ci\n")
	// An allowlisted dir and an allowlisted exact file: found, but not reported as leaks.
	write("docs/build-tasks/spec.md", "target: acmecorp-product\n")
	write("ONBOARDING.md", "clone migrate-acmecorp first\n")
	// A non-prose file with a token: out of scope for this scanner (Go is covered by
	// TestSource_IsProjectAgnostic, which is where a .go finding must surface).
	write("leak.go", "package x\n\nvar X = \"acmecorp\"\n")

	all := scanDocsForTokens(t, dir, fixtureDenylist)
	var leaked, allowed []string
	for _, f := range all {
		rel := f[:strings.Index(f, ":")]
		if docAllowlisted(rel, agnosticDocAllowlist) {
			allowed = append(allowed, f)
		} else {
			leaked = append(leaked, f)
		}
	}
	if len(leaked) != 2 || !strings.HasPrefix(leaked[0], "README.md:") || !strings.HasPrefix(leaked[1], "leadkit.yaml.example:") {
		t.Errorf("want exactly two leaks, from README.md and leadkit.yaml.example; got %v", leaked)
	}
	// Three, not two: the ONBOARDING.md line matches both "migrate-acmecorp" and the
	// shorter "acmecorp" it contains. Overlapping denylist tokens double-report by design.
	if len(allowed) != 3 {
		t.Errorf("want the allowlisted findings suppressed as known debt, got %d: %v", len(allowed), allowed)
	}
	for _, f := range all {
		if strings.HasPrefix(f, "leak.go:") {
			t.Errorf("the prose scanner must not scan Go; it flagged %q", f)
		}
	}
}
