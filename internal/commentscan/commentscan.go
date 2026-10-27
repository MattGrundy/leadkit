// Package commentscan is the one definition of the source-comment shapes the history ratchet
// (cmd/leadkit) and the shapes report (tools/shapes) both look for, and of which files count as
// source.
package commentscan

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// NumberWords are the spelled-out numbers a comment can state a count with.
const NumberWords = `four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|hundred`

var (
	historyPhrases = regexp.MustCompile(`(?i)\b(previously|used to|no longer|which meant|before this (fix|change|commit|pr|was)|an earlier version|an earlier draft|was changed from|were changed from|formerly|the old|pre-fix|pre-change|had the same bug|the same bug|the mistake|originally|was a bug|was fixed|has been fixed|times running)\b`)

	// "X is used to do Y" is present tense; "X used to do Y" is history.
	passiveUsedTo = regexp.MustCompile(`(?i)\b(is|are|be|was|were|been|being|and|or|only|also|then|that|which|it|can|could|will|would|must|should|to|when|if|as|not)\s+used to\b`)

	// SpelledCount matches a spelled-out count followed by the word it counts.
	SpelledCount = regexp.MustCompile(`(?i)\b(a dozen|dozens|` + NumberWords + `)\s+(\w+)`)

	generatedHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)
)

// IsHistory reports whether a comment line carries a past-tense marker, ignoring the
// present-tense "is used to".
func IsHistory(text string) bool {
	for _, m := range historyPhrases.FindAllStringIndex(text, -1) {
		if strings.EqualFold(text[m[0]:m[1]], "used to") && passiveUsedTo.MatchString(text[:m[1]]) {
			continue
		}
		return true
	}
	return false
}

// IsGenerated reports whether src carries Go's generated-file header on a line of its own.
func IsGenerated(src []byte) bool {
	return generatedHeader.Match(src)
}

// CommentLine is one line of one comment.
type CommentLine struct {
	Line int
	Text string // trimmed of its comment markers
}

// CommentLines returns every comment line of a Go file.
func CommentLines(path string, src []byte) ([]CommentLine, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var out []CommentLine
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			start := fset.Position(c.Pos()).Line
			for i, line := range strings.Split(c.Text, "\n") {
				text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(line), "//"), "/*"))
				out = append(out, CommentLine{start + i, text})
			}
		}
	}
	return out, nil
}

// SkipPath reports whether the slash-separated repo-relative file path is not source. State
// directories are skipped only at the root: internal/queue is a real package.
func SkipPath(rel string) bool {
	segs := strings.Split(rel, "/")
	switch segs[0] {
	case "queue", "tasks", "bin", "team", ".claude":
		return true
	}
	for _, s := range segs[:len(segs)-1] {
		switch s {
		case ".git", ".leadkit", "vendor", "testdata", "node_modules":
			return true
		}
	}
	return false
}

// Files returns the slash-separated paths under root, relative to it and sorted, of the files with
// one of exts. In a git checkout it asks git, so gitignored local state (agent worktrees, a
// captured team snapshot) can never leak in; elsewhere it walks the directory.
func Files(root string, exts ...string) ([]string, error) {
	all, err := gitFiles(root)
	if err != nil {
		all, err = walkFiles(root)
		if err != nil {
			return nil, err
		}
	}
	var out []string
	for _, p := range all {
		if SkipPath(p) {
			continue
		}
		for _, e := range exts {
			if path.Ext(p) == e {
				out = append(out, p)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// gitFiles lists the tracked and not-ignored files of the checkout at root.
func gitFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil, err
	}
	var all []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			all = append(all, p)
		}
	}
	return all, nil
}

func walkFiles(root string) ([]string, error) {
	var all []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && SkipPath(rel+"/.") {
				return filepath.SkipDir
			}
			return nil
		}
		all = append(all, rel)
		return nil
	})
	return all, err
}
