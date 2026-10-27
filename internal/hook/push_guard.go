package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// push-guard has no bash predecessor — it's new, built after an incident: an agent
// running with a human's bypass-capable GitHub privileges pushed directly to a
// release/* branch, bypassing the PR/gate flow. This is client-side defense-in-depth;
// the durable fix (agents must not hold bypass-capable credentials) is an infra
// change on the user's side — see docs/ADOPTION-BRIEF.md's push-policy section.
//
// WIRING (not performed here, zero-impact this step): a later, reviewed `leadkit
// init` wires this as a PreToolUse(Bash) hook pointing at `leadkit hook push-guard`.
// This subcommand does not touch settings.json.
//
// FAIL POSTURE — the one hook in this package that differs from the rest: a
// command with no push in it still fails OPEN (this guard doesn't govern it), but
// a recognised `git push` whose target CANNOT be verified against the whitelist
// (git exec failed, an unparseable refspec, an unresolvable repo path) fails CLOSED
// — denied. A push we can't prove is safe is treated as unsafe. So does a command the
// shared parser cannot read (parsePushCommands' pushUnsure: a subshell runner, `bash -c`,
// `eval`, `$( … )` …): it may hold a push, and "I could not tell" is not "no push". push-guard is also
// the only hook that execs git at all (to resolve a bare push's current branch, and
// the repo's origin remote for the per-repo whitelist override) — via the single
// runGit func var below, injectable so tests never shell out.
//
// VIOLATION LOGGING — every deny (whitelist-miss AND fail-closed-unverifiable) is a
// security event, not just an agent bounce: it appends one best-effort JSONL record
// to cfg.ViolationsLogPath (see violationRecord) so it leaves a durable, user-visible
// trail — internal/viewer's /health reads the same file. A logging failure (missing
// dir, unwritable path, ...) NEVER blocks the deny itself; see logViolation.

// runGit execs `git [-C repoPath] args...` and returns stdout trimmed. repoPath ==
// "" omits -C (uses the process cwd). ok is false on any failure — callers apply
// this package's fail-closed/fail-open rules themselves; runGit itself just reports
// what happened.
var runGit = func(repoPath string, args ...string) (out string, ok bool) {
	full := args
	if repoPath != "" {
		full = append([]string{"-C", repoPath}, args...)
	}
	data, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// PushGuardConfig is push-guard's whitelist inputs — kept as a plain struct (not
// *config.Profile) so this package doesn't depend on internal/config, matching every
// other hook. EffectiveWhitelist resolves the branch-pattern whitelist a push is judged
// by FROM THE TARGET REPO — given the repo path actually being pushed (repoPath == ""
// means the process cwd) and its resolved origin remote URL (origin == "" when it could
// not be resolved). The injected implementation (cmd/leadkit) loads the profile that
// GOVERNS that target repo (walk-up from repoPath, the same way config show / doctor
// resolve from a directory) and computes its effective whitelist — the target's own
// authority.<key> override else its top-level push_whitelist — falling back to the
// BUILT-IN default whitelist when the target has no leadkit profile (or an unreadable
// one). This is the correctness fix: the whitelist comes from the pushed repo, NOT the
// session-ambient profile the hook loaded at startup. The guard itself stays PURE — it
// loads no profiles; it only calls this resolver. EffectiveWhitelist == nil (the zero
// value) is FAIL-CLOSED, not allow-all: the resolved whitelist is then empty and every
// non-trivial push is denied (a misconfigured caller can't accidentally open the guard).
// The resolver must never return an empty/allow-all list — it returns the built-in
// default when it cannot resolve the target (see the cmd/leadkit implementation).
// ViolationsLogPath == "" disables logging entirely (logViolation is a no-op then).
type PushGuardConfig struct {
	EffectiveWhitelist func(repoPath, origin string) []string
	ViolationsLogPath  string

	// Mode resolves push-guard's ENFORCEMENT posture for the TARGET repo (advisory |
	// enforce), from that repo's own governing profile — the same per-target resolution
	// the whitelist uses. Optional: nil (every existing test, an unconfigured adopter)
	// means ModeAdvisory, so behavior is unchanged. push-guard ALREADY fail-CLOSES on its
	// unverifiable paths (a git-exec failure on a bare push, an unparseable refspec, a nil
	// resolver → empty whitelist → default-deny) in BOTH modes — so the verdict is
	// identical advisory-vs-enforce; the mode only NAMES the posture in the deny reason
	// (and guarantees no unverifiable path silently opens). See docs/build-tasks/
	// configurable-guard-enforcement.md.
	Mode func(repoPath string) GuardMode
}

// pushTarget is one resolved push destination — a branch (or, if IsTag, a tag) name
// push-guard checks against the whitelist. Tags never match the default whitelist by
// construction (no pattern in it is meant to match a tag, and IsTag is checked
// directly, never against the whitelist at all).
type pushTarget struct {
	Name  string
	IsTag bool
}

// violationRecord is one JSONL line appended to the violations log on every deny.
// Field names are the log's stable file-based contract (internal/viewer's /health
// reads them independently, not via this type).
type violationRecord struct {
	TS        string `json:"ts"`
	SessionID string `json:"session_id"`
	Repo      string `json:"repo"`
	Target    string `json:"target"`
	Command   string `json:"command"`
	Force     bool   `json:"force"`
	Reason    string `json:"reason"` // "whitelist-miss" | "unverifiable" | "unparseable"
}

// maxLoggedCommandLen truncates the logged command string to a sane length — the
// spec's own wording ("command, truncated to a sane length"); denied commands are
// ordinary shell one-liners, this is just a guard against a pathological input.
const maxLoggedCommandLen = 500

// logViolation appends rec as one JSONL line to path — best-effort: ANY failure
// (path == "", missing parent dir, unwritable, marshal error) is silently swallowed.
// A logging failure must never block the deny that triggered it. An injectable func
// var so tests can assert on calls without touching disk, though the default real-
// file behavior is also exercised directly (an unwritable path still denies).
var logViolation = func(path string, rec violationRecord) {
	if path == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// truncate returns s, cut to at most max bytes.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// isForcePush reports whether a push's raw args (everything after "push") carry a
// force marker: --force, -f, --force-with-lease (with or without a =<ref> suffix), or
// a leading "+" on a refspec. Force-push isn't special to the whitelist check itself
// (same patterns apply) — this only feeds the violation log's "force" field.
func isForcePush(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--force", a == "-f", a == "--force-with-lease":
			return true
		case strings.HasPrefix(a, "--force-with-lease="):
			return true
		case strings.HasPrefix(a, "+") && a != "+":
			return true
		}
	}
	return false
}

// PushGuard is the whitelist-only, default-deny git-push guard. See the package doc
// above for the fail-open/fail-closed split this hook alone has, and for the
// violation-logging behavior on every deny.
func PushGuard(in Input, cfg PushGuardConfig) Result {
	parsed := parsePushCommands(in.ToolInput.Command)
	switch parsed.State {
	case pushNone:
		// No git-push in ANY command segment, and nothing that could hide one — this guard
		// doesn't govern it (fail OPEN).
		return allow
	case pushUnsure:
		// The command holds a construct that may carry a push the parser cannot read. That
		// is not "no push": refuse, exactly as for a target that cannot be verified.
		return pushGuardUnparseable(in, cfg, parsed.Why)
	}
	// Default-deny across EVERY push in the command: a chained/pipe-joined command may
	// carry more than one `git push` (e.g. `git push feat/ok && git push release/bad`).
	// Any single whitelist-miss or unverifiable target denies the whole command.
	for _, inv := range parsed.Invocations {
		if res, denied := checkOnePush(in, cfg, inv.repoPath, inv.args, inv.gitConfig); denied {
			return res
		}
	}
	return allow
}

// pushGuardUnparseable is the deny for a command whose shell structure may hide a push
// (a subshell runner, `bash -c`, `eval`, `$( … )`, …): the guard cannot tell it from a
// command with no push, so it refuses and logs it as a security event like any other deny.
func pushGuardUnparseable(in Input, cfg PushGuardConfig, why string) Result {
	logViolation(cfg.ViolationsLogPath, violationRecord{
		TS:        time.Now().UTC().Format(time.RFC3339),
		SessionID: in.SessionID,
		Command:   truncate(in.ToolInput.Command, maxLoggedCommandLen),
		Reason:    "unparseable",
	})
	mode := ModeAdvisory
	if cfg.Mode != nil {
		mode = cfg.Mode("")
	}
	return jsonDeny("Push-guard: command denied — it may contain a `git push` the guard cannot read (" + why + "). " + unreadablePushAdvice + loggedNotice + guardModeNote(mode))
}

// unreadablePushAdvice is the shared instruction both push hooks give when they refuse a
// command they cannot read: say the push plainly, as its own command.
const unreadablePushAdvice = "Run the push as its own plain command — `git push <remote> <branch>`, `git -C <clone> push <remote> <branch>` or `leadkit push --repo <clone> --branch <branch>` — not inside a subshell, `$( … )`, backticks, `bash -c`, `eval`, `xargs` or another wrapper."

// checkOnePush applies the whitelist/fail-closed verdict to ONE resolved git-push
// invocation (repoPath + the cleaned args after "push"). denied=false means this push
// is allowed; the caller keeps checking the rest. The whitelist is resolved FROM THE
// TARGET REPO (repoPath + its origin) via cfg.EffectiveWhitelist — the correctness fix,
// so a push is judged by the pushed repo's own policy, never the session-ambient one.
// The force flag, target resolution, fail-closed cases and violation logging are all
// unchanged; only WHERE the whitelist comes from moved out to the injected resolver.
func checkOnePush(in Input, cfg PushGuardConfig, repoPath string, args []string, gitConfig []string) (res Result, denied bool) {
	// Resolve the target repo's origin (best-effort). "" when git can't resolve it —
	// NOT fail-closed: the resolver then returns the target's top-level default, the
	// spec's rule "if the remote can't be resolved, use the top-level default" (now
	// the TARGET's default, not the session's). A nil resolver leaves the whitelist
	// empty → fail-closed (default-deny), never allow-all.
	origin, _ := runGit(repoPath, "config", "--get", "remote.origin.url")
	var whitelist []string
	if cfg.EffectiveWhitelist != nil {
		whitelist = cfg.EffectiveWhitelist(repoPath, origin)
	}
	mode := ModeAdvisory
	if cfg.Mode != nil {
		mode = cfg.Mode(repoPath)
	}

	force := isForcePush(args)
	deny := func(target, reason string) (Result, bool) {
		logViolation(cfg.ViolationsLogPath, violationRecord{
			TS:        time.Now().UTC().Format(time.RFC3339),
			SessionID: in.SessionID,
			Repo:      repoPath,
			Target:    target,
			Command:   truncate(in.ToolInput.Command, maxLoggedCommandLen),
			Force:     force,
			Reason:    reason,
		})
		if reason == "unverifiable" {
			return pushGuardFailClosed(mode), true
		}
		return pushGuardDeny(target, whitelist, mode), true
	}

	remote, refspecs, _ := parsePushArgs(args)
	if o := firstPushRefSetOption(args); o != "" {
		return deny("--"+o, "whitelist-miss")
	}
	if mirroredRemote(repoPath, remote, gitConfig) != "" {
		return deny("", "unverifiable")
	}
	if followTagsConfigured(repoPath, args, gitConfig) != "" {
		return deny("push.followTags", "whitelist-miss")
	}

	var targets []pushTarget
	if len(refspecs) == 0 {
		// Bare `git push` / `git push <remote>` — the target is whatever the
		// current branch pushes to. EXEC git; a failure here IS fail-closed (this
		// is the "target can't be verified" case the spec calls out by name).
		if unnamedRefSetConfig(repoPath, remote, gitConfig) != "" {
			return deny("", "unverifiable")
		}
		branch, ok := runGit(repoPath, "rev-parse", "--abbrev-ref", "HEAD")
		if !ok || branch == "" || branch == "HEAD" { // "HEAD" itself = detached
			return deny("", "unverifiable")
		}
		targets = []pushTarget{{Name: branch}}
	} else {
		targets = targetsFromRefspecs(refspecs)
		for _, t := range targets {
			if t.Name == "" {
				// An unparseable refspec (e.g. a bare ":") — can't verify it, so
				// fail closed rather than silently letting it through as "".
				return deny("", "unverifiable")
			}
		}
	}

	for _, t := range targets {
		if t.IsTag || !matchesWhitelist(t.Name, whitelist) {
			return deny(t.Name, "whitelist-miss")
		}
	}
	return allow, false
}

// commandIsLeadkitPush reports whether cmd invokes the native `leadkit push` primitive in
// ANY simple-command segment (after any leading `VAR=value` env assignments). push-gate
// uses this to ALLOW a `leadkit push …` Bash command without re-deriving anything: the
// primitive gates in-process on the resolved full sha, so re-checking it in the hook would
// double-block our own gated push. The internal `git -C <repo> push …` the primitive runs
// is a subprocess, never a Bash tool call, so the hook never sees it — only this top-level
// `leadkit push` command.
func commandIsLeadkitPush(cmd string) bool {
	toks := lexShell(cmd)
	// Split into simple-command segments on separators, then look for a segment whose first
	// two command words (after leading assignments) are `leadkit push`.
	var seg []string
	segs := [][]string{}
	for _, t := range toks {
		if t.kind == tokSep {
			segs = append(segs, seg)
			seg = nil
			continue
		}
		if t.kind == tokWord {
			seg = append(seg, t.text)
		}
	}
	segs = append(segs, seg)
	for _, words := range segs {
		words = stripLeadingAssignmentWords(words)
		if len(words) >= 2 && words[0] == "leadkit" && words[1] == "push" {
			return true
		}
	}
	return false
}

// stripLeadingAssignmentWords drops leading `NAME=value` tokens from a segment's words, so
// the first REAL command word can be identified (`FOO=bar leadkit push` → `leadkit push`).
// A word is a leading assignment only until the first non-assignment word — anything after
// the command word is an argument, never a leading assignment.
func stripLeadingAssignmentWords(words []string) []string {
	i := 0
	for i < len(words) && looksLikeAssignmentWord(words[i]) {
		i++
	}
	return words[i:]
}

// looksLikeAssignmentWord reports whether w is a `NAME=…` shell assignment token (a valid
// env var name, then `=`). It does NOT validate the value (that's readShellWord's job).
func looksLikeAssignmentWord(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	return isAssignName(w[:eq])
}

// isAssignName reports whether s is a valid shell variable name (`[A-Za-z_][A-Za-z0-9_]*`).
func isAssignName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// leadingAssignments parses the shell-style leading `VAR=value …` assignments at the front
// of cmd — the `VAR=value command …` prefix that sets env vars for the command word that
// follows — and returns them as a name→value map. It stops at the FIRST word that is not a
// valid assignment (the command word), so an assignment AFTER the command word is never
// treated as a leading one. Values are shell-word-correct: single/double quotes (INCLUDING
// spaces inside them, which the coarser lexShell would split) and adjacent-quote
// concatenation are honored — because the escape hatch the push-gate deny advertises,
// `REVIEW_GATE_BYPASS="a reason" git push`, must actually be reachable for the hook path.
func leadingAssignments(cmd string) map[string]string {
	out := map[string]string{}
	i, n := 0, len(cmd)
	for i < n {
		for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		start := i
		for i < n {
			c := cmd[i]
			if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > start && c >= '0' && c <= '9') {
				i++
				continue
			}
			break
		}
		// A valid assignment needs a non-empty name immediately followed by '='.
		if i == start || i >= n || cmd[i] != '=' {
			break // the command word (or junk) — stop; no more leading assignments.
		}
		name := cmd[start:i]
		i++ // consume '='
		val, ni := readShellWord(cmd, i)
		out[name] = val
		i = ni
	}
	return out
}

// readShellWord reads one shell word starting at s[i] — up to the next unquoted whitespace
// or a shell control metacharacter — honoring single quotes (literal, no escapes), double
// quotes (with `\"`/`\\` escapes), and adjacent-quote concatenation (`"a"b'c'` → `abc`). It
// returns the UNQUOTED value and the index just past the word. An unterminated quote reads
// to end-of-string (best-effort, never panics).
func readShellWord(s string, i int) (string, int) {
	n := len(s)
	var b strings.Builder
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			return b.String(), i
		case c == '|' || c == '&' || c == ';' || c == '<' || c == '>' || c == '(' || c == ')':
			return b.String(), i
		case c == '\'':
			i++
			for i < n && s[i] != '\'' {
				b.WriteByte(s[i])
				i++
			}
			if i < n {
				i++ // closing quote
			}
		case c == '"':
			i++
			for i < n && s[i] != '"' {
				if s[i] == '\\' && i+1 < n && (s[i+1] == '"' || s[i+1] == '\\') {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if i < n {
				i++ // closing quote
			}
		case c == '\\' && i+1 < n:
			b.WriteByte(s[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i
}

// lexRedirect parses one redirect operator starting at s[i] (a `<`, `>`, or the `&` of
// `&>`), with fd being any leading file-descriptor digits already consumed (e.g. the
// "2" of `2>&1`). It greedily absorbs the operator plus an inline fd-duplication
// target (`&1`, `&-`); expectsOperand is true only when the redirect still needs a
// following file word (the bare `>`, `>>`, `2>`, `&>`, `>&file` forms).
func lexRedirect(s string, i int, fd string) (shellToken, int) {
	n := len(s)
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	var b strings.Builder
	b.WriteString(fd)
	c := s[i]
	if c == '&' { // &> / &>>
		b.WriteByte('&')
		i++
		if i < n && s[i] == '>' {
			b.WriteByte('>')
			i++
			if i < n && s[i] == '>' {
				b.WriteByte('>')
				i++
			}
		}
		return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: true}, i
	}
	if c == '<' {
		b.WriteByte('<')
		i++
		for i < n && s[i] == '<' { // << heredoc, <<< herestring
			b.WriteByte('<')
			i++
		}
		if i < n && s[i] == '&' { // <&N fd-dup — self-contained
			b.WriteByte('&')
			i++
			for i < n && isDigit(s[i]) {
				b.WriteByte(s[i])
				i++
			}
			if i < n && s[i] == '-' {
				b.WriteByte('-')
				i++
			}
			return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: false}, i
		}
		return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: true}, i
	}
	// c == '>'
	b.WriteByte('>')
	i++
	if i < n && s[i] == '>' { // >> append
		b.WriteByte('>')
		i++
	}
	if i < n && s[i] == '&' { // >&N fd-dup (self-contained) or >&file (needs operand)
		b.WriteByte('&')
		i++
		if i < n && (isDigit(s[i]) || s[i] == '-') {
			for i < n && isDigit(s[i]) {
				b.WriteByte(s[i])
				i++
			}
			if i < n && s[i] == '-' {
				b.WriteByte('-')
				i++
			}
			return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: false}, i
		}
		return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: true}, i
	}
	return shellToken{kind: tokRedirect, text: b.String(), expectsOperand: true}, i
}

// allDigits reports whether s is non-empty and all ASCII digits (a redirect fd prefix).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pushRefSetOptions are the long options that publish a SET of refs the command does not name.
// follow-tags is one: it adds every annotated tag that points into the pushed history, so it
// publishes tags exactly as --tags does, and a tag push can start a release. Refusing --tags and
// allowing --follow-tags would make this a policy about how tags are spelled, not about tags.
var pushRefSetOptions = []string{"all", "mirror", "tags", "branches", "follow-tags"}

// pushRefSetOption says which ref-set option a push argument is, "" for none. Git accepts any
// unambiguous abbreviation of a long option, so `--tag` is `--tags` and `--mir` is `--mirror`;
// matching only the full spellings read those pushes as naming no such option and judged them by
// HEAD. A token that is a prefix of one of these is read as that option even where git would call
// it ambiguous and refuse to run: that can only over-refuse a command that does not run anyway.
// An `=value` is ignored.
func pushRefSetOption(arg string) string {
	name, ok := strings.CutPrefix(arg, "--")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return ""
	}
	for _, o := range pushRefSetOptions {
		if strings.HasPrefix(o, name) {
			return o
		}
	}
	return ""
}

// firstPushRefSetOption is the first ref-set option among args, "" when there is none.
func firstPushRefSetOption(args []string) string {
	for _, a := range args {
		if o := pushRefSetOption(a); o != "" {
			return o
		}
	}
	return ""
}

// gitConfigValues reads every value of a git config key in repo ("" for the process cwd), nil
// when it is unset. A var so tests need no real repo.
var gitConfigValues = func(repo, key string) []string {
	full := []string{"config", "--get-all", key}
	if repo != "" {
		full = append([]string{"-C", repo}, full...)
	}
	data, err := exec.Command("git", full...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// pushLongOptions are the long options of `git push`, for resolving an abbreviation as git
// resolves it: an exact name wins, else a unique prefix. A negated form (`--no-verify`) is its own entry.
var pushLongOptions = []string{
	"all", "atomic", "branches", "delete", "dry-run", "exec", "follow-tags", "force", "force-if-includes",
	"force-with-lease", "help", "ipv4", "ipv6", "mirror", "no-atomic", "no-force-if-includes",
	"no-force-with-lease", "no-recurse-submodules", "no-signed", "no-thin", "no-verify", "porcelain",
	"progress", "prune", "push-option", "quiet", "receive-pack", "recurse-submodules", "repo",
	"set-upstream", "signed", "tags", "thin", "verbose", "verify",
}

// pushValueOptions are the long options that take a value, which git accepts as the NEXT word as well
// as after an `=`. The value is not the remote: `git push -o ci.skip` publishes to origin, and reading
// `ci.skip` as the remote would look every remote-keyed setting (mirror, remote.<r>.push) up on a
// remote git does not use.
//
// `repo` is deliberately absent: its value IS the remote (parsePushArgs reads it as one), so it is
// not something to discard. `recurse-submodules` requires its mode (check|on-demand|no) and takes
// it as the next word like the rest.
var pushValueOptions = map[string]bool{"push-option": true, "receive-pack": true, "exec": true, "recurse-submodules": true}

// pushOptionTakesNextWord reports whether arg is a push option whose value is the following word:
// the short `-o` when it is the last character of its cluster (`-o x`, `-uo x`; with letters after
// it the rest of the cluster is its value, `-ox`, `-ohello`, `-ono`, and no word is taken), or a long
// value option, written in full or as a unique abbreviation, with no `=`. An abbreviation that is
// ambiguous makes git refuse to run, so it can be read either way; one that is a prefix of a boolean
// option as well as of a value option is not consumed, because swallowing a real remote or
// refspec would be the worse mistake.
func pushOptionTakesNextWord(arg string) bool {
	if name, long := strings.CutPrefix(arg, "--"); long {
		if name == "" || strings.Contains(name, "=") {
			return false
		}
		return pushValueOptions[resolvePushLongOption(name)]
	}
	// A short cluster: `-o` is the only short option with a value, and it takes the REST of the cluster
	// as that value (`-ohello`, `-uohello`, `-ono`). Only when the first `o` is the last character is
	// the value the next word (`-o x`, `-uo x`).
	if !strings.HasPrefix(arg, "-") || len(arg) < 2 {
		return false
	}
	flags := arg[1:]
	o := strings.IndexByte(flags, 'o')
	return o >= 0 && o == len(flags)-1
}

// resolvePushLongOption resolves a long option written in full or as a unique abbreviation to its
// full name, as git does: an exact name wins, else the one option it is a prefix of. "" when it is
// ambiguous (git refuses to run) or unknown.
func resolvePushLongOption(name string) string {
	var matches []string
	for _, o := range pushLongOptions {
		if o == name {
			return o
		}
		if strings.HasPrefix(o, name) {
			matches = append(matches, o)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// pushRepoOption reports whether arg is `--repo` (in full or as a unique abbreviation) and, for the
// `--repo=<repository>` form, the repository it carries. The separate-word form leaves value ""
// and inline false: the caller takes the next word. `--repo=<repository>` is equivalent to the
// <repository> argument, so its value is the remote.
func pushRepoOption(arg string) (value string, inline, isRepo bool) {
	name, long := strings.CutPrefix(arg, "--")
	if !long || name == "" {
		return "", false, false
	}
	name, value, inline = strings.Cut(name, "=")
	if resolvePushLongOption(name) != "repo" {
		return "", false, false
	}
	return value, inline, true
}

// parsePushArgs splits a git-push invocation's arguments (everything after "push")
// into the remote and its refspecs, treating any "-..." token as a flag to skip
// (force forms included — force-push follows the same whitelist, it isn't special),
// together with the next word when the option takes its value there, and
// noting --tags (or an abbreviation of it) separately. `--repo <repository>` (or `=`) is the
// remote when no positional one is given. A leading "+" on a refspec
// (another force marker) is NOT treated as a flag — it's part of the refspec, handled by
// targetsFromRefspecs.
func parsePushArgs(args []string) (remote string, refspecs []string, tagsFlag bool) {
	var positional []string
	var repoOpt string // the remote `--repo` names, used when no positional remote is given
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case pushRefSetOption(a) == "tags":
			tagsFlag = true
		case strings.HasPrefix(a, "-"):
			if value, inline, isRepo := pushRepoOption(a); isRepo {
				if !inline && i+1 < len(args) {
					i++
					value = args[i]
				}
				repoOpt = value
				continue
			}
			// A flag (--force, -f, --force-with-lease[=...], -u, --dry-run, ...) —
			// none change target resolution, so a blanket skip is enough. An option whose value is
			// the next word takes that word with it.
			if pushOptionTakesNextWord(a) && i+1 < len(args) {
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return repoOpt, nil, tagsFlag
	}
	// A positional repository takes precedence over --repo, as it does in git.
	return positional[0], positional[1:], tagsFlag
}

// targetsFromRefspecs converts refspecs (positional args after the remote) into the
// branch/tag names to check. Handles a bare branch name, a leading "+" force marker,
// "src:dst" (target = dst; "src:" — a delete — targets src, the ref being acted on),
// and refs/heads/ or refs/tags/ prefixes.
func targetsFromRefspecs(refspecs []string) []pushTarget {
	out := make([]pushTarget, 0, len(refspecs))
	for _, r := range refspecs {
		r = strings.TrimPrefix(r, "+")
		name := r
		if colon := strings.Index(r, ":"); colon >= 0 {
			dst := r[colon+1:]
			if dst == "" {
				dst = r[:colon]
			}
			name = dst
		}
		isTag := strings.HasPrefix(name, "refs/tags/")
		name = strings.TrimPrefix(name, "refs/tags/")
		name = strings.TrimPrefix(name, "refs/heads/")
		out = append(out, pushTarget{Name: name, IsTag: isTag})
	}
	return out
}

// matchesWhitelist reports whether target matches any pattern in whitelist. "*"
// matches any sequence INCLUDING "/" — NOT filepath.Match (which stops at "/"), a
// plain pattern->regex translation instead.
func matchesWhitelist(target string, whitelist []string) bool {
	for _, pattern := range whitelist {
		if globCrossSlash(pattern, target) {
			return true
		}
	}
	return false
}

// MatchWhitelist is matchesWhitelist, exported: the exact `*`-crosses-`/` semantics
// push-guard enforces, for callers OUTSIDE this package that must never disagree with
// the real guard about whether a target matches (e.g. internal/doctor's
// branch-whitelist preflight warning) — reuse this rather than reimplementing glob
// matching, or the warning can drift from what push-guard actually decides.
func MatchWhitelist(target string, whitelist []string) bool {
	return matchesWhitelist(target, whitelist)
}

func globCrossSlash(pattern, s string) bool {
	quoted := regexp.QuoteMeta(pattern)
	re := "^" + strings.ReplaceAll(quoted, regexp.QuoteMeta("*"), ".*") + "$"
	matched, err := regexp.MatchString(re, s)
	return err == nil && matched
}

// loggedNotice tells the agent the deny is not a dead end it can quietly retry past —
// it's a recorded security event. Appended to both deny reasons below.
const loggedNotice = " This attempt has been logged for review."

// guardModeNote is the trailing clause every security-guard deny appends so logs/doctor
// (and the agent) can see whether the deny was advisory or enforcing. push-guard's
// whitelist-miss and unverifiable denies are authoritative in BOTH modes (it fail-closes
// regardless), so the note is informational here — it reports the resolved posture.
func guardModeNote(mode GuardMode) string {
	return " [push-guard mode: " + mode.String() + "]"
}

func pushGuardDeny(target string, whitelist []string, mode GuardMode) Result {
	return jsonDeny(fmt.Sprintf(
		"Push to `%s` denied — not on the push whitelist (%s). Protected branches (master/main/develop/release/*) and tags receive changes only via the gated PR flow. Force-push follows the same whitelist. If this branch should be pushable, add its pattern to push_whitelist.%s%s%s",
		target, strings.Join(whitelist, ", "), refSetAdvice(target), loggedNotice, guardModeNote(mode),
	))
}

// refSetAdvice says what to do instead when the denied target is an option that publishes refs the
// command does not name, since "add it to the whitelist" is not an answer to a flag.
func refSetAdvice(target string) string {
	switch target {
	case "--tags", "--follow-tags", "push.followTags":
		return " `" + target + "` publishes tags the command does not name, and a tag push can start a release. Push the branch without it (`--no-follow-tags` overrides the config), and push a tag deliberately as its own command naming it, which is judged like any other tag push."
	case "--all", "--mirror", "--branches":
		return " `" + target + "` publishes every branch, protected ones included. Name the branch you mean: `git push <remote> <branch>`."
	}
	return ""
}

func pushGuardFailClosed(mode GuardMode) Result {
	return jsonDeny("Push target could not be verified against the whitelist (git exec failed, an unparseable refspec, or an unresolvable repo path) — denied. A push we can't prove is safe is treated as unsafe. Re-run with an explicit whitelisted branch, e.g. `git push origin feat/<branch>`." + loggedNotice + guardModeNote(mode))
}
