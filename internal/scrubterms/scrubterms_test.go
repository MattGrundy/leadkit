package scrubterms

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse_CommentsBlanksScopeAndCase(t *testing.T) {
	got, err := Parse("// a comment\n\n  Acme  \nsrc: #42\nsrc:   \nWidget-CI\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"acme", "widget-ci"}; !reflect.DeepEqual(got.All, want) {
		t.Errorf("All = %v, want %v", got.All, want)
	}
	if want := []string{"#42"}; !reflect.DeepEqual(got.SourceOnly, want) {
		t.Errorf("SourceOnly = %v, want %v", got.SourceOnly, want)
	}
	if want := []string{"acme", "widget-ci", "#42"}; !reflect.DeepEqual(got.Everywhere(), want) {
		t.Errorf("Everywhere = %v, want %v", got.Everywhere(), want)
	}
	if want := []string{"acme", "widget-ci"}; !reflect.DeepEqual(got.ForTests(), want) {
		t.Errorf("ForTests = %v, want %v: a src: term must not apply to _test.go files", got.ForTests(), want)
	}
}

func TestParse_EmptyListIsAnError(t *testing.T) {
	if _, err := Parse("// only a comment\n\n"); err == nil {
		t.Fatal("a file with no terms must be an error, or a broken file reads as a clean ratchet")
	}
}

func TestLoadFrom_AbsentIsNotAnError(t *testing.T) {
	_, found, err := LoadFrom(filepath.Join(t.TempDir(), "nope.txt"))
	if found || err != nil {
		t.Fatalf("absent file: found=%v err=%v, want false/nil", found, err)
	}
	if _, found, err := LoadFrom(""); found || err != nil {
		t.Fatalf("empty path: found=%v err=%v, want false/nil", found, err)
	}
}

func TestLoadFrom_PresentButEmptyIsFoundAndAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, found, err := LoadFrom(p)
	if !found || err == nil {
		t.Fatalf("empty file: found=%v err=%v, want true/error: only absence may stand a ratchet down", found, err)
	}
}

func TestPath_EnvOverridesAndDefaultIsUnderTheConfigDir(t *testing.T) {
	t.Setenv(EnvVar, "/x/terms.txt")
	if got := Path(); got != "/x/terms.txt" {
		t.Errorf("Path() = %q, want the env override", got)
	}
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", "/h")
	if got, want := Path(), filepath.Join("/h", ".claude", "leadkit", "config", "scrub-terms.txt"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

type fakeT struct{ skipped, failed, logged string }

func (*fakeT) Helper()                     {}
func (f *fakeT) Logf(s string, a ...any)   { f.logged = s }
func (f *fakeT) Fatalf(s string, a ...any) { f.failed = s }
func (f *fakeT) Skipf(s string, a ...any)  { f.skipped = s }

func TestRequire_SkipsWhenAbsentAndReturnsTermsWhenPresent(t *testing.T) {
	t.Setenv(EnvVar, filepath.Join(t.TempDir(), "absent.txt"))
	ft := &fakeT{}
	Require(ft)
	if ft.skipped == "" || ft.failed != "" {
		t.Fatalf("absent: skipped=%q failed=%q, want a skip", ft.skipped, ft.failed)
	}
	if !strings.Contains(SkipMessage("p"), EnvVar) {
		t.Error("the skip message must say how to supply the file")
	}

	p := filepath.Join(t.TempDir(), "t.txt")
	if err := os.WriteFile(p, []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, p)
	ft = &fakeT{}
	if got := Require(ft); len(got.All) != 1 || ft.skipped != "" || ft.failed != "" {
		t.Fatalf("present: got=%v skipped=%q failed=%q", got, ft.skipped, ft.failed)
	}
}

func TestOptional_LogsWhenAbsentAndStaysQuietWhenPresent(t *testing.T) {
	t.Setenv(EnvVar, filepath.Join(t.TempDir(), "absent.txt"))
	ft := &fakeT{}
	if got := Optional(ft); len(got.All) != 0 || ft.skipped != "" || ft.failed != "" {
		t.Fatalf("absent: got=%v skipped=%q failed=%q, want zero terms and no skip", got, ft.skipped, ft.failed)
	}
	if ft.logged == "" {
		t.Fatal("absent: Optional must say it checked less, not stay silent")
	}

	p := filepath.Join(t.TempDir(), "t.txt")
	if err := os.WriteFile(p, []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, p)
	ft = &fakeT{}
	if got := Optional(ft); len(got.All) != 1 || ft.logged != "" {
		t.Fatalf("present: got=%v logged=%q, want the terms and no notice", got, ft.logged)
	}
}

// Ticket 330, pin SC2. Mutant: Parse's strings.ToLower on a "src:" term dropped. Every scan
// lower-cases what it reads, so a term left as "ZZCorp" can never match, while the entry check
// (Resolve) accepts upper-case letters: publish would print CLEAN for a leak it was given the
// name of. TestParse_CommentsBlanksScopeAndCase only had lower-case scoped terms. The unscoped
// form is checked beside it so the two stay in step.
func TestParse_AScopedTermIsLowercasedLikeEveryOther(t *testing.T) {
	got, err := Parse("src: ZZCorp\nWidgetCI\nsrc:#AbC\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"zzcorp", "#abc"}; !reflect.DeepEqual(got.SourceOnly, want) {
		t.Errorf("SourceOnly = %v, want %v: the scan lower-cases what it reads, so a term kept in mixed case never matches", got.SourceOnly, want)
	}
	if want := []string{"widgetci"}; !reflect.DeepEqual(got.All, want) {
		t.Errorf("All = %v, want %v", got.All, want)
	}
}

// Ticket 330, pin SC5c (ticket 269's original defect). Mutant: LoadFrom treating ANY read error
// as an absent file (`if rerr != nil` returning found=false), so a term file that exists but
// cannot be read made a ratchet SKIP instead of fail. Only absence may stand a ratchet down. A
// directory at the path is an unreadable "file" on every platform and for every user, root
// included, which a chmod would not be.
func TestLoadFrom_APresentButUnreadablePathIsFoundAndAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "terms.txt")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	terms, found, err := LoadFrom(p)
	if !found || err == nil {
		t.Fatalf("unreadable path: found=%v err=%v, want true/error: a ratchet may skip only on absence", found, err)
	}
	if len(terms.All)+len(terms.SourceOnly) != 0 {
		t.Errorf("an error must not carry terms: %+v", terms)
	}
	// The read failure's own wording: a parse failure says "scrub terms <path>" without "read", and
	// the path alone proves nothing (the OS error carries it whatever LoadFrom wraps it in).
	if !strings.Contains(err.Error(), "read scrub terms") {
		t.Errorf("the error must be the read failure, not a parse failure: %v", err)
	}
}
