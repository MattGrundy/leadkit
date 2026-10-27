package main

import (
	"os"
	"os/exec"
	"strings"

	"mgpp.dev/leadkit/internal/config"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/prcreate"
)

// guardVerdict is confirmGuard's decision — see below.
type guardVerdict int

const (
	guardMutate  guardVerdict = iota // both --apply and --confirm: the only case that mutates
	guardDryRun                      // neither, or --confirm alone (meaningless without --apply): ordinary preview
	guardRefused                     // --apply without --confirm: the post-incident refusal
)

// confirmGuard is the shared --apply/--confirm decision `init` and `audit --apply`
// both use — split out as a pure function (no I/O, no os.Exit) so the post-incident
// guard itself is directly unit-testable without exec'ing the built binary.
//
// POST-INCIDENT (2026-09-04, see docs/build-tasks/apply-safety-fixes.md): a bare Go
// bool flag parses true with no value at all (`--apply` alone == `--apply=true`) —
// that's exactly what caused a live `audit --apply` run during dogfooding. Requiring
// a SECOND explicit flag closes it: --apply alone can never mutate, it only computes
// and prints the plan, then refuses (non-zero exit) — the caller must type
// `--apply --confirm` together, on purpose, to actually write anything.
func confirmGuard(apply, confirm bool) guardVerdict {
	switch {
	case apply && confirm:
		return guardMutate
	case apply:
		return guardRefused
	default:
		return guardDryRun
	}
}

// gitOriginRemote is a read-only `git -C <repo> remote get-url origin` — "" on
// any failure (no repo, no origin remote, git not found), never a hard error;
// callers already treat that as "no override configured." It resolves the `origin`
// remote SPECIFICALLY (not the first remote), and a worktree returns its shared main
// repo's origin, which is why identity is stable across worktrees. An injectable var
// so identity tests inject a deterministic origin without a real git binary.
var gitOriginRemote = func(repo string) string {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// loadProfileOrDefaults is the same "load, or fall back to Defaults()" pattern every
// subcommand that touches the profile uses — never a hard failure on a bad/missing
// leadkit.yaml.
func loadProfileOrDefaults() *config.Profile {
	prof, _, err := config.Load()
	if err != nil {
		d := config.Defaults()
		return &d
	}
	return prof
}

// newGate is the ONE place every scope-sensitive gate-construction call site in
// this file goes through — wires prof.EnforcedRemotes AND prof.ProjectRoot onto a
// real *gate.Gate, so a future new call site can't forget either (a missed site is a
// scope inconsistency between commands). The gate ALWAYS covers the project's OWN origin
// (keyed on ProjectRoot's origin), whatever enforced_remotes holds — so a project with a
// leadkit.yaml gates its own pushes with zero config, and an unprofiled Defaults()
// profile (empty list + empty root) is out of scope. Used by gate check /
// gate check-push / gate verdict — NOT gate record, which never consults scope at all
// (gate.Record doesn't call inScope).
func newGate(prof *config.Profile) *gate.Gate {
	g := gate.New()
	// records_dir is part of "which gate governs this project": without it a profile that
	// sets one is written by `gate record` (the target's own records_dir) but read from the
	// central default here, and the milestone gate reports "unverified" for a gated SHA.
	if rd := prof.Resolve(prof.RecordsDir); rd != "" {
		g.RecordsDir = rd
	}
	g.EnforcedRemotes = prof.EnforcedRemotes
	g.ProjectRoot = prof.ProjectRoot()
	// The direct-command sites (gate check / check-push / verdict) resolve prof as the
	// SESSION's own ambient profile and the target IS that project, so union in its
	// enforced_remotes too — harmless where target==session, and it keeps the scope
	// decision identical to the push-gate path's union.
	g.SessionEnforcedRemotes = prof.EnforcedRemotes
	return g
}

// targetGateResolver returns the resolver every verb that judges a TARGET repo goes
// through: the review-gate that GOVERNS that repo (resolveTargetGate: its own profile's
// enforced_remotes, project root and records_dir), with the acting session's own
// enforced_remotes unioned in ADD-ONLY. It is the one definition shared by the push-gate
// hook, `leadkit push`, `gate check`, `gate check-push`, `gate record` and `gate verdict`,
// so they cannot disagree about whether a repo is gated. newGate(prof) is NOT that: it
// scopes by the session's own profile, which is only right when the target IS the session's
// project; used for another repo it answers "out of scope — allow" for a repo its own
// profile gates.
//
// The session's remotes are read once, when the resolver is built, from the session's
// ambient profile.
func targetGateResolver() func(repo string) *gate.Gate {
	sessionRemotes := append([]string(nil), loadProfileOrDefaults().EnforcedRemotes...)
	return func(repo string) *gate.Gate {
		g := resolveTargetGate(repo)
		g.SessionEnforcedRemotes = sessionRemotes
		return g
	}
}

// prGate is the review-gate `pr create` and `pr merge` judge a head SHA against. WHERE the
// records live (records_dir) and the project root come from the TARGET repo's own profile
// (resolveTargetGate), the same place `gate record` writes and push-gate reads, so a SHA
// that is gated is found. The SCOPE is deliberately not the target's: it is the session's
// enforced_remotes plus the authority key, because authority.<repo> is the session's grant
// and the key is what makes a governed repo in scope for PR-create.
func prGate(repoDir string, prof *config.Profile, key string) *gate.Gate {
	g := resolveTargetGate(repoDir)
	g.EnforcedRemotes = append(append([]string(nil), prof.EnforcedRemotes...), key)
	return g
}

// sessionSlugAndID resolves THIS session's addressing IDENTITY: its project slug and a
// per-instance session id. The slug comes from sessionIdentitySlug(cwd) — the session's
// OWN git-repo/cwd, NEVER an inherited $LEADKIT_PROFILE or ancestor leadkit.yaml — so a
// config-less collaborating project can't collapse onto a wrong shared slug (its brief
// `from=`/inbox and the lead-identity registry all key off this). The session id follows
// resolveSessionID's chain ($LEADKIT_SESSION_ID → $CLAUDE_CODE_SESSION_ID → slug), so two
// live instances are DISTINCT rather than clobbering one presence file. This is the
// IDENTITY path — the POLICY profile resolution (install.ResolveProjectSlug off prof, for
// guards/gate/queue/daemon) is deliberately left untouched (see sessionIdentitySlug).
func sessionSlugAndID(prof *config.Profile) (slug, sessionID string) {
	slug, _ = sessionIdentitySlug(sessionCwd())
	sessionID, _ = resolveSessionID(slug)
	return slug, sessionID
}

// resolveSessionID resolves THIS instance's unique session id and its source, in
// priority order: $LEADKIT_SESSION_ID (explicit override) → $CLAUDE_CODE_SESSION_ID
// (the harness's real per-session UUID — the correct instance identity) → the project
// slug (last-resort fallback, today's behavior, now rarely hit). The slug fallback is
// the ONLY case where two instances of one project collide onto a single presence file;
// with the harness UUID present (the norm) they register DISTINCT files.
func resolveSessionID(slug string) (id, source string) {
	if v := os.Getenv(sessionIDEnvVar); v != "" {
		return v, sessionIDEnvVar
	}
	if v := os.Getenv(harnessSessionEnvVar); v != "" {
		return v, harnessSessionEnvVar
	}
	return slug, "slug-fallback"
}

// requiredGatesProblem says why spec's required_gates cannot be satisfied: "" when every entry is a
// review layer the project has (the built-in pair, or a roster role declared review_layer), else
// what is wrong and what the layers are. A gate that names a layer no record can carry would
// otherwise surface only as "layers not ready: x=MISSING", which reads as work still to do.
func requiredGatesProblem(prof *config.Profile, spec config.AuthoritySpec) string {
	if err := gate.ValidateChain(spec.RequiredGates, gate.Vocabulary(prof.ReviewRoles())); err != nil {
		return "required_gates cannot be satisfied: " + err.Error()
	}
	return ""
}

// reviewGateResolver is targetGateResolver for the verbs that RECORD or SHOW layers (gate record,
// gate show, gate verdict): the same gate that governs the target repo, plus what only the acting
// project knows about its layers, which is the roster's review roles (the layer names it adds) and
// the chain its grant for this repo requires. The push gate does not use it: a push is judged by
// the default pair, since the grant that names a chain is the session's and not the target's.
func reviewGateResolver() func(repo string) *gate.Gate {
	prof := loadProfileOrDefaults()
	base := targetGateResolver()
	return func(repo string) *gate.Gate {
		g := base(repo)
		g.ReviewRoles = prof.ReviewRoles()
		if _, spec, ok := prcreate.ResolveAuthority(prof, gitOriginRemote(repo)); ok && len(spec.RequiredGates) > 0 && g.ValidateChain(spec.RequiredGates) == nil {
			g.Chain = append([]string(nil), spec.RequiredGates...)
		}
		return g
	}
}
