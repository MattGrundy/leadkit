package hook

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// The shared parser has three outcomes. For every construct it used to miss,
// the push is either FOUND (and then judged by both hooks) or the command is UNSURE (and
// then refused by both hooks) — never silently absent. BR stands for the branch under test.

type parseCase struct {
	name string
	cmd  string
	// want is pushFound, pushUnsure, or pushNone (a control that must stay allowed).
	want pushState
	// args, for pushFound, is each push's args joined by spaces, in order.
	args []string
}

const (
	bad  = "BR"
	good = "feat/gated"
)

// ticketCorpus is every line of ticket 165's corpus of constructs the parser missed.
var ticketCorpus = []parseCase{
	{"subshell then push", "( git push origin BR ) && git push origin feat/gated", pushFound,
		[]string{"origin BR", "origin feat/gated"}},
	{"subshell pair, second arg intact", "( git push origin feat/gated ; git push origin BR )", pushFound,
		[]string{"origin feat/gated", "origin BR"}},
	{"group, first not lost", "{ git push origin BR ; git push origin feat/gated ; }", pushFound,
		[]string{"origin BR", "origin feat/gated"}},
	{"command substitution", "$( git push origin BR )", pushUnsure, nil},
	{"command substitution as an argument", "echo $(git push origin BR)", pushUnsure, nil},
	{"backticks", "`git push origin BR`", pushUnsure, nil},
	{"substitution inside double quotes", `echo "$(git push origin BR)"`, pushUnsure, nil},
	{"process substitution", "cat <(git push origin BR)", pushUnsure, nil},
	{"bash -c", "bash -c 'git push origin BR'", pushUnsure, nil},
	{"eval", "eval 'git push origin BR'", pushUnsure, nil},
	{"sh -c with a chain", `sh -c "true && git push origin BR"`, pushUnsure, nil},
	{"if body", "if true; then git push origin BR; fi", pushFound, []string{"origin BR"}},
	{"for body", "for b in x; do git push origin BR; done", pushFound, []string{"origin BR"}},
	{"while body", "while true; do git push origin BR; done", pushFound, []string{"origin BR"}},
	{"command git", "command git push origin BR", pushUnsure, nil},
	{"env git", "env git push origin BR", pushUnsure, nil},
	{"absolute path git", "/usr/bin/git push origin BR", pushFound, []string{"origin BR"}},
	{"git -c k=v", "git -c k=v push origin BR", pushFound, []string{"origin BR"}},
	{"xargs", "echo BR | xargs git push origin", pushUnsure, nil},
	{"quoted separators are not separators", `echo "a && b"; git push origin BR`, pushFound, []string{"origin BR"}},
	{"quotes are resolved in a push arg", `git push 'origin' "BR"`, pushFound, []string{"origin BR"}},
}

// moreUnreadable are further spellings of the same hole that the ticket's list implies.
var moreUnreadable = []parseCase{
	{"sudo bash -c", "sudo bash -c 'git push origin BR'", pushUnsure, nil},
	{"env sh -c", `env X=1 sh -c "git push origin BR"`, pushUnsure, nil},
	{"python3 -c", `python3 -c "import os; os.system('git push origin BR')"`, pushUnsure, nil},
	{"H1 double-quoted delimiter with an escaped quote (unterminated string)", "cat <<\"E\\\"F\"\nE\"F\ngit push origin BR", pushUnsure, nil},
	{"zsh -c", `zsh -c "git push origin BR"`, pushUnsure, nil},
	{"sudo", "sudo git push origin BR", pushUnsure, nil},
	{"exec", "exec git push origin BR", pushUnsure, nil},
	{"nohup", "nohup git push origin BR", pushUnsure, nil},
	{"printed text piped into a shell", `echo "git push origin BR" | sh`, pushUnsure, nil},
	{"printf piped into bash", `printf 'git push origin BR\n' | bash`, pushUnsure, nil},
	{"a wrapper the list does not name", "time -p git push origin BR", pushUnsure, nil},
	{"timeout", "timeout 5 git push origin BR", pushUnsure, nil},
	{"ssh", "ssh host 'git push origin BR'", pushUnsure, nil},
	{"find -exec", `find . -maxdepth 0 -exec git push origin BR \;`, pushUnsure, nil},
	{"here-string into a shell", `bash <<< "git push origin BR"`, pushUnsure, nil},
	{"push verb split by quotes inside a substitution", `echo $(git pu""sh origin BR)`, pushUnsure, nil},
	{"push verb split by a backslash inside a substitution", `echo $(git pu\sh origin BR)`, pushUnsure, nil},
	{"push verb split by a continuation inside a substitution", "echo $(git pu\\\nsh origin BR)", pushUnsure, nil},
	{"expansion as the command word", "$GIT push origin BR", pushUnsure, nil},
	{"expansion where a git option could be", "git ${OPT} push origin BR", pushUnsure, nil},
	{"repo path computed by a substitution", `git -C "$(git rev-parse --show-toplevel)" push origin BR`, pushUnsure, nil},
	{"carried cd computed by a substitution", `cd "$(git rev-parse --show-toplevel)" && git push origin BR`, pushUnsure, nil},
	{"carried cd to OLDPWD", "cd - && git push origin BR", pushUnsure, nil},
	{"carried cd with an option cd does not have", "cd -x /r && git push origin BR", pushUnsure, nil},
	{"carried cd with two operands", "cd /a /b && git push origin BR", pushUnsure, nil},
	{"git --git-dir", "git --git-dir=/x/.git push origin BR", pushUnsure, nil},
	{"git --work-tree", "git --work-tree /x push origin BR", pushUnsure, nil},
	{"unclosed quote over a push", `echo "oops; git push origin BR`, pushUnsure, nil},
	{"unclosed substitution over a push", `echo $(git push origin BR`, pushUnsure, nil},
	{"heredoc into a shell", "bash <<'EOF'\ngit push origin BR\nEOF", pushUnsure, nil},
	{"heredoc piped into a shell", "cat <<'EOF' | sh\ngit push origin BR\nEOF", pushUnsure, nil},
	{"heredoc into an unlisted interpreter", "deno run - <<'EOF'\nimport os; os.system('git push origin BR')\nEOF", pushUnsure, nil},
	{"unquoted heredoc whose body substitutes", "cat <<EOF\n$(git push origin BR)\nEOF", pushUnsure, nil},
	{"substitution in a nested group", "( echo ok ; echo $(git push origin BR) )", pushUnsure, nil},
}

// moreReadable are spellings that are read, not refused.
var moreReadable = []parseCase{
	{"arithmetic shift is not a heredoc", "echo $((1<<EOF)); git push origin BR\nEOF", pushFound, []string{"origin BR"}},
	// Each heredoc family below carries a REAL terminator line. Without one a wrong reading
	// of the operator leaves the text in place anyway and the case proves nothing.
	{"a here-string is not a heredoc", "cat <<<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"a here-string into a shell is not a heredoc", "bash <<<x\ngit push origin BR\nx", pushFound, []string{"origin BR"}},
	{"a partly quoted delimiter", "cat <<EO\"F\"\nEOF\ngit push origin BR\nEO", pushFound, []string{"origin BR"}},
	{"a partly quoted delimiter, single quotes", "cat <<'EO'F\nEOF\ngit push origin BR\nEO", pushFound, []string{"origin BR"}},
	{"an escaped delimiter tail", "cat <<EO\\F\nEOF\ngit push origin BR\nEO", pushFound, []string{"origin BR"}},
	{"a delimiter with a trailing colon", "cat <<EOF:\nEOF:\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"<< inside a comment", "echo hi # <<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"<< in a comment after a separator", "true; # <<'EOF'\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	// A quote inside a comment opens nothing: each push line below sits after one, and real
	// bash runs it.
	{"apostrophe in a comment line", "# don't forget\ngit push origin BR && echo 'done'", pushFound, []string{"origin BR"}},
	{"apostrophe in a trailing comment", "git status # it's clean\ngit push origin BR\ngit log --format='%h'", pushFound, []string{"origin BR"}},
	{"apostrophe in a comment after a tab", "echo a\t# it's\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"apostrophe in a comment after a semicolon", "true;# it's\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"apostrophe in a comment after an ampersand", "true &# it's\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"double quote in a comment", "# a \"quoted\nword\ngit push origin BR\necho \"x\"", pushFound, []string{"origin BR"}},
	{"apostrophe in a comment inside a substitution", "x=$(echo a # it's\ntrue); git push origin BR\necho 'q'", pushFound, []string{"origin BR"}},
	{"backtick in a comment", "# a `tick\ngit push origin BR\necho `date`", pushFound, []string{"origin BR"}},
	{"comment after a continuation", "echo a \\\n# it's\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"# after a tab hides a << (tab, not space)", "cat f\t# <<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"# after a semicolon hides a <<", "true;# <<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"# after ( hides a <<", "(# <<EOF\ngit push origin BR\nEOF\n)", pushFound, []string{"origin BR"}},
	{"# after ) hides a <<", "(true)# <<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	{"# after a newline hides a <<", "true\n# <<EOF\ngit push origin BR\nEOF", pushFound, []string{"origin BR"}},
	// One backslash in a double-quoted delimiter is an unterminated string in bash; the lexer's
	// own rule refuses it. (The real two-backslash form is H1, in lostPushes.)
	{"quoted delimiter, one backslash, unterminated", "cat <<\"EOF\\\"\nEOF\\\ngit push origin BR", pushUnsure, nil},
	// A # that is not a comment must not hide the push after it.
	{"a # inside a word is not a comment", "echo a#b; git push origin BR", pushFound, []string{"origin BR"}},
	{"a # after a carriage return is not a comment", "echo a\r#b; git push origin BR", pushFound, []string{"origin BR"}},
	{"an escaped space does not make a comment", "echo \\ #b; git push origin BR", pushFound, []string{"origin BR"}},
	{"a # in quotes is not a comment", "echo 'a # b'; git push origin BR", pushFound, []string{"origin BR"}},
	{"a # in a redirect target is not a comment", "cat >f#1; git push origin BR", pushFound, []string{"origin BR"}},
	{"a comment after a pipe", "echo a |# it's\ncat\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"an escaped backslash before a newline is not a continuation", "echo a \\\\\n# it's\ngit push origin BR\necho 'x'", pushFound, []string{"origin BR"}},
	{"push verb split by quotes", `git pu""sh origin BR`, pushFound, []string{"origin BR"}},
	{"leading backslash", `\git push origin BR`, pushFound, []string{"origin BR"}},
	{"backslash-newline continuation", "git push \\\n  origin BR", pushFound, []string{"origin BR"}},
	{"backslash-newline before the verb", "git \\\n push origin BR", pushFound, []string{"origin BR"}},
	{"subshell then a second subshell", "( git push origin BR ) ; ( git push origin feat/gated )", pushFound,
		[]string{"origin BR", "origin feat/gated"}},
	{"nested groups", "( { git push origin BR ; } )", pushFound, []string{"origin BR"}},
	{"then on its own line", "if true\nthen\n  git push origin BR\nfi", pushFound, []string{"origin BR"}},
	{"substitution that hides no push, then a push", "x=$(git rev-parse HEAD) && git push origin BR", pushFound, []string{"origin BR"}},
	{"push inside a heredoc-bearing command line", "cat <<'EOF'\nnotes\nEOF\ngit push origin BR", pushFound, []string{"origin BR"}},
	{"heredoc with no terminator is left in place", "cat <<EOF\ngit push origin BR", pushFound, []string{"origin BR"}},
}

// controls must stay silently allowed: refusing them would make the hooks unusable.
var controls = []parseCase{
	{"plain command", "ls -la", pushNone, nil},
	{"substitution without a push", "echo $(date) && echo `hostname`", pushNone, nil},
	{"bash -c without a push", "bash -c 'make test'", pushNone, nil},
	{"eval without a push", `eval "$(ssh-agent -s)"`, pushNone, nil},
	{"subshell without a push", "( cd /x && make )", pushNone, nil},
	{"git subcommands that are not push", "git stash push -m wip && git commit -m 'fix the push gate'", pushNone, nil},
	{"a script whose name says push", "bash scripts/push-docs.sh", pushNone, nil},
	{"a commit message that mentions push, with a substitution elsewhere", `git commit -m "fix push gate $(date)"`, pushNone, nil},
	{"the commit idiom with prose about pushing", "git commit -m \"$(cat <<'EOF'\nfix: the push parser, don't break\n\npushing is gated\nEOF\n)\"", pushNone, nil},
	{"a heredoc body about the push guard is data", "cat > notes.md <<'EOF'\nthe push guard refuses wrappers\nEOF", pushNone, nil},
	{"leadkit push is not git push", "leadkit push --repo /r --branch BR", pushNone, nil},
	{"echo of the word push", `echo "the push gate"`, pushNone, nil},
	{"a comment-free echo of a pipe", "echo a | grep push", pushNone, nil},
	// Near misses of the shape: each is one character from matching, and each is an everyday
	// command. They pin both boundaries of the shape and its refusal to read past a subcommand.
	{"push as a prefix of a longer word", "git pushed", pushNone, nil},
	{"push as a prefix of a hyphenated word", "git push-all", pushNone, nil},
	{"push as the stem of a file name", "git push.txt", pushNone, nil},
	{"git as the tail of a longer word", "legit push", pushNone, nil},
	{"git as the tail of a hyphenated word", "my-git push", pushNone, nil},
	{"git subtree push", "git subtree push --prefix x origin main", pushNone, nil},
	{"git lfs push", "git lfs push origin main", pushNone, nil},
	{"git help push", "git help push", pushNone, nil},
	{"a subcommand after a value option", `git -C "/tmp/a b" stash push -m wip`, pushNone, nil},
	{"a subcommand after a long value option", "git --namespace x stash push", pushNone, nil},
	{"a commit message after global options", `git -C /r -c k=v commit -m "fix push"`, pushNone, nil},
	{"a log search for push", "git --no-pager log --grep push", pushNone, nil},
}

// costOfInvariant are commands that name `git push` only as DATA, and are refused anyway:
// the text holds a push the structured reading did not find, and the check that backs the
// tokeniser cannot tell data from a push the tokeniser lost. Keeping the prose in a file
// and passing the file is the way out, if the file is written by a file-writing tool: a
// shell heredoc that creates it carries the same words and is refused the same way, and the
// deny says so.
var costOfInvariant = []parseCase{
	{"a quoted push is data", `echo "x; git push origin BR"`, pushUnsure, nil},
	{"the commit idiom with prose that names git push", "git commit -m \"$(cat <<'EOF'\nfix: the git push parser\n\ngit push origin BR was ungated\nEOF\n)\"", pushUnsure, nil},
	{"a heredoc body that names git push", "cat > notes.md <<'EOF'\ngit push origin BR\nEOF", pushUnsure, nil},
	{"an unquoted heredoc body that names git push", "cat <<EOF\ngit push origin BR\nEOF", pushUnsure, nil},
	{"a heredoc body feeding gh", "gh pr create --body-file - <<'EOF'\nrun git push origin BR\nEOF", pushUnsure, nil},
	{"echo of a push", "echo git push origin BR | cat", pushUnsure, nil},
	// The stripper must still read a heredoc after an escaped blank and a #: a rule that took
	// that # for a comment would parse the body as commands and call the data a push.
	{"a heredoc after an escaped blank and a #", "cat \\ #b <<EOF\ngit push origin BR\nEOF", pushUnsure, nil},
	{"a comment that names git push", "ls # git push origin BR", pushUnsure, nil},
}

// lostPushes are pushes the tokeniser loses and real bash runs. Each REQUIRES the
// independent check, so the tests pin that it, and not a tokeniser rule, is what refuses
// them. If a tokeniser fix finds one, move the line to moreReadable. Every line is run
// under real bash by TestParsePushCommands_LostPushes_AreRealPushes, so a spelling bash
// does not push cannot sit here as a vacuous pin.
//
// `cat <(true)# ; ` is the lead-in that makes the tokeniser read the rest of the line as a
// comment, which real bash does not (the `#` continues the process-substitution word).
var lostPushes = []parseCase{
	{"H1 double-quoted delimiter of two backslashes", "cat <<\"EOF\\\\\"\nEOF\\\ngit push origin BR", pushUnsure, nil},
	{"H1 double-quoted delimiter of two backslashes, <<-", "cat <<-\"EOF\\\\\"\nEOF\\\ngit push origin BR", pushUnsure, nil},
	{"H1 with two bare pushes", "cat <<\"EOF\\\\\"\nEOF\\\ngit push\ngit push", pushUnsure, nil},
	{"a lost push beside a found one", "git push origin BR ; cat <(true)# ; git push origin BR", pushUnsure, nil},
	{"two adjacent bare pushes, the first lost", "cat <(true)# ; git push\ngit push", pushUnsure, nil},
	{"two bare pushes on one line, the first lost", "cat <(true)# ; git push ; git push", pushUnsure, nil},
	{"a lost push with global options", "cat <(true)# ; git -C /r -c k=v push origin BR", pushUnsure, nil},
	{"a lost push, quoted value of -C", "cat <(true)# ; git -C \"/tmp/a b\" push origin BR", pushUnsure, nil},
	{"a lost push, single-quoted value of -c", "cat <(true)# ; git -c 'k=a b' push origin BR", pushUnsure, nil},
	{"a lost push, escaped blank in the value of -C", "cat <(true)# ; git -C /tmp/a\\ b push origin BR", pushUnsure, nil},
	{"a lost push, --namespace with a separate value", "cat <(true)# ; git --namespace x push origin BR", pushUnsure, nil},
	{"a lost push, --git-dir with a separate value", "cat <(true)# ; git --git-dir /x push origin BR", pushUnsure, nil},
	{"a lost push, --work-tree with a separate value", "cat <(true)# ; git --work-tree /x push origin BR", pushUnsure, nil},
	{"a lost push spelled with quotes", "cat <(true)# ; g'i't pu\"s\"h origin BR", pushUnsure, nil},
	{"a lost push spelled with a backslash", "cat <(true)# ; git pu\\sh origin BR", pushUnsure, nil},
	{"a lost push, verb split by a line continuation", "cat <(true)# ; git pu\\\nsh origin BR", pushUnsure, nil},
	{"two lost pushes, one with a quoted value and an apostrophe in a comment before it", "echo a # it's\ngit pu\\\nsh${IFS}origin BR | cat ; git -C \"/tmp/a b\" push origin BR\n\nx=$(echo a # it's\ntrue)", pushUnsure, nil},
	{"a lost push, quoted value after an odd quote on an earlier line", "cat <(true)#\"\ncat <<EOF \"\n\necho \"a\n# not a comment\" & git -C \"/tmp/a b\" push origin BR", pushUnsure, nil},
	{"a lost push, attached double-quoted value with a blank after -c", "cat <(true)# ; git -c http.extraheader=\"Authorization: x\" push origin BR", pushUnsure, nil},
	{"a lost push, attached single-quoted value with a blank after -c", "cat <(true)# ; git -c user.name='A B' push origin BR", pushUnsure, nil},
	{"a lost push, attached quoted value after a key", "cat <(true)# ; git -c a=\"b c\" push origin BR", pushUnsure, nil},
	{"a lost push, --git-dir= with a quoted blank", "cat <(true)# ; git --git-dir=\"/a b\" push origin BR", pushUnsure, nil},
	{"a lost push, --work-tree= with a quoted blank", "cat <(true)# ; git --work-tree=\"/a b\" push origin BR", pushUnsure, nil},
	{"a lost push, --namespace= with a quoted blank", "cat <(true)# ; git --namespace=\"a b\" push origin BR", pushUnsure, nil},
	{"a lost push, --config-env= with a quoted blank", "cat <(true)# ; git --config-env=\"a b\" push origin BR", pushUnsure, nil},
	{"a lost push, -C value of two adjacent quoted strings", "cat <(true)# ; git -C 'a b'\"c d\" push origin BR", pushUnsure, nil},
	{"a lost push, -C value with a quote inside a word", "cat <(true)# ; git -C a\"b c\" push origin BR", pushUnsure, nil},
	{"a lost push, -C value is an ANSI-C string with a blank", "cat <(true)# ; git -C $'a b' push origin BR", pushUnsure, nil},
	{"a lost push, escaped quote inside a quoted value", "cat <(true)# ; git -c a=\"b\\\" c\" push origin BR", pushUnsure, nil},
	{"a lost push, command word is a parameter expansion with a default", "cat <(true)# ; ${g:-git} push origin BR", pushUnsure, nil},
	{"a lost push, a literal STX byte inside a quoted value attached to its option", "cat <(true)# ; git -c a=\"b\x02 c\" push origin BR", pushUnsure, nil},
	{"a lost push, a literal ETX byte inside a single-quoted value attached to its option", "cat <(true)# ; git -c a='b\x03 c' push origin BR", pushUnsure, nil},
	{"a lost push, newline inside a single-quoted value", "cat <(true)# ; git -c 'a\nb' push origin BR\necho '", pushUnsure, nil},
	{"a lost push, newline inside a double-quoted value", "cat <(true)# ; git -C \"a\nb\" push origin BR\necho \"", pushUnsure, nil},
	{"a lost push, newline inside a single-quoted value attached to its option", "cat <(true)# ; git -c a='b\nc' push origin BR\necho '", pushUnsure, nil},
	// A comment apostrophe, then a lost push whose value spans lines, then an odd quote. The
	// push is on the lead-in's line so the tokeniser loses it, and only the wide shape reads
	// a value across a line: without the wide shape this is "no push".
	{"a lost push, a comment apostrophe, a multi-line quoted value, an odd quote after", "echo a # it's\ncat <(true)# ; git -c 'a\nb' push origin BR\necho 'x'", pushUnsure, nil},
	// Three pushes, the first behind the lead-in, mixed blank-free and blank quoted values.
	// The tokeniser finds two of the three, so only the count refuses it: a quote span that
	// pairs shifted by one swallows a whole push, and a shape that did so would count two,
	// equal to the tokeniser's. The count is at least the line shape's, which reads all three.
	{"a lost push among three, mixed blank-free and blank double-quoted values", "cat <(true)# ; git -C \"/a\" push origin BR\ngit -C \"/b\" push origin BR\ngit -C \"c d\" push origin BR", pushUnsure, nil},
	{"a lost push among three, mixed blank-free and blank single-quoted values", "cat <(true)# ; git -C '/a' push origin BR\ngit -C '/b' push origin BR\ngit -C 'c d' push origin BR", pushUnsure, nil},
	{"a lost push among three, a blank-free value holding an escaped quote", "cat <(true)# ; git -c a=\"b\\\"c\" push origin BR\ngit -c a=\"b\\\"c\" push origin BR\ngit -C \"c d\" push origin BR", pushUnsure, nil},
	{"a lost push among three, a blank-free value holding backticks", "cat <(true)# ; git -c \"`echo y`\" push origin BR\ngit -c \"`echo y`\" push origin BR\ngit -C \"c d\" push origin BR", pushUnsure, nil},
	{"a lost push, -C value is a $( ) with blanks", "cat <(true)# ; git -C $(echo /x) push origin BR", pushUnsure, nil},
	{"a lost push, -C value is a $( ) nested twice", "cat <(true)# ; git -C $(echo $(echo $(echo /x))) push origin BR", pushUnsure, nil},
	{"a lost push, attached $( ) running git, as in the rev-parse idiom", "cat <(true)# ; git -c a=$(git rev-parse --show-toplevel) push origin BR", pushUnsure, nil},
	{"a lost push, -C value is a $( ) holding &&", "cat <(true)# ; git -C $(true && echo /x) push origin BR", pushUnsure, nil},
	{"a lost push, -C value is backticks", "cat <(true)# ; git -C `true && echo /x` push origin BR", pushUnsure, nil},
	{"a lost push, -C value is arithmetic", "cat <(true)# ; git -C $((1 + 2)) push origin BR", pushUnsure, nil},
	{"a lost push, -C value is a $( ) inside a $( )", "cat <(true)# ; git -C $(echo $(echo /x)) push origin BR", pushUnsure, nil},
	{"a lost push, attached $( ) after -c", "cat <(true)# ; git -c a=$(true && echo b) push origin BR", pushUnsure, nil},
	{"a lost push, attached $( ) after --git-dir=", "cat <(true)# ; git --git-dir=$(true && echo b) push origin BR", pushUnsure, nil},
	{"a lost push, a $( ) holding a quoted blank", "cat <(true)# ; git -C $(echo \"  /x\") push origin BR", pushUnsure, nil},
	{"a lost push, command word is a variable", "cat <(true)# ; g=git; $g push origin BR", pushUnsure, nil},
	{"H2 # directly after a process substitution", "cat <(true)# ; git push origin BR", pushUnsure, nil},
	{"H3 # after $( ) opens a double quote the stripper skips", "echo $(true)#\"\ncat <<EOF \"\ngit push origin BR\nEOF", pushUnsure, nil},
	{"H3 # after $(( )) opens a double quote the stripper skips", "echo $((1))#\"\ncat <<EOF \"\ngit push origin BR\nEOF", pushUnsure, nil},
	{"H3 # after <( ) opens a double quote the stripper skips", "cat <(true)#\"\ncat <<EOF \"\ngit push origin BR\nEOF", pushUnsure, nil},
	// No tokeniser lead-in is needed for these: a verb directly followed by an expansion
	// reads as one dynamic word, never as `push`.
	{"a verb directly before $( )", "git push$(true) origin BR", pushUnsure, nil},
	{"a continuation-split verb before $( )", "git pu\\\nsh$(true) origin BR", pushUnsure, nil},
	{"a continuation-split verb before backticks", "git pu\\\nsh`true` origin BR", pushUnsure, nil},
	{"a continuation-split verb before $x", "git pu\\\nsh$x origin BR", pushUnsure, nil},
	{"a continuation-split verb before ${IFS}", "git pu\\\nsh${IFS}origin BR", pushUnsure, nil},
}

// parserOnly are read correctly but are not meaningful refs, so only the parser is asked.
var parserOnly = []parseCase{
	{"git -C and -c together", "git -C /r -c k=v push origin BR", pushFound, []string{"origin BR"}},
	{"assignment with a quoted value", `REVIEW_GATE_BYPASS="a reason" git push origin BR`, pushFound, []string{"origin BR"}},
	{"substitution argument of a push", `git push origin "$(git rev-parse --abbrev-ref HEAD)"`, pushFound,
		[]string{"origin $(git rev-parse --abbrev-ref HEAD)"}},
	{"a quoted separator inside a push arg stays one word", `git push origin 'BR;x'`, pushFound, []string{"origin BR;x"}},
	{"a quoted && inside a push arg", `git push origin "a && b"`, pushFound, []string{"origin a && b"}},
}

func isInvariantCase(name string) bool {
	for _, set := range [][]parseCase{costOfInvariant, lostPushes} {
		for _, c := range set {
			if c.name == name {
				return true
			}
		}
	}
	return false
}

// corpusCases is every line that carries a push, for the hook tests.
func corpusCases() []parseCase {
	var all []parseCase
	for _, set := range [][]parseCase{ticketCorpus, moreUnreadable, moreReadable, costOfInvariant, lostPushes} {
		all = append(all, set...)
	}
	return all
}

func allParseCases() []parseCase {
	var all []parseCase
	all = append(all, parserOnly...)
	all = append(all, ticketCorpus...)
	all = append(all, moreUnreadable...)
	all = append(all, moreReadable...)
	all = append(all, controls...)
	all = append(all, costOfInvariant...)
	all = append(all, lostPushes...)
	return all
}

func TestParsePushCommands_Corpus_FoundOrRefusedNeverAbsent(t *testing.T) {
	for _, c := range allParseCases() {
		t.Run(c.name, func(t *testing.T) {
			p := parsePushCommands(c.cmd)
			if p.State != c.want {
				t.Fatalf("%q: state = %v (why %q, pushes %v), want %v", c.cmd, p.State, p.Why, p.Invocations, c.want)
			}
			switch c.want {
			case pushFound:
				if len(p.Invocations) != len(c.args) {
					t.Fatalf("%q: found %d pushes %v, want %d", c.cmd, len(p.Invocations), p.Invocations, len(c.args))
				}
				for i, inv := range p.Invocations {
					if got := strings.Join(inv.args, " "); got != c.args[i] {
						t.Errorf("%q: push %d args = %q, want %q", c.cmd, i, got, c.args[i])
					}
				}
			case pushUnsure:
				if p.Why == "" {
					t.Errorf("%q: unsure must say why (the deny message names it)", c.cmd)
				}
				// A line with a rule of its own must be refused BY that rule: the independent
				// check would refuse it too, and would hide a rule that stopped working.
				byInvariant := isInvariantCase(c.name)
				if byInvariant && p.Why != unreadInvariantWhy {
					t.Errorf("%q: refused for %q, want the independent check", c.cmd, p.Why)
				}
				if !byInvariant && p.Why == unreadInvariantWhy {
					t.Errorf("%q: refused only by the independent check; the rule meant to catch it did not", c.cmd)
				}
			}
		})
	}
}

// The corpus must have no silent member: every non-control line is found or unsure.
func TestParsePushCommands_Corpus_NothingSilentlyAbsent(t *testing.T) {
	for _, set := range [][]parseCase{ticketCorpus, moreUnreadable, moreReadable, costOfInvariant, lostPushes} {
		for _, c := range set {
			if c.want == pushNone {
				t.Errorf("%s: a corpus line expected to be silently absent", c.name)
			}
		}
	}
}

func TestParsePushCommands_RepoCarryAndHints(t *testing.T) {
	p := parsePushCommands("cd /r && ( git push origin x )")
	if p.State != pushFound || p.Invocations[0].repoPath != "/r" {
		t.Errorf("cd carries into a group opened by &&: %+v", p)
	}
	p = parsePushCommands("( cd /r ) && git push origin x")
	if p.State != pushFound || p.Invocations[0].repoPath != "" {
		t.Errorf("a cd inside a closed subshell must not carry: %+v", p)
	}
	p = parsePushCommands("cd /r && git -C /s push origin x")
	if p.Invocations[0].repoPath != "/s" {
		t.Errorf("-C wins over a carried cd: %+v", p)
	}
	p = parsePushCommands("cd /r && bash -c 'git -C /s push origin x'")
	if p.State != pushUnsure || strings.Join(p.Repos, ",") != "/r,/s" {
		t.Errorf("an unreadable command must report the repos it mentions: %+v", p)
	}
	p = parsePushCommands("( git push origin x )")
	if p.State != pushFound || p.Invocations[0].leading {
		t.Errorf("a push inside a group is not the line's leading command: %+v", p)
	}
	p = parsePushCommands("git push origin x && git push origin y")
	if !p.Invocations[0].leading || p.Invocations[1].leading {
		t.Errorf("only the first push leads: %+v", p)
	}
}

// substituteBranch turns a corpus line into a concrete command pushing branch br.
func substituteBranch(cmd, br string) string { return strings.ReplaceAll(cmd, bad, br) }

// The gate, fed every line: a push of an UNGATED branch is refused whether the parser found
// it or could not read it; a GATED branch is allowed when the push was found and judged,
// but still refused when it was unreadable (the gate cannot know what was pushed).
func TestPushGate_Corpus_BothOutcomesAreHonoured(t *testing.T) {
	cases := corpusCases()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := refPushEnv(t, shaGated)
			ungated := substituteBranch(c.cmd, "feat/ungated")
			res := PushGate(bashInput(ungated), cfg)
			assertDeny(t, res, ungated)
			if c.want == pushFound && !strings.Contains(res.Stdout, shaUngated[:12]) {
				t.Errorf("a found push must be judged on its sha %s, got:\n%s", shaUngated[:12], res.Stdout)
			}
			if c.want == pushUnsure && !strings.Contains(res.Stdout, "plain command") {
				t.Errorf("an unreadable command's deny must say what to use instead, got:\n%s", res.Stdout)
			}

			gated := substituteBranch(c.cmd, "feat/gated")
			res = PushGate(bashInput(gated), cfg)
			if c.want == pushFound {
				assertAllow(t, res, gated+" (found, judged, gated)")
			} else {
				assertDeny(t, res, gated+" (unreadable, so refused even though the branch is gated)")
			}
		})
	}
}

func TestPushGate_Controls_StayAllowed(t *testing.T) {
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := refPushEnv(t, shaGated)
			cmd := substituteBranch(c.cmd, "feat/ungated")
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd)
			cfg.Mode = modeResolver(ModeEnforce)
			assertAllow(t, PushGate(bashInput(cmd), cfg), cmd+" (enforce)")
		})
	}
}

// The ticket's discriminating control: the same ungated push is refused unwrapped and was
// permitted wrapped. Both must now be refused.
func TestPushGate_SamePush_WrappedAndUnwrapped_BothRefused(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	for _, cmd := range []string{
		"git push origin feat/ungated",
		"( git push origin feat/ungated )",
		"bash -c 'git push origin feat/ungated'",
		"eval 'git push origin feat/ungated'",
		"$(git push origin feat/ungated)",
	} {
		assertDeny(t, PushGate(bashInput(cmd), cfg), cmd)
	}
}

// A refusal for an unreadable command is still subject to scope and the documented bypass,
// like the refusal of a push whose commits cannot be named.
func TestPushGate_Unsure_HonoursScopeAndBypass(t *testing.T) {
	const cmd = "bash -c 'git push origin feat/ungated'"
	cfg, repo := refPushEnv(t, shaGated)
	cfg.ResolveGate = staticGate(&gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{repo: "git@github.com:other/unrelated.git"}, head: shaGated},
		EnforcedRemotes: []string{enforcedSubstr},
	})
	assertAllow(t, PushGate(bashInput(cmd), cfg), "out of scope")

	cfg, _ = refPushEnv(t, shaGated)
	t.Setenv("REVIEW_GATE_BYPASS", "hotfix")
	assertAllow(t, PushGate(bashInput(cmd), cfg), "env bypass")
	t.Setenv("REVIEW_GATE_BYPASS", "")

	// An inline assignment prefixes ONE command and the hidden push is not known to be it,
	// so the unreadable form does not honour it.
	assertDeny(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="hotfix" `+cmd), cfg), "inline bypass is not honoured for an unreadable command")
	assertAllow(t, PushGate(bashInput(`REVIEW_GATE_BYPASS="hotfix" git push origin feat/ungated`), cfg), "...but is for a push that was read")
}

// An unreadable command that moves into a gated repo is judged against THAT repo, even when
// the session sits in one the gate does not govern.
func TestPushGate_Unsure_JudgesTheReposTheCommandMentions(t *testing.T) {
	session, gated := t.TempDir(), t.TempDir()
	g := &gate.Gate{
		RecordsDir: t.TempDir(),
		Git: fakeGateGit{
			remotes: map[string]string{session: "git@github.com:other/unrelated.git", gated: enforcedRemote},
			head:    shaGated,
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{ResolveGate: staticGate(g), Cwd: session}
	assertDeny(t, PushGate(bashInput("cd "+gated+" && bash -c 'git push origin feat/x'"), cfg), "cd into a gated repo")
	assertDeny(t, PushGate(bashInput("bash -c 'cd "+gated+" && git push origin feat/x'"), cfg), "cd inside the unreadable part")
	assertAllow(t, PushGate(bashInput("bash -c 'git push origin feat/x'"), cfg), "no gated repo mentioned")
}

// `leadkit push` is allowed, but must never whitewash an unreadable command beside it.
func TestPushGate_LeadkitPush_DoesNotWhitewashAnUnreadableCommand(t *testing.T) {
	cfg, _ := refPushEnv(t, shaGated)
	assertAllow(t, PushGate(bashInput("leadkit push --repo /r --branch feat/gated"), cfg), "leadkit push alone")
	assertDeny(t, PushGate(bashInput("leadkit push --repo /r --branch feat/gated && bash -c 'git push origin feat/ungated'"), cfg), "leadkit push then a hidden push")
}

// The guard, fed every line: a push the parser found is judged on the whitelist; a command
// it could not read is refused, and logged as a security event.
func TestPushGuard_Corpus_BothOutcomesAreHonoured(t *testing.T) {
	noRemoteStub(t)
	cfg := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	cases := corpusCases()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			protected := substituteBranch(c.cmd, "release/bad")
			assertDeny(t, PushGuard(pushInput(protected), cfg), protected)

			ok := substituteBranch(strings.ReplaceAll(c.cmd, good, "feat/ok"), "feat/ok")
			res := PushGuard(pushInput(ok), cfg)
			if c.want == pushFound {
				assertAllow(t, res, ok+" (found, whitelisted)")
			} else {
				assertDeny(t, res, ok+" (unreadable, so refused even though the branch is whitelisted)")
				if !strings.Contains(res.Stdout, "plain command") {
					t.Errorf("an unreadable command's deny must say what to use instead, got:\n%s", res.Stdout)
				}
			}
		})
	}
}

func TestPushGuard_Controls_StayAllowed(t *testing.T) {
	noRemoteStub(t)
	cfg := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			cmd := substituteBranch(c.cmd, "release/bad")
			assertAllow(t, PushGuard(pushInput(cmd), cfg), cmd)
		})
	}
}

func TestPushGuard_Unsure_IsLoggedAsAViolation(t *testing.T) {
	noRemoteStub(t)
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	res := PushGuard(
		Input{ToolName: "Bash", ToolInput: toolInput{Command: "bash -c 'git push origin feat/ok'"}, SessionID: "sess-165"},
		PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath},
	)
	assertDeny(t, res, "unreadable push")
	recs := readViolationLines(t, logPath)
	if len(recs) != 1 || recs[0].Reason != "unparseable" || recs[0].SessionID != "sess-165" || recs[0].Command == "" {
		t.Errorf("logged records = %+v, want one with reason=unparseable", recs)
	}
	if !strings.Contains(res.Stdout, "push-guard mode: advisory") {
		t.Errorf("the deny names the mode, got:\n%s", res.Stdout)
	}
}

// gitPushShapeLine is frozen. It is the one pattern the count's floor rests on: the count is
// the largest over the shapes, so while the line shape is this exact pattern the count can
// never fall below what this pattern reads. The wide shape shares shapeGap and the quote
// marks with it, so the COMPILED text is what is pinned: an edit to shapeValueLine, shapeGap
// or a mark changes it. If this fails, the floor has moved; change the pattern only
// deliberately, and then update the digest here.
//
// This freezes the PATTERN, not the whole floor. The floor also depends on shapeTexts (the
// readings it is counted on) and on countPushShapes taking the maximum over the shapes, and
// this test pins neither. TestCountPushShapes pins both readings of shapeTexts: each has a
// case only it reads (a ")" inside a quote inside a $( ) for the quote-stripped reading, a
// quoted value with a blank for the marked one), and with either reading deleted that test
// fails. What it pins about the maximum over the shapes is the cases only the line shape
// reads.
func TestGitPushShapeLine_IsFrozen(t *testing.T) {
	const wantLen = 414
	const wantSum = "2f4e4d70523b9b57e448c8fa5bf238d4216e37111216ef4d1e7a9d5b91c1cb35"
	got := gitPushShapeLine.String()
	if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(got))); len(got) != wantLen || sum != wantSum {
		t.Errorf("the line shape's compiled pattern changed (len %d, sha256 %s), want len %d, sha256 %s:\n%q",
			len(got), sum, wantLen, wantSum, got)
	}
}

// countPushShapes is the independent check's measure. It must count each push the text
// holds, whatever it is spelled with and however close the next one sits: two adjacent bare
// pushes are two.
func TestCountPushShapes(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want int
	}{
		{"one", "git push", 1},
		{"two bare pushes on adjacent lines", "git push\ngit push", 2},
		{"two bare pushes after a semicolon", "git push;git push", 2},
		{"two bare pushes after &&", "git push && git push", 2},
		{"three bare pushes", "git push\ngit push\ngit push", 3},
		{"two pushes with arguments", "git push origin a\ngit push origin b", 2},
		{"a quoted value of -C", `git -C "/tmp/a b" push`, 1},
		{"a single-quoted value of -c", `git -c 'k=a b' push`, 1},
		{"an escaped blank in a value", `git -C /tmp/a\ b push`, 1},
		{"a long option with a separate value", "git --namespace x push", 1},
		{"--git-dir with a separate value", "git --git-dir /x push", 1},
		{"a verb split by a continuation", "git pu\\\nsh", 1},
		{"a command word split by a continuation", "g\\\nit push", 1},
		{"a continuation between the words", "git \\\n push", 1},
		{"a continuation-split verb before an expansion", "git pu\\\nsh$(true)", 1},
		{"a backslash-newline that is only text before git", "echo EOF\\\ngit push", 1},
		{"a verb spelled with quotes", `g'i't pu"s"h`, 1},
		{"a command word that is a variable", "g=git; $g push", 1},
		{"a braced variable", "g=git; ${g} push", 1},
		{"a quoted value after a comment with an apostrophe", "echo a # it's\ngit -C \"/tmp/a b\" push origin x\necho 'z'", 1},
		{"a single-quoted value after a comment with a double quote", "echo a # say \"hi\ngit -C '/tmp/a b' push origin x\necho \"z\"", 1},
		{"a quoted value after an odd quote on an earlier line", "echo \"a\n# not a comment\" & git -C \"/tmp/a b\" push origin x", 1},
		{"an attached double-quoted value with a blank", `git -c http.extraheader="Authorization: x" push`, 1},
		{"an attached single-quoted value with a blank", `git -c user.name='A B' push`, 1},
		{"--git-dir= with a quoted blank", `git --git-dir="/a b" push`, 1},
		{"--work-tree= with a quoted blank", `git --work-tree="/a b" push`, 1},
		{"--namespace= with a quoted blank", `git --namespace="a b" push`, 1},
		{"--config-env= with a quoted blank", `git --config-env="a b" push`, 1},
		{"a value of two adjacent quoted strings", `git -C 'a b'"c d" push`, 1},
		{"a quote inside a word of a value", `git -C a"b c" push`, 1},
		{"an ANSI-C value with a blank", `git -C $'a b' push`, 1},
		{"an escaped quote inside a quoted value", `git -c a="b\" c" push`, 1},
		{"an escaped backslash before the closing quote", `git -c a="b c\\" push`, 1},
		{"a command word that is an expansion with a default", "${g:-git} push", 1},
		{"a literal STX inside a quoted value attached to its option", "git -c a=\"b\x02 c\" push", 1},
		{"a literal ETX inside a single-quoted value attached to its option", "git -c a='b\x03 c' push", 1},
		{"a single-quoted value with a newline", "git -c 'a\nb' push", 1},
		{"a double-quoted value with a newline", "git -C \"a\nb\" push", 1},
		{"an attached single-quoted value with a newline", "git -c a='b\nc' push", 1},
		{"a quoted value with a newline, an odd quote after", "git -c 'a\nb' push origin x\necho '", 1},
		{"a comment apostrophe, a multi-line quoted value, an odd quote after", "echo a # it's\ngit -c 'a\nb' push origin x\necho 'z'", 1},
		// A KNOWN FALSE POSITIVE, held here as a cost so that it is not a surprise: a quote in a
		// comment after `git -C <word>` pairs across the line with the next quote, and the word
		// `push` follows that. Bash does not push; the check counts one, and the command is refused.
		// It needs the data text `git -C <word>'` and, on a later line, a quote and `push`. Only
		// the wide shape reads a quote across a line, so ending the cost means changing that.
		{"a comment apostrophe pairing across a line with a quote before push is a known false positive", "git status # git -C it's\necho 'now push'", 1},
		// Three pushes with mixed blank-free and blank quoted values. A quote span that pairs
		// shifted by one, closing one value to opening the next, swallows a whole push across the
		// newline: the count was two. Each line is a push, so each is counted.
		{"three pushes, double-quoted values, two without a blank and one with", "git -C \"/a\" push origin x\ngit -C \"/b\" push origin x\ngit -C \"c d\" push origin x", 3},
		{"three pushes, single-quoted values, two without a blank and one with", "git -C '/a' push origin x\ngit -C '/b' push origin x\ngit -C 'c d' push origin x", 3},
		{"three pushes, a blank-free value holding an escaped quote", "git -c a=\"b\\\"c\" push origin x\ngit -c a=\"b\\\"c\" push origin x\ngit -C \"c d\" push origin x", 3},
		{"three pushes, a blank-free value holding backticks", "git -c \"`echo y`\" push origin x\ngit -c \"`echo y`\" push origin x\ngit -C \"c d\" push origin x", 3},
		{"three pushes, a blank-free value holding an escaped dollar", "git -C \"\\$(x)\" push origin x\ngit -C \"\\$(x)\" push origin x\ngit -C \"c d\" push origin x", 3},
		{"three pushes, the last value spans lines", "git -C \"/a\" push origin x\ngit -C \"/a\" push origin x\ngit -C 'a\nb' push origin x", 3},
		{"three pushes, a middle value spans lines", "git -C '/a' push origin x\ngit -C \"a\nb\" push origin x\ngit -C '/a' push origin x", 3},
		// The line shape's own pin: a lone quote on a push line (data, a half-typed command) lets
		// a value span the lines after it, which swallows a push in the wide shape alone. Taking
		// the larger of the two shapes keeps the third.
		{"three pushes, a lone quote on the first line", "git -C \"a push x\ngit -C \"/a\" push x\ngit -C \"c d\" push x", 3},
		{"three pushes, the value with a blank first", "git -C \"c d\" push origin x\ngit -C \"/a\" push origin x\ngit -C \"/b\" push origin x", 3},
		{"three pushes, values of mixed quotes and one bare", "git -C \"/a\" push origin x\ngit -C '/b' push origin x\ngit -C /c push origin x", 3},
		{"three pushes on one line", "git -C \"/a\" push origin x; git -C \"/b\" push origin x; git -C \"c d\" push origin x", 3},
		{"a $( ) value", "git -C $(echo /x) push", 1},
		// Each reading shapeTexts returns needs a case that only it satisfies, or the other
		// reading can be deleted without a failure. The $( ) ones above hold no quote, so both
		// readings read them. A ")" inside a quote closes the $( ) early for the marked reading,
		// which sees the quote as one word and finds no push (0); the stripped reading deletes
		// the quotes and reads it (1). And "/a b" the other way: marked keeps the value one
		// word (1), stripped splits it at the blank and finds no push (0).
		{"a ) in a quote inside a $( ) value, which only the stripped reading reads", `git -C $(echo ")") push`, 1},
		{"a quoted value with a blank, which only the marked reading reads", `git -C "a b" push`, 1},
		{"a $( ) value holding &&", "git -C $(true && echo /x) push", 1},
		{"the rev-parse idiom", "git -C $(git rev-parse --show-toplevel) push", 1},
		{"a backtick value", "git -C `true && echo /x` push", 1},
		{"an arithmetic value", "git -C $((1 + 2)) push", 1},
		{"a $( ) inside a $( )", "git -C $(dirname $(echo /x/y)) push", 1},
		{"a $( ) two levels inside a $( )", "git -C $(a $(b $(c))) push", 1},
		{"a $( ) two levels inside a $( ), each holding blanks", "git -C $(dirname $(echo $(echo /x y))) push", 1},
		{"an attached $( ) value", "git -c a=$(true && echo b) push", 1},
		{"--git-dir= with a $( ) value", "git --git-dir=$(true && echo b) push", 1},
		{"a $( ) value before a subcommand that is not push", "git -C $(echo /x) stash push", 0},
		{"a $( ) value with no push after it", "git -C $(echo /x) status", 0},
		{"a whole word quoted", `"git" "push"`, 1},
		{"a quoted option name", `git "-C" /x push`, 1},
		{"a subcommand after global options", `git -C "/tmp/a b" stash push`, 0},
		{"push as a prefix", "git pushed", 0},
		{"push with a hyphen", "git push-all", 0},
		{"push as a file stem", "git push.txt", 0},
		{"git as a suffix", "legit push", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := countPushShapes(c.cmd); got != c.want {
				t.Errorf("countPushShapes(%q) = %d, want %d", c.cmd, got, c.want)
			}
		})
	}
}

// The deny for a command refused only by the independent check has to point somewhere that
// works: the usual way to create a file, a shell heredoc, carries the same words and is
// refused the same way, so the advice names a file-writing tool and says so.
func TestUnreadInvariantWhy_AdviceDoesNotSendYouToAHeredoc(t *testing.T) {
	p := parsePushCommands("cat > notes.md <<'EOF'\ngit push origin BR\nEOF")
	if p.State != pushUnsure || p.Why != unreadInvariantWhy {
		t.Fatalf("expected the independent check to refuse this: %+v", p)
	}
	for _, want := range []string{"file-writing tool", "not a shell heredoc"} {
		if !strings.Contains(p.Why, want) {
			t.Errorf("the deny %q does not say %q", p.Why, want)
		}
	}
}

// An agent with no file-writing tool is told what to do instead, and what it is told to do
// has to work: the example in the deny is itself read as no push at all.
func TestUnreadInvariantWhy_HasAWayOutWithoutAWriteTool(t *testing.T) {
	if !strings.Contains(unreadInvariantWhy, "no file-writing tool") || !strings.Contains(unreadInvariantWhy, unreadInvariantNoTool) {
		t.Errorf("the deny %q does not give the printf way out", unreadInvariantWhy)
	}
	if p := parsePushCommands(unreadInvariantNoTool); p.State != pushNone {
		t.Errorf("the example in the deny is itself refused or read as a push: %+v", p)
	}
}
