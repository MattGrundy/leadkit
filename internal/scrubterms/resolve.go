package scrubterms

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// VarName is the name of the GitHub Actions variable that holds the term list, the durable
// home that is not a workstation (ticket 269). TextEnvVar is how that variable reaches a CI job:
// a workflow maps it in as `LEADKIT_SCRUB_TERMS_TEXT: ${{ vars.LEADKIT_SCRUB_TERMS }}`.
const (
	VarName    = "LEADKIT_SCRUB_TERMS"
	TextEnvVar = "LEADKIT_SCRUB_TERMS_TEXT"
)

// Runner is the seam resolution reads GitHub through. internal/forge's Runner has the same
// method set and satisfies it, so tests drive resolution with the same fakes and nothing here
// shells out to a live gh.
type Runner interface {
	Run(argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// ResolveOptions say where to look. The zero value looks at Path() and the environment only.
type ResolveOptions struct {
	// Path is the local file the operator NAMED. A named path that does not exist is a refusal,
	// not a reason to try another source: a typo must not quietly check a different list. Empty
	// means $LEADKIT_SCRUB_TERMS if set (named too), else the standard path (which may be absent).
	Path string
	// Getenv reads the environment; nil means os.Getenv.
	Getenv func(string) string
	// Runner reads the GitHub variable with `gh variable get`; nil means that source is not
	// consulted, and the refusal says so rather than leaving it out.
	Runner Runner
	// Repo is the owner/name the variable lives on; empty lets gh infer it from the working
	// directory's remote.
	Repo string
}

// Resolved is a term list and the proof of where it came from.
//
// Its bound on Unicode: matching is byte-for-byte after lowercasing. A leak spelled in another
// Unicode form (decomposed rather than composed accents, fullwidth letters, a ligature) is not
// the term that was listed and will pass. Normalising with NFKC and case folding would close that,
// and needs golang.org/x/text, which leadkit does not depend on; the threat modelled is a name
// typed normally by accident, not one deliberately respelled.
type Resolved struct {
	Terms Terms
	// Source says which source answered: a path, the CI environment variable, or the GitHub
	// variable.
	Source string
	// Entries is the number of non-comment lines in the source, counted independently of the
	// parser. It equals the terms parsed, or Resolve would have refused.
	Entries int
}

// Count is the number of terms in the list.
func (r Resolved) Count() int { return len(r.Terms.All) + len(r.Terms.SourceOnly) }

// CountEntries is the number of non-comment, non-blank lines in a term file, counted without
// the parser. It is deliberately the dumbest possible reading of the format so that a parser
// which drops or invents a term disagrees with it. An earlier gate parsed with the wrong comment
// marker, dropped a term, and printed CLEAN against 15 of 16.
func CountEntries(data string) int {
	n := 0
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		n++
	}
	return n
}

// termAlphabet is every character a term may use: what a name or a path needs. Anything else
// is refused rather than guessed at.
const termAlphabet = "letters, digits, space and . _ / # @ : + -"

func inTermAlphabet(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte(" ._/#@:+-", c) >= 0
}

// checkEntry allow-lists an entry: it may use only termAlphabet, with single spaces, and may not
// begin "- ". Parse and CountEntries share the format's assumptions (what a comment is, what
// whitespace is), so their agreeing proves only that the parser kept every line, not that every
// line means what its author thought. Each way of refusing here was first found as a term that
// parsed, counted and then matched nothing while the run printed CLEAN:
//
//   - invisible or look-alike characters (a byte-order mark, a no-break space, a Cyrillic letter).
//     A deny-list of those was defeated by ones nobody had listed, so the check is an allow-list;
//   - punctuation that is syntax in some other format: quotes, a trailing comma, backticks, a
//     regular-expression or glob (acme.*corp, *acme), a backslash, a list marker ("- acme"), a
//     doubled space. Each is a valid entry for a different tool and a literal that matches nothing
//     here.
//
// Defining what is allowed closes the class by construction, including cases nobody has thought
// of; the cost is that a term outside the alphabet cannot be listed at all. See Resolved's bound
// on Unicode forms for what remains.
//
// An inline `//` is refused too: only a line that STARTS with it is a comment, so "acme // note"
// is the single term "acme // note", which matches nothing.
func checkEntry(line string) error {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inTermAlphabet(c) {
			continue
		}
		what := fmt.Sprintf("the character %q", rune(c))
		if c >= 0x80 {
			r, _ := utf8.DecodeRuneInString(line[i:])
			what = fmt.Sprintf("the character %U", r)
			if r == utf8.RuneError {
				what = fmt.Sprintf("the invalid UTF-8 byte 0x%02x", c)
			}
		} else if c < 0x20 || c == 0x7f {
			what = fmt.Sprintf("the control character %U", rune(c))
		}
		return fmt.Errorf("entry %q holds %s at byte %d. A term may use only %s: a quote, comma, backtick, "+
			"wildcard, backslash or any non-ASCII character makes a term that matches nothing while looking "+
			"right (a leading byte-order mark is the usual cause: save the file as plain UTF-8 without one). "+
			"A name outside that alphabet cannot be listed at all, and there is no opt-in. If you leave it off "+
			"the list or respell it, `leadkit publish` will NOT check for the original name: check that name "+
			"by hand, because the run will still print CLEAN", line, what, i, termAlphabet)
	}
	if strings.Contains(line, "  ") {
		return fmt.Errorf("entry %q holds two spaces in a row; terms are matched literally, so use a single space", line)
	}
	if strings.HasPrefix(line, "- ") {
		return fmt.Errorf("entry %q begins with %q, a list marker; the file is one term per line with no markers", line, "- ")
	}
	// Parse recognises a scoped term only as exactly "src:". Any other spelling of it ("Src:",
	// "src :") is printable ASCII, agrees on count, and parses as an ordinary term that matches
	// nothing, so it is refused rather than left to pass.
	if lower := strings.ToLower(line); strings.HasPrefix(lower, "src") &&
		strings.HasPrefix(strings.TrimLeft(lower[3:], " "), ":") && !strings.HasPrefix(line, srcPrefix) {
		return fmt.Errorf("entry %q looks like a scoped term but is not spelled exactly %q (lower case, no space "+
			"before the colon), so it would be read as an ordinary term that matches nothing", line, srcPrefix)
	}
	if strings.Contains(line, " //") {
		return fmt.Errorf("entry %q holds an inline //; only a line that starts with // is a comment, "+
			"so this is one term that would never match", line)
	}
	return nil
}

// parseChecked parses a term list and refuses when the parser did not keep every entry, or when
// an entry could never match.
func parseChecked(data, source string) (Resolved, error) {
	terms, err := Parse(data)
	if err != nil {
		return Resolved{}, fmt.Errorf("%s: %w", source, err)
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if err := checkEntry(line); err != nil {
			return Resolved{}, fmt.Errorf("%s: %w", source, err)
		}
	}
	entries := CountEntries(data)
	if got := len(terms.All) + len(terms.SourceOnly); got != entries {
		return Resolved{}, fmt.Errorf("%s: parsed %d terms from %d entries; refusing, because a list that "+
			"drops terms reports clean against the ones it kept", source, got, entries)
	}
	return Resolved{Terms: terms, Source: source, Entries: entries}, nil
}

// Resolve finds the term list in a fixed order and reports which source answered:
//
//  1. the local file (o.Path, else $LEADKIT_SCRUB_TERMS, else the standard path),
//  2. the CI environment variable TextEnvVar,
//  3. the GitHub variable VarName, read with `gh variable get`.
//
// A source that is ABSENT falls through to the next, except a local file the operator named, whose
// absence is a refusal. A source that is PRESENT and broken (an
// unreadable or empty file, a parse that dropped an entry) is an error and does not fall through,
// because a second source answering would hide that the first is wrong. When no source answers
// the error names every one tried and why, and no list is returned: the caller must refuse, not
// skip. The bound is the one in the package doc: a resolving list is the list the owner gave,
// not proof it is complete.
func Resolve(o ResolveOptions) (Resolved, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path, named := o.Path, o.Path != ""
	if path == "" {
		if v := getenv(EnvVar); v != "" {
			path, named = v, true
		} else {
			path = defaultPath()
		}
	}
	var tried []string

	switch {
	case path == "":
		tried = append(tried, "local path: none configured (no home directory)")
	default:
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			return parseChecked(string(data), "local file "+path)
		case errors.Is(err, fs.ErrNotExist) && named:
			return Resolved{}, fmt.Errorf("local file %s was named (by --terms-file or $%s) and does not exist; "+
				"refusing rather than checking a different list", path, EnvVar)
		case errors.Is(err, fs.ErrNotExist):
			tried = append(tried, "local file "+path+": does not exist")
		default:
			return Resolved{}, fmt.Errorf("local file %s: %w", path, err)
		}
	}

	if text := getenv(TextEnvVar); strings.TrimSpace(text) != "" {
		return parseChecked(text, "environment variable "+TextEnvVar)
	}
	tried = append(tried, "environment variable "+TextEnvVar+": unset or empty")

	if o.Runner == nil {
		tried = append(tried, "GitHub variable "+VarName+": not consulted (no runner)")
	} else {
		argv := []string{"gh", "variable", "get", VarName}
		if o.Repo != "" {
			argv = append(argv, "--repo", o.Repo)
		}
		out, stderr, err := o.Runner.Run(argv, nil)
		switch {
		case err != nil:
			why := strings.TrimSpace(string(stderr))
			if why == "" {
				why = err.Error()
			}
			tried = append(tried, "GitHub variable "+VarName+": "+why)
		case strings.TrimSpace(string(out)) == "":
			tried = append(tried, "GitHub variable "+VarName+": empty")
		default:
			return parseChecked(string(out), "GitHub variable "+VarName)
		}
	}

	return Resolved{}, fmt.Errorf("the scrub term list does not resolve, so nothing was checked "+
		"(a gate with no list would report clean):\n  - %s", strings.Join(tried, "\n  - "))
}

// defaultPath is the standard location, ignoring $LEADKIT_SCRUB_TERMS (Resolve reads that itself,
// through its Getenv, so it can tell a named path from the default).
func defaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "leadkit", "config", "scrub-terms.txt")
}
