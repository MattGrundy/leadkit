package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The matrices are only a net while they cover the verbs. A path-taking verb added after them is
// the next ticket: the ambient-profile class recurred past a matrix because nothing
// extended it when new verbs appeared (ticket 105). So this test finds every verb
// that declares a target flag and refuses one that is neither pinned in a matrix nor exempted with
// a reason.
//
// A verb that takes its target POSITIONALLY declares no flag and cannot be found this way; those
// are listed by hand in positionalTargetVerbs, and a new one is not detected.

// targetFlags are the flags by which a verb names the repo, ticket or store it acts on.
var targetFlags = map[string]bool{"repo": true, "task": true, "ticket": true, "queue": true, "store": true}

// positionalTargetVerbs take their target as a positional argument or on stdin.
var positionalTargetVerbs = []string{"queue assign", "gate check-push"}

// verbCovered maps a verb to the test file that pins what it does with its target. The file must
// exist and mention the verb.
var verbCovered = map[string]string{
	"gate check":      "verb_state_matrix_test.go",
	"gate show":       "verb_state_matrix_test.go",
	"gate record":     "verb_state_matrix_test.go",
	"push":            "verb_state_matrix_test.go",
	"task note":       "verb_store_matrix_test.go",
	"review send":     "verb_store_matrix_test.go",
	"queue move":      "verb_store_matrix_test.go",
	"queue assign":    "verb_store_matrix_test.go",
	"gate verdict":    "cli_target_matrix_test.go",
	"gate check-push": "cli_target_matrix_test.go",
	"pr create":       "cli_target_matrix_test.go",
	"pr merge":        "cli_target_matrix_test.go",
}

// verbExempt maps a verb to why it needs no row. A reason starting "DEBT:" is a verb that does need
// one and has none yet; it is a to-do and not a decision, and is listed so it stays visible.
var verbExempt = map[string]string{
	"pr edit":              "reads the session's authority entry for the target's origin, like pr create; it opens no gate and touches no store",
	"forge protect":        "the authority entry is the SESSION's grant, keyed by the target's origin, by design (see prGate); it reads no target profile",
	"test run":             "the tests override is the SESSION's profile keyed by the target's origin, by design; it reads no target profile",
	"sync run":             "--repo is a tracker's owner/name, not a path on disk",
	"team fetch-reference": "--repo is a git URL to fetch from, not a target repo",
	"queue claim":          "DEBT: names its queue with --queue, defaulting to the session's; no row yet for an entry in another store",
	"queue repair":         "DEBT: --queue defaults to the session's; no row for a queue of another store",
	"queue rm":             "DEBT: --queue and --store default to the session's; no row for a task in another store",
	"queue sweep":          "DEBT: --queue defaults to the session's; sweep re-scopes its outputs to the swept queue's project, with no row",
	"queue unlink":         "DEBT: --queue defaults to the session's; no row for an entry in another store",
	"queue watch":          "DEBT: --queue defaults to the session's; no row for another store's queue",
	"serve":                "DEBT: --queue and --store default to the session's; account mode reads the registry instead",
}

// declaredTargetVerbs parses the non-test sources of this package and returns every verb (the name
// given to flag.NewFlagSet) that also declares one of targetFlags.
func declaredTargetVerbs(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var verb string
			var flags []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok || len(c.Args) == 0 {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				lit, isLit := c.Args[0].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					return true
				}
				val, _ := strconv.Unquote(lit.Value)
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "flag" && sel.Sel.Name == "NewFlagSet" {
					verb = val
				} else if targetFlags[val] && (sel.Sel.Name == "String" || sel.Sel.Name == "Var" || sel.Sel.Name == "StringVar") {
					flags = append(flags, val)
				}
				return true
			})
			// fs.Var(&x, "branch", ...) puts the name second; only the String form matters for the
			// verbs that exist today, and a missed Var-declared target flag is the documented limit.
			if verb != "" && len(flags) > 0 {
				out[verb] = flags
			}
		}
	}
	return out
}

func TestVerbMatrixCoverage_EveryTargetVerbIsPinnedOrExemptedWithAReason(t *testing.T) {
	declared := declaredTargetVerbs(t)
	for _, v := range positionalTargetVerbs {
		declared[v] = append(declared[v], "(positional)")
	}
	var names []string
	for v := range declared {
		names = append(names, v)
	}
	sort.Strings(names)
	if len(names) < 10 {
		t.Fatalf("found only %d target-taking verbs (%v): the source scan went blind", len(names), names)
	}
	for _, v := range names {
		_, covered := verbCovered[v]
		reason, exempt := verbExempt[v]
		switch {
		case covered && exempt:
			t.Errorf("%q is both covered and exempted: pick one", v)
		case !covered && !exempt:
			t.Errorf("%q takes a target (%v) and is in no matrix and has no exemption. Add a row to verb_state_matrix_test.go or verb_store_matrix_test.go, or exempt it in verbExempt with the reason it needs none", v, declared[v])
		case exempt && strings.TrimSpace(strings.TrimPrefix(reason, "DEBT:")) == "":
			t.Errorf("%q is exempted with no reason", v)
		}
	}
	// An entry must name a verb that exists and declares a target.
	for v := range verbCovered {
		if _, ok := declared[v]; !ok {
			t.Errorf("verbCovered names %q, which declares no target flag any more", v)
		}
	}
	for v := range verbExempt {
		if _, ok := declared[v]; !ok {
			t.Errorf("verbExempt names %q, which declares no target flag any more", v)
		}
	}
}

// A coverage entry is a claim that a file pins the verb; check the file says so.
func TestVerbMatrixCoverage_TheNamedFileMentionsTheVerb(t *testing.T) {
	for v, file := range verbCovered {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: covered by %s, which cannot be read: %v", v, file, err)
			continue
		}
		words := strings.Fields(v)
		quoted := `"` + strings.Join(words, `", "`) + `"`
		if !strings.Contains(string(src), v) && !strings.Contains(string(src), quoted) {
			t.Errorf("%s: covered by %s, which never mentions it", v, file)
		}
	}
}

// The debt is allowed to exist and is not allowed to be forgotten: it is logged on every run.
func TestVerbMatrixCoverage_ReportsTheDebt(t *testing.T) {
	var debt []string
	for v, r := range verbExempt {
		if strings.HasPrefix(r, "DEBT:") {
			debt = append(debt, v)
		}
	}
	sort.Strings(debt)
	t.Logf("verbs with a target and no matrix row yet (%d): %s", len(debt), strings.Join(debt, ", "))
}
