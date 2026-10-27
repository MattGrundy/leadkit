package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Ticket 198 through the real binary: the repository-level refusal is reachable as `leadkit hook pr-guard`,
// the command the installed settings wire, and a read is not refused.
func TestCLI_PRGuard_RefusesRepositoryCreationAndNotReading(t *testing.T) {
	bin := testBinary(t)
	home := t.TempDir()
	run := func(command string) string {
		t.Helper()
		cmd := exec.Command(bin, "hook", "pr-guard")
		cmd.Dir = home
		cmd.Env = sandboxedEnv(home, nil)
		cmd.Stdin = strings.NewReader(`{"tool_name":"Bash","cwd":"` + home + `","tool_input":{"command":"` + command + `"}}`)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hook pr-guard %q: %v\n%s", command, err, out)
		}
		return string(out)
	}
	for _, c := range []string{"gh repo create throwaway --private", "gh repo delete acme/x --yes", "gh api -X POST user/repos -f name=x", "glab repo create x"} {
		if out := run(c); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "owner-direct") {
			t.Errorf("%q must be refused as owner-direct, got: %s", c, out)
		}
	}
	for _, c := range []string{"gh repo view", "gh repo list acme", "gh api user/repos"} {
		if out := strings.TrimSpace(run(c)); out != "" {
			t.Errorf("%q is a read and must be allowed silently, got: %s", c, out)
		}
	}
}
