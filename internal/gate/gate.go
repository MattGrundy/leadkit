// Package gate folds review-gate.sh into leadkit: the local review-gate that records
// per-layer agent-review verdicts and blocks pushes that lack all mandated layers.
//
// INTEROPERABILITY IS THE RULE HERE. review-gate.sh stays live and in use; this package
// reads and writes the SAME records under the SAME records dir
// (~/.claude/review-gate/records/<full-sha>.json), with the same schema and the same
// accept/refuse rules, so a record written by bash is honoured by Go and vice-versa.
// Byte-for-byte JSON formatting need not match jq's — only the keys, values, and
// decisions have to.
//
// Layers: code-review (independent correctness/reuse lens) and critique (chained —
// terminal, only recordable once code-review is ready on the same sha), which are the
// default chain; a project can name its own chain and add review roles (see chain.go). Scope
// (which pushes the gate enforces) is per-project: by default a project gates its
// OWN origin ALWAYS, and an enforced_remotes list ADDS other repos to that — it never
// removes the project's own origin. See inScope.
package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// RequiredLayers is the ordered set of layers Check requires to be "ready".
var RequiredLayers = []string{"code-review", "critique"}

// supersededMarker is the verdict a sibling layer's `ready` is demoted to when a peer
// layer records `changes-needed` on the same sha (Fix 2 — sibling invalidation). It is
// deliberately neither "ready" nor "changes-needed": Check (and every other consumer,
// all of which test `Verdict != "ready"`) reads it as NOT ready, so the gate can only
// be re-satisfied by a fresh review pass — in practice, a re-gate on a new sha.
const supersededMarker = "needs-re-review"

// Layer is one recorded layer verdict.
//
// SupersededVerdict/SupersededBy are the AUDIT trail of a prior verdict this layer
// entry replaced: they carry the previous verdict and its author when a `changes-needed`
// dissent superseded an earlier `ready` on the same layer/sha (asymmetric write-once),
// or when this layer's `ready` was invalidated because a PEER layer recorded
// `changes-needed` on the same sha (Fix 2). Additive + omitempty: a record written by
// review-gate.sh (which never sets them) round-trips unchanged, and this is a gate
// record — not a config Profile — so it has no config/CONFIG-REFERENCE impact.
type Layer struct {
	Verdict           string `json:"verdict"`
	By                string `json:"by"`
	Ts                string `json:"ts"`
	SupersededVerdict string `json:"superseded_verdict,omitempty"`
	SupersededBy      string `json:"superseded_by,omitempty"`
	// Base is the full sha the layer's review read from (it covers Base...<record sha>); empty covers only the sha.
	Base string `json:"base,omitempty"`
	// ByBound is true when By was taken from, or checked against, the recorder's own identity
	// ($CLAUDE_AGENT_NAME) rather than asserted on a flag. It is evidence about HOW By was
	// obtained, not proof of who recorded: an environment variable can be set by anyone able to run
	// the command. A reader that wants to know whether the two layers' names were ever checked
	// against anything looks here.
	ByBound bool `json:"by_bound,omitempty"`
	// MutationEvidence is the reviewer's own free-text statement of how it checked that the change's
	// tests discriminate: the mutants applied and the test that failed for each, or "no mutant applies,
	// because ...". Absent means NOT RECORDED (every record written before it existed lacks it), never
	// "no mutants" and never invalid. It is evidence of the basis for a verdict, not a gate input:
	// Check never reads it, and it is as honest as the By that wrote it.
	MutationEvidence string `json:"mutation_evidence,omitempty"`
}

// Record is the on-disk record shape: keep exactly, review-gate.sh reads/writes the
// same fields. Ts is UTC "2006-01-02T15:04:05Z" (second precision, literal Z).
type Record struct {
	SHA    string           `json:"sha"`
	Layers map[string]Layer `json:"layers"`
}

// Git abstracts the two git-touching calls Record/Check/CheckPush need, so tests can
// stub them without a real git repository. The default implementation shells out to
// git; construct a Gate directly with a fake Git in tests.
type Git interface {
	// RevParse resolves ref to its full sha in repo (empty repo = process cwd, no -C).
	// ok is false on any git failure (bad ref, no repo, not a git dir) — callers apply
	// the bash contract's fallback themselves (see NormalizeSHA).
	RevParse(repo, ref string) (sha string, ok bool)
	// RemoteURL returns repo's origin remote URL, or "" if it can't be determined.
	RemoteURL(repo string) string
}

// execGit is the real Git: shells out to the git binary.
type execGit struct{}

func (execGit) RevParse(repo, ref string) (string, bool) {
	args := gitArgs(repo, "rev-parse", ref)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func (execGit) RemoteURL(repo string) string {
	args := gitArgs(repo, "config", "--get", "remote.origin.url")
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitArgs(repo string, rest ...string) []string {
	if repo == "" {
		return rest
	}
	return append([]string{"-C", repo}, rest...)
}

// Gate is the review-gate engine. The zero value is unusable for the real thing — use
// New() for the real (HOME + real git) config, or construct directly with a stub Git
// and a t.TempDir() RecordsDir in tests, so tests never touch the real records dir or
// a real git repo.
type Gate struct {
	RecordsDir string
	Git        Git

	// EnforcedRemotes is the OTHER-repos list: THIS Gate's inScope is also true for a
	// repo whose origin URL CONTAINS any listed substring (the historical
	// substring semantics). It is the per-project enforced_remotes config field,
	// wired in from the loaded profile. It only ADDS scope: the project's own
	// origin (see ProjectRoot) is gated whether the list is empty, populated, or
	// omits that origin. Empty/nil (the zero value) means "no OTHER repos named",
	// not "enforce nothing" — see inScope.
	EnforcedRemotes []string

	// ProjectRoot is the governing profile's own project-root directory. inScope
	// gates the project's OWN origin regardless of EnforcedRemotes: a target repo
	// whose origin remote equals this ProjectRoot's origin remote is in scope. So a
	// project gates its own pushes with zero config and no named list. An empty
	// ProjectRoot yields no own-origin, so a genuinely different, UNPROFILED repo
	// (config.Defaults(), which sets neither it nor a list) is out of scope — the
	// intended "gate your own repo, not everyone else's" default. Set it alongside EnforcedRemotes on every scope-sensitive
	// Gate (Check/CheckPush and their callers); Record never consults scope at all.
	ProjectRoot string

	// SessionEnforcedRemotes is the ACTING SESSION's own enforced_remotes list (the
	// ambient policy profile, NOT the target repo's). It is UNIONED into inScope,
	// ADD-ONLY: a target is in scope if the target's OWN gating (EnforcedRemotes /
	// ProjectRoot above) says so, OR the acting session's list substring-matches the
	// target's origin. This closes the cross-project coverage gap where a session
	// whose profile declares enforced_remotes pushes an UNPROFILED clone of one of
	// those repos (target-own gating alone reads it out of scope). It can only ADD
	// scope — a narrow/empty session list can NEVER remove scope a target's own
	// config already grants (the push-gate bypass fix stays intact). Wired from the
	// session/ambient profile's EnforcedRemotes at the push-gate + direct-command
	// gate-construction sites.
	SessionEnforcedRemotes []string

	// Chain is the ordered layers this repo requires (its grant's required_gates); empty means
	// RequiredLayers. List order is chain order. See chain.go.
	Chain []string

	// ReviewRoles are the roles the project declares review-capable (roster review_layer: true).
	// Each is a valid layer name in addition to the built-in review layers.
	ReviewRoles []string

	// RefuseUnverifiable makes CheckWithOutcome report ok=false for OutcomeUnverifiable, the
	// outcome the gate returns when it cannot confirm a push is gated. It is for the entry points
	// that read only the ok-bool and have no mode-aware layer of their own (`gate check`, the git
	// pre-push hook): the caller sets it when the target's push-gate guard is in enforce mode, so
	// they refuse exactly what the push-gate hook denies. Left false (the zero value, and the
	// hook's and `leadkit push`'s own Gate) the ok-bool is the legacy fail-open.
	RefuseUnverifiable bool
}

// New returns a Gate wired to the real records dir and real git. EnforcedRemotes and
// ProjectRoot are left unset — callers that have a loaded profile should set
// g.EnforcedRemotes = prof.EnforcedRemotes and g.ProjectRoot = prof.ProjectRoot() for
// any scope-sensitive operation (see the fields' own doc comments).
func New() *Gate {
	return &Gate{RecordsDir: DefaultRecordsDir(), Git: execGit{}}
}

// DefaultRecordsDir is ~/.claude/review-gate/records — the single central directory
// review-gate.sh's rec_dir() resolves to.
func DefaultRecordsDir() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "review-gate", "records")
}

// normalizeSHA mirrors the bash's `git rev-parse "$ref" 2>/dev/null || echo "$ref"` —
// resolve short→full, but on any failure keep ref unchanged rather than erroring.
func normalizeSHA(g Git, repo, ref string) string {
	if full, ok := g.RevParse(repo, ref); ok {
		return full
	}
	return ref
}

func (g *Gate) recordPath(sha string) string {
	return filepath.Join(g.RecordsDir, sha+".json")
}

// readRecord loads sha's record. The error is os.IsNotExist-checkable — Check needs
// to tell "no record" apart from other failures.
func (g *Gate) readRecord(sha string) (Record, error) {
	data, err := os.ReadFile(g.recordPath(sha))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("gate: parse record %s: %w", g.recordPath(sha), err)
	}
	if rec.Layers == nil {
		rec.Layers = map[string]Layer{}
	}
	return rec, nil
}

// readRecordOrNew is readRecord but a missing file yields a fresh empty record rather
// than an error, plus whether a record already existed on disk (Record needs this to
// tell "first verdict on this layer" from "re-recording").
func (g *Gate) readRecordOrNew(sha string) (rec Record, existed bool, err error) {
	rec, err = g.readRecord(sha)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{SHA: sha, Layers: map[string]Layer{}}, false, nil
		}
		return Record{}, false, err
	}
	return rec, true, nil
}

func (g *Gate) writeRecord(sha string, rec Record) error {
	if err := os.MkdirAll(g.RecordsDir, 0o755); err != nil {
		return fmt.Errorf("gate: mkdir records dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("gate: marshal record: %w", err)
	}
	if err := os.WriteFile(g.recordPath(sha), data, 0o644); err != nil {
		return fmt.Errorf("gate: write record %s: %w", g.recordPath(sha), err)
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// trailingCounter matches one-or-more trailing "-<digits>" groups, e.g. "-1-3" in
// "cr-1-3". Matches canonical_role()'s `sed -E 's/(-[0-9]+)+$//'`.
var trailingCounter = regexp.MustCompile(`(-[0-9]+)+$`)

// layerOwner is who recorded a layer. A layer demoted by a sibling's dissent keeps no By, only
// SupersededBy, and that is still the role that owned it: independence must not lapse with the
// demotion.
func layerOwner(l Layer) string {
	if l.By != "" {
		return l.By
	}
	return l.SupersededBy
}

// CanonicalRole maps a `by` agent/instance name to its role family, so layer
// independence is enforced on ROLE (code-review vs critique), not
// on instance name — 'cr-1-3' and 'code-review-agent' must compare equal, or a
// code-review agent could satisfy the critique layer too. The two built-in review roles have
// the short families cr and crit; every other role is its own name with the trailing instance
// counter folded away, which is what a project-declared review role (a layer named for it)
// is held to as well.
//
// Exported for the independence rule (Gate.CheckIndependence), which gate-verdict calls before
// Record even runs and Record itself applies, so both places read one rule.
func CanonicalRole(name string) string {
	switch {
	case name == "code-review-agent" || name == "cr" || strings.HasPrefix(name, "cr-") || strings.HasPrefix(name, "code-review"):
		return "cr"
	case name == "critique-agent" || name == "crit" || strings.HasPrefix(name, "crit-") || strings.HasPrefix(name, "critique"):
		return "crit"
	default:
		s := trailingCounter.ReplaceAllString(name, "")
		s = strings.TrimSuffix(s, "-relief")
		return s
	}
}

// RecordOpts are Record's arguments — mirrors review-gate.sh's `record` flags.
type RecordOpts struct {
	SHA     string // empty: resolve HEAD in Repo
	Layer   string // must be in the gate's Vocabulary
	Verdict string // "ready" | "changes-needed"
	By      string // default "claude"
	Repo    string // empty: process cwd
	// Base, if set, is a ref resolving to the commit the review read from; stored as a full sha, ready only, write-once.
	Base string
	// ByBound records that By came from ResolveBy with an identity present (see Layer.ByBound).
	ByBound bool
	// MutationEvidence is stored on the layer as given (see Layer.MutationEvidence). Record does not
	// judge it; the verbs that take it from a reviewer refuse a ready without it (ResolveMutationEvidence).
	MutationEvidence string
}

// ResolveBy is the name a verdict is recorded under, given the -by the caller asked for (empty when
// it did not pass one) and the identity of the session ($CLAUDE_AGENT_NAME, empty when unset).
//
// With an identity, the identity IS the name: omitting -by uses it, repeating it is fine, and any
// other name is refused, so an agent cannot casually record as a different role than the one it was
// started as, and the role-independence rules (which key on this name) see the name the harness gave
// it. Without one, -by is self-asserted exactly as before and the record is marked so.
//
// This is a control against accident, not intent: the identity is an environment variable, and a
// process that wants to can set it. The project's .leadkit/active-agent file is deliberately NOT a
// source here: it is one value shared by every session in the project, so it cannot say which of
// several concurrent reviewers is speaking.
func ResolveBy(asked, identity string) (by string, bound bool, err error) {
	asked, identity = strings.TrimSpace(asked), strings.TrimSpace(identity)
	switch {
	case identity == "" && asked == "":
		return "claude", false, nil
	case identity == "":
		return asked, false, nil
	case asked == "" || asked == identity:
		return identity, true, nil
	default:
		return "", false, fmt.Errorf("gate: REFUSED — this session is identified as %q ($CLAUDE_AGENT_NAME), so it cannot record as %q. Omit -by to record as %q; a verdict under another name would defeat the role-independence rules, which key on it", identity, asked, identity)
	}
}

// ResolveCommit resolves ref to a full 40-hex commit sha, refusing option-like refs and non-commit objects.
func (g *Gate) ResolveCommit(repo, ref string) (string, bool) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", false
	}
	sha, ok := g.Git.RevParse(repo, ref+"^{commit}")
	if !ok || len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return "", false
	}
	return sha, true
}

// Record records one layer verdict for a sha, applying the chaining, write-once, and
// layer-independence rules review-gate.sh enforces. Returns the resolved full sha (for
// the caller's confirmation message) and an error — non-nil only — on refusal, which
// callers should treat as review-gate.sh's `die` (exit 2).
func (g *Gate) Record(opts RecordOpts) (string, error) {
	sha := opts.SHA
	if sha == "" {
		resolved, ok := g.Git.RevParse(opts.Repo, "HEAD")
		if !ok {
			return "", fmt.Errorf("gate: record: could not resolve HEAD sha in %q", opts.Repo)
		}
		sha = resolved
	} else {
		sha = normalizeSHA(g.Git, opts.Repo, sha)
	}

	by := opts.By
	if by == "" {
		by = "claude"
	}
	if opts.Layer == "" {
		return "", fmt.Errorf("gate: record: --layer required (%s)", strings.Join(g.Vocabulary(), " "))
	}
	if opts.Verdict == "" {
		return "", fmt.Errorf("gate: record: --verdict required (ready|changes-needed)")
	}
	if !containsStr(g.Vocabulary(), opts.Layer) {
		return "", fmt.Errorf("gate: record: unknown layer %q (want: %s)", opts.Layer, strings.Join(g.Vocabulary(), " "))
	}
	if opts.Verdict != "ready" && opts.Verdict != "changes-needed" {
		return "", fmt.Errorf("gate: record: unknown verdict %q (want: ready|changes-needed)", opts.Verdict)
	}

	baseSHA := ""
	if opts.Base != "" {
		if opts.Verdict != "ready" {
			return "", fmt.Errorf("gate: record: --base qualifies a ready verdict only; a %s covers nothing beneath its sha", opts.Verdict)
		}
		resolved, ok := g.ResolveCommit(opts.Repo, opts.Base)
		if !ok {
			return "", fmt.Errorf("gate: record: --base %q does not resolve to a commit in %q", opts.Base, opts.Repo)
		}
		baseSHA = resolved
	}

	rec, existed, err := g.readRecordOrNew(sha)
	if err != nil {
		return "", fmt.Errorf("gate: record: %w", err)
	}

	rec, err = g.decide(rec, existed, opts, sha, by, baseSHA)
	if err != nil {
		return "", err
	}

	if err := g.writeRecord(sha, rec); err != nil {
		return "", err
	}
	return sha, nil
}

// decide is Record's decision core: given the record as it stands and what is being recorded, it
// returns the record that would be written, or the refusal. It reads and writes nothing, which is
// what lets a test run every short sequence of records through it and through the pre-166 logic.
func (g *Gate) decide(rec Record, existed bool, opts RecordOpts, sha, by, baseSHA string) (Record, error) {
	// Chained gate: a `ready` is only recordable once every layer before it in the chain is ready.
	// A `changes-needed` is EXEMPT: a blocking finding must never be gated behind the very chain it
	// is blocking, so a dissent on any layer is always recordable regardless of chain state.
	order, idx, inChain := g.order(opts.Layer)
	if err := chained(rec, order, idx, inChain, opts.Layer, opts.Verdict, sha); err != nil {
		return Record{}, err
	}

	// Asymmetric write-once per layer per sha. Blocking must ALWAYS be recordable; only
	// UN-blocking is controlled:
	//   - new = changes-needed over any differing prev (incl. a stale `ready`): ALLOW —
	//     the dissent SUPERSEDES. The prior verdict + author are preserved on the layer
	//     (SupersededVerdict/SupersededBy) for audit; Check then reads the layer as
	//     not-ready and the gate BLOCKS. A blocking dissent must never be refused here.
	//   - new = ready over a prev `changes-needed`: REFUSE (today's message) — un-blocking
	//     on the SAME sha stays controlled; rework is resolved by a NEW sha, never a
	//     silent in-place flip of a dissent to ready.
	//   - same verdict again: idempotent (re-stamped), carrying forward any existing
	//     audit trail so a re-record never wipes the record of an earlier supersede.
	var supersededVerdict, supersededBy string
	var peer *Layer // a different recorder's identical verdict this record would replace; see below
	base, layerBy, byBound := baseSHA, by, opts.ByBound
	evidence := opts.MutationEvidence
	if existed {
		if prev, ok := rec.Layers[opts.Layer]; ok {
			switch {
			case prev.Verdict != "" && prev.Verdict != opts.Verdict:
				if opts.Verdict != "changes-needed" {
					return Record{}, fmt.Errorf("gate: REFUSED — %s for %s is already %q (by %s). A %q verdict is resolved by a NEW sha, not by re-recording this one. Rework, commit, re-gate.", opts.Layer, shortSHA(sha), prev.Verdict, prev.By, prev.Verdict)
				}
				// Dissent supersedes a differing prior verdict — preserve it for audit.
				supersededVerdict, supersededBy = prev.Verdict, prev.By
			default:
				if prev.Verdict != "" && prev.By != "" && prev.By != by && baseSHA == "" {
					peer = &prev
				}
				// Same verdict (or an empty prior): carry forward any existing audit.
				supersededVerdict, supersededBy = prev.SupersededVerdict, prev.SupersededBy
				// The same name recording again keeps the evidence its first record had.
				byBound = byBound || (prev.By == by && prev.ByBound)
				// A re-record that states no evidence keeps the evidence the layer already had.
				if evidence == "" {
					evidence = prev.MutationEvidence
				}
				// A base is write-once: re-recording without one keeps it, adding one to a layer
				// that has none is the amend path, and a DIFFERENT one is refused.
				if prev.Base != "" {
					if baseSHA != "" && baseSHA != prev.Base {
						return Record{}, fmt.Errorf("gate: REFUSED — %s for %s already covers from base %s (by %s); a base is not rewritten. Rework, commit, re-gate on a new sha.", opts.Layer, shortSHA(sha), shortSHA(prev.Base), prev.By)
					}
					base = prev.Base
				} else if baseSHA != "" {
					// Widening is held to the first record's independence, and the layer keeps its original By.
					layerBy, byBound = prev.By, prev.ByBound
					if CanonicalRole(by) != CanonicalRole(prev.By) {
						return Record{}, fmt.Errorf("gate: REFUSED — %q cannot add a base to %s for %s: it was recorded by %q, a different role (%s, not %s). Only the role that made the verdict can widen what it covers.", by, opts.Layer, shortSHA(sha), prev.By, CanonicalRole(prev.By), CanonicalRole(by))
					}
				}
			}
		}
	}

	// Layer independence, keyed on ROLE not instance name. It is checked on EVERY record, not
	// only the first of a layer: a `ready` re-recorded under another layer's role would
	// otherwise leave one role owning both. A blocking verdict is exempt where it can only close
	// the gate; see independent.
	if err := independent(rec, existed, order, idx, opts.Layer, opts.Verdict, by, sha); err != nil {
		return Record{}, err
	}

	// The same verdict from a DIFFERENT recorder is a second reviewer's work landing on the first one's
	// layer. Re-stamping would change whose verdict stands, and the independence checks above key on
	// that, without anyone being told. Refuse, and name the verdict already there. Adding a base is
	// the one exception: it keeps the original recorder and is held to the same role (above). The
	// independence rules run first so a wrong-role recorder is still told it is the wrong role.
	if peer != nil {
		return Record{}, fmt.Errorf("gate: REFUSED — %s for %s is already %q, recorded by %q; %q recording it again would replace %q's verdict. The review is done, so leave the record as it is (a blocking finding is still recordable as changes-needed).", opts.Layer, shortSHA(sha), peer.Verdict, peer.By, by, peer.By)
	}

	if rec.Layers == nil {
		rec.Layers = map[string]Layer{}
	}
	rec.SHA = sha
	rec.Layers[opts.Layer] = Layer{
		Verdict:           opts.Verdict,
		By:                layerBy,
		Ts:                nowUTC(),
		SupersededVerdict: supersededVerdict,
		SupersededBy:      supersededBy,
		Base:              base,
		ByBound:           byBound,
		MutationEvidence:  evidence,
	}

	// Fix 2 — a dissent invalidates sibling `ready`s on the SAME sha: those peers
	// reviewed code a reviewer has now flagged, so their `ready` is suspect. Demote each
	// sibling `ready` to the superseded marker (Check reads it as not-ready), preserving
	// the prior verdict + author for audit. The gate can then be re-satisfied only by a
	// fresh pass — in practice the re-gate on a new sha. Idempotent: a re-recorded
	// changes-needed finds no sibling still `ready` and changes nothing.
	if opts.Verdict == "changes-needed" {
		ts := nowUTC()
		for layer, l := range rec.Layers {
			if layer == opts.Layer || l.Verdict != "ready" {
				continue
			}
			rec.Layers[layer] = Layer{
				Verdict:           supersededMarker,
				Ts:                ts,
				SupersededVerdict: l.Verdict,
				SupersededBy:      l.By,
				MutationEvidence:  l.MutationEvidence,
			}
		}
	}

	return rec, nil
}

// ReadRecord loads sha's existing record, if any — a read-only accessor for callers
// (gate-verdict's layer-independence preflight) that need to inspect a record BEFORE
// deciding whether to call Record at all.
func (g *Gate) ReadRecord(sha string) (Record, error) {
	return g.readRecord(sha)
}

// LayerStatus is one required layer as Show reports it.
type LayerStatus struct {
	Layer             string `json:"layer"`
	Verdict           string `json:"verdict"` // "MISSING" when nothing is recorded for the layer
	By                string `json:"by,omitempty"`
	Ts                string `json:"ts,omitempty"`
	SupersededVerdict string `json:"superseded_verdict,omitempty"`
	SupersededBy      string `json:"superseded_by,omitempty"`
	Base              string `json:"base,omitempty"`              // the sha this layer's review read from; "" = covers only its own sha
	ByBound           bool   `json:"by_bound,omitempty"`          // By was bound to the recorder's identity; false = self-asserted
	MutationEvidence  string `json:"mutation_evidence,omitempty"` // "" = not recorded
}

// Status is Show's answer: what each required layer has recorded for one sha.
type Status struct {
	SHA        string        `json:"sha"`
	RecordsDir string        `json:"records_dir"`
	Layers     []LayerStatus `json:"layers"`
	// ChainSatisfied is true when every required layer is "ready", the same condition
	// Check applies to a governed repo.
	ChainSatisfied bool `json:"chain_satisfied"`
	// Next is the first required layer that is not ready, i.e. the one that has to be recorded
	// (or re-recorded) next given the chain order; "" when the chain is satisfied.
	Next string `json:"next,omitempty"`
	// Scope is "in-scope", "out-of-scope" or "unresolved". Check answers "allow" for the last
	// two whatever is recorded, which is why Show reports the record and the scope separately.
	Scope string `json:"scope"`
}

// Show reports what is recorded for sha (resolved from repo's HEAD if empty) WITHOUT writing
// anything and without Check's fail-open: a sha that cannot be resolved, a repo that is not a
// directory, or a record that cannot be parsed is an error, never an empty answer. It exists so
// that "has code-review recorded yet?" can be asked without recording a verdict to find out.
func (g *Gate) Show(sha, repo string) (Status, error) {
	if repo != "" {
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			return Status{}, fmt.Errorf("gate: show: bad repo %q", repo)
		}
	}
	if sha == "" {
		resolved, ok := g.Git.RevParse(repo, "HEAD")
		if !ok {
			return Status{}, fmt.Errorf("gate: show: could not resolve HEAD (pass --sha, or run inside the repo or with --repo)")
		}
		sha = resolved
	}
	sha = normalizeSHA(g.Git, repo, sha)

	rec, err := g.readRecord(sha)
	if err != nil && !os.IsNotExist(err) {
		return Status{}, err
	}
	st := Status{SHA: sha, RecordsDir: g.RecordsDir, ChainSatisfied: true}
	for _, layer := range g.chain() {
		ls := LayerStatus{Layer: layer, Verdict: "MISSING"}
		if l, ok := rec.Layers[layer]; ok {
			ls = LayerStatus{Layer: layer, Verdict: l.Verdict, By: l.By, Ts: l.Ts,
				SupersededVerdict: l.SupersededVerdict, SupersededBy: l.SupersededBy, Base: l.Base, ByBound: l.ByBound, MutationEvidence: l.MutationEvidence}
		}
		if ls.Verdict != "ready" {
			st.ChainSatisfied = false
			if st.Next == "" {
				st.Next = layer
			}
		}
		st.Layers = append(st.Layers, ls)
	}
	switch g.scopeOf(repo) {
	case scopeIn:
		st.Scope = "in-scope"
	case scopeOut:
		st.Scope = "out-of-scope"
	default:
		st.Scope = "unresolved"
	}
	return st, nil
}

// CheckOutcome classifies a Check result for enforcement-mode-aware callers (push-gate,
// which flips its fail-open on the UNVERIFIABLE outcome when its mode is enforce). The
// legacy Check ok-bool alone can't distinguish a fail-open we COULDN'T evaluate
// (OutcomeUnverifiable — deny under enforce) from a legitimate not-governed allow
// (OutcomeOutOfScope — allow in both modes). See docs/build-tasks/
// configurable-guard-enforcement.md.
type CheckOutcome int

const (
	// OutcomeGated is a clear PASS: all required layers ready, or an explicit BYPASS —
	// allow in every mode.
	OutcomeGated CheckOutcome = iota
	// OutcomeUngated is a clear FAIL: the repo is in scope but its record is
	// missing/incomplete — deny in every mode.
	OutcomeUngated
	// OutcomeOutOfScope means this gate does not GOVERN the repo (origin resolved, no
	// scope match) — a legitimate not-my-repo allow in every mode, never an enforce deny.
	OutcomeOutOfScope
	// OutcomeUnverifiable means the gate COULD NOT evaluate the push — a bad repo path, an
	// unresolvable HEAD, or an unresolvable target origin (can't confirm scope). advisory
	// fails OPEN (allow, today); enforce fails CLOSED (deny).
	OutcomeUnverifiable
)

// Check reports whether sha (resolved from Repo's HEAD if empty) passes the gate, and
// why. Fail-open on anything that leaves the check unable to determine a real answer —
// a bad repo path, an unresolvable HEAD, or an out-of-scope remote — mirrors
// review-gate.sh's cmd_check exactly (a push the gate can't evaluate isn't blocked by
// it). ok is false only for an in-scope repo with a missing or incomplete record. It is
// the byte-identical legacy signature — CheckWithOutcome is the classification-carrying
// form; Check just drops the outcome.
func (g *Gate) Check(sha, repo string) (ok bool, reason string) {
	ok, _, reason = g.CheckWithOutcome(sha, repo)
	return ok, reason
}

// CheckWithOutcome is Check plus the CheckOutcome classification an enforcement-mode
// caller needs. The (ok, reason) pair is byte-identical to what Check has always
// returned — advisory callers (and every existing gate test, which uses Check) are
// unaffected; only the extra outcome is new.
func (g *Gate) CheckWithOutcome(sha, repo string) (ok bool, outcome CheckOutcome, reason string) {
	ok, outcome, reason = g.checkWithOutcome(sha, repo)
	if g.RefuseUnverifiable && outcome == OutcomeUnverifiable {
		return false, outcome, strings.TrimSuffix(reason, " — allow") + " — refused: this repo's push-gate guard is ENFORCING, so a push the gate cannot confirm is refused (fail-closed)"
	}
	return ok, outcome, reason
}

func (g *Gate) checkWithOutcome(sha, repo string) (ok bool, outcome CheckOutcome, reason string) {
	if repo != "" {
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			return true, OutcomeUnverifiable, fmt.Sprintf("bad repo %q — allow", repo)
		}
	}
	if sha == "" {
		resolved, rpOK := g.Git.RevParse(repo, "HEAD")
		if !rpOK {
			return true, OutcomeUnverifiable, "could not resolve HEAD sha — allow"
		}
		sha = resolved
	}
	sha = normalizeSHA(g.Git, repo, sha)

	if ok, outcome, reason, done := g.screen(sha, repo); done {
		return ok, outcome, reason
	}

	chain := g.chain()
	rec, err := g.readRecord(sha)
	if err != nil {
		return false, OutcomeUngated, fmt.Sprintf("no review record for %s. Required layers: %s", shortSHA(sha), strings.Join(chain, " "))
	}
	if missing := Missing(rec, chain); len(missing) > 0 {
		return false, OutcomeUngated, "layers not ready: " + strings.Join(missing, " ")
	}
	return true, OutcomeGated, fmt.Sprintf("all layers ready (%s) for %s", strings.Join(chain, " "), shortSHA(sha))
}

// screen is the part of the check that does not depend on any record: scope, then the
// BYPASS escape hatch. done is true when it settled the verdict (out of scope, scope
// unresolvable, or bypassed); false means the caller must go on to judge a record. sha
// is only for the bypass message and may be "" when no commit is known.
func (g *Gate) screen(sha, repo string) (ok bool, outcome CheckOutcome, reason string, done bool) {
	forWhat := "this push"
	if sha != "" {
		forWhat = shortSHA(sha)
	}
	switch g.scopeOf(repo) {
	case scopeOriginUnreadable:
		// Its own reason, because the cause is known and so is the fix, and it is not a
		// statement about what the profile governs. The escape hatch the refusal names is read
		// here, ahead of the outcome, so it lifts the refusal in every mode.
		if reason, bypassed := g.bypass(forWhat); bypassed {
			return true, OutcomeGated, reason, true
		}
		return true, OutcomeUnverifiable, OriginUnreadableReason + " — allow", true
	case scopeUnresolved:
		// Legacy reason preserved byte-for-byte; only the outcome distinguishes this
		// can't-confirm-scope case (enforce denies) from a genuine out-of-scope allow.
		return true, OutcomeUnverifiable, "out of scope — allow", true
	case scopeOut:
		return true, OutcomeOutOfScope, "out of scope — allow", true
	}
	if reason, bypassed := g.bypass(forWhat); bypassed {
		return true, OutcomeGated, reason, true
	}
	return false, 0, "", false
}

// OriginUnreadableReason is the reason a profiled repo gets when the origin its scope is decided
// by cannot be read, and OriginUnreadableFix is what to do about it. Callers that explain a
// refusal recognise the cause by OriginUnreadable and give this advice instead of the generic
// "push from the real repo" one, which would send the user to the wrong place.
const (
	OriginUnreadableReason = "scope unresolved: this project has a profile but no remote named `origin` can be read from it, so whether this repo is the one the profile governs cannot be confirmed"
	OriginUnreadableFix    = "Name the remote origin: `git remote rename <name> origin` (`git remote -v` lists the remotes)"
)

// OriginUnreadable reports whether a Check reason is the unreadable-origin one.
func OriginUnreadable(reason string) bool {
	return strings.Contains(reason, OriginUnreadableReason)
}

// bypass reports whether the BYPASS escape hatch (the env var, else the records dir's
// BYPASS file) is in force, with the allow reason the gate logs for forWhat.
func (g *Gate) bypass(forWhat string) (reason string, bypassed bool) {
	if bp := os.Getenv("REVIEW_GATE_BYPASS"); bp != "" {
		return fmt.Sprintf("BYPASS env set (%q) — allow (logged) for %s", bp, forWhat), true
	}
	if data, err := os.ReadFile(filepath.Join(g.RecordsDir, "BYPASS")); err == nil {
		return fmt.Sprintf("BYPASS file — allow (logged) for %s: %s", forWhat, strings.TrimSpace(string(data))), true
	}
	return "", false
}

// CheckUnknownTarget judges a push whose TARGET REPO the caller cannot determine from the
// command's text: a path built by a command substitution, a glob or brace pattern or `$'…'`
// quoting, a `~user`, a variable the line assigns or the hook does not hold, `cd -`, a directory
// change the hook does not read the target of. It is not the same question as "this path is
// not a repo". A plain path the command spells, as the first thing on the line, that does not
// exist is a `cd` or `git -C` that fails AT CHECK TIME, so no push runs there: the ordinary
// bad-repo case, which stays the mode-dependent OutcomeUnverifiable. That is a property of the
// moment of the check, not of the command: a process started in an earlier call can create the
// directory (a clone of a gated repo) between the check and the shell's `cd`, so the answer is a
// known residual, not a guarantee. Anything the shell rewrites before
// cd sees it, or that an earlier command on the line may create, could be a repo this gate
// governs, and whether it does cannot be settled, so the scope test does not apply: only
// the BYPASS escape hatch lets it through, and without it the answer is OutcomeUngated, which
// refuses in every mode. g is the gate of the acting session, the only repo known.
func (g *Gate) CheckUnknownTarget(why string) (ok bool, outcome CheckOutcome, reason string) {
	if reason, bypassed := g.bypass("this push"); bypassed {
		return true, OutcomeGated, reason
	}
	return false, OutcomeUngated, "could not determine which repository this push targets (" + why + ")"
}

// CheckUnreadableProfile judges a push whose repo reads as out of scope only because its
// governing profile could not be READ (it exists and was not accepted, or the repo is
// evidently a leadkit project and no profile was found). Scope does not apply: the built-in
// default that stood in for the profile governs nothing, which is an unknown and not a
// decision. Only the BYPASS escape hatch lets it through; without it the answer is
// OutcomeUngated, which refuses in every mode. why says what could not be read.
func (g *Gate) CheckUnreadableProfile(why string) (ok bool, outcome CheckOutcome, reason string) {
	if reason, bypassed := g.bypass("this push"); bypassed {
		return true, OutcomeGated, reason
	}
	return false, OutcomeUngated, why
}

// CheckUndetermined judges a push whose commits the caller could NOT determine (an
// unresolvable ref, a wildcard refspec, --all/--mirror/--tags). There is no sha to look a
// record up for and none may be borrowed from anywhere ambient, so the answer is: out of
// scope or bypassed is allowed exactly as in CheckWithOutcome, a bad repo or unresolvable
// scope is OutcomeUnverifiable, and everything else is REFUSED (OutcomeUngated) — a push
// the gate cannot name is not a push it can call gated. why says what could not be
// determined.
func (g *Gate) CheckUndetermined(repo, why string) (ok bool, outcome CheckOutcome, reason string) {
	if repo != "" {
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			return true, OutcomeUnverifiable, fmt.Sprintf("bad repo %q — allow", repo)
		}
	}
	if ok, outcome, reason, done := g.screen("", repo); done {
		return ok, outcome, reason
	}
	return false, OutcomeUngated, "could not determine which commits this push publishes (" + why + ")"
}

// inScope decides whether the gate governs a push to repo. Two independent sources,
// either of which puts the repo in scope:
//
//   - OWN ORIGIN (always): repo's origin remote equals the governing project's own
//     origin (derived from g.ProjectRoot's origin remote). So a project gates its own
//     pushes with zero config, and NOTHING in enforced_remotes can switch that off — a
//     repo's own safety property must not hang on a list meant for naming other repos.
//     An own origin is resolvable only when g.ProjectRoot is set AND that project's
//     `origin` remote can be read. Two different things leave it unresolvable, and they
//     must not be confused: g.ProjectRoot unset (an UNPROFILED target, config.Defaults())
//     contributes nothing, so a genuinely different, unprofiled repo (governed by its
//     own profile, if any) is not swept in and is out of scope; g.ProjectRoot set with no
//     readable `origin` (the remote has another name, or there is none) also contributes
//     nothing, but a profile DOES govern, so, with no explicit list, the answer is "cannot
//     confirm" (scopeOriginUnreadable), never "out of scope". With an explicit list the
//     list is consulted first and the answer concerns the target: a readable target the
//     list does not name is out of scope, an unreadable one is scopeUnresolved. An
//     unresolvable own origin does not imply an unset ProjectRoot.
//
//   - EXPLICIT list (g.EnforcedRemotes non-empty): repo's origin URL CONTAINS any listed
//     substring — the historical semantics. An adopter whose profile gates repos OTHER
//     than its own origin names them here. The list ADDS to own-origin gating; it never
//     replaces it.
//
// UNIONED on top of both modes is g.SessionEnforcedRemotes — the ACTING SESSION's own
// enforced_remotes. It is ADD-ONLY: a target the session's list names is in scope even
// when the target's own gating would not cover it (an unprofiled cross-project clone),
// but the session list can NEVER take a target OUT of scope that its own config gates.
func (g *Gate) inScope(repo string) bool {
	return g.scopeOf(repo) == scopeIn
}

// scopeResult is inScope's refinement for the enforcement-mode outcome. inScope == scopeIn;
// every other value is "not in scope", and they differ in what the gate may say about it:
//
//   - scopeOut is a RESOLVED answer: the repo was read and is not this project's, or no
//     profile governs at all. It is allowed in every mode.
//   - scopeUnresolved and scopeOriginUnreadable are NOT answers: the gate could not
//     confirm whether the repo is governed. They classify as OutcomeUnverifiable, so
//     enforce mode fails CLOSED and advisory mode allows. The boolean inScope was never
//     able to tell "resolved, not my repo" from "couldn't resolve, can't confirm."
//
// Of the two, scopeUnresolved is the TARGET's origin being unreadable when there is
// something to match it against (an explicit list, or a readable own origin);
// scopeOriginUnreadable is a profile governing the project whose OWN origin cannot be
// read and no explicit list to decide by.
type scopeResult int

const (
	scopeIn scopeResult = iota
	scopeOut
	scopeUnresolved
	// scopeOriginUnreadable is scopeUnresolved for one known cause: a profile governs the
	// project and the origin it is scoped by cannot be read. It gets its own reason and advice,
	// and the BYPASS escape hatch lifts it in every mode.
	scopeOriginUnreadable
)

// scopeOf is inScope's classifying core. It preserves inScope's boolean exactly (scopeIn
// where inScope is true, anything else where it is false) and adds the distinction the
// boolean could not make on the not-in-scope side: resolved and not governed (scopeOut),
// versus unable to confirm (scopeUnresolved, scopeOriginUnreadable). With no explicit
// list, an empty own origin is scopeOut when ProjectRoot is also empty (no profile
// governs) and scopeOriginUnreadable when it is set. With an explicit list the list is
// decided first and the answer is about the target, whatever the own origin: scopeIn on
// a match, scopeUnresolved if the target's origin is unreadable, else scopeOut. See
// inScope's doc for the session-union / own-origin / explicit-list rules.
func (g *Gate) scopeOf(repo string) scopeResult {
	url := g.Git.RemoteURL(repo)
	// (b) The acting session's declared list gates its cross-project pushes — checked
	// first, ADD-ONLY (only ever returns scopeIn; never shrinks scope).
	for _, r := range g.SessionEnforcedRemotes {
		if r != "" && url != "" && strings.Contains(url, r) {
			return scopeIn
		}
	}
	// (a) The target's OWN origin is ALWAYS gated, independent of enforced_remotes: a
	// repo's own safety property must not be switchable by a list whose job is to name
	// OTHER repos (tickets 11 and 87). Checked before the explicit list so a populated
	// list can never make the project's own origin out of scope.
	own := g.ownOrigin()
	if own != "" && url != "" && url == own {
		return scopeIn
	}
	// (b') An explicit list ADDS the OTHER repos it names.
	if len(g.EnforcedRemotes) > 0 {
		for _, r := range g.EnforcedRemotes {
			// Preserves the historical semantics exactly, including an empty entry
			// (strings.Contains(url, "") == true) — no behavior change.
			if strings.Contains(url, r) {
				return scopeIn
			}
		}
		if url == "" {
			// An explicit list to match against, but the target's origin couldn't be
			// resolved — can't confirm it's out of scope.
			return scopeUnresolved
		}
		return scopeOut
	}
	if own == "" {
		if g.ProjectRoot != "" {
			// A profile governs this project, and the origin it would be gated by cannot be read
			// (the remote is not named `origin`, or there is none). That is "cannot confirm", not
			// "not governed": answering scopeOut turned the whole gate off, in every mode, by
			// renaming one remote. Enforce denies; advisory allows.
			return scopeOriginUnreadable
		}
		// No profile at all — an UNPROFILED target (config.Defaults()): the
		// gate-your-own-repo-not-everyone-else's default. Genuinely not governed.
		return scopeOut
	}
	if url == "" {
		// We DO have an own origin to gate, but the target's origin is unknown — can't
		// confirm whether it's our repo. Unverifiable (enforce denies; advisory allows).
		return scopeUnresolved
	}
	return scopeOut
}

// ownOrigin is the governing project's own origin remote URL — the origin of
// g.ProjectRoot — or "" when ProjectRoot is unset or has no resolvable origin. It is
// the anchor for the always-on gate-my-own-origin scope (see inScope).
func (g *Gate) ownOrigin() string {
	if g.ProjectRoot == "" {
		return ""
	}
	return g.Git.RemoteURL(g.ProjectRoot)
}

// isAllZeroOrEmpty reports whether s is "" or made up only of '0' characters — the
// pre-push protocol's marker for "no sha" (branch delete).
func isAllZeroOrEmpty(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

// CheckPush implements the git pre-push protocol: stdin lines of
// "<local-ref> <local-sha> <remote-ref> <remote-sha>", one per ref being pushed. Each
// local sha (skipping all-zero, a branch delete) is run through Check in repo; the
// push is refused if any fails. No qualifying rows at all is a pass (nothing to check).
func (g *Gate) CheckPush(r io.Reader, repo string) (bool, error) {
	ok, _, err := g.CheckPushReasons(r, repo)
	return ok, err
}

// PushVerdict is what the pre-push protocol decided for every ref being pushed.
type PushVerdict struct {
	OK         bool
	Reasons    []string // one line per refused ref
	OutOfScope bool     // some ref was allowed ONLY because the gate does not govern the repo
}

// CheckPushReasons is CheckPush that also returns why each refused ref was refused, one
// line per ref, so the pre-push hook can say what to do. A refusal nobody can diagnose
// drives people to the standing BYPASS file.
func (g *Gate) CheckPushReasons(r io.Reader, repo string) (ok bool, reasons []string, err error) {
	v, err := g.CheckPushVerdict(r, repo)
	return v.OK, v.Reasons, err
}

// CheckPushVerdict is CheckPushReasons that also says whether any ref was let through only as
// out of scope. An out-of-scope verdict is an exemption only if a profile was READ to reach it,
// and the caller (which can ask that) must; see cmd/leadkit.hookProfileProblem.
func (g *Gate) CheckPushVerdict(r io.Reader, repo string) (PushVerdict, error) {
	var v PushVerdict
	scanner := bufio.NewScanner(r)
	any := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 {
			continue
		}
		lsha := fields[1]
		if isAllZeroOrEmpty(lsha) {
			continue
		}
		any = true
		// git hands the hook the sha of the REF'S OBJECT, which for an annotated tag is the tag
		// object and not a commit anyone reviewed. The record is of a commit, so judge the commit
		// the ref peels to, and name both in the refusal. A ref that does not peel to a commit (a
		// tag of a tree or a blob, or one git cannot resolve) is judged as it stands: in scope that
		// finds no record for it and refuses, out of scope it is allowed exactly as before.
		named, subject := lsha, lsha
		if commit, resolved := g.ResolveCommit(repo, lsha); resolved && commit != lsha {
			named, subject = lsha+" -> commit "+commit, commit
		}
		ok, outcome, reason := g.CheckWithOutcome(subject, repo)
		if !ok {
			v.Reasons = append(v.Reasons, fields[0]+" ("+named+"): "+reason)
		}
		if outcome == OutcomeOutOfScope {
			v.OutOfScope = true
		}
	}
	if err := scanner.Err(); err != nil {
		return PushVerdict{}, fmt.Errorf("gate: check-push: read stdin: %w", err)
	}
	v.OK = len(v.Reasons) == 0
	if !any {
		v.OK = true
	}
	return v, nil
}
