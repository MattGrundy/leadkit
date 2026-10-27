package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mutant is one small change to one source file. Offsets are byte offsets into the ORIGINAL file.
type Mutant struct {
	File   string // path relative to the repo root, slash-separated
	Func   string // enclosing function, "" outside one
	Line   int
	Op     string // the mutation operator
	Before string // the text replaced
	After  string // the text it was replaced with
	start  int
	end    int
	nth    int // occurrence index among mutants with an identical Key in this file
}

// Key identifies a mutant by file, function and the change itself rather than by line, so an
// accepted-survivor entry does not go stale when unrelated code above it moves, and does go stale
// when the mutated code changes.
func (m Mutant) Key() string {
	return fmt.Sprintf("%s: %s: %s: %q => %q #%d", m.File, m.Func, m.Op, m.Before, m.After, m.nth)
}

// Source returns the file with the mutation applied.
func (m Mutant) Source(orig []byte) []byte {
	out := make([]byte, 0, len(orig)+len(m.After))
	out = append(out, orig[:m.start]...)
	out = append(out, m.After...)
	return append(out, orig[m.end:]...)
}

// swap maps an operator to its mutations. Comparison operators get a negation and a boundary
// shift; logical operators get their dual. Arithmetic is left out on purpose: the guards are
// decision logic, and mutating `+` on strings only produces mutants that do not compile.
var swap = map[token.Token][]string{
	token.EQL:  {"!="},
	token.NEQ:  {"=="},
	token.LSS:  {">=", "<="},
	token.LEQ:  {">", "<"},
	token.GTR:  {"<=", ">="},
	token.GEQ:  {"<", ">"},
	token.LAND: {"||"},
	token.LOR:  {"&&"},
}

// Generate returns every mutant of one file. rel is the repo-relative name recorded on each.
func Generate(path, rel string) ([]Mutant, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	var out []Mutant
	fn := ""
	add := func(pos token.Pos, op string, start, end int, after string) {
		out = append(out, Mutant{
			File: filepath.ToSlash(rel), Func: fn, Line: fset.Position(pos).Line, Op: op,
			Before: string(src[start:end]), After: after, start: start, end: end,
		})
	}
	visit := func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			for _, to := range swap[x.Op] {
				s := off(x.OpPos)
				add(x.OpPos, "operator", s, s+len(x.Op.String()), to)
			}
		case *ast.IfStmt:
			s, e := off(x.Cond.Pos()), off(x.Cond.End())
			add(x.Cond.Pos(), "negate-condition", s, e, "!("+string(src[s:e])+")")
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				if id, ok := r.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
					flip := "true"
					if id.Name == "true" {
						flip = "false"
					}
					add(id.Pos(), "flip-bool", off(id.Pos()), off(id.End()), flip)
				}
			}
		case *ast.ExprStmt:
			if call, ok := x.X.(*ast.CallExpr); ok && removable(call) {
				add(x.Pos(), "remove-call", off(x.Pos()), off(x.End()), "_ = 0")
			}
		}
		return true
	}
	for _, d := range f.Decls {
		fn = ""
		if fd, ok := d.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				fn = types(fd.Recv.List[0].Type) + "." + fn
			}
		}
		ast.Inspect(d, visit)
	}
	// Number identical keys so each is separately addressable in the accepted list.
	seen := map[string]int{}
	for i := range out {
		k := fmt.Sprintf("%s|%s|%s|%s|%s", out[i].File, out[i].Func, out[i].Op, out[i].Before, out[i].After)
		out[i].nth = seen[k]
		seen[k]++
	}
	return out, nil
}

// types renders a receiver type as its bare name, so a pointer and value receiver key alike.
func types(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return types(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return types(x.X)
	}
	return "?"
}

// removable reports whether deleting a bare call statement is a meaningful mutation. Printing and
// panicking are not behaviour a guard's tests are expected to pin, and removing a lock or wait
// call only deadlocks the test run.
func removable(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name != "panic" && fn.Name != "print" && fn.Name != "println"
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && (pkg.Name == "fmt" || pkg.Name == "log") {
			return false
		}
		switch fn.Sel.Name {
		case "Lock", "Unlock", "RLock", "RUnlock", "Wait", "Done":
			return false
		}
	}
	return true
}

// GeneratePackage returns the mutants of every non-test .go file directly in dir.
func GeneratePackage(root, dir string) ([]Mutant, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return nil, err
	}
	var out []Mutant
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		ms, err := Generate(filepath.Join(root, dir, name), filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}
