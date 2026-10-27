package hook

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Creating, copying, renaming, deleting, archiving or changing the visibility of a repository is an
// outward-facing act on the owner's account, so it is the owner's: this refuses it for everyone, with no flag
// and no authority entry that lifts it (a refusal an agent can lift by editing its own grant is no refusal).
// Delete is included because an agent that can delete can destroy what review would have kept, and an owner
// who wants it easy can run it by hand. It rides pr-guard, which already reads every raw forge command.
//
// A command is read as the shell reads it, word by word (splitSegments, the lexer the git hooks share): the
// CLI is a command word whatever its spelling, the noun and verb are the words after any global options once
// quotes are removed, and a group, pipeline, `if` or env prefix is just where a segment sits. A word that is
// itself a script (`bash -c`, `eval`, a substitution, a here-string, a heredoc body or text piped into a shell)
// is read the same way to a bounded depth; the text match is the fallback and refuses on the mention. The
// program text an interpreter is handed inline (`python3 -c`, `node -e`, `ruby -e`, `perl -e`, a here-string or
// heredoc to one) is cut into words and read for the CLI followed by a repository verb, so a program that names
// the call is refused on the mention.
//
// THIS DOES NOT CLOSE THE CLASS. It reads the text of one command line, so it stops an agent typing the verb.
// It cannot stop a PROGRAM that makes the call, and the incident that raised this ticket was a program: the
// call is not in the line, and no reading of the line can put it there. The bound that does not depend on
// reading is the token's scope (scopedTokenNote), which is the owner's decision.
//
// What is not read is the table forgeStatedLimits, one row per limit with the commands that pin it as ALLOWED;
// this list is generated from it and a test fails when they differ. Add a limit there, not here.
// BEGIN STATED LIMITS (generated from forgeStatedLimits in pr_guard_repo_limits_test.go; UPDATE_LIMITS_HEADER=1 go test rewrites it)
//   - a script file or program run by name (the incident's route) (10 pinned)
//   - a list run by xargs or while read (2 pinned)
//   - a raw API client: curl, or an inline program's own HTTP client (4 pinned)
//   - a command word built by expansion (4 pinned)
//   - the output of a substitution that is then run (1 pinned)
//   - a trap (1 pinned)
//   - a shell alias (1 pinned)
//   - a gh alias or extension (2 pinned)
//   - GraphQL, which is read for a repository-mutation name in the line and no further (4 pinned)
//   - create-on-publish (publishing to a path that does not exist yet) (1 pinned)
//   - an inline program that builds the call from pieces, escapes or a helper (5 pinned)
//   - a program handed to an interpreter as a positional parameter of another shell (1 pinned)
//   - a script nested more than forgeRepoMaxDepth shells deep (2 pinned)
//   - an interpreter that is not in programRunners (2 pinned)
//   - a launcher that is not in forgeLaunchers (3 pinned)
// END STATED LIMITS

// forgeRepoVerbs are the `gh repo` / `glab repo` / `glab project` verbs that create or change a repository's
// existence, name or standing.
var forgeRepoVerbs = map[string]bool{"create": true, "fork": true, "rename": true, "delete": true, "archive": true, "unarchive": true, "transfer": true}

// forgeLaunchers are commands that start another program from their arguments but are not in shellWrappers
// (which the git hooks share); the forge reader looks through them as it does through `env`.
var forgeLaunchers = map[string]bool{
	"busybox": true, "toybox": true, "uv": true, "uvx": true, "npx": true, "bunx": true, "pnpm": true, "yarn": true,
	"poetry": true, "pipx": true, "pipenv": true, "pdm": true, "rye": true, "conda": true, "mamba": true,
}

func forgeWrapperName(w string) bool {
	b := path.Base(w)
	return shellWrappers[b] || forgeLaunchers[b]
}

// forgeGraphQLRepoMutation matches a GraphQL mutation that creates, copies, changes or removes a repository.
var forgeGraphQLRepoMutation = regexp.MustCompile(`(?i)\b(?:create|delete|update|archive|unarchive|transfer|fork|clonetemplate|rename)\w*repositor(?:y|ies)\b`)

// forgeRepoEndpoint matches an API path that names a repository or the place repositories are created: with an
// optional absolute API URL in front (`https://api.github.com/`, a GitLab `.../api/v4/`) and an optional
// leading slash. A write to it is a repository-level act.
var forgeRepoEndpoint = regexp.MustCompile(`(?i)^(?:https?://[^/\s]+/(?:api/v[0-9]+/)?)?/?(?:user/repos|orgs/[^/\s]+/repos|repos/[^/\s]+/[^/\s]+(?:/forks|/transfer|/generate)?|projects(?:/(?:user/[^/\s]+|[^/\s]+(?:/(?:fork(?:/[^/\s]+)?|transfer|archive|unarchive|restore|import))?))?|groups/[^/\s]+/projects(?:/[^/\s]+)?|import/[^/\s]+)/?(?:\?\S*)?$`)

// forgeAPIWrites says whether a `gh api` / `glab api` call's flags make it a write: a method other than GET in
// any spelling of the flag (`-X DELETE`, `-XDELETE`, `-X=delete`, `--method delete`, `--method=DELETE`), or any
// field or input flag, which makes gh send a POST (`-f`, `-F`, `-fname=x`, `--field`, `--raw-field`, `--input`).
func forgeAPIWrites(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		method := ""
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = args[i+1]
			}
		case strings.HasPrefix(a, "--method="):
			method = strings.TrimPrefix(a, "--method=")
		case strings.HasPrefix(a, "-X") && !strings.HasPrefix(a, "--"):
			method = strings.TrimPrefix(strings.TrimPrefix(a, "-X"), "=")
		case a == "-f" || a == "-F" || a == "--field" || a == "--raw-field" || a == "--input":
			return true
		case strings.HasPrefix(a, "--field=") || strings.HasPrefix(a, "--raw-field=") || strings.HasPrefix(a, "--input="):
			return true
		case (strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "-F")) && !strings.HasPrefix(a, "--") && len(a) > 2:
			return true // an attached field: -fname=x
		}
		switch strings.ToUpper(strings.Trim(method, `"'`)) {
		case "", "GET", "HEAD":
		default:
			return true
		}
	}
	return false
}

// classifyForgeCLI reads the words that start at a forge CLI (words[0] is gh or glab, any spelling) and says
// what repository-level act they perform, "" for none.
func classifyForgeCLI(words []string) string {
	cli := strings.TrimPrefix(path.Base(words[0]), `\`)
	rest := words[1:]
	// Global options before the noun: a flag, with its value when the flag takes one (-R owner/name).
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if (rest[0] == "-R" || rest[0] == "--repo" || rest[0] == "--hostname") && len(rest) > 1 {
			rest = rest[2:]
			continue
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return ""
	}
	noun := strings.ToLower(rest[0])
	args := rest[1:]
	switch noun {
	case "repo", "project":
		if len(args) == 0 {
			return ""
		}
		verb := strings.ToLower(args[0])
		switch {
		case forgeRepoVerbs[verb]:
			return cli + " " + noun + " " + verb
		case verb == "edit" || verb == "update":
			for _, a := range args[1:] {
				if a == "--visibility" || strings.HasPrefix(a, "--visibility=") {
					return cli + " " + noun + " " + verb + " --visibility"
				}
				if strings.HasPrefix(a, "--archive") {
					return cli + " " + noun + " " + verb + " --archive"
				}
			}
		}
	case "api":
		if !forgeAPIWrites(args) {
			return ""
		}
		if containsWord(args, "graphql") {
			for _, a := range args {
				if forgeGraphQLRepoMutation.MatchString(a) {
					return cli + " api graphql (a repository mutation)"
				}
			}
		}
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && forgeRepoEndpoint.MatchString(a) {
				return cli + " api (a write to a repository endpoint)"
			}
		}
	}
	return ""
}

// forgeCLIName reports whether a command word is gh or glab, however it is spelled.
func forgeCLIName(w string) bool {
	b := strings.TrimPrefix(path.Base(strings.TrimPrefix(w, `\`)), `\`)
	return b == "gh" || b == "glab"
}

// matchForgeRepoVerb says what repository-level act cmd performs, "" for none.
func matchForgeRepoVerb(cmd string) string {
	return forgeRepoScan(cmd, 0)
}

// forgeRepoScan reads cmd as commands, then as text fed to something that runs it. A script found inside the
// line (bash -c, eval, a substitution) goes through the same two readings, so a pipe into an interpreter is
// read at any depth.
func forgeRepoScan(cmd string, depth int) string {
	if v := forgeRepoVerbIn(cmd, depth); v != "" {
		return v
	}
	return forgeRepoVerbFromText(cmd, depth)
}

const forgeRepoMaxDepth = 4

func forgeRepoVerbIn(cmd string, depth int) string {
	if depth > forgeRepoMaxDepth {
		return ""
	}
	segs, _, _ := splitSegments(cmd)
	for _, sg := range segs {
		// A segment of nothing but assignments (`x=$(...)`) has no command word, and its values still run.
		ci := commandStart(sg.words)
		wrapper := ci < len(sg.words) && (forgeWrapperName(sg.words[ci]) || forgeCLIName(sg.words[ci]))
		// The command word, or any word of a wrapper's arguments, may be the forge CLI.
		for i := ci; i < len(sg.words); i++ {
			if !forgeCLIName(sg.words[i]) || (i > ci && !wrapper) {
				continue
			}
			if v := classifyForgeCLI(sg.words[i:]); v != "" {
				return v
			}
		}
		// An interpreter's inline program names the call as text: python3 -c "...", node -e "...", a here-string.
		if ri := programRunnerIndex(sg); ri >= 0 {
			if v := forgeProgramVerb(strings.Join(append(append([]string{}, sg.words[ri+1:]...), sg.herestr...), " ")); v != "" {
				return v
			}
		}
		// A word that is a script is read as one: bash -c "...", eval "...", a substitution, a here-string.
		for i, w := range sg.words {
			isSubst := i < len(sg.dynamic) && sg.dynamic[i]
			if isSubst || (wrapper && strings.ContainsAny(w, " \t\n;&|")) {
				if v := forgeRepoScan(stripSubstitution(w), depth+1); v != "" {
					return v
				}
			}
		}
		if wrapper {
			for _, h := range sg.herestr {
				if v := forgeRepoScan(h, depth+1); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// programRunners are the interpreters whose arguments, here-string or heredoc are a program.
var programRunners = regexp.MustCompile(`^(?:python[0-9.]*|node(?:js)?|bun|deno|ruby|perl|php|lua|osascript|g?awk|mawk|nawk|rscript|julia|pwsh|powershell|tclsh|tsx|ts-node)$`)

// programRunnerIndex is the index of the interpreter in a segment that runs a program given as text, -1 if it
// has none: the command word, or any word of a wrapper's arguments (`env python3 -c ...`).
func programRunnerIndex(sg segment) int {
	ci := commandStart(sg.words)
	if ci >= len(sg.words) {
		return -1
	}
	wrapper := forgeWrapperName(sg.words[ci])
	for i := ci; i < len(sg.words) && (i == ci || wrapper); i++ {
		if programRunners.MatchString(strings.ToLower(path.Base(sg.words[i]))) {
			return i
		}
	}
	return -1
}

// stdinFedRunner says whether a segment is an interpreter that reads its program from stdin: it has flags only
// (`-` counts as one), because a program given inline, a script file and a module name are all operands. So
// `python3`, `python3 -` and `node -u` read stdin; `python3 x.py`, `python3 -c "..."` and `python3 -m json.tool` do not.
func stdinFedRunner(sg segment) bool {
	ri := programRunnerIndex(sg)
	if ri < 0 {
		return false
	}
	for _, a := range sg.words[ri+1:] {
		if !strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

// forgeProgramVerb reads a program's text for a forge CLI followed by a repository verb. A program cannot be
// lexed as a shell line (`subprocess.run(['gh','repo','create'])` is one shell word), so it is cut at quotes,
// brackets, commas and spaces into the words it would pass, and classifyForgeCLI reads those as it reads a
// command's. Refuses on the mention, as the text fallback does.
func forgeProgramVerb(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("'\"`,[](){};\\", r)
	})
	for i, w := range words {
		if forgeCLIName(w) {
			if v := classifyForgeCLI(words[i:]); v != "" {
				return v
			}
		}
	}
	return ""
}

// stripSubstitution turns a word that holds a `$( ... )` or backtick substitution (`x=$(gh ...)`, `"$(gh ...)"`)
// into text whose commands are separate: the marks of the substitution become command separators.
func stripSubstitution(w string) string {
	return strings.NewReplacer("$(", " ; ", "`", " ; ", ")", " ; ").Replace(w)
}

// The text fallback: the line has a segment that runs text it was not given as an argument (a shell reading
// stdin, eval, xargs), so the verb may be anywhere in it, and the text match refuses on the mention.
var forgeTextVerb = regexp.MustCompile(`(?i)(^|[;&|(){!"'\x60$\s\\])(gh|glab)\s+(?:-\S+\s+(?:[^-\s]\S*\s+)?)*(repo|project)\s+(create|fork|rename|delete|archive|unarchive|transfer)(\s|$|[;&|)"'])`)

func forgeRepoVerbFromText(cmd string, depth int) string {
	if depth > forgeRepoMaxDepth {
		return ""
	}
	segs, bodies, _ := splitSegments(cmd)
	runsStdinText := false
	feedsProgram := false
	for _, sg := range segs {
		if stdinFedRunner(sg) {
			feedsProgram = true
		}
		if ci := commandStart(sg.words); ci < len(sg.words) && shellInterpreters[path.Base(sg.words[ci])] && !containsWord(sg.words[ci+1:], "-c") {
			runsStdinText = true // a shell given no -c script reads its script from stdin (`| sh`, `| bash -s`)
		}
	}
	if feedsProgram {
		// An interpreter that reads its program from stdin gets whatever the line feeds it, from wherever in the
		// line (a group, a variable, a nested script): every word of every segment and every heredoc body is
		// read as program text (a here-string to the interpreter is read where the inline program is).
		var all []string
		for _, sg := range segs {
			all = append(all, sg.words...)
		}
		for _, b := range bodies {
			all = append(all, b.body)
		}
		if v := forgeProgramVerb(strings.Join(all, " ")); v != "" {
			return v
		}
	}
	if !runsStdinText {
		return ""
	}
	// Whatever feeds it: every word with a space in it (echo 'gh repo delete x'), the text of a heredoc, and a
	// printf format's \n escapes, each read as a script of its own.
	read := func(text string) string {
		return forgeRepoScan(strings.ReplaceAll(text, `\n`, "\n"), depth+1)
	}
	for _, sg := range segs {
		for _, w := range sg.words {
			if strings.ContainsAny(w, " \t\n") {
				if v := read(w); v != "" {
					return v
				}
			}
		}
		for _, h := range sg.herestr {
			if v := read(h); v != "" {
				return v
			}
		}
	}
	for _, b := range bodies {
		if v := read(b.body); v != "" {
			return v
		}
	}
	// And the mention itself, when the payload is built in a way none of that reads.
	if m := forgeTextVerb.FindStringSubmatch(cmd); m != nil {
		return strings.ToLower(m[2] + " " + m[3] + " " + m[4])
	}
	return ""
}

func containsWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// forgeRepoReason is the refusal. It names what was attempted, says whose decision it is, and says what
// to do, which is to ask: there is no flag, entry or sentinel that lifts it.
func forgeRepoReason(verb string) string {
	return "Refused: `" + strings.TrimSpace(verb) + "` creates, copies, renames, deletes, archives or changes the visibility of a repository, " +
		"which is an outward-facing act on the owner's account, and approval for it is owner-direct. There is no flag, authority entry or sentinel that lifts it " +
		"(an approval an agent can pass to itself is not an approval). Say what repository you need and why, and the owner will make it. " +
		"Reading a repository (`gh repo view`, `gh repo list`, `gh repo clone`, `gh api` without a write) is unaffected."
}
