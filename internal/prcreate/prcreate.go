// Package prcreate is the governed replacement for a legacy shell PR-create wrapper (which
// was literally `exec gh pr create --repo owner/repo "$@"` — zero governance,
// hardcoded to one repo). It resolves a repo's authority.<repo> policy from the loaded
// leadkit profile and ENFORCES it — the policy config.go's own doc still calls
// "data only" — before shelling out to the forge CLI (gh for GitHub, glab for GitLab).
//
// The whole reason for the port is testability: the governance decision (refuse /
// draft-forced / gate-checked / the exact forge argv) is computed by the PURE Decide
// function returning a Decision struct, with NO I/O. Both `--dry-run` and the real
// execution path render/execute that same struct. The forge CLI runner, the gate
// check, and origin/HEAD resolution are all injected, so tests never invoke a real
// gh/glab or touch a real GitHub/GitLab.
//
// Scope is `create` only. `pr merge` (the much larger merge-wrapper port) is a follow-on
// that reuses this package's authority resolver + Runner + gate seam.
package prcreate

import (
	"fmt"
	"os/exec"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/redact"
)

// Request is the uniform, forge-agnostic PR-create request as the CLI expresses it —
// mapped onto each forge's own flag names by the forge impls.
type Request struct {
	Base     string   // target branch (gh --base / glab --target-branch)
	Head     string   // source branch (gh --head / glab --source-branch)
	Title    string   // PR/MR title
	BodyFile string   // body file path (gh --body-file / glab --description)
	Draft    bool     // caller's draft intent (pr_draft_only can force this true)
	Extra    []string // passthrough forge flags, appended after the composed flags
}

// Decision is the pure, assertable outcome of the governance resolution. Both the
// dry-run plan and the real execution path render/execute this same struct.
type Decision struct {
	Allowed bool     // false: a governance refusal — Reason says why, Argv is nil
	Reason  string   // human-readable verdict (the refusal message, or the allow summary)
	Draft   bool     // the resolved draft decision (caller intent OR pr_draft_only)
	Argv    []string // the exact forge argv that WOULD run (only when Allowed)
}

// DecideInput is everything Decide needs — all already-resolved, no I/O. The caller
// resolves origin, authority, the HEAD sha, and the gate outcome, then Decide applies
// the governance ordering deterministically.
type DecideInput struct {
	OriginURL    string               // the target repo's origin remote URL
	AuthKey      string               // the matched authority.<repo> key (for messages)
	Spec         config.AuthoritySpec // the resolved authority policy
	HasAuthority bool                 // did any authority key match the origin?
	// ProfileMissing, when non-empty, says no profile was found at all (what was searched), so
	// "no authority matched" is an unresolved environment and not a policy. See noAuthorityReason.
	ProfileMissing string
	Req            Request // the composed PR-create request
	UserApproved   bool    // explicit human substitute for the auto-gate
	HeadSHA        string  // the head branch's HEAD sha (for the gate message)
	GateOK         bool    // did the review gate pass for HeadSHA?
	GateReason     string  // the gate's reason (shown when it did not pass)
	// LineageOK says every branch the head is built on is itself gated (CheckLineage). It is a
	// separate input from GateOK, with no default: a caller that forgets to resolve it is refused,
	// because a gated tip over ungated commits is exactly what the head's own gate cannot see.
	LineageOK     bool
	LineageReason string
}

// noAuthorityReason is the refusal for a repo no authority entry covers. Two different
// facts reach it. ProfileMissing non-empty means no profile was READ, so no entry could have
// matched and the cause is the environment. Otherwise a profile was read and has no entry for
// the origin. Neither advises adding an entry: grants are the repo owner's to give, and an agent
// that follows such advice is writing the thing it was refused.
func noAuthorityReason(what, origin, profileMissing string) string {
	if profileMissing != "" {
		return fmt.Sprintf("cannot judge governed %s for origin %s: %s", what, redact.Origin(origin), profileMissing)
	}
	return fmt.Sprintf("no authority.<repo> policy matches origin %s; authority grants are the repo owner's to give: ask the repo owner whether governed %s should be allowed here (do not add an entry yourself)", redact.Origin(origin), what)
}

// Decide is the PURE governance resolver — no I/O, fully unit-testable. It enforces,
// IN ORDER (default-DENY throughout):
//  1. no authority entry matched the origin → REFUSE (never exec the forge for an
//     ungoverned repo).
//  2. can_open_pr: true → allow; false → refuse ("not wired"); "human" → refuse with a
//     DISTINCT escalation message.
//  3. required_gates (gate-BEFORE-PR): the caller's GateOK must hold UNLESS
//     UserApproved is set (a human substitute; the gate is NEVER silently skipped).
//  4. only after all pass: select the forge, force --draft when pr_draft_only, and
//     compose the exact argv.
func Decide(in DecideInput) Decision {
	// 1. default-deny: an ungoverned repo never reaches the forge.
	if !in.HasAuthority {
		return Decision{Allowed: false, Reason: noAuthorityReason("PR-create", in.OriginURL, in.ProfileMissing)}
	}

	// The passthrough is an allowlist, and nothing is on it: see allowedPassthrough.
	if reason := refusePassthrough("create", in.Req.Extra); reason != "" {
		return Decision{Allowed: false, Reason: reason}
	}

	// 2. can_open_pr — human is distinct from false (escalate vs not-yet-wired).
	if in.Spec.CanOpenPR.Human {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"opening a PR in %s requires a human; the lead escalates to the user", in.AuthKey)}
	}
	if !in.Spec.CanOpenPR.Allow {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"can_open_pr is false for %s — not wired for autonomous PR-create", in.AuthKey)}
	}

	// draft-only wins over the caller's intent — a draft-only repo can never open a
	// ready PR. Computed here so it shows on a gate/forge refusal's Draft field too.
	draft := in.Req.Draft || in.Spec.PRDraftOnly

	// 3. gate-before-PR — not satisfied AND no explicit --user-approved → refuse.
	if !in.UserApproved && !in.GateOK {
		return Decision{Allowed: false, Draft: draft, Reason: fmt.Sprintf(
			"head %s not gated: %s; gate it before opening the PR", shortSHA(in.HeadSHA), in.GateReason)}
	}

	// 3b. lineage: a gated head says nothing about the commits beneath it. Substitutable by
	// --user-approved exactly as the gate is, because it IS the gate, looking down.
	if !in.UserApproved && !in.LineageOK {
		return Decision{Allowed: false, Draft: draft, Reason: fmt.Sprintf(
			"head %s is gated but its lineage is not: %s; pass --user-approved only if a human reviewed what lies beneath",
			shortSHA(in.HeadSHA), reasonOrUnchecked(in.LineageReason))}
	}

	// 4. only now build the forge argv. An unresolvable forge is itself a refusal
	//    (never guess a forge for an unknown host).
	f, err := forgeFor(in.OriginURL, in.Spec)
	if err != nil {
		return Decision{Allowed: false, Draft: draft, Reason: err.Error()}
	}
	argv := f.CreateArgv(prReq{
		OwnerName: OwnerName(in.OriginURL),
		Base:      in.Req.Base,
		Head:      in.Req.Head,
		Title:     in.Req.Title,
		BodyFile:  in.Req.BodyFile,
		Draft:     draft,
		Extra:     in.Req.Extra,
	})
	return Decision{Allowed: true, Draft: draft, Argv: argv, Reason: fmt.Sprintf(
		"%s: allowed for %s (%s -> %s%s)", f.Name(), in.AuthKey, in.Req.Head, in.Req.Base, draftNote(draft))}
}

func reasonOrUnchecked(r string) string {
	if strings.TrimSpace(r) == "" {
		return "the lineage was never checked"
	}
	return r
}

func draftNote(draft bool) string {
	if draft {
		return ", draft"
	}
	return ""
}

// Runner is the forge-CLI executor seam — the real impl execs gh/glab; tests inject a
// fake that CAPTURES argv and never touches a real forge or the network.
type Runner interface {
	Run(argv []string) (stdout string, err error)
}

// ExecRunner is the real Runner: it execs argv[0] with argv[1:].
type ExecRunner struct{}

// Run execs the forge CLI. CombinedOutput so the forge's own error text surfaces.
func (ExecRunner) Run(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("prcreate: empty argv")
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

// Execute renders or executes a Decision — the single orchestration point both the CLI
// and the tests drive. It is pure orchestration around Decide (no resolution of its
// own), so the "runner invoked once with the argv" / "dry-run never invokes the runner"
// / "a refusal never invokes the runner" guarantees are all directly assertable. When
// the decision is refused OR dryRun is set, the runner is NEVER called.
func Execute(in DecideInput, dryRun bool, runner Runner) (Decision, string, error) {
	d := Decide(in)
	if !d.Allowed || dryRun {
		return d, "", nil
	}
	out, err := runner.Run(d.Argv)
	return d, out, err
}

// GateChecker is the gate seam prcreate needs from internal/gate — *gate.Gate already
// satisfies it. Both methods are reused: Check for the default required-layer set
// (preserving gate.Check's scope/bypass/HEAD semantics), ReadRecord to verify an
// authority-pinned CUSTOM required set exactly.
type GateChecker interface {
	Check(sha, repo string) (ok bool, reason string)
	ReadRecord(sha string) (gate.Record, error)
}

// GateReady decides whether headSHA is gated for PR-create, honouring the repo's
// authority.RequiredGates as the required-layer set when it is set, and falling back to
// the gate's default RequiredLayers otherwise. For the default set (the common case,
// and what the shipped authority sample uses) it defers ENTIRELY to gate.Check so the
// scope/bypass/HEAD semantics are reused unchanged; for a custom set it verifies those
// exact layers from the record, so a governed repo's PR gate neither over-requires a
// subset nor silently passes a superset.
func GateReady(g GateChecker, headSHA, repo string, required []string) (bool, string) {
	if len(required) == 0 || sameLayers(required, gate.RequiredLayers) {
		return g.Check(headSHA, repo)
	}
	rec, err := g.ReadRecord(headSHA)
	if err != nil {
		return false, fmt.Sprintf("no review record for %s. Required layers: %s",
			shortSHA(headSHA), strings.Join(required, " "))
	}
	if missing := gate.Missing(rec, required); len(missing) > 0 {
		return false, "layers not ready: " + strings.Join(missing, " ")
	}
	return true, fmt.Sprintf("all layers ready (%s) for %s", strings.Join(required, " "), shortSHA(headSHA))
}

// ResolveAuthority substring-matches originURL against prof.Authority's keys — via the
// SHARED, DETERMINISTIC config.MatchAuthorityKey (the SAME matcher push-guard's
// RepoWhitelist uses), so pr create/merge and the push-guard pick the IDENTICAL entry
// when several keys match: the longest (most-specific) key wins, not a map-random first
// match. Unfiltered (any authority match, regardless of push_whitelist). ok=false (no
// matching key, nil profile, or empty origin) means default-deny: no governed PR-create
// here — unchanged for a single-match config.
func ResolveAuthority(prof *config.Profile, originURL string) (key string, spec config.AuthoritySpec, ok bool) {
	if prof == nil || originURL == "" {
		return "", config.AuthoritySpec{}, false
	}
	return config.MatchAuthorityKey(originURL, prof.Authority, false)
}

// prReq is the forge-facing request — Request plus the derived owner/name and the
// resolved draft decision.
type prReq struct {
	OwnerName string
	Base      string
	Head      string
	Title     string
	BodyFile  string
	Draft     bool
	Extra     []string
}

// forge abstracts the two supported forges — they differ in binary, vocabulary, AND
// flag names, so the SAME Decision path composes either argv from the same prReq
// (create) or editReq (edit — see edit.go).
type forge interface {
	CreateArgv(req prReq) []string
	EditArgv(req editReq) []string
	MergeArgv(req mergeReq) []string
	Name() string
}

// githubForge composes `gh pr create ...`.
type githubForge struct{}

func (githubForge) Name() string { return "github" }

func (githubForge) CreateArgv(r prReq) []string {
	argv := []string{"gh", "pr", "create",
		"--repo", r.OwnerName,
		"--base", r.Base,
		"--head", r.Head,
		"--title", r.Title,
		"--body-file", r.BodyFile,
	}
	if r.Draft {
		argv = append(argv, "--draft")
	}
	return append(argv, r.Extra...)
}

// gitlabForge composes `glab mr create ...`. GitLab calls it an MR and its flag names
// differ: --source-branch/--target-branch (not --head/--base) and --description (not
// --body-file). The description value is the body-file PATH here — the same
// never-run-for-real caveat internal/hook's prAuthorLookup carries: on first live use
// this may need to become the file's CONTENTS, but the injectable seam and argv shape
// are what matter for governance + tests.
type gitlabForge struct{}

func (gitlabForge) Name() string { return "gitlab" }

func (gitlabForge) CreateArgv(r prReq) []string {
	argv := []string{"glab", "mr", "create",
		"--source-branch", r.Head,
		"--target-branch", r.Base,
		"--title", r.Title,
		"--description", r.BodyFile,
	}
	if r.Draft {
		argv = append(argv, "--draft")
	}
	return append(argv, r.Extra...)
}

// forgeFor selects the forge from the origin's HOST: a host containing "github" → gh,
// "gitlab" → glab; else the per-repo authority.<repo>.forge override; else REFUSE
// (never guess a forge for an unknown self-hosted host).
func forgeFor(originURL string, spec config.AuthoritySpec) (forge, error) {
	host, _ := ParseOrigin(originURL)
	h := strings.ToLower(host)
	switch {
	case strings.Contains(h, "github"):
		return githubForge{}, nil
	case strings.Contains(h, "gitlab"):
		return gitlabForge{}, nil
	}
	switch strings.ToLower(strings.TrimSpace(spec.Forge)) {
	case "github":
		return githubForge{}, nil
	case "gitlab":
		return gitlabForge{}, nil
	case "":
		return nil, fmt.Errorf(
			"cannot determine forge for origin %s; set authority.<repo>.forge: github|gitlab", redact.Origin(originURL))
	default:
		return nil, fmt.Errorf(
			"unknown authority.<repo>.forge %q for origin %s; want github|gitlab", spec.Forge, redact.Origin(originURL))
	}
}

// ParseOrigin extracts the host and the owner/name path from a git remote URL,
// handling both the scp-like ssh shape (git@host:owner/name.git) and the
// scheme://[user@]host/owner/name(.git) shape (https and ssh://). A path deeper than
// owner/name (e.g. a GitLab group/subgroup/project) is returned in full — gh/glab both
// accept the full path as the repo identifier.
func ParseOrigin(raw string) (host, ownerName string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	// scp-like: [user@]host:path (no scheme, a ':' that isn't a '://').
	if !strings.Contains(s, "://") {
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		if colon := strings.Index(s, ":"); colon >= 0 {
			host = s[:colon]
			ownerName = trimGitSuffix(strings.TrimPrefix(s[colon+1:], "/"))
			return host, ownerName
		}
		return host, ownerName
	}
	// scheme://[user@]host/path
	rest := s[strings.Index(s, "://")+3:]
	slash := strings.Index(rest, "/")
	authority := rest
	path := ""
	if slash >= 0 {
		authority = rest[:slash]
		path = rest[slash+1:]
	}
	if at := strings.Index(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	host = authority
	ownerName = trimGitSuffix(path)
	return host, ownerName
}

// OwnerName is the owner/name path from a git remote URL (ParseOrigin's second return).
func OwnerName(raw string) string {
	_, on := ParseOrigin(raw)
	return on
}

func trimGitSuffix(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	return strings.TrimSuffix(p, ".git")
}

// sameLayers reports whether a and b hold the same layer set (order-insensitive).
func sameLayers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, y := range b {
		if seen[y] == 0 {
			return false
		}
		seen[y]--
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
