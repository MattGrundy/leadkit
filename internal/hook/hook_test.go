package hook

import (
	"testing"

	"mgpp.dev/leadkit/internal/audit"
)

var testWorkish = audit.WorkishConfig{DescriptionLengthThreshold: 280, Markers: []string{"##", "```", "acceptance", "DoD"}}

var testConfig = Config{
	QueueBase: "queue",
	LogPath:   "/tmp/does-not-matter.jsonl",
	Workish:   testWorkish,
	PushGuard: PushGuardConfig{EffectiveWhitelist: func(repoPath, origin string) []string { return []string{"feat/*"} }},
}

func TestDispatch_MalformedJSONFailsOpen(t *testing.T) {
	res := Dispatch(NameBriefLength, []byte("not json at all"), testConfig)
	if res != allow {
		t.Errorf("Dispatch(malformed json) = %+v, want the silent allow result", res)
	}
}

func TestDispatch_EmptyStdinFailsOpen(t *testing.T) {
	res := Dispatch(NamePRGuard, []byte(""), testConfig)
	if res != allow {
		t.Errorf("Dispatch(empty stdin) = %+v, want the silent allow result", res)
	}
}

func TestDispatch_UnknownNameFailsOpen(t *testing.T) {
	res := Dispatch("not-a-real-hook", []byte(`{"tool_name":"Bash"}`), testConfig)
	if res != allow {
		t.Errorf("Dispatch(unknown name) = %+v, want the silent allow result", res)
	}
}

func TestDispatch_RoutesToTheNamedHook(t *testing.T) {
	// A sanity check that Dispatch actually wires the name to the right function —
	// pr-guard on a raw `gh pr create` must produce the deny JSON, not brief-length's
	// stderr-block style.
	res := Dispatch(NamePRGuard, []byte(`{"tool_name":"Bash","tool_input":{"command":"gh pr create --title x"}}`), testConfig)
	if res.ExitCode != 0 || res.Stdout == "" || res.Stderr != "" {
		t.Errorf("Dispatch(pr-guard, raw gh pr create) = %+v, want a stdout-JSON deny with exit 0", res)
	}
}

func TestDispatch_RoutesToTaskMechanism(t *testing.T) {
	longDesc := ""
	for i := 0; i < 300; i++ {
		longDesc += "x"
	}
	res := Dispatch(NameTaskMechanism, []byte(`{"tool_name":"TaskCreate","tool_input":{"subject":"s","description":"`+longDesc+`"}}`), testConfig)
	if res.ExitCode != 2 || res.Stderr == "" {
		t.Errorf("Dispatch(task-mechanism, workish TaskCreate) = %+v, want an exit-2 stderr block", res)
	}
}

func TestDispatch_RoutesToPushGuard(t *testing.T) {
	// push-guard always execs git to resolve the origin remote (for the per-repo
	// whitelist override), even here where the refspec is explicit — stub it so this
	// test's outcome never depends on the ambient git state of wherever `go test`
	// happens to run (this package's own dedicated push_guard_test.go always does
	// the same; this is Dispatch's own routing test, not a push-guard behavior test,
	// so it just needs runGit to fail cleanly rather than hit real git).
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) { return "", false }
	defer func() { runGit = orig }()

	res := Dispatch(NamePushGuard, []byte(`{"tool_name":"Bash","tool_input":{"command":"git push origin master"}}`), testConfig)
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Errorf("Dispatch(push-guard, push to master) = %+v, want a stdout-JSON deny with exit 0", res)
	}
}
