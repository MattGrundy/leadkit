package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/scrubterms"
)

// stubOrigin makes runGit's "remote get-url origin" call return url for the
// duration of the test, restoring the real func on cleanup — no test here ever
// shells out to git.
func stubOrigin(t *testing.T, url string) {
	t.Helper()
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) {
		return url, true
	}
	t.Cleanup(func() { runGit = orig })
}

// stubUnresolvableOrigin makes runGit fail, as if the origin couldn't be resolved.
func stubUnresolvableOrigin(t *testing.T) {
	t.Helper()
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) { return "", false }
	t.Cleanup(func() { runGit = orig })
}

// stubAuthor makes prAuthorLookup return a fixed (author, self, ok) for the
// duration of the test — no test here ever shells out to gh/glab.
func stubAuthor(t *testing.T, author, self string, ok bool) {
	t.Helper()
	orig := prAuthorLookup
	prAuthorLookup = func(m forgeMatch, repoPath, repoFlag, prNumber string) (string, string, bool) {
		return author, self, ok
	}
	t.Cleanup(func() { prAuthorLookup = orig })
}

func decodeDeny(t *testing.T, res Result) string {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (this style denies via stdout JSON, not exit code)", res.ExitCode)
	}
	var payload denyPayload
	if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
		t.Fatalf("Stdout is not valid JSON: %v (%q)", err, res.Stdout)
	}
	out := payload.HookSpecificOutput
	if out.HookEventName != "PreToolUse" || out.PermissionDecision != "deny" {
		t.Fatalf("hookSpecificOutput = %+v, want PreToolUse/deny", out)
	}
	return out.PermissionDecisionReason
}

// ============================================================================
// REGRESSION LOCK — must pass first. platform-infra (can_open_pr:true, no
// raw_pr_verbs) and "no authority entry at all" must both deny every raw PR
// verb with the base messages followed by scopedTokenNote, verbatim.
// ============================================================================

func TestPRGuard_RegressionLock_CIServicesAllDeniedVerbatim(t *testing.T) {
	stubOrigin(t, "git@github.com:acmecorp/platform-infra.git")
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"platform-infra": {CanPush: true, CanOpenPR: config.BoolOrHuman{Allow: true}},
	}}

	tests := []struct {
		cmd    string
		reason string
	}{
		{"gh pr create --title x --body y", prCreateReason + scopedTokenNote},
		{"gh pr edit 42 --title x", prEditReason + scopedTokenNote},
		{"gh pr merge 42", prMergeReason + scopedTokenNote},
		{"cd /tmp && gh pr create --title x", prCreateReason + scopedTokenNote},
		{"echo hi | gh pr create --title x", prCreateReason + scopedTokenNote},
	}
	for _, tt := range tests {
		res := PRGuard(Input{ToolInput: toolInput{Command: tt.cmd}}, prof, "/repo")
		got := decodeDeny(t, res)
		if got != tt.reason {
			t.Errorf("cmd %q: reason = %q, want VERBATIM %q", tt.cmd, got, tt.reason)
		}
	}
}

func TestPRGuard_RegressionLock_NoAuthorityEntryAllDeniedVerbatim(t *testing.T) {
	stubOrigin(t, "git@github.com:someorg/unrelated-repo.git")
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{}}

	tests := []struct {
		cmd    string
		reason string
	}{
		{"gh pr create --title x", prCreateReason + scopedTokenNote},
		{"gh pr edit 42 --title x", prEditReason + scopedTokenNote},
		{"gh pr merge 42", prMergeReason + scopedTokenNote},
	}
	for _, tt := range tests {
		res := PRGuard(Input{ToolInput: toolInput{Command: tt.cmd}}, prof, "/repo")
		got := decodeDeny(t, res)
		if got != tt.reason {
			t.Errorf("cmd %q: reason = %q, want VERBATIM %q", tt.cmd, got, tt.reason)
		}
	}
}

// TestPRGuard_PRFallbackHint_AppendedWhenSet proves the project-agnostic hint
// mechanism (docs/build-tasks/project-agnostic-defaults.md §3): the base deny message
// names the governed `leadkit pr ...` path generically and mentions NO adopter script
// path, and an adopter's pr_fallback_hint (e.g. a legacy local helper) is appended only
// when configured.
func TestPRGuard_PRFallbackHint_AppendedWhenSet(t *testing.T) {
	stubOrigin(t, "git@github.com:someorg/unrelated-repo.git")

	// No hint → the generic base const plus scopedTokenNote, nothing else (no adopter path baked in).
	base := &config.Profile{Authority: map[string]config.AuthoritySpec{}}
	if got := decodeDeny(t, PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --title x"}}, base, "/repo")); got != prCreateReason+scopedTokenNote {
		t.Errorf("no-hint create reason = %q, want the generic base const plus scopedTokenNote verbatim", got)
	}
	// And the generic base must not leak any adopter script path. The needles are the REAL
	// names, read from the scrub term file rather than written here (a prose stand-in would
	// never appear in the deny message, so it would pass vacuously).
	for _, leak := range adopterNamesIn(t, prCreateReason) {
		t.Errorf("generic base message still names an adopter path %q: %q", leak, prCreateReason)
	}

	// With a hint set, it is appended as a trailer to each github deny message.
	hint := "Legacy helper: ~/.acmecorp/pr-create.sh"
	withHintProf := &config.Profile{PRFallbackHint: hint}
	for _, cmd := range []string{"gh pr create --title x", "gh pr edit 42 --title x", "gh pr merge 42"} {
		got := decodeDeny(t, PRGuard(Input{ToolInput: toolInput{Command: cmd}}, withHintProf, "/repo"))
		if !strings.Contains(got, hint) {
			t.Errorf("cmd %q: deny reason = %q, want it to append the configured hint %q", cmd, got, hint)
		}
	}
}

func TestPRGuard_RegressionLock_NilProfileDeniesVerbatim(t *testing.T) {
	stubOrigin(t, "git@github.com:acmecorp/platform-infra.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --title x"}}, nil, "/repo")
	if got := decodeDeny(t, res); got != prCreateReason+scopedTokenNote {
		t.Errorf("reason = %q, want VERBATIM %q (nil profile must never panic or open up)", got, prCreateReason+scopedTokenNote)
	}
}

func TestPRGuard_UnresolvableOriginDeniesVerbatim(t *testing.T) {
	stubUnresolvableOrigin(t)
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"anything": {RawPRVerbs: []string{"create", "edit"}},
	}}
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --title x"}}, prof, "/repo")
	if got := decodeDeny(t, res); got != prCreateReason+scopedTokenNote {
		t.Errorf("reason = %q, want VERBATIM %q (unresolvable origin fails closed)", got, prCreateReason+scopedTokenNote)
	}
}

func TestPRGuard_NoMatchAllows(t *testing.T) {
	prof := &config.Profile{}
	tests := []string{
		"",
		"gh pr view 42",
		"gh pr list",
		"ghost pr create", // "gh" must be a whole word — "ghost" must not match
		"echo gh pr createx",
	}
	for _, cmd := range tests {
		if got := PRGuard(Input{ToolInput: toolInput{Command: cmd}}, prof, "/repo"); got != allow {
			t.Errorf("PRGuard(%q) = %+v, want allow", cmd, got)
		}
	}
}

// ============================================================================
// GRANT — another-project-shaped authority: raw_pr_verbs:[create,edit],
// pr_draft_only:true, owner_prs_only:true.
// ============================================================================

func teamsBridgeProfile() *config.Profile {
	return &config.Profile{Authority: map[string]config.AuthoritySpec{
		"another-project": {
			CanOpenPR: config.BoolOrHuman{Allow: true}, PRDraftOnly: true, CanMerge: config.BoolOrHuman{Allow: false},
			OwnerPRsOnly: true, RawPRVerbs: []string{"create", "edit"},
		},
	}}
}

func TestPRGuard_Grant_DraftCreateAllowed(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --draft --title x"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("draft create = %+v, want allow", res)
	}
}

func TestPRGuard_Grant_NonDraftCreateDeniedDraftRequired(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --title x"}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == prCreateReason+scopedTokenNote {
		t.Error("non-draft create on a granted repo must NOT get the generic verbatim deny — it's a draft-required deny")
	}
	if reason == "" {
		t.Error("want a non-empty draft-required reason")
	}
}

func TestPRGuard_Grant_EditOwnPRAllowed(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	stubAuthor(t, "alice", "alice", true)
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr edit 42 --title x"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("edit own PR = %+v, want allow", res)
	}
}

func TestPRGuard_Grant_EditOthersPRDenied(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	stubAuthor(t, "bob", "alice", true)
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr edit 42 --title x"}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == prEditReason+scopedTokenNote {
		t.Error("owner-mismatch on a granted repo must NOT get the generic verbatim deny — it's an owner-mismatch deny")
	}
}

func TestPRGuard_Grant_MergeAlwaysDenied(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr merge 42"}}, teamsBridgeProfile(), "/repo")
	if got := decodeDeny(t, res); got != prMergeReason+scopedTokenNote {
		t.Errorf("merge reason = %q, want VERBATIM %q — merge is never grantable, even on a granted repo", got, prMergeReason+scopedTokenNote)
	}
}

// ============================================================================
// MULTI-FORGE — gitlab routes through the same create/edit/merge logic.
// ============================================================================

func TestPRGuard_GitLab_DraftCreateAllowed(t *testing.T) {
	stubOrigin(t, "git@gitlab.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "glab mr create --draft --title x"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("glab draft create = %+v, want allow", res)
	}
}

func TestPRGuard_GitLab_UpdateRoutesToEditLogic(t *testing.T) {
	stubOrigin(t, "git@gitlab.com:org/another-project.git")
	stubAuthor(t, "alice", "alice", true)
	res := PRGuard(Input{ToolInput: toolInput{Command: "glab mr update 7 --title x"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("glab mr update (own MR) = %+v, want allow", res)
	}
}

func TestPRGuard_GitLab_MergeDenied(t *testing.T) {
	stubOrigin(t, "git@gitlab.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "glab mr merge 7"}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == prMergeReason+scopedTokenNote {
		t.Error("gitlab merge deny must be gitlab's OWN message, not github's verbatim one")
	}
}

func TestPRGuard_GitLab_NoGrantDeniesEverything(t *testing.T) {
	stubOrigin(t, "git@gitlab.com:someorg/unrelated.git")
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{}}
	res := PRGuard(Input{ToolInput: toolInput{Command: "glab mr create --title x"}}, prof, "/repo")
	_ = decodeDeny(t, res) // just must be a deny, not allow/panic
}

// ============================================================================
// GUARD-RAILS
// ============================================================================

func TestPRGuard_GuardRail_RawPRVerbsMergeIsIgnored(t *testing.T) {
	stubOrigin(t, "git@github.com:org/misconfigured.git")
	prof := &config.Profile{Authority: map[string]config.AuthoritySpec{
		"misconfigured": {RawPRVerbs: []string{"merge"}}, // misconfigured on purpose
	}}
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr merge 42"}}, prof, "/repo")
	if got := decodeDeny(t, res); got != prMergeReason+scopedTokenNote {
		t.Errorf("reason = %q, want VERBATIM %q — raw_pr_verbs:[merge] must be ignored outright", got, prMergeReason+scopedTokenNote)
	}
}

func TestPRGuard_GuardRail_OwnerLookupFailureDeniesFailClosed(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	stubAuthor(t, "", "", false) // lookup failed (offline, auth error, ...)
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr edit 42 --title x"}}, teamsBridgeProfile(), "/repo")
	_ = decodeDeny(t, res) // must be a deny — fail-closed
}

func TestPRGuard_NonForgeCommandAllows(t *testing.T) {
	prof := &config.Profile{}
	if got := PRGuard(Input{ToolInput: toolInput{Command: "npm test"}}, prof, "/repo"); got != allow {
		t.Errorf("PRGuard(npm test) = %+v, want allow", got)
	}
}

// ============================================================================
// SECURITY REVIEW FIXES — one test per finding (F1-F5).
// ============================================================================

// F1: an explicit -R/--repo target must be resolved on its OWN merits, never
// inherit a grant from cwd's origin, and never let cwd's grant leak into a
// DIFFERENT repo named via -R.
func TestPRGuard_F1_ExplicitRepoFlagOverridesCWDOrigin_GrantedTarget(t *testing.T) {
	// cwd's origin is UNRELATED/ungranted; -R names the GRANTED repo — must allow.
	stubOrigin(t, "git@github.com:someorg/unrelated.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr create --draft --title x -R org/another-project"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("want allow — -R names a granted repo even though cwd's origin doesn't, got %+v", res)
	}
}

func TestPRGuard_F1_ExplicitRepoFlagPreventsCWDGrantLeak(t *testing.T) {
	// cwd's origin IS the granted repo; -R redirects to an UNRELATED/ungranted
	// one — must deny (the old bug: authority was read from cwd, not -R).
	stubOrigin(t, "git@github.com:org/another-project.git")
	prof := teamsBridgeProfile()
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr edit 42 --title x -R victim/repo"}}, prof, "/repo")
	reason := decodeDeny(t, res)
	if reason == "" {
		t.Fatal("want a deny — another-project's grant must not leak to -R's actual target")
	}
}

// F2: `gh api`/`glab api` hitting a PR's own merge endpoint is a raw merge the
// verb table alone can't see — must still deny, with merge's own message.
func TestPRGuard_F2_APIMergeEndpointDenied(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh api -X PUT repos/o/r/pulls/5/merge"}}, teamsBridgeProfile(), "/repo")
	if got := decodeDeny(t, res); got != prMergeReason+scopedTokenNote {
		t.Errorf("reason = %q, want VERBATIM %q — gh api merge escape hatch must still be denied", got, prMergeReason+scopedTokenNote)
	}
}

func TestPRGuard_F2_APIMergeEndpointDenied_GitLab(t *testing.T) {
	stubOrigin(t, "git@gitlab.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: "glab api -X PUT projects/1/merge_requests/5/merge"}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == prMergeReason+scopedTokenNote {
		t.Error("gitlab api-merge deny must use gitlab's own message, not github's")
	}
	if reason == "" {
		t.Fatal("want a deny")
	}
}

// F3: a number embedded in --title/--body must NEVER be mistaken for the PR
// number — only the command's true positional argument counts.
func TestPRGuard_F3_TitleEmbeddedNumberNeverUsedAsPRNumber(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	var gotPRNumber string
	orig := prAuthorLookup
	prAuthorLookup = func(m forgeMatch, repoPath, repoFlag, prNumber string) (string, string, bool) {
		gotPRNumber = prNumber
		return "alice", "alice", true // pretend it's always "yours" — irrelevant, we're checking the NUMBER
	}
	defer func() { prAuthorLookup = orig }()

	res := PRGuard(Input{ToolInput: toolInput{Command: `gh pr edit 42 --title "PR #99 follow-up"`}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Fatalf("setup: want allow (stubbed as own PR), got %+v", res)
	}
	if gotPRNumber != "42" {
		t.Errorf("prNumber passed to the author lookup = %q, want 42 (the true positional, NOT 99 from --title)", gotPRNumber)
	}
}

// F4: `cd a && cd b && ...` must resolve against b (the LAST cd), not a.
func TestPRGuard_F4_LastCdWinsOverFirst(t *testing.T) {
	var gotRepoPath string
	orig := runGit
	runGit = func(repoPath string, args ...string) (string, bool) {
		gotRepoPath = repoPath
		return "git@github.com:org/another-project.git", true
	}
	defer func() { runGit = orig }()

	PRGuard(Input{ToolInput: toolInput{Command: "cd /repo-a && cd /repo-b && gh pr create --draft --title x"}}, teamsBridgeProfile(), "/fallback")
	if gotRepoPath != "/repo-b" {
		t.Errorf("resolved repoPath = %q, want /repo-b (the LAST cd), not the first", gotRepoPath)
	}
}

// F5: `-d` is not a GitHub draft shorthand — an unrelated `-d <value>` must NOT
// satisfy a draft-only requirement.
func TestPRGuard_F5_DashDIsNotADraftFlagOnGitHub(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	res := PRGuard(Input{ToolInput: toolInput{Command: `gh pr create --title x -d "some value"`}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == "" {
		t.Fatal("want a deny — -d must NOT satisfy pr_draft_only on GitHub")
	}
	if reason == prCreateReason+scopedTokenNote {
		t.Error("want the draft-required deny specifically (create IS granted, just not draft), not the generic verbatim one")
	}
}

// F1 RESIDUAL: matchForgeVerb's boundary regex treats a bare ";" as a
// boundary even with NO following space ("x;gh"), but strings.Fields only
// splits on whitespace, so verbFieldIndex can't find the triple in that same
// glued command — the two matchers desync. Falling through to cwd-origin
// resolution in that case would let a grant on the checked-out repo authorize
// an action a HIDDEN -R (invisible to the word-level scan) actually targets
// elsewhere — the same repo-bypass class as F1, via desync instead of a
// parsed-but-ignored flag. Must deny outright instead.
func TestPRGuard_F1Residual_GluedSeparatorDesyncDeniesRatherThanFallThrough(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git") // cwd IS granted create+edit, no owner_prs_only
	res := PRGuard(Input{ToolInput: toolInput{Command: "echo x;gh pr edit 5 -R victim/repo"}}, teamsBridgeProfile(), "/repo")
	reason := decodeDeny(t, res)
	if reason == "" {
		t.Fatal("want a deny — a matched-but-unparseable forge command must never fall through to cwd's grant")
	}
}

func TestPRGuard_F1Residual_NormalSpacedCommandUnaffected(t *testing.T) {
	stubOrigin(t, "git@github.com:org/another-project.git")
	stubAuthor(t, "alice", "alice", true)
	res := PRGuard(Input{ToolInput: toolInput{Command: "gh pr edit 5 --title x"}}, teamsBridgeProfile(), "/repo")
	if res != allow {
		t.Errorf("a normal, properly-spaced granted edit must still work, got %+v", res)
	}
}

// adopterNamesIn returns the scrub terms (from the external term file; none when it is absent)
// that msg contains, case-insensitively.
func adopterNamesIn(t *testing.T, msg string) []string {
	t.Helper()
	var found []string
	for _, term := range scrubterms.Optional(t).ForTests() {
		if strings.Contains(strings.ToLower(msg), term) {
			found = append(found, term)
		}
	}
	return found
}

// TestAdopterNamesIn_ReadsTheTermFile pins the move of the adopter script names out of this file:
// a term in the file must be found in a message that contains it, and nothing is found when the
// file lacks it or is absent. Without this the check above could ignore the file and stay green.
func TestAdopterNamesIn_ReadsTheTermFile(t *testing.T) {
	msg := "Use the Frobnicate-Wrapper to open it."
	terms := filepath.Join(t.TempDir(), "terms.txt")
	if err := os.WriteFile(terms, []byte("// fixture\nfrobnicate-wrapper\nunrelated-name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(scrubterms.EnvVar, terms)
	if got := adopterNamesIn(t, msg); len(got) != 1 || got[0] != "frobnicate-wrapper" {
		t.Fatalf("with the term in the file: got %v, want [frobnicate-wrapper]", got)
	}
	t.Setenv(scrubterms.EnvVar, filepath.Join(t.TempDir(), "absent.txt"))
	if got := adopterNamesIn(t, msg); len(got) != 0 {
		t.Fatalf("with no term file nothing specific can be found, got %v", got)
	}
}
