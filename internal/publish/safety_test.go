package publish

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/scrubterms"
)

// baseWithIgnored makes a public checkout that holds a file its own rules ignore, the way an
// editor's or the OS's litter does, and returns it with the source repository.
func baseWithIgnored(t *testing.T, rule, name, content string) (repo, pub string) {
	t.Helper()
	files := cleanFiles()
	files[".gitignore"] = rule + "\n"
	repo = source(t, files, "msg")
	pub = publicClone(t, repo)
	writeFiles(t, pub, map[string]string{name: content})
	if got := git(t, pub, "status", "--porcelain"); got != "" {
		t.Fatalf("the fixture must look clean to a plain status (that is the trap): %q", got)
	}
	return repo, pub
}

func nextRelease(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	writeFiles(t, repo, files)
	git(t, repo, "add", "-A", "-f") // the source tracks it despite the rule, as a release may
	git(t, repo, "commit", "-q", "-m", "more")
}

// The three ways an ignored file in a --base checkout can be lost or abused. The checkout is
// refused up front, before anything is written, and what it held is exactly as it was.
func TestPrepare_UpdateRefusesACheckoutHoldingIgnoredFiles(t *testing.T) {
	hermetic(t)

	// (a) The release drops the rule that hid the file. A rollback would delete it.
	repo, pub := baseWithIgnored(t, "notes.txt", "notes.txt", "MINE\n")
	git(t, repo, "rm", "-q", ".gitignore")
	git(t, repo, "commit", "-q", "-m", "drop the rule")
	head := git(t, pub, "rev-parse", "HEAD")
	_, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("a checkout holding an ignored file must be refused, naming it: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(pub, "notes.txt")); string(data) != "MINE\n" {
		t.Errorf("(a) the ignored file must survive: %q", data)
	}
	if git(t, pub, "rev-parse", "HEAD") != head {
		t.Error("(a) nothing may be committed")
	}

	// (b) The release carries a file with the same name as an ignored local one.
	repo, pub = baseWithIgnored(t, "local.env", "local.env", "PRECIOUS-LOCAL\n")
	nextRelease(t, repo, map[string]string{"local.env": "from the release\n"})
	if _, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil)); err == nil {
		t.Fatal("(b) must be refused")
	}
	if data, _ := os.ReadFile(filepath.Join(pub, "local.env")); string(data) != "PRECIOUS-LOCAL\n" {
		t.Errorf("(b) the ignored file must not be overwritten: %q", data)
	}

	// (c) An ignored symlink at a path the release writes must not carry the write out.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, pub = baseWithIgnored(t, "link.txt", "link.txt", "x")
	if err := os.Remove(filepath.Join(pub, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pub, "link.txt")); err != nil {
		t.Fatal(err)
	}
	nextRelease(t, repo, map[string]string{"link.txt": "from the release\n"})
	if _, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil)); err == nil {
		t.Fatal("(c) must be refused")
	}
	if data, _ := os.ReadFile(outside); string(data) != "OUTSIDE\n" {
		t.Errorf("(c) a file outside the checkout was written through a link: %q", data)
	}
	if _, err := os.Lstat(filepath.Join(pub, "link.txt")); err != nil {
		t.Errorf("(c) the owner's link must survive: %v", err)
	}
}

// copyTree is the second line behind the status check: even with a file in the way it must not
// write over it, or through a link.
func TestCopyTree_NeverWritesOverAnything(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"a/b.txt": "new\n", "c.txt": "new\n"})

	dst := t.TempDir()
	writeFiles(t, dst, map[string]string{"c.txt": "old\n"})
	if err := copyTree(src, dst); err == nil {
		t.Error("an existing file at a target must be refused")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "c.txt")); string(data) != "old\n" {
		t.Errorf("the existing file was changed: %q", data)
	}

	dst = t.TempDir()
	outside := filepath.Join(t.TempDir(), "o.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err == nil {
		t.Error("a symlink at a target must be refused, not followed")
	}
	if data, _ := os.ReadFile(outside); string(data) != "OUTSIDE\n" {
		t.Errorf("a write followed a symlink out of the destination: %q", data)
	}

	dst = t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dst, "a")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err == nil {
		t.Error("a symlink where a directory belongs must be refused")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("files were written through a directory symlink: %v", entries)
	}

	if err := copyTree(src, t.TempDir()); err != nil {
		t.Errorf("an empty destination must work (the control): %v", err)
	}
}

// `check` is read-only. A committed symlink in the middle of a manifest path must not carry the
// removal to a file elsewhere on the machine.
func TestCheck_AManifestPathThroughASymlinkDeletesNothingOutsideTheTree(t *testing.T) {
	hermetic(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := cleanFiles()
	files[DefaultManifest] = manifestText + "docs/ext/victim.txt\n"
	repo := source(t, files, "msg")
	if err := os.Symlink(outside, filepath.Join(repo, "docs", "ext")); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "link")

	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err == nil {
		t.Fatalf("a manifest path through a symlink must refuse: %+v", rep)
	}
	if data, rerr := os.ReadFile(victim); rerr != nil || string(data) != "keep me\n" {
		t.Fatalf("check deleted a file outside its scratch tree: %v %q", rerr, data)
	}

	// The control: the same manifest entry, with no link in the way, is removed normally.
	files = cleanFiles()
	files[DefaultManifest] = manifestText + "docs/ext/victim.txt\n"
	files["docs/ext/victim.txt"] = "inside\n"
	if rep, err := Check(Options{Repo: source(t, files, "msg")}, deps(t, nil)); err != nil || len(rep.Stale) != 0 {
		t.Errorf("an ordinary nested manifest path must work: %v %+v", err, rep.Stale)
	}
}

// A term file with a leading byte-order mark is one entry to both Parse and CountEntries, so the
// count agrees; the term can never match, and a gate that trusts the count prints CLEAN against a planted leak.
func TestCheck_ATermFileWithABOMRefusesInsteadOfReportingClean(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "#!/bin/sh\n# " + fixtureTerm + "\n"
	repo := source(t, files, "msg")

	d := deps(t, nil)
	d.Terms.Path = termsFile(t, "\ufeff"+fixtureTerm+"\n")
	rep, err := Check(Options{Repo: repo}, d)
	if err == nil || !strings.Contains(err.Error(), "U+FEFF") {
		t.Fatalf("a BOM must refuse, naming the character: %v", err)
	}
	if rep.SHA != "" || rep.TermCount != 0 || len(rep.TreeFindings) != 0 {
		t.Errorf("a refusal returns no report to read: %+v", rep)
	}

	// The control: the same list without the mark finds the planted term.
	d.Terms.Path = termsFile(t, fixtureTerm+"\n")
	rep, err = Check(Options{Repo: repo}, d)
	if err != nil || rep.OK() || len(rep.TreeFindings) != 1 {
		t.Fatalf("without the BOM the leak must be found: %v %+v", err, rep.TreeFindings)
	}

	d.Terms.Path = termsFile(t, fixtureTerm+" // the staging cluster\n")
	if _, err := Check(Options{Repo: repo}, d); err == nil {
		t.Error("an inline comment makes a term that never matches; it must refuse")
	}

	d.Terms.Path = filepath.Join(t.TempDir(), "typo.txt")
	if _, err := Check(Options{Repo: repo}, d); err == nil {
		t.Error("an explicit terms path that does not exist must refuse")
	}
}

// The wiring of the GitHub runner is only observable here: scrubterms builds the one gh command,
// so the guard on it is checked at the place that applies it.
func TestTermOptions_GuardsTheGitHubRunner(t *testing.T) {
	gh := &fakeGH{}
	o := termOptions(Deps{GH: gh, Terms: scrubterms.ResolveOptions{Repo: "o/r"}})
	if o.Runner == nil {
		t.Fatal("a GitHub runner was supplied and must be used")
	}
	for _, argv := range [][]string{{"gh", "repo", "create", "x"}, {"gh", "variable", "set", "X"}, {"git", "push"}} {
		if _, _, err := o.Runner.Run(argv, nil); err == nil {
			t.Errorf("%v reached the GitHub runner", argv)
		}
	}
	if len(gh.calls) != 0 {
		t.Fatalf("refused commands reached the executor: %v", gh.calls)
	}
	if _, _, err := o.Runner.Run([]string{"gh", "variable", "get", "LEADKIT_SCRUB_TERMS"}, nil); err != nil || len(gh.calls) != 1 {
		t.Errorf("reading the variable must pass through (the control): %v %v", err, gh.calls)
	}
	if o.Repo != "o/r" {
		t.Errorf("termOptions dropped the caller's options: %+v", o)
	}
	if termOptions(Deps{}).Runner != nil {
		t.Error("with no GitHub runner, none must be invented")
	}
}

func TestPrepare_AnExistingEmptyOutDirectoryIsKeptWhenTheReleaseFails(t *testing.T) {
	hermetic(t)
	out := t.TempDir() // the caller made it; the tool only fills it
	rep, err := Prepare(Options{Repo: source(t, cleanFiles(), "msg"), Out: out, Message: "for " + fixtureTerm}, deps(t, nil))
	if err != nil || rep.OK() {
		t.Fatalf("the leaking release must fail: %v", err)
	}
	if entries, rerr := os.ReadDir(out); rerr != nil || len(entries) != 0 {
		t.Errorf("the directory must remain, emptied: %v %v", rerr, entries)
	}
}

// A look-alike or invisible character inside a term makes a term that matches nothing. Each of
// these printed CLEAN against a tree that really leaks when the check was a deny-list.
func TestCheck_ATermWithANonASCIICharacterRefusesInsteadOfReportingClean(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "#!/bin/sh\n# " + fixtureTerm + " " + fixtureTerm + "\n"
	repo := source(t, files, "msg")
	for name, term := range map[string]string{
		"no-break space":  "zz\u00a0corp",
		"ideographic sp":  "zz\u3000corp",
		"grapheme joiner": "zz\u034fcorp",
		"replacement chr": "zz\ufffdcorp",
		"cyrillic z":      "\u0437zcorp",
	} {
		d := deps(t, nil)
		d.Terms.Path = termsFile(t, term+"\n")
		rep, err := Check(Options{Repo: repo}, d)
		if err == nil || rep.SHA != "" {
			t.Errorf("%s: want a refusal, got err=%v %+v", name, err, rep)
		}
	}
}

func TestReport_StatesWhatAGreenDoesNotCover(t *testing.T) {
	hermetic(t)
	rep, err := Check(Options{Repo: source(t, cleanFiles(), "msg")}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	rep.Write(&out)
	for _, want := range []string{"bound:", "byte-for-byte after lowercasing", "another Unicode form", "absent from the term list", "not UTF-8 text", "encoded inside a text file"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if !rep.OK() {
		t.Fatal("control: the clean tree must still pass")
	}
}

func TestCheck_AMisspeltScopeRefusesInsteadOfReportingClean(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "#!/bin/sh\n# acme corp\n"
	repo := source(t, files, "msg")
	for _, text := range []string{"Src: acme corp\n", "src : acme corp\n"} {
		d := deps(t, nil)
		d.Terms.Path = termsFile(t, text)
		if rep, err := Check(Options{Repo: repo}, d); err == nil {
			t.Errorf("%q: a term that matches nothing must refuse, not pass: %+v", text, rep)
		}
	}
	d := deps(t, nil)
	d.Terms.Path = termsFile(t, "acme corp\n")
	if rep, err := Check(Options{Repo: repo}, d); err != nil || rep.OK() {
		t.Errorf("control: the correctly spelt term must find the leak: %v", err)
	}
}

// A UTF-16 file and a gzip file holding the term were reported CLEAN. They are named on every
// run instead, so the gap is visible where it applies.
func TestCheck_FilesThatAreNotTextAreNamedEveryRun(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	var u16 bytes.Buffer
	for _, r := range "# " + fixtureTerm + "\n" {
		u16.WriteByte(byte(r))
		u16.WriteByte(0)
	}
	files["notes/utf16.txt"] = u16.String()
	// Deflate bit-packs, so the term is usually not a plain substring of the output; pad the text
	// until it is certainly not, so the fixture is the case it claims to be.
	var gz bytes.Buffer
	for pad := 0; ; pad++ {
		gz.Reset()
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(strings.Repeat("x", pad) + " deploys to " + fixtureTerm + " staging\n"))
		_ = zw.Close()
		if !bytes.Contains(bytes.ToLower(gz.Bytes()), []byte(fixtureTerm)) {
			break
		}
		if pad > 64 {
			t.Fatal("could not build a gzip file that hides the term")
		}
	}
	files["data/archive.gz"] = gz.String()
	files["notes/latin1.txt"] = "caf\xe9 " + fixtureTerm[:3] + "\n" // invalid UTF-8, no NUL
	repo := source(t, files, "msg")

	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("the term is hidden by the encoding, so the search is clean (that is the gap): %v", rep.TreeFindings)
	}
	if strings.Join(rep.NotText, ",") != "data/archive.gz,notes/latin1.txt,notes/utf16.txt" {
		t.Fatalf("NotText = %v, want both files named", rep.NotText)
	}
	var out strings.Builder
	rep.Write(&out)
	for _, want := range []string{"not searched as text: 3 file(s)", "data/archive.gz", "notes/latin1.txt", "notes/utf16.txt"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	// Control: an all-text tree says so, rather than saying nothing.
	rep, err = Check(Options{Repo: source(t, cleanFiles(), "msg")}, deps(t, nil))
	if err != nil || len(rep.NotText) != 0 {
		t.Fatalf("a text tree names nothing: %v %v", err, rep.NotText)
	}
	out.Reset()
	rep.Write(&out)
	if !strings.Contains(out.String(), "not searched as text: none") {
		t.Errorf("the line must be printed even when empty:\n%s", out.String())
	}
}

// Each is a valid, printable-ASCII, correctly parsed entry that is not what its author meant, and
// each printed CLEAN against a tree holding the term.
func TestCheck_AnEntryOutsideTheAlphabetRefusesInsteadOfReportingClean(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "#!/bin/sh\n# acmecorp and acme.com\n"
	repo := source(t, files, "msg")
	for _, text := range []string{"\"acmecorp\"", "acmecorp,", "`acmecorp`", "acme.*corp", "acme\\.com", "*acmecorp", "acme  corp", "- acmecorp"} {
		d := deps(t, nil)
		d.Terms.Path = termsFile(t, text+"\n")
		if rep, err := Check(Options{Repo: repo}, d); err == nil || rep.SHA != "" {
			t.Errorf("%q: want a refusal, got err=%v %+v", text, err, rep)
		}
	}
	d := deps(t, nil)
	d.Terms.Path = termsFile(t, "acmecorp\nacme.com\n")
	if rep, err := Check(Options{Repo: repo}, d); err != nil || len(rep.TreeFindings) != 2 {
		t.Errorf("control: the plain spellings must find both leaks: %v %+v", err, rep.TreeFindings)
	}
}
