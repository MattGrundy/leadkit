package scrubterms

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeGH struct {
	body, stderr string
	err          error
	calls        [][]string
}

func (f *fakeGH) Run(argv []string, _ []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, argv)
	return []byte(f.body), []byte(f.stderr), f.err
}

func file(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "terms.txt")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCountEntries_IsIndependentOfTheParser(t *testing.T) {
	if got := CountEntries("// c\n\n a \nsrc: b\nsrc:\n   \n// d\nc"); got != 4 {
		t.Errorf("CountEntries = %d, want 4 (a, src: b, an empty src:, c)", got)
	}
}

func TestResolve_ReportsWhichSourceAnswered(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the standard path is absent
	gh := &fakeGH{body: "ghterm\n"}
	cases := []struct {
		name string
		o    ResolveOptions
		want string
		n    int
	}{
		{"local file first", ResolveOptions{Path: file(t, "a\nb\n"), Getenv: env(map[string]string{TextEnvVar: "envterm"}), Runner: gh}, "local file ", 2},
		{"env when the file is absent", ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "e1\ne2\ne3"}), Runner: gh}, "environment variable " + TextEnvVar, 3},
		{"GitHub variable last", ResolveOptions{Getenv: env(nil), Runner: gh, Repo: "o/r"}, "GitHub variable " + VarName, 1},
	}
	for _, c := range cases {
		got, err := Resolve(c.o)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.HasPrefix(got.Source, c.want) || got.Count() != c.n || got.Entries != c.n {
			t.Errorf("%s: source %q count %d entries %d, want %q and %d", c.name, got.Source, got.Count(), got.Entries, c.want, c.n)
		}
	}
	if len(gh.calls) != 1 || strings.Join(gh.calls[0], " ") != "gh variable get "+VarName+" --repo o/r" {
		t.Errorf("gh must be asked once, and only when nothing local answered: %v", gh.calls)
	}
}

// TestResolve_NothingResolvingRefuses is the pin for "refuse rather than report clean": an
// absent file, no environment and a failing gh must be an error that names each, and returns no
// terms a caller could mistake for an empty-but-valid list.
func TestResolve_NothingResolvingRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the standard path is absent
	for name, o := range map[string]ResolveOptions{
		"gh fails":  {Getenv: env(nil), Runner: &fakeGH{err: errors.New("exit 1"), stderr: "HTTP 404"}},
		"gh empty":  {Getenv: env(nil), Runner: &fakeGH{body: " \n"}},
		"no runner": {Getenv: env(nil)},
		"blank env": {Getenv: env(map[string]string{TextEnvVar: "  \n"})},
	} {
		got, err := Resolve(o)
		if err == nil {
			t.Errorf("%s: want a refusal, got %+v", name, got)
			continue
		}
		if got.Count() != 0 || got.Source != "" {
			t.Errorf("%s: a refusal must return no list: %+v", name, got)
		}
		if !strings.Contains(err.Error(), "does not resolve") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolve_APresentButBrokenSourceDoesNotFallThrough(t *testing.T) {
	gh := &fakeGH{body: "fine\n"}
	_, err := Resolve(ResolveOptions{Path: file(t, "// only a comment\n"), Getenv: env(nil), Runner: gh})
	if err == nil {
		t.Fatal("an empty local file is broken; a good second source must not hide it")
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh was consulted after the local file was found broken: %v", gh.calls)
	}
	// A path that is there but cannot be read is not an absent file, and a good second source
	// must not paper over it.
	if _, err := Resolve(ResolveOptions{Path: t.TempDir(), Getenv: env(map[string]string{TextEnvVar: "fine"})}); err == nil {
		t.Error("an unreadable local path (a directory) is an error, not an absent file")
	}
}

// TestResolve_ATermCountMismatchRefuses is the 15-of-16 case. An empty `src:` is a line the
// parser drops, so the parser and the file disagree; every source must refuse on it, not just
// the file.
func TestResolve_ATermCountMismatchRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the standard path is absent
	text := "alpha\nsrc:\nbeta\n"
	for name, o := range map[string]ResolveOptions{
		"file": {Path: file(t, text), Getenv: env(nil)},
		"env":  {Getenv: env(map[string]string{TextEnvVar: text})},
		"gh":   {Getenv: env(nil), Runner: &fakeGH{body: text}},
	} {
		got, err := Resolve(o)
		if err == nil || !strings.Contains(err.Error(), "parsed 2 terms from 3 entries") {
			t.Errorf("%s: err = %v, want a refusal naming 2 of 3", name, err)
		}
		if got.Count() != 0 {
			t.Errorf("%s: a refusal must return no list: %+v", name, got)
		}
	}
	// The control: the same list without the dropped entry resolves.
	if got, err := Resolve(ResolveOptions{Path: file(t, "alpha\nbeta\n"), Getenv: env(nil)}); err != nil || got.Count() != 2 {
		t.Errorf("a list the parser keeps whole must resolve: %v %+v", err, got)
	}
}

// TestResolve_ANamedFileThatIsAbsentRefuses: a typo in --terms-file (or $LEADKIT_SCRUB_TERMS) must
// not fall through to another source and check a different list.
func TestResolve_ANamedFileThatIsAbsentRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gh := &fakeGH{body: "fine\n"}
	missing := filepath.Join(t.TempDir(), "typo.txt")
	for name, o := range map[string]ResolveOptions{
		"flag": {Path: missing, Getenv: env(map[string]string{TextEnvVar: "fine"}), Runner: gh},
		"env":  {Getenv: env(map[string]string{EnvVar: missing, TextEnvVar: "fine"}), Runner: gh},
	} {
		got, err := Resolve(o)
		if err == nil || !strings.Contains(err.Error(), "does not exist") || got.Count() != 0 {
			t.Errorf("%s: err = %v got %+v, want a refusal naming the missing file", name, err, got)
		}
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh was consulted after a named file was missing: %v", gh.calls)
	}
	// Control: an UNNAMED standard path that is absent does fall through.
	if got, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "fine"})}); err != nil || got.Count() != 1 {
		t.Errorf("an absent standard path must fall through: %v %+v", err, got)
	}
}

// TestResolve_AnEntryThatCanNeverMatchRefuses: Parse and CountEntries share the format's
// assumptions, so they agree about every one of these and the count check passes. Each is a
// term that looks right and matches nothing.
func TestResolve_AnEntryThatCanNeverMatchRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, text := range map[string]string{
		"leading BOM":     "\ufeffacmecorp\nother\n",
		"BOM before //":   "\ufeff// comment\nacmecorp\n",
		"inline comment":  "acmecorp // note\n",
		"scoped, inline":  "src: #42 // note\n",
		"zero-width":      "acme\u200bcorp\n",
		"control char":    "acme\x01corp\n",
		"tab inside term": "acme\tcorp\n",
		"no-break space":  "acme\u00a0corp\n",
		"ideographic sp":  "acme\u3000corp\n",
		"grapheme joiner": "acme\u034fcorp\n",
		"replacement chr": "acme\ufffdcorp\n",
		"cyrillic a":      "\u0430cmecorp\n",
		"invalid utf-8":   "acme\xffcorp\n",
		"delete (0x7f)":   "acme\x7fcorp\n",
		"capital Src":     "Src: acme corp\n",
		"space before :":  "src : acme corp\n",
		"all caps SRC":    "SRC:acme\n",
	} {
		for src, o := range map[string]ResolveOptions{
			"file": {Path: file(t, text), Getenv: env(nil)},
			"env":  {Getenv: env(map[string]string{TextEnvVar: text})},
			"gh":   {Getenv: env(nil), Runner: &fakeGH{body: text}},
		} {
			got, err := Resolve(o)
			if err == nil || got.Count() != 0 {
				t.Errorf("%s via %s: err = %v got %+v, want a refusal", name, src, err, got)
			}
		}
	}
	// Controls: ordinary lines, CRLF endings, a scoped term and a // comment still resolve.
	ok := "// header\r\nacmecorp\r\nsrc: #42\r\n\r\nhttp://example.test/x\r\n"
	if got, err := Resolve(ResolveOptions{Path: file(t, ok), Getenv: env(nil)}); err != nil || got.Count() != 3 {
		t.Errorf("a valid list must resolve: %v %+v", err, got)
	}
}

// The refusal names the character and where it is, so the owner can fix the file.
func TestResolve_TheRefusalNamesTheCharacterAndItsPosition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "fine\nacme\u00a0corp\n"})})
	if err == nil || !strings.Contains(err.Error(), "U+00A0") || !strings.Contains(err.Error(), "at byte 4") {
		t.Fatalf("err = %v, want the character and its byte position", err)
	}
	_, err = Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "acme\xffcorp\n"})})
	if err == nil || !strings.Contains(err.Error(), "invalid UTF-8 byte 0xff") {
		t.Fatalf("err = %v, want the invalid byte named", err)
	}
	// Control: every character of the alphabet is legal, so the refusals above are about the
	// character and not about the position.
	if got, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: strings.ReplaceAll(literalAlphabet, " ", "") + "\n"})}); err != nil || got.Count() != 1 {
		t.Errorf("the alphabet must resolve: %v %+v", err, got)
	}
}

// "Src:" is printable ASCII and agrees on count, but Parse only knows "src:", so it is an ordinary
// term that matches nothing. Spellings that merely START with src are plain terms and stay legal.
func TestResolve_AMisspeltScopePrefixRefusesButPlainTermsDoNot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "Src: acme corp\n"})})
	if err == nil || !strings.Contains(err.Error(), "scoped term") {
		t.Fatalf("err = %v, want the misspelt scope named", err)
	}
	for _, ok := range []string{"src: acme corp", "srcfoo", "src/main", "source: x", "acme src : y"} {
		got, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: ok + "\n"})})
		if err != nil || got.Count() != 1 {
			t.Errorf("%q must resolve: %v %+v", ok, err, got)
		}
	}
	got, _ := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "src: acme corp\n"})})
	if len(got.Terms.SourceOnly) != 1 {
		t.Errorf("the exact spelling must still scope the term: %+v", got.Terms)
	}
}

func TestResolve_TheRefusalSaysANonASCIINameCannotBeListed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "caf\u00e9\n"})})
	if err == nil || !strings.Contains(err.Error(), "cannot be listed at all") ||
		!strings.Contains(err.Error(), "will NOT check for the original name") || !strings.Contains(err.Error(), "check that name by hand") {
		t.Fatalf("err = %v, want the way forward stated", err)
	}
}

// A valid, correctly parsed entry that is not what its author meant: each of these is printable
// ASCII, agrees on count and matches nothing, so the run printed CLEAN against a tree holding the
// term. The alphabet is the fix: stop enumerating what is wrong, define what is allowed.
func TestResolve_AnEntryOutsideTheAlphabetRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for name, text := range map[string]string{
		"quoted":         "\"acmecorp\"",
		"single quoted":  "'acmecorp'",
		"trailing comma": "acmecorp,",
		"backticked":     "`acmecorp`",
		"regexp":         "acme.*corp",
		"escaped dot":    "acme\\.com",
		"glob":           "*acmecorp",
		"double space":   "acme  corp",
		"list marker":    "- acmecorp",
		"semicolon":      "acmecorp;",
		"brackets":       "[acmecorp]",
		"scoped, quoted": "src: \"acmecorp\"",
		"scoped, double": "src:  acmecorp",
		"parenthesised":  "(acmecorp)",
	} {
		got, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: text + "\n"})})
		if err == nil || got.Count() != 0 {
			t.Errorf("%s (%q): err = %v got %+v, want a refusal", name, text, err, got)
		}
	}
	// Every printable ASCII character outside the alphabet refuses; every one inside resolves. The
	// oracle is the literal alphabet above, NOT the function under test: an oracle that is the
	// code proves only that the code agrees with itself, and could not catch a metacharacter
	// added to it later.
	for c := 0x20; c <= 0x7e; c++ {
		_, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: "ab" + string(rune(c)) + "cd\n"})})
		inside := strings.IndexByte(literalAlphabet, byte(c)) >= 0
		if inside == (err != nil) {
			t.Errorf("%q: in the alphabet = %v but err = %v", rune(c), inside, err)
		}
	}
	// The real shapes of a term all fit: a ticket number, a path, a hyphenated name, an id, a
	// scoped term, a dotted host, an address.
	for _, ok := range []string{"a_b", "a+b", "#456", "/users/someone", "hyphen-ated-name", "a1b2c3d4", "src: #456", "internal.example.test", "someone@example.test", "two words"} {
		if got, err := Resolve(ResolveOptions{Getenv: env(map[string]string{TextEnvVar: ok + "\n"})}); err != nil || got.Count() != 1 {
			t.Errorf("%q must resolve: %v %+v", ok, err, got)
		}
	}
}

// literalAlphabet is the alphabet a term may use, written out independently of the code under
// test. Changing the alphabet means changing this line, which is the point.
const literalAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ._/#@:+-"
