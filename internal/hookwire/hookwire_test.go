package hookwire

import (
	"os"
	"path/filepath"
	"testing"
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func allWiredHooksJSON() string {
	return `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "SendMessage", "hooks": [
        {"type":"command","command":"leadkit hook brief-length"},
        {"type":"command","command":"leadkit hook milestone-backstop"}
      ]},
      {"matcher": "Bash", "hooks": [
        {"type":"command","command":"leadkit hook pr-guard"},
        {"type":"command","command":"leadkit hook queue-guard"},
        {"type":"command","command":"leadkit hook usage-logger"},
        {"type":"command","command":"leadkit hook push-guard"},
        {"type":"command","command":"leadkit hook push-gate"}
      ]},
      {"matcher": "TaskCreate|TaskUpdate", "hooks": [{"type":"command","command":"/opt/bin/leadkit hook task-mechanism"}]},
      {"matcher": "Edit|Write|MultiEdit", "hooks": [{"type":"command","command":"leadkit hook task-status-guard"}]},
      {"matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": [{"type":"command","command":"leadkit hook owner-guard"}]},
      {"matcher": "Bash|Agent|Task", "hooks": [{"type":"command","command":"leadkit hook routing-inbox"}]}
    ]
  }
}`
}

func TestLoadHooks_MissingFileIsNotAnError(t *testing.T) {
	hooks, found, err := LoadHooks(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil || found || hooks != nil {
		t.Errorf("LoadHooks(missing) = (%v, %v, %v), want (nil, false, nil)", hooks, found, err)
	}
}

func TestLoadHooks_MalformedFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mustWriteFile(t, path, `{"hooks": not valid json`)
	_, found, err := LoadHooks(path)
	if err == nil || !found {
		t.Errorf("LoadHooks(malformed) = found=%v err=%v, want found=true err!=nil", found, err)
	}
}

func TestEvaluateWiring_AllWired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mustWriteFile(t, path, allWiredHooksJSON())
	hooks, _, err := LoadHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	statuses := EvaluateWiring(hooks)
	if len(statuses) != len(ExpectedHooks) {
		t.Fatalf("got %d statuses, want %d", len(statuses), len(ExpectedHooks))
	}
	for _, s := range statuses {
		if !s.Wired || s.Misconfigured() {
			t.Errorf("hook %s = %+v, want fully wired", s.Name, s)
		}
	}
}

func TestEvaluateWiring_SomeMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mustWriteFile(t, path, `{"hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"leadkit hook pr-guard"}]}
	]}}`)
	hooks, _, err := LoadHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	statuses := EvaluateWiring(hooks)
	byName := map[string]WireStatus{}
	for _, s := range statuses {
		byName[s.Name] = s
	}
	if !byName["pr-guard"].Wired {
		t.Errorf("pr-guard should be wired, got %+v", byName["pr-guard"])
	}
	if byName["push-guard"].Wired || byName["push-guard"].Found {
		t.Errorf("push-guard should be entirely missing, got %+v", byName["push-guard"])
	}
}

func TestEvaluateWiring_WrongMatcherIsMisconfiguredNotMissing(t *testing.T) {
	hooks := map[string][]MatcherGroup{
		"PreToolUse": {
			{Matcher: "Edit", Hooks: []HookCommand{{Type: "command", Command: "leadkit hook push-guard"}}},
		},
	}
	statuses := EvaluateWiring(hooks)
	for _, s := range statuses {
		if s.Name != "push-guard" {
			continue
		}
		if s.Wired {
			t.Errorf("push-guard under matcher=Edit should not count as wired: %+v", s)
		}
		if !s.Found || !s.Misconfigured() {
			t.Errorf("push-guard under the wrong matcher should be Found+Misconfigured, got %+v", s)
		}
	}
}

func TestEvaluateWiring_TaskMechanismNeedsBothTaskTools(t *testing.T) {
	hooks := map[string][]MatcherGroup{
		"PreToolUse": {
			{Matcher: "TaskCreate", Hooks: []HookCommand{{Type: "command", Command: "leadkit hook task-mechanism"}}},
		},
	}
	statuses := EvaluateWiring(hooks)
	for _, s := range statuses {
		if s.Name == "task-mechanism" && s.Wired {
			t.Errorf("task-mechanism under matcher=TaskCreate only (missing TaskUpdate) should not be Wired: %+v", s)
		}
	}
}

func TestEvaluateWiring_StarMatcherCoversEverything(t *testing.T) {
	hooks := map[string][]MatcherGroup{
		"PreToolUse": {
			{Matcher: "*", Hooks: []HookCommand{{Type: "command", Command: "leadkit hook push-guard"}}},
		},
	}
	statuses := EvaluateWiring(hooks)
	for _, s := range statuses {
		if s.Name == "push-guard" && !s.Wired {
			t.Errorf("push-guard under matcher=* should be wired: %+v", s)
		}
	}
}

func TestEvaluateWiring_ToleratesAbsolutePathToBinary(t *testing.T) {
	hooks := map[string][]MatcherGroup{
		"PreToolUse": {
			{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "/Users/matt/go/bin/leadkit hook push-guard"}}},
		},
	}
	statuses := EvaluateWiring(hooks)
	for _, s := range statuses {
		if s.Name == "push-guard" && !s.Wired {
			t.Errorf("push-guard via an absolute binary path should be wired: %+v", s)
		}
	}
}

func TestEvaluateWiring_ToleratesAbsolutePathToDifferentlyNamedBinary(t *testing.T) {
	// The real deployed binary is named "leadkit-bin" (ONBOARDING.md's deployment
	// note), not "leadkit" — detection must not require the literal name "leadkit"
	// immediately before "hook", only the trailing `hook <subcommand>`.
	hooks := map[string][]MatcherGroup{
		"PreToolUse": {
			{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "/Users/matt/.claude/leadkit-bin hook push-guard"}}},
		},
	}
	statuses := EvaluateWiring(hooks)
	for _, s := range statuses {
		if s.Name == "push-guard" && !s.Wired {
			t.Errorf("push-guard via an absolute leadkit-bin path should be wired: %+v", s)
		}
	}
}

func TestInvokes_CanonicalizesBareAndAbsoluteAsTheSameHook(t *testing.T) {
	cases := []string{
		"leadkit hook push-guard",
		"/Users/matt/go/bin/leadkit hook push-guard",
		"/Users/matt/.claude/leadkit-bin hook push-guard",
	}
	for _, c := range cases {
		if !Invokes(c, "push-guard") {
			t.Errorf("Invokes(%q, push-guard) = false, want true", c)
		}
	}
	if Invokes("leadkit hook queue-guard", "push-guard") {
		t.Error("Invokes must not match a different hook subcommand")
	}
	if Invokes("bash $HOME/.claude/hooks/push-guard.sh", "push-guard") {
		t.Error("Invokes must not match a bash script whose filename merely contains the hook name")
	}
}

func TestMergeHooks_UnionsAcrossFiles(t *testing.T) {
	a := map[string][]MatcherGroup{
		"PreToolUse": {{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "leadkit hook pr-guard"}}}},
	}
	b := map[string][]MatcherGroup{
		"PreToolUse": {{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: "leadkit hook push-guard"}}}},
	}
	merged := MergeHooks(a, b)
	statuses := EvaluateWiring(merged)
	for _, s := range statuses {
		if (s.Name == "pr-guard" || s.Name == "push-guard") && !s.Wired {
			t.Errorf("%s should be wired after merge: %+v", s.Name, s)
		}
	}
}
