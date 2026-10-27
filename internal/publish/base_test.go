package publish

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/scrubterms"
)

const lateTerm = "zzlate"

// The case this tool exists for, at the second release. Release 1 shipped a line that was not yet
// on the list; the owner learns the term, adds it, removes the line from the source, and runs the
// update. The current tree is clean and no commit message mentions it, but a delta cannot remove
// that file, so the public history leaks for good. A gate that reads only the current tree and the
// messages prints CLEAN for all of it.
func TestPrepare_UpdateSearchesTheFilesOfEveryPastRelease(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "#!/bin/sh\n# deploys to " + lateTerm + " staging\n"
	repo := source(t, files, "msg")
	pub := publicClone(t, repo) // the term is not on the list yet

	// The owner learns the term, and the source drops the line.
	writeFiles(t, repo, map[string]string{"scripts/build.sh": "#!/bin/sh\necho build\n"})
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "remove it")
	head := git(t, pub, "rev-parse", "HEAD")

	d := deps(t, nil)
	d.Terms.Path = termsFile(t, fixtureTerm+"\n"+lateTerm+"\n")
	rep, err := Prepare(Options{Repo: repo, Base: pub}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.TreeFindings) != 0 {
		t.Fatalf("the current tree is clean (that is the trap): %v", rep.TreeFindings)
	}
	b := rep.Built
	if rep.OK() || b == nil || len(b.HistoryFindings) == 0 {
		t.Fatalf("a term in an earlier release's files must refuse: %+v", b)
	}
	if !strings.Contains(b.HistoryFindings[0].Where, "scripts/build.sh") || !strings.Contains(b.HistoryFindings[0].Where, "commit ") {
		t.Errorf("the finding must name the commit and the file: %v", b.HistoryFindings[0])
	}
	if b.Commit != "" || b.Undone != "" {
		t.Errorf("the refusal comes before anything is written: %+v", b)
	}
	if git(t, pub, "rev-parse", "HEAD") != head || git(t, pub, "status", "--porcelain") != "" {
		t.Error("the public checkout must be untouched")
	}
	var out strings.Builder
	rep.Write(&out)
	if !strings.Contains(out.String(), "NOT READY") || !strings.Contains(out.String(), "rewrite the published history") {
		t.Errorf("the report must refuse and name the dead end:\n%s", out.String())
	}

	// Control: the same history with a list that does not hold the term proceeds, and says how
	// many trees it searched.
	rep, err = Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("control: %v %+v", err, rep.Built)
	}
	if rep.Built.TreesSearched < 2 {
		t.Errorf("TreesSearched = %d, want the past tree(s) and the new one", rep.Built.TreesSearched)
	}
	out.Reset()
	rep.Write(&out)
	if !strings.Contains(out.String(), "the files of 2 distinct tree(s)") {
		t.Errorf("the history line must say what was searched:\n%s", out.String())
	}
}

// A release is a full overwrite. A file that exists only in the public checkout is deleted, and
// the report must say so, by name, rather than leave a merged contribution to vanish silently.
func TestPrepare_UpdateNamesWhatItWillDeleteFromThePublicSide(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	writeFiles(t, pub, map[string]string{"LICENSE": "public-only\n", "ci/ci.yml": "public-only\n"})
	git(t, pub, "add", "-A")
	git(t, pub, "commit", "-q", "-m", "add public-only files")

	nextRelease(t, repo, map[string]string{"added.go": "package added\n", "main.go": "package main // v2\n"})
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || !rep.OK() || rep.Built == nil {
		t.Fatalf("update: %v %+v", err, rep.Built)
	}
	b := rep.Built
	if b.Added != 1 || b.Changed != 1 || strings.Join(b.Removed, ",") != "LICENSE,ci/ci.yml" {
		t.Errorf("added/changed/removed = %d/%d/%v, want 1/1/[LICENSE ci/ci.yml]", b.Added, b.Changed, b.Removed)
	}
	// The behaviour is kept: the public tree equals the source.
	if _, err := os.Stat(filepath.Join(pub, "LICENSE")); err == nil {
		t.Error("the release makes the tree equal the source, so the public-only file is gone")
	}
	var out strings.Builder
	rep.Write(&out)
	for _, want := range []string{"EQUAL the source tree: 1 added, 1 changed, 2 removed", "WARNING: 2 file(s)", "LICENSE", "ci/ci.yml", "reverted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	// Control: with nothing public-only there is no warning.
	nextRelease(t, repo, map[string]string{"more.go": "package more\n"})
	rep, err = Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || !rep.OK() || len(rep.Built.Removed) != 0 {
		t.Fatalf("control: %v %+v", err, rep.Built)
	}
	out.Reset()
	rep.Write(&out)
	if strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "1 added, 0 changed, 0 removed") {
		t.Errorf("no public-only files, so no warning:\n%s", out.String())
	}
}

func TestReport_TheBoundNamesAnUnlistableName(t *testing.T) {
	if !strings.Contains(MatchBound, "mergetag") {
		t.Errorf("a signed tag merged into a commit is not searched, and the bound must say so: %s", MatchBound)
	}
	if !strings.Contains(MatchBound, "not past releases") {
		t.Errorf("files in past trees are not listed as non-text, and the bound must say so: %s", MatchBound)
	}
	if !strings.Contains(MatchBound, "outside the term alphabet") || !strings.Contains(MatchBound, "check them by hand") {
		t.Errorf("a name that could not be listed is a silent hole unless the bound says so: %s", MatchBound)
	}
}

// Only a change of the executable bit: still a changed file, and the new mode must reach the release.
func TestPrepare_UpdateCountsAModeOnlyChangeAndSearchesEachTreeOnce(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	pastTree := git(t, pub, "rev-parse", "HEAD^{tree}")

	if err := os.Chmod(filepath.Join(repo, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "mode only")

	rec := &recorder{}
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, rec))
	if err != nil || !rep.OK() {
		t.Fatalf("update: %v %+v", err, rep.Built)
	}
	if rep.Built.Changed != 1 || rep.Built.Added != 0 || len(rep.Built.Removed) != 0 {
		t.Errorf("added/changed/removed = %d/%d/%v, want 0/1/[]", rep.Built.Added, rep.Built.Changed, rep.Built.Removed)
	}
	listings := 0
	for _, argv := range rec.calls {
		if len(argv) > 4 && argv[3] == "ls-tree" && argv[len(argv)-1] == pastTree {
			listings++
		}
	}
	if listings != 1 {
		t.Errorf("the past tree was read %d times, want exactly 1 (it is searched once and remembered)", listings)
	}
}

// publicSideCommit commits files directly in the public checkout, as a contribution would be.
func publicSideCommit(t *testing.T, pub string, files map[string]string) {
	t.Helper()
	writeFiles(t, pub, files)
	git(t, pub, "add", "-A")
	git(t, pub, "commit", "-q", "-m", "a contribution")
}

// `git archive` honours the tree's own .gitattributes, so a file marked export-ignore is omitted
// from it while really existing in public history. The search of past releases must read the
// blobs, not the export, or this file is never looked at and the report says CLEAN.
func TestPrepare_UpdateFindsATermInAnExportIgnoredFileOfAPastRelease(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	publicSideCommit(t, pub, map[string]string{
		".gitattributes":  "hidden/leak.txt export-ignore\n",
		"hidden/leak.txt": "deploys to " + fixtureTerm + "\n",
	})

	// The fixture is the case it claims to be: the export omits the file.
	exported, err := exec.Command("git", "-C", pub, "archive", "--format=tar", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	for tr := tar.NewReader(bytes.NewReader(exported)); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "hidden/leak.txt" {
			t.Fatal("fixture: git archive must omit an export-ignore file")
		}
	}
	head := git(t, pub, "rev-parse", "HEAD")

	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	b := rep.Built
	if rep.OK() || b == nil || len(b.HistoryFindings) == 0 {
		t.Fatalf("a term in an export-ignored file of a past release must refuse: %+v", b)
	}
	if w := b.HistoryFindings[0].Where; !strings.Contains(w, "hidden/leak.txt") || !strings.Contains(w, "commit ") {
		t.Errorf("the finding must name the commit and the file: %v", b.HistoryFindings[0])
	}
	if b.Commit != "" || git(t, pub, "rev-parse", "HEAD") != head {
		t.Error("the refusal comes before anything is written")
	}

	// Control: the same history without the term proceeds.
	pub2 := publicClone(t, source(t, cleanFiles(), "msg"))
	publicSideCommit(t, pub2, map[string]string{".gitattributes": "hidden/ok.txt export-ignore\n", "hidden/ok.txt": "harmless\n"})
	repo2 := source(t, cleanFiles(), "msg")
	nextRelease(t, repo2, map[string]string{"added.go": "package added\n"})
	if rep, err := Prepare(Options{Repo: repo2, Base: pub2}, deps(t, nil)); err != nil || !rep.OK() {
		t.Fatalf("control: %v %+v", err, rep.Built)
	}
}

// A past symlink's target is its blob content, and a past _test.go keeps the src: exemption.
func TestPrepare_UpdateSearchesPastSymlinkTargetsAndKeepsTheTestFileScope(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")

	pub := publicClone(t, repo)
	publicSideCommit(t, pub, map[string]string{"ok_test.go": "package x // " + fixtureScoped + "\n"})
	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	if rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil)); err != nil || !rep.OK() {
		t.Fatalf("a src: term in a past _test.go is a placeholder: %v %+v", err, rep.Built)
	}

	repo = source(t, cleanFiles(), "msg")
	pub = publicClone(t, repo)
	// The same content under both names shares one blob: the scope is per PATH, not per blob.
	same := "package x // " + fixtureScoped + "\n"
	publicSideCommit(t, pub, map[string]string{"bad.go": same, "bad_test.go": same})
	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || rep.OK() || rep.Built == nil {
		t.Fatalf("a src: term in a past non-test file is a leak: %v %+v", err, rep.Built)
	}
	if !hasFinding(rep.Built.HistoryFindings, "commit "+short(git(t, pub, "rev-parse", "HEAD"))+" · bad.go") {
		t.Errorf("bad.go must be found: %v", rep.Built.HistoryFindings)
	}
	for _, f := range rep.Built.HistoryFindings {
		if strings.Contains(f.Where, "bad_test.go") {
			t.Errorf("a test file keeps the src: exemption even when its blob is shared: %v", f)
		}
	}

	repo = source(t, cleanFiles(), "msg")
	pub = publicClone(t, repo)
	if err := os.Symlink("/home/"+fixtureTerm+"/work", filepath.Join(pub, "link")); err != nil {
		t.Fatal(err)
	}
	git(t, pub, "add", "-A")
	git(t, pub, "commit", "-q", "-m", "a link")
	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	rep, err = Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || rep.OK() || rep.Built == nil || !strings.Contains(rep.Built.HistoryFindings[0].Where, "link (symlink target)") {
		t.Fatalf("a term in a past symlink's target must be found: %v %+v", err, rep.Built)
	}

	repo = source(t, cleanFiles(), "msg")
	pub = publicClone(t, repo)
	publicSideCommit(t, pub, map[string]string{"notes-" + fixtureTerm + ".md": "plain\n"})
	nextRelease(t, repo, map[string]string{"added.go": "package added\n"})
	rep, err = Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil || rep.OK() || rep.Built == nil || !strings.Contains(rep.Built.HistoryFindings[0].Where, "path of notes-"+fixtureTerm+".md") {
		t.Fatalf("a term in a past file NAME must be found: %v %+v", err, rep.Built)
	}
}

// A history that cannot be read cannot be called clean.
func TestReadBlobs_RefusesWhatItCannotRead(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for name, out := range map[string]string{
		"missing object": a + " missing\n",
		"wrong object":   b + " blob 1\nx\n",
		"not a blob":     a + " tree 1\nx\n",
		"bad size":       a + " blob nope\n",
		"truncated":      a + " blob 9\nxy",
		"no terminator":  a + " blob 3\nabc",
		"empty output":   "",
	} {
		_, err := readBlobs(&fakeGH{body: out}, "/r", []string{a})
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	got, err := readBlobs(&fakeGH{body: a + " blob 3\nabc\n" + b + " blob 0\n\n"}, "/r", []string{a, b})
	if err != nil || string(got[a]) != "abc" || len(got[b]) != 0 {
		t.Errorf("control: %v %q", err, got)
	}
}

// A submodule entry is a commit id in another repository: there is no content to read here, and
// asking for it would fail the whole search.
func TestScanHistoryTrees_SkipsSubmoduleEntries(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	git(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+git(t, repo, "rev-parse", "HEAD")+",vendor/sub")
	git(t, repo, "commit", "-q", "-m", "a submodule")
	terms, err := scrubterms.Parse(termFileText)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ScanHistoryTrees(&recorder{}, repo, []string{"--all"}, terms, map[string]bool{})
	if err != nil || res.Trees != 2 {
		t.Fatalf("a gitlink must be skipped, not read: %v %+v", err, res)
	}
	for _, f := range res.Findings {
		if strings.Contains(f.Where, "vendor") {
			t.Errorf("a gitlink has no content to match: %v", f)
		}
	}
}
