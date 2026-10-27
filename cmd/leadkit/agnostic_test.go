package main

// Project-agnostic regression guard (docs/build-tasks/project-agnostic-defaults.md §4):
// leadkit SOURCE must name no specific adopter/org and carry no author-machine path.
// This test greps ALL shipping Go under internal/ + cmd/ (excluding _test.go and
// generated files) for a denylist of adopter tokens and home-path fragments, and FAILS
// if any appears — so a future hardcoded name breaks CI instead of shipping. Adopter
// specifics belong in leadkit.yaml (config), never in the source.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/commentscan"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// The denylist is NOT in this file. A test that spells out "these names must never appear"
// is itself a place they appear, and publishing the repo would publish the list, so the terms
// live in a file outside the committed tree (see internal/scrubterms) and the real-tree
// ratchets SKIP, with a message, when it is absent. TestSource_TestFilesNameNoTerms is what
// keeps them out: it scans the _test.go files, this one included, which the source scan
// below deliberately does not.
//
// fixtureDenylist is the stand-in the meta-tests run against. It is synthetic on purpose and
// always available: the scanner's behaviour does not depend on which names it is hunting, so
// the meta-proofs below run on every clone, public or private.
var fixtureDenylist = []string{"acmecorp", "widget-ci", "migrate-acmecorp"}

// agnosticScanRoots are the shipping-source trees relative to the repo root.
var agnosticScanRoots = []string{"internal", "cmd"}

// scanGoSourceForTokens walks root, reading every *.go file that is NOT a _test.go and
// NOT generated, and returns one "<relpath>:<line>: <token>" finding per denylisted
// token occurrence (case-insensitive). The shared scanner behind both the real-tree
// assertion and the meta-fixture below.
func scanGoSourceForTokens(t *testing.T, root string, denylist []string) []string {
	t.Helper()
	var findings []string
	lowered := make([]string, len(denylist))
	for i, d := range denylist {
		lowered[i] = strings.ToLower(d)
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(data)
		// Skip generated files (the standard `// Code generated ... DO NOT EDIT.` marker).
		if commentscan.IsGenerated(data) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for lineNo, line := range strings.Split(text, "\n") {
			low := strings.ToLower(line)
			for _, tok := range lowered {
				if strings.Contains(low, tok) {
					findings = append(findings, rel+":"+itoa(lineNo+1)+": "+tok)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return findings
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// repoRootFromCwd walks up from the test's cwd (the package dir) to the module root
// (the dir holding go.mod).
func repoRootFromCwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test cwd")
		}
		dir = parent
	}
}

// TestSource_IsProjectAgnostic is the regression guard: no denylisted adopter token or
// author home-path fragment may appear in shipping Go (internal/ + cmd/, excluding
// _test.go and generated files). A future hardcoded name fails here. Green proves the source
// lacks the terms in the scrub term file, nothing more: a leak not on the list passes.
func TestSource_IsProjectAgnostic(t *testing.T) {
	terms := scrubterms.Require(t).Everywhere()
	repoRoot := repoRootFromCwd(t)
	var all []string
	for _, sub := range agnosticScanRoots {
		all = append(all, scanGoSourceForTokens(t, filepath.Join(repoRoot, sub), terms)...)
	}
	if len(all) > 0 {
		t.Fatalf("shipping Go source names adopter/machine specifics (must be config-driven or generic) — %d finding(s):\n%s",
			len(all), strings.Join(all, "\n"))
	}
}

// TestSource_AgnosticGuardCatchesReintroduction is the meta-proof: the scanner WOULD
// flag a denylisted token if one were reintroduced (asserted against a temp fixture),
// so a green TestSource_IsProjectAgnostic means "genuinely clean", not "scanner inert".
func TestSource_AgnosticGuardCatchesReintroduction(t *testing.T) {
	dir := t.TempDir()
	// A non-test .go file that reintroduces a denylisted token.
	if err := os.WriteFile(filepath.Join(dir, "leak.go"),
		[]byte("package x\n\n// gate for the widget-ci remote\nvar X = \"acmecorp-product\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A _test.go file with the same tokens must be IGNORED (fixtures may name things).
	if err := os.WriteFile(filepath.Join(dir, "leak_test.go"),
		[]byte("package x\n\nvar Y = \"migrate-acmecorp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := scanGoSourceForTokens(t, dir, fixtureDenylist)
	if len(findings) != 2 { // "widget-ci" + "acmecorp" on the two lines of leak.go
		t.Fatalf("meta-guard: want 2 findings from leak.go only (the _test.go is ignored), got %d: %v", len(findings), findings)
	}
	for _, f := range findings {
		if strings.HasPrefix(f, "leak_test.go") {
			t.Errorf("meta-guard must ignore _test.go fixtures, but flagged %q", f)
		}
	}
}
