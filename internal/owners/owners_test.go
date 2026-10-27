package owners

import (
	"strings"
	"testing"
)

const testHome = "/home/me"

// testPolicy is the injected policy every decision-branch test shares — NEVER read from
// a real ~/.claude. hard_block refuses secrets and .env files; two owned path families;
// a clone-family collapse mapping platform-infra clones onto the canonical.
func testPolicy() *Policy {
	return &Policy{
		HardBlock: []string{"**/.env", "**/.env.*", "secrets/**"},
		Paths: []PathOwner{
			{Glob: "Projects/repo/frontend/**", Owner: "fe-agent"},
			{Glob: "Projects/platform-infra/**", Owner: "ci-agent"},
		},
		CloneFamilies: map[string][]string{
			"platform-infra": {"ci", "platform-infra"},
		},
	}
}

// edit builds an Edit Input for a $HOME-absolute path.
func edit(path, caller string) Input {
	return Input{Tool: "Edit", FilePath: testHome + "/" + path, Caller: caller, Home: testHome, Pwd: testHome, Policy: testPolicy()}
}

func TestDecide_HardBlockDeniesAllCallers(t *testing.T) {
	for _, caller := range []string{"lead", "fe-agent", "random-agent"} {
		d := edit("secrets/key.pem", caller)
		got := Decide(d)
		if got.Allow || got.Code != 2 {
			t.Errorf("caller %q on secrets/ = %+v, want DENY", caller, got)
		}
		if !strings.Contains(got.Reason, "HARD-BLOCK") {
			t.Errorf("caller %q reason = %q, want HARD-BLOCK", caller, got.Reason)
		}
	}
}

func TestDecide_EnvTemplateExemptButRealEnvBlocked(t *testing.T) {
	// A committed template is exempt from the .env hard-block → allowed (unowned).
	tmpl := Decide(edit("Projects/repo/.env.example", "fe-agent"))
	if !tmpl.Allow {
		t.Errorf(".env.example must be exempt from the hard-block, got %+v", tmpl)
	}
	// A real secret file is still blocked.
	real := Decide(edit("Projects/repo/.env.local", "fe-agent"))
	if real.Allow || real.Code != 2 {
		t.Errorf(".env.local must stay hard-blocked, got %+v", real)
	}
}

func TestDecide_LeadAllowedAnywhereNotHardBlocked(t *testing.T) {
	// Another agent's owned path.
	owned := Decide(edit("Projects/repo/frontend/app.tsx", "lead"))
	if !owned.Allow || !strings.Contains(owned.Reason, "ALLOW-LEAD") {
		t.Errorf("lead on an owned path = %+v, want ALLOW-LEAD", owned)
	}
	// An unowned path.
	unowned := Decide(edit("Projects/repo/docs/readme.md", "lead"))
	if !unowned.Allow || !strings.Contains(unowned.Reason, "ALLOW-LEAD") {
		t.Errorf("lead on an unowned path = %+v, want ALLOW-LEAD", unowned)
	}
}

func TestDecide_OwnerAllowedOnOwnPath(t *testing.T) {
	got := Decide(edit("Projects/repo/frontend/app.tsx", "fe-agent"))
	if !got.Allow || !strings.Contains(got.Reason, "ALLOW-OWNED") {
		t.Errorf("owner on own path = %+v, want ALLOW-OWNED", got)
	}
}

func TestDecide_NonOwnerDeniedWithHandoff(t *testing.T) {
	got := Decide(edit("Projects/repo/frontend/app.tsx", "backend-agent"))
	if got.Allow || got.Code != 2 {
		t.Fatalf("non-owner on owned path = %+v, want DENY", got)
	}
	if !strings.Contains(got.Reason, "owned by 'fe-agent'") || !strings.Contains(got.Reason, "TaskCreate(owner=fe-agent)") {
		t.Errorf("deny reason = %q, want the handoff message", got.Reason)
	}
}

func TestDecide_UnownedAllowedAndWarned(t *testing.T) {
	got := Decide(edit("Projects/repo/docs/readme.md", "backend-agent"))
	if !got.Allow || got.Code != 0 {
		t.Fatalf("unowned path = %+v, want ALLOW", got)
	}
	if !strings.Contains(got.Reason, "ALLOW-UNOWNED") {
		t.Errorf("unowned reason = %q, want the ALLOW-UNOWNED warn marker", got.Reason)
	}
}

func TestDecide_BypassOverridesEvenHardBlock(t *testing.T) {
	in := edit("secrets/key.pem", "backend-agent")
	in.Bypass = true
	got := Decide(in)
	if !got.Allow || !strings.Contains(got.Reason, "BYPASS") {
		t.Errorf("bypass on a hard-blocked path = %+v, want ALLOW (BYPASS, precedence over hard-block)", got)
	}
}

func TestDecide_CloneFamilyCollapseAntiEscape(t *testing.T) {
	// A brand-new clone name the config never enumerated: Projects/platform-infra-newclone
	// must collapse onto Projects/platform-infra and match the ci-agent owner — the exact
	// escape the hardcoded shell couldn't prevent.
	owned := Decide(edit("Projects/platform-infra-newclone-42/app.go", "ci-agent"))
	if !owned.Allow || !strings.Contains(owned.Reason, "ALLOW-OWNED") {
		t.Errorf("owner on a new-clone path = %+v, want ALLOW-OWNED via clone collapse", owned)
	}
	// The wrong owner on the same collapsed path is denied — proving the collapse maps
	// ownership rather than just allowing.
	denied := Decide(edit("Projects/platform-infra-newclone-42/app.go", "fe-agent"))
	if denied.Allow || denied.Code != 2 {
		t.Errorf("non-owner on a new-clone path = %+v, want DENY via clone collapse", denied)
	}
	// WITHOUT clone families the same path would NOT match the canonical owner glob →
	// unowned (the escape the collapse closes).
	noFamilies := edit("Projects/platform-infra-newclone-42/app.go", "fe-agent")
	noFamilies.Policy.CloneFamilies = nil
	esc := Decide(noFamilies)
	if !esc.Allow || !strings.Contains(esc.Reason, "ALLOW-UNOWNED") {
		t.Errorf("without clone families the new clone escapes to unowned = %+v", esc)
	}
}

func TestDecide_BashDestructiveCrossBoundaryDenied(t *testing.T) {
	base := func(cmd string) Input {
		return Input{Tool: "Bash", Command: cmd, Caller: "backend-agent", Home: testHome, Pwd: testHome, Policy: testPolicy()}
	}
	for _, cmd := range []string{
		"rm Projects/repo/frontend/app.tsx",
		"git rm Projects/repo/frontend/app.tsx",
		"cd x && mv Projects/repo/frontend/app.tsx /tmp/x",
	} {
		got := Decide(base(cmd))
		if got.Allow || got.Code != 2 {
			t.Errorf("Bash %q = %+v, want DENY (cross-boundary deletion/move)", cmd, got)
		}
	}
	// mv entirely outside any owned tree → allowed.
	if got := Decide(base("mv /tmp/a /tmp/b")); !got.Allow {
		t.Errorf("mv /tmp/a /tmp/b = %+v, want ALLOW", got)
	}
	// A Bash command with no destructive verb → allowed (this hook only guards deletes/moves).
	if got := Decide(base("cat Projects/repo/frontend/app.tsx")); !got.Allow {
		t.Errorf("non-destructive Bash = %+v, want ALLOW", got)
	}
}

func TestDecide_AbsentPolicyAllowsAll(t *testing.T) {
	got := Decide(Input{Tool: "Edit", FilePath: testHome + "/secrets/key.pem", Caller: "backend-agent", Home: testHome, Policy: nil})
	if !got.Allow || got.Code != 0 {
		t.Errorf("nil policy (boundary disabled) = %+v, want ALLOW-ALL", got)
	}
	if !strings.Contains(got.Reason, "DISABLED") {
		t.Errorf("nil policy reason = %q, want the DISABLED marker", got.Reason)
	}
}

func TestDecide_UnknownToolAndEmptyPathFailOpen(t *testing.T) {
	if got := Decide(Input{Tool: "WebFetch", Caller: "x", Home: testHome, Policy: testPolicy()}); !got.Allow {
		t.Errorf("unknown tool must fail open, got %+v", got)
	}
	if got := Decide(Input{Tool: "Edit", FilePath: "", Caller: "x", Home: testHome, Policy: testPolicy()}); !got.Allow {
		t.Errorf("empty file_path must fail open, got %+v", got)
	}
}

func TestExtractDestructivePaths(t *testing.T) {
	cases := map[string][]string{
		"rm -rf a b":              {"a", "b"},
		"rm":                      nil,
		"git rm -f x":             {"x"},
		"rmdir d":                 {"d"},
		"mv src dst":              {"src", "dst"},
		"echo hi; rm a && mv b c": {"a", "b", "c"},
		"cat x":                   nil,
		"ls -la":                  nil,
	}
	for cmd, want := range cases {
		var got []string
		for _, tg := range ExtractDestructivePaths(cmd, testHome, testHome) {
			if tg.Unresolved {
				t.Errorf("ExtractDestructivePaths(%q) left %q unresolved", cmd, tg.Path)
			}
			got = append(got, tg.Path)
		}
		if len(got) != len(want) {
			t.Errorf("ExtractDestructivePaths(%q) = %v, want %v", cmd, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("ExtractDestructivePaths(%q)[%d] = %q, want %q", cmd, i, got[i], want[i])
			}
		}
	}
}

func TestMatchGlob_CrossSlash(t *testing.T) {
	if !matchGlob("Projects/repo/**", "Projects/repo/a/b/c.go") {
		t.Error("`*` must span `/` (shell case-glob semantics)")
	}
	if matchGlob("Projects/repo/*.go", "Projects/repo/a/b.go") == false {
		t.Error("`*` spanning `/` must match a nested .go too")
	}
	if matchGlob("secrets/**", "public/x") {
		t.Error("must not match a different prefix")
	}
}

func TestResolveCaller(t *testing.T) {
	if got := ResolveCaller("env-agent", "file-agent", "lead"); got != "env-agent" {
		t.Errorf("env wins, got %q", got)
	}
	if got := ResolveCaller("", "file-agent\n", "lead"); got != "file-agent" {
		t.Errorf(".active-agent wins when env empty (trimmed), got %q", got)
	}
	if got := ResolveCaller("", "", ""); got != DefaultLead {
		t.Errorf("empty all → default lead, got %q", got)
	}
}

func TestParsePolicy(t *testing.T) {
	data := []byte(`
hard_block:
  - "**/.env"
paths:
  - glob: "Projects/repo/frontend/**"
    owner: fe-agent
clone_families:
  platform-infra: [ci, platform-infra]
`)
	p, err := ParsePolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HardBlock) != 1 || len(p.Paths) != 1 || p.Paths[0].Owner != "fe-agent" {
		t.Errorf("parsed policy = %+v, want the hard_block/paths shape", p)
	}
	if got := p.CloneFamilies["platform-infra"]; len(got) != 2 || got[0] != "ci" {
		t.Errorf("clone_families = %v, want [ci platform-infra]", got)
	}
}

// TestRelFor pins the REL each spelling of a path reduces to.
func TestRelFor(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	cases := []struct {
		name, path, home, pwd string
		want                  string
	}{
		{"plain relative", "sub/x.go", testHome, pwd, "Projects/a/sub/x.go"},
		{"dotdot into a sibling", "../b/x", testHome, pwd, "Projects/b/x"},
		{"dotdot up to home", "../../x", testHome, pwd, "x"},
		{"absolute with dotdot", testHome + "/Projects/a/../b/x", testHome, pwd, "Projects/b/x"},
		{"dot segment", "./x", testHome, pwd, "Projects/a/x"},
		{"absolute dot segment", testHome + "/Projects/a/./x", testHome, pwd, "Projects/a/x"},
		{"doubled slash", "sub//x", testHome, pwd, "Projects/a/sub/x"},
		{"absolute doubled slash after home", testHome + "//Projects/a/x", testHome, pwd, "Projects/a/x"},
		{"absolute outside home", "/tmp/x", testHome, pwd, "/tmp/x"},
		{"absolute outside home with dotdot", "/tmp/../etc/x", testHome, pwd, "/etc/x"},
		{"dotdot out of home stays absolute", "../../../../x", testHome, pwd, "/x"},
		{"home with trailing slash", "sub/x", testHome + "/", pwd, "Projects/a/sub/x"},
		{"empty home strips nothing", "sub/x", "", pwd, pwd + "/sub/x"},
		// Directory spellings keep a trailing slash.
		{"trailing slash", "sub/", testHome, pwd, "Projects/a/sub/"},
		{"trailing dot", "sub/.", testHome, pwd, "Projects/a/sub/"},
		{"bare dot", ".", testHome, pwd, "Projects/a/"},
		{"trailing dotdot", "sub/..", testHome, pwd, "Projects/a/"},
		{"absolute trailing slash", testHome + "/Projects/a/sub/", testHome, pwd, "Projects/a/sub/"},
		{"no trailing slash stays none", "sub", testHome, pwd, "Projects/a/sub"},
	}
	for _, c := range cases {
		got, ok := relFor(c.path, c.home, c.pwd)
		if !ok || got != c.want {
			t.Errorf("%s: relFor(%q, %q, %q) = (%q, %v), want (%q, true)", c.name, c.path, c.home, c.pwd, got, ok, c.want)
		}
	}
}

func TestRelFor_RelativePathWithoutAbsolutePwdIsNotPlaced(t *testing.T) {
	for _, pwd := range []string{"", ".", "Projects/a"} {
		if got, ok := relFor("x.go", testHome, pwd); ok {
			t.Errorf("relFor(relative, pwd=%q) = (%q, true), want ok=false", pwd, got)
		}
	}
	// An absolute path needs no pwd.
	if got, ok := relFor(testHome+"/Projects/a/x.go", testHome, ""); !ok || got != "Projects/a/x.go" {
		t.Errorf("absolute path with empty pwd = (%q, %v), want (Projects/a/x.go, true)", got, ok)
	}
}

// pathPolicy is a policy with one owned tree and one hard-blocked tree, both under the
// same project, for the spelling-equivalence tests.
func pathPolicy() *Policy {
	return &Policy{
		HardBlock: []string{"Projects/b/secrets/**"},
		Paths:     []PathOwner{{Glob: "Projects/b/tasks/**", Owner: "tasks-agent"}},
	}
}

func decideAt(tool, path, caller, pwd string) Decision {
	in := Input{Tool: tool, Caller: caller, Home: testHome, Pwd: pwd, Policy: pathPolicy()}
	if tool == "Bash" {
		in.Command = path
	} else {
		in.FilePath = path
	}
	return Decide(in)
}

// A non-owner editing an owned file must be refused whichever way the path is spelled.
// The plain spelling is asserted to DENY first, so the equivalence below is not two
// allows comparing equal.
func TestDecide_OwnedPathSpellingsReachTheSameVerdictAsTheCleanOne(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	plain := decideAt("Edit", testHome+"/Projects/b/tasks/1.md", "other", pwd)
	if plain.Allow || plain.Code != 2 || !strings.Contains(plain.Reason, "owned by 'tasks-agent'") {
		t.Fatalf("control: the plain spelling must be refused as owned, got %+v", plain)
	}
	for _, path := range []string{
		"../b/tasks/1.md",
		"../a/../b/tasks/1.md",
		"./../b/tasks/1.md",
		"../b/./tasks/1.md",
		"../b//tasks/1.md",
		testHome + "/Projects/a/../b/tasks/1.md",
		testHome + "/Projects/b/./tasks/1.md",
		testHome + "//Projects/b/tasks/1.md",
		testHome + "/Projects/b/x/../tasks/1.md",
	} {
		if got := decideAt("Edit", path, "other", pwd); got != plain {
			t.Errorf("Edit %q = %+v, want the clean spelling's verdict %+v", path, got, plain)
		}
	}
	// The same through Bash deletion, whose tokens take the same route.
	for _, cmd := range []string{"rm ../b/tasks/1.md", "mv ../b/tasks/1.md /tmp/x", "rm -rf " + testHome + "/Projects/a/../b/tasks/1.md"} {
		if got := decideAt("Bash", cmd, "other", pwd); got.Allow || got.Code != 2 {
			t.Errorf("Bash %q = %+v, want DENY", cmd, got)
		}
	}
	// The owner is still let through by the same spellings.
	if got := decideAt("Edit", "../b/tasks/1.md", "tasks-agent", pwd); !got.Allow || !strings.Contains(got.Reason, "ALLOW-OWNED Projects/b/tasks/1.md") {
		t.Errorf("owner via ../ = %+v, want ALLOW-OWNED on the cleaned REL", got)
	}
}

// Every spelling of a hard-blocked path is refused for the lead.
func TestDecide_HardBlockSpellingsReachTheSameVerdictAsTheCleanOne(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	plain := decideAt("Edit", testHome+"/Projects/b/secrets/k", "lead", pwd)
	if plain.Allow || !strings.Contains(plain.Reason, "HARD-BLOCK") {
		t.Fatalf("control: the plain spelling must be hard-blocked for the lead, got %+v", plain)
	}
	for _, path := range []string{
		"../b/secrets/k",
		testHome + "/Projects/a/../b/secrets/k",
		"./../b/secrets/k",
		"../../Projects/b/secrets/k",
	} {
		if got := decideAt("Edit", path, "lead", pwd); got != plain {
			t.Errorf("Edit %q = %+v, want the clean spelling's verdict %+v", path, got, plain)
		}
	}
}

// A `..` out of the owned tree reads as unowned.
func TestDecide_DotDotOutOfOwnedTreeIsNoLongerOwned(t *testing.T) {
	const pwd = testHome + "/Projects/b"
	for _, path := range []string{"tasks/../docs/a.md", testHome + "/Projects/b/tasks/../docs/a.md"} {
		got := decideAt("Edit", path, "other", pwd)
		if !got.Allow || got.Reason != "ALLOW-UNOWNED Projects/b/docs/a.md" {
			t.Errorf("Edit %q = %+v, want ALLOW-UNOWNED Projects/b/docs/a.md", path, got)
		}
	}
}

// Directory spellings that matched the owned glob before cleaning must still match.
func TestDecide_DirectorySpellingsStayOwned(t *testing.T) {
	for _, c := range []struct{ path, pwd string }{
		{"tasks/", testHome + "/Projects/b"},
		{"tasks/.", testHome + "/Projects/b"},
		{".", testHome + "/Projects/b/tasks"},
		{"../tasks/", testHome + "/Projects/b/docs"},
		{testHome + "/Projects/b/tasks/", testHome},
	} {
		got := decideAt("Bash", "rm -r "+c.path, "other", c.pwd)
		if got.Allow || got.Code != 2 {
			t.Errorf("rm -r %q from %q = %+v, want DENY (owned directory)", c.path, c.pwd, got)
		}
	}
}

// A path that cleans out of $HOME reads as unowned and is allowed, as an absolute path
// elsewhere does.
func TestDecide_PathCleaningOutOfHomeStaysUnowned(t *testing.T) {
	got := decideAt("Edit", "../../../../tmp/x", "other", testHome+"/Projects/a")
	if !got.Allow || got.Reason != "ALLOW-UNOWNED /tmp/x" {
		t.Errorf("got %+v, want ALLOW-UNOWNED /tmp/x", got)
	}
}

// A relative path with no absolute pwd is refused, for the lead too, unless bypassed.
func TestDecide_UnplaceableRelativePathIsRefused(t *testing.T) {
	for _, caller := range []string{"other", "lead"} {
		got := decideAt("Edit", "x.go", caller, "")
		if got.Allow || got.Code != 2 || !strings.Contains(got.Reason, "cannot place 'x.go'") {
			t.Errorf("caller %q, relative path, no pwd = %+v, want DENY (cannot place)", caller, got)
		}
	}
	if got := decideAt("Bash", "rm x.go", "other", ""); got.Allow {
		t.Errorf("Bash rm of a relative path, no pwd = %+v, want DENY", got)
	}
	in := Input{Tool: "Edit", FilePath: "x.go", Caller: "lead", Home: testHome, Policy: pathPolicy(), Bypass: true}
	if got := Decide(in); !got.Allow || !strings.HasPrefix(got.Reason, "BYPASS") {
		t.Errorf("bypass must still override, got %+v", got)
	}
	// An absolute path needs no pwd and is judged as ever.
	if got := decideAt("Edit", testHome+"/Projects/b/tasks/1.md", "other", ""); got.Allow || !strings.Contains(got.Reason, "owned by") {
		t.Errorf("absolute path with no pwd = %+v, want the ownership DENY", got)
	}
}

// The words of a destructive command, as the shell would hand them to rm.
func TestExtractDestructivePaths_ShellWordsAreExpandedBeforeJudging(t *testing.T) {
	const pwd = testHome + "/Projects/b"
	cases := []struct {
		cmd  string
		want []string // Unresolved words are written with a leading "?"
	}{
		{`rm ~/Projects/b/tasks/1.md`, []string{testHome + "/Projects/b/tasks/1.md"}},
		{`rm ~`, []string{testHome}},
		{`rm $HOME/Projects/b/tasks/1.md`, []string{testHome + "/Projects/b/tasks/1.md"}},
		{`rm ${HOME}/x`, []string{testHome + "/x"}},
		{`rm "$HOME/x"`, []string{testHome + "/x"}},
		{`rm "$HOME"/x`, []string{testHome + "/x"}},
		{`rm $PWD/tasks/1.md ${PWD}/y ~+/z`, []string{pwd + "/tasks/1.md", pwd + "/y", pwd + "/z"}},
		{`rm "tasks/1.md"`, []string{"tasks/1.md"}},
		{`rm 'tasks/1.md'`, []string{"tasks/1.md"}},
		{`rm ta"sks"/'1'.md`, []string{"tasks/1.md"}},
		{`rm tasks/\1.md`, []string{"tasks/1.md"}},
		{`rm "my tasks/1.md"`, []string{"my tasks/1.md"}},
		{`rm my\ tasks/1.md`, []string{"my tasks/1.md"}},
		{`"rm" tasks/1.md`, []string{"tasks/1.md"}},
		{`\rm tasks/1.md`, []string{"tasks/1.md"}},
		{`git "rm" -f 'x y'`, []string{"x y"}},
		{`mv '-x' "$HOME/y"`, []string{testHome + "/y"}},
		// Quoting protects: nothing is expanded inside '…', and a quoted ~ is not a home.
		{`rm '$HOME/x' "~/y" \~/z '\'`, []string{`$HOME/x`, `~/y`, `~/z`, `\`}},
		{`rm "a\"b" "c\$HOME"`, []string{`a"b`, `c$HOME`}},
		// A quoted flag is still a flag; an empty word names nothing.
		{`rm "-rf" '' ""`, nil},
		// Unknowable expansions are flagged, not guessed.
		{`rm $X/1.md`, []string{"?$X/1.md"}},
		{`rm ${X}/1.md "$Y"`, []string{"?${X}/1.md", "?$Y"}},
		{`rm ${HOME:-/tmp}/x`, []string{"?${HOME:-/tmp}/x"}},
		{`rm ~bob/x ~-/y`, []string{"?~bob/x", "?~-/y"}},
		{`rm $(echo tasks)/1.md`, []string{"?$(echo", "tasks)/1.md"}},
		{"rm `echo tasks`/1.md", []string{"?`echo", "?tasks`/1.md"}},
		{`rm $'tasks/1.md' $1 $@ $*`, []string{"?$tasks/1.md", "?$1", "?$@", "?$*"}},
		{`rm "$HOMEDIR/x" $HOME_X`, []string{"?$HOMEDIR/x", "?$HOME_X"}},
		// A flag that is spelled by an expansion might be a path.
		{`rm -$F`, []string{"?-$F"}},
		{`rm $F tasks/1.md`, []string{"?$F", "tasks/1.md"}},
		// A literal dollar is not an expansion.
		{`rm a$ '$' "$"`, []string{"a$", "$", "$"}},
		{`rm "a$'b" a~/x`, []string{"a$'b", "a~/x"}},
	}
	for _, c := range cases {
		var got []string
		for _, tg := range ExtractDestructivePaths(c.cmd, testHome, pwd) {
			if tg.Unresolved {
				got = append(got, "?"+tg.Path)
			} else {
				got = append(got, tg.Path)
			}
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") || len(got) != len(c.want) {
			t.Errorf("ExtractDestructivePaths(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
	// Without a home or pwd to expand against, the word is unresolved, not empty.
	for _, cmd := range []string{"rm ~/x", "rm $HOME/x", "rm ${HOME}/x"} {
		got := ExtractDestructivePaths(cmd, "", pwd)
		if len(got) != 1 || !got[0].Unresolved {
			t.Errorf("ExtractDestructivePaths(%q, home=\"\") = %+v, want one unresolved word", cmd, got)
		}
	}
	if got := ExtractDestructivePaths("rm $PWD/x", testHome, ""); len(got) != 1 || !got[0].Unresolved {
		t.Errorf("rm $PWD/x with no pwd = %+v, want one unresolved word", got)
	}
}

// Every Bash spelling of an owned file reaches the verdict of the plain one: the word is
// expanded, THEN cleaned, THEN matched.
func TestDecide_BashExpansionSpellingsReachTheSameVerdictAsTheCleanOne(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	plain := decideAt("Bash", "rm ../b/tasks/1.md", "other", pwd)
	if plain.Allow || plain.Code != 2 || !strings.Contains(plain.Reason, "owned by 'tasks-agent'") {
		t.Fatalf("control: the plain spelling must be refused as owned, got %+v", plain)
	}
	for _, cmd := range []string{
		"rm ~/Projects/b/tasks/1.md",
		"rm $HOME/Projects/b/tasks/1.md",
		"rm ${HOME}/Projects/b/tasks/1.md",
		`rm "$HOME/Projects/b/tasks/1.md"`,
		`rm "$HOME"/Projects/b/tasks/1.md`,
		`rm "../b/tasks/1.md"`,
		`rm '../b/tasks/1.md'`,
		`rm ../b/"tasks"/'1.md'`,
		`rm ../b/tasks/\1.md`,
		`"rm" ../b/tasks/1.md`,
		`\rm ../b/tasks/1.md`,
		`rm $PWD/../b/tasks/1.md`,
		`rm ~+/../b/tasks/1.md`,
		// The expansion introduces `..` and a second prefix: cleaned after expansion.
		"rm ~/Projects/a/../b/tasks/1.md",
		"rm $HOME/../me/Projects/b/tasks/1.md",
		"rm ~/../me/Projects/b/./tasks/1.md",
		"rm $HOME//Projects/b/tasks/1.md",
		`mv ~/Projects/b/tasks/1.md /tmp/x`,
		`git rm -f "$HOME/Projects/b/tasks/1.md"`,
	} {
		if got := decideAt("Bash", cmd, "other", pwd); got != plain {
			t.Errorf("Bash %q = %+v, want the clean spelling's verdict %+v", cmd, got, plain)
		}
	}
	// The owner is still let through by an expanded spelling.
	if got := decideAt("Bash", "rm ~/Projects/b/tasks/1.md", "tasks-agent", pwd); !got.Allow || got.Reason != "ALLOW-OWNED Projects/b/tasks/1.md" {
		t.Errorf("owner via ~ = %+v, want ALLOW-OWNED on the expanded REL", got)
	}
}

func TestDecide_BashExpansionSpellingsOfAHardBlockedFileAreRefusedForTheLead(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	plain := decideAt("Bash", "rm ../b/secrets/k", "lead", pwd)
	if plain.Allow || !strings.Contains(plain.Reason, "HARD-BLOCK") {
		t.Fatalf("control: the plain spelling must be hard-blocked for the lead, got %+v", plain)
	}
	for _, cmd := range []string{
		"rm ~/Projects/b/secrets/k",
		"rm $HOME/Projects/b/secrets/k",
		`rm "$HOME/Projects/b/secrets/k"`,
		`rm '../b/secrets/k'`,
		"rm ~/Projects/a/../b/secrets/k",
	} {
		if got := decideAt("Bash", cmd, "lead", pwd); got != plain {
			t.Errorf("lead Bash %q = %+v, want the clean spelling's verdict %+v", cmd, got, plain)
		}
	}
}

// What the expansion rules must NOT refuse, so the guard is not simply refusing everything.
func TestDecide_BashExpansionDoesNotOverRefuse(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	for _, cmd := range []string{
		"rm ~/Projects/b/docs/a.md",
		"rm $HOME/Projects/b/docs/a.md",
		`rm "$HOME/Projects/b/docs/a.md"`,
		"rm ~/Projects/b/tasks/../docs/a.md",
		"rm $HOME/Projects/b/tasks/../docs/a.md",
		"rm ~/notes.txt",
		"rm $HOME",
		// Single quotes and a quoted tilde are literal: these name files called `$HOME` and
		// `~` under the cwd, which no glob owns.
		`rm '$HOME/Projects/b/tasks/1.md'`,
		`rm "~/Projects/b/tasks/1.md"`,
		`rm \$HOME/Projects/b/tasks/1.md`,
		// Not a destructive verb: an unresolvable word elsewhere is not this guard's business.
		`echo $UNSET ~bob $(date)`,
		`cat "$HOME/Projects/b/tasks/1.md"`,
		`FOO=$X ls`,
		`echo "rm"; ls $X`,
	} {
		if got := decideAt("Bash", cmd, "other", pwd); !got.Allow {
			t.Errorf("Bash %q = %+v, want ALLOW", cmd, got)
		}
	}
}

// An expansion this package cannot evaluate is refused for every caller, since the unowned
// fallback would allow it; the bypass sentinel still overrides.
func TestDecide_BashUnresolvableExpansionFailsClosed(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	for _, cmd := range []string{
		"rm $X/1.md",
		`rm "$TARGET"`,
		"rm ${X}",
		"rm ${HOME:-/tmp}/x",
		"rm ~bob/x",
		"rm $(echo ../b/tasks)/1.md",
		"rm `echo ../b/tasks`/1.md",
		"rm $'../b/tasks/1.md'",
		"rm -rf $F",
		"mv $A $B",
		"git rm $X",
		// A harmless word first must not mask the unresolvable one.
		"rm ok.txt $X",
	} {
		for _, caller := range []string{"other", "lead", "tasks-agent"} {
			got := decideAt("Bash", cmd, caller, pwd)
			if got.Allow || got.Code != 2 || !strings.Contains(got.Reason, "cannot resolve") {
				t.Errorf("caller %q Bash %q = %+v, want DENY (cannot resolve)", caller, cmd, got)
			}
		}
		in := Input{Tool: "Bash", Command: cmd, Caller: "lead", Home: testHome, Pwd: pwd, Policy: pathPolicy(), Bypass: true}
		if got := Decide(in); !got.Allow || !strings.HasPrefix(got.Reason, "BYPASS") {
			t.Errorf("bypass on %q = %+v, want BYPASS", cmd, got)
		}
	}
	in := Input{Tool: "Bash", Command: "rm ~/x", Caller: "other", Home: "", Pwd: pwd, Policy: pathPolicy()}
	if got := Decide(in); got.Allow || !strings.Contains(got.Reason, "cannot resolve") {
		t.Errorf("rm ~/x with no HOME = %+v, want DENY (cannot resolve)", got)
	}
	// The edit family has no shell: a `~` or `$X` in file_path is just a name.
	if got := decideAt("Edit", "$X/1.md", "other", pwd); !got.Allow {
		t.Errorf("Edit of a file literally named $X = %+v, want ALLOW", got)
	}
}

// The words the narrowing round stops reading as paths. Every row has a counterpart that
// still fires (TestDecide_BashNarrowingStillRefusesWhatItShould).
func TestExtractDestructivePaths_WordsThatCannotBePathsAreNotCandidates(t *testing.T) {
	const pwd = testHome + "/Projects/b"
	cases := []struct {
		cmd  string
		want []string // Unresolved words are written with a leading "?"
	}{
		// $$ is the shell's PID, which the command's author cannot force to a value.
		{`rm -f /tmp/foo-$$`, []string{"/tmp/foo-0"}},
		{`rm "/tmp/foo-$$"`, []string{"/tmp/foo-0"}},
		// $! (empty with no background job), $? (steerable in the same command) and $# (steerable
		// by argument count) CAN be forced, so `.env$!` is `.env`: unknowable, like a named variable.
		{`rm x.$? y$# z$!`, []string{"?x.$?", "?y$#", "?z$!"}},
		{`rm .env$! "k.pem$!"`, []string{"?.env$!", "?k.pem$!"}},
		// A positional, $@, $* and a named variable are unknowable too.
		{`rm $1 $@ $* $X`, []string{"?$1", "?$@", "?$*", "?$X"}},
		// A comment is not an argument; it ends at the newline, not at a `;`.
		{`rm foo.txt # remove $OLD`, []string{"foo.txt"}},
		{`rm foo.txt #remove $OLD; rm $X`, []string{"foo.txt"}},
		{"rm foo.txt # c\nbar.txt", []string{"foo.txt", "bar.txt"}},
		{`rm "it's" # don't $X`, []string{"it's"}},
		// A `#` that does not start a word is part of the word.
		{`rm a#b "#c" \#d`, []string{"a#b", "#c", "#d"}},
		// A redirection and its operand are not arguments, wherever they sit.
		{`rm x > "$OUT"`, []string{"x"}},
		{`rm x >"$OUT"`, []string{"x"}},
		{`rm x>$OUT y`, []string{"x", "y"}},
		{`rm x 2>"$E" y`, []string{"x", "y"}},
		{`rm x >> $LOG`, []string{"x"}},
		{`rm x < $IN`, []string{"x"}},
		{`rm x 2>&1 y`, []string{"x", "y"}},
		{`rm x >&2`, []string{"x"}},
		{`rm x &>$OUT`, []string{"x"}},
		{`rm x &>$OUT tasks/1.md`, []string{"x", "tasks/1.md"}},
		{`rm x &>> $OUT; rm y`, []string{"x", "y"}},
		{`rm x >| $OUT`, []string{"x"}},
		{`rm x <<< "$IN"`, []string{"x"}},
		{`rm >$OUT x`, []string{"x"}},
		// A quoted digit is a file name, not an fd number.
		{`rm "2">f`, []string{"2"}},
		{`rm x 2 > f`, []string{"x", "2"}},
		// <(…) is a word, and unknowable.
		{`rm <(echo a)`, []string{"?<(echo", "a)"}},
		// A redirect operand is exactly one word.
		{`rm x > out $X`, []string{"x", "?$X"}},
		{`rm x > out tasks/1.md`, []string{"x", "tasks/1.md"}},
	}
	for _, c := range cases {
		var got []string
		for _, tg := range ExtractDestructivePaths(c.cmd, testHome, pwd) {
			if tg.Unresolved {
				got = append(got, "?"+tg.Path)
			} else {
				got = append(got, tg.Path)
			}
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") || len(got) != len(c.want) {
			t.Errorf("ExtractDestructivePaths(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// The segment split is quote-aware: a separator inside quotes neither invents a verb nor
// ends an argument list early; one inside a substitution still starts a new command.
func TestExtractDestructivePaths_SegmentSplitIsQuoteAware(t *testing.T) {
	const pwd = testHome + "/Projects/b"
	cases := []struct {
		cmd  string
		want []string
	}{
		// No verb is invented from prose after a quoted `;`, `&` or `|`.
		{`git commit -m "fix: tidy; rm $TMP handled"`, nil},
		{`git commit -m 'a; rm tasks/1.md'`, nil},
		{`echo "x && rm tasks/1.md | rm y"`, nil},
		{`echo a\; rm tasks/1.md`, nil},
		// A quoted separator does not cut the argument list and drop what follows it.
		{`rm "a;b" tasks/1.md`, []string{"a;b", "tasks/1.md"}},
		{`rm 'a|b' c\&d tasks/1.md`, []string{"a|b", "c&d", "tasks/1.md"}},
		{`mv "a&b" "c;d"`, []string{"a&b", "c;d"}},
		// A real separator still splits, before and after quoted text.
		{`echo "a;b"; rm tasks/1.md`, []string{"tasks/1.md"}},
		{`echo 'a' && rm "x;y" || rm z`, []string{"x;y", "z"}},
		// The text of a substitution is commands, even inside double quotes.
		{`echo "$(true; rm tasks/1.md)"`, []string{"tasks/1.md)"}},
		{"echo \"`true; rm tasks/1.md`\"", []string{"?tasks/1.md`"}},
		{`echo $(true; rm tasks/1.md)`, []string{"tasks/1.md)"}},
		{`(true; rm tasks/1.md)`, []string{"tasks/1.md)"}},
		// ...and the quote that closes a substitution returns to the enclosing "…".
		{`echo "$(date)"; rm "a;b"`, []string{"a;b"}},
		{`echo "$(echo "x;y")z;w"; rm q`, []string{"q"}},
		// An unterminated quote runs to the end: nothing after it is a verb.
		{`echo "oops; rm tasks/1.md`, nil},
	}
	for _, c := range cases {
		var got []string
		for _, tg := range ExtractDestructivePaths(c.cmd, testHome, pwd) {
			if tg.Unresolved {
				got = append(got, "?"+tg.Path)
			} else {
				got = append(got, tg.Path)
			}
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") || len(got) != len(c.want) {
			t.Errorf("ExtractDestructivePaths(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// What the narrowing round stops refusing, each paired with a spelling that still fires.
func TestDecide_BashNarrowingDoesNotRefuseWordsThatCannotBePaths(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	for _, cmd := range []string{
		"rm -f /tmp/foo-$$",
		"rm foo.txt # remove $OLD",
		"rm foo.txt # remove $OLD; rm $X",
		`rm x > "$OUT"`,
		`rm x >"$OUT"`,
		`rm x 2>"$ERR"`,
		`rm x >> $LOG 2>&1`,
		`rm x < $IN`,
		`git commit -m "fix: tidy; rm $TMP handled"`,
		`git commit -m 'also: rm ../b/tasks/1.md'`,
		`echo "done | rm ../b/tasks/1.md"`,
		`rm "a;b" docs/1.md`,
	} {
		if got := decideAt("Bash", cmd, "other", pwd); !got.Allow {
			t.Errorf("Bash %q = %+v, want ALLOW", cmd, got)
		}
	}
}

func TestDecide_BashNarrowingStillRefusesWhatItShould(t *testing.T) {
	const pwd = testHome + "/Projects/a"
	owned := func(cmd string) {
		t.Helper()
		got := decideAt("Bash", cmd, "other", pwd)
		if got.Allow || got.Code != 2 || !strings.Contains(got.Reason, "owned by 'tasks-agent'") {
			t.Errorf("Bash %q = %+v, want the ownership DENY", cmd, got)
		}
	}
	unresolved := func(cmd string) {
		t.Helper()
		for _, caller := range []string{"other", "lead"} {
			got := decideAt("Bash", cmd, caller, pwd)
			if got.Allow || got.Code != 2 || !strings.Contains(got.Reason, "cannot resolve") {
				t.Errorf("caller %q Bash %q = %+v, want DENY (cannot resolve)", caller, cmd, got)
			}
		}
	}
	// Only $$ is safe; any other parameter stays unresolved, including the specials the
	// command's author can force ($! empty, $? steerable, $# by argument count).
	unresolved("rm -f /tmp/foo-$X")
	unresolved("rm -f /tmp/foo.$?")
	unresolved("rm -f /tmp/foo$#")
	unresolved("rm -f /tmp/foo$!")
	unresolved("rm $1")
	unresolved("rm $@")
	unresolved("rm /tmp/$$/$X")
	// Comments: only from a word start, and only to the end of the line.
	unresolved("rm a#b $X")
	unresolved("rm a # c\nrm $X")
	unresolved(`rm "a # b" $X`)
	owned("rm a # c\nrm ../b/tasks/1.md")
	owned("rm a#b ../b/tasks/1.md")
	// Redirects: the operand is one word, and a redirect does not hide a later argument.
	unresolved(`rm x > out $X`)
	unresolved(`rm x 2>&1 $X`)
	unresolved(`rm x <(echo a)`)
	unresolved(`rm "2">f $X`)
	owned(`rm x > out ../b/tasks/1.md`)
	owned(`rm x 2>&1 ../b/tasks/1.md`)
	owned(`rm x >&2 ../b/tasks/1.md`)
	owned(`rm ../b/tasks/1.md > "$OUT"`)
	owned(`rm ../b/tasks/1.md 2>/dev/null`)
	// Quote-aware split: a real verb is still found, and a quoted separator does not drop the argument after it.
	owned(`rm "a;b" ../b/tasks/1.md`)
	owned(`rm 'a|b' ../b/tasks/1.md`)
	owned(`echo "a;b"; rm ../b/tasks/1.md`)
	owned(`echo "$(true; rm ../b/tasks/1.md)"`)
	unresolved("echo \"`true; rm ../b/tasks/1.md`\"")
	unresolved(`echo "x;y"; rm $X`)
	unresolved(`echo 'a' && rm "x;y" $X`)
	// The hard-block still binds the lead through all of it.
	for _, cmd := range []string{`rm "a;b" ../b/secrets/k`, `rm ../b/secrets/k 2>&1`, `rm ../b/secrets/k # c`} {
		if got := decideAt("Bash", cmd, "lead", pwd); got.Allow || !strings.Contains(got.Reason, "HARD-BLOCK") {
			t.Errorf("lead Bash %q = %+v, want HARD-BLOCK", cmd, got)
		}
	}
}

// $!, $? and $# can be forced to a value by whoever writes the command, so a name spelled
// with one is an exact-name bypass if it is judged as a digit: `rm .env$!` is `rm .env`.
// Neither the lead's hard-block nor a non-owner's refusal may be dodged that way. The
// policy owns exact names, so a digit placeholder (`.env0`) would match nothing.
func TestDecide_BashForceableSpecialsCannotDodgeAnExactNameMatch(t *testing.T) {
	const pwd = testHome + "/Projects/b"
	pol := &Policy{
		HardBlock: []string{"Projects/b/.env", "Projects/b/k.pem"},
		Paths:     []PathOwner{{Glob: "Projects/b/tasks/157.md", Owner: "tasks-agent"}},
	}
	decide := func(cmd, caller string) Decision {
		return Decide(Input{Tool: "Bash", Command: cmd, Caller: caller, Home: testHome, Pwd: pwd, Policy: pol})
	}
	// Controls: the plain names FIRE, or the rows below prove nothing.
	for _, cmd := range []string{"rm .env", "rm k.pem"} {
		if got := decide(cmd, "lead"); got.Allow || !strings.Contains(got.Reason, "HARD-BLOCK") {
			t.Fatalf("control: lead %q = %+v, want HARD-BLOCK", cmd, got)
		}
	}
	if got := decide("rm tasks/157.md", "other"); got.Allow || !strings.Contains(got.Reason, "owned by 'tasks-agent'") {
		t.Fatalf("control: other rm tasks/157.md = %+v, want the ownership DENY", got)
	}
	// $$ cannot be forced, so it is still judged as a digit: the name it makes is not the exact one.
	if got := decide("rm .env$$", "lead"); !got.Allow {
		t.Errorf("lead rm .env$$ = %+v, want ALLOW (a PID suffix is not the exact name)", got)
	}
	for _, sp := range []string{"$!", "$?", "$#"} {
		for _, cmd := range []string{"rm .env" + sp, "rm k.pem" + sp, `rm ".env` + sp + `"`, "rm tasks/157.md" + sp, "rm tasks/15" + sp + ".md"} {
			for _, caller := range []string{"lead", "other"} {
				if got := decide(cmd, caller); got.Allow || got.Code != 2 || !strings.Contains(got.Reason, "cannot resolve") {
					t.Errorf("%s %q = %+v, want DENY (cannot resolve)", caller, cmd, got)
				}
			}
		}
	}
}
