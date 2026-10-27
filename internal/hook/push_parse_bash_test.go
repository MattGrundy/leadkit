package hook

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The parser's one safety property is: if a real shell would run a push, the parser does
// not answer "no push", and does not answer "these pushes" when the shell ran more. A
// hole in a push parser breaks exactly that, and a hand-written table cannot show it,
// because the table is written by the person who also wrote the tokeniser. These tests ask
// the shell.
// They run commands under real bash with a stub `git` that records its arguments, and fail
// when bash ran a push the parser did not account for. Refusing (pushUnsure) a command bash
// did not push is always safe and is not checked.
//
// Every command is checked TWICE where it can be. The composite is what the hooks run: the
// tokeniser plus the independent cross-check. The tokeniser alone is what the cross-check
// would otherwise hide: with the check on, a tokeniser that loses a push still ends in a
// refusal, so a regression in it would pass. The tokeniser-alone run skips only commands
// that are known to need the check (lostPushes, costOfInvariant, and the generators'
// "needs the check" fragments), so a tokeniser fail-open that is not on that list fails.
//
// SAFETY, BY CONSTRUCTION. Nothing here can run a real program, whatever a command says:
//   - bash runs with -r (restricted): a command name containing `/` is refused, so
//     `/usr/bin/git` and `/bin/rm` cannot execute; `command -p` and `exec` are refused, so
//     the default PATH is unreachable; PATH cannot be reassigned; `>` redirects are refused,
//     so nothing writes a file. `cd` is refused, so a command is always run in the temp dir.
//   - PATH is the stub directory alone, so a bare command name resolves to the stub `git` or
//     to a shell builtin, and to nothing else.
//   - GIT_DIR names a path that does not exist and HOME is a temp dir, so even a real git
//     reached some other way has no repository to act on.
//
// The cost is fidelity: a command that needs `cd`, a redirect or an external tool before
// its push does not push here, and is not checked. The tests that matter use builtins.

// stubRecordEnd and stubArgSep frame what the stub git logs, one record per invocation and
// one field per argument, so that an argument containing a blank stays one argument.
const (
	stubRecordEnd = "\x1e"
	stubArgSep    = "\x1f"
)

// stubVerb is the subcommand real git would run for args: the first word that is not a
// global option or the value of one. A real push spelled `-C "a b"` or `--namespace x` must
// be seen as a push, or the harness reports clean on exactly the holes that exist.
func stubVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" ||
			a == "--super-prefix" || a == "--config-env" || a == "--attr-source":
			i++ // the option's value is the next word
		case strings.HasPrefix(a, "-"):
			// a flag, or `--option=value`: no separate value
		default:
			return a
		}
	}
	return ""
}

func TestStubVerb_FollowsGitsGlobalOptionGrammar(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"push", "origin"}, "push"},
		{[]string{"-C", "a b", "push"}, "push"},
		{[]string{"-c", "k=v", "-C", "/r", "push"}, "push"},
		{[]string{"--namespace", "x", "push"}, "push"},
		{[]string{"--git-dir", "/x", "push"}, "push"},
		{[]string{"--git-dir=/x", "--no-pager", "push"}, "push"},
		{[]string{"-C", "push"}, ""},
		{[]string{"stash", "push"}, "stash"},
		{[]string{"-C", "/r", "log", "--grep", "push"}, "log"},
		{nil, ""},
	} {
		if got := stubVerb(c.args); got != c.want {
			t.Errorf("stubVerb(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

// bashRunner runs scripts under real bash with a recording stub in place of git. It skips
// the test where there is no bash to ask.
type bashRunner struct {
	t    *testing.T
	bash string
	dir  string
	path string
	runs int
	// composite and tokeniser count the commands bash pushed that each mode was asked about.
	composite, tokeniser int
}

func newBashRunner(t *testing.T) *bashRunner {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("no bash to ask")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n{ for a in \"$@\"; do printf '%s\\037' \"$a\"; done; printf '\\036'; } >> \"$STUB_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return &bashRunner{t: t, bash: bash, dir: dir, path: bin}
}

// pushCount runs script and returns how many times the stub git was invoked with the push
// verb.
//
// A command can leave a background job (`true & git push`) running after bash exits. Each
// run therefore logs to a file of its own, so a straggler from one run cannot be counted in
// the next, and the script ends with `wait` so that, when the script ran to its end, the
// jobs have finished before the log is read.
func (b *bashRunner) pushCount(script string) int {
	b.t.Helper()
	b.runs++
	log := filepath.Join(b.dir, fmt.Sprintf("stub-%d.log", b.runs))
	file := filepath.Join(b.dir, "s.sh")
	if err := os.WriteFile(file, []byte(script+"\nwait\n"), 0o644); err != nil {
		b.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bash, "-r", file)
	cmd.Dir = b.dir
	cmd.Env = []string{
		"PATH=" + b.path,
		"HOME=" + b.dir,
		"STUB_LOG=" + log,
		"GIT_DIR=" + filepath.Join(b.dir, "no-such-git-dir"),
	}
	_ = cmd.Run() // a syntax error or a failing command is fine: only the stub's log matters
	data, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	n := 0
	for _, rec := range strings.Split(string(data), stubRecordEnd) {
		if rec == "" {
			continue
		}
		if stubVerb(strings.Split(strings.TrimSuffix(rec, stubArgSep), stubArgSep)) == "push" {
			n++
		}
	}
	return n
}

// The harness must see a push however it is spelled, and must be safe whatever it is given.
func TestBashRunner_SeesRealPushesAndRunsNothingElse(t *testing.T) {
	b := newBashRunner(t)
	for _, c := range []struct {
		script string
		want   int
	}{
		{"git push origin x", 1},
		{"git push\ngit push", 2},
		{`git -C "/tmp/a b" push origin x`, 1},
		{"git --namespace x push", 1},
		{"git stash push", 0},
		{"git status", 0},
		{"echo git push", 0},
		// Nothing outside the stub runs: each of these would reach a real git or a real
		// program if the guarantees in the file comment did not hold.
		{"/usr/bin/git push", 0},
		{"command -p git push", 0},
		{"exec git push", 0},
		{"cd / && git push", 0},
	} {
		if got := b.pushCount(c.script); got != c.want {
			t.Errorf("%q: stub saw %d pushes, want %d", c.script, got, c.want)
		}
	}
}

// loops are commands whose one `git push` runs more than once; a found-count comparison
// would be wrong for them. A loop keyword counts only in command position (the start of
// the text or after a separator) and only with a `push` after it, so the word `for` in
// quoted prose does not disable the comparison. (A loop that ends before the push still
// exempts it: the comparison is a text test, not a parse.)
var loops = regexp.MustCompile(`(?s)(?:(?:^|[;&|({\n])\s*(?:for|while|until|select|function|xargs|seq)\b|\w\(\)\s*\{).*push`)

// judge says what is wrong, if anything, with a parse of a command in which bash ran
// bashPushes pushes. Refusing is never wrong. Finding fewer pushes than bash ran is, unless
// a loop explains the difference.
func judge(p pushParse, cmd string, bashPushes int) string {
	switch {
	case p.State == pushNone:
		return fmt.Sprintf("bash pushed %d time(s) but the parser said no push", bashPushes)
	case p.State == pushFound && bashPushes > len(p.Invocations) && !loops.MatchString(cmd):
		return fmt.Sprintf("bash pushed %d times but the parser found %d, so the rest were never judged", bashPushes, len(p.Invocations))
	}
	return ""
}

func TestJudge(t *testing.T) {
	found := func(n int) pushParse { return pushParse{State: pushFound, Invocations: make([]pushInvocation, n)} }
	for _, c := range []struct {
		name string
		p    pushParse
		cmd  string
		n    int
		bad  bool
	}{
		{"no push found where bash pushed", pushParse{State: pushNone}, "git push", 1, true},
		{"fewer found than bash ran", found(1), "git push\ngit push", 2, true},
		{"as many found as bash ran", found(2), "git push\ngit push", 2, false},
		{"more found than bash ran (a branch not taken)", found(2), "false && git push; git push", 1, false},
		{"refusing is never wrong", pushParse{State: pushUnsure, Why: "x"}, "git push\ngit push", 2, false},
		{"a loop runs one push twice", found(1), "for i in 1 2; do git push; done", 2, false},
		{"a loop after a separator", found(1), "true; while true; do git push; done", 2, false},
		{"a function runs one push twice", found(1), "f() { git push; }; f; f", 2, false},
		{"xargs runs one push twice", found(1), "echo a b | xargs -n1 git push", 2, false},
		{"the word for in prose is no loop", found(1), "echo 'a message for you'; git push\ngit push", 2, true},
		{"the word function in prose is no loop", found(1), "git commit -m 'function'; git push\ngit push", 2, true},
	} {
		if got := judge(c.p, c.cmd, c.n) != ""; got != c.bad {
			t.Errorf("%s: judge flagged = %v, want %v", c.name, got, c.bad)
		}
	}
}

// verdict fails t when the parse of cmd is wrong given that bash ran bashPushes pushes.
// crossCheck selects the composite (the hooks' behaviour) or the tokeniser alone.
func verdict(t *testing.T, label, cmd string, bashPushes int, crossCheck bool) {
	t.Helper()
	mode := "tokeniser alone"
	if crossCheck {
		mode = "composite"
	}
	if problem := judge(parsePush(cmd, crossCheck), cmd, bashPushes); problem != "" {
		t.Errorf("%s [%s]: %s:\n%q", label, mode, problem, cmd)
	}
}

// checkAgainstBash runs cmd under bash and judges the parser against it. needsCheck marks a
// command known to be beyond the tokeniser alone; only the composite is asked about it.
func (b *bashRunner) checkAgainstBash(t *testing.T, label, cmd string, needsCheck bool) {
	t.Helper()
	n := b.pushCount(cmd)
	if n == 0 {
		return
	}
	b.composite++
	verdict(t, label, cmd, n, true)
	if !needsCheck {
		b.tokeniser++
		verdict(t, label, cmd, n, false)
	}
}

// requireBothModesRan fails when the tokeniser-alone run was skipped or starved: a mode
// that quietly checks nothing reads as a pass, and the composite alone cannot see a
// regressed tokeniser.
func (b *bashRunner) requireBothModesRan(t *testing.T, minTokeniser int) {
	t.Helper()
	t.Logf("bash pushed in %d commands; %d were also checked against the tokeniser alone", b.composite, b.tokeniser)
	if b.tokeniser < minTokeniser {
		t.Errorf("only %d commands were checked against the tokeniser alone, want at least %d", b.tokeniser, minTokeniser)
	}
}

// runnable reports whether a corpus command is finite under the stub. (That it is SAFE does
// not depend on this: see the file comment.)
func runnable(c parseCase) bool {
	for _, bad := range []string{"while", "until", "for ((", "sleep", "read "} {
		if strings.Contains(c.cmd, bad) {
			return false
		}
	}
	return true
}

func TestParsePushCommands_NeverSaysNoPushWhereBashPushes_Corpus(t *testing.T) {
	b := newBashRunner(t)
	for _, c := range corpusCases() {
		if !runnable(c) {
			continue
		}
		b.checkAgainstBash(t, c.name, substituteBranch(c.cmd, "feat/x"), isInvariantCase(c.name))
	}
	b.requireBothModesRan(t, 40)
}

// The count rule, in the bash harness: where the tokeniser finds FEWER pushes than bash
// ran, and at least one, only the rule can refuse. A case with none found is refused by the
// no-push rule anyway, so without these the rule could be switched off and every other test
// here would still pass.
func TestParsePushCommands_CountRule_RefusesATokeniserThatFindsFewerThanBashRan(t *testing.T) {
	b := newBashRunner(t)
	for _, cmd := range []string{
		"git push origin BR ; cat <(true)# ; git push origin BR",
		"git push origin BR\ncat <(true)# ; git -c a=\"b c\" push origin BR",
		"cat <(true)# ; git push\ngit push",
	} {
		ran := b.pushCount(substituteBranch(cmd, "feat/x"))
		alone := parsePush(cmd, false)
		if alone.State != pushFound || len(alone.Invocations) >= ran {
			t.Errorf("not a case of the tokeniser finding fewer than bash ran (bash %d, tokeniser %v with %d):\n%q",
				ran, alone.State, len(alone.Invocations), cmd)
			continue
		}
		if got := parsePushCommands(cmd); got.State != pushUnsure {
			t.Errorf("the composite answered %v where the tokeniser found %d of the %d pushes bash ran:\n%q",
				got.State, len(alone.Invocations), ran, cmd)
		}
	}
}

// verifiedBashMajor is the bash major version every lostPushes line was checked against.
const verifiedBashMajor = "3"

// Every lostPushes line is a repro, so bash must actually push it: otherwise the line proves
// nothing. On the bash the lines were verified against, one that does not push FAILS. On
// another bash an edge may be read differently, so a few lines may be skipped there, but
// most of the list must still push, or the test is not testing.
func TestParsePushCommands_LostPushes_AreRealPushes(t *testing.T) {
	b := newBashRunner(t)
	major, _ := exec.Command(b.bash, "-c", "echo ${BASH_VERSINFO[0]}").Output()
	verified := strings.TrimSpace(string(major)) == verifiedBashMajor
	confirmed := 0
	for _, c := range lostPushes {
		cmd := substituteBranch(c.cmd, "feat/x")
		n := b.pushCount(cmd)
		if n == 0 {
			if verified {
				t.Errorf("%s: bash does not run this push, so the line proves nothing:\n%q", c.name, cmd)
			} else {
				t.Logf("%s: this bash does not run the push", c.name)
			}
			continue
		}
		confirmed++
		verdict(t, c.name, cmd, n, true)
	}
	if confirmed*5 < len(lostPushes)*4 {
		t.Errorf("bash ran the push for only %d of %d lostPushes lines: the list is not being tested", confirmed, len(lostPushes))
	}
}

// preludes are lines that can confuse a tokeniser (comments with quotes, heredocs with odd
// delimiters, substitutions followed by #), safe to run and finite. The tokeniser alone must
// survive every one of these.
var tokenisedPreludes = []string{
	"", "# don't forget", "echo a # it's", "echo a\t# it's", "true;# it's", "true &# it's",
	"# a \"dq", "# a `tick", "echo a |# it's\ncat", "echo a \\\\\n# it's", "echo a#b",
	"echo 'a # b'", "echo \"it's\"", "echo $(true) # it's", "x=$(echo a # it's\ntrue)",
	"cat <<EOF\nbody\nEOF", "cat <<'EOF'\ndon't\nEOF", "cat <<-EOF\n\tx\n\tEOF",
	"cat <<EO\"F\"\nEOF", "cat <<'EO'F\nEOF", "cat <<EOF:\nEOF:", "cat <<<EOF\nEOF",
	"echo hi # <<EOF", "echo $((1<<2))", "cat <<EOF\n$(true)\nEOF", "cat <<\\EOF\nEOF",
	"echo \"a\n# not a comment\"", "echo 'a\n# not a comment'", "(\n# it's\ntrue)",
}

// checkPreludes are the preludes the tokeniser alone is known to lose a push after. Only the
// composite is asked about them. One that ends in a blank is a lead-in on the push's own
// line: the push follows it directly, not on a new line.
var checkPreludes = []string{
	"echo $(true)#\"\ncat <<EOF \"", "echo $((1))#\"\ncat <<EOF \"", "cat <(true)#\"\ncat <<EOF \"",
	"cat <(true)# ; ", "cat <(true)# ; true; ",
	"cat <<EOF\\\nEOF\\", "cat <<\"EOF\\\\\"\nEOF\\", "cat <<-\"EOF\\\\\"\nEOF\\",
}

// tokenisedPushes are pushes the tokeniser alone reads in any position.
var tokenisedPushes = []string{
	"git push origin BR", "(git push origin BR)", "if true; then git push origin BR; fi",
	"git push", `git -C "/tmp/a b" push origin BR`, "git --namespace x push origin BR",
	"git pu\\\nsh origin BR", "git pu\\sh origin BR", `g'i't pu"s"h origin BR`, "g=git; $g push origin BR",
	// An option value attached to its option, or made of several quoted strings, with blanks
	// in it: the shape has to read a quote pair that follows `=` or other attached text.
	`git -c http.extraheader="Authorization: x" push origin BR`, `git -c user.name='A B' push origin BR`,
	`git --git-dir="/a b" push origin BR`, `git --work-tree='/a b' push origin BR`,
	`git --namespace="a b" push origin BR`, `git --config-env="a b" push origin BR`,
	`git -C 'a b'"c d" push origin BR`, `git -C a"b c" push origin BR`, `git -C $'a b' push origin BR`,
	`git -c a="b\" c" push origin BR`, "git -C \"a\x02 b\" push origin BR", "git -C 'a\x03 b' push origin BR", "git -c a=\"b\x02 c\" push origin BR", "git -c a='b\x03 c' push origin BR",
	// A quoted value that spans lines, and an odd quote left on a later line by a fragment
	// that follows: the shape pairs a quote with the next one, so a value with a newline and a
	// stray quote after it is the form that mispairs.
	"git -c 'a\nb' push origin BR", "git -C \"a\nb\" push origin BR", "git -c a='b\nc' push origin BR",
	// Quoted values with NO blank in them, beside ones with a blank. The vocabulary has to
	// express the mix because the mix is the shape that went unseen: a count that pairs a
	// closing quote with the next opening one is only wrong where several pushes with quoted
	// values sit close together (see mixedValuePushes, which crosses them in threes).
	`git -C "/a" push origin BR`, `git -C '/b' push origin BR`, `git -C "c d" push origin BR`, `git -C 'c d' push origin BR`,
	`git -c a="b\"c" push origin BR`, "git -c \"`echo y`\" push origin BR",
	// A substitution as the value of an option, which holds blanks and operators the shape
	// would otherwise take for the end of the value.
	"git -C $(echo /x) push origin BR", "git -C $(true && echo /x) push origin BR",
	"git -C `true && echo /x` push origin BR", "git -C $((1 + 2)) push origin BR",
	"git -C $(echo $(echo /x)) push origin BR", "git -C $(echo $(echo $(echo /x))) push origin BR", "git -C $(echo \"  /x\") push origin BR",
	"git -c a=$(true && echo b) push origin BR", "git --git-dir=$(true && echo b) push origin BR",
	"git -c a=$(git rev-parse --show-toplevel) push origin BR",
	// Quotes inside a quoted substitution, and ANSI-C strings. The spellings of these that
	// the shape does not read (a quote nested inside a quoted $( ) that holds a blank, an
	// escaped quote in $'...') cannot be fragments, because a fragment that is not read fails
	// the harness; they are pinned as documented limits instead.
	"git -C \"$(echo \"$(echo /x)\" && echo y)\" push origin BR", "git -c a=\"b $(echo c)\" push origin BR",
	"git -c a=\"b $(echo \"c\")\" push origin BR", "git -c a=$'b c' push origin BR", "git -C $'a\\tb c' push origin BR",
}

// checkPushes are pushes spelled so that no tokeniser rule is expected to read them.
var checkPushes = []string{
	"git push$(true) origin BR", "git pu\\\nsh$(true) origin BR", "git pu\\\nsh`true` origin BR",
	"git pu\\\nsh${IFS}origin BR", "git pu\\\nsh$x origin BR", "${g:-git} push origin BR",
	"git -C $(echo /x) push$(true) origin BR", "git -c 'a\nb' pu\\\nsh`true` origin BR",
	"git -c a=$(true && echo b) pu\\\nsh${IFS}origin BR",
	// An odd quote left after a multi-line quoted value: it leaves the rest of the sweep inside
	// a quote, which only the composite is asked about.
	"git -c 'a\nb' push origin BR\necho '", "git -C \"a\nb\" push origin BR\necho \"",
}

// join puts a prelude before a command: on its own line, or, when the prelude ends in a
// blank, directly before it on the same line.
func join(pre, cmd string) string {
	switch {
	case strings.HasSuffix(pre, " "):
		return pre + cmd
	case pre != "":
		return pre + "\n" + cmd
	}
	return cmd
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A generated differential, in two fixed cross-products. First every prelude before a few
// pushes, each with a few tails: the preludes are what confuse a tokeniser. Then every push
// spelling after a few lead-ins: the spellings are what a shape must recognise.
func TestParsePushCommands_NeverSaysNoPushWhereBashPushes_Generated(t *testing.T) {
	b := newBashRunner(t)
	tails := []string{"", "\necho 'x'"}
	pushes := tokenisedPushes[:3]
	for _, pre := range append(append([]string{}, tokenisedPreludes...), checkPreludes...) {
		for _, push := range pushes {
			for _, tail := range tails {
				b.checkAgainstBash(t, "prelude", substituteBranch(join(pre, push+tail), "feat/x"), contains(checkPreludes, pre))
			}
		}
	}
	leads := []string{"", "# it's\n", "cat <(true)# ; ", "cat <<\"EOF\\\\\"\nEOF\\\n"}
	for _, lead := range leads {
		for _, push := range append(append([]string{}, tokenisedPushes...), checkPushes...) {
			needs := contains(checkPushes, push) || contains(checkPreludes, lead) || lead == "cat <(true)# ; " || strings.HasPrefix(lead, "cat <<\"EOF")
			b.checkAgainstBash(t, "spelling", substituteBranch(lead+push, "feat/x"), needs)
		}
	}
	b.requireBothModesRan(t, 120)
}

// mixedValuePushes are the quoted values that matter in threes: with and without a blank, in
// both quotes. Three pushes of these, the first behind a lead-in the tokeniser cannot read, is
// the command whose count went wrong when a quote span could pair a closing quote with the next
// value's opening one, and with a value that spans lines it is the lazy gap that keeps it right.
// A sweep reaches such a triple by chance; this crosses all of them.
var mixedValuePushes = []string{
	`git -C "/a" push origin BR`, `git -C '/a' push origin BR`, `git -C "a b" push origin BR`, `git -C 'a b' push origin BR`,
	"git -C 'a\nb' push origin BR",
}

// Every ordered triple of mixedValuePushes, on three lines, with and without a lead-in the
// tokeniser loses a push behind. With the lead-in only the count can refuse it.
func TestParsePushCommands_NeverSaysNoPushWhereBashPushes_MixedValueTriples(t *testing.T) {
	b := newBashRunner(t)
	for _, lead := range []string{"", "cat <(true)# ; "} {
		for _, x := range mixedValuePushes {
			for _, y := range mixedValuePushes {
				for _, z := range mixedValuePushes {
					b.checkAgainstBash(t, "mixed triple", substituteBranch(lead+x+"\n"+y+"\n"+z, "feat/x"), lead != "")
				}
			}
		}
	}
	b.requireBothModesRan(t, 100)
}

// sweepSeed fixes the random sweep: the same commands on every run, so a failure is a
// failure everywhere. LEADKIT_PUSH_SWEEP_SEED looks at a different slice of the space.
const sweepSeed = 165

// sweepParams returns the sweep's seed and size. The default size is small enough for
// `make check`; a deeper sweep is
//
//	LEADKIT_PUSH_SWEEP=20000 go test ./internal/hook -run RandomSweep -count=1
func sweepParams() (seed int64, size int) {
	seed, size = sweepSeed, 150
	if v, err := strconv.ParseInt(os.Getenv("LEADKIT_PUSH_SWEEP_SEED"), 10, 64); err == nil {
		seed = v
	}
	if v, err := strconv.Atoi(os.Getenv("LEADKIT_PUSH_SWEEP")); err == nil && v > 0 {
		size = v
	}
	return seed, size
}

// A seeded random sweep over the same fragments: several statements, drawn at random from
// preludes and pushes and joined by random separators. The fixed cross-product above covers
// each fragment beside each other; this covers them beside each other in combination, which
// is where a tokeniser fails.
func TestParsePushCommands_NeverSaysNoPushWhereBashPushes_RandomSweep(t *testing.T) {
	b := newBashRunner(t)
	seed, size := sweepParams()
	rng := rand.New(rand.NewSource(seed))
	joiners := []string{"\n", " ; ", " && ", " || ", " & ", " | cat ; ", "\n\n", ";\n"}
	type frag struct {
		text       string
		needsCheck bool
	}
	var frags []frag
	for _, s := range tokenisedPreludes {
		frags = append(frags, frag{s, false})
	}
	for _, s := range tokenisedPushes {
		// Weighted: pushes are the point.
		frags = append(frags, frag{s, false}, frag{s, false})
	}
	for _, s := range checkPreludes {
		frags = append(frags, frag{s, true})
	}
	for _, s := range checkPushes {
		frags = append(frags, frag{s, true})
	}
	for i := 0; i < size; i++ {
		var sb strings.Builder
		needs := false
		for k, n := 0, 2+rng.Intn(3); k < n; k++ {
			f := frags[rng.Intn(len(frags))]
			if k > 0 && !strings.HasSuffix(sb.String(), " ") {
				sb.WriteString(joiners[rng.Intn(len(joiners))])
			}
			sb.WriteString(f.text)
			needs = needs || f.needsCheck
		}
		b.checkAgainstBash(t, fmt.Sprintf("sweep #%d (seed %d)", i, seed), substituteBranch(sb.String(), "feat/x"), needs)
	}
	b.requireBothModesRan(t, size/10)
}

// The spellings the parser and its cross-check do not read, as documented in
// docs/PORTABLE-OPERATING-KIT.md. Each is a real push under bash and passes as "no push".
// This pins the documentation: when one starts to be read, delete it here and from the
// documented list, so that list never claims a hole that is closed or hides one that is open.
func TestParsePushCommands_DocumentedLimits_AreRealAndOpen(t *testing.T) {
	b := newBashRunner(t)
	for _, c := range []struct {
		cmd string
		// stubSeesPush is false where only a real git would push: `-c alias.p=push p`
		// names the verb `p` to the stub.
		stubSeesPush bool
	}{
		{"git $'push' origin BR", true},
		{"git pu{s,s}h origin BR", true},
		{"git ${IFS}push origin BR", true},
		{"git -c alias.p=push p origin BR", false},
		{"p=push; git $p origin BR", true},
		// The cross-check's shape (gitPushShapeLine, gitPushShapeWide) is closed one idiom at a time and each one it
		// does not read is a limit: the structural answer is a git pre-push hook (ticket 171).
		// These evade only where the tokeniser has ALSO lost the push. Any tokeniser loss does
		// it (the checkPreludes family); these carry the `cat <(true)# ; ` lead-in, which is
		// one. They need a tokeniser failure as well as the spelling.
		//
		// A quote inside a quoted $( ), in a value that has text before the substitution.
		{"cat <(true)# ; git -c a=\"b $(echo \"c d\")\" push origin BR", true},
		// ... or text after it: a blank, or a path with a blank. Alone, `"$(echo "c d")"` is read.
		{"cat <(true)# ; git -C \"$(echo \"c d\") b\" push origin BR", true},
		{"cat <(true)# ; git -C \"$(echo \"c d\")/sub dir\" push origin BR", true},
		// ... and the backtick form.
		{"cat <(true)# ; git -C \"a `echo \"c d\"`\" push origin BR", true},
		// A parenthesis inside a quote inside a substitution: the shape counts parentheses
		// without reading quotes, so the substitution seems to end early or never. The limit is
		// any imbalance of parentheses inside quotes WHEN THE QUOTE-STRIPPED READING ALSO FAILS.
		// A lone `")"` is READ, because that reading balances it; another quoted value with a
		// blank in the command breaks that reading, and then it evades (the first case below).
		{"cat <(true)# ; git -C $(echo \"(\") push origin BR", true},
		{"cat <(true)# ; git -C $(echo '(') push origin BR", true},
		{"cat <(true)# ; git -C $(echo ') ') push origin BR", true},
		{"cat <(true)# ; git -C $(echo \"a(b\") push origin BR", true},
		{"cat <(true)# ; git -C \"/a b\" -C $(echo \")\") push origin BR", true},
		// An ANSI-C string holding an escaped quote: `\'` ends the pairing the shape relies on.
		{"cat <(true)# ; git -c a=$'b\\'c d' push origin BR", true},
		// A command word with an expansion attached to it.
		{"cat <(true)# ; x=; git$x push origin BR", true},
		{"cat <(true)# ; x=; git${x} push origin BR", true},
		// A command word assembled at run time from more than one variable.
		{"cat <(true)# ; g=gi; h=t; $g$h push origin BR", true},
		// A substitution nested deeper than the shape reads (three levels of parentheses).
		{"cat <(true)# ; git -C $(echo $(echo $(echo $(echo /x)))) push origin BR", true},
	} {
		if p := parsePushCommands(c.cmd); p.State != pushNone {
			t.Errorf("%q is now read (%v): remove it from the documented limits", c.cmd, p.State)
		}
		if c.stubSeesPush && b.pushCount(c.cmd) == 0 {
			t.Errorf("%q: bash does not push it, so it is not a limit", c.cmd)
		}
	}
}
