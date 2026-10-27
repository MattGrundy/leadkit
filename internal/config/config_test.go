package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// chdir switches the process cwd for the duration of the test (Load() discovers via
// os.Getwd(), not an injectable root), restoring it on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func TestLoad_DiscoveryWalksUpToFindProfile(t *testing.T) {
	t.Setenv("LEADKIT_PROFILE", "")
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DefaultProfileName), []byte("queue_root: my-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)

	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.QueueRoot != "my-queue" {
		t.Errorf("QueueRoot = %q, want my-queue", prof.QueueRoot)
	}
	gotDir, _ := filepath.EvalSymlinks(filepath.Dir(path))
	wantDir, _ := filepath.EvalSymlinks(root)
	if gotDir != wantDir {
		t.Errorf("discovered profile dir = %s, want %s", gotDir, wantDir)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(path, []byte("queue_root: custom-queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)

	prof, gotPath, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != path {
		t.Errorf("path = %s, want %s", gotPath, path)
	}
	if prof.QueueRoot != "custom-queue" {
		t.Errorf("QueueRoot = %q, want custom-queue", prof.QueueRoot)
	}
}

func TestLoad_NoProfileFoundReturnsDefaults(t *testing.T) {
	t.Setenv("LEADKIT_PROFILE", "")
	chdir(t, t.TempDir())

	prof, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty (no leadkit.yaml found)", path)
	}
	if prof.QueueRoot == "" {
		t.Error("want a Defaults()-populated profile, got an empty QueueRoot")
	}
}

func TestLoad_FileValuesMergeOverDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultProfileName)
	// Only queue_root is set — everything else must keep its Defaults() value.
	if err := os.WriteFile(path, []byte("queue_root: /custom/queue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)

	prof, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := Defaults()
	if prof.QueueRoot != "/custom/queue" {
		t.Errorf("QueueRoot = %q, want the file's override", prof.QueueRoot)
	}
	if prof.KeychainService != d.KeychainService {
		t.Errorf("KeychainService = %q, want the default %q to survive an unset field", prof.KeychainService, d.KeychainService)
	}
	if prof.Burn.OpRateWarn != d.Burn.OpRateWarn {
		t.Errorf("Burn.OpRateWarn = %d, want the default %d to survive", prof.Burn.OpRateWarn, d.Burn.OpRateWarn)
	}
}

func TestWakeCommand_DefaultEmptyAndParsesWhenSet(t *testing.T) {
	if Defaults().WakeCommand != "" {
		t.Errorf("Defaults().WakeCommand = %q, want empty (no external wake by default)", Defaults().WakeCommand)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, DefaultProfileName)
	if err := os.WriteFile(path, []byte("wake_command: \"claude-wake {agent} {msg}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)

	prof, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.WakeCommand != "claude-wake {agent} {msg}" {
		t.Errorf("WakeCommand = %q, want the file's value with placeholders intact", prof.WakeCommand)
	}
}

// The `type: script` DoD signal toggles: enable_script_signals DEFAULTS TRUE (a project
// that wrote a script condition wants it run) and script_timeout DEFAULTS to 30s; both
// parse off a milestone: block and an explicit false/duration override the defaults.
func TestMilestoneScriptSignals_DefaultsAndParse(t *testing.T) {
	d := Defaults()
	if !d.Milestone.EnableScriptSignals {
		t.Error("milestone.enable_script_signals must default TRUE")
	}
	if got := d.Milestone.ScriptTimeoutDur(); got != 30*time.Second {
		t.Errorf("script_timeout default = %v, want 30s", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, DefaultProfileName)
	if err := os.WriteFile(path, []byte("milestone:\n  enable_script_signals: false\n  script_timeout: 5s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADKIT_PROFILE", path)

	prof, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if prof.Milestone.EnableScriptSignals {
		t.Error("enable_script_signals: false in the file must override the true default")
	}
	if got := prof.Milestone.ScriptTimeoutDur(); got != 5*time.Second {
		t.Errorf("script_timeout = %v, want the file's 5s", got)
	}

	// An unparseable/empty duration falls back to the 30s default.
	if got := (MilestoneGate{ScriptTimeout: "garbage"}).ScriptTimeoutDur(); got != DefaultScriptTimeoutDur {
		t.Errorf("unparseable script_timeout = %v, want fallback %v", got, DefaultScriptTimeoutDur)
	}
}

func TestDefaults_Usable(t *testing.T) {
	d := Defaults()
	if d.QueueRoot == "" || d.RecordsDir == "" || d.RolesPath == "" {
		t.Errorf("Defaults() left a path field empty: %+v", d)
	}
	if d.GoalFile == "" || d.HandoverFile == "" {
		t.Error("Defaults() should set goal/handover file locations")
	}
	if len(d.EnforcedRemotes) != 0 {
		t.Errorf("Defaults() must ship enforced_remotes EMPTY (an unset list gates the project's own origin, not a baked-in named list); got %v", d.EnforcedRemotes)
	}
	if len(d.CanonicalStatuses) == 0 {
		t.Error("Defaults() should carry the canonical status vocabulary")
	}
	if d.SessionTasksRoot == "" {
		t.Error("Defaults() should set session_tasks_root")
	}
	if d.SessionTaskWorkish.DescriptionLengthThreshold == 0 || len(d.SessionTaskWorkish.Markers) == 0 {
		t.Errorf("Defaults() session_task_workish looks unset: %+v", d.SessionTaskWorkish)
	}
}

func TestDefaults_CanonicalStatusesIsTheFullWorkflowPlusDocMetaSet(t *testing.T) {
	// User decision (audit-2): the workflow set PLUS doc/meta types — "completed",
	// not "done", is this project's real terminal-status word, and awaiting_review
	// deliberately stays OUT (it's a true stray, an alias for in_review).
	want := []string{
		"pending", "in_progress", "in_review", "awaiting_pr_review", "blocked",
		"needs_rework", "gated", "completed", "dropped", "superseded",
		"reference", "plan", "draft", "design",
	}
	got := Defaults().CanonicalStatuses
	if len(got) != len(want) {
		t.Fatalf("CanonicalStatuses = %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CanonicalStatuses[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, stray := range []string{"done", "awaiting_review"} {
		for _, s := range got {
			if s == stray {
				t.Errorf("CanonicalStatuses must NOT contain %q — it's a deliberate stray/alias", stray)
			}
		}
	}
}

func TestProfile_Resolve(t *testing.T) {
	p := Profile{root: "/some/root"}
	if got := p.Resolve("sub/file.md"); got != filepath.Join("/some/root", "sub/file.md") {
		t.Errorf("Resolve(rel) = %s", got)
	}
	if got := p.Resolve("/abs/path"); got != "/abs/path" {
		t.Errorf("Resolve(abs) = %s, want unchanged", got)
	}
	if got := p.Resolve(""); got != "" {
		t.Errorf("Resolve(\"\") = %q, want empty", got)
	}
}

func TestProfile_ResolveExpandsHomeAndEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := Profile{root: "/some/root"}

	if got := p.Resolve("~/.claude/records"); got != filepath.Join(home, ".claude/records") {
		t.Errorf("Resolve(~/...) = %s, want %s", got, filepath.Join(home, ".claude/records"))
	}

	t.Setenv("LEADKIT_TEST_VAR", "/env/expanded")
	if got := p.Resolve("$LEADKIT_TEST_VAR/x"); got != "/env/expanded/x" {
		t.Errorf("Resolve($VAR/...) = %s, want /env/expanded/x", got)
	}
}

func TestProfile_ValidateWarnsOnMissingReferencedFile(t *testing.T) {
	p := Profile{root: t.TempDir(), RolesPath: "does-not-exist/ROLES"}
	warnings := p.Validate()
	if len(warnings) == 0 {
		t.Fatal("want at least one warning for a missing roles_path")
	}
}

func TestProfile_ValidateNoWarningsWhenFieldsUnset(t *testing.T) {
	p := Profile{}
	if warnings := p.Validate(); len(warnings) != 0 {
		t.Errorf("want no warnings for an all-empty profile, got %v", warnings)
	}
}

func TestProfile_ValidateNeverErrorsOnlyWarns(t *testing.T) {
	// Validate's signature itself (no error return) is the contract: it can only
	// warn. This just exercises it against a profile whose every referenced path is
	// missing, to confirm it returns cleanly rather than panicking.
	p := Profile{root: t.TempDir(), RolesPath: "x", GoalFile: "y", HandoverFile: "z"}
	if len(p.Validate()) != 3 {
		t.Errorf("want 3 warnings (roles_path, goal_file, handover_file), got %v", p.Validate())
	}
}

func TestBoolOrHuman_UnmarshalYAML(t *testing.T) {
	var human AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_merge: human\n"), &human); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !human.CanMerge.Human {
		t.Error("want Human=true for can_merge: human")
	}

	var allowed AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_merge: true\n"), &allowed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if allowed.CanMerge.Human || !allowed.CanMerge.Allow {
		t.Errorf("want {Allow:true Human:false}, got %+v", allowed.CanMerge)
	}

	var invalid AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_merge: banana\n"), &invalid); err == nil {
		t.Fatal("want an error for an invalid can_merge string")
	}
}

func TestBoolOrHuman_MarshalYAML(t *testing.T) {
	out, err := yaml.Marshal(AuthoritySpec{CanMerge: BoolOrHuman{Human: true}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back AuthoritySpec
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if !back.CanMerge.Human {
		t.Errorf("round-trip lost Human=true: %+v", back.CanMerge)
	}
}

func TestAuthoritySpec_CanOpenPRAcceptsHuman(t *testing.T) {
	var a AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_open_pr: human\n"), &a); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !a.CanOpenPR.Human {
		t.Errorf("want CanOpenPR.Human=true, got %+v", a.CanOpenPR)
	}

	var b AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_open_pr: true\n"), &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if b.CanOpenPR.Human || !b.CanOpenPR.Allow {
		t.Errorf("want {Allow:true Human:false}, got %+v", b.CanOpenPR)
	}
}

func TestAuthoritySpec_PRDraftOnly(t *testing.T) {
	tests := map[string]bool{
		"pr_draft_only: true\n":  true,
		"pr_draft_only: false\n": false,
		"":                       false, // absent — zero value
	}
	for doc, want := range tests {
		var a AuthoritySpec
		if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
			t.Fatalf("Unmarshal(%q): %v", doc, err)
		}
		if a.PRDraftOnly != want {
			t.Errorf("Unmarshal(%q).PRDraftOnly = %v, want %v", doc, a.PRDraftOnly, want)
		}
	}
}

func TestAuthoritySpec_MergeGrantPresentOrAbsent(t *testing.T) {
	var granted AuthoritySpec
	if err := yaml.Unmarshal([]byte("merge_grant: \"#123-critical\"\n"), &granted); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if granted.MergeGrant != "#123-critical" {
		t.Errorf("MergeGrant = %q, want #123-critical", granted.MergeGrant)
	}

	var absent AuthoritySpec
	if err := yaml.Unmarshal([]byte("can_push: true\n"), &absent); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if absent.MergeGrant != "" {
		t.Errorf("MergeGrant = %q, want empty when the key is absent", absent.MergeGrant)
	}

	// omitempty: an absent merge_grant must not resurface on Marshal.
	out, err := yaml.Marshal(absent)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "merge_grant") {
		t.Errorf("Marshal output contains merge_grant with nothing set: %s", out)
	}
}

// TestLoadProjectFile_ExactNoWalkupNoEnv: LoadProjectFile reads EXACTLY the given file
// (ProjectID + a Resolve() rooted at the file's dir), never walking up and never honoring
// $LEADKIT_PROFILE — the identity-path guarantee that a config-less dir does NOT inherit
// an ancestor's config.
func TestLoadProjectFile_ExactNoWalkupNoEnv(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DefaultProfileName), []byte("project_id: alpha\nqueue_root: q\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stray env profile must be irrelevant to LoadProjectFile.
	t.Setenv("LEADKIT_PROFILE", filepath.Join(t.TempDir(), DefaultProfileName))

	prof, ok, err := LoadProjectFile(filepath.Join(root, DefaultProfileName))
	if err != nil || !ok || prof == nil {
		t.Fatalf("LoadProjectFile ok=%v err=%v prof=%v", ok, err, prof)
	}
	if prof.ProjectID != "alpha" {
		t.Errorf("project_id = %q, want alpha", prof.ProjectID)
	}
	if got := prof.Resolve(prof.QueueRoot); got != filepath.Join(root, "q") {
		t.Errorf("Resolve(queue_root) = %q, want it rooted at the file's dir %q", got, filepath.Join(root, "q"))
	}

	// A config-less dir (own file absent) → ok=false, NO walk-up to an ancestor.
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, DefaultProfileName), []byte("project_id: ancestor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	_, ok2, err2 := LoadProjectFile(filepath.Join(child, DefaultProfileName))
	if err2 != nil {
		t.Fatalf("err = %v", err2)
	}
	if ok2 {
		t.Error("LoadProjectFile must NOT walk up to an ancestor leadkit.yaml — ok should be false for a config-less dir")
	}
}
