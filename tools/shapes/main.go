// Command shapes scans the repo for the recurring defect shapes that a script can find, and
// reports them for a human to read. It never fails the build on a finding: these are candidates,
// and a red build over a judgement call would get the check disabled. The mutation harness
// (tools/mutate) is the hard-failing one.
//
//	go run ./tools/shapes [-v] [root]
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mgpp.dev/leadkit/internal/commentscan"
)

// Finding is one candidate: a place and the check that flagged it.
type Finding struct {
	Check string
	File  string
	Line  int
	Text  string
}

// The checks, in the order they are reported.
const (
	CheckPastTense = "past-tense-comment"
	CheckShouted   = "absolute-claim-shouted"
	CheckAbsolute  = "absolute-claim-lowercase"
	CheckCount     = "hardcoded-count"
	CheckShellNum  = "shell-literal-count"
	CheckLoose     = "loose-substring-on-identity"
)

var checkOrder = []string{CheckPastTense, CheckShouted, CheckAbsolute, CheckCount, CheckShellNum, CheckLoose}

var (
	// The shouted forms are the writer's own emphasis on a claim; the lowercase ones are far
	// commoner, so they are a separate count rather than drowning the first.
	shouted   = regexp.MustCompile(`\b(MUST|NEVER|ALWAYS|CANNOT)\b`)
	lowercase = regexp.MustCompile(`\b(never|always|cannot)\b`)

	nouns = `files?|tests?|rows?|lines?|cases?|commands?|checks?|fields?|keys?|entries|items?|hooks?|agents?|guards?|packages?|arms?|spans?|layers?|steps?|tickets?|roles?|options?|verbs?|flags?|subcommands?|sites?|callers?|places?`
	count = regexp.MustCompile(`(?i)\b(\d+|` + commentscan.NumberWords + `)\s+(` + nouns + `)\b|\b\d{2,}/\d{2,}\b`)

	// A numeric test against a literal other than 0 or 1 (an `-eq 4` in `[ ]`) that nothing keeps in
	// step with what it counts.
	shellNum = regexp.MustCompile(`\[\[?\s+[^\]]*\s-(eq|ne|gt|ge|lt|le)\s+([2-9]|\d{2,})\s+\]`)

	identWord = regexp.MustCompile(`^(id|ids|key|keys|path|paths|ref|refs|slug|remote|url|rel|dir|target|ticket|sha|branch|name|lane|queue|task)$`)
)

// looseFuncs are the strings functions whose substring semantics are wrong for an identity.
var looseFuncs = map[string]bool{"Contains": true, "HasPrefix": true, "HasSuffix": true, "Index": true}

// Scan returns every finding under root, sorted by check order then place.
func Scan(root string) ([]Finding, error) {
	files, err := commentscan.Files(root, ".go", ".sh", ".md", ".yml", ".yaml")
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, rel := range files {
		// Its own fixtures are deliberate instances of every shape.
		if rel == "tools/shapes/main_test.go" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if filepath.Ext(rel) == ".go" {
			found, err := scanGo(path, rel, data)
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
		out = append(out, scanShellNums(rel, string(data))...)
	}
	rank := map[string]int{}
	for i, c := range checkOrder {
		rank[c] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank[a.Check] != rank[b.Check] {
			return rank[a.Check] < rank[b.Check]
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return out, nil
}

func scanGo(path, rel string, data []byte) ([]Finding, error) {
	if commentscan.IsGenerated(data) {
		return nil, nil
	}
	lines, err := commentscan.CommentLines(rel, data)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, l := range lines {
		out = append(out, commentFindings(rel, l.Line, l.Text)...)
	}
	if strings.HasSuffix(path, "_test.go") {
		return out, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "strings" || !looseFuncs[sel.Sel.Name] {
			return true
		}
		for _, a := range call.Args {
			if name := identityName(a); name != "" {
				pos := fset.Position(call.Pos())
				out = append(out, Finding{CheckLoose, rel, pos.Line, "strings." + sel.Sel.Name + " on " + name})
				break
			}
		}
		return true
	})
	return out, nil
}

// identityName returns the name of the identifier-ish operand if its last camel/snake word says it
// is an identity (an id, key, path, ref ...), else "".
func identityName(e ast.Expr) string {
	var name string
	switch x := e.(type) {
	case *ast.Ident:
		name = x.Name
	case *ast.SelectorExpr:
		name = x.Sel.Name
	case *ast.CallExpr: // filepath.Base(p), strings.TrimSpace(id): look at the first argument
		if len(x.Args) > 0 {
			return identityName(x.Args[0])
		}
	}
	if name == "" {
		return ""
	}
	words := splitWords(name)
	if identWord.MatchString(words[len(words)-1]) {
		return name
	}
	return ""
}

// splitWords splits fooBarID / foo_bar into lowercase words.
func splitWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '_':
			flush()
		case r >= 'A' && r <= 'Z':
			// a new word starts at an upper rune unless it continues an acronym (ID, URL)
			prevUpper := i > 0 && rs[i-1] >= 'A' && rs[i-1] <= 'Z'
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			if !prevUpper || nextLower {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	if len(words) == 0 {
		return []string{""}
	}
	return words
}

func commentFindings(rel string, line int, text string) []Finding {
	if text == "" {
		return nil
	}
	var out []Finding
	if commentscan.IsHistory(text) {
		out = append(out, Finding{CheckPastTense, rel, line, text})
	}
	if shouted.MatchString(text) {
		out = append(out, Finding{CheckShouted, rel, line, text})
	}
	if lowercase.MatchString(text) {
		out = append(out, Finding{CheckAbsolute, rel, line, text})
	}
	if count.MatchString(text) {
		out = append(out, Finding{CheckCount, rel, line, text})
	}
	return out
}

func scanShellNums(rel, data string) []Finding {
	var out []Finding
	for i, line := range strings.Split(data, "\n") {
		if shellNum.MatchString(line) {
			out = append(out, Finding{CheckShellNum, rel, i + 1, strings.TrimSpace(line)})
		}
	}
	return out
}

// Report writes the per-check counts, and every finding when verbose.
func Report(w io.Writer, found []Finding, verbose bool) {
	counts := map[string]int{}
	for _, f := range found {
		counts[f.Check]++
	}
	for _, c := range checkOrder {
		fmt.Fprintf(w, "%-30s %d\n", c, counts[c])
		if !verbose {
			continue
		}
		for _, f := range found {
			if f.Check == c {
				fmt.Fprintf(w, "  %s:%d: %s\n", f.File, f.Line, f.Text)
			}
		}
	}
}

func main() {
	verbose := flag.Bool("v", false, "list every finding, not just the counts")
	flag.Parse()
	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}
	found, err := Scan(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shapes:", err)
		os.Exit(2)
	}
	Report(os.Stdout, found, *verbose)
	fmt.Println("\nthese are candidates for a human read, not failures")
}
