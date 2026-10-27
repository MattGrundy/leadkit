package hook

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// What the repository refusal does not read, as data. This table is the ONLY place a limit is named: the
// header of pr_guard_repo.go lists exactly these names (its block is generated from this table and checked by
// TestPRGuard_TheLimitsInTheHeaderAreTheLimitsInTheTable), and every row below is asserted ALLOWED. A limit
// cannot be written in the header without a pin, or pinned without being named; closing one is a decision,
// because its rows then fail and have to be moved to the refused table.
type forgeLimit struct {
	Name   string
	Pinned []string
}

var forgeStatedLimits = []forgeLimit{
	{"a script file or program run by name (the incident's route)", []string{
		`python3 pg198.py`, `bash ./verbs.sh`, `sh verbs.sh`, `. ./verbs.sh`, `source ./verbs.sh`, `./create-repo.sh x`, `node make-repo.js`,
		`ruby script.rb`, `make publish-repo`, `npm run create-repo`,
	}},
	{"a list run by xargs or while read", []string{
		`xargs -I{} sh -c {} < verbs.txt`, `while read l; do sh -c "$l"; done < verbs.txt`,
	}},
	{"a raw API client: curl, or an inline program's own HTTP client", []string{
		`curl -X POST -H "Authorization: token $GH_TOKEN" https://api.github.com/user/repos -d '{"name":"x"}'`,
		`python3 -c "import requests; requests.post('https://api.github.com/user/repos', json={'name':'x'}, headers={'Authorization':'token '+T})"`,
		`python3 -c "import urllib.request as u; u.urlopen(u.Request('https://api.github.com/user/repos', data=b'{}', method='POST'))"`,
		`node -e "fetch('https://api.github.com/user/repos', {method:'POST', body:'{}'})"`,
	}},
	{"a command word built by expansion", []string{
		`$(command -v gh) repo delete x --yes`, "`which gh` repo delete x --yes", `${GHB:-gh} api -X DELETE repos/o/r`, `g=gh; $g repo delete x --yes`,
	}},
	{"the output of a substitution that is then run", []string{
		`eval "$(echo 'gh api -X DELETE repos/o/r')"`,
	}},
	{"a trap", []string{`trap 'gh repo delete x --yes' EXIT`}},
	{"a shell alias", []string{`alias mk='gh repo create'; mk x`}},
	{"a gh alias or extension", []string{
		`gh alias set mk 'repo create' && gh mk x`, `gh extension install foo/gh-bar && gh bar create x`,
	}},
	{"GraphQL, which is read for a repository-mutation name in the line and no further", []string{
		`gh api graphql -f query="$Q"`, `gh api graphql -F query=@create.graphql`, `gh api graphql --input q.json`,
		`glab api graphql -f query='mutation{projectCreate(input:{name:"x"}){project{id}}}'`,
	}},
	{"create-on-publish (publishing to a path that does not exist yet)", []string{
		`git remote add origin git@gitlab.com:me/not-yet.git && git push -u origin main`,
	}},
	{"an inline program that builds the call from pieces, escapes or a helper", []string{
		`python3 -c "import os; os.system('g'+'h repo create x')"`,
		`python3 -c "import os,sys; os.system(' '.join(sys.argv[1:]))" gh_repo_create x`,
		`python3 -c "import os; os.system('gh\trepo\tcreate x')"`,
		`Rscript -e 'system2("gh", c("repo","create","x"))'`,
		`deno eval "new Deno.Command('gh',{args:['repo','create','x']}).outputSync()"`,
	}},
	{"a program handed to an interpreter as a positional parameter of another shell", []string{
		`sh -c 'printf %s "$0" | node' "require('child_process').execSync('gh repo delete x')"`,
	}},
	{"a script nested more than forgeRepoMaxDepth shells deep", []string{
		nestShell(forgeRepoMaxDepth+1, "gh repo create x"), nestShell(forgeRepoMaxDepth+3, "gh repo create x"),
	}},
	{"an interpreter that is not in programRunners", []string{
		`swift -e 'import Foundation; let p = Process(); p.launchPath = "/usr/bin/env"; p.arguments = ["gh","repo","create","x"]; try p.run()'`,
		`sed -n '1e gh repo create x' f.txt`,
	}},
	{"a launcher that is not in forgeLaunchers", []string{
		`docker run --rm img python3 -c "import os; os.system('gh repo create x')"`,
		`npm exec -- node -e "require('child_process').execSync('gh repo create x')"`,
		`nix run nixpkgs#python3 -- -c "import os; os.system('gh repo create x')"`,
	}},
}

// nestShell wraps s in n levels of `bash -c '...'`.
func nestShell(n int, s string) string {
	for i := 0; i < n; i++ {
		s = "bash -c '" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// The depth limit is a boundary. The loop and the guard below use a literal rather than the constant, so that
// moving the constant is a decision made here too and not a tautology; the rows of the limit above start one
// level past the boundary.
func TestPRGuard_ANestedScriptIsReadUpToTheDepthLimitAndNoFurther(t *testing.T) {
	if forgeRepoMaxDepth != 4 {
		t.Fatalf("forgeRepoMaxDepth is %d: update this test and the stated limit together", forgeRepoMaxDepth)
	}
	for n := 0; n <= 4; n++ {
		assertDeny(t, PRGuard(bashInput(nestShell(n, "gh repo create x")), nil, ""), fmt.Sprintf("%d levels deep", n))
	}
}

func TestPRGuard_TheStatedLimitsOfTheRepositoryRefusalAreAllowedAndPinned(t *testing.T) {
	for _, l := range forgeStatedLimits {
		if len(l.Pinned) == 0 {
			t.Errorf("the limit %q is named and pinned by nothing", l.Name)
		}
		for _, cmd := range l.Pinned {
			if res := PRGuard(bashInput(cmd), nil, ""); res.Stdout != "" {
				t.Errorf("%s: %q was refused: if a limit has been closed, move its rows to the refused table and remove it from forgeStatedLimits\n%s", l.Name, cmd, res.Stdout)
			}
		}
	}
}

func TestPRGuard_EveryStatedLimitIsNamedOnceAndEveryRowPinnedOnce(t *testing.T) {
	names, rows := map[string]bool{}, map[string]string{}
	for _, l := range forgeStatedLimits {
		if names[l.Name] {
			t.Errorf("the limit %q is named twice", l.Name)
		}
		names[l.Name] = true
		for _, r := range l.Pinned {
			if prev, dup := rows[r]; dup {
				t.Errorf("%q is pinned under %q and %q", r, prev, l.Name)
			}
			rows[r] = l.Name
		}
	}
}

const (
	limitsBegin = "// BEGIN STATED LIMITS (generated from forgeStatedLimits in pr_guard_repo_limits_test.go; UPDATE_LIMITS_HEADER=1 go test rewrites it)"
	limitsEnd   = "// END STATED LIMITS"
)

// renderLimitsBlock is the header text for the table: one line per limit, with how many rows pin it.
func renderLimitsBlock() string {
	var b strings.Builder
	b.WriteString(limitsBegin + "\n")
	for _, l := range forgeStatedLimits {
		fmt.Fprintf(&b, "//   - %s (%d pinned)\n", l.Name, len(l.Pinned))
	}
	b.WriteString(limitsEnd)
	return b.String()
}

// The header names the limits; it does not enumerate them by hand. Its block is the table rendered, so naming a
// limit without pinning it, or pinning one without naming it, fails here and not in review.
func TestPRGuard_TheLimitsInTheHeaderAreTheLimitsInTheTable(t *testing.T) {
	const file = "pr_guard_repo.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	i, j := strings.Index(text, limitsBegin[:strings.Index(limitsBegin, " (generated")]), strings.Index(text, limitsEnd)
	if i < 0 || j < i {
		t.Fatalf("%s has no %q ... %q block", file, limitsBegin, limitsEnd)
	}
	have, want := text[i:j+len(limitsEnd)], renderLimitsBlock()
	if have == want {
		return
	}
	if os.Getenv("UPDATE_LIMITS_HEADER") == "1" {
		if err := os.WriteFile(file, []byte(text[:i]+want+text[j+len(limitsEnd):]), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("rewrote the header block")
		return
	}
	t.Errorf("the limits named in the header of %s are not the limits in forgeStatedLimits.\nheader:\n%s\n\ntable:\n%s\n\n(UPDATE_LIMITS_HEADER=1 go test -run TheLimitsInTheHeader ./internal/hook rewrites it)", file, have, want)
}
