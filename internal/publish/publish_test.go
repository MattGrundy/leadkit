package publish

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"mgpp.dev/leadkit/internal/forge"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// The fixtures use synthetic names. The real term list is never in this file: it is kept out of
// the committed tree (internal/scrubterms), and a test that spelled it out would put it back.
const (
	fixtureTerm   = "zzcorp"
	fixtureScoped = "#42"
	termFileText  = "// fixture terms\n" + fixtureTerm + "\nsrc: " + fixtureScoped + "\n"
	manifestText  = "// fixture manifest\ndocs/internal.md\nprivate-dir\n"
)

// recorder is the real git behind the Runner seam, remembering every argv. Git here is local and
// runs on temporary repositories; nothing in this package's tests reaches a forge.
type recorder struct {
	forge.ExecRunner
	calls [][]string
}

func (r *recorder) Run(argv []string, stdin []byte) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string{}, argv...))
	return r.ExecRunner.Run(argv, stdin)
}

// fakeGH answers `gh variable get` with a canned result and records the calls.
type fakeGH struct {
	body   string
	stderr string
	err    error
	calls  [][]string
}

func (f *fakeGH) Run(argv []string, _ []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{}, argv...))
	return []byte(f.body), []byte(f.stderr), f.err
}

// hermetic keeps the machine's git configuration out of the tests, and gives them no scrub terms
// of their own.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", scrubterms.EnvVar, scrubterms.TextEnvVar} {
		t.Setenv(k, "") // registers the restore
		os.Unsetenv(k)  // an empty identity variable is not the same as an unset one
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Src Dev", "-c", "user.email=src@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// source builds a repository with one commit holding files (path to content). The manifest is
// added unless the map already has one. A path ending in ".sh" is made executable.
func source(t *testing.T, files map[string]string, message string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	if _, ok := files[DefaultManifest]; !ok {
		files[DefaultManifest] = manifestText
	}
	writeFiles(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func cleanFiles() map[string]string {
	return map[string]string{
		"main.go":            "package main\n",
		"README.md":          "hello\n",
		"scripts/build.sh":   "#!/bin/sh\necho build\n",
		"docs/internal.md":   "internal only, mentions " + fixtureTerm + "\n",
		"private-dir/a.txt":  "also " + fixtureTerm + "\n",
		"private-dir/b/c.go": "package b\n",
	}
}

func termsFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "terms.txt")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func deps(t *testing.T, rec *recorder) Deps {
	t.Helper()
	if rec == nil {
		rec = &recorder{}
	}
	return Deps{
		Git:     rec,
		Terms:   scrubterms.ResolveOptions{Path: termsFile(t, termFileText)},
		Now:     func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) },
		TempDir: t.TempDir(),
	}
}

func hasFinding(fs []Finding, where string) bool {
	for _, f := range fs {
		if f.Where == where {
			return true
		}
	}
	return false
}

func TestCheck_CleanTreePasses(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "an ordinary message")
	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The discriminating control: every other test here is a variation of this one that must
	// fail, so this one must pass for them to mean anything.
	if !rep.OK() || len(rep.TreeFindings) != 0 || len(rep.Stale) != 0 {
		t.Fatalf("a clean tree must pass: %+v", rep)
	}
	if rep.TermCount != 2 {
		t.Errorf("TermCount = %d, want 2", rep.TermCount)
	}
	if want := []string{"docs/internal.md", "private-dir"}; strings.Join(rep.Excluded, ",") != strings.Join(want, ",") {
		t.Errorf("Excluded = %v, want %v", rep.Excluded, want)
	}
	// main.go, README.md, scripts/build.sh and the manifest itself.
	if rep.Files != 4 {
		t.Errorf("Files = %d, want 4 (the excluded paths must not be searched or counted)", rep.Files)
	}
	if rep.ExcludedFiles != 3 {
		t.Errorf("ExcludedFiles = %d, want 3", rep.ExcludedFiles)
	}
}

func TestCheck_TermInShellScriptFails(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	// The repo's own ratchets cover Go and prose only, so a term in a shell script passes them.
	// This check has no extension filter, and this is the case it exists for.
	files["scripts/build.sh"] = "#!/bin/sh\n# deploys to ZzCorp staging\n"
	repo := source(t, files, "msg")
	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a term in a shell script must fail")
	}
	if len(rep.TreeFindings) != 1 || rep.TreeFindings[0].Where != "scripts/build.sh" || rep.TreeFindings[0].Line != 2 {
		t.Errorf("findings = %v, want scripts/build.sh:2 (case-insensitively)", rep.TreeFindings)
	}
}

func TestCheck_ExcludedPathsAreNotSearched(t *testing.T) {
	hermetic(t)
	// cleanFiles carries the term in two excluded paths. They must not count: the exclusion
	// happens before the search. The control is the test above, where the same term elsewhere fails.
	rep, err := Check(Options{Repo: source(t, cleanFiles(), "msg")}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("a term only in excluded paths must pass: err=%v %+v", err, rep.TreeFindings)
	}
}

func TestCheck_TermOnlyInACommitMessage_FailsTheHistoryHalfAndPassesTheTreeHalf(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "fix the "+fixtureTerm+" deploy")
	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.TreeFindings) != 0 {
		t.Fatalf("the tree half must pass: %v", rep.TreeFindings)
	}
	if len(rep.SourceHistory.Findings) == 0 || rep.SourceHistory.Affected() != 1 {
		t.Fatalf("the history half must find the message: %+v", rep.SourceHistory)
	}
	if !rep.OK() {
		t.Error("source history is informational by default: a private history is expected to match")
	}
	strict, err := Check(Options{Repo: repo, RequireCleanHistory: true}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strict.OK() {
		t.Error("with RequireCleanHistory a message match must fail the run")
	}
}

func TestCheck_AuthorIdentityIsPartOfTheHistoryHalf(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	d := deps(t, nil)
	d.Terms.Path = termsFile(t, "src@example.test\n")
	rep, err := Check(Options{Repo: repo}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SourceHistory.Findings) == 0 {
		t.Fatal("an author address that matches a term must be found")
	}
}

func TestCheck_StaleManifestEntryIsReported(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files[DefaultManifest] = manifestText + "docs/renamed-away.md\n"
	rep, err := Check(Options{Repo: source(t, files, "msg")}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Stale) != 1 || rep.Stale[0] != "docs/renamed-away.md" {
		t.Fatalf("Stale = %v, want [docs/renamed-away.md]", rep.Stale)
	}
	if rep.OK() {
		t.Error("a stale manifest must fail the run: the list is no longer a list of what is excluded")
	}
	if strings.Contains(strings.Join(rep.Excluded, ","), "renamed-away") {
		t.Error("a stale entry must not also count as excluded")
	}
}

func TestCheck_TermListThatDoesNotResolveRefuses(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	rec := &recorder{}
	gh := &fakeGH{err: errors.New("exit status 1"), stderr: "HTTP 404: Not Found"}
	d := Deps{
		Git: rec, GH: gh, TempDir: t.TempDir(),
		Terms: scrubterms.ResolveOptions{Repo: "o/r"},
	}
	rep, err := Check(Options{Repo: repo}, d)
	if err == nil {
		t.Fatalf("an unresolved term list must REFUSE, not report: %+v", rep)
	}
	for _, want := range []string{"does not resolve", "scrub-terms.txt", scrubterms.TextEnvVar, "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must name every source tried; missing %q", err, want)
		}
	}
	if len(rec.calls) != 0 {
		t.Errorf("nothing may be searched before the list resolves; git ran: %v", rec.calls)
	}
	if rep.SHA != "" || rep.TermCount != 0 {
		t.Errorf("a refusal returns no report that could be read as clean: %+v", rep)
	}
}

func TestCheck_ATermCountMismatchRefuses(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	rec := &recorder{}
	d := deps(t, rec)
	// The empty `src:` entry is a line in the file that the parser drops. Reporting clean against
	// the remaining term is the 15-of-16 failure.
	d.Terms.Path = termsFile(t, fixtureTerm+"\nsrc:\n")
	_, err := Check(Options{Repo: repo}, d)
	if err == nil || !strings.Contains(err.Error(), "parsed 1 terms from 2 entries") {
		t.Fatalf("err = %v, want a refusal naming 1 of 2", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("git ran after a refusal: %v", rec.calls)
	}
}

func TestCheck_TermListFromTheGitHubVariable(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	gh := &fakeGH{body: termFileText}
	d := Deps{
		Git: &recorder{}, GH: gh, TempDir: t.TempDir(),
		Terms: scrubterms.ResolveOptions{Repo: "o/r"},
	}
	rep, err := Check(Options{Repo: repo}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.TermsSource, "GitHub variable") || rep.TermCount != 2 {
		t.Errorf("source = %q count = %d, want the GitHub variable and 2", rep.TermsSource, rep.TermCount)
	}
	if len(gh.calls) != 1 || strings.Join(gh.calls[0], " ") != "gh variable get LEADKIT_SCRUB_TERMS --repo o/r" {
		t.Errorf("gh calls = %v", gh.calls)
	}
}

func TestCheck_SrcScopedTermsAreAllowedInTestFilesOnly(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["x_test.go"] = "package main // uses " + fixtureScoped + " as a placeholder\n"
	rep, err := Check(Options{Repo: source(t, files, "msg")}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("a src: term in a _test.go file is a placeholder, not a leak: err=%v %v", err, rep.TreeFindings)
	}
	files["x.go"] = "package main // refers to " + fixtureScoped + "\n"
	rep, err = Check(Options{Repo: source(t, files, "msg")}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || !hasFinding(rep.TreeFindings, "x.go") || hasFinding(rep.TreeFindings, "x_test.go") {
		t.Fatalf("a src: term outside a test file is a leak, and only there: %v", rep.TreeFindings)
	}
	// A non-src term applies to test files too.
	files = cleanFiles()
	files["y_test.go"] = "package main // " + fixtureTerm + "\n"
	rep, _ = Check(Options{Repo: source(t, files, "msg")}, deps(t, nil))
	if rep.OK() {
		t.Error("an unscoped term in a _test.go file must fail")
	}
}

func TestCheck_TermsInANameAndInASymlinkTarget(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	if err := os.Symlink("/home/"+fixtureTerm+"/work", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"notes-" + fixtureTerm + ".md": "plain\n"})
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "more")
	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(rep.TreeFindings, "path of notes-"+fixtureTerm+".md") {
		t.Errorf("a term in a file name must be found: %v", rep.TreeFindings)
	}
	if !hasFinding(rep.TreeFindings, "link (symlink target)") {
		t.Errorf("a term in a symlink target must be found: %v", rep.TreeFindings)
	}
}

func TestCheck_ManifestProblemsRefuse(t *testing.T) {
	hermetic(t)
	for name, text := range map[string]string{
		"glob":     "docs/*.md\n",
		"climbing": "../outside\n",
		"absolute": "/etc/passwd\n",
		"git dir":  ".git/config\n",
		"empty":    "// nothing listed\n",
	} {
		files := cleanFiles()
		files[DefaultManifest] = text
		if _, err := Check(Options{Repo: source(t, files, "msg")}, deps(t, nil)); err == nil {
			t.Errorf("%s: a manifest the tool cannot honour must refuse", name)
		}
	}
	files := cleanFiles()
	delete(files, DefaultManifest)
	repo := source(t, files, "msg")
	git(t, repo, "rm", "-q", DefaultManifest)
	git(t, repo, "commit", "-q", "-m", "drop manifest")
	if _, err := Check(Options{Repo: repo}, deps(t, nil)); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("a missing manifest must refuse, naming it: %v", err)
	}
}

func TestCheck_ChecksTheRefNotTheWorkingTree(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	writeFiles(t, repo, map[string]string{"scratch.sh": "# " + fixtureTerm + "\n"}) // uncommitted
	rep, err := Check(Options{Repo: repo}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("an uncommitted file is not in the ref and must not be searched: %v %v", err, rep.TreeFindings)
	}
	if _, err := Check(Options{Repo: repo, Ref: "--output=x"}, deps(t, nil)); err == nil {
		t.Error("a ref that looks like an option must refuse")
	}
	if _, err := Check(Options{Repo: repo, Ref: "no-such-ref"}, deps(t, nil)); err == nil {
		t.Error("an unknown ref must refuse")
	}
}

func fileList(t *testing.T, dir string) []string {
	t.Helper()
	out := git(t, dir, "ls-files")
	l := strings.Split(out, "\n")
	sort.Strings(l)
	return l
}

func TestPrepare_FirstReleaseIsOneFreshCommit(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "fix the "+fixtureTerm+" deploy") // history is unclean, as it is for real
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "a second private commit")
	out := filepath.Join(t.TempDir(), "public")
	rec := &recorder{}
	rep, err := Prepare(Options{Repo: repo, Out: out}, deps(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Built == nil {
		t.Fatalf("a clean tree must be prepared: %+v", rep)
	}
	if rep.Built.Commits != 1 || git(t, out, "rev-list", "--count", "--all") != "1" {
		t.Errorf("the public repository must be ONE commit, not the private history: %+v", rep.Built)
	}
	if got := git(t, out, "log", "-1", "--format=%s%n%an <%ae>%n%cn <%ce>"); got != "Initial public release\nleadkit publish <publish@localhost.invalid>\nleadkit publish <publish@localhost.invalid>" {
		t.Errorf("message and identity must be generated, never taken from the source: %q", got)
	}
	want := []string{"README.md", DefaultManifest, "main.go", "scripts/build.sh"}
	sort.Strings(want)
	if got := fileList(t, out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("committed files = %v, want %v", got, want)
	}
	if fi, err := os.Stat(filepath.Join(out, "scripts/build.sh")); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("the executable bit must survive: %v %v", fi, err)
	}
	if hooks, _ := os.ReadDir(filepath.Join(out, ".git", "hooks")); len(hooks) != 0 {
		t.Errorf("the new repository must not inherit hook templates: %v", hooks)
	}
	if git(t, out, "status", "--porcelain") != "" {
		t.Error("the working tree must match the commit")
	}
	var text strings.Builder
	rep.Write(&text)
	for _, want := range []string{"NOT PUBLISHED", "tree: CLEAN in the committed tree", "history: CLEAN", "informational"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, text.String())
		}
	}
	assertOnlyLocalCommands(t, rec)
}

// assertOnlyLocalCommands is the behavioural half of the "never creates or pushes" pin: whatever
// path ran, the argv that reached the executor holds nothing that talks to a remote.
func assertOnlyLocalCommands(t *testing.T, rec *recorder) {
	t.Helper()
	if len(rec.calls) == 0 {
		t.Fatal("recorded no commands; the assertion would be vacuous")
	}
	for _, argv := range rec.calls {
		if err := Permitted(argv); err != nil {
			t.Errorf("a command outside the allow-list reached the executor: %v", argv)
		}
		for _, a := range argv {
			switch a {
			case "push", "remote", "fetch", "pull", "clone", "create", "fork", "rename":
				t.Errorf("forbidden verb %q in %v", a, argv)
			}
		}
	}
}

func TestPrepare_RefusesWithoutWritingWhenTheTreeIsNotClean(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	files["scripts/build.sh"] = "# " + fixtureTerm + "\n"
	out := filepath.Join(t.TempDir(), "public")
	rep, err := Prepare(Options{Repo: source(t, files, "msg"), Out: out}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built != nil {
		t.Fatalf("a leaking tree must not be built: %+v", rep)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("nothing may be written for a tree that did not pass: %v", err)
	}

	// A stale manifest also stops the build.
	files = cleanFiles()
	files[DefaultManifest] = manifestText + "gone.md\n"
	rep, _ = Prepare(Options{Repo: source(t, files, "msg"), Out: out}, deps(t, nil))
	if rep.OK() || rep.Built != nil {
		t.Errorf("a stale manifest must not be built: %+v", rep)
	}
}

func TestPrepare_ATermInTheMessageFailsTheHistoryHalfInsideTheNewRepository(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	out := filepath.Join(t.TempDir(), "public")
	rep, err := Prepare(Options{Repo: repo, Out: out, Message: "release for " + fixtureTerm}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	b := rep.Built
	if rep.OK() || b == nil {
		t.Fatalf("a leaking message must fail: %+v", rep)
	}
	if len(b.TreeFindings) != 0 {
		t.Errorf("the tree half passes: %v", b.TreeFindings)
	}
	if len(b.HistoryFindings) == 0 {
		t.Error("the history half, run inside the new repository, must find the message")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) || b.Undone == "" {
		t.Errorf("a repository that failed its re-check must not be left to be published: %v %q", err, b.Undone)
	}
}

func TestPrepare_AnIdentityTheToolDidNotSetFails(t *testing.T) {
	hermetic(t)
	// Environment overrides git config, so a private identity could ride along unseen.
	t.Setenv("GIT_COMMITTER_NAME", "Someone Private")
	t.Setenv("GIT_COMMITTER_EMAIL", "private@example.test")
	out := filepath.Join(t.TempDir(), "public")
	rep, err := Prepare(Options{Repo: source(t, cleanFiles(), "msg"), Out: out}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built == nil || len(rep.Built.Problems) == 0 {
		t.Fatalf("a committer the tool did not choose must fail the re-check: %+v", rep.Built)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Error("the failed repository must be removed")
	}
}

func TestPrepare_RefusesBadTargets(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	taken := t.TempDir()
	if err := os.WriteFile(filepath.Join(taken, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"neither out nor base": {Repo: repo},
		"both":                 {Repo: repo, Out: filepath.Join(t.TempDir(), "o"), Base: t.TempDir()},
		"out not empty":        {Repo: repo, Out: taken},
		"out inside the repo":  {Repo: repo, Out: filepath.Join(repo, "public")},
		"base is not a repo":   {Repo: repo, Base: t.TempDir()},
		"base is the source":   {Repo: repo, Base: repo},
	} {
		if rep, err := Prepare(o, deps(t, nil)); err == nil {
			t.Errorf("%s: want a refusal, got %+v", name, rep)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "public")); err == nil {
		t.Error("a refused --out inside the repository must not have been created")
	}
}

// publicClone prepares a first release and returns the directory, standing in for a human's
// checkout of the public repository.
func publicClone(t *testing.T, repo string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "public")
	rep, err := Prepare(Options{Repo: repo, Out: out}, deps(t, nil))
	if err != nil || !rep.OK() {
		t.Fatalf("first release: %v %+v", err, rep)
	}
	return out
}

func TestPrepare_SecondReleaseIsADeltaOnTopOfPublicHistory(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	first := git(t, pub, "rev-parse", "HEAD")

	git(t, repo, "rm", "-q", "README.md")
	writeFiles(t, repo, map[string]string{"added.go": "package added\n", "main.go": "package main // v2\n"})
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a private message that must never be read into the release")

	rec := &recorder{}
	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Built == nil {
		t.Fatalf("update: %+v", rep)
	}
	if git(t, pub, "rev-list", "--count", "--all") != "2" || git(t, pub, "rev-parse", "HEAD~1") != first {
		t.Error("an update adds ONE commit on top of the existing public history and leaves it alone")
	}
	if got := git(t, pub, "log", "-1", "--format=%s"); got != "Release 2026-10-17" {
		t.Errorf("message = %q, want a generated one", got)
	}
	want := []string{DefaultManifest, "added.go", "main.go", "scripts/build.sh"}
	sort.Strings(want)
	if got := fileList(t, pub); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v (README.md deleted, added.go new)", got, want)
	}
	if data, _ := os.ReadFile(filepath.Join(pub, "main.go")); !strings.Contains(string(data), "v2") {
		t.Error("changed content must carry over")
	}
	if git(t, pub, "status", "--porcelain") != "" {
		t.Error("the checkout must be clean after the commit")
	}
	assertOnlyLocalCommands(t, rec)
}

func TestPrepare_UpdateRefusals(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")

	pub := publicClone(t, repo)
	writeFiles(t, pub, map[string]string{"stray.txt": "uncommitted\n"})
	if _, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil)); err == nil || !strings.Contains(err.Error(), "stray.txt") {
		t.Errorf("a dirty base must refuse so a rollback cannot destroy work: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pub, "stray.txt")); err != nil {
		t.Error("the refusal must leave the checkout as it was")
	}

	pub = publicClone(t, repo)
	if _, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil)); err == nil || !strings.Contains(err.Error(), "nothing to release") {
		t.Errorf("an unchanged tree has nothing to release: %v", err)
	}
	if git(t, pub, "rev-list", "--count", "--all") != "1" || git(t, pub, "status", "--porcelain") != "" {
		t.Error("a refused update must leave the base exactly as it was")
	}
}

func TestPrepare_UpdateRefusesToStackOnAnUncleanPublicHistory(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	git(t, pub, "commit", "-q", "--allow-empty", "-m", "mentions "+fixtureTerm)
	head := git(t, pub, "rev-parse", "HEAD")
	writeFiles(t, repo, map[string]string{"added.go": "package added\n"})
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "more")

	rep, err := Prepare(Options{Repo: repo, Base: pub}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built == nil || len(rep.Built.HistoryFindings) == 0 {
		t.Fatalf("an unclean public history must fail: %+v", rep.Built)
	}
	if rep.Built.Commit != "" || rep.Built.Undone != "" {
		t.Errorf("the refusal comes BEFORE anything is written, not after a rollback: %+v", rep.Built)
	}
	if git(t, pub, "rev-parse", "HEAD") != head {
		t.Error("nothing may be committed on top of an unclean history")
	}
	if _, err := os.Stat(filepath.Join(pub, "added.go")); err == nil {
		t.Error("nothing may be written into the checkout either")
	}
}

func TestPrepare_UpdateRollsBackAReleaseThatFailsItsRecheck(t *testing.T) {
	hermetic(t)
	repo := source(t, cleanFiles(), "msg")
	pub := publicClone(t, repo)
	head := git(t, pub, "rev-parse", "HEAD")
	writeFiles(t, repo, map[string]string{"added.go": "package added\n"})
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "more")

	rep, err := Prepare(Options{Repo: repo, Base: pub, Message: "for " + fixtureTerm}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built == nil || len(rep.Built.HistoryFindings) == 0 || rep.Built.Undone == "" {
		t.Fatalf("the leaking release must fail and be undone: %+v", rep.Built)
	}
	if git(t, pub, "rev-parse", "HEAD") != head || git(t, pub, "status", "--porcelain") != "" {
		t.Error("the checkout must be back at the commit it started on, with nothing left over")
	}
	if _, err := os.Stat(filepath.Join(pub, "added.go")); err == nil {
		t.Error("the new file must be gone after the rollback")
	}
}

func TestPrepare_ACommitThatIsNotTheCheckedTreeFails(t *testing.T) {
	hermetic(t)
	files := cleanFiles()
	// A file the tree's own ignore rules would drop from a plain `git add`. Without the
	// comparison of the committed tree to the checked one, the release would silently lack it.
	files[".gitignore"] = "*.log\n"
	files["keep.log"] = "tracked in the source despite the rule\n"
	repo := source(t, files, "msg")
	git(t, repo, "add", "-f", "keep.log")
	git(t, repo, "commit", "-q", "-m", "force-tracked")
	out := filepath.Join(t.TempDir(), "public")
	rep, err := Prepare(Options{Repo: repo, Out: out}, deps(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || rep.Built == nil || !strings.Contains(strings.Join(rep.Built.Problems, "\n"), "keep.log") {
		t.Fatalf("a committed tree that lacks a checked file must fail, naming it: %+v", rep.Built)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Error("the incomplete repository must be removed")
	}
}
