package hook

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CANONICAL SOURCE: ~/.claude/hooks/brief-length-guard.sh. Keep this in sync by hand.

// DefaultBriefMaxChars mirrors brief-length-guard.sh's MAX_CHARS default.
const DefaultBriefMaxChars = 1400

// briefLengthMaxChars resolves the effective limit: BRIEF_GUARD_MAX_CHARS (the
// bash's own ${BRIEF_GUARD_MAX_CHARS:-1400} escape hatch, mirrored here) wins if
// set; else cfgMax (the profile's brief_length_max, layered user/team/project —
// see internal/config's PreferenceKeys/Profile.BriefLengthMax) if positive; else
// DefaultBriefMaxChars. An unset/unparsable env var or a zero cfgMax both fall
// through rather than erroring, keeping this hook itself fail-open.
func briefLengthMaxChars(cfgMax int) int {
	if v := os.Getenv("BRIEF_GUARD_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if cfgMax > 0 {
		return cfgMax
	}
	return DefaultBriefMaxChars
}

// BriefLength is brief-length-guard.sh: blocks an over-long SendMessage brief
// unless it carries the explicit "[LONG-BRIEF:" override marker. maxChars is the
// caller's resolved profile value (cmd/leadkit's runHook passes prof.
// BriefLengthMax) — 0 (e.g. a direct/test call with no profile) falls back to
// DefaultBriefMaxChars, same as before this was wired to config.
func BriefLength(in Input, maxChars int) Result {
	if in.ToolName != "SendMessage" {
		return allow
	}
	msg := in.ToolInput.Message
	if msg == "" {
		return allow
	}
	if strings.Contains(msg, "[LONG-BRIEF:") {
		return allow
	}

	max := briefLengthMaxChars(maxChars)
	// Deliberate, documented choice: rune count, not the bash's byte-ish/locale
	// ${#msg} — a rune-accurate limit is the more honest "character" count for
	// non-ASCII briefs, and the two only diverge for multi-byte UTF-8 content.
	length := utf8.RuneCountInString(msg)
	if length <= max {
		return allow
	}

	return Result{Stderr: fmt.Sprintf(briefLengthGuidance, length, max), ExitCode: 2}
}

const briefLengthGuidance = `BLOCKED: agent brief is %d chars, over the %d limit.

Most routing needs three lines: WHAT ticket, WHERE (clone + sha), WHAT to watch out for.
The agent can read the ticket — do not restate it in the message.

Trim it, or if the length is genuinely earned, prefix the message with:
  [LONG-BRIEF: <one-line reason>]

Valid reasons are narrow: reversing your own prior instruction, correcting something that would
otherwise be re-derived wrongly, or re-briefing after an agent died. "Useful context" is not a
reason — that belongs in the ticket, where it persists and the next agent finds it.
`
