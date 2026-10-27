package prcreate

import (
	"fmt"
	"strings"
)

// allowedPassthrough is, per governed verb, the forge flags a caller may append after `--`.
// It is an ALLOWLIST and it is EMPTY on purpose: each verb composes its own argv from a fixed set
// and anything else is refused, so a flag the forge CLI gains tomorrow is unreachable by default.
//
// Why empty and not "the harmless ones": the passthrough sits after the flags the governance
// decision was made on, so a later flag overrides them. A repeated -R/--repo retargets the call at
// a repository the authority entry never covered (the last value wins), gh pr edit's --attach
// uploads a local file, and gh pr merge's --admin merges past the protections a gate is meant to
// respect. Nothing the governed verbs need is only reachable this way. To admit a flag, add it
// here with a test that shows the decision still holds with it present.
var allowedPassthrough = map[string]map[string]bool{
	"create": {},
	"edit":   {},
	"merge":  {},
}

// refusePassthrough returns a refusal reason when extra contains anything outside the verb's
// allowlist, else "". Every token is judged, flag or positional: a bare word could be a second
// argument the forge CLI interprets, and `--flag=value` is judged by its flag name.
func refusePassthrough(verb string, extra []string) string {
	allowed := allowedPassthrough[verb]
	for _, tok := range extra {
		name, _, _ := strings.Cut(tok, "=")
		if allowed[name] {
			continue
		}
		return fmt.Sprintf("%q is not on the passthrough allowlist for pr %s (the governed verb builds its own flags and accepts none after `--`: "+
			"a trailing flag can override the repo, base or merge options the decision was made on)", tok, verb)
	}
	return ""
}
