// Package repoguard holds tests over repository files that are not Go code.
package repoguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	usesRe = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	// owner/repo[/path]@<40 hex>, then a "# <tag>" comment saying what the SHA is.
	pinnedRe  = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	tagCommRe = regexp.MustCompile(`^\s+#\s*\S+`)
)

// checkWorkflow returns one problem per `uses:` line that is not pinned to a full commit
// SHA with a trailing tag comment. Local actions ("./...") and docker:// references are
// not fetched from a mutable git ref, so they are left alone.
func checkWorkflow(name, content string) []string {
	var out []string
	for i, line := range strings.Split(content, "\n") {
		m := usesRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref, rest := m[1], m[2]
		if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
			continue
		}
		switch {
		case !pinnedRe.MatchString(ref):
			out = append(out, name+":"+strconv.Itoa(i+1)+": "+ref+" is not pinned to a 40-hex commit SHA")
		case !tagCommRe.MatchString(rest):
			out = append(out, name+":"+strconv.Itoa(i+1)+": "+ref+" has no trailing `# <tag>` comment saying what the SHA is")
		}
	}
	return out
}

// A floating tag is mutable: whatever `v4` points at when the job runs is what executes,
// in a workflow that holds contents:write on the release path. Every action is pinned.
func TestEveryWorkflowActionIsPinnedToACommitSHA(t *testing.T) {
	files, err := filepath.Glob("../../.github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflow files found (glob err %v): the guard would pass vacuously", err)
	}
	uses := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		uses += strings.Count(string(b), "uses:")
		for _, p := range checkWorkflow(filepath.Base(f), string(b)) {
			t.Error(p)
		}
	}
	if uses == 0 {
		t.Fatal("found workflow files but no `uses:` lines: the guard would pass vacuously")
	}
}

func TestCheckWorkflowRecognisesWhatItShouldReject(t *testing.T) {
	const sha = "11d5960a326750d5838078e36cf38b85af677262"
	cases := []struct {
		name, line string
		problems   int
	}{
		{"floating tag", "      - uses: actions/checkout@v4", 1},
		{"floating tag, key on its own line", "        uses: actions/setup-go@v5", 1},
		{"branch ref", "        uses: actions/checkout@main", 1},
		{"short sha", "        uses: actions/checkout@11d5960", 1},
		{"no ref at all", "        uses: actions/checkout", 1},
		{"uppercase hex (the convention is the lowercase GitHub prints)", "        uses: actions/checkout@" + strings.ToUpper(sha) + " # v4", 1},
		{"pinned but uncommented", "        uses: actions/checkout@" + sha, 1},
		{"pinned and commented", "        uses: actions/checkout@" + sha + " # v4", 0},
		{"pinned, list form", "      - uses: actions/checkout@" + sha + " # v4", 0},
		{"subpath action pinned", "        uses: github/codeql-action/init@" + sha + " # v3", 0},
		{"local action", "        uses: ./.github/actions/thing", 0},
		{"docker", "        uses: docker://alpine:3", 0},
		{"not a uses line", "        run: echo uses: actions/checkout@v4", 0},
	}
	for _, c := range cases {
		if got := checkWorkflow("w.yml", c.line); len(got) != c.problems {
			t.Errorf("%s: %d problems %v, want %d", c.name, len(got), got, c.problems)
		}
	}
}
