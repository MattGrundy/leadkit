package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/gate"
)

// fakeGateGit is a stub gate.Git for push-gate tests — no real git repo, no real
// records dir. RemoteURL is keyed on the repo path so a test can prove push-gate
// resolved the RIGHT target repo (the `git -C`/`cd`/~ path) before scoping it.
type fakeGateGit struct {
	remotes map[string]string // resolved repo path -> origin url ("" = out of scope)
	head    string            // sha HEAD resolves to ("" = HEAD unresolvable)
	refs    map[string]string // ref name -> sha, for a pushed ref (absent = unresolvable)
}

func (f fakeGateGit) RevParse(repo, ref string) (string, bool) {
	ref = strings.TrimPrefix(strings.TrimSuffix(ref, "^{commit}"), "refs/heads/")
	if ref == "HEAD" {
		if f.head == "" {
			return "", false
		}
		return f.head, true
	}
	if sha, ok := f.refs[ref]; ok {
		return sha, true
	}
	if len(ref) == 40 && strings.Trim(ref, "0123456789abcdef") == "" {
		return ref, true // a full sha is its own resolution (also normalizeSHA's identity)
	}
	return "", false
}

func (f fakeGateGit) RemoteURL(repo string) string { return f.remotes[repo] }

const gateTestSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// enforcedRemote is a generic in-scope origin; enforcedSubstr is the enforced_remotes
// substring the test gates on (an EXPLICIT list — the unchanged path). A repo whose
// origin CONTAINS enforcedSubstr is in scope; one that doesn't is out of scope.
const (
	enforcedRemote = "git@github.com:acmecorp/widgets.git"
	enforcedSubstr = "acmecorp/widgets"
)

// writeFullRecord writes a passing (both layers ready) review-gate record for sha into
// recordsDir, the same on-disk shape internal/gate reads.
func writeFullRecord(t *testing.T, recordsDir, sha string) {
	t.Helper()
	rec := gate.Record{
		SHA: sha,
		Layers: map[string]gate.Layer{
			"code-review": {Verdict: "ready", By: "cr-1", Ts: "2026-01-01T00:00:00Z"},
			"critique":    {Verdict: "ready", By: "crit-1", Ts: "2026-01-01T00:00:00Z"},
		},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordsDir, sha+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// staticGate is the trivial injected resolver most tests use: it returns the SAME
// *gate.Gate for every target repo, so a test can drive PushGate through the injected
// ResolveGate seam without a real profile on disk (the real cmd/leadkit resolver builds a
// per-target gate from the target repo's governing profile — see the cross-project test).
func staticGate(g *gate.Gate) func(string) *gate.Gate {
	return func(string) *gate.Gate { return g }
}

// pushGateEnv builds a push-gate config whose (injected) gate has a tempdir records dir
// and a fake git that reports `remote` for `repoDir`. head defaults to gateTestSHA.
func pushGateEnv(t *testing.T, repoDir, remote string) (PushGateConfig, string) {
	t.Helper()
	recordsDir := t.TempDir()
	g := &gate.Gate{
		RecordsDir:      recordsDir,
		Git:             fakeGateGit{remotes: map[string]string{repoDir: remote}, head: gateTestSHA},
		EnforcedRemotes: []string{enforcedSubstr}, // EXPLICIT list (unchanged path): in scope iff origin contains it
	}
	return PushGateConfig{ResolveGate: staticGate(g), Cwd: repoDir}, recordsDir
}

func bashInput(cmd string) Input {
	return Input{ToolName: "Bash", ToolInput: toolInput{Command: cmd}}
}

func assertAllow(t *testing.T, res Result, ctx string) {
	t.Helper()
	if res.ExitCode != 0 || res.Stdout != "" || res.Stderr != "" {
		t.Errorf("%s: want silent allow, got %+v", ctx, res)
	}
}

func assertDeny(t *testing.T, res Result, ctx string) {
	t.Helper()
	if res.ExitCode != 0 {
		t.Errorf("%s: want exit 0 (deny is via stdout JSON), got %d", ctx, res.ExitCode)
	}
	var payload struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
		t.Fatalf("%s: deny stdout not valid JSON: %v\n%s", ctx, err, res.Stdout)
	}
	if payload.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("%s: permissionDecision = %q, want deny", ctx, payload.HookSpecificOutput.PermissionDecision)
	}
}

func TestPushGate_NoRecordInEnforcedRepo_Denies(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote)
	res := PushGate(bashInput("git push"), cfg)
	assertDeny(t, res, "no record / enforced")
	// The deny carries the gate's own reason + the record/BYPASS guidance.
	if !strings.Contains(res.Stdout, "no review record") {
		t.Errorf("deny reason should include the gate's 'no review record' reason, got:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "leadkit gate record") || !strings.Contains(res.Stdout, "REVIEW_GATE_BYPASS") {
		t.Errorf("deny should carry the record + BYPASS guidance, got:\n%s", res.Stdout)
	}
}

func TestPushGate_FullPassingRecord_Allows(t *testing.T) {
	repo := t.TempDir()
	cfg, recordsDir := pushGateEnv(t, repo, enforcedRemote)
	writeFullRecord(t, recordsDir, gateTestSHA)
	assertAllow(t, PushGate(bashInput("git push"), cfg), "full passing record")
}

func TestPushGate_OutOfScopeRepo_Allows(t *testing.T) {
	repo := t.TempDir()
	// A repo whose origin is not an enforced remote — Check returns out-of-scope allow.
	cfg, _ := pushGateEnv(t, repo, "git@github.com:someorg/random.git")
	assertAllow(t, PushGate(bashInput("git push"), cfg), "out of scope")
}

func TestPushGate_BypassEnv_Allows(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote) // in scope, but bypassed
	t.Setenv("REVIEW_GATE_BYPASS", "hotfix in progress")
	assertAllow(t, PushGate(bashInput("git push"), cfg), "BYPASS env")
}

func TestPushGate_BypassFile_Allows(t *testing.T) {
	repo := t.TempDir()
	cfg, recordsDir := pushGateEnv(t, repo, enforcedRemote)
	if err := os.WriteFile(filepath.Join(recordsDir, "BYPASS"), []byte("maintenance window"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertAllow(t, PushGate(bashInput("git push"), cfg), "BYPASS file")
}

// TestPushGate_ResolvesTargetRepo proves push-gate hands the RIGHT repo path to the
// gate for `git -C <path> push` and `cd <path> && git push` — the enforced repo has no
// record, so a correct resolution DENIES; a wrong one would read the (out-of-scope) cwd
// and allow.
func TestPushGate_ResolvesTargetRepo(t *testing.T) {
	enforcedRepo := t.TempDir()
	otherCwd := t.TempDir() // the process cwd is out of scope — only the -C/cd path is enforced
	recordsDir := t.TempDir()
	g := &gate.Gate{
		RecordsDir: recordsDir,
		Git: fakeGateGit{
			remotes: map[string]string{enforcedRepo: enforcedRemote, otherCwd: ""},
			head:    gateTestSHA,
			refs:    map[string]string{"master": gateTestSHA},
		},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{ResolveGate: staticGate(g), Cwd: otherCwd}

	for _, cmd := range []string{
		"git -C " + enforcedRepo + " push",
		"cd " + enforcedRepo + " && git push origin master",
	} {
		res := PushGate(bashInput(cmd), cfg)
		assertDeny(t, res, cmd)
	}
}

// TestPushGate_TildePathResolves is the regression test for the shell's 2026-08-03 bug:
// an unexpanded leading `~` made `cd "$repo"` fail silently and the hook blocked with an
// empty diagnostic. push-gate expands `~` in Go, so a `git -C ~/repo push` resolves to
// $HOME/repo and the enforced-but-ungated repo DENIES (never the empty-diagnostic block,
// and never a silent allow from a stat failure on a literal "~/repo").
func TestPushGate_TildePathResolves(t *testing.T) {
	home := t.TempDir()
	enforcedRepo := filepath.Join(home, "myrepo")
	if err := os.MkdirAll(enforcedRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	recordsDir := t.TempDir()
	g := &gate.Gate{
		RecordsDir:      recordsDir,
		Git:             fakeGateGit{remotes: map[string]string{enforcedRepo: enforcedRemote}, head: gateTestSHA},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	// Home is passed explicitly (no env mutation) — the resolver expands ~/ against it.
	cfg := PushGateConfig{ResolveGate: staticGate(g), Home: home}
	res := PushGate(bashInput("git -C ~/myrepo push"), cfg)
	assertDeny(t, res, "~-path resolves to an enforced repo")

	// Sanity: had the ~ NOT expanded, gate.Check's os.Stat on the literal "~/myrepo"
	// would fail and fail-open ALLOW — so an allow here would mean the bug recurred.
	if res.Stdout == "" {
		t.Fatal("~-path push was allowed — the shell's unexpanded-~ bug has recurred")
	}
}

func TestPushGate_NonPushAndNonBash_Allow(t *testing.T) {
	repo := t.TempDir()
	cfg, _ := pushGateEnv(t, repo, enforcedRemote)

	// A non-push Bash command — parse failure, fail open.
	assertAllow(t, PushGate(bashInput("git status"), cfg), "non-push command")
	// A bare noise command.
	assertAllow(t, PushGate(bashInput("echo hi"), cfg), "unrelated command")
	// A non-Bash tool never reaches the parser.
	assertAllow(t, PushGate(Input{ToolName: "Edit", ToolInput: toolInput{Command: "git push"}}, cfg), "non-Bash tool")
	// A nil ResolveGate disables the hook (fail open) even for a real push.
	assertAllow(t, PushGate(bashInput("git push"), PushGateConfig{Cwd: repo}), "nil resolver")
	// A resolver that returns a nil *gate.Gate also fails open (nothing to check against).
	assertAllow(t, PushGate(bashInput("git push"), PushGateConfig{ResolveGate: func(string) *gate.Gate { return nil }, Cwd: repo}), "nil gate from resolver")
}

// TestPushGate_CrossProject_BypassClosed is the OPEN-HOLE regression under the new
// own-origin default: a foreign session pushes a repo that has its OWN leadkit.yaml
// (no enforced_remotes) and NO gate record. The gate is resolved from the TARGET
// repo's governance (resolveTargetGate) — here a gate with an empty EnforcedRemotes
// but a ProjectRoot set to the target, so the own-origin default gates the target's
// OWN origin regardless of the pusher's session → DENY. (fakeGateGit reports the same
// origin for the ProjectRoot as for the target, standing in for "the target IS the
// profile's own project.")
func TestPushGate_CrossProject_BypassClosed(t *testing.T) {
	targetRepo := t.TempDir() // a repo governed by its OWN profile
	recordsDir := t.TempDir() // no record on disk → ungated
	// The target's governing gate: NO explicit list, but its own project root — so the
	// own-origin default gates it no matter what the pushing session configured.
	targetGate := &gate.Gate{
		RecordsDir:      recordsDir,
		Git:             fakeGateGit{remotes: map[string]string{targetRepo: enforcedRemote}, head: gateTestSHA},
		EnforcedRemotes: nil,        // unset → own-origin default (NOT "enforce nothing")
		ProjectRoot:     targetRepo, // the target IS the governing profile's own project
	}
	cfg := PushGateConfig{
		ResolveGate: func(repoPath string) *gate.Gate {
			if repoPath != targetRepo {
				t.Errorf("resolver got repoPath=%q, want the push's own target %q", repoPath, targetRepo)
			}
			return targetGate
		},
		Cwd: t.TempDir(), // an unrelated session cwd — the -C path is what governs
	}
	res := PushGate(bashInput("git -C "+targetRepo+" push"), cfg)
	assertDeny(t, res, "cross-project ungated push to an own-origin-gated repo must DENY")
	if !strings.Contains(res.Stdout, "no review record") {
		t.Errorf("deny reason should be the missing-record reason, got:\n%s", res.Stdout)
	}
}

// TestPushGate_SessionEnforcedRemotes_GatesUnprofiledCrossProjectClone is the
// coverage-regression fix at the push-gate layer: an acting SESSION whose
// enforced_remotes names a repo pushes an UNPROFILED clone of it — the target's own
// gating (empty list, no project root) reads it out of scope, but the session list
// (SessionEnforcedRemotes, wired by buildPushGateConfig) unions it back IN → DENY when
// there's no record.
func TestPushGate_SessionEnforcedRemotes_GatesUnprofiledCrossProjectClone(t *testing.T) {
	targetClone := t.TempDir() // an UNPROFILED clone: no own list, no own origin gating
	recordsDir := t.TempDir()  // no record → ungated
	targetGate := &gate.Gate{
		RecordsDir:             recordsDir,
		Git:                    fakeGateGit{remotes: map[string]string{targetClone: "git@github.com:acmecorp/widgets.git"}, head: gateTestSHA},
		EnforcedRemotes:        nil, // target unprofiled
		ProjectRoot:            "",  // no own origin
		SessionEnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{ResolveGate: staticGate(targetGate), Cwd: t.TempDir()}
	res := PushGate(bashInput("git -C "+targetClone+" push"), cfg)
	assertDeny(t, res, "session-declared cross-project push to an unprofiled clone must DENY")
	if !strings.Contains(res.Stdout, "no review record") {
		t.Errorf("deny reason should be the missing-record reason, got:\n%s", res.Stdout)
	}
}

// TestPushGate_UngovernedRepo_StillAllows proves the complement: a genuinely-UNPROFILED
// target repo (empty enforced_remotes AND no project root → no derivable own origin)
// stays OUT of scope → allow. The own-origin default gates your own repo, not every
// repo the world over.
func TestPushGate_UngovernedRepo_StillAllows(t *testing.T) {
	targetRepo := t.TempDir()
	ungovernedGate := &gate.Gate{
		RecordsDir:      t.TempDir(),
		Git:             fakeGateGit{remotes: map[string]string{targetRepo: "git@github.com:someorg/random.git"}, head: gateTestSHA},
		EnforcedRemotes: nil, // unset AND no ProjectRoot → unprofiled target, out of scope
	}
	cfg := PushGateConfig{ResolveGate: staticGate(ungovernedGate), Cwd: targetRepo}
	assertAllow(t, PushGate(bashInput("git -C "+targetRepo+" push"), cfg), "unprofiled repo stays out of scope")
}

// TestPushGate_RelativeRecordsDir_ResolvesAgainstTargetRoot proves the records_dir side of
// the fix: a target profile's RELATIVE records_dir must resolve against the TARGET repo
// root, so a cross-project gate record written under <target>/.leadkit/records is FOUND
// (the false-deny fix). The resolver stands in for resolveTargetGate anchoring records_dir
// at the target root; a complete record there → allow.
func TestPushGate_RelativeRecordsDir_ResolvesAgainstTargetRoot(t *testing.T) {
	targetRepo := t.TempDir()
	targetRecords := filepath.Join(targetRepo, ".leadkit", "records")
	if err := os.MkdirAll(targetRecords, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFullRecord(t, targetRecords, gateTestSHA) // the record lives under the TARGET root
	targetGate := &gate.Gate{
		RecordsDir:      targetRecords, // resolveTargetGate would have anchored the relative path here
		Git:             fakeGateGit{remotes: map[string]string{targetRepo: enforcedRemote}, head: gateTestSHA},
		EnforcedRemotes: []string{enforcedSubstr},
	}
	cfg := PushGateConfig{ResolveGate: staticGate(targetGate), Cwd: targetRepo}
	assertAllow(t, PushGate(bashInput("git -C "+targetRepo+" push"), cfg), "record under the target-rooted relative records_dir is found → allow")
}

func TestPushGateDenyReason_NamesOnlyRealRecordPath(t *testing.T) {
	got := pushGateDenyReason("missing code-review", "/r", ModeEnforce)
	if strings.Contains(got, ".sh") {
		t.Errorf("deny reason names a shell script that does not exist: %s", got)
	}
	if !strings.Contains(got, "leadkit gate record") {
		t.Errorf("deny reason does not name `leadkit gate record`: %s", got)
	}
}
