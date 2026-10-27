package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
)

// A refusal of a raw forge verb says what the durable fix is, so it teaches instead of only
// blocking (ticket 219). The specific denies (draft-required, owner mismatch) are about a
// granted repo and do not carry it.
func TestPRGuard_RefusalNamesTheScopedTokenAsTheDurableFix(t *testing.T) {
	none := &config.Profile{Authority: map[string]config.AuthoritySpec{}}
	for name, c := range map[string]struct{ origin, cmd string }{
		"github create": {"git@github.com:o/r.git", "gh pr create --title x"},
		"github edit":   {"git@github.com:o/r.git", "gh pr edit 1 --title x"},
		"github merge":  {"git@github.com:o/r.git", "gh pr merge 1"},
		"gitlab create": {"git@gitlab.com:o/r.git", "glab mr create --title x"},
		"gitlab update": {"git@gitlab.com:o/r.git", "glab mr update 1"},
		"gitlab merge":  {"git@gitlab.com:o/r.git", "glab mr merge 1"},
	} {
		t.Run(name, func(t *testing.T) {
			stubOrigin(t, c.origin)
			reason := decodeDeny(t, PRGuard(Input{ToolInput: toolInput{Command: c.cmd}}, none, "/repo"))
			for _, w := range []string{"fine-grained token", "leadkit doctor", "PORTABLE-OPERATING-KIT.md"} {
				if !strings.Contains(reason, w) {
					t.Errorf("the refusal does not mention %q:\n%s", w, reason)
				}
			}
		})
	}
	t.Run("an adopter's fallback hint keeps its place before the note", func(t *testing.T) {
		stubOrigin(t, "git@github.com:o/r.git")
		prof := &config.Profile{PRFallbackHint: "Legacy helper: ~/x/pr.sh"}
		reason := decodeDeny(t, PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --title x"}}, prof, "/repo"))
		if !strings.Contains(reason, "Legacy helper") || strings.Index(reason, "Legacy helper") > strings.Index(reason, "fine-grained token") {
			t.Errorf("%s", reason)
		}
	})
}
