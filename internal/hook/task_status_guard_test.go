package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tsScope is the standard task-status scope for a given store root — the trigger set and
// aliases mirror what taskStatusScope derives from a default profile.
func tsScope(storeRoot string) TaskStatusScope {
	return TaskStatusScope{
		StoreRoot:       storeRoot,
		TriggerStatuses: []string{"completed", "in_review", "awaiting_pr_review"},
		StatusAliases:   map[string]string{"done": "completed", "awaiting_review": "in_review"},
		CanonicalLegend: []string{"pending", "in_progress", "in_review", "awaiting_pr_review", "completed", "dropped"},
	}
}

// tsCfg is the standard task-status-guard config: an injected resolver that returns the
// SAME scope for every target file, driving TaskStatusGuard through the injected Resolve
// seam without a real profile on disk (the real cmd/leadkit resolver derives the scope from
// the TARGET file's own project — see the cross-project test).
func tsCfg(storeRoot string) TaskStatusConfig {
	return TaskStatusConfig{Resolve: func(string) TaskStatusScope { return tsScope(storeRoot) }}
}

// writeTaskFile creates <storeRoot>/<name> with body and returns its path.
func writeTaskFile(t *testing.T, storeRoot, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(storeRoot, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func editInput(filePath, newString string) Input {
	return Input{ToolName: "Edit", ToolInput: toolInput{FilePath: filePath, NewString: newString}}
}

func TestTaskStatusGuard_TerminalWithoutRefDenies(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	p := writeTaskFile(t, store, "t1.md", "---\nstatus: in_progress\n---\nbody\n")
	res := TaskStatusGuard(editInput(p, "---\nstatus: completed\n---\nbody\n"), tsCfg(store))
	if res.ExitCode != 2 {
		t.Fatalf("want DENY (exit 2) for completed with no ref, got %+v", res)
	}
	if res.Stderr == "" || !strings.Contains(res.Stderr, "Canonical statuses") {
		t.Errorf("want guidance with a canonical-statuses legend, got %q", res.Stderr)
	}
}

func TestTaskStatusGuard_PopulatedRefInPayloadAllows(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	sha40 := strings.Repeat("a", 40)
	cases := map[string]string{
		"prs inline list": "---\nstatus: completed\nprs: [748]\n---\n",
		"prs list item":   "---\nstatus: completed\nprs:\n  - 748\n---\n",
		"sha scalar":      "---\nstatus: completed\nsha: " + sha40 + "\n---\n",
		"output scalar":   "---\nstatus: in_review\noutput: design/x.md\n---\n",
		"no_pr_reason":    "---\nstatus: completed\nno_pr_reason: superseded\n---\n",
	}
	for name, payload := range cases {
		p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
		if got := TaskStatusGuard(editInput(p, payload), tsCfg(store)); got != allow {
			t.Errorf("%s: want ALLOW, got %+v", name, got)
		}
	}
}

func TestTaskStatusGuard_RefAlreadyInFileAllows(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	// The file already carries prs:; this edit only flips status and touches no ref.
	p := writeTaskFile(t, store, "t2.md", "---\nstatus: in_progress\nprs: [500]\n---\nbody\n")
	res := TaskStatusGuard(editInput(p, "---\nstatus: completed\nprs: [500]\n---\nbody\n"), tsCfg(store))
	if res != allow {
		t.Errorf("want ALLOW via the two-source (file) check, got %+v", res)
	}
	// Even when the payload itself omits the ref entirely, the file's ref counts.
	res2 := TaskStatusGuard(editInput(p, "status: completed\n"), tsCfg(store))
	if res2 != allow {
		t.Errorf("want ALLOW — ref present in the untouched file, got %+v", res2)
	}
}

func TestTaskStatusGuard_NonTriggeringStatusesAllow(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	p := writeTaskFile(t, store, "t3.md", "---\nstatus: in_progress\n---\n")
	for _, s := range []string{"in_progress", "blocked", "dropped", "pending"} {
		if got := TaskStatusGuard(editInput(p, "status: "+s+"\n"), tsCfg(store)); got != allow {
			t.Errorf("status %q must not trigger, got %+v", s, got)
		}
	}
}

func TestTaskStatusGuard_OutsideStoreRootAllows(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	other := writeTaskFile(t, filepath.Join(t.TempDir(), "docs"), "d.md", "x")
	if got := TaskStatusGuard(editInput(other, "status: completed\n"), tsCfg(store)); got != allow {
		t.Errorf("a file outside store_root is out of scope, got %+v", got)
	}
	// A non-.md file under the store is also out of scope.
	txt := writeTaskFile(t, store, "notes.txt", "x")
	if got := TaskStatusGuard(editInput(txt, "status: completed\n"), tsCfg(store)); got != allow {
		t.Errorf("a non-.md file is out of scope, got %+v", got)
	}
}

func TestTaskStatusGuard_BareOrEmptyRefStillDenies(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	for _, payload := range []string{
		"---\nstatus: completed\nprs:\n---\n",    // bare key
		"---\nstatus: completed\nprs: []\n---\n", // empty list
		"---\nstatus: completed\nprs: [ ]\n---\n",
	} {
		p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
		if got := TaskStatusGuard(editInput(p, payload), tsCfg(store)); got.ExitCode != 2 {
			t.Errorf("empty claim %q must still DENY, got %+v", payload, got)
		}
	}
}

func TestTaskStatusGuard_HonorsStatusAliases(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
	// `done` is an alias of `completed` — must trigger the guard.
	if got := TaskStatusGuard(editInput(p, "status: done\n"), tsCfg(store)); got.ExitCode != 2 {
		t.Errorf("alias `done`->completed must trigger a DENY with no ref, got %+v", got)
	}
	// And an unrelated non-canonical value must NOT trigger (fail open).
	if got := TaskStatusGuard(editInput(p, "status: wibble\n"), tsCfg(store)); got != allow {
		t.Errorf("an unknown status must not trigger, got %+v", got)
	}
}

func TestTaskStatusGuard_ProfileDrivenStoreRoot(t *testing.T) {
	// A non-default store root ("board", not "tasks") must scope correctly against THAT
	// root — proving the guard is profile-driven, not hardcoded to tasks/.
	store := filepath.Join(t.TempDir(), "board")
	p := writeTaskFile(t, store, "b.md", "---\nstatus: in_progress\n---\n")
	if got := TaskStatusGuard(editInput(p, "status: completed\n"), tsCfg(store)); got.ExitCode != 2 {
		t.Errorf("a task under the configured non-default store root must be in scope, got %+v", got)
	}
	// The same file basename under a DIFFERENT root is out of scope.
	elsewhere := writeTaskFile(t, filepath.Join(t.TempDir(), "tasks"), "b.md", "---\nstatus: in_progress\n---\n")
	if got := TaskStatusGuard(editInput(elsewhere, "status: completed\n"), tsCfg(store)); got != allow {
		t.Errorf("a task outside the configured store root must be out of scope, got %+v", got)
	}
}

func TestTaskStatusGuard_FailsOpen(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	// Non-edit tool.
	if got := TaskStatusGuard(Input{ToolName: "Bash", ToolInput: toolInput{Command: "echo hi"}}, tsCfg(store)); got != allow {
		t.Errorf("non-edit tool must fail open, got %+v", got)
	}
	// Empty file_path.
	if got := TaskStatusGuard(editInput("", "status: completed\n"), tsCfg(store)); got != allow {
		t.Errorf("empty file_path must fail open, got %+v", got)
	}
	// Empty payload (a MultiEdit with no new_string / a bare status change).
	p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
	if got := TaskStatusGuard(Input{ToolName: "Edit", ToolInput: toolInput{FilePath: p}}, tsCfg(store)); got != allow {
		t.Errorf("empty payload must fail open, got %+v", got)
	}
}

// TestTaskStatusGuard_ResolvesScopeFromTargetFile is the cross-project fix: the store
// scope is resolved from the TARGET file's OWN project (as cmd/leadkit's
// resolveTargetTaskStatus would, from that file's governing profile), not an ambient
// session field. A `completed` flip with no evidence on a task file under ITS project's
// store is GUARDED even from a foreign session (before this fix the session's store_root
// no-op'd on another project's file → allow); a file under no known store → allow.
func TestTaskStatusGuard_ResolvesScopeFromTargetFile(t *testing.T) {
	targetStore := filepath.Join(t.TempDir(), "target-proj", "tasks")
	targetTask := writeTaskFile(t, targetStore, "t.md", "---\nstatus: in_progress\n---\n")
	foreignFile := filepath.Join(t.TempDir(), "elsewhere", "x.md")

	var gotTarget string
	cfg := TaskStatusConfig{Resolve: func(target string) TaskStatusScope {
		gotTarget = target
		if strings.HasPrefix(target, targetStore) {
			return tsScope(targetStore) // the file's own project store governs it
		}
		return tsScope(filepath.Join(t.TempDir(), "no-such-store")) // under no known store
	}}

	// A foreign session flips the target project's task to completed with no ref → DENY,
	// because the scope came from the TARGET file's own project.
	res := TaskStatusGuard(editInput(targetTask, "---\nstatus: completed\n---\n"), cfg)
	if res.ExitCode != 2 {
		t.Errorf("cross-project completed-with-no-ref = %+v, want DENY (target-project store governs)", res)
	}
	if gotTarget != targetTask {
		t.Errorf("resolver got target=%q, want the edited task file %q", gotTarget, targetTask)
	}
	// A file under no known store is out of scope → allow (unchanged).
	if got := TaskStatusGuard(editInput(foreignFile, "---\nstatus: completed\n---\n"), cfg); got != allow {
		t.Errorf("file under no known store = %+v, want allow (no-op)", got)
	}
}

// TestTaskStatusGuard_NilResolverFailsOpen proves a misconfigured config (no injected
// resolver) fails OPEN (silent allow), never a panic.
func TestTaskStatusGuard_NilResolverFailsOpen(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
	if got := TaskStatusGuard(editInput(p, "---\nstatus: completed\n---\n"), TaskStatusConfig{}); got != allow {
		t.Errorf("nil resolver must fail open, got %+v", got)
	}
}

func TestTaskStatusGuard_MultiEditPayload(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks")
	p := writeTaskFile(t, store, "t.md", "---\nstatus: in_progress\n---\n")
	in := Input{ToolName: "MultiEdit", ToolInput: toolInput{
		FilePath: p,
		Edits:    []editEntry{{NewString: "title: x\n"}, {NewString: "status: completed\n"}},
	}}
	if got := TaskStatusGuard(in, tsCfg(store)); got.ExitCode != 2 {
		t.Errorf("a status flip inside a MultiEdit edit must DENY with no ref, got %+v", got)
	}
}
