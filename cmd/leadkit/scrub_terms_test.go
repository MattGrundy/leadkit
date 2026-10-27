package main

// The scrub terms are kept out of the committed tree (internal/scrubterms), which leaves two
// ways for the ratchets to rot silently: a test file could name a term again, and a ratchet
// could treat "no term file" as "nothing to find". This file closes both.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/commentscan"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// scanGoTestsForTokens is the counterpart of scanGoSourceForTokens for the _test.go files it
// skips, and the gap this exists to close: a ratchet that cannot see its own test file cannot
// notice that the file carries the list.
func scanGoTestsForTokens(t *testing.T, root string, denylist []string) []string {
	t.Helper()
	files, err := commentscan.Files(root, ".go")
	if err != nil {
		t.Fatalf("list %s: %v", root, err)
	}
	var findings []string
	for _, rel := range files {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			low := strings.ToLower(line)
			for _, tok := range denylist {
				if strings.Contains(low, strings.ToLower(tok)) {
					findings = append(findings, rel+":"+itoa(lineNo+1)+": "+tok)
				}
			}
		}
	}
	return findings
}

// TestSource_TestFilesNameNoTerms is the actual fix for ticket 268: the committed tests,
// including the ratchets themselves, must not name a forbidden term. Fixtures use synthetic
// names; anything that needs the real ones reads them from the external file.
func TestSource_TestFilesNameNoTerms(t *testing.T) {
	terms := scrubterms.Require(t).ForTests()
	if found := scanGoTestsForTokens(t, repoRootFromCwd(t), terms); len(found) > 0 {
		t.Fatalf("committed _test.go files name scrub terms — %d finding(s). Use a synthetic name in the "+
			"fixture, or read the real one from the scrub term file:\n%s", len(found), strings.Join(found, "\n"))
	}
}

// TestSource_TestFileScanCatchesAPlantedTerm is the meta-proof for the scan above, against a
// fixture: it flags a _test.go, and ignores a non-test file (the source scan owns those).
func TestSource_TestFileScanCatchesAPlantedTerm(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"leak_test.go":  "package x\n\nvar Y = \"acmecorp\"\n",
		"clean_test.go": "package x\n\nvar Z = 1\n",
		"shipping.go":   "package x\n\nvar W = \"acmecorp\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := scanGoTestsForTokens(t, dir, fixtureDenylist)
	if len(got) != 1 || !strings.HasPrefix(got[0], "leak_test.go:3:") {
		t.Fatalf("want exactly one finding, at leak_test.go:3; got %v", got)
	}
}

// TestScrubRatchets_SkipWhenTheTermFileIsAbsent runs the three real-tree ratchets in a child
// process with no term file and requires each to report SKIP, with the reason. A ratchet that
// returned silently, or ran on an empty list, would PASS instead and this fails: "skips" has to
// be observable, because a pass is exactly what a stood-down ratchet looks like.
func TestScrubRatchets_SkipWhenTheTermFileIsAbsent(t *testing.T) {
	ratchets := []string{
		"TestSource_IsProjectAgnostic",
		"TestSource_TestFilesNameNoTerms",
		"TestDocs_ScrubIsRatcheted",
		"TestDocs_AllowlistHasNoStaleEntries",
	}
	missing := filepath.Join(t.TempDir(), "no-such-terms.txt")
	cmd := exec.Command(os.Args[0], "-test.v", "-test.count=1", "-test.run=^("+strings.Join(ratchets, "|")+")$")
	cmd.Env = append(os.Environ(), scrubterms.EnvVar+"="+missing)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("with no term file the package must still pass: %v\n%s", err, out)
	}
	for _, name := range ratchets {
		if !strings.Contains(string(out), "--- SKIP: "+name+" ") {
			t.Errorf("%s did not SKIP with no term file:\n%s", name, out)
		}
	}
	if !strings.Contains(string(out), "no scrub term file at "+missing) {
		t.Errorf("the skip message must say which file is absent and why:\n%s", out)
	}
}

// TestScrubRatchets_FailOnAnUnusableTermFile pins the other half: a term file that exists but
// is empty must FAIL the ratchet, not skip it. Only absence stands a ratchet down.
func TestScrubRatchets_FailOnAnUnusableTermFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, []byte("// nothing here\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.v", "-test.count=1", "-test.run=^TestSource_IsProjectAgnostic$")
	cmd.Env = append(os.Environ(), scrubterms.EnvVar+"="+empty)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an empty term file must fail the ratchet, not pass or skip it:\n%s", out)
	}
	if !strings.Contains(string(out), "no terms in the file") {
		t.Errorf("the failure must say the file holds no terms:\n%s", out)
	}
}

// TestScrubRatchets_FailOnAPlantedLeakInTheRealTree runs the real ratchet with a term file that
// names something the tree does contain, and requires it to FAIL. This is the "term file present
// still ratchets" case, against the real flow end to end rather than the scanner alone.
func TestScrubRatchets_FailOnAPlantedLeakInTheRealTree(t *testing.T) {
	// "func TestSource_IsProjectAgnostic" is in this tree by construction.
	terms := filepath.Join(t.TempDir(), "terms.txt")
	if err := os.WriteFile(terms, []byte("func TestSource_IsProjectAgnostic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.v", "-test.count=1", "-test.run=^TestSource_TestFilesNameNoTerms$")
	cmd.Env = append(os.Environ(), scrubterms.EnvVar+"="+terms)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a term the tree contains must fail the ratchet:\n%s", out)
	}
	if !strings.Contains(string(out), "agnostic_test.go") {
		t.Errorf("the finding must name the offending file:\n%s", out)
	}
}
