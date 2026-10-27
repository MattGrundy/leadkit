// Package owners is the pure decision core of leadkit's owner-guard: it enforces
// file-ownership boundaries for sub-agent edits and destructive Bash commands.
//
// CANONICAL SOURCE: ~/.claude/hooks/owner-guard.sh (237 lines). Ported so the policy
// lives in the PROFILE (owners_file) rather than an out-of-repo owners.yaml, the
// per-agent clone-family collapse is config-driven (clone_families) rather than a set of
// hardcoded per-repo cases, and every decision branch is unit-tested.
//
// Decide is a PURE function over its Input (caller identity, candidate paths, tool
// name, the policy + bypass flag, and $HOME/cwd) — it never reads the environment, the
// filesystem, or a real ~/.claude, so tests inject a policy directly and assert the
// Decision struct. The thin hook wrapper (internal/hook.OwnerGuard) resolves the policy,
// caller, bypass sentinel and audit log, then calls Decide.
//
// FAILS OPEN: an unknown tool, an absent policy, no candidate path, or a Bash command
// with no destructive verb all yield Allow — a boundary guard that wrongly blocks is
// worse than the drift it prevents. The one narrow exception is a relative path with no
// absolute working directory to resolve it against: it cannot be placed, so it cannot be
// shown to sit outside a boundary, and it is refused.
package owners

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// PathOwner is one `paths:` entry — a glob and the agent that owns everything matching
// it. First matching glob wins (declaration order).
type PathOwner struct {
	Glob  string `yaml:"glob"`
	Owner string `yaml:"owner"`
}

// Policy is the parsed owners file. HardBlock globs refuse everyone (except .env
// templates and the .bypass escape hatch); Paths assign ownership; CloneFamilies maps a
// canonical repo name to the per-agent clone-name prefixes that collapse onto it before
// matching, so a renamed/new clone can't silently escape ownership.
type Policy struct {
	HardBlock     []string            `yaml:"hard_block"`
	Paths         []PathOwner         `yaml:"paths"`
	CloneFamilies map[string][]string `yaml:"clone_families"`
}

// Decision is one Decide outcome: Allow (exit Code 0) or deny (Code 2, Reason surfaced
// to the agent on stderr). Reason is populated for every branch so the audit log and
// tests can distinguish ALLOW-LEAD / ALLOW-OWNED / ALLOW-UNOWNED / BYPASS from the two
// deny forms.
type Decision struct {
	Allow  bool
	Reason string
	Code   int
}

// Input is everything Decide needs — pure data, no I/O. Policy == nil means the
// boundary system is disabled (no owners_file / missing file) → allow-all.
type Input struct {
	Tool     string  // Edit|Write|MultiEdit|NotebookEdit|Bash
	FilePath string  // the edit-family candidate (single)
	Command  string  // the Bash command (destructive-verb paths are extracted)
	Caller   string  // resolved caller identity
	Lead     string  // the lead identity; "" defaults to "lead"
	Bypass   bool    // the project .bypass sentinel is present
	Home     string  // $HOME, for the home-relative REL
	Pwd      string  // process cwd, for a relative candidate path
	Policy   *Policy // nil => boundary disabled (allow-all)

	// Resolve maps an absolute path to the path the filesystem reaches it by, symlinks resolved (see
	// ResolvePath). Decide never touches the filesystem itself, so the built-in block can only be
	// checked against a symlink alias or a symlinked HOME when the caller supplies it. nil means the
	// built-in block sees case-folded lexical paths only.
	Resolve func(absPath string) string
}

// DefaultLead is the caller identity an unknown/unset agent falls back to — the lead,
// who may edit anywhere not hard-blocked.
const DefaultLead = "lead"

// envTemplateSuffixes are the committed, no-secret .env example files exempt from the
// .env hard-block. Real secret files (.env, .env.local, …) are NOT here and stay blocked.
var envTemplateSuffixes = []string{".env.example", ".env.sample", ".env.template", ".env.dist", ".env.defaults"}

// BuiltinHardBlock are home-relative globs refused to every caller whatever the project's
// owners policy says, and even when it has none. The file that grants merge authority lives in
// the account's leadkit config directory, outside any repo and so outside the two-layer
// review gate: an edit there is the one change to what authorises merges that no reviewer
// sees. The owner edits it deliberately, by hand; the lead's `.bypass` escape hatch lifts the
// block for an authorised edit. Limit: the Bash path only reads rm, rmdir, git rm and mv, so a
// redirect or `sed -i` into the directory is not seen by this guard (tickets 177 and 182 cover
// those shapes for every owned path).
var BuiltinHardBlock = []string{".claude/leadkit/config/*"}

// builtinBlocked reports the BuiltinHardBlock pattern rel matches, "" for none.
func builtinBlocked(rel string) string {
	for _, pat := range BuiltinHardBlock {
		if matchGlob(pat, rel) {
			return pat
		}
	}
	return ""
}

// builtinBlockedFor is the built-in block for one candidate: the lexical REL as before, and the
// path as the filesystem sees it (case folded, symlinks and a symlinked HOME resolved).
func builtinBlockedFor(rawPath, rel string, in Input) string {
	if pat := builtinBlocked(rel); pat != "" {
		return pat
	}
	return builtinBlockedPath(rawPath, in)
}

// Decide runs the boundary decision. It computes the candidate path(s) from the tool,
// then evaluates each in the shell's EXACT order; the FIRST deny across candidates wins,
// otherwise the last per-path allow is returned.
func Decide(in Input) Decision {
	if in.Policy == nil {
		// No owners policy disables the project boundary, not the built-in block.
		for _, p := range candidatePaths(in) {
			if p.Unresolved || in.Bypass {
				continue
			}
			rel, placed := relFor(p.Path, in.Home, in.Pwd)
			if !placed {
				continue
			}
			if pat := builtinBlockedFor(p.Path, rel, in); pat != "" {
				return builtinDeny(rel, pat)
			}
		}
		return Decision{Allow: true, Code: 0, Reason: "DISABLED no owners policy"}
	}
	lead := in.Lead
	if lead == "" {
		lead = DefaultLead
	}

	paths := candidatePaths(in)
	if len(paths) == 0 {
		// Unknown tool, empty edit path, or a Bash command with no destructive verb.
		return Decision{Allow: true, Code: 0, Reason: "ALLOW no candidate paths"}
	}

	last := Decision{Allow: true, Code: 0, Reason: "ALLOW"}
	for _, p := range paths {
		d := decideOne(p.Path, p.Unresolved, in, lead)
		if !d.Allow {
			return d
		}
		last = d
	}
	return last
}

// candidatePaths returns the file paths a given tool contributes to the decision. For
// the edit family it's the single file_path; for Bash it's every path an rm/rmdir/
// git rm/mv touches; any other tool contributes none (fail open).
func candidatePaths(in Input) []Target {
	switch in.Tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if in.FilePath == "" {
			return nil
		}
		return []Target{{Path: in.FilePath}}
	case "Bash":
		return ExtractDestructivePaths(in.Command, in.Home, in.Pwd)
	default:
		return nil
	}
}

func builtinDeny(rel, pat string) Decision {
	return Decision{
		Allow:  false,
		Code:   2,
		Reason: fmt.Sprintf("owner-guard: HARD-BLOCK %s matches the built-in '%s' — this is where merge authority is granted, outside the repo and the review gate; the owner edits it by hand — refused; set .bypass to override (lead-only).", rel, pat),
	}
}

// decideOne evaluates ONE candidate path in the shell's decision order:
// clone-canonicalization → bypass → .env-template exemption → hard-block → lead →
// owner check → unowned.
func decideOne(rawPath string, unresolved bool, in Input, lead string) Decision {
	rel, placed := relFor(rawPath, in.Home, in.Pwd)
	rel = canonicalizeClone(rel, in.Policy.CloneFamilies)

	// 2. Bypass — precedence even over hard-block (lead escape hatch).
	if in.Bypass {
		return Decision{Allow: true, Code: 0, Reason: "BYPASS " + rel}
	}

	// 2a. A Bash word whose shell expansion cannot be known here ($VAR, ~user, $(…), …)
	// cannot be shown to sit outside a boundary, and the unowned fallback below ALLOWS —
	// so it is refused, for every caller including lead, exactly as an unplaceable path is.
	if unresolved {
		return Decision{
			Allow:  false,
			Code:   2,
			Reason: fmt.Sprintf("owner-guard: cannot resolve '%s' — the shell expands it to a path this guard cannot know (only ~, $HOME and $PWD are resolved) — refused; spell the path out, or set .bypass to override (lead-only).", rawPath),
		}
	}

	// 2b. A path that cannot be placed cannot be shown to sit outside a boundary, and the
	// unowned fallback below ALLOWS — so it is refused, for every caller including lead.
	if !placed {
		return Decision{
			Allow:  false,
			Code:   2,
			Reason: fmt.Sprintf("owner-guard: cannot place '%s' — it is relative and there is no absolute working directory to resolve it against — refused; use an absolute path, or set .bypass to override (lead-only).", rawPath),
		}
	}

	// 3. .env template exemption (computed before the hard-block that would otherwise
	// catch these committed, no-secret examples).
	envTemplate := isEnvTemplate(rel)

	// 4. Hard-block — universal, except .env templates. The built-in block comes first and has
	// no exemption.
	if pat := builtinBlockedFor(rawPath, rel, in); pat != "" {
		return builtinDeny(rel, pat)
	}
	if !envTemplate {
		for _, pat := range in.Policy.HardBlock {
			if matchGlob(pat, rel) {
				return Decision{
					Allow:  false,
					Code:   2,
					Reason: fmt.Sprintf("owner-guard: HARD-BLOCK %s matches '%s' — refused; set .bypass to override (lead-only).", rel, pat),
				}
			}
		}
	}

	// 5. Lead can edit anywhere not hard-blocked.
	if in.Caller == lead {
		return Decision{Allow: true, Code: 0, Reason: "ALLOW-LEAD " + rel}
	}

	// 6. Owner check — first matching glob wins.
	for _, po := range in.Policy.Paths {
		if matchGlob(po.Glob, rel) {
			if po.Owner == in.Caller {
				return Decision{Allow: true, Code: 0, Reason: "ALLOW-OWNED " + rel}
			}
			return Decision{
				Allow:  false,
				Code:   2,
				Reason: fmt.Sprintf("owner-guard: %s is owned by '%s', caller '%s' — refused; hand off via TaskCreate(owner=%s).", rel, po.Owner, in.Caller, po.Owner),
			}
		}
	}

	// 7. Unowned — allow but warn (audited as ALLOW-UNOWNED).
	return Decision{Allow: true, Code: 0, Reason: "ALLOW-UNOWNED " + rel}
}

// relFor computes the $HOME-relative REL the shell matches globs against. A relative
// path is joined onto pwd, the result is lexically cleaned (symlinks are not resolved, and case is
// kept: only the built-in block is checked against the resolved, case-folded path, see
// builtinBlockedPath),
// and a "$HOME/" prefix is then stripped; an absolute path outside $HOME is kept as-is.
//
// A path whose last element is empty, `.` or `..` names a directory and keeps a trailing
// `/` after cleaning.
//
// ok is false when the path is relative and pwd is not absolute.
func relFor(path, home, pwd string) (rel string, ok bool) {
	abs := path
	if !filepath.IsAbs(abs) {
		if !filepath.IsAbs(pwd) {
			return path, false
		}
		abs = pwd + "/" + path
	}
	rel = filepath.Clean(abs)
	switch path[strings.LastIndex(path, "/")+1:] {
	case "", ".", "..":
		if !strings.HasSuffix(rel, "/") {
			rel += "/"
		}
	}
	if home != "" {
		if h := filepath.Clean(home); h != "/" && strings.HasPrefix(rel, h+"/") {
			return rel[len(h)+1:], true
		}
	}
	return rel, true
}

// canonicalizeClone collapses `Projects/<clone>/<tail>` to `Projects/<canonical>/<tail>`
// when <clone> equals, or is a `<prefix>-…` extension of, one of a canonical's
// clone_families prefixes — replacing the shell's hardcoded per-clone glob cases with a
// config-driven map so a new clone name can't escape
// ownership. Requires at least one tail segment (Projects/<clone>/<something>), matching
// the shell's `Projects/*/*` guard.
func canonicalizeClone(rel string, families map[string][]string) string {
	const prefix = "Projects/"
	if len(families) == 0 || !strings.HasPrefix(rel, prefix) {
		return rel
	}
	rest := rel[len(prefix):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rel
	}
	head := rest[:slash]
	tail := rest[slash+1:]
	if tail == "" {
		return rel
	}
	for canonical, prefixes := range families {
		for _, p := range prefixes {
			if head == p || strings.HasPrefix(head, p+"-") {
				return prefix + canonical + "/" + tail
			}
		}
	}
	return rel
}

// isEnvTemplate reports whether rel is a committed .env EXAMPLE file (no secrets).
func isEnvTemplate(rel string) bool {
	for _, suf := range envTemplateSuffixes {
		if strings.HasSuffix(rel, suf) {
			return true
		}
	}
	return false
}

// matchGlob reports whether pattern matches s with the shell `case`-glob semantics the
// owners file relies on: `*` spans `/` (so `Projects/foo/**` ≡ `Projects/foo/*`), `?`
// matches one char, everything else is literal. Not filepath.Match (which stops `*` at
// `/`); a plain pattern->regex translation instead.
func matchGlob(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	matched, err := regexp.MatchString(b.String(), s)
	return err == nil && matched
}

// Target is one path a Bash command's destructive verb acts on, after the shell's quote
// removal and ~ / $HOME / $PWD expansion. Unresolved is true when the word contains an
// expansion this package cannot evaluate (any other $VAR, ${…}, $(…), `…`, $'…', ~user);
// Path then holds the word as far as it was read, for the refusal message only.
type Target struct {
	Path       string
	Unresolved bool
}

// ExtractDestructivePaths returns every path a command's destructive verbs (rm, rmdir,
// git rm, mv — both mv ends) act on, splitting the command into segments at `; & |` and
// skipping `-`-flags. A command with no destructive verb yields none (the Bash path only
// guards deletions/moves).
//
// Each word is read the way the shell reads it BEFORE the path is judged: quotes are
// removed, a backslash escapes the next character, and `~`, `$HOME` and `$PWD` are expanded
// against home and pwd. Callers then Clean and match the expanded text (relFor does), so
// an expansion that introduces `..` or an absolute prefix is cleaned like any other path.
//
// Any other expansion is NOT guessed at: the word is returned Unresolved, which Decide
// refuses. The hook's own environment is no authority for it — the shell the command runs
// in can assign the variable earlier in the same command — so there is no lookup. The
// exceptions are the parameters that can never hold a `/` or `..` (`$$ $? $# $!`), which
// are not paths and so are not refused.
//
// Words that the shell does not hand to the verb as arguments are not read as arguments: a
// `#` comment, and a redirection (`> f`, `2>&1`, `<<EOF`) with its operand.
//
// The segment split is quote-aware, so a `;` inside a quoted string (a commit message, a
// filename) neither invents a verb nor cuts a real argument list short. The text of a
// `$(…)` or backtick substitution is scanned as commands even inside double quotes, and a
// segment boundary inside one still splits — the split never hides a VERB. (It can still
// miss a command the verb check does not recognise: see ticket 177.)
func ExtractDestructivePaths(cmd, home, pwd string) []Target {
	var paths []Target
	for _, toks := range scanSegments(cmd, home, pwd) {
		switch {
		case toks[0].Path == "rm" || toks[0].Path == "rmdir":
			paths = append(paths, nonFlags(toks[1:])...)
		case toks[0].Path == "git" && len(toks) >= 2 && toks[1].Path == "rm":
			paths = append(paths, nonFlags(toks[2:])...)
		case toks[0].Path == "mv":
			paths = append(paths, nonFlags(toks[1:])...)
		}
	}
	return paths
}

// nonFlags returns toks with `-`-prefixed flags dropped. An unresolved word is never
// dropped: it might expand to a flag, but it might equally expand to a path.
func nonFlags(toks []Target) []Target {
	var out []Target
	for _, t := range toks {
		if !t.Unresolved && strings.HasPrefix(t.Path, "-") {
			continue
		}
		out = append(out, t)
	}
	return out
}

// scanSegments splits a command into its simple-command segments, each a list of shell
// words (never empty). Outside quotes `; & |` end a segment and whitespace ends a word;
// '…' is literal; "…" is literal except for `$` and backtick expansion and the backslash
// escapes `\$ \` \" \\`; outside quotes a backslash escapes the next character. Quoting
// nests through `$(…)` and backticks: their text is scanned as plain command text even
// inside "…". A word made only of empty quotes is dropped — it names nothing. An
// unterminated quote runs to the end of the command.
//
// An unquoted `#` at the start of a word begins a comment, which runs to the end of the
// line. An unquoted `<` or `>` begins a redirection: the operator (with any leading fd
// number, and `&`, `>>`, `<<<` …) and the one word after it are dropped. `&` directly
// before or after `>` is part of the operator, not a separator.
func scanSegments(cmd, home, pwd string) [][]Target {
	var (
		segs       [][]Target
		words      []Target
		cur        strings.Builder
		started    bool // a word is in progress (even if still empty, e.g. `""`)
		quoted     bool // the word in progress used a quote or backslash
		unresolved bool
		redirect   bool // the next word is a redirection's operand
		modes      []byte
	)
	// modes is the nesting stack: '\'' and '"' are quotes; '(' and '`' are substitutions
	// (or subshells), which read as plain command text. Empty means plain.
	top := func() byte {
		if len(modes) == 0 {
			return 0
		}
		return modes[len(modes)-1]
	}
	push := func(m byte) { modes = append(modes, m) }
	pop := func() { modes = modes[:len(modes)-1] }
	reset := func() {
		cur.Reset()
		started, quoted, unresolved = false, false, false
	}
	flush := func() {
		if !started {
			return
		}
		switch {
		case redirect:
			redirect = false
		case cur.Len() > 0 || unresolved:
			words = append(words, Target{Path: cur.String(), Unresolved: unresolved})
		}
		reset()
	}
	endSegment := func() {
		flush()
		redirect = false
		if len(words) > 0 {
			segs = append(segs, words)
		}
		words = nil
	}
	isDigits := func(s string) bool {
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return s != ""
	}
	rs := []rune(cmd)
	// escape handles a backslash at rs[i]; it returns the index of the last rune consumed.
	escape := func(i int, inDouble bool) int {
		started, quoted = true, true
		if i+1 >= len(rs) {
			return i
		}
		next := rs[i+1]
		if inDouble && !strings.ContainsRune("$`\"\\", next) {
			cur.WriteRune('\\') // inside "…" the backslash is literal unless it escapes one of these
			return i
		}
		cur.WriteRune(next)
		return i + 1
	}
	dollar := func(i int, inDouble bool) int {
		started = true
		if i+1 < len(rs) && rs[i+1] == '(' {
			push('(')
		}
		return expandDollar(rs, i, inDouble, home, pwd, &cur, &unresolved)
	}

	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch top() {
		case '\'':
			if c == '\'' {
				pop()
			} else {
				cur.WriteRune(c)
			}
			continue
		case '"':
			switch c {
			case '"':
				pop()
			case '\\':
				i = escape(i, true)
			case '`':
				unresolved = true
				cur.WriteRune(c)
				push('`')
			case '$':
				i = dollar(i, true)
			default:
				cur.WriteRune(c)
			}
			continue
		}
		switch {
		case c == '\'':
			started, quoted = true, true
			push('\'')
		case c == '"':
			started, quoted = true, true
			push('"')
		case c == '\\':
			i = escape(i, false)
		case c == '`':
			started, unresolved = true, true
			cur.WriteRune(c)
			if top() == '`' {
				pop()
			} else {
				push('`')
			}
		case c == '$':
			i = dollar(i, false)
		case c == '(':
			started = true
			cur.WriteRune(c)
			push('(')
		case c == ')' && top() == '(':
			started = true
			cur.WriteRune(c)
			pop()
		case c == '~' && !started:
			started = true
			i = expandTilde(rs, i, home, pwd, &cur, &unresolved)
		case c == '#' && !started:
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case c == ';' || c == '|' || (c == '&' && !(i+1 < len(rs) && rs[i+1] == '>')):
			endSegment()
		case (c == '<' || c == '>') && i+1 < len(rs) && rs[i+1] == '(':
			// <(…) / >(…) is a process substitution, a word this package cannot evaluate.
			started, unresolved = true, true
			cur.WriteRune(c)
		case c == '<' || c == '>' || c == '&':
			if started && !quoted && !unresolved && isDigits(cur.String()) {
				reset() // `2>f`: the 2 is the operator's fd number, not an argument
			} else {
				flush()
			}
			j := i
			if rs[j] == '&' { // &> and &>>
				j++
			}
			for j < len(rs) && (rs[j] == '<' || rs[j] == '>') {
				j++
			}
			if j < len(rs) && (rs[j] == '&' || (rs[j] == '|' && rs[j-1] == '>')) { // >&2 <&- >|
				j++
			}
			i = j - 1
			redirect = true
		case isWordSpace(c):
			flush()
		default:
			started = true
			cur.WriteRune(c)
		}
	}
	endSegment()
	return segs
}

// expandTilde expands the unquoted word-initial `~` at rs[i]. Only a bare `~` (followed by
// `/` or the end of the word) is resolved, to home; `~+` is pwd. `~user`, `~-` and the like
// are left unresolved. It returns the index of the last rune consumed.
func expandTilde(rs []rune, i int, home, pwd string, cur *strings.Builder, unresolved *bool) int {
	end := i + 1
	for end < len(rs) && rs[end] != '/' && !isWordSpace(rs[end]) {
		end++
	}
	prefix := string(rs[i+1 : end])
	switch {
	case prefix == "" && home != "":
		cur.WriteString(home)
	case prefix == "+" && pwd != "":
		cur.WriteString(pwd)
	default:
		cur.WriteString(string(rs[i:end]))
		*unresolved = true
	}
	return end - 1
}

// expandDollar expands the `$` at rs[i]. `$HOME`/`${HOME}` resolve to home and
// `$PWD`/`${PWD}` to pwd; `$$` (the shell's PID) stands in as a digit; every other parameter
// (`$!`, `$?` and `$#` included), `$(…)`, `$((…))`, and `$'…'` / `$"…"` is unresolved.
// A `$` followed by anything else is a literal dollar sign. It returns the
// index of the last rune consumed.
func expandDollar(rs []rune, i int, inDouble bool, home, pwd string, cur *strings.Builder, unresolved *bool) int {
	lookup := func(name string) bool {
		switch {
		case name == "HOME" && home != "":
			cur.WriteString(home)
		case name == "PWD" && pwd != "":
			cur.WriteString(pwd)
		default:
			return false
		}
		return true
	}
	if i+1 >= len(rs) {
		cur.WriteRune('$')
		return i
	}
	next := rs[i+1]
	switch {
	case next == '{':
		end := i + 2
		for end < len(rs) && rs[end] != '}' {
			end++
		}
		if end < len(rs) && lookup(string(rs[i+2:end])) {
			return end
		}
		cur.WriteString(string(rs[i:min(end+1, len(rs))]))
		*unresolved = true
		return min(end, len(rs)-1)
	case next == '_' || (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z'):
		end := i + 1
		for end < len(rs) && (rs[end] == '_' || (rs[end] >= '0' && rs[end] <= '9') || (rs[end] >= 'A' && rs[end] <= 'Z') || (rs[end] >= 'a' && rs[end] <= 'z')) {
			end++
		}
		if lookup(string(rs[i+1 : end])) {
			return end - 1
		}
		cur.WriteString(string(rs[i:end]))
		*unresolved = true
		return end - 1
	case !inDouble && (next == '\'' || next == '"'):
		// $'…' / $"…": mark it, and leave the quote for the caller to read as a quote.
		cur.WriteRune('$')
		*unresolved = true
		return i
	case next == '$':
		// $$ is the shell's PID: whoever writes the command cannot force it to a chosen value, so
		// it can neither spell out nor hide a name. A placeholder digit stands in, and the path
		// is judged as if the PID were any other digit.
		cur.WriteRune('0')
		return i + 1
	case next == '(' || (next >= '0' && next <= '9') || strings.ContainsRune("@*-?!#", next):
		// Unresolved, including $? $! $#. The test is not whether a value could contain a `/` or
		// `..` but whether the command's author can force it: $! is empty with no background
		// job (`.env$!` is `.env`), $? is steerable in the same command (`(exit 7); tasks/15$?.md`),
		// $# by argument count. A placeholder digit would let any of them dodge an exact-name match.
		cur.WriteString(string(rs[i : i+2]))
		*unresolved = true
		return i + 1
	}
	cur.WriteRune('$')
	return i
}

func isWordSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// ParsePolicy unmarshals an owners file's YAML into a Policy. A malformed file is an
// error (the caller then fails open — a policy it can't parse must not silently block);
// an empty file yields an empty (allow-everything-unowned) policy.
func ParsePolicy(data []byte) (Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("owners: parse policy: %w", err)
	}
	return p, nil
}

// ResolveCaller applies the shell's identity precedence: $CLAUDE_AGENT_NAME, else the
// project .active-agent file's contents, else the lead default. activeAgent is the raw
// file contents ("" when absent); lead is the fallback ("" → DefaultLead).
func ResolveCaller(agentEnv, activeAgent, lead string) string {
	if lead == "" {
		lead = DefaultLead
	}
	if s := strings.TrimSpace(agentEnv); s != "" {
		return s
	}
	if s := strings.TrimSpace(activeAgent); s != "" {
		return s
	}
	return lead
}
