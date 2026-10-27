package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"mgpp.dev/leadkit/internal/config"
)

// This is NO LONGER a verbatim bash port (see docs/build-tasks/pr-guard-authority.md)
// — it is now the authority-aware, multi-forge source. ~/.claude/hooks/
// pr-repo-guard.sh remains the legacy, repo-blind fallback for sessions that
// haven't wired `leadkit hook pr-guard` as their PreToolUse hook yet (via `leadkit
// init` — see docs/PORTABLE-OPERATING-KIT.md's adoption note). The three original
// deny messages (prCreateReason/prEditReason/prMergeReason) are fixed text,
// followed by scopedTokenNote — every repo with no matching `authority.<repo>` entry,
// or one that grants no raw verb, gets exactly those messages (locked by tests). Unlike every other hook in this package, this one takes the resolved
// *config.Profile directly (per the spec's own explicit instruction) rather than
// a hand-rolled plain-struct config — the authority schema it reads is
// non-trivial enough that a parallel struct would just be duplication.
//
// THE ONE INVARIANT: a naive "can_open_pr -> allow raw create" would let a repo
// bypass its own gated wrapper — can_open_pr is permission to open a PR AT ALL (via
// the wrapper), not permission to do it RAW. Raw access is a SEPARATE, additive,
// explicit grant: RawPRVerbs. A repo that grants none stays byte-for-byte denied.

// forgeSpec is one supported forge's CLI shape.
type forgeSpec struct {
	Name  string     // "github" | "gitlab" — also selects deny-message wording
	CLI   string     // "gh" | "glab"
	Noun  string     // "pr" | "mr"
	Verbs []verbSpec // this forge's own verb spellings, in match-priority order
}

// verbSpec pairs a forge's own verb spelling with the CANONICAL verb
// (create|edit|merge) that all authority/owner-check logic is written against —
// gitlab's "update" IS an edit, even though the word differs.
type verbSpec struct {
	Raw       string
	Canonical string
}

// forges is the built-in forge table (Addendum A). Extensible later via a
// `forges:` profile block — not built here, ship github+gitlab only.
var forges = []forgeSpec{
	{
		Name: "github", CLI: "gh", Noun: "pr",
		Verbs: []verbSpec{{"create", "create"}, {"edit", "edit"}, {"merge", "merge"}},
	},
	{
		Name: "gitlab", CLI: "glab", Noun: "mr",
		Verbs: []verbSpec{{"create", "create"}, {"update", "edit"}, {"merge", "merge"}},
	},
}

// forgeMatch is one recognized `<cli> <noun> <verb>` invocation.
type forgeMatch struct {
	Forge     forgeSpec
	RawVerb   string // the forge's OWN verb spelling (e.g. "update" for gitlab-edit) — used in deny text
	Canonical string // "create" | "edit" | "merge"
}

// matchForgeVerb scans cmd against every forge/verb combination, using the SAME
// boundary rule the original bash's grep -E (and the pre-existing prVerbPattern)
// used: the verb preceded by start-of-string, a shell separator (;, &, |), or
// whitespace, and followed by whitespace or end-of-string. No match -> ok=false
// (unchanged: allow).
func matchForgeVerb(cmd string) (forgeMatch, bool) {
	for _, f := range forges {
		for _, v := range f.Verbs {
			re := regexp.MustCompile(`(^|[;&|]|\s)` + regexp.QuoteMeta(f.CLI) + `\s+` + regexp.QuoteMeta(f.Noun) + `\s+` + regexp.QuoteMeta(v.Raw) + `(\s|$)`)
			if re.MatchString(cmd) {
				return forgeMatch{Forge: f, RawVerb: v.Raw, Canonical: v.Canonical}, true
			}
		}
	}
	return forgeMatch{}, false
}

// prCreateReason/prEditReason/prMergeReason are the GENERIC base deny messages that
// point a raw forge PR verb at its governed `leadkit pr <verb>` replacement — locked
// by pr_guard_test.go (compared against these symbols plus scopedTokenNote) for the no-hint case.
// They name NO adopter script path; an org that keeps a legacy/local PR helper appends
// a pointer to it via the pr_fallback_hint config field (see denyForVerb / withHint).
// Reword only alongside the test.
const (
	prCreateReason = "Raw `gh pr create` / `glab mr create` is disabled. Use `leadkit pr create --base <b> --head <branch> --title ... --body-file ...` — the governed, forge-aware path that enforces authority.<repo> (draft-only + required gates) before opening the PR/MR. A repo with no authority.<repo> entry is refused — the lead escalates to the user."
	prEditReason   = "Raw `gh pr edit` / `glab mr update` is disabled. Use `leadkit pr edit <pr-number> --title ... --body-file ...` — the governed, forge-aware path that enforces authority.<repo> (default-deny; owner_prs_only fails closed) and can only amend title/body (never undraft, ready, or merge). A repo with no authority.<repo> entry is refused — the lead escalates to the user."
	prMergeReason  = "Raw `gh pr merge` is disabled. Use `leadkit pr merge --pr <number> [--method squash|merge|rebase] [--user-approved]` — the governed, forge-aware path that enforces authority.<repo> (default-deny; can_merge + merge_grant/--user-approved to proceed) then the full merge ladder (PR OPEN + not-draft + MERGEABLE + CLEAN + no skip-ci HEAD + CI-green + review-gate 2/2 on the HEAD sha) and strips any skip-ci directive from the squash body. --user-approved substitutes for the review-gate 2/2 ONLY — never for the CI-green / mergeable-clean / skip-ci checks. Merges are lead-only."
)

// withHint appends the adopter's optional pr_fallback_hint to a base deny message —
// an empty hint returns base unchanged (so the no-hint case stays byte-identical to
// the const, which the tests lock), a set hint is appended as a parenthetical trailer.
// This is how an org points at a legacy/local PR helper WITHOUT baking its path into
// the source.
func withHint(base, hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return base
	}
	return base + " (" + hint + ")"
}

// glabCreateReason/glabEditReason/glabMergeReason are gitlab's own deny messages
// — new (gitlab has no legacy bash precedent), but same tone/shape.
const (
	glabCreateReason = "Raw `glab mr create` is disabled. This profile has no configured GitLab wrapper for PR creation — the lead escalates to the user."
	glabEditReason   = "Raw `glab mr update` is disabled. This profile has no configured GitLab wrapper for PR edits — the lead escalates to the user."
	glabMergeReason  = "Raw `glab mr merge` is disabled. Merges are lead-only, regardless of repo authority — the lead escalates to the user."
)

// scopedTokenNote ends every refusal of a raw forge verb. This guard reads one command line and
// cannot see every spelling of the CLI; the control that holds whatever the spelling is the
// token the CLI runs with, so the refusal says so instead of only blocking.
const scopedTokenNote = " (This guard reads text. The durable bound on which repositories the forge CLI can act on is a fine-grained token limited to the repos agents may use: `leadkit doctor` reports when the token is broader than the grant, and docs/PORTABLE-OPERATING-KIT.md says how to set one up.)"

// denyForVerb is the deny for m's forge+verb — used whenever the resolved authority
// doesn't grant this verb raw (no entry, unresolvable origin, verb not in RawPRVerbs,
// or — for merge — unconditionally, always). hint is the adopter's optional
// pr_fallback_hint, appended to the github base messages (which name the governed
// path); an empty hint adds nothing to the const but scopedTokenNote.
func denyForVerb(m forgeMatch, hint string) Result {
	switch m.Forge.Name {
	case "github":
		switch m.Canonical {
		case "create":
			return jsonDeny(withHint(prCreateReason, hint) + scopedTokenNote)
		case "edit":
			return jsonDeny(withHint(prEditReason, hint) + scopedTokenNote)
		default:
			return jsonDeny(withHint(prMergeReason, hint) + scopedTokenNote)
		}
	default: // gitlab
		switch m.Canonical {
		case "create":
			return jsonDeny(glabCreateReason + scopedTokenNote)
		case "edit":
			return jsonDeny(glabEditReason + scopedTokenNote)
		default:
			return jsonDeny(glabMergeReason + scopedTokenNote)
		}
	}
}

// draftFlagPattern matches ONLY `--draft` — F5 fix: `-d` is NOT a draft shorthand
// on either forge (it's glab's own `-d`/`--description` value flag, which takes
// an argument); treating it as a draft marker let an unrelated `-d <text>` on
// GitHub satisfy a draft-only requirement it never meant to.
var draftFlagPattern = regexp.MustCompile(`(^|\s)--draft(\s|$)`)

func hasDraftFlag(cmd string) bool {
	return draftFlagPattern.MatchString(cmd)
}

// prNumberURLPattern extracts a PR/MR number from a URL form
// (.../pull/42, .../merge_requests/42) — checked as a fallback when a
// command's positional argument isn't a bare integer.
var prNumberURLPattern = regexp.MustCompile(`/(?:pull|merge_requests)/(\d+)\b`)

// valueFlags lists, per forge, the flags that consume the NEXT token as their
// OWN value rather than being a bare boolean — needed to correctly skip past
// them when scanning for the command's true positional argument (the PR/MR
// number, and the -R/--repo target). F3 fix: the old extractPRNumber matched
// ANY standalone digit run anywhere in the command, so a number embedded in
// --title/--body ("PR #42 follow-up") was wrongly taken as the PR number,
// making owner_prs_only check the WRONG PR (a false-allow if the caller
// happened to own that unrelated number). An unrecognized flag is treated as a
// bare boolean (skipped alone) — if that guess is wrong for some flag not
// listed here, the worst case is failing to find a real positional at all,
// which correctly resolves to "" (fail-closed), never a wrong number.
var valueFlags = map[string]map[string]bool{
	"github": {
		"-R": true, "--repo": true, "--title": true, "--body": true, "--body-file": true,
		"--base": true, "-B": true, "--head": true, "-H": true,
		"--add-assignee": true, "--remove-assignee": true, "--add-label": true, "--remove-label": true,
		"--add-reviewer": true, "--remove-reviewer": true, "--add-project": true, "--remove-project": true,
		"--milestone": true,
	},
	"gitlab": {
		"-R": true, "--repo": true, "--title": true, "-t": true, "--description": true, "-d": true,
		"--label": true, "-l": true, "--assignee": true, "-a": true, "--reviewer": true,
		"--milestone": true, "-m": true, "--target-branch": true, "-b": true, "--source-branch": true,
	},
}

// prCommandArgs scans cmd's fields AFTER the matched verb for: repoFlag (the
// value of -R/--repo, wherever it appears — F1's fix, an explicit target
// OVERRIDES cwd/`cd`-resolved origin entirely) and prNumber (the first genuine
// positional token — a bare integer, or a /pull/N or /merge_requests/N URL —
// skipping every recognized value-flag's OWN value first, per valueFlags
// above).
//
// parsed=false (verbFieldIndex found no matching triple) is a SECURITY-
// SIGNIFICANT case, not a benign "nothing to report": matchForgeVerb's
// boundary regex treats a bare `;`/`&`/`|` as a word boundary (so
// `echo x;gh pr edit 5 -R victim/repo`, glued with no space after `;`,
// still MATCHES there), but strings.Fields only splits on whitespace, so
// "x;gh" becomes ONE token and this word-level scan can't find the
// cli/noun/verb triple at all. PRGuard's own caller MUST treat
// parsed=false as "an -R may be hiding in here and we can't see it" and
// deny outright — falling through to cwd-origin resolution would let a
// grant on the checked-out repo authorize an action a hidden -R actually
// points at a different one (the same repo-bypass class as F1, just via
// a desync between the two matchers instead of a parsed-but-ignored flag).
func prCommandArgs(cmd string, forgeName string) (repoFlag, prNumber string, parsed bool) {
	fields := strings.Fields(cmd)
	idx := verbFieldIndex(fields, forgeName)
	if idx < 0 {
		return "", "", false
	}
	values := valueFlags[forgeName]
	rest := fields[idx+3:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "-R" || tok == "--repo":
			if i+1 < len(rest) {
				repoFlag = rest[i+1]
			}
			i++
		case strings.HasPrefix(tok, "--repo="):
			repoFlag = strings.TrimPrefix(tok, "--repo=")
		case strings.HasPrefix(tok, "-"):
			if values[tok] {
				i++ // this flag's own value — skip it too
			}
		default:
			if prNumber == "" {
				prNumber = parsePRNumberToken(tok)
			}
		}
	}
	return repoFlag, prNumber, true
}

// verbFieldIndex finds the field index of the FIRST <cli> <noun> <verb> triple
// for forgeName in fields (word-level, matching matchForgeVerb's own forge/verb
// pairing) — the position prCommandArgs scans forward from. -1 if not found
// (e.g. the boundary-regex matched a shape this simpler word-level scan
// doesn't recognize, such as a CLI glued to a separator with no space) — see
// prCommandArgs's own doc comment for why that MUST be treated as deny, not a
// benign fallback.
func verbFieldIndex(fields []string, forgeName string) int {
	var f forgeSpec
	for _, cand := range forges {
		if cand.Name == forgeName {
			f = cand
			break
		}
	}
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] != f.CLI || fields[i+1] != f.Noun {
			continue
		}
		for _, v := range f.Verbs {
			if fields[i+2] == v.Raw {
				return i
			}
		}
	}
	return -1
}

// parsePRNumberToken returns tok itself if it's a bare integer, or the number
// extracted from a /pull/N or /merge_requests/N URL — "" if tok is neither
// (the positional we found isn't recognizable as a PR/MR reference at all;
// callers do NOT keep scanning past it — see prCommandArgs's own doc comment).
func parsePRNumberToken(tok string) string {
	if isAllDigits(tok) {
		return tok
	}
	if m := prNumberURLPattern.FindStringSubmatch(tok); m != nil {
		return m[1]
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cdPrefix says which repo a raw PR verb runs in, from the `cd` that moves the shell there.
// gh/glab carry no `-C <path>` flag of their own, so the repo is the directory the line has
// moved to. It reads the line the way the push gate does (cdCarry): a plain `cd <path>` sets the
// directory, and it reaches the verb only across `&&` (`cd a && cd b && gh …` resolves against b;
// `true && cd V && gh …` against V). Over `;`, `||`, `&` or a pipe the cd may have failed, and a
// `)` closes the subshell it ran in, so those are not read as a target: a loose carry is unknown,
// a closed one moves nothing.
//
// A repo that cannot be named is unknown, because the authority that applies is that repo's and
// the verb would otherwise be judged by the session's own directory, whose grant it has not
// earned: `pushd`, `popd`, `builtin cd`, `command cd`, a cd behind an assignment or inside a
// group, a computed, globbed or assigned target, and a line where the verb's segment cannot be
// told. Lexing is the push hooks' (splitSegments), so quotes, redirects and heredoc bodies are read
// the way they read them.
//
// Every segment that RUNS the raw verb is judged, and only those: a segment runs it when its
// command word (past assignments and keywords) is the forge CLI and its words match the verb. A
// segment that merely MENTIONS the verb (`echo gh pr create`, a commit message naming it) runs
// nothing and is no anchor; treating it as one would let a decoy hide the real `cd V && gh pr
// create` behind it. A `cd` after the last such segment cannot move any verb and is ignored; an
// earlier gh or glab command that is not the verb moves nothing, so a `cd` after it still decides
// where the verb runs. A line with several verbs is read only if they all run in the same place.
func cdPrefix(cmd string) (repoPath string, ok bool, unknown string) {
	segs, _, _ := splitSegments(cmd)
	assigned := assignedVars(cmd)
	var tr cdTracker
	type carry struct {
		path string
		ok   bool
	}
	var carries []carry
	for i, sg := range segs {
		tr.step(sg.link)
		pend := tr.current()
		if segmentRunsRawVerb(sg.words) {
			repo, known, why := carriedTarget(pend)
			if why != "" {
				return "", false, why
			}
			carries = append(carries, carry{repo, known})
			continue
		}
		name, args, isCd := dirChange(sg.words)
		if !isCd {
			continue
		}
		if name != "cd" || sg.words[0] != "cd" {
			// Not read, and not a reason to refuse by itself: it only matters if a verb follows.
			tr.set(cdCarry{why: "a `" + name + "` (or one behind an assignment or in a group) moves the shell where the guard cannot read the target, so where the verb runs is not read", at: i})
			continue
		}
		target, why := dirChangeTarget(name, args)
		if isNoopCd(name, target, why) {
			continue
		}
		if why == "" {
			if w := pathUnknownWhy(target, assigned); w != "" {
				why = "the `cd` target " + w
			}
		}
		target, why = tr.resolveRelative(name, target, why)
		tr.set(cdCarry{path: target, why: why, at: i})
	}
	if len(carries) == 0 {
		// No segment runs the verb on its own (a wrapper such as `env`, `sudo` or `xargs`, or a
		// glued line): a cd in force cannot be shown to be irrelevant to it.
		if tr.current().active {
			return "", false, "a `cd` is in force but the guard cannot tell which command on the line runs the verb"
		}
		return "", false, ""
	}
	for _, c := range carries[1:] {
		if c != carries[0] {
			return "", false, "the line runs the verb more than once, from different places"
		}
	}
	return carries[0].path, carries[0].ok, ""
}

// segmentRunsRawVerb reports whether a segment's COMMAND is a raw forge verb: its command word
// is the forge CLI and its words from there match the verb.
func segmentRunsRawVerb(words []string) bool {
	ci := commandStart(words)
	if ci >= len(words) {
		return false
	}
	m, matched := matchForgeVerb(strings.Join(words[ci:], " "))
	return matched && path.Base(words[ci]) == m.Forge.CLI
}

// carriedTarget is what a directory change in force at a verb means for it: the repo it names, or
// why it cannot be named. A change that is not in force names nothing and is no reason to refuse.
func carriedTarget(c cdCarry) (repoPath string, ok bool, why string) {
	switch {
	case !c.active:
		return "", false, ""
	case c.why != "":
		return "", false, c.why
	case c.loose:
		return "", false, "a `cd` joined to the verb by anything but `&&` may have failed, so the verb may run where the session stands"
	}
	return c.path, true, ""
}

// apiMergePattern matches a raw `gh api`/`glab api` call whose endpoint path
// targets a PR/MR's own merge action — F2 (partial mitigation): `gh pr merge`/
// `glab mr merge` are the ONLY verbs matchForgeVerb recognizes, but `gh api -X
// PUT repos/o/r/pulls/N/merge` performs the exact same merge via the raw REST
// escape hatch, invisible to that verb table. This closes the specific,
// highest-risk "raw merge via the API" gap; `gh api`/`glab api` are general
// REST clients that can express far more than merge (raw create/edit via API
// calls, arbitrary repo mutations, ...) — those residual paths are NOT covered
// here and are a known, accepted gap, not a solved one.
// ".*" (not "\S*") between "api" and the path — real invocations carry flags
// (-X PUT, -f, ...) as separate space-separated tokens before the endpoint
// argument, e.g. `gh api -X PUT repos/o/r/pulls/5/merge`.
var apiMergePattern = regexp.MustCompile(`(^|[;&|]|\s)(gh|glab)\s+api\s+.*(pulls|merge_requests)/\d+/merge`)

func matchAPIMerge(cmd string) (forgeMatch, bool) {
	m := apiMergePattern.FindStringSubmatch(cmd)
	if m == nil {
		return forgeMatch{}, false
	}
	for _, f := range forges {
		if f.CLI == m[2] {
			return forgeMatch{Forge: f, RawVerb: "api", Canonical: "merge"}, true
		}
	}
	return forgeMatch{}, false
}

// resolveAuthorityByTarget substring-matches target (an origin remote URL, OR
// an explicit `-R owner/repo`/`--repo owner/repo` value — F1's fix) against
// prof.Authority's keys, the same convention EnforcedRemotes/push-guard's own
// target-repo whitelist resolver already use. ok=false (no matching key, or a nil
// profile/empty target) means: existing verbatim deny — fail-closed.
func resolveAuthorityByTarget(prof *config.Profile, target string) (config.AuthoritySpec, bool) {
	if prof == nil || target == "" {
		return config.AuthoritySpec{}, false
	}
	for repoKey, spec := range prof.Authority {
		if repoKey != "" && strings.Contains(target, repoKey) {
			return spec, true
		}
	}
	return config.AuthoritySpec{}, false
}

// resolveAuthority resolves repoPath's origin remote (via the package's shared
// runGit — the SAME injectable push-guard uses, stubbed in tests) and defers
// the actual matching to resolveAuthorityByTarget. Used only when the command
// carries no explicit -R/--repo (see PRGuard) — an explicit target always
// wins over cwd's origin.
func resolveAuthority(prof *config.Profile, repoPath string) (config.AuthoritySpec, bool) {
	if prof == nil {
		return config.AuthoritySpec{}, false
	}
	url, ok := runGit(repoPath, "remote", "get-url", "origin")
	if !ok || url == "" {
		return config.AuthoritySpec{}, false
	}
	return resolveAuthorityByTarget(prof, url)
}

// prAuthorLookup resolves a PR/MR's author login and the authenticated forge
// user's own login, for owner_prs_only's edit check — injectable so tests never
// shell out / hit the network (like runGit for origin resolution). ok=false
// means the lookup failed for ANY reason (network, auth, missing number, parse) —
// callers MUST treat that as deny, never allow. repoFlag (F1 fix), when set, is
// passed through as the SAME -R/--repo target PRGuard itself resolved authority
// against — the lookup must check the PR the command actually targets, not
// whatever repo happens to be checked out at repoPath.
//
// Real (non-stub) shape: github `gh pr view <n> [-R <repoFlag>] --json author
// -q .author.login` + `gh api user -q .login`; gitlab `glab mr view <n> [-R
// <repoFlag>]` (parsing its Author field) + `glab api user -q .username`. This
// has never been run for real (outward-facing, HELD) — the exact flag/field
// names may need a small adjustment on first live use; the INJECTABLE SEAM is
// what matters for safety.
var prAuthorLookup = func(m forgeMatch, repoPath, repoFlag, prNumber string) (author, self string, ok bool) {
	if prNumber == "" {
		return "", "", false
	}
	switch m.Forge.Name {
	case "github":
		args := []string{"pr", "view", prNumber}
		if repoFlag != "" {
			args = append(args, "-R", repoFlag)
		}
		args = append(args, "--json", "author", "-q", ".author.login")
		a, aok := runCLI(repoPath, "gh", args...)
		s, sok := runCLI(repoPath, "gh", "api", "user", "-q", ".login")
		if !aok || !sok || a == "" || s == "" {
			return "", "", false
		}
		return a, s, true
	default: // gitlab
		args := []string{"mr", "view", prNumber}
		if repoFlag != "" {
			args = append(args, "-R", repoFlag)
		}
		args = append(args, "-F", "json", "-q", ".author.username")
		a, aok := runCLI(repoPath, "glab", args...)
		s, sok := runCLI(repoPath, "glab", "api", "user", "-q", ".username")
		if !aok || !sok || a == "" || s == "" {
			return "", "", false
		}
		return a, s, true
	}
}

// ConfirmPRAuthor is the exported owner_prs_only check for the GOVERNED `leadkit pr
// edit` path. It REUSES this package's prAuthorLookup seam (it does NOT fork it — tests
// that stub prAuthorLookup drive this too), applying the SAME fail-closed semantics as
// PRGuard's own raw-edit branch: ok=false whenever ownership can't be positively
// confirmed for ANY reason (lookup failed, offline, empty logins, or a different
// author). forgeName is "github"|"gitlab" (the caller picks it from the origin host);
// repoPath is the exec dir; repoFlag is an explicit -R target ("" when authority was
// resolved from the local clone's origin, as `leadkit pr edit` does). Callers MUST
// treat ok=false as DENY — never allow on an unconfirmed check.
func ConfirmPRAuthor(forgeName, repoPath, repoFlag, prNumber string) (ok bool, reason string) {
	var fm forgeMatch
	for _, f := range forges {
		if f.Name == forgeName {
			fm = forgeMatch{Forge: f, RawVerb: "edit", Canonical: "edit"}
			break
		}
	}
	if fm.Forge.Name == "" {
		return false, fmt.Sprintf("unknown forge %q for the owner_prs_only author lookup", forgeName)
	}
	author, self, lookupOK := prAuthorLookup(fm, repoPath, repoFlag, prNumber)
	if !lookupOK || author == "" || self == "" || author != self {
		return false, "the PR/MR author couldn't be confirmed as yours (lookup failed, offline, or a different author)"
	}
	return true, fmt.Sprintf("confirmed %s authored PR/MR %s", self, prNumber)
}

func draftRequiredReason(m forgeMatch) string {
	return fmt.Sprintf(
		"Raw `%s %s %s` requires --draft — this repo's authority grants raw create as draft-only. Add --draft and retry, or use the repo's normal (wrapper/human) PR flow for a non-draft PR.",
		m.Forge.CLI, m.Forge.Noun, m.RawVerb,
	)
}

func ownerMismatchReason(m forgeMatch, prNumber string) string {
	target := prNumber
	if target == "" {
		target = "(number not found in the command)"
	}
	return fmt.Sprintf(
		"Raw `%s %s %s` denied — this repo's authority restricts raw edit to PRs/MRs you authored (owner_prs_only), and %s %s couldn't be confirmed as yours (lookup failed, offline, or a different author). Fail-closed: an unconfirmed ownership check is treated as not yours.",
		m.Forge.CLI, m.Forge.Noun, m.RawVerb, m.Forge.Noun, target,
	)
}

// PRGuard is the authority-aware, multi-forge PR guard — see the package doc
// above. cwd is the invocation's own working directory (the CLI process's,
// e.g. os.Getwd() — used only when cmd carries no `cd <path> &&` prefix of its
// own, mirroring push-guard's repoPath resolution).
func PRGuard(in Input, prof *config.Profile, cwd string) Result {
	cmd := in.ToolInput.Command

	// The adopter's optional pr_fallback_hint, appended to the github deny messages
	// (empty when unset/nil profile → the message is the base const plus scopedTokenNote).
	hint := ""
	if prof != nil {
		hint = prof.PRFallbackHint
	}

	// Ticket 198: a command that creates, copies, renames, deletes, archives or changes the visibility of
	// a repository is the owner's, for everyone, and nothing lifts it. Before any per-repo authority.
	if verb := matchForgeRepoVerb(cmd); verb != "" {
		return jsonDeny(forgeRepoReason(verb) + scopedTokenNote)
	}

	// F2: `gh api`/`glab api` hitting a PR/MR's own merge endpoint is a raw
	// merge the normal verb table can't see — checked FIRST, unconditionally
	// (merge is never grantable, so no authority lookup is even needed).
	if am, ok := matchAPIMerge(cmd); ok {
		return denyForVerb(am, hint)
	}

	m, matched := matchForgeVerb(cmd)
	if !matched {
		return allow
	}

	repoFlag, prNumber, parsed := prCommandArgs(cmd, m.Forge.Name)
	if !parsed {
		// matchForgeVerb (boundary regex, treats ;/&/| as a boundary even with
		// no following space) and verbFieldIndex (word-level, strings.Fields
		// only splits on whitespace) DISAGREED on where the verb is — e.g.
		// "echo x;gh pr edit 5 -R victim/repo" matches the former but "x;gh"
		// is one token to the latter. We can no longer see whether an -R is
		// hiding in there, so this MUST deny rather than fall through to
		// cwd-origin resolution (which would let a grant on the checked-out
		// repo authorize an action a hidden -R actually targets elsewhere).
		return denyForVerb(m, hint)
	}

	// F1: an explicit -R/--repo target OVERRIDES cwd/`cd`-resolved origin
	// entirely — a grant in the checked-out repo must never authorize an
	// action the command actually points at a DIFFERENT repo.
	var spec config.AuthoritySpec
	var ok bool
	var repoPath string
	if repoFlag != "" {
		spec, ok = resolveAuthorityByTarget(prof, repoFlag)
		repoPath = cwd // only used for the author-lookup's exec dir; -R itself targets the real repo
	} else {
		var unknown string
		repoPath, _, unknown = cdPrefix(cmd)
		if unknown != "" {
			return denyForVerb(m, hint) // the authority that applies is the unknown repo's
		}
		if repoPath == "" {
			repoPath = cwd
		}
		spec, ok = resolveAuthority(prof, repoPath)
	}
	if !ok {
		return denyForVerb(m, hint)
	}

	switch m.Canonical {
	case "create":
		if !containsStr(spec.RawPRVerbs, "create") {
			return denyForVerb(m, hint)
		}
		if spec.PRDraftOnly && !hasDraftFlag(cmd) {
			return jsonDeny(draftRequiredReason(m))
		}
		return allow

	case "edit":
		if !containsStr(spec.RawPRVerbs, "edit") {
			return denyForVerb(m, hint)
		}
		if spec.OwnerPRsOnly {
			author, self, lookupOK := prAuthorLookup(m, repoPath, repoFlag, prNumber)
			if !lookupOK || author == "" || self == "" || author != self {
				return jsonDeny(ownerMismatchReason(m, prNumber))
			}
		}
		return allow

	default: // merge — GUARD-RAILED: raw_pr_verbs is never consulted here, ever.
		return denyForVerb(m, hint)
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// runCLI execs `name args...` (in repoPath, if set) and returns stdout trimmed —
// the same shape as runGit, generalized to any CLI (gh/glab, not just git).
// Injectable indirectly via prAuthorLookup itself (tests stub the whole
// prAuthorLookup func var, never runCLI directly, since the exact args differ
// per forge).
var runCLI = func(repoPath string, name string, args ...string) (out string, ok bool) {
	cmd := exec.Command(name, args...)
	if repoPath != "" {
		cmd.Dir = repoPath
	}
	data, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// hookSpecificOutput / denyPayload mirror the JSON-deny protocol:
// {"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"..."}}
type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

type denyPayload struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// jsonDeny builds the deny-JSON Result — shared by every hook using the JSON-deny
// style (pr-guard, push-guard). Uses encoding/json (not hand-built string
// interpolation) so the reason text is always correctly escaped, unlike the bash's
// jq -n construction this mirrors semantically rather than byte-for-byte — except
// HTML-escaping, which is turned off: encoding/json escapes <, >, and & by default
// (a safety default for HTML contexts that doesn't apply here), and reason text like
// pr-guard's literal "<branch>"/"<pr-number>" placeholders should read exactly as the
// bash's do, not as &lt;branch&gt;.
func jsonDeny(reason string) Result {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(denyPayload{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	}})
	return Result{Stdout: buf.String(), ExitCode: 0}
}
