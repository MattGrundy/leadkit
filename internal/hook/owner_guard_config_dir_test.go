package hook

import (
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/owners"
)

// Ticket 138. The file granting merge authority lives in the account's leadkit config directory,
// outside the repo and so outside the review gate. An agent write to it is refused whatever the
// project's owners policy says, including no policy at all.
func TestOwnerGuard_AgentWriteToTheAccountConfigDirIsRefused(t *testing.T) {
	const grant = ".claude/leadkit/config/leadkit.yaml"
	policies := map[string]func(string) (*owners.Policy, bool){
		"no resolver":           nil,
		"policy not loaded":     func(string) (*owners.Policy, bool) { return nil, false },
		"policy that owns it":   staticPolicy(&owners.Policy{Paths: []owners.PathOwner{{Glob: ".claude/**", Owner: "fe-agent"}}}),
		"policy without blocks": staticPolicy(&owners.Policy{}),
	}
	for name, resolve := range policies {
		for _, caller := range []string{"fe-agent", "lead", "anyone"} {
			cfg := OwnerGuardConfig{ResolvePolicy: resolve, Caller: caller, Lead: "lead", Home: "/home/me", Pwd: "/home/me"}
			for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
				in := Input{ToolName: tool, ToolInput: toolInput{FilePath: "/home/me/" + grant}}
				got := OwnerGuard(in, cfg)
				if got.ExitCode != 2 || !strings.Contains(got.Stderr, grant) {
					t.Errorf("%s / %s / %s: want a refusal naming %s, got %+v", name, caller, tool, grant, got)
				}
			}
			// Not only the grant file: anything the directory holds, at any depth (a team layer, a second profile).
			for _, other := range []string{".claude/leadkit/config/other.yaml", ".claude/leadkit/config/sub/team.yaml"} {
				if got := OwnerGuard(Input{ToolName: "Write", ToolInput: toolInput{FilePath: "/home/me/" + other}}, cfg); got.ExitCode != 2 {
					t.Errorf("%s / %s: %s must be refused too, got %+v", name, caller, other, got)
				}
			}
			rm := Input{ToolName: "Bash", ToolInput: toolInput{Command: "rm /home/me/" + grant}}
			if got := OwnerGuard(rm, cfg); got.ExitCode != 2 {
				t.Errorf("%s / %s: rm of the grant file must be refused, got %+v", name, caller, got)
			}
		}
	}

	// CONTROLS: neighbours of the directory are untouched, so the rule is not "refuse everything".
	cfg := OwnerGuardConfig{Caller: "lead", Lead: "lead", Home: "/home/me", Pwd: "/home/me"}
	for _, ok := range []string{
		".claude/leadkit/config-history/leadkit/x.yaml",
		".claude/leadkit/configx/y.yaml",
		".claude/leadkit/state.json",
		".claude/settings.json",
		"Projects/repo/leadkit.yaml",
	} {
		if got := OwnerGuard(Input{ToolName: "Write", ToolInput: toolInput{FilePath: "/home/me/" + ok}}, cfg); got != allow {
			t.Errorf("%s must stay writable, got %+v", ok, got)
		}
	}

	// The lead's .bypass escape hatch lifts it, as for every hard block.
	cfg.Bypass = true
	if got := OwnerGuard(Input{ToolName: "Write", ToolInput: toolInput{FilePath: "/home/me/" + grant}}, cfg); got != allow {
		t.Errorf("an authorised edit under .bypass must pass, got %+v", got)
	}
}
