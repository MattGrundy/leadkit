package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// Pins for ticket 330 (from the ticket 156 mutation sweep). Each test names the mutant it kills:
// the properties held, but nothing failed when an edit removed them, and these are the checks a
// publish must not report CLEAN without.

// Mutant (S12/S12b): scan.go's `if rerr != nil { return rerr }` after os.ReadFile swallowed, so a
// file the scan could not read was skipped and the run reported CLEAN against it. This is ticket
// 324's shape inside publish. The control is the same file made readable.
func TestScanTree_AnUnreadableFileIsAnErrorNotACleanResult(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file, so there is no unreadable file to make")
	}
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("deploys to "+fixtureTerm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	terms := scrubterms.Terms{All: []string{fixtureTerm}}

	if f, _, _, err := ScanTree(root, terms); err != nil || len(f) == 0 {
		t.Fatalf("control: a readable file holding the term must be found: %v %v", f, err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if f, _, _, err := ScanTree(root, terms); err == nil {
		t.Errorf("an unreadable file cannot be called clean: ScanTree must return an error (findings %v)", f)
	}
}

// Mutant (S2): the path scan made case-sensitive (strings.ToLower(rel) dropped), which the
// fixtures could not see because the term and the file name were both lower-case. A file named
// Notes-ZZCorp.md went unflagged. The content scan has the same property in scanBytes
// (bytes.ToLower(data)), pinned by the second half.
func TestScanTree_FindsATermWhateverItsCaseInANameOrInContent(t *testing.T) {
	terms := scrubterms.Terms{All: []string{fixtureTerm}}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Notes-ZZCorp.md"), []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _, _, err := ScanTree(root, terms)
	if err != nil || !hasFinding(f, "path of Notes-ZZCorp.md") {
		t.Errorf("a term in an upper-case file name must be found: %v %v", f, err)
	}

	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plain.md"), []byte("ok\nDeploys to ZZCORP staging\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _, _, err = ScanTree(root, terms)
	if err != nil || len(f) != 1 || f[0].Where != "plain.md" || f[0].Line != 2 {
		t.Errorf("a term in upper-case content must be found on its line: %v %v", f, err)
	}
}

// Mutant (S6c): in ScanHistoryTrees the decision to fetch a blob consulted the cache without the
// path's test-file scope (the key built with a fixed `false`, or with the sha alone). The cache is
// keyed by blob AND scope because the same bytes are scanned with different terms under a
// _test.go name. A blob that is first met at a non-test path and later at a _test.go path was
// then taken as already fetched, never read, and scanned as empty: a leak in the test file passed.
// The reverse order is run too, because a mutant that fixes the key the other way fails only there.
func TestScanHistoryTrees_ABlobMetUnderBothScopesInDifferentTreesIsReadForEach(t *testing.T) {
	hermetic(t)
	terms := scrubterms.Terms{All: []string{fixtureTerm}}
	leak := "package x // " + fixtureTerm + "\n"

	// git log lists the newest commit first, so the first tree scanned is the second one made.
	for name, c := range map[string]struct{ older, newer string }{
		"non-test path first, test path later": {older: "x_test.go", newer: "x.go"},
		"test path first, non-test path later": {older: "x.go", newer: "x_test.go"},
	} {
		repo := source(t, map[string]string{c.older: leak}, "one")
		git(t, repo, "mv", c.older, c.newer)
		git(t, repo, "commit", "-q", "-m", "two")

		res, err := ScanHistoryTrees(&recorder{}, repo, []string{"--all"}, terms, map[string]bool{})
		if err != nil || res.Trees != 2 {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		for _, file := range []string{c.older, c.newer} {
			found := false
			for _, f := range res.Findings {
				if strings.HasSuffix(f.Where, " · "+file) && f.Term == fixtureTerm {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: the term in %s (the same blob in the other tree) must be found: %v", name, file, res.Findings)
			}
		}
	}
}
