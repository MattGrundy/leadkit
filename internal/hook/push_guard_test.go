package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// defaultWL mirrors config.DefaultPushWhitelist without importing internal/config
// (this package stays decoupled from it, per PushGuardConfig's doc comment).
var defaultWL = []string{"feat/*", "fix/*", "chore/*", "mg/*", "user/*"}

// staticWL is the trivial injected resolver most tests use: it returns the SAME
// whitelist for any (repoPath, origin) — reproducing the old single-profile
// DefaultWhitelist behaviour, so every pre-existing case keeps its exact meaning now
// that the guard resolves its whitelist through the injected EffectiveWhitelist func.
func staticWL(wl []string) func(repoPath, origin string) []string {
	return func(repoPath, origin string) []string { return wl }
}

func pushInput(cmd string) Input {
	return Input{ToolName: "Bash", ToolInput: toolInput{Command: cmd}}
}

func withGitStub(t *testing.T, fn func(repoPath string, args ...string) (string, bool)) {
	t.Helper()
	orig := runGit
	runGit = fn
	t.Cleanup(func() { runGit = orig })
}

// noRemoteStub answers every runGit call as a failure — no origin remote, no
// current-branch lookup — the common case for tests that supply an explicit refspec
// and don't care about per-repo override or bare-push resolution.
func noRemoteStub(t *testing.T) {
	withGitStub(t, func(repoPath string, args ...string) (string, bool) {
		return "", false
	})
}

func TestPushGuard_ExplicitRefspec(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string
		wantDeny bool
	}{
		{"feat allowed", "git push origin feat/x", false},
		{"master denied", "git push origin master", true},
		{"release denied", "git push origin release/1.2", true},
		{"force-push mg allowed", "git push --force origin mg/x", false},
		{"force-push main denied", "git push -f origin main", true},
		{"plus-refspec force main denied", "git push origin +main", true},
		{"HEAD:develop denied (default, no override)", "git push origin HEAD:develop", true},
		{"tag push denied", "git push origin v1.2.3 refs/tags/v1.2.3", true},
		{"tags flag denied", "git push origin --tags", true},
		{"cross-slash user allowed", "git push origin user/matt/thing", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteStub(t)
			res := PushGuard(pushInput(tc.cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
			denied := res.ExitCode == 0 && res.Stdout != ""
			if denied != tc.wantDeny {
				t.Errorf("PushGuard(%q) denied=%v, want %v (res=%+v)", tc.cmd, denied, tc.wantDeny, res)
			}
		})
	}
}

// TestPushGuard_ShellOperators_FalsePositiveGone covers the reported bug (a trailing
// redirect/pipe was parsed as a refspec and DENIED a legit whitelisted push) and its
// variants: every one must ALLOW. The refspec is explicit and whitelisted, so the git
// stub never needs to resolve a branch (noRemoteStub answers every call as failure).
func TestPushGuard_ShellOperators_FalsePositiveGone(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		// The exact reported command — denied before the fix, allowed after.
		{"reported command", "git push origin feat/outbound-proxy-resources 2>&1 | tail -8"},
		{"stderr-to-stdout only", "git push origin feat/outbound-proxy-resources 2>&1"},
		{"stdout redirect to file", "git push origin feat/x >/tmp/x"},
		{"fd2 redirect to file", "git push origin feat/x 2>/tmp/x"},
		{"pipe to tee", "git push origin feat/x | tee log"},
		{"and-chain echo", "git push origin feat/x && echo done"},
		{"semicolon echo", "git push origin feat/x ; echo done"},
		{"append + dup", "git push origin feat/x >>log 2>&1"},
		{"bare redirect operator spaced", "git push origin feat/x > /tmp/out"},
		{"git -C with redirect+pipe", "git -C /path push origin fix/x 2>&1 | tail"},
		{"cd carry with redirect", "cd /path && git push origin chore/y 2>&1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteStub(t)
			res := PushGuard(pushInput(tc.cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
			if res != allow {
				t.Errorf("PushGuard(%q) = %+v, want ALLOW (a trailing redirect/pipe is not a refspec)", tc.cmd, res)
			}
		})
	}
}

// TestPushGuard_ShellOperators_BypassClosed covers the mirror-image bug: a `git push`
// hidden after a shell separator used to slip the guard entirely (fail OPEN → an
// ungoverned push). Each hidden/second push must now be recognised and DENIED.
func TestPushGuard_ShellOperators_BypassClosed(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"redirect not a refspec, real target denied", "git push origin release/x 2>&1 | tail"},
		{"push after pipe", "foo | git push origin release/x"},
		{"push after semicolon", "echo x; git push origin master"},
		{"push after and-chain", "true && git push origin release/z"},
		{"two pushes, second bad", "git push origin feat/ok && git push origin release/bad"},
		{"tags flag with redirects", "git push origin --tags 2>&1 | tail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteStub(t)
			res := PushGuard(pushInput(tc.cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
			denied := res.ExitCode == 0 && res.Stdout != ""
			if !denied {
				t.Errorf("PushGuard(%q) = %+v, want DENY (a push hidden by a shell operator must still be caught)", tc.cmd, res)
			}
		})
	}
}

// TestPushGuard_ShellOperators_TwoPushesFirstFineSecondBad proves ALL pushes in a
// command are checked, not just the first: the good push does not shortcut the guard
// past the bad one.
func TestPushGuard_ShellOperators_TwoPushesFirstFineSecondBad(t *testing.T) {
	noRemoteStub(t)
	res := PushGuard(pushInput("git push origin feat/ok && git push origin release/bad"),
		PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
	denied := res.ExitCode == 0 && res.Stdout != ""
	if !denied {
		t.Fatalf("two-push command = %+v, want DENY on the second (release/bad) push", res)
	}
	if !strings.Contains(res.Stdout, "release/bad") {
		t.Errorf("deny reason = %q, want it to name the bad target release/bad", res.Stdout)
	}
}

// TestPushGuard_BarePush_RedirectsAppended proves a bare `git push` with a trailing
// redirect/pipe still resolves the current branch via runGit and checks it — the
// redirect never becomes a spurious refspec.
func TestPushGuard_BarePush_RedirectsAppended(t *testing.T) {
	cases := []struct {
		branch   string
		wantDeny bool
	}{
		{"feat/x", false},
		{"master", true},
	}
	for _, tc := range cases {
		t.Run(tc.branch, func(t *testing.T) {
			withGitStub(t, func(repoPath string, args ...string) (string, bool) {
				if len(args) > 0 && args[0] == "rev-parse" {
					return tc.branch, true
				}
				return "", false
			})
			res := PushGuard(pushInput("git push origin 2>&1 | tail -8"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
			denied := res.ExitCode == 0 && res.Stdout != ""
			if denied != tc.wantDeny {
				t.Errorf("bare push (branch=%s) with redirects: denied=%v, want %v (res=%+v)", tc.branch, denied, tc.wantDeny, res)
			}
		})
	}
}

// TestPushGuard_ShellOperators_UnparseableFailsClosed proves a genuinely unparseable
// refspec (bare `:`) still fails CLOSED even with redirects appended.
func TestPushGuard_ShellOperators_UnparseableFailsClosed(t *testing.T) {
	noRemoteStub(t)
	res := PushGuard(pushInput("git push origin : 2>&1"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Errorf("unparseable refspec with redirect = %+v, want a fail-closed deny", res)
	}
}

// TestPushGuard_ShellOperators_NonPushFailsOpen proves a command with NO git push in
// any segment is not governed (fail OPEN), even when the word "push" appears elsewhere.
// A command that spells out `git push` as text is NOT in this list: the independent check
// in parsePushCommands refuses it (see TestParsePushCommands_Corpus_FoundOrRefusedNeverAbsent).
func TestPushGuard_ShellOperators_NonPushFailsOpen(t *testing.T) {
	noRemoteStub(t)
	for _, cmd := range []string{"ls | grep push", "git status 2>&1 | tail", "git stash push -m x"} {
		res := PushGuard(pushInput(cmd), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
		if res != allow {
			t.Errorf("PushGuard(%q) = %+v, want ALLOW (not a git push — fail open)", cmd, res)
		}
	}
}

func TestPushGuard_BarePush_ResolvesCurrentBranch(t *testing.T) {
	cases := []struct {
		branch   string
		wantDeny bool
	}{
		{"feat/x", false},
		{"master", true},
	}
	for _, tc := range cases {
		t.Run(tc.branch, func(t *testing.T) {
			withGitStub(t, func(repoPath string, args ...string) (string, bool) {
				if len(args) > 0 && args[0] == "rev-parse" {
					return tc.branch, true
				}
				return "", false // no origin remote configured
			})
			res := PushGuard(pushInput("git push origin"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
			denied := res.ExitCode == 0 && res.Stdout != ""
			if denied != tc.wantDeny {
				t.Errorf("bare push, branch=%s: denied=%v, want %v (res=%+v)", tc.branch, denied, tc.wantDeny, res)
			}
		})
	}
}

func TestPushGuard_BarePush_GitExecFailsFailsClosed(t *testing.T) {
	withGitStub(t, func(repoPath string, args ...string) (string, bool) {
		return "", false // every git call fails: no remote, no branch resolution
	})
	res := PushGuard(pushInput("git push"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Errorf("bare push with git exec failure = %+v, want a deny (fail-closed)", res)
	}
}

func TestPushGuard_NonPushCommandAllowsOpen(t *testing.T) {
	noRemoteStub(t)
	res := PushGuard(pushInput("git status"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)})
	if res != allow {
		t.Errorf("PushGuard(git status) = %+v, want allow", res)
	}
}

func TestPushGuard_MalformedJSONFailsOpenViaDispatch(t *testing.T) {
	res := Dispatch(NamePushGuard, []byte("not json"), Config{PushGuard: PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL)}})
	if res != allow {
		t.Errorf("Dispatch(push-guard, malformed json) = %+v, want allow", res)
	}
}

// TestPushGuard_PerRepoOverride proves the guard judges a push by the whitelist the
// injected resolver returns FOR THAT (repoPath, origin) — the per-repo override seam.
// The resolver keys on the resolved origin exactly as the real cmd/leadkit resolver
// does after loading the target repo's profile: platform-infra' origin resolves a list
// that adds `develop`; web-client's resolves the plain default (no override).
func TestPushGuard_PerRepoOverride(t *testing.T) {
	cfg := PushGuardConfig{
		EffectiveWhitelist: func(repoPath, origin string) []string {
			switch {
			case strings.Contains(origin, "platform-infra"):
				return append(append([]string{}, defaultWL...), "develop")
			default:
				// No override for this origin (web-client, or an unresolved
				// origin) → the plain default, exactly as the real resolver falls back.
				return defaultWL
			}
		},
	}

	t.Run("platform-infra allows develop", func(t *testing.T) {
		withGitStub(t, func(repoPath string, args ...string) (string, bool) {
			if len(args) > 0 && args[0] == "config" {
				return "git@github.com:acmecorp/platform-infra.git", true
			}
			return "", false
		})
		res := PushGuard(pushInput("git push origin develop"), cfg)
		if res != allow {
			t.Errorf("platform-infra push to develop = %+v, want allow", res)
		}
	})

	t.Run("web-client denies develop (no override, inherits default)", func(t *testing.T) {
		withGitStub(t, func(repoPath string, args ...string) (string, bool) {
			if len(args) > 0 && args[0] == "config" {
				return "git@github.com:acmecorp/web-client.git", true
			}
			return "", false
		})
		res := PushGuard(pushInput("git push origin develop"), cfg)
		denied := res.ExitCode == 0 && res.Stdout != ""
		if !denied {
			t.Errorf("web-client push to develop = %+v, want deny", res)
		}
	})
}

// TestPushGuard_ResolverReceivesTargetRepoAndOrigin proves the guard resolves the
// whitelist from the TARGET repo: it forwards the push's OWN repoPath (from `git -C
// <path>`) and the origin it resolves for THAT repo to EffectiveWhitelist, and judges
// the push by the list the resolver returns for that pair. This is the seam the
// ambient-profile cross-repo fix rides on — the resolver, not an ambient field, decides.
func TestPushGuard_ResolverReceivesTargetRepoAndOrigin(t *testing.T) {
	withGitStub(t, func(repoPath string, args ...string) (string, bool) {
		if len(args) > 0 && args[0] == "config" && repoPath == "/clones/shared-infra" {
			return "git@github.com:acmecorp/shared-infra.git", true
		}
		return "", false
	})
	var gotRepoPath, gotOrigin string
	cfg := PushGuardConfig{
		EffectiveWhitelist: func(repoPath, origin string) []string {
			gotRepoPath, gotOrigin = repoPath, origin
			// The TARGET repo's resolved list (built-in default, includes mg/*) —
			// NOT some ambient [chore/*]. mg/example-proxy must therefore be ALLOWED.
			return defaultWL
		},
	}
	res := PushGuard(pushInput("git -C /clones/shared-infra push origin mg/example-proxy"), cfg)
	if res != allow {
		t.Fatalf("cross-repo mg/* push = %+v, want ALLOW (judged by the target repo's resolved whitelist)", res)
	}
	if gotRepoPath != "/clones/shared-infra" {
		t.Errorf("resolver got repoPath=%q, want the push's own target path /clones/shared-infra", gotRepoPath)
	}
	if gotOrigin != "git@github.com:acmecorp/shared-infra.git" {
		t.Errorf("resolver got origin=%q, want the TARGET repo's resolved origin", gotOrigin)
	}
}

// TestPushGuard_NilResolverFailsClosed proves a misconfigured config (no injected
// resolver) is FAIL-CLOSED, never allow-all: the whitelist is empty, so an ordinary
// whitelisted-looking branch is DENIED rather than silently let through.
func TestPushGuard_NilResolverFailsClosed(t *testing.T) {
	noRemoteStub(t)
	res := PushGuard(pushInput("git push origin feat/x"), PushGuardConfig{})
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Errorf("nil resolver, push feat/x = %+v, want DENY (fail-closed, empty whitelist — never allow-all)", res)
	}
}

// readViolationLines reads path's JSONL records — a small test helper, distinct from
// internal/viewer's readPushViolations (that package deliberately doesn't depend on
// this one; this is just the mirror-image read for asserting what PushGuard wrote).
func readViolationLines(t *testing.T, path string) []violationRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var out []violationRecord
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec violationRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal violation line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestPushGuard_DenyLogsAParseableViolation(t *testing.T) {
	noRemoteStub(t)
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	res := PushGuard(
		Input{ToolName: "Bash", ToolInput: toolInput{Command: "git push origin master"}, SessionID: "sess-123"},
		PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath},
	)
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Fatalf("expected a deny, got %+v", res)
	}
	if !strings.Contains(res.Stdout, "logged for review") {
		t.Errorf("deny reason = %q, want it to mention the attempt was logged", res.Stdout)
	}

	recs := readViolationLines(t, logPath)
	if len(recs) != 1 {
		t.Fatalf("violations log has %d records, want 1 (log=%+v)", len(recs), recs)
	}
	rec := recs[0]
	if rec.Target != "master" || rec.Reason != "whitelist-miss" || rec.SessionID != "sess-123" || rec.TS == "" || rec.Command == "" {
		t.Errorf("logged record = %+v, want target=master reason=whitelist-miss session_id=sess-123 with ts/command set", rec)
	}
}

func TestPushGuard_FailClosedDenyLogsUnverifiable(t *testing.T) {
	withGitStub(t, func(repoPath string, args ...string) (string, bool) { return "", false })
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	res := PushGuard(pushInput("git push"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath})
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Fatalf("expected a fail-closed deny, got %+v", res)
	}
	recs := readViolationLines(t, logPath)
	if len(recs) != 1 || recs[0].Reason != "unverifiable" {
		t.Errorf("logged records = %+v, want exactly one with reason=unverifiable", recs)
	}
}

func TestPushGuard_AllowLogsNothing(t *testing.T) {
	noRemoteStub(t)
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	res := PushGuard(pushInput("git push origin feat/x"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath})
	if res != allow {
		t.Fatalf("expected allow, got %+v", res)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("allow must not create/write the violations log, but %s exists", logPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", logPath, err)
	}
}

func TestPushGuard_UnwritableLogStillDenies(t *testing.T) {
	noRemoteStub(t)
	// A parent directory that doesn't exist — os.OpenFile can't create it, so every
	// write attempt fails deterministically regardless of OS/user permissions.
	logPath := filepath.Join(t.TempDir(), "no-such-dir", "push-violations.jsonl")
	res := PushGuard(pushInput("git push origin master"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath})
	if !(res.ExitCode == 0 && res.Stdout != "") {
		t.Errorf("deny with an unwritable log path = %+v, want the deny to still happen", res)
	}
	if strings.Contains(res.Stdout, "\\ufffd") {
		t.Errorf("deny result looks corrupted: %q", res.Stdout)
	}
}

func TestPushGuard_ForcePushLogsForceTrue(t *testing.T) {
	noRemoteStub(t)
	logPath := filepath.Join(t.TempDir(), "push-violations.jsonl")
	PushGuard(pushInput("git push --force origin main"), PushGuardConfig{EffectiveWhitelist: staticWL(defaultWL), ViolationsLogPath: logPath})
	recs := readViolationLines(t, logPath)
	if len(recs) != 1 || !recs[0].Force {
		t.Errorf("force-push violation record = %+v, want Force=true", recs)
	}
}
