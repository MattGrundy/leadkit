package prcreate

// edit.go adds the governed `leadkit pr edit` path — a SIBLING of pr create, NOT an
// overload of it. It mirrors create's design exactly: a PURE DecideEdit returning the
// same Decision struct (no I/O), an injected Runner, and the forge abstraction reused
// unchanged. create's own Decide/ordering is untouched.
//
// A PR-metadata edit is an outward-facing forge WRITE, so this path is adversarially
// default-DENY and, crucially, NARROWER than create in what it can express:
//   - it can NEVER mark a PR ready / undraft it, and NEVER merge — the edit argv only
//     ever carries --title / --body-file (gh) / --description (glab). No --draft, no
//     --ready, no undraft, no merge verb is ever composable here.
//   - an omitted --title or --body-file must NOT blank that field: only a field the
//     caller explicitly set is put on the wire (TitleSet / BodySet).
//   - a no-op edit (neither field set) is REFUSED — never a silent empty forge call.
//   - owner_prs_only fails CLOSED: the author lookup (reused from internal/hook, NOT
//     forked) is resolved by the CLI and fed in as OwnerConfirmed; when the policy is
//     set and ownership is NOT confirmed (lookup failed, offline, or a different
//     author), DecideEdit refuses. It NEVER allows on an unresolvable/errored lookup.
//   - there is NO required_gates check: a metadata edit introduces no new sha, so the
//     gate-before-PR rule (which gates a sha) simply does not apply here.
//   - a BASE change (--base) is the one edit that alters what a merge will do, so it is
//     permitted only when it is STRICTLY NARROWING: the PR's current base must already be
//     merged into the new base (an ancestor of it), which cannot change the diff. That is the
//     stacked-PR case, where the forge does not retarget after the base PR merges. Anything
//     else is refused and escalated, and so is any base the CLI could not verify (fail-closed).

import (
	"fmt"
	"strings"

	"mgpp.dev/leadkit/internal/config"
)

// EditRequest is the uniform, forge-agnostic PR-edit request as the CLI expresses it.
// Title / BodyFile carry the new values; TitleSet / BodySet say whether the caller
// actually PROVIDED that flag — an unset field is left untouched (never blanked), and
// with neither set the edit is a refused no-op.
type EditRequest struct {
	PRNumber string   // the PR/MR number to edit (gh pr edit <n> / glab mr update <n>)
	Title    string   // new title (only applied when TitleSet)
	TitleSet bool     // did the caller pass --title?
	BodyFile string   // new body file path (only applied when BodySet)
	BodySet  bool     // did the caller pass --body-file?
	Base     string   // new base branch (only applied when BaseSet, and only when BaseCheck permits)
	BaseSet  bool     // did the caller pass --base?
	Extra    []string // passthrough forge flags, appended after the composed flags
}

// BaseCheck is the resolved evidence DecideEdit needs to judge a retarget — gathered by the CLI
// (forge lookup + git), fed in so the decision stays pure. Resolved=false means the evidence could
// not be gathered, which is a refusal, never an allow.
type BaseCheck struct {
	Resolved    bool   // were both CurrentBase and Ancestor actually determined?
	CurrentBase string // the PR's present base branch
	Ancestor    bool   // is CurrentBase already merged into (an ancestor of) the requested base?
	Reason      string // why it could not be resolved, or the detail to show on a refusal
}

// DecideEditInput is everything DecideEdit needs — all already-resolved, NO I/O. The
// CLI resolves origin, authority, and (for owner_prs_only) the author lookup, then
// DecideEdit applies the governance ordering deterministically. Mirrors DecideInput,
// but with OwnerConfirmed/OwnerReason in place of create's gate fields (edit has no
// gate) and no draft/gate/user-approved concepts.
type DecideEditInput struct {
	OriginURL      string               // the target repo's origin remote URL
	AuthKey        string               // the matched authority.<repo> key (for messages)
	Spec           config.AuthoritySpec // the resolved authority policy
	HasAuthority   bool                 // did any authority key match the origin?
	ProfileMissing string               // no profile was found at all: see noAuthorityReason
	Req            EditRequest          // the composed PR-edit request
	OwnerConfirmed bool                 // ONLY consulted when Spec.OwnerPRsOnly: did the injected author lookup confirm the authenticated user authored this PR?
	OwnerReason    string               // why ownership was not confirmed (shown on the fail-closed refusal)
	BaseCheck      BaseCheck            // ONLY consulted when Req.BaseSet: the retarget evidence

	// WHO may retarget, independent of the ancestry check. No default: an unresolved input is a refusal.
	LeadOK     bool   // is the calling session registered as a live lead? (a retarget is the lead's act)
	LeadReason string // why not

	// ConfirmAtUse asks the owner, at the moment of use, to confirm exactly this retarget. ExecuteEdit
	// calls it after every check has passed and immediately before the forge, so there is no artefact
	// for anything to forge in between, and a refused or dry-run edit never reaches it. nil refuses:
	// a retarget that cannot ask the owner is not performed.
	ConfirmAtUse func(RetargetFacts) error
}

// RetargetFacts is what the owner is shown, and must confirm, before a retarget runs.
type RetargetFacts struct {
	Repo string // the matched authority.<repo> key
	PR   string
	From string // the PR's current base
	To   string // the requested base
}

// DecideEdit is the PURE governance resolver for `leadkit pr edit` — no I/O, fully
// unit-testable. It enforces, IN ORDER (default-DENY throughout):
//  1. no authority entry matched the origin → REFUSE (never exec the forge for an
//     ungoverned repo — same default-deny posture as create).
//  2. no-op edit (neither --title nor --body-file set) → REFUSE (never an empty edit).
//  3. can_open_pr: true → continue; false → refuse ("not wired"); "human" → refuse
//     with a DISTINCT escalation message. A repo not trusted to open PRs autonomously
//     is not trusted to amend them autonomously either — edit is a forge write too.
//  4. owner_prs_only: when set, OwnerConfirmed MUST hold — an unconfirmed/errored
//     ownership check fails CLOSED (refuse), NEVER allow.
//  5. only after all pass: select the forge and compose the edit argv — which can only
//     ever carry title/body, never a draft/ready/undraft/merge flag.
func DecideEdit(in DecideEditInput) Decision {
	// 1. default-deny: an ungoverned repo never reaches the forge.
	if !in.HasAuthority {
		return Decision{Allowed: false, Reason: noAuthorityReason("PR-edit", in.OriginURL, in.ProfileMissing)}
	}

	// 2. no-op: no field provided → refuse (never a silent empty forge write).
	if !in.Req.TitleSet && !in.Req.BodySet && !in.Req.BaseSet {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"nothing to edit for %s: pass --title, --body-file and/or --base (a no-op edit is refused)", in.AuthKey)}
	}
	// The passthrough is an allowlist, and nothing is on it: see allowedPassthrough.
	if reason := refusePassthrough("edit", in.Req.Extra); reason != "" {
		return Decision{Allowed: false, Reason: reason}
	}

	// 3. can_open_pr — human is distinct from false (escalate vs not-yet-wired).
	if in.Spec.CanOpenPR.Human {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"editing a PR in %s requires a human; the lead escalates to the user", in.AuthKey)}
	}
	if !in.Spec.CanOpenPR.Allow {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"can_open_pr is false for %s — not wired for autonomous PR-edit", in.AuthKey)}
	}

	// 4. owner_prs_only — fail CLOSED: unconfirmed ownership is treated as not yours.
	if in.Spec.OwnerPRsOnly && !in.OwnerConfirmed {
		return Decision{Allowed: false, Reason: fmt.Sprintf(
			"PR #%s edit denied for %s — owner_prs_only: %s. Fail-closed: an unconfirmed ownership check is treated as not yours.",
			in.Req.PRNumber, in.AuthKey, ownerReasonOrDefault(in.OwnerReason))}
	}

	// 4b. a retarget must be strictly narrowing, and verified.
	if in.Req.BaseSet {
		if d := decideRetarget(in); !d.Allowed {
			return d
		}
	}

	// 5. only now build the forge argv. An unresolvable forge is itself a refusal.
	f, err := forgeFor(in.OriginURL, in.Spec)
	if err != nil {
		return Decision{Allowed: false, Reason: err.Error()}
	}
	argv := f.EditArgv(editReq{
		OwnerName: OwnerName(in.OriginURL),
		Number:    in.Req.PRNumber,
		Title:     in.Req.Title,
		TitleSet:  in.Req.TitleSet,
		BodyFile:  in.Req.BodyFile,
		BodySet:   in.Req.BodySet,
		Base:      in.Req.Base,
		BaseSet:   in.Req.BaseSet,
		Extra:     in.Req.Extra,
	})
	return Decision{Allowed: true, Argv: argv, Reason: fmt.Sprintf(
		"%s: edit allowed for %s (PR #%s%s)", f.Name(), in.AuthKey, in.Req.PRNumber, editFieldsNote(in.Req))}
}

// decideRetarget refuses unless changing the base is strictly narrowing: the current base is
// already merged into the requested one, so the PR's diff cannot change. Every doubt is a refusal.
func decideRetarget(in DecideEditInput) Decision {
	refuse := func(format string, a ...interface{}) Decision {
		return Decision{Allowed: false, Reason: fmt.Sprintf("retarget of PR #%s refused — ", in.Req.PRNumber) + fmt.Sprintf(format, a...)}
	}
	want := strings.TrimSpace(in.Req.Base)
	if want == "" {
		return refuse("--base needs a branch name")
	}
	// WHO: a retarget is the lead's act. The role comes from the registry, which a session writes for
	// itself, so it is a bar and not a lock.
	if !in.LeadOK {
		return refuse("only a lead may retarget a base: %s", orLead(in.LeadReason))
	}
	// WITH WHAT AUTHORITY: the owner reserved base changes to himself, per use, confirmed when it runs
	// (see ConfirmAtUse). Without a way to ask him there is no retarget.
	if in.ConfirmAtUse == nil {
		return refuse("there is no way to ask the owner to confirm it, and a retarget needs his confirmation each time")
	}
	bc := in.BaseCheck
	if !bc.Resolved {
		return refuse("the current base and its relation to %q could not be verified (%s). Fail-closed: an unverified retarget is not performed", want, orUnknown(bc.Reason))
	}
	if bc.CurrentBase == want {
		return refuse("%q is already the base (a no-op edit is refused)", want)
	}
	if !bc.Ancestor {
		return refuse("the current base %q is NOT merged into %q, so the retarget would change what a merge does. Only a strictly narrowing retarget "+
			"(current base already an ancestor of the new one) is governed; anything else is a human decision, so escalate", bc.CurrentBase, want)
	}
	return Decision{Allowed: true}
}

func orLead(r string) string {
	if strings.TrimSpace(r) == "" {
		return "this session is not registered as a live lead (leadkit register --role lead)"
	}
	return r
}

func ownerReasonOrDefault(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "ownership could not be confirmed (lookup failed, offline, or a different author)"
	}
	return reason
}

func editFieldsNote(r EditRequest) string {
	var fields []string
	if r.TitleSet {
		fields = append(fields, "title")
	}
	if r.BodySet {
		fields = append(fields, "body")
	}
	if r.BaseSet {
		fields = append(fields, "base")
	}
	if len(fields) == 0 {
		return ""
	}
	return ", " + strings.Join(fields, "+")
}

// ExecuteEdit renders or executes an edit Decision — the single orchestration point
// both the CLI and the tests drive, mirroring Execute. When the decision is refused OR
// dryRun is set, neither the owner confirmation nor the runner is ever reached.
func ExecuteEdit(in DecideEditInput, dryRun bool, runner Runner) (Decision, string, error) {
	d := DecideEdit(in)
	if !d.Allowed || dryRun {
		return d, "", nil
	}
	if in.Req.BaseSet {
		facts := RetargetFacts{Repo: in.AuthKey, PR: in.Req.PRNumber, From: in.BaseCheck.CurrentBase, To: strings.TrimSpace(in.Req.Base)}
		if err := in.ConfirmAtUse(facts); err != nil {
			return Decision{Allowed: false, Reason: fmt.Sprintf("retarget of PR #%s not performed — the owner's confirmation was not given: %v", in.Req.PRNumber, err)}, "", nil
		}
	}
	out, err := runner.Run(d.Argv)
	return d, out, err
}

// ForgeName resolves the forge NAME ("github"|"gitlab") for originURL/spec, reusing the
// SAME forgeFor selection the argv composition uses — so the CLI's owner_prs_only author
// lookup targets the same forge as the eventual edit. Returns the forgeFor error
// unchanged when the forge can't be determined (the CLI then lets DecideEdit surface it).
func ForgeName(originURL string, spec config.AuthoritySpec) (string, error) {
	f, err := forgeFor(originURL, spec)
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

// editReq is the forge-facing edit request — EditRequest plus the derived owner/name.
type editReq struct {
	OwnerName string
	Number    string
	Title     string
	TitleSet  bool
	BodyFile  string
	BodySet   bool
	Base      string
	BaseSet   bool
	Extra     []string
}

// EditArgv composes `gh pr edit <n> --repo <owner/name> [--title <t>] [--body-file <p>]`.
// It carries ONLY the fields the caller set, and NEVER any draft/ready/undraft flag — a
// governed edit can never change a PR's draft state or merge it.
func (githubForge) EditArgv(r editReq) []string {
	argv := []string{"gh", "pr", "edit", r.Number, "--repo", r.OwnerName}
	if r.TitleSet {
		argv = append(argv, "--title", r.Title)
	}
	if r.BodySet {
		argv = append(argv, "--body-file", r.BodyFile)
	}
	if r.BaseSet {
		argv = append(argv, "--base", r.Base)
	}
	return append(argv, r.Extra...)
}

// EditArgv composes `glab mr update <n> --repo <owner/name> [--title <t>]
// [--description <p>]`. GitLab's edit verb is `mr update` and its body flag is
// --description (path here — same never-run-for-real caveat as create). ONLY set fields
// are carried; no draft/ready/merge flag is ever composable.
func (gitlabForge) EditArgv(r editReq) []string {
	argv := []string{"glab", "mr", "update", r.Number, "--repo", r.OwnerName}
	if r.TitleSet {
		argv = append(argv, "--title", r.Title)
	}
	if r.BodySet {
		argv = append(argv, "--description", r.BodyFile)
	}
	if r.BaseSet {
		argv = append(argv, "--target-branch", r.Base)
	}
	return append(argv, r.Extra...)
}
