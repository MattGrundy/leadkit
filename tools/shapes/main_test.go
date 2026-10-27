package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scanSrc writes one file into a fresh tree and returns the checks that fired, one per finding.
func scanSrc(t *testing.T, name, src string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range found {
		out = append(out, f.Check)
	}
	return out
}

func TestScan_CommentChecks(t *testing.T) {
	cases := []struct {
		name, comment string
		want          []string
	}{
		{"history", "// This used to run over the store directly.", []string{CheckPastTense}},
		{"history no longer", "// The caller can no longer see the flag.", []string{CheckPastTense}},
		{"noun then used to is ambiguous and stays a candidate", "// The schedule used to re-arm the loop.", []string{CheckPastTense}},
		{"present tense is used to", "// It is used to detect skew.", nil},
		{"shouted", "// This MUST run before the check.", []string{CheckShouted}},
		{"lowercase", "// It never rewrites the file.", []string{CheckAbsolute}},
		{"count digits", "// 73 files are still dirty.", []string{CheckCount}},
		{"count word", "// eleven rows buried the finding.", []string{CheckCount}},
		{"count fraction", "// coverage was 492/78 here.", []string{CheckCount}},
		{"two is prose", "// the two keys differ.", nil},
		{"plain", "// returns the id.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanSrc(t, "a.go", "package a\n\n"+c.comment+"\nvar x = 1\n")
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%q: got %v, want %v", c.comment, got, c.want)
			}
		})
	}
}

func TestScan_LooseSubstringOnIdentity(t *testing.T) {
	src := func(body string) string {
		return "package a\n\nimport \"strings\"\n\nfunc f(ticketID, text, repoPath string) bool {\n\t" + body + "\n}\n"
	}
	cases := []struct {
		name, body string
		want       []string
	}{
		{"id", "return strings.Contains(text, ticketID)", []string{CheckLoose}},
		{"path via call", "return strings.HasPrefix(strings.TrimSpace(repoPath), \"x\")", []string{CheckLoose}},
		{"acronym word", "return strings.HasSuffix(ticketID, \"x\")", []string{CheckLoose}},
		{"not an identity", "return strings.Contains(text, \"x\")", nil},
		{"identity is only a prefix of the name", "idle := text; return strings.Contains(idle, \"x\")", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanSrc(t, "a.go", src(c.body))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	if got := scanSrc(t, "a_test.go", src("return strings.Contains(text, ticketID)")); got != nil {
		t.Errorf("test files assert on output with Contains on purpose, got %v", got)
	}
}

func TestScan_ShellLiteralCount(t *testing.T) {
	for line, want := range map[string][]string{
		`[ "$n" -eq 4 ] || exit 1`:   {CheckShellNum},
		`[ "$n" -ge 12 ]`:            {CheckShellNum},
		`[ "$n" -eq 0 ]`:             nil,
		`[ "$n" -eq 1 ]`:             nil,
		`[ "$n" -eq "$expected" ]`:   nil,
		`echo "-eq 4 is not a test"`: nil,
	} {
		if got := scanSrc(t, "x.sh", line+"\n"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", line, got, want)
		}
	}
}

func TestScan_SkipsStateDirectories(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"queue", "tasks", ".git"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, d, "x.go"), []byte("package x\n// 73 files\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err := Scan(dir)
	if err != nil || len(found) != 0 {
		t.Errorf("state directories are not source: %v %v", found, err)
	}
}

func TestScan_NestedStateNamedPackagesAreSource(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"internal/queue", "internal/tasks", "internal/bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, d, "x.go"), []byte("package x\n// 73 files\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range found {
		seen[filepath.ToSlash(f.File)] = true
	}
	for _, d := range []string{"internal/queue", "internal/tasks", "internal/bin"} {
		found := false
		for k := range seen {
			if strings.Contains(k, d+"/x.go") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is a package and must be scanned, got %v", d, found)
		}
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{
		"ticketID": {"ticket", "id"}, "repo_path": {"repo", "path"}, "ID": {"id"},
		"idle": {"idle"}, "parseURL": {"parse", "url"}, "HTTPSRemote": {"https", "remote"},
	} {
		if got := splitWords(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitWords(%q) = %v, want %v", in, got, want)
		}
	}
}
