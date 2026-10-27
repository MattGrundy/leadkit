package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/roles"
)

// critiqueRole and finaliseRole are the fixed canonical role lanes GateVerdict
// routes the code-review-ready and critique-ready transitions into by default —
// role-keyed-lanes.md: the LANE is the canonical role, not a resolved live
// instance, so these need no resolver; they're simply always these two names.
const (
	critiqueRole = "critique"
	finaliseRole = "finalise"
)

// VerdictOpts are GateVerdict's arguments — mirrors gate-verdict.sh's flags.
type VerdictOpts struct {
	Ticket  string // the ticket's CURRENT queue location (a queue/<agent>/<status>/<base> path)
	Verdict string // "ready" | "changes-needed"
	By      string // default "claude"
	// ByBound: By was resolved against the session's identity (gate.ResolveBy); stored on the layer.
	ByBound bool
	Next    string // role lane for the next layer; default: the fixed "critique" role lane
	Layer   string // "code-review" | "critique"; default: derived from the existing gate record
	Base    string // ready only: the commit the review read FROM, stored on the layer (gate.RecordOpts.Base); "" covers only the sha
	// MutationEvidence is stored on the layer with the verdict (gate.RecordOpts.MutationEvidence); GateVerdict does not judge it.
	MutationEvidence string
	// Roster is the leadkit.yaml roster's role names (config.Profile.Roster types) and
	// LaneAliases the project's lane_aliases map — together the `known`+aliases the
	// changes-needed path feeds roles.CanonicalLane to reconcile a raw `owner` field
	// (owner-ci-infra / ci-infra-agent / an instance name) to its canonical role lane
	// BEFORE building the rework destination (the SAME resolver assign/orchestrate use),
	// so rework never bounces to a stray owner-<x> lane no owner watches. Both may be nil.
	Roster      []string
	LaneAliases map[string]string
	// DryRun: run every validation/derivation step unchanged, print the plan
	// (the record that WOULD be written, the frontmatter changes, the destination
	// move), but call NEITHER gate.Record NOR touch the ticket/queue. Mutates nothing.
	DryRun bool
	// GateForRepo, when set, supplies the review-gate that GOVERNS the ticket's repo, called
	// once the ticket's `repo:` frontmatter is known and used in place of the gate passed to
	// GateVerdict for everything after (sha resolution, scope, the record, the re-check). The
	// ticket names a repo that need not be the session's, so the scope decision belongs to
	// THAT repo's profile; the gate argument is then only the fallback for a caller (a test)
	// that supplies no resolver.
	GateForRepo func(repo string) *gate.Gate
}

// VerdictResult is what GateVerdict did (or, if DryRun, would have done).
type VerdictResult struct {
	NoOp        bool // already fully gated — nothing recorded, nothing moved
	GuardFailed bool // critique recorded ready, but gate.Check still fails — left in place
	DryRun      bool
	Layer       string
	NewStage    string
	NewStatus   string
	MovedTo     string // new (or would-be) queue location
	Message     string
}

// loudInconsistency wraps a failure that happens AFTER gate.Record already succeeded —
// the record and the ticket's location/frontmatter now disagree, which the INVARIANT
// this package is built around says must never happen silently. Never swallow this.
func loudInconsistency(err error) error {
	return fmt.Errorf("gate-verdict: INCONSISTENT STATE — the gate record was written but updating/moving the ticket failed; reconcile %w", err)
}

// CanonicalOwner reconciles a ticket's raw owner (owner-ci-infra / ci-infra-agent / an
// instance name / a type: value) to the canonical role LANE its rework is routed to: a
// leading `owner-` stripped, then roles.CanonicalLane over the roster + lane_aliases. The
// ONE place that rule lives — GateVerdict applies it when routing rework and
// `queue assign --owner` applies it before writing the field, so what assign writes is
// always something verdict accepts.
func CanonicalOwner(owner string, roster []string, aliases map[string]string) (string, bool) {
	stripped := strings.TrimPrefix(strings.TrimSpace(owner), "owner-")
	return roles.CanonicalLane(stripped, roles.KnownRoles(roster), aliases)
}

// GateVerdict is gate-verdict.sh: record a layer verdict AND reconcile the ticket's
// frontmatter/queue location to match, as one action. INVARIANT: the gate record and
// the ticket's location/frontmatter can never disagree — every validation below runs
// BEFORE gate.Record; only a failure AFTER that point (routing) can produce
// disagreement, and that always comes back as a loud, unmistakable error.
//
// g supplies the gate engine (records dir + git) — inject a stub Gate in tests so
// nothing here touches the real records dir or a real git repo.
func (rv *Reviewer) GateVerdict(g *gate.Gate, opts VerdictOpts) (VerdictResult, error) {
	if opts.Verdict != "ready" && opts.Verdict != "changes-needed" {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — unknown verdict %q (want: ready|changes-needed)", opts.Verdict)
	}
	by := opts.By
	if by == "" {
		by = "claude"
	}

	queuePath, base, canonicalPath, err := rv.resolveVerdictTicket(opts.Ticket)
	if err != nil {
		return VerdictResult{}, err
	}

	// A live claim by someone else says this review is theirs. Refuse now, before the work is done and
	// wasted: the record-time collision check only fires once the other reviewer has finished.
	if held, ok := queue.ClaimedByOther(queuePath, by, time.Now()); ok {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — %s is %s, and you are %q. A verdict from a second reviewer duplicates theirs and, if it differs, may overwrite it. Take a different ticket, or wait for the claim to lapse", base, held.String(), by)
	}

	fm, err := ReadFrontmatter(canonicalPath)
	if err != nil {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — %w", err)
	}
	repo := fm["repo"]
	owner, ownerFromType := fm["owner"], false
	if owner == "" && fm["type"] != "" {
		// No owner set — and nothing sets one after creation, so the ticket has none in
		// practice (ticket 173). `type:` already names the canonical agent that does the
		// work, so it is the rework destination; roles.CanonicalLane below reconciles it
		// exactly as it reconciles an explicit owner. An explicit owner always wins.
		owner, ownerFromType = fm["type"], true
	}
	if repo == "" {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — ticket %s has no repo in frontmatter", base)
	}

	if opts.GateForRepo != nil {
		g = opts.GateForRepo(repo)
	}

	sha, ok := g.Git.RevParse(repo, fm["sha"])
	if !ok || len(sha) != 40 {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — ticket %s's sha (%q) does not resolve to a full sha in %s", base, fm["sha"], repo)
	}

	if opts.Base != "" {
		if opts.Verdict != "ready" {
			return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — --base qualifies a ready verdict only, not %s", opts.Verdict)
		}
		if _, ok := g.ResolveCommit(repo, opts.Base); !ok {
			return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — --base %q does not resolve to a commit in %s", opts.Base, repo)
		}
	}

	// Role-keyed lanes: the code-review-ready and critique-ready destinations are
	// ALWAYS their fixed role lanes — no queue/ROLES role->instance lookup.
	// (Previously "critique"/"finalise" were resolved through roles.Resolve to
	// whatever live instance dir currently served them; that indirection is gone
	// from this routing path entirely — see docs/build-tasks/role-keyed-lanes.md.)
	finaliseLive := finaliseRole
	chain := g.Layers()

	layer := opts.Layer
	if layer == "" {
		ok, outcome, reason := g.CheckWithOutcome(sha, repo)
		if ok && outcome == gate.OutcomeGated {
			return VerdictResult{NoOp: true, Message: "already gated — " + reason}, nil
		}
		if ok && outcome == gate.OutcomeOutOfScope {
			// The repo is resolved and is not this project's: nothing to record, and it is not "gated" either.
			return VerdictResult{NoOp: true, Message: "NOT recorded — " + reason}, nil
		}
		// Any other ok means the gate could not JUDGE the commit (its scope could not be read), which says
		// nothing about whether it is reviewed: the caller asked to record a verdict, so derive the layer
		// from the record itself and record it.
		//
		// The layer to record is the first of the chain that is not ready yet; with no record at all,
		// the first. A record with every layer ready (an unjudged scope can leave one) falls to the last.
		layer = chain[len(chain)-1]
		if rec, rerr := g.ReadRecord(sha); rerr != nil {
			layer = chain[0]
		} else if missing := gate.Missing(rec, chain); len(missing) > 0 {
			layer, _, _ = strings.Cut(missing[0], "=")
		}
	}

	// A ready on a layer that is not the last of the chain hands the ticket to the next layer's lane
	// (the layer name is the role, and the role is the lane); the last, or one the chain does not
	// list, goes on to the all-layers re-check below.
	nextLayer, hasNext := "", false
	for i, l := range chain {
		if l == layer && i+1 < len(chain) {
			nextLayer, hasNext = chain[i+1], true
		}
	}
	next := opts.Next
	if next == "" {
		next = nextLayer
	}

	if opts.Verdict == "changes-needed" && owner == "" {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — changes-needed requires an owner, ticket %s has neither an owner: nor a type: to default it from", base)
	}

	// Reconcile the RAW owner field to its canonical role LANE before we build the
	// rework destination — the SAME resolver assign/orchestrate use (roster as `known`
	// + the always-on -agent fold + lane_aliases + a leading `owner-` prefix strip). A
	// raw owner like "owner-ci-infra" / "ci-infra-agent" / an instance name must bounce
	// to queue/ci-infra/pending/ where the live owner is waiting, never to a stray
	// queue/owner-ci-infra/pending/ lane no owner is `queue wait`-ing. Computed BEFORE
	// gate.Record so an unreconcilable owner is refused with nothing written — never a
	// silent stray lane, never a record/location disagreement.
	var canonicalOwner string
	if opts.Verdict == "changes-needed" {
		role, ok := CanonicalOwner(owner, opts.Roster, opts.LaneAliases)
		if !ok {
			if ownerFromType {
				return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — ticket %s has no owner:, and its type %q (used as the owner) does not reconcile to a known role — add that role to the roster or set a canonical owner:", base, owner)
			}
			return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — owner %q does not reconcile to a known role — set a canonical owner", owner)
		}
		if ownerFromType && isFixedRole(role) {
			// A type that names a GATE role (code-review-agent, critique) is not a work
			// owner: routing rework there puts the ticket back in the lane that just
			// bounced it, and the reviewer re-claims its own bounce forever. Usually such
			// a ticket is work ABOUT the review system, not work FOR a reviewer. Refused
			// loudly instead, before anything is recorded; an explicit owner: is a
			// deliberate claim and is not second-guessed here.
			return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — ticket %s has no owner:, and its type %q (used as the owner) is the gate role %q, which would bounce the rework into the lane that raised it. A type that names a reviewer usually means the ticket is work ABOUT the review system, not work FOR a reviewer: the ticket needs an explicit owner: — set owner: (e.g. developer) in its frontmatter (a reviewer is read-only, so the lead does this), or `leadkit queue assign --owner developer <ticket> <role>` for a ticket not yet queued", base, owner, role)
		}
		canonicalOwner = role
	}

	// Layer-independence preflight: the rule Record itself enforces, asked of the gate (so the two
	// cannot disagree) before anything is touched, with the existing record as the source of truth.
	// A record that cannot be read is no record: Record judges that case itself.
	rec, rerr := g.ReadRecord(sha)
	if err := g.CheckIndependence(rec, rerr == nil, layer, opts.Verdict, by, sha); err != nil {
		return VerdictResult{}, fmt.Errorf("gate-verdict: refuse — %s", strings.TrimPrefix(err.Error(), "gate: REFUSED — "))
	}

	recordPlan := fmt.Sprintf("record sha=%s layer=%s verdict=%s by=%s", shortSHA(sha), layer, opts.Verdict, by)
	if opts.Base != "" {
		recordPlan += " base=" + opts.Base
	}
	if !opts.DryRun {
		if _, err := g.Record(gate.RecordOpts{SHA: sha, Layer: layer, Verdict: opts.Verdict, By: by, ByBound: opts.ByBound, Repo: repo, Base: opts.Base, MutationEvidence: opts.MutationEvidence}); err != nil {
			return VerdictResult{}, err
		}
	}

	// From here on (non-dry-run) the record exists — any failure is a loud
	// inconsistency, never a silent partial. In dry-run, nothing has been written at
	// all — planOrApply below never touches disk.
	res := VerdictResult{Layer: layer, DryRun: opts.DryRun}

	if opts.Verdict == "changes-needed" {
		res.NewStage, res.NewStatus = "needs_rework", "pending"
		dest := filepath.Join(rv.QueueRoot, canonicalOwner, "pending", base)
		movedTo, msg, err := rv.planOrApply(opts.DryRun, canonicalPath, queuePath, dest,
			[]KV{{"stage", res.NewStage}, {"status", res.NewStatus}}, recordPlan,
			fmt.Sprintf("changes-needed — routed to %s's pending", canonicalOwner))
		if err != nil {
			return res, loudInconsistency(err)
		}
		res.MovedTo, res.Message = movedTo, msg
		return res, nil
	}

	if hasNext {
		res.NewStage, res.NewStatus = nextLayer, "pending"
		dest := filepath.Join(rv.QueueRoot, next, "pending", base)
		movedTo, msg, err := rv.planOrApply(opts.DryRun, canonicalPath, queuePath, dest,
			[]KV{{"stage", res.NewStage}, {"status", res.NewStatus}}, recordPlan,
			fmt.Sprintf("%s ready — routed to %s's pending for %s", layer, next, nextLayer))
		if err != nil {
			return res, loudInconsistency(err)
		}
		res.MovedTo, res.Message = movedTo, msg
		return res, nil
	}

	// The last layer of the chain (or one it does not list), verdict ready. Real runs re-check the actual record;
	// dry-run simulates it (the real record was never written) by asking whether
	// every required layer WOULD be ready with this verdict folded in.
	var stillFails bool
	var reason string
	if opts.DryRun {
		stillFails = !wouldAllLayersBeReady(g, chain, sha, layer, opts.Verdict)
		reason = "simulated: not every required layer would be ready"
	} else {
		var ok bool
		ok, reason = g.Check(sha, repo)
		stillFails = !ok
	}
	if stillFails {
		res.GuardFailed = true
		res.NewStatus = "pending"
		if opts.DryRun {
			res.Message = fmt.Sprintf("DRY RUN: would %s; gate would still not be passing (%s) — would leave in place (nothing done)", recordPlan, reason)
			return res, nil
		}
		if err := UpdateFrontmatter(canonicalPath, []KV{{"status", "pending"}}); err != nil {
			return res, loudInconsistency(err)
		}
		res.Message = fmt.Sprintf("critique recorded, but the gate still isn't passing (%s) — left in place, guard working", reason)
		return res, nil
	}

	res.NewStage, res.NewStatus = "finalise", "gated"
	finaliseLiveUp := isLiveDir(filepath.Join(rv.QueueRoot, finaliseLive, "pending"))
	var dest string
	successMsg := fmt.Sprintf("critique ready, all layers gated — routed to %s's pending", finaliseLive)
	if finaliseLiveUp {
		dest = filepath.Join(rv.QueueRoot, finaliseLive, "pending", base)
	} else {
		dest = filepath.Join(rv.QueueRoot, by, "done", base)
		successMsg = fmt.Sprintf("LEAD MUST PUSH MANUALLY: repo=%s sha=%s", repo, sha)
	}
	movedTo, msg, err := rv.planOrApply(opts.DryRun, canonicalPath, queuePath, dest,
		[]KV{{"stage", res.NewStage}, {"status", res.NewStatus}}, recordPlan, successMsg)
	if err != nil {
		return res, loudInconsistency(err)
	}
	res.MovedTo, res.Message = movedTo, msg
	return res, nil
}

// planOrApply either applies fmUpdates + moves the queue entry from->to (real run,
// returning successMsg), or — dryRun — prints the plan and touches nothing.
func (rv *Reviewer) planOrApply(dryRun bool, canonicalPath, from, to string, fmUpdates []KV, recordPlan, successMsg string) (movedTo, message string, err error) {
	if dryRun {
		var changes []string
		for _, kv := range fmUpdates {
			changes = append(changes, kv.Key+"="+kv.Value)
		}
		return to, fmt.Sprintf("DRY RUN: would %s; frontmatter: %s; would move %s -> %s (nothing done)",
			recordPlan, strings.Join(changes, ","), from, to), nil
	}
	if err := rv.applyRouting(canonicalPath, from, to, fmUpdates); err != nil {
		return "", "", err
	}
	return to, successMsg, nil
}

// applyRouting mutates the ticket's frontmatter and moves its queue entry from ->to,
// in that order. Called only after gate.Record already succeeded (never in dry-run).
func (rv *Reviewer) applyRouting(canonicalPath, from, to string, fmUpdates []KV) error {
	if err := UpdateFrontmatter(canonicalPath, fmUpdates); err != nil {
		return fmt.Errorf("update frontmatter: %w", err)
	}
	if err := queue.Move(rv.QueueRoot, from, to, false); err != nil {
		return fmt.Errorf("move ticket: %w", err)
	}
	return nil
}

// wouldAllLayersBeReady simulates gate.Check's "all required layers ready" question
// for dry-run, without writing a record: it folds simulatedVerdict into sha's
// EXISTING record (in memory only) and checks every gate.RequiredLayers entry against
// that. An unreadable/absent record is treated as empty, same as gate.Check does.
func wouldAllLayersBeReady(g *gate.Gate, chain []string, sha, simulatedLayer, simulatedVerdict string) bool {
	rec, err := g.ReadRecord(sha)
	if err != nil {
		rec = gate.Record{}
	}
	layers := make(map[string]gate.Layer, len(rec.Layers)+1)
	for l, lv := range rec.Layers {
		layers[l] = lv
	}
	layers[simulatedLayer] = gate.Layer{Verdict: simulatedVerdict}
	return len(gate.Missing(gate.Record{Layers: layers}, chain)) == 0
}

// resolveVerdictTicket resolves ticket to its CURRENT queue location, requiring its
// state dir (parent) to be inside the queue root — the queue-move preflight — and to
// its canonical tasks/<base> file. ticket may be absolute, cwd-relative, or the
// "queue/<agent>/<status>/<base>" form a wake message hands an agent (resolved
// against the queue root's parent). The leaf is never dereferenced when computing the
// queue location — only its parent's symlinks are resolved — matching queue.Move.
func (rv *Reviewer) resolveVerdictTicket(ticket string) (queuePath, base, canonicalPath string, err error) {
	p := rv.locateTicketArg(ticket)
	if !filepath.IsAbs(p) {
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			return "", "", "", fmt.Errorf("gate-verdict: refuse — bad ticket path %q: %w", ticket, aerr)
		}
		p = abs
	}

	queueRootReal, err := filepath.EvalSymlinks(rv.QueueRoot)
	if err != nil {
		return "", "", "", fmt.Errorf("gate-verdict: refuse — queue root unresolvable: %w", err)
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil || !strings.HasPrefix(parentReal, queueRootReal+string(os.PathSeparator)) {
		return "", "", "", fmt.Errorf("gate-verdict: refuse — ticket's state dir is not inside the queue root: %s", ticket)
	}
	queuePath = filepath.Join(parentReal, filepath.Base(p))

	real, err := filepath.EvalSymlinks(queuePath)
	if err != nil {
		return "", "", "", fmt.Errorf("gate-verdict: refuse — ticket not found: %s", ticket)
	}
	base = filepath.Base(real)
	canonicalPath = filepath.Join(rv.TasksDir, base)
	if _, err := os.Stat(canonicalPath); err != nil {
		return "", "", "", fmt.Errorf("gate-verdict: refuse — resolved ticket %s is not under the tasks dir %s", real, rv.TasksDir)
	}
	return queuePath, base, canonicalPath, nil
}

// locateTicketArg maps the -ticket argument to the path to resolve. A ticket entry is
// always a .md file, so an argument that omits the extension is tried with it appended
// when — and only when — the argument as given names nothing: the lane entry is found
// rather than reported as a missing ticket (ticket 173). A relative argument that is not
// under the cwd is tried against the queue root's parent, the "queue/<agent>/<status>/<base>"
// form a wake message hands an agent. Nothing found: the argument comes back unchanged,
// so the caller's refusal names what the operator typed.
func (rv *Reviewer) locateTicketArg(ticket string) string {
	locate := func(p string) (string, bool) {
		if filepath.IsAbs(p) {
			return p, isEntry(p)
		}
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		if joined := filepath.Join(filepath.Dir(rv.QueueRoot), p); isEntry(joined) {
			return joined, true
		}
		return p, false
	}
	if found, ok := locate(ticket); ok {
		return found
	}
	if !strings.HasSuffix(ticket, ".md") {
		if found, ok := locate(ticket + ".md"); ok {
			return found
		}
	}
	return ticket
}

func isFixedRole(role string) bool {
	for _, f := range roles.FixedRoles {
		if role == f {
			return true
		}
	}
	return false
}

func isEntry(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isLiveDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// shortSHA is the same "first 12 chars" convention internal/gate uses in its own
// messages.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
