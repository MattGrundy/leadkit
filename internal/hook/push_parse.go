package hook

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// The shared push parser: what `git push` invocations a Bash command carries, for BOTH
// push-gate and push-guard. It has three outcomes, not two, because "no push" and "I
// could not read this command" are different facts and a hook that cannot tell them apart
// fails OPEN on every phrasing it does not understand (a subshell, `bash -c`, `eval` or
// `$( … )` would walk a push straight past both hooks).
//
//	pushNone    nothing in the command is a `git push`, and nothing in it could hide one.
//	pushFound   every `git push` the command carries, each to be judged.
//	pushUnsure  the command contains a construct that may hold a push this parser does not
//	            read. BOTH hooks REFUSE, naming what to use instead.
//
// REFUSE, NOT PARSE. The shell is not regular; recognising `bash -c '…'`, `eval` and
// command substitution means writing a shell parser, and every gap in that parser is a new
// hole. Refusing is small and fails closed. The one thing this file reads structurally is
// what is cheap and exact: quotes and escapes, the separators, `( … )` and `{ … }` groups,
// the `if`/`for`/`while` keywords, an absolute-path or `-c k=v` spelling of git.
//
// A SECOND, INDEPENDENT CHECK backs all of this. Tokenising is where the holes are: a
// quote, a heredoc or a comment read differently from the shell loses the text of a push.
// So the result is cross-checked against the raw text: if the text holds more `git … push`
// than were found, the command is unsure (see gitPushShapeLine and gitPushShapeWide). That turns a tokeniser bug
// into a refusal instead of a hole. Its cost is that a command which names `git push` only
// as data (a commit message, an echo, a heredoc body) is refused too.
//
// UNSURE is deliberately NOT "contains a construct": `git commit -m "$(cat <<'EOF' … EOF)"`
// is an everyday command and must stay allowed. A construct only makes the command unsure
// when the text it hides mentions a `push` (see containsPush); a substitution that reads
// `$(date)` or `$(git rev-parse HEAD)` hides no push. What this does NOT catch is a verb
// assembled at run time (`p=push; git $p`, `$(printf pu%s sh)`), a verb spelled in a form the
// shape does not read (`git $'push'`, `git pu{s,s}h`, `git ${IFS}push`, `git -c alias.p=push
// p`), or a script file's contents (`bash release.sh`): no static reading can, short of
// running the shell.

// pushState is parsePushCommands' three-way outcome.
type pushState int

const (
	pushNone   pushState = iota // no git push, nothing that could hide one: the hook allows
	pushFound                   // pushes found: the hook judges each
	pushUnsure                  // may hide a push this parser cannot read: the hook REFUSES
)

// pushParse is what parsePushCommands returns.
type pushParse struct {
	State       pushState
	Invocations []pushInvocation // pushFound: one per push, in command order
	// Why says what could not be read (pushUnsure only), for the deny message.
	Why string
	// Repos lists the repo paths an unreadable command mentions through `cd <path>` or
	// `-C <path>` (pushUnsure only). A refusal is scoped by the repo the push would
	// target, which an unreadable command cannot name, so the gate judges the session's
	// cwd AND each of these.
	Repos []string
	// Unknown, when non-empty (pushUnsure only), says that a push in the command targets a repo
	// the text does not name (a computed, globbed or assigned path, a `cd -`), so no scope can be
	// decided for it; see gate.CheckUnknownTarget.
	Unknown string
}

// pushInvocation is one `git push` found in a command — the resolved repo path
// ("" = process cwd) and the cleaned tokens AFTER "push" (flags + remote + refspecs,
// with all shell redirects/operators already stripped out).
type pushInvocation struct {
	repoPath string
	args     []string
	// leading is true for a push that opens the command line, the only one a leading
	// `VAR=value` assignment prefixes.
	leading bool
	// afterOthers is true when another command precedes the push on the line, which could
	// have made the directory the push targets (`git clone u d && cd d && …`).
	afterOthers bool
	// gitConfig is the configuration the push's own command line sets (gitPrefixConfig).
	gitConfig []string
}

// shellWrappers are commands that run text they are given as a command: their arguments
// can carry a whole `git push` the parser cannot see as one. A wrapper whose arguments
// mention a push makes the command unsure. The list is explicit and bounded, not
// "anything unrecognised", because `echo`, `grep` and `git commit -m` take push as data.
var shellWrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true,
	"eval": true, "source": true, ".": true, "exec": true, "command": true, "builtin": true, "env": true,
	"xargs": true, "parallel": true, "find": true, "watch": true, "sudo": true, "doas": true, "su": true,
	"ssh": true, "nohup": true, "nice": true, "ionice": true, "timeout": true, "setsid": true, "stdbuf": true,
	"flock": true, "unshare": true, "chroot": true, "script": true, "tmux": true, "screen": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true, "php": true, "lua": true,
	"awk": true, "gawk": true, "osascript": true,
}

// shellInterpreters are the shellWrappers that run text from stdin or a here-string.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true,
}

// heredocConsumers are commands that read a heredoc as DATA. A heredoc body that mentions
// push is dropped from the reading only when its consumer is one of these and no wrapper
// runs anywhere in the command (`cat <<EOF | sh` feeds the body to a shell).
var heredocConsumers = map[string]bool{
	"cat": true, "tee": true, "git": true, "gh": true, "leadkit": true, "grep": true,
	"wc": true, "sort": true, "head": true, "tail": true,
}

// shellKeywords are words that open a command position without being the command: after
// one, the NEXT word is the command (`if git push …`, `then git push`, `{ git push; }`).
var shellKeywords = map[string]bool{
	"{": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"while": true, "until": true, "do": true, "time": true,
}

// pushToken matches `push` as a word of its own, not as part of a path, a file name or a
// longer word (`scripts/push-docs.sh`, `--push`, `pushed`).
var pushToken = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])push($|[^A-Za-z0-9_./-])`)

// gitPushShapeLine and gitPushShapeWide match the command word, any git global options, then
// `push`, in text that has been through shapeTexts: the shape of a push however it is quoted,
// escaped or path-spelled (`/usr/bin/git`, `g'i't`, `git -C x -c k=v push`). The command word is `git`
// or a `$variable` standing for it (`g=git; $g push`). A global option that takes a value
// may take it as the next word, quoted or not (`-C "a b"`, `--namespace x`, `--git-dir /x`).
//
// It is deliberately NOT "the word push": `git stash push`, `leadkit push` and `make push`
// do not match, so the governed verb and ordinary subcommands are untouched.
//
// shapeTexts leaves three kinds of mark in the text, which the pattern reads through:
// a removed line continuation, and a double or single quote. A mark is ignorable inside a
// word (`pu"s"h`) and a blank between words, and a quoted value is read by pairing the two
// marks that follow its option, nothing else, so a stray quote elsewhere (an apostrophe in
// a comment) cannot mis-pair it.
//
// There are TWO shapes and a text is counted on both (see countPushShapes). They differ in
// what an option's value may hold (see shapeValue):
//
//	gitPushShapeLine  a quoted value on one line, and no substitution. Its compiled text is
//	                  frozen by a test (TestGitPushShapeLine_IsFrozen): the count's floor.
//	gitPushShapeWide  a quoted value that spans lines, and a `$(...)` or backtick value.
//
// The wide shape is not a superset of the line one, and that is why both are counted. A
// quote span that can cross a line can pair the CLOSING mark of one value with the OPENING
// mark of the next, and one match then swallows a whole push in between (three pushes whose
// options are `-C "/a"`, `-C "/b"` and `-C "c d"` count three on the line shape and two on
// a wide one that lets the gap before a value take its opening mark). The wide shape's gap
// is lazy to stop that, and counting the line shape as well means it cannot happen again by
// construction: the count is at least the line shape's, and the line shape is a fixed pattern.
var (
	gitPushShapeLine = compilePushShape(shapeValueLine, shapeGap)
	gitPushShapeWide = compilePushShape(shapeValueWide, shapeLazyGap)
)

// compilePushShape builds the shape for one reading of an option's value. valueGap is the
// gap between an option and its value.
func compilePushShape(value, valueGap string) *regexp.Regexp {
	return regexp.MustCompile(
		`(^|[^a-z0-9_-])(?:` + spelledAcross("git") + `|\$\{?[a-z0-9_]+\}?|\$\{[^}\n]*\})` +
			`(?:` + shapeGap + shapeOption(value, valueGap) + `)*` + shapeGap + spelledAcross("push") + `($|[^a-z0-9_./-])`)
}

// The marks shapeTexts leaves in the text. They are control characters standing for a
// removed backslash-newline and for the two quotes. A command can contain such a byte (as
// one inside a JSON string), so shapeTexts first turns any it finds into a letter: a mark is
// then always one shapeTexts placed, and a quote pair cannot be forged or broken by input.
//
// A backslash-newline is the shell's line continuation, deleted whole, so `pu\<nl>sh` is
// the word `push`. It is marked, not deleted: where the line before ends in a backslash
// that is only text (a heredoc body), deleting the pair would glue `EOF\<nl>git` into
// `EOFgit` and hide the `git`.
const (
	continuationMark = "\x01"
	doubleQuoteMark  = "\x02"
	singleQuoteMark  = "\x03"
	anyMark          = `[\x01-\x03]`
)

// shapeGap is what separates two words of the shape: blanks, continuations or quotes.
// shapeLazyGap is the same, taking as little as it can: the gap between an option and its
// value, where taking the mark that OPENS a quoted value leaves the pairing one off.
const (
	shapeGap     = `[\s\x01-\x03]+`
	shapeLazyGap = `[\s\x01-\x03]+?`
)

// shapeValue is the value of a global option, or the part of it after an `=`: a run of
// characters, quoted strings and (in the wide reading) substitutions, which may hold blanks
// and may be joined without a gap (`a="b c"`, `'a b'"c d"`, `$'a b'`,
// `$(git rev-parse --show-toplevel)`). There are two readings, one per shape (see
// gitPushShapeLine and gitPushShapeWide).
//
// The line reading ends a quoted string at the end of its line and reads no substitution.
//
// The wide reading lets a quoted string span lines: `-c 'a<nl>b'` is one value to the shell.
// That cannot be told from a stray apostrophe pairing across lines by looking at the text,
// but the pairing is anchored to the option that precedes it (see gitPushShapeWide), and widening
// it costs a refusal of the odd mispairing, not a hole. It reads a substitution to its
// closing parenthesis, or backtick, with parentheses inside it nested up to two deep
// (`$((1 + 2))`, `$(dirname $(which x))`, `$(a $(b $(c)))`). Deeper nesting, a parenthesis
// inside a quote inside a substitution, and a quote inside a substitution that is itself
// inside a quote followed by more text, are not read: documented limits.
const (
	shapeParens = `(?:[^()]|\((?:[^()]|\([^()]*\))*\))*`

	shapeValueLine = `(?:[^\s\x01-\x03]|` + doubleQuoteMark + `[^` + doubleQuoteMark + `\n]*` + doubleQuoteMark +
		`|` + singleQuoteMark + `[^` + singleQuoteMark + `\n]*` + singleQuoteMark + `)+`
	shapeValueWide = `(?:[^\s\x01-\x03]|` + doubleQuoteMark + `[^` + doubleQuoteMark + `]*` + doubleQuoteMark +
		`|` + singleQuoteMark + `[^` + singleQuoteMark + `]*` + singleQuoteMark +
		`|\$\(` + shapeParens + `\)|` + "`[^`]*`" + `)+`
)

// shapeOption is one global option: a flag, an option that takes a value as the next word,
// or `--option=value`. valueGap is the gap between an option and a value that is the next word.
func shapeOption(value, valueGap string) string {
	return `(?:(?:-c|--git-dir|--work-tree|--namespace|--super-prefix|--config-env|--attr-source)` +
		valueGap + value + `|--[a-z0-9-]+=` + value + `|-\S*)`
}

// spelledAcross matches word with a mark allowed between any two letters.
func spelledAcross(word string) string {
	letters := make([]string, len(word))
	for i := range word {
		letters[i] = word[i : i+1]
	}
	return anyMark + "*" + strings.Join(letters, anyMark+"*") + anyMark + "*"
}

// unreadInvariantWhy is the reason given when the text holds more `git … push` than the
// structured reading found. Mentioning a push as data (a commit message, an echo, a
// heredoc) trips it too: that is the price of not trusting the tokeniser. Prose can be
// written to a file with a file-writing tool and passed by name; a shell heredoc that
// creates the file carries the same words and is refused the same way. With no such tool,
// `printf` can assemble the text from pieces that do not read as `git push` (see
// unreadInvariantNoTool, which a test runs through the parser).
const unreadInvariantWhy = "the command text contains `git push` that was not read as a push (quoted prose and heredoc bodies count: write such text to a file with a file-writing tool, not a shell heredoc, and pass the file; with no file-writing tool, assemble it with printf from pieces, as in `" + unreadInvariantNoTool + "`)"

// unreadInvariantNoTool is the example the deny gives to an agent with no file-writing
// tool: it writes `git push origin x` to a file without the command text holding it.
const unreadInvariantNoTool = `printf '%s%s\n' 'git pu' 'sh origin x' > notes.md`

var quoteStripper = strings.NewReplacer("'", "", `"`, "", `\`, "")

// lineContinuation deletes a backslash-newline whole, as the shell does. containsPush uses
// it beside the undeleted reading; the shape marks the pair instead (continuationMark).
var lineContinuation = strings.NewReplacer("\\\n", "")

// shapeTexts returns the readings of cmd that the shapes are counted on. There is more
// than one because the text cannot be read without the lexer whose mistakes this check
// exists to survive. Each reading can only ADD suspicion (the count is the largest of
// them, and of the two shapes), so a misreading in one never hides what another sees.
//
//	stripped  quotes and backslashes deleted: `g'i't pu"s"h` is `git push`.
//	marked    quotes kept as marks, so that a quoted option value stays one word, and a
//	          backslash-escaped blank or double quote made a non-blank for the same reason.
func shapeTexts(cmd string) []string {
	plain := strings.Map(func(r rune) rune {
		if r <= '\x03' {
			return 'a'
		}
		return r
	}, cmd)
	low := strings.NewReplacer("\\\n", continuationMark).Replace(strings.ToLower(plain))
	marked := strings.NewReplacer(
		`\\`, "\x00", `\"`, "\x00", "\\ ", "\x00", "\\\t", "\x00", `\`, "", `"`, doubleQuoteMark, `'`, singleQuoteMark,
	).Replace(low)
	return []string{quoteStripper.Replace(low), marked}
}

// countPushShapes counts the `git … push` occurrences in cmd, the largest count over the
// readings of the text (shapeTexts) and the two shapes (gitPushShapeLine, gitPushShapeWide).
// The scan resumes at the character after each match, and the pattern's `^` stands for the boundary that match
// consumed: `push` is followed by a boundary character, which is also the one before the next
// `git`, so a scan that required the boundary to still be there (regexp.FindAllString) would
// count `git push<nl>git push` as one push.
func countPushShapes(cmd string) int {
	most := 0
	for _, text := range shapeTexts(cmd) {
		for _, shape := range []*regexp.Regexp{gitPushShapeLine, gitPushShapeWide} {
			n := 0
			for rest := text; ; {
				loc := shape.FindStringIndex(rest)
				if loc == nil {
					break
				}
				n++
				rest = rest[loc[1]:]
			}
			most = max(most, n)
		}
	}
	return most
}

// containsPush reports whether raw shell text mentions `push` as a word, ignoring the
// quotes, escapes and line continuations that can split it (`pu""sh`, `pu\sh`).
func containsPush(raw string) bool {
	low := strings.ToLower(raw)
	return pushToken.MatchString(quoteStripper.Replace(low)) || pushToken.MatchString(quoteStripper.Replace(lineContinuation.Replace(low)))
}

// segment is one simple command of a lexed command line: its words (redirects and their file
// operands dropped), and the separators that joined it to the one before.
type segment struct {
	words   []string
	dynamic []bool
	herestr []string // `<<<` operands: a here-string is text handed to the command
	link    string   // the separators between the previous non-empty segment and this one
	// outRedirect: the segment sends its stdout somewhere other than the next pipe stage (`> f`,
	// `>> f`, `&> f`, `1> f`, `>&2`). A stderr-only redirect (`2>&1`) does not set it.
	outRedirect bool
	hereOps     int // heredoc operators (`<<`, `<<-`, not `<<<`) among the segment's redirects
}

// splitSegments strips the heredoc bodies from cmd, lexes the rest and splits it into simple-command
// segments. Both push hooks and pr-guard read a command line through it, so they agree on what its
// commands are.
func splitSegments(cmd string) (segs []segment, bodies []hereBody, lx shellLex) {
	text, bodies := stripHeredocs(cmd)
	lx = lexShellFull(text)
	cur := segment{}
	link := ""
	flush := func() {
		if len(cur.words) > 0 {
			cur.link = link
			segs = append(segs, cur)
			link = ""
		}
		cur = segment{}
	}
	skipOperand := false // a bare redirect just consumed → drop the next word (its file)
	hereString := false  // ...but keep a here-string's operand: it is text the command runs
	for _, t := range lx.toks {
		switch t.kind {
		case tokSep:
			flush()
			link += t.text
			skipOperand, hereString = false, false
		case tokRedirect:
			// A redirect and (for a bare operator) its following file operand are NOT
			// command arguments — drop them wherever they appear in the command.
			skipOperand = t.expectsOperand
			hereString = t.text == "<<<" || strings.HasSuffix(t.text, "<<<")
			if redirectsStdout(t.text) {
				cur.outRedirect = true
			}
			if isHeredocOperator(t.text) {
				cur.hereOps++
			}
		case tokWord:
			if skipOperand {
				skipOperand = false
				if hereString {
					cur.herestr = append(cur.herestr, t.text)
				}
				continue
			}
			cur.words = append(cur.words, t.text)
			cur.dynamic = append(cur.dynamic, t.dynamic)
		}
	}
	flush()
	return segs, bodies, lx
}

// parsePushCommands finds EVERY `git push` in a shell command, understanding just enough
// shell structure to (a) not mistake a redirect/pipe/quoted text for a refspec, (b) not
// miss a push hidden after a separator or inside a `( … )` / `{ … }` / `if` / `for`
// body, and (c) say pushUnsure — rather than "no push" — when the command holds a
// construct that may hide a push it does not read. See the file comment.
//
// It strips heredoc bodies (data, not commands), lexes the rest into words / separators /
// redirects with quotes honoured, splits into simple-command segments, and recognises
// `git push`, `git -C <path> push`, `git -c k=v push`, `/path/to/git push`, and a `cd
// <path> && git push` carry (the one cross-`&&` carry: a `cd <path>` segment joined by
// `&&` to a push sets that push's repo path).
func parsePushCommands(cmd string) pushParse { return parsePush(cmd, true) }

// parsePush is parsePushCommands with the independent check optional, so a test can ask the
// tokeniser alone and see a fail-open the check would otherwise cover. Production callers
// go through parsePushCommands.
func parsePush(cmd string, crossCheck bool) pushParse {
	segs, bodies, lx := splitSegments(cmd)
	var unsure []string
	unsure = append(unsure, lx.unsure...)
	assigned := assignedVars(cmd)

	var out pushParse
	var tr cdTracker
	var unknown []string // pushes whose target repo the text does not name
	anyWrapper := false
	printedPush := false // an echo/printf whose arguments mention push
	anyShell := false    // a segment that is itself a shell
	for i, sg := range segs {
		tr.step(sg.link)
		pend := tr.current()
		repoPath, args, isPush, why := recognizeGitPush(sg.words, sg.dynamic)
		if why != "" {
			unsure = append(unsure, why)
			continue
		}
		if isPush {
			loose, after := false, i > 0
			if repoPath == "" && pend.active {
				if pend.why != "" {
					unsure, unknown = append(unsure, pend.why), append(unknown, pend.why)
					continue
				}
				repoPath, loose, after = pend.path, pend.loose, pend.at > 0
			} else if w := pathUnknownWhy(repoPath, assigned); w != "" {
				w = "the repo path of `git -C` " + w
				unsure, unknown = append(unsure, w), append(unknown, w)
				continue
			} else if repoPath != "" && pend.active && isRelativeOperand(repoPath) {
				// `cd V && git -C . push`: a relative -C resolves against the directory the carried cd
				// moved into, not the session's.
				if pend.why != "" || pend.loose {
					w := "a relative `git -C " + repoPath + "` resolves against a directory the command does not name (" + carryUnnamedWhy(pend) + ")"
					unsure, unknown = append(unsure, w), append(unknown, w)
					continue
				}
				repoPath, after = joinDirOperand(pend.path, repoPath), pend.at > 0
			}
			out.Invocations = append(out.Invocations, pushInvocation{repoPath: repoPath, args: args, leading: i == 0 && sg.link == "", afterOthers: after, gitConfig: gitPrefixConfig(sg.words)})
			if loose {
				// The cd and this push are not joined by `&&` alone, so the cd may have failed
				// (`cd T ; push`) and the push then runs where the session stands: judge both.
				out.Invocations = append(out.Invocations, pushInvocation{args: args, gitConfig: gitPrefixConfig(sg.words)})
			}
			continue
		}
		ci := commandStart(sg.words)
		if ci < len(sg.words) {
			name := path.Base(sg.words[ci])
			switch {
			case sg.dynamic[ci] && anyWordIs(sg.words[ci+1:], "push"):
				unsure = append(unsure, "the command word is an expansion ("+sg.words[ci]+") followed by `push`")
			case shellWrappers[name]:
				anyWrapper = true
				anyShell = anyShell || shellInterpreters[name]
				if anyContainsPush(sg.words[ci+1:]) || anyContainsPush(sg.herestr) {
					unsure = append(unsure, "`"+name+"` is given text that mentions `push`")
				}
			}
			// A `git push` that is not the command word is an argument of whatever IS
			// (`time -p git push`, `timeout 5 git push`, `xargs git push`): that command
			// may run it, and the wrapper list above cannot name them all.
			if name == "echo" || name == "printf" {
				// ...except a command that only prints its arguments. `echo git push …` is
				// text (a pinned, long-standing fail-open); the one way it becomes a push is
				// a pipe into a shell, which is refused below.
				if anyContainsPush(sg.words[ci+1:]) {
					printedPush = true
				}
			} else if name != "git" {
				for j := ci + 1; j < len(sg.words); j++ {
					if _, _, isPush, why := recognizeGitPush(sg.words[j:], sg.dynamic[j:]); isPush || why != "" {
						unsure = append(unsure, "a `git push` is an argument of `"+name+"`")
						break
					}
				}
			}
		}
		if name, rest, ok := dirChange(sg.words); ok {
			target, why := dirChangeTarget(name, rest)
			if isNoopCd(name, target, why) {
				continue
			}
			if why == "" {
				if why = pathUnknownWhy(target, assigned); why != "" {
					why = "the directory of the carried `" + name + "` " + why
				}
			}
			target, why = tr.resolveRelative(name, target, why)
			tr.set(cdCarry{path: target, why: why, at: i})
		}
	}

	// An environment variable can point git at another repository exactly as --git-dir can, so the
	// text of the command does not name the repo a push publishes. The variable is read from
	// the raw text (assignedVars), so a prefix, `export`, a separate statement and `env` all count.
	if len(out.Invocations) > 0 || countPushShapes(cmd) > 0 {
		for _, v := range gitTargetEnv {
			if assigned[v] {
				w := "`" + v + "` is assigned on the line, so the repository a `git push` publishes is not the one the command names"
				unsure, unknown = append([]string{w}, unsure...), append(unknown, w)
				break
			}
		}
	}

	if printedPush && anyShell {
		unsure = append(unsure, "text that mentions `push` is printed in a command that also runs a shell (`echo … | sh`)")
	}
	for _, b := range bodies {
		if !b.mentionsPush {
			continue
		}
		switch {
		case !heredocConsumers[b.consumer]:
			unsure = append(unsure, "a heredoc body that mentions `push` is fed to `"+b.consumer+"`, which may run it")
		case anyWrapper && wrapperMayRunBody(segs, len(bodies)):
			unsure = append(unsure, "a heredoc body that mentions `push` sits in a command that also runs a wrapper (`… | sh`)")
		case !b.quoted && (strings.Contains(b.body, "$(") || strings.Contains(b.body, "`")):
			unsure = append(unsure, "an unquoted heredoc body expands a command substitution and mentions `push`")
		}
	}

	// THE INVARIANT, independent of everything above: the text of the command is checked
	// against the structured reading. Every way this parser has lost a push (a quote opened
	// by a comment, a heredoc read where bash reads none, a `#` after a `)` it misclassified)
	// shares one shape: the raw text plainly holds `git … push` and the reading found fewer
	// pushes than the text holds. So that disagreement is itself the signal, whatever caused
	// it. It trusts neither the lexer nor the stripper, which is the point: their next bug
	// cannot reopen the hole, only turn into a refusal.
	if crossCheck && len(unsure) == 0 {
		if countPushShapes(cmd) > len(out.Invocations) {
			unsure = append(unsure, unreadInvariantWhy)
		}
	}
	if len(unsure) > 0 {
		res := pushParse{State: pushUnsure, Why: unsure[0], Repos: repoHints(cmd)}
		if len(unknown) > 0 {
			res.Unknown = unknown[0]
		}
		for _, h := range res.Repos {
			// An unreadable command that moves into a directory the text does not name.
			if w := pathUnknownWhy(h, assigned); w != "" && res.Unknown == "" {
				res.Unknown = "the command moves into `" + h + "`, which " + w
			} else if h == "-" && res.Unknown == "" {
				res.Unknown = "the command moves into `-`, which is $OLDPWD"
			}
		}
		return res
	}
	if len(out.Invocations) > 0 {
		out.State = pushFound
		return out
	}
	return pushParse{State: pushNone}
}

// anyWordIs reports whether any word equals w exactly.
func anyWordIs(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// anyContainsPush reports whether any word mentions push.
func anyContainsPush(words []string) bool {
	for _, x := range words {
		if containsPush(x) {
			return true
		}
	}
	return false
}

// commandStart returns the index of a segment's command word: past any leading
// `VAR=value` assignments and the shell keywords that open a command position.
func commandStart(words []string) int {
	i := 0
	for i < len(words) && (looksLikeAssignmentWord(words[i]) || shellKeywords[words[i]]) {
		i++
	}
	return i
}

// gitPrefixConfig returns the git configuration a segment sets for its own git command, as
// `key=value` strings, in the order git applies them (the last of a key wins): `-c k=v`,
// `--config-env=k=ENVVAR` (the value is in an environment the hook cannot read, so it is `?`),
// and the `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n` assignments in front of the
// command. `GIT_CONFIG_PARAMETERS`, git's internal channel, is reported as the key itself with the
// value `?`. These override the config files, so a bare push is only the current branch if none
// of them says otherwise (see unnamedRefSetConfig).
func gitPrefixConfig(fields []string) []string {
	var out []string
	ci := commandStart(fields)
	env := map[string]string{}
	for _, w := range fields[:min(ci, len(fields))] {
		if k, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(k, "GIT_CONFIG_") {
			env[k] = v
		}
	}
	if _, ok := env["GIT_CONFIG_PARAMETERS"]; ok {
		out = append(out, "GIT_CONFIG_PARAMETERS=?")
	}
	if n, err := strconv.Atoi(env["GIT_CONFIG_COUNT"]); err == nil {
		for j := 0; j < n; j++ {
			if k := env["GIT_CONFIG_KEY_"+strconv.Itoa(j)]; k != "" {
				out = append(out, k+"="+env["GIT_CONFIG_VALUE_"+strconv.Itoa(j)])
			}
		}
	} else if _, ok := env["GIT_CONFIG_COUNT"]; ok {
		out = append(out, "GIT_CONFIG_COUNT=?") // a count that is not a number: unreadable
	}
	for j := ci + 1; j < len(fields); j++ {
		w := fields[j]
		switch {
		case w == "push":
			return out
		case w == "-c" && j+1 < len(fields):
			out = append(out, fields[j+1])
			j++
		case strings.HasPrefix(w, "--config-env="):
			if k, _, ok := strings.Cut(strings.TrimPrefix(w, "--config-env="), "="); ok {
				out = append(out, k+"=?")
			}
		case w == "-C" && j+1 < len(fields):
			j++
		}
	}
	return out
}

// recognizeGitPush matches the git-push forms on ONE segment's cleaned words: `git push`,
// `git -C <path> push`, `git -c k=v push`, and a path-spelled git (`/usr/bin/git push`),
// after any leading assignments and shell keywords. Returns the repo path ("" = none /
// process cwd) and every token after "push". ok=false means this segment is not a git
// push. unsure is non-empty when it IS a push but of a repo the words do not name
// (`--git-dir`, `--work-tree`), so it cannot be judged.
func recognizeGitPush(fields []string, dynamic []bool) (repoPath string, pushArgs []string, ok bool, unsure string) {
	// Skip leading `VAR=value` env assignments (`REVIEW_GATE_BYPASS=x git push`, `FOO=bar
	// git push …`): they set env for the command, they are not the command word. Without
	// this, an env prefix made the segment unrecognizable and BOTH push guards failed OPEN
	// — a trivial bypass. Stripping them here means the push underneath is still judged
	// (and push-gate can then honor an inline REVIEW_GATE_BYPASS as a deliberate bypass).
	i := commandStart(fields)
	if i >= len(fields) || path.Base(fields[i]) != "git" {
		return "", nil, false, ""
	}
	i++
	redirected, expanded := false, false
scan:
	for i < len(fields) {
		w := fields[i]
		switch {
		case w == "-C" && i+1 < len(fields):
			repoPath = joinDirOperand(repoPath, fields[i+1])
			i += 2
		case w == "-c" && i+1 < len(fields):
			i += 2
		case isRepoRedirectOption(w):
			redirected = true
			if strings.Contains(w, "=") {
				i++
			} else {
				i += 2
			}
		case strings.HasPrefix(w, "-"):
			i++ // a global flag that names no repo (--no-pager, -p, --bare, …)
		case dynamic[i]:
			expanded = true // `git $OPT push`: the expansion may be any global option
			i++
		default:
			break scan
		}
	}
	if i >= len(fields) || fields[i] != "push" {
		return "", nil, false, ""
	}
	if redirected {
		return "", nil, false, "`git --git-dir/--work-tree … push` pushes a repo the command does not name by path"
	}
	if expanded {
		return "", nil, false, "an expansion between `git` and `push` may be a global option such as --git-dir"
	}
	if isComputedPath(repoPath) {
		return "", nil, false, "the repo path of `git -C` is computed by a command substitution"
	}
	i++
	return repoPath, fields[i:], true, ""
}

// cdOptionChars are the options bash's cd takes (-L -P -e -@), alone or bundled as `-LP`.
const cdOptionChars = "LPe@"

// cdTarget reads the directory a `cd` segment moves into, from its words (words[0] is
// `cd`). It does what bash's cd does with its arguments: the options (cdOptionChars) are not
// the directory, `--` ends them, and the first word after is the operand. No operand means HOME,
// which is returned as `~` for resolveRepoPath to expand. why is non-empty when the directory
// cannot be known from the text, so the push that follows it cannot be pinned to a repo:
//
//   - a computed operand (`$( … )`, backticks);
//   - `-`, which means $OLDPWD, a value the command line does not hold;
//   - an option cd does not have, or more than one operand, where bash refuses the cd and
//     zsh does something else: which shell runs it decides, so it is not read here.
//
// Reading the word after `cd` as the path took `--` and `-P` for the directory, which names
// no repo, and an unnamed repo was not judged by anything.
func cdTarget(words []string) (target, why string) {
	i := 1
	for i < len(words) {
		w := words[i]
		if w == "--" {
			i++
			break
		}
		if len(w) < 2 || w[0] != '-' {
			break
		}
		if strings.Trim(w[1:], cdOptionChars) != "" {
			return "", "the carried `cd` has an option (`" + w + "`) the guard does not read"
		}
		i++
	}
	switch rest := words[i:]; {
	case len(rest) == 0:
		return "~", ""
	case len(rest) > 1:
		return "", "the carried `cd` has more than one directory operand"
	case rest[0] == "-":
		return "", "the carried `cd -` goes to $OLDPWD, which the command does not name"
	case isComputedPath(rest[0]):
		return "", "the repo path of the carried `cd` is computed by a command substitution"
	default:
		return rest[0], ""
	}
}

// dirChange reports whether a segment moves the shell to another directory, and by which
// command: `cd`, `pushd` or `popd`, however spelled (`builtin cd`, `command cd`, behind
// `VAR=value` assignments). Only these are read; a function, alias or sourced script that
// changes directory is not (see the file comment's limits).
func dirChange(words []string) (name string, args []string, ok bool) {
	i := commandStart(words)
	for i < len(words) && (words[i] == "builtin" || words[i] == "command") {
		i++
		for i < len(words) && strings.HasPrefix(words[i], "-") {
			i++
		}
	}
	if i >= len(words) {
		return "", nil, false
	}
	switch words[i] {
	case "cd", "pushd", "popd":
		return words[i], words[i+1:], true
	}
	return "", nil, false
}

// isNoopCd reports a `cd` whose operand is the empty string, however quoted. Bash and zsh treat it
// as a success that changes nothing, so the directory in force is the one before it. That is a
// determinate meaning, not an unknown: the carry is left as it was, which keeps `cd V && cd <empty>
// && cmd` judged against V (and costs nothing where a shell instead refuses the cd, since `&&` then
// stops the line). Reading the empty operand as a target instead resolved to the session's own
// directory. (The empty operand is not spelled out here: gofmt turns a doubled single quote in a
// doc comment into a typographic pair.)
func isNoopCd(name, target, why string) bool { return name == "cd" && why == "" && target == "" }

// dirChangeTarget is the directory a dirChange command moves into, or why that cannot be named.
func dirChangeTarget(name string, args []string) (target, why string) {
	switch name {
	case "cd":
		return cdTarget(append([]string{"cd"}, args...))
	case "pushd":
		if len(args) == 1 && args[0] != "" && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[0], "+") {
			if isComputedPath(args[0]) {
				return "", "the repo path of the carried `pushd` is computed by a command substitution"
			}
			return args[0], ""
		}
		return "", "the carried `pushd` does not name exactly one directory"
	}
	return "", "the carried `" + name + "` returns to a directory the command does not name"
}

// isRelativeOperand reports whether a directory operand resolves against the directory the shell
// is in, rather than naming one: it is not absolute and does not start with `~` (which is HOME or
// another user's, handled by pathUnknownWhy and resolveRepoPath).
func isRelativeOperand(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "~")
}

// joinDirOperand is the directory op names when the shell is already in base: op alone when it is
// absolute (or base is not known), else base/op cleaned the way the shell's logical `cd` and git's
// `-C` do. An empty op leaves base, which is what both do with an empty operand.
func joinDirOperand(base, op string) string {
	switch {
	case op == "":
		return base
	case base == "" || !isRelativeOperand(op):
		return op
	}
	return path.Join(base, op)
}

// resolveRelative applies the change in force to a directory change that names a RELATIVE operand,
// where the shell moves from the directory already in force and not from the session's. A named
// change in force is joined onto; one that is not named (or is only a candidate, being joined by
// something other than `&&`) makes the new directory unknown, as every other unnameable target is.
// An operand that is absolute, computed or already unknown (why != "") is returned as it was. Both
// readers of a line's directory changes (the push hooks and the raw PR verb) use it, so they cannot
// resolve the same `cd` two ways.
func (t *cdTracker) resolveRelative(name, target, why string) (string, string) {
	if why != "" || !isRelativeOperand(target) {
		return target, why
	}
	// No change in force is the zero carry: it has no path, so the join below leaves the operand as it is.
	cur := t.current()
	if cur.why != "" || cur.loose {
		return target, "the relative `" + name + " " + target + "` moves from a directory the command does not name (" + carryUnnamedWhy(cur) + ")"
	}
	return joinDirOperand(cur.path, target), ""
}

// carryUnnamedWhy says why a directory change in force cannot be used as the base of a relative one.
func carryUnnamedWhy(c cdCarry) string {
	if c.why != "" {
		return c.why
	}
	return "it is joined to the command by something other than `&&`, so it may not have happened"
}

// cdCarry is a directory change in force at a point of the line: where the next command runs.
type cdCarry struct {
	active bool
	// loose is true once something other than `&&` joins the change to the command: it then runs in
	// the new directory only if the change succeeded, and where the session stands if it did not,
	// so both are candidates.
	loose bool
	path  string
	why   string // non-empty: the directory cannot be named
	at    int    // index of the segment that made the change; > 0 means a command precedes it
	depth int    // parenthesis depth the change ran at
}

// cdTracker follows the directory changes across a line, with the parenthesis depth each ran at.
// A `( ... )` group is a subshell: a change made inside it ends at the `)` that closes THAT group,
// and the change in force outside it is back in force. A `)` that closes an unrelated group (the
// change was made outside it: `cd V && (true) && cmd`) ends nothing. Treating every `)` as the end
// of the carry let such a group hide the change, and the command was then judged by the session.
type cdTracker struct {
	depth int
	stack []cdCarry // outermost first
}

// step moves the tracker across the link (the separators) into the next segment. Parentheses open
// and close groups; any separator other than `&&` loosens every change in force.
func (t *cdTracker) step(link string) {
	for _, r := range link {
		switch r {
		case '(':
			t.depth++
		case ')':
			t.depth--
			for len(t.stack) > 0 && t.stack[len(t.stack)-1].depth > t.depth {
				t.stack = t.stack[:len(t.stack)-1]
			}
		}
	}
	if sep := strings.NewReplacer("(", "", ")", "").Replace(link); sep != "" && sep != "&&" {
		for i := range t.stack {
			t.stack[i].loose = true
		}
	}
}

// set records a change made at the current depth; it replaces one made at the same depth (`cd a &&
// cd b` is b) and sits over one made outside.
func (t *cdTracker) set(c cdCarry) {
	for len(t.stack) > 0 && t.stack[len(t.stack)-1].depth >= t.depth {
		t.stack = t.stack[:len(t.stack)-1]
	}
	c.active, c.depth = true, t.depth
	t.stack = append(t.stack, c)
}

// current is the change in force: the innermost, or none.
func (t *cdTracker) current() cdCarry {
	if len(t.stack) == 0 {
		return cdCarry{}
	}
	return t.stack[len(t.stack)-1]
}

var (
	assignRe = regexp.MustCompile(`(?:^|[\s;&|('"\x60])(?:(?:export|declare|typeset|local|readonly)\s+(?:-\S+\s+)*)?([A-Za-z_][A-Za-z0-9_]*)\+?=`)
	readRe   = regexp.MustCompile(`(?:^|[\s;&|('"\x60])(?:read|for|select)\s+(?:-\S+\s+)*([A-Za-z_][A-Za-z0-9_]*)`)
	printfRe = regexp.MustCompile(`printf\s+-v\s+([A-Za-z_][A-Za-z0-9_]*)`)
	varRefRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
)

// assignedVars names the variables the command line sets itself, read from the raw text so it
// trusts no lexer: `R=T; cd $R`. The hook cannot see such a variable (it holds the environment the
// session started the command in, not the one the command builds), and one it does hold, such as
// HOME or PWD, would expand to the wrong thing. The text over-reads (an assignment inside quotes
// counts), which can only turn a path into an unknown one.
func assignedVars(cmd string) map[string]bool {
	out := map[string]bool{}
	for _, re := range []*regexp.Regexp{assignRe, readRe, printfRe} {
		for _, m := range re.FindAllStringSubmatch(cmd, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// pathUnknownWhy says why a repo path in the text does not name the directory the shell will use,
// "" when it does (or is a plain path that simply may not exist). The shell rewrites a word
// before cd or git sees it, and the hook sees only the text:
//
//   - a command substitution, and `$'…'` / `$"…"` quoting;
//   - a glob or brace pattern (`T*`, `{T,x}`), which the shell expands to a real directory.
//     The lexer drops quoting, so a quoted `"a*b"` is refused as well: accepted over-refusal;
//   - `~user`, `~+`, `~-`, which are not HOME;
//   - a variable the line itself assigns (see assignedVars), including HOME for a bare `~`.
//
// A variable the line does not assign is expanded from the hook's environment by
// resolveRepoPath, which reports one it does not hold.
func pathUnknownWhy(p string, assigned map[string]bool) string {
	switch {
	case p == "":
		return ""
	case isComputedPath(p):
		return "is computed by a command substitution"
	case strings.Contains(p, "$'") || strings.Contains(p, `$"`):
		return "is written in $'…' or $\"…\" quoting, which the shell rewrites"
	case strings.ContainsAny(p, "*?[{"):
		return "is a glob or brace pattern, which the shell expands"
	case p[0] == '~' && len(p) > 1 && p[1] != '/':
		return "starts with ~user, ~+ or ~-, which is not HOME"
	case p[0] == '~' && assigned["HOME"]:
		return "starts with ~ and the line assigns HOME"
	}
	for _, m := range varRefRe.FindAllStringSubmatch(p, -1) {
		if assigned[m[1]] {
			return "uses $" + m[1] + ", which the line assigns"
		}
	}
	return ""
}

// isComputedPath reports whether a repo path is built by a command substitution. The hooks
// cannot evaluate it, an unresolvable path reads as "no such repo" and the gate allows that,
// so a push to it would be judged by nothing.
func isComputedPath(p string) bool {
	return strings.Contains(p, "$(") || strings.Contains(p, "`")
}

// gitTargetEnv are the environment variables that point git at a repository (or a namespace within
// one) other than the one the command's working directory and `-C` name: the environment form of
// the options isRepoRedirectOption refuses.
var gitTargetEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_NAMESPACE"}

// isRepoRedirectOption reports whether w is a git global option that points git at a
// different repository (so `-C` does not say which repo a push targets).
func isRepoRedirectOption(w string) bool {
	for _, o := range []string{"--git-dir", "--work-tree", "--namespace", "--super-prefix"} {
		if w == o || strings.HasPrefix(w, o+"=") {
			return true
		}
	}
	return false
}

var (
	cdHint = regexp.MustCompile(`(?:^|[\s;&|(\x60'"])(?:cd|pushd)\s+(?:-[` + cdOptionChars + `]+\s+)*(?:--\s+)?("[^"]*"|'[^']*'|[^\s;&|)'"]+)`)
	cHint  = regexp.MustCompile(`(?:^|\s)-C\s+("[^"]*"|'[^']*'|[^\s;&|)'"]+)`)
)

// repoHints extracts the `cd <path>` / `-C <path>` targets mentioned anywhere in cmd,
// INCLUDING inside quotes and substitutions the parser does not read, so an unreadable
// command that moves into another repo is still judged against that repo's gate.
func repoHints(cmd string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{cdHint, cHint} {
		for _, m := range re.FindAllStringSubmatch(cmd, -1) {
			p := strings.Trim(m[1], `"'`)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// hereBody is a heredoc body taken out of the command text.
type hereBody struct {
	consumer     string // the command the heredoc feeds ("" when it cannot be named)
	body         string
	quoted       bool // delimiter was quoted: the body is literal, nothing in it expands
	mentionsPush bool // the body names push; the push hooks read only those, other readers read every body
}

// commentStarts reports whether the `#` at s[i] opens a comment. This is the ONE definition
// of where a comment starts: stripHeredocs, lexShellFull and skipParen all call it, because
// a comment is text none of them may read as quotes, heredocs or commands, and any
// disagreement between them lets an apostrophe or a `<<` in a comment swallow the lines
// after it. Bash's rule: a `#` that begins a word, which is after a blank (space or tab,
// not CR), a newline, or one of `; & | ( ) < >`, unless that character is backslash-escaped.
// A backslash-newline continuation is skipped over, since it joins the lines.
func commentStarts(s string, i int) bool {
	if i >= len(s) || s[i] != '#' {
		return false
	}
	j := i
	for j >= 2 && s[j-1] == '\n' && s[j-2] == '\\' && !escapedAt(s, j-2) {
		j -= 2
	}
	if j == 0 {
		return true
	}
	return strings.IndexByte(" \t\n;&|()<>", s[j-1]) >= 0 && !escapedAt(s, j-1)
}

// escapedAt reports whether the byte at s[k] is escaped by an odd run of backslashes.
func escapedAt(s string, k int) bool {
	n := 0
	for j := k - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// commentEnd returns the index of the newline that ends the comment starting at s[i], or
// len(s).
func commentEnd(s string, i int) int {
	if e := strings.IndexByte(s[i:], '\n'); e >= 0 {
		return i + e
	}
	return len(s)
}

// stripHeredocs removes heredoc bodies (and their terminator lines) from cmd. A body is
// DATA — the prose of a commit message or a PR description — and parsing its lines as
// commands both blocks commands that merely document a push and, worse, made every
// `$(cat <<'EOF' … push … EOF)` look like a substitution that hides one. It returns the
// remaining text and, for each removed body that mentions push, who consumes it, so the
// caller can still refuse a body that is fed to a shell.
//
// Every doubt resolves to leaving text in place, where it is parsed as ordinary commands:
//   - `<<` inside quotes, inside `$(( … ))`, or in a comment is not a heredoc;
//   - `<<<` is a here-string: a delimiter cannot begin with `<`, so it is never taken for a
//     heredoc;
//   - a delimiter that is not one plain word (`<<EO"F"`, `<<'EO'F`, `<<EOF:`) is not
//     understood, so the operator is left alone: reading it as the prefix `EO` would end the
//     body where bash's body continues, or the reverse, and hide a push either way;
//   - a heredoc with no terminator line is left in place;
//   - the terminator matches the line trimmed of surrounding blanks, which accepts every
//     line bash accepts, so the body can end EARLIER than bash's, never later.
func stripHeredocs(s string) (string, []hereBody) {
	type pending struct {
		delim    string
		quoted   bool
		consumer string
	}
	var out strings.Builder
	var bodies []hereBody
	var queue []pending
	// modes: 'c' code (incl. $( … ) and ( … )), 'd' inside double quotes, 'a' inside
	// arithmetic `$(( … ))` / `(( … ))`, where `<<` is a shift and never a heredoc. Single
	// quotes and backticks are skipped whole.
	stack := []byte{'c'}
	n := len(s)
	lineStart := 0
	for i := 0; i < n; {
		c := s[i]
		top := stack[len(stack)-1]
		switch {
		case c == '\n':
			out.WriteByte(c)
			i++
			lineStart = i
			if len(queue) > 0 {
				j := i
				ok := true
				var taken []hereBody
				for _, q := range queue {
					start := j
					found := false
					for j < n {
						ls := j
						e := strings.IndexByte(s[j:], '\n')
						line := s[j:]
						j = n
						if e >= 0 {
							line, j = s[ls:ls+e], ls+e+1
						}
						if strings.TrimSpace(line) == q.delim {
							found = true
							body := s[start:ls]
							taken = append(taken, hereBody{consumer: q.consumer, body: body, quoted: q.quoted, mentionsPush: containsPush(body)})
							break
						}
					}
					if !found {
						ok = false
						break
					}
				}
				queue = nil
				if ok {
					bodies = append(bodies, taken...)
					i = j
					lineStart = i
				}
			}
		case c == '\\' && i+1 < n:
			out.WriteString(s[i : i+2])
			i += 2
		case top == 'c' && c == '\'':
			e := strings.IndexByte(s[i+1:], '\'')
			if e < 0 {
				out.WriteString(s[i:])
				i = n
				break
			}
			out.WriteString(s[i : i+e+2])
			i += e + 2
		case c == '`':
			e := skipBacktick(s, i+1)
			out.WriteString(s[i:e])
			i = e
		case c == '"':
			if top == 'd' {
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, 'd')
			}
			out.WriteByte(c)
			i++
		case c == '$' && i+2 < n && s[i+1] == '(' && s[i+2] == '(' && top != 'a':
			stack = append(stack, 'a', 'a')
			out.WriteString("$((")
			i += 3
		case c == '(' && i+1 < n && s[i+1] == '(' && top == 'c':
			stack = append(stack, 'a', 'a')
			out.WriteString("((")
			i += 2
		case top == 'a' && c == '(':
			stack = append(stack, 'a')
			out.WriteByte(c)
			i++
		case c == '$' && i+1 < n && s[i+1] == '(':
			stack = append(stack, 'c')
			out.WriteString("$(")
			i += 2
		case top == 'c' && c == '(':
			stack = append(stack, 'c')
			out.WriteByte(c)
			i++
		case (top == 'c' || top == 'a') && c == ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			out.WriteByte(c)
			i++
		case top == 'c' && commentStarts(s, i):
			// A comment runs to the end of the line; a `<<` inside it is text.
			e := commentEnd(s, i)
			out.WriteString(s[i:e])
			i = e
		case top == 'c' && c == '<' && i+1 < n && s[i+1] == '<':
			delim, quoted, end, ok := heredocDelim(s, i+2)
			if !ok {
				out.WriteString("<<")
				i += 2
				break
			}
			queue = append(queue, pending{delim: delim, quoted: quoted, consumer: heredocConsumer(s[lineStart:i])})
			out.WriteString(s[i:end])
			i = end
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), bodies
}

// heredocDelim reads the delimiter after `<<` at s[i:]: an optional `-`, optional blanks,
// then a bare word or a quoted/escaped one. quoted reports a delimiter quoted in any way
// (the body is then literal). end is the index just past the delimiter.
func heredocDelim(s string, i int) (delim string, quoted bool, end int, ok bool) {
	n := len(s)
	if i < n && s[i] == '-' {
		i++
	}
	for i < n && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= n {
		return "", false, i, false
	}
	switch s[i] {
	case '\'', '"':
		q := s[i]
		e := strings.IndexByte(s[i+1:], q)
		if e <= 0 {
			return "", false, i, false
		}
		end = i + e + 2
		if !endsWord(s, end) {
			return "", false, end, false
		}
		return s[i+1 : i+1+e], true, end, true
	case '\\':
		i++
		quoted = true
	}
	start := i
	for i < n && (s[i] == '_' || s[i] == '.' || s[i] == '-' || (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= '0' && s[i] <= '9' && i > start)) {
		i++
	}
	if i == start || !endsWord(s, i) {
		return "", false, i, false
	}
	return s[start:i], quoted, i, true
}

// endsWord reports whether s[i:] begins at the end of a shell word: the end of the text,
// a blank, a newline, or a metacharacter. A heredoc delimiter followed by anything else is
// only part of a longer, partly quoted or escaped word.
func endsWord(s string, i int) bool {
	return i >= len(s) || strings.IndexByte(" \t\r\n;&|<>()", s[i]) >= 0
}

// heredocConsumer names the command a heredoc on this line feeds: the first command word
// of the last simple command before the `<<`. "" when none is found (`<<EOF cat`).
func heredocConsumer(before string) string {
	if k := strings.LastIndexAny(before, "|&;(`\n"); k >= 0 {
		before = before[k+1:]
	}
	words := strings.Fields(before)
	i := commandStart(words)
	if i >= len(words) {
		return ""
	}
	return path.Base(strings.Trim(words[i], `"'`))
}

// skipBacktick returns the index just past the backtick that closes the section opened
// just before s[i], or len(s) when it never closes.
func skipBacktick(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '`':
			return i + 1
		}
		i++
	}
	return len(s)
}

// skipParen returns the index just past the `)` that closes a `$(` / `<(` whose body
// starts at s[i], honouring nested parens and quotes. ok=false: it never closes.
func skipParen(s string, i int) (end int, ok bool) {
	depth := 1
	for i < len(s) {
		if commentStarts(s, i) {
			i = commentEnd(s, i)
			continue
		}
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			e := strings.IndexByte(s[i+1:], '\'')
			if e < 0 {
				return len(s), false
			}
			i += e + 2
			continue
		case '"':
			e, closed := skipDouble(s, i+1)
			if !closed {
				return len(s), false
			}
			i = e
			continue
		case '`':
			i = skipBacktick(s, i+1)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return len(s), false
}

// skipDouble returns the index just past the `"` that closes a double-quoted string whose
// body starts at s[i]; a `$( … )` inside it is skipped whole. ok=false: never closes.
func skipDouble(s string, i int) (end int, ok bool) {
	for i < len(s) {
		switch {
		case s[i] == '\\':
			i += 2
			continue
		case s[i] == '"':
			return i + 1, true
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
			e, closed := skipParen(s, i+2)
			if !closed {
				return len(s), false
			}
			i = e
			continue
		case s[i] == '`':
			i = skipBacktick(s, i+1)
			continue
		}
		i++
	}
	return len(s), false
}

// shellTokKind classifies a lexed shell token.
type shellTokKind int

const (
	tokWord     shellTokKind = iota // a command word (may be a git-push arg)
	tokSep                          // a command separator: | || && ; & ( ) or newline
	tokRedirect                     // a redirect operator (with its fd/target)
)

// shellToken is one lexed token. For a redirect, expectsOperand is true when the
// operator is a bare form (`>`, `>>`, `2>`, `&>`, …) whose target file is the NEXT
// word (which must then also be dropped); a self-contained fd-dup (`2>&1`, `>&2`)
// carries its target inline and expectsOperand is false. dynamic marks a word holding an
// expansion (`$VAR`, `$( … )`, backticks, `$'…'`): the text here is not what will run.
type shellToken struct {
	kind           shellTokKind
	text           string
	expectsOperand bool
	dynamic        bool
}

// shellLex is a lexed command plus what the lexer found it cannot vouch for.
type shellLex struct {
	toks   []shellToken
	unsure []string
}

// lexShell is lexShellFull's tokens, for callers that only need the words.
func lexShell(s string) []shellToken { return lexShellFull(s).toks }

// lexShellFull tokenises a shell command just far enough for the push hooks: it separates
// command words (quotes and escapes resolved, so a quoted `;` stays inside its word) from
// the separators (`|`, `||`, `&&`, `;`, `&`, `(`, `)`, newline) that end a simple command
// and from the redirect operators that are never command arguments. Git remotes/refspecs
// contain none of the shell metacharacters, so any bare token bearing one is structural.
// A command substitution, backtick section or process substitution that mentions push,
// or a quote that never closes over a push, is recorded in unsure: the lexer does not
// look inside them, it says it did not.
func lexShellFull(s string) shellLex {
	var lx shellLex
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == '\n':
			lx.toks = append(lx.toks, shellToken{kind: tokSep, text: "\n"})
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\\' && i+1 < n && s[i+1] == '\n':
			i += 2 // a line continuation joins lines; it is not a separator
		case commentStarts(s, i):
			i = commentEnd(s, i) // a comment is not text: its quotes open nothing
		case c == '(' || c == ')':
			lx.toks = append(lx.toks, shellToken{kind: tokSep, text: string(c)})
			i++
		case c == ';':
			j := i + 1
			for j < n && s[j] == ';' {
				j++
			}
			lx.toks = append(lx.toks, shellToken{kind: tokSep, text: s[i:j]})
			i = j
		case c == '|':
			if i+1 < n && s[i+1] == '|' {
				lx.toks = append(lx.toks, shellToken{kind: tokSep, text: "||"})
				i += 2
			} else {
				lx.toks = append(lx.toks, shellToken{kind: tokSep, text: "|"})
				i++
			}
		case c == '&':
			switch {
			case i+1 < n && s[i+1] == '&':
				lx.toks = append(lx.toks, shellToken{kind: tokSep, text: "&&"})
				i += 2
			case i+1 < n && s[i+1] == '>': // &> / &>> redirect (both streams to a file)
				tok, ni := lexRedirect(s, i, "")
				lx.toks = append(lx.toks, tok)
				i = ni
			default: // background separator
				lx.toks = append(lx.toks, shellToken{kind: tokSep, text: "&"})
				i++
			}
		case (c == '<' || c == '>') && i+1 < n && s[i+1] == '(':
			// Process substitution `<( … )` / `>( … )`: a word standing for a command.
			e, _ := skipParen(s, i+2)
			lx.region(s[i:e])
			lx.toks = append(lx.toks, shellToken{kind: tokWord, text: s[i:e], dynamic: true})
			i = e
		case c == '<' || c == '>':
			tok, ni := lexRedirect(s, i, "")
			lx.toks = append(lx.toks, tok)
			i = ni
		default:
			word, dynamic, ni := lx.readWord(s, i)
			i = ni
			// A leading fd number binds to a following redirect: `2>&1`, `2>file`.
			if i < n && (s[i] == '<' || s[i] == '>') && allDigits(word) {
				tok, ni := lexRedirect(s, i, word)
				lx.toks = append(lx.toks, tok)
				i = ni
				continue
			}
			lx.toks = append(lx.toks, shellToken{kind: tokWord, text: word, dynamic: dynamic})
		}
	}
	return lx
}

// region records an unreadable section (a substitution or backtick body) that mentions
// push, whether or not it closed.
func (lx *shellLex) region(raw string) {
	if containsPush(raw) {
		lx.unsure = append(lx.unsure, "a command substitution, backtick section or process substitution contains `push`")
	}
}

// unterminated records a quote that never closes over text mentioning push.
func (lx *shellLex) unterminated(rest string) {
	if containsPush(rest) {
		lx.unsure = append(lx.unsure, "a quote that never closes hides text containing `push`")
	}
}

// isExpansionStart reports whether c, after a `$`, begins a parameter expansion.
func isExpansionStart(c byte) bool {
	return c == '{' || c == '_' || c == '@' || c == '*' || c == '#' || c == '?' || c == '!' || c == '$' || c == '-' ||
		(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// readWord reads one shell word starting at s[i] with quotes and escapes resolved:
// single quotes are literal, double quotes honour `\` escapes, adjacent pieces
// concatenate (`pu""sh` is `push`), and a backslash-newline joins. A `$( … )`, backtick
// section, `$'…'` or `$VAR` marks the word dynamic; substitution bodies are skipped whole
// (and recorded when they mention push).
func (lx *shellLex) readWord(s string, i int) (val string, dynamic bool, end int) {
	n := len(s)
	var b strings.Builder
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '|' || c == '&' || c == ';' || c == '<' || c == '>' || c == '(' || c == ')':
			return b.String(), dynamic, i
		case c == '\\':
			switch {
			case i+1 >= n:
				b.WriteByte(c)
				i++
			case s[i+1] == '\n':
				i += 2
			default:
				b.WriteByte(s[i+1])
				i += 2
			}
		case c == '\'':
			e := strings.IndexByte(s[i+1:], '\'')
			if e < 0 {
				lx.unterminated(s[i:])
				b.WriteString(s[i+1:])
				return b.String(), dynamic, n
			}
			b.WriteString(s[i+1 : i+1+e])
			i += e + 2
		case c == '"':
			i = lx.readDouble(s, i+1, &b, &dynamic)
		case c == '`':
			e := skipBacktick(s, i+1)
			lx.region(s[i:e])
			b.WriteString(s[i:e])
			dynamic = true
			i = e
		case c == '$':
			i = lx.readDollar(s, i, &b, &dynamic)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), dynamic, i
}

// readDollar handles a `$` at s[i] in word or double-quote context and returns the index
// to continue from.
func (lx *shellLex) readDollar(s string, i int, b *strings.Builder, dynamic *bool) int {
	n := len(s)
	switch {
	case i+1 < n && s[i+1] == '(':
		e, _ := skipParen(s, i+2)
		lx.region(s[i:e])
		b.WriteString(s[i:e])
		*dynamic = true
		return e
	case i+1 < n && s[i+1] == '\'': // $'…': ANSI-C escapes, so the text is not literal
		j := i + 2
		for j < n && s[j] != '\'' {
			if s[j] == '\\' {
				j++
			}
			j++
		}
		if j >= n {
			lx.unterminated(s[i:])
			b.WriteString(s[i:])
			*dynamic = true
			return n
		}
		b.WriteString(s[i : j+1])
		*dynamic = true
		return j + 1
	case i+1 < n && s[i+1] == '"': // $"…": a locale string; read it as a double-quoted one
		return lx.readDouble(s, i+2, b, dynamic)
	case i+1 < n && isExpansionStart(s[i+1]):
		*dynamic = true
	}
	b.WriteByte('$')
	return i + 1
}

// readDouble reads the body of a double-quoted string starting at s[i] (just past the
// opening quote) into b and returns the index just past the closing quote.
func (lx *shellLex) readDouble(s string, i int, b *strings.Builder, dynamic *bool) int {
	n := len(s)
	start := i - 1
	for i < n {
		c := s[i]
		switch {
		case c == '"':
			return i + 1
		case c == '\\' && i+1 < n:
			switch s[i+1] {
			case '\n':
			case '"', '\\', '$', '`':
				b.WriteByte(s[i+1])
			default:
				b.WriteByte(c)
				b.WriteByte(s[i+1])
			}
			i += 2
		case c == '`':
			e := skipBacktick(s, i+1)
			lx.region(s[i:e])
			b.WriteString(s[i:e])
			*dynamic = true
			i = e
		case c == '$':
			i = lx.readDollar(s, i, b, dynamic)
		default:
			b.WriteByte(c)
			i++
		}
	}
	lx.unterminated(s[max(start, 0):])
	return n
}
