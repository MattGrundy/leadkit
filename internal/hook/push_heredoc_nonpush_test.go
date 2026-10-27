package hook

import "testing"

// A heredoc body that does not mention push is not a reason to refuse a push on the same line: the push hooks
// read only the bodies that name it, although the lexer now keeps every body for the readers that need them.
func TestPushGuard_AHeredocBodyThatDoesNotMentionPushIsNotACauseToRefuse(t *testing.T) {
	withGitStub(t, func(repoPath string, args ...string) (string, bool) {
		if len(args) >= 1 && args[0] == "rev-parse" {
			return "feat/ok", true
		}
		return "", false
	})
	wl := PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}
	for _, cmd := range []string{
		"sh <<EOF\ntrue\nEOF\ngit push origin feat/ok",
		"bash <<'EOF'\necho hello\nEOF\ngit push origin feat/ok",
		"python3 <<EOF\nprint(1)\nEOF\ngit push origin feat/ok",
	} {
		assertAllow(t, PushGuard(pushInput(cmd), wl), cmd)
	}
}
