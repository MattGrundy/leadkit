package hook

import (
	"path"
	"regexp"
	"strings"
)

// CANONICAL SOURCE: ~/.claude/hooks/queue-mv-guard.sh. Keep this in sync by hand.
//
// `leadkit queue move`/`leadkit queue sweep` are the only sanctioned movers. The legacy
// bash scripts are gone and are not recognised: a name that refers to nothing can only
// ever be used to switch the guard off.

// queueVerbPattern matches a bare mv/ln/rm token, mirroring the bash's
// `(^|[^-A-Za-z])(mv|ln|rm)([[:space:]])` — a flag like `-rm` or a longer word ending
// in one of these must not trigger.
var queueVerbPattern = regexp.MustCompile(`(^|[^-A-Za-z])(mv|ln|rm)\s`)

// queueTriggerPattern reports whether cmd references the queue in the shape the bash
// guards: "<queueBase>/<lane>/", where lane keeps the bash's lane-prefix intent
// (cr-/crit-/ov-/finalise/done/lowercase-anything). Unanchored, like the bash's own
// `(<project>/)?queue/...` — that optional literal project-name prefix never actually
// required anything (it's optional with no anchor either side), so dropping the
// hardcoded project name here is exactly equivalent, just now portable to any project
// via its own queue_root basename.
//
// The leading `(^|[^A-Za-z0-9_.-])` requires the base to sit at a PATH-SEGMENT start —
// string start, or after a `/`, whitespace, `=`, a quote, etc. — never mid-word. Without
// it the base matched as a bare substring, so a commit message like "requeue/route
// terminal/review" contained "queue/route…terminal/" and false-triggered. Now
// "requeue/route", "dequeue/x", "myqueue/y", "keyboard/" don't match, while "queue/cr-1/",
// "../queue/devops/" and "cd migrate/queue/finalise/" still do.
func queueTriggerPattern(queueBase string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(queueBase) + `/(cr-|crit-|ov-|finalise|done|[a-z].*)/`)
}

// clauseSeparatorPattern splits a compound command into individual clauses on the shell
// control operators `;`, `&&`, `||`, `|`, `&`, and newline. A raw mv/ln/rm is only a queue
// mutation when the verb and the queue path live in the SAME clause; an unrelated
// `rm -f /tmp/x` in a different clause of `git commit -m "…requeue…" && rm -f /tmp/x` must
// not count. `&&`/`||` come first in the alternation so they win over the single-char `&`/`|`.
var clauseSeparatorPattern = regexp.MustCompile(`&&|\|\||[;&|\n]`)

// sanctionedClause reports whether the COMMAND a clause runs is a sanctioned queue mover:
// `leadkit queue move|sweep ...` (optionally behind leading VAR=val assignments, and with
// the binary named leadkit or leadkit-bin, bare or by path). It identifies the invoked
// command, not a name somewhere in the line: `rm queue/x/ # leadkit queue move` and
// `echo leadkit queue move; rm queue/x/` run rm, so they are not sanctioned.
//
// A clause that also contains a command or process substitution is never sanctioned, since
// `leadkit queue move a b $(rm queue/x/y/)` runs an rm inside a sanctioned-looking line.
// `=(` is zsh's process substitution into a temp file: it executes like `<(`, and zsh is a
// shell this guard really sees.
// The match is by name, not by path: this guard stops accidents and the obvious bypass; it
// is not a sandbox against a hostile binary called leadkit.
func sanctionedClause(clause string) bool {
	if strings.Contains(clause, "$(") || strings.Contains(clause, "`") ||
		strings.Contains(clause, "<(") || strings.Contains(clause, ">(") ||
		strings.Contains(clause, "=(") {
		return false
	}
	words := strings.Fields(clause)
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) < 3 {
		return false
	}
	bin := path.Base(strings.Trim(words[0], `"'`))
	if bin != "leadkit" && bin != "leadkit-bin" {
		return false
	}
	return words[1] == "queue" && (words[2] == "move" || words[2] == "sweep")
}

// isAssignment reports whether w is a leading shell VAR=value word.
func isAssignment(w string) bool {
	k, _, ok := strings.Cut(w, "=")
	if !ok || k == "" {
		return false
	}
	for i, r := range k {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// defaultQueueBase is used when queueBase is empty or unusable (e.g. "." or "/") —
// fails open to the conventional name rather than building a garbage pattern.
const defaultQueueBase = "queue"

// QueueGuard is queue-mv-guard.sh: deny a raw mv/ln/rm against the queue overlay
// unless the clause doing the mutation IS a sanctioned `leadkit queue move`/`sweep`.
func QueueGuard(in Input, queueBase string) Result {
	cmd := in.ToolInput.Command
	if cmd == "" {
		return allow
	}
	if queueBase == "" || queueBase == "." || queueBase == "/" {
		queueBase = defaultQueueBase
	}
	trigger := queueTriggerPattern(queueBase)
	// A queue mutation requires a SINGLE clause that BOTH invokes a raw mv/ln/rm verb AND
	// names the queue path. Splitting on clause separators keeps a cross-clause coincidence
	// — a commit message mentioning the base in one clause and an unrelated temp-file rm in
	// another — from false-triggering, while a real `mv <base>/cr-1/… <base>/cr-1/…` (verb
	// and path in the same clause) still trips.
	//
	// Sanction is judged per clause for the same reason: a sanctioned mover in one clause
	// says nothing about an rm in the next.
	for _, clause := range clauseSeparatorPattern.Split(cmd, -1) {
		if queueVerbPattern.MatchString(clause) && trigger.MatchString(clause) && !sanctionedClause(clause) {
			return Result{Stderr: queueGuardGuidance, ExitCode: 2}
		}
	}
	return allow
}

const queueGuardGuidance = `BLOCKED: raw mv/ln/rm on the queue hangs on an approval prompt in autonomous mode and bypasses status sync.
Use the leadkit verbs instead:
  leadkit queue move [--dry-run] <src> <dst>   (move a queue item between stages)
  leadkit review send --task <t> --repo <r>    (enqueue a gate)
  leadkit gate verdict --ticket <t> --verdict <ready|changes-needed>   (record a review verdict + advance stage)
  leadkit queue assign [--dry-run] <ticket-number> <role>   (assign backlog)
  leadkit queue sweep                          (report/poke stalled inbox entries)
`
