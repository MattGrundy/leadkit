// Package config loads leadkit's per-project profile — the single declarative
// leadkit.yaml that consolidates everything currently scattered across the Go
// packages' own hardcoded defaults and paths (queue root, records dir, ROLES path,
// enforced remotes, keychain service, telemetry log, burn thresholds, the
// reviewer-pool roster, the heartbeat schedule, the goal/handover file locations, and
// per-repo authority policy). See docs/PORTABLE-OPERATING-KIT.md.
//
// Every field is optional: Defaults() returns a profile pointed at this project's
// existing built-in defaults (the same ones internal/queue, internal/gate, etc.
// already use), and Load() merges a discovered leadkit.yaml over that — so a project
// with no profile file still gets a fully usable Profile.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"mgpp.dev/leadkit/internal/burn"
	"mgpp.dev/leadkit/internal/gate"
	"mgpp.dev/leadkit/internal/queue"
	"mgpp.dev/leadkit/internal/redact"
	"mgpp.dev/leadkit/internal/telemetry"
	"mgpp.dev/leadkit/internal/usage"
)

// DefaultProfileName is the filename Load discovers by walking up from cwd, mirroring
// git's .git discovery.
const DefaultProfileName = "leadkit.yaml"

// Default burn-rate advisory thresholds — mirror the literals cmd/leadkit's `rate`
// subcommand currently hardcodes (400/hr, 40/hr). Kit-1 only defines the profile
// field; wiring `rate` to read it is a later step.
const (
	DefaultOpRateWarn    = 400
	DefaultAgentRateWarn = 40
)

// Default goal/handover locations — deliberately namespaced under .leadkit/ rather
// than the project root: a project may already keep a large hand-maintained
// HANDOVER.md at its root, and defaulting to that name would risk leadkit's own
// Emit/ensure machinery overwriting it.
const (
	DefaultGoalFile     = ".leadkit/goal.md"
	DefaultHandoverFile = ".leadkit/handover.md"
)

// DefaultMilestonesFile is milestones_file's default — the machine-readable milestone
// + DoD anchor, namespaced under .leadkit/ like goal.md/handover.md, and DISTINCT from
// the taxonomy's <store_root>/milestones.yaml. DefaultIdleGateMode is
// milestone.idle_gate_mode's default: block (the hard gate only earns its keep by
// actually blocking; a supervised run passes --mode warn).
const (
	DefaultMilestonesFile = ".leadkit/milestones.yaml"
	DefaultIdleGateMode   = "block"
	// DefaultScriptTimeout is milestone.script_timeout's default — a time.ParseDuration
	// string bounding every `type: script` DoD signal run so a hung check can't wedge
	// `milestone check`/the sweep re-anchor (a timeout reads unverified, never a false met).
	DefaultScriptTimeout = "30s"
)

// DefaultScriptTimeoutDur is the parsed DefaultScriptTimeout, the fallback for an
// empty/unparseable milestone.script_timeout.
const DefaultScriptTimeoutDur = 30 * time.Second

// RosterSpec is one entry of the reviewer/agent pool that should be live.
type RosterSpec struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`

	// Lane is an OPTIONAL explicit live queue-lane name override — `leadkit team
	// scaffold` (queue-mode-enforcement item 4/GAP-4) uses it to bind more than one
	// roster role to the SAME queue-consumer lane (e.g. qa-agent + recon-agent
	// sharing one lightweight reviewer, instead of forcing a separate consumer per
	// role). Unset: scaffold defaults the lane to the role name itself, same as
	// today. Binding code-review and critique to the same lane is REFUSED by
	// scaffold — that pair must never share a consumer (layer independence); see
	// docs/build-tasks/queue-mode-enforcement.md item 4.
	Lane string `yaml:"lane,omitempty"`

	// ReviewLayer declares this role review-capable: the role's name becomes a review layer, a
	// valid `gate record --layer` value and a valid authority.<repo>.required_gates entry.
	// code-review and critique are review layers in every project and need no entry. A role is
	// review-capable because the roster says so, not because of what its name looks like.
	ReviewLayer bool `yaml:"review_layer,omitempty"`
}

// Heartbeat is the canonical cron schedule used to re-arm the idle heartbeat.
type Heartbeat struct {
	Schedule  string `yaml:"schedule,omitempty"`   // cron expression
	PromptRef string `yaml:"prompt_ref,omitempty"` // what to run on each tick
}

// Burn holds the burn-governor / burn-rate thresholds.
type Burn struct {
	SessionReserve float64 `yaml:"session_reserve"`
	OpRateWarn     int     `yaml:"op_rate_warn"`
	AgentRateWarn  int     `yaml:"agent_rate_warn"`

	// WorkstreamTTL is how stale a project's entry in the account-wide workstream
	// registry (docs/build-tasks/cross-project-burn.md) may be before it's pruned
	// from the account total burn.Input.AccountN sums — a crashed/idle project's last
	// count can't hold phantom account headroom forever. A time.ParseDuration string
	// (e.g. "30m"), same convention as Heartbeat.Schedule being a plain cron string —
	// parsed on demand by the caller (cmd/leadkit), falling back to
	// burn.DefaultWorkstreamTTL on empty/unparseable.
	WorkstreamTTL string `yaml:"workstream_ttl"`

	// WorkstreamStaleWindow is how stale a workstream/presence registry entry may be
	// before the GOVERNOR ignores it when computing account_n
	// (docs/build-tasks/burn-auto-enforce.md) — tighter than WorkstreamTTL (which prunes
	// the registry) so a crashed/exited session stops inflating the account count (and
	// stops holding the floor) promptly. The auto-register presence heartbeat is
	// refreshed on every session activity (the usage-logger PreToolUse hook), so a
	// genuinely-live session never falls past this window. A time.ParseDuration string
	// (e.g. "15m"), parsed on demand via WorkstreamStaleWindowDur, falling back to
	// burn.DefaultWorkstreamStaleWindow on empty/unparseable.
	WorkstreamStaleWindow string `yaml:"workstream_stale_window"`

	// EnforceInterval is how often the account-level burn-enforce daemon
	// (dev.leadkit.burn, docs/build-tasks/burn-auto-enforce.md) ticks: each tick reads
	// real account weekly usage and writes/clears the account pause lock from the
	// pause_pct/resume_pct hysteresis, so the throttle fires WITHOUT any session running
	// `leadkit burn`. A time.ParseDuration string (e.g. "2m"), parsed on demand via
	// EnforceIntervalDur, falling back to burn.DefaultEnforceInterval on empty/
	// unparseable. `leadkit init` translates it into the daemon's StartInterval.
	EnforceInterval string `yaml:"enforce_interval"`

	// ProductivityStallWindow is how long an agent may be ACTIVE (alive + churning:
	// spawning subagents, touching the fs) while producing NO deliverable — no new
	// commit, recorded gate verdict, queue advancement, or session-task→done — before
	// the productivity watcher (docs/build-tasks/productivity-watcher.md) flags a
	// productive-stall. "Busy is not productive": the "orchestrator ran 12h, zero
	// deliverables" drift. A time.ParseDuration string (e.g. "3h"), same convention as
	// WorkstreamTTL — parsed on demand via ProductivityStallWindowDur, falling back to
	// burn.DefaultProductivityStallWindow on empty/unparseable.
	ProductivityStallWindow string `yaml:"productivity_stall_window"`

	// InprogressStallWindow is how long a ticket may sit CLAIMED in an in-progress
	// lane (its in-progress symlink mtime) with NO deliverable advancement — no new
	// commit, gate verdict, or queue stage-move — before the frozen-claim detector
	// (docs/build-tasks/frozen-claim-and-noninteractive.md) flags it as a hung owner's
	// frozen claim (a held ticket standing still, invisible to owner-liveness — which
	// reads in-progress>0 as "consuming" — and to productivity — which needs an activity
	// signal a hung agent doesn't emit). A time.ParseDuration string (e.g. "45m"), same
	// convention as ProductivityStallWindow — parsed on demand via
	// InprogressStallWindowDur, falling back to burn.DefaultInprogressStallWindow on
	// empty/unparseable. Generous by default so a legitimately long task doesn't nag.
	InprogressStallWindow string `yaml:"inprogress_stall_window"`

	// SessionPriority is this session's self-declared priority tier
	// ("critical"|"high"|"normal"|"low") in the account-wide session registry
	// (docs/build-tasks/session-priority-burn.md) — governs preemptive
	// shedding order under account budget pressure. Empty/unset means normal.
	// A $LEADKIT_SESSION_PRIORITY env var, read by the caller (cmd/leadkit),
	// OVERRIDES this field — lets a session set its priority for one run
	// without editing the profile.
	SessionPriority string `yaml:"session_priority,omitempty"`
}

// WorkstreamStaleWindowDur parses WorkstreamStaleWindow, falling back to
// burn.DefaultWorkstreamStaleWindow on empty/unparseable — so every consumer (the
// burn governor's account_n read + the auto-register presence read + doctor's presence
// check) reads the window the same way and can never disagree about it.
func (b Burn) WorkstreamStaleWindowDur() time.Duration {
	if b.WorkstreamStaleWindow == "" {
		return burn.DefaultWorkstreamStaleWindow
	}
	if d, err := time.ParseDuration(b.WorkstreamStaleWindow); err == nil && d > 0 {
		return d
	}
	return burn.DefaultWorkstreamStaleWindow
}

// EnforceIntervalDur parses EnforceInterval, falling back to burn.DefaultEnforceInterval
// on empty/unparseable — so `leadkit init` (daemon StartInterval) and any other consumer
// read the tick cadence the same way.
func (b Burn) EnforceIntervalDur() time.Duration {
	if b.EnforceInterval == "" {
		return burn.DefaultEnforceInterval
	}
	if d, err := time.ParseDuration(b.EnforceInterval); err == nil && d > 0 {
		return d
	}
	return burn.DefaultEnforceInterval
}

// ProductivityStallWindowDur parses ProductivityStallWindow, falling back to
// burn.DefaultProductivityStallWindow on empty/unparseable — so every consumer (the
// doctor productivity check + the sweep-daemon signal) reads the window the same way
// and can never disagree about it.
func (b Burn) ProductivityStallWindowDur() time.Duration {
	if b.ProductivityStallWindow == "" {
		return burn.DefaultProductivityStallWindow
	}
	if d, err := time.ParseDuration(b.ProductivityStallWindow); err == nil && d > 0 {
		return d
	}
	return burn.DefaultProductivityStallWindow
}

// InprogressStallWindowDur parses InprogressStallWindow, falling back to
// burn.DefaultInprogressStallWindow on empty/unparseable — so every consumer (the
// doctor frozen-claims check + the sweep-daemon frozen-claim report line) reads the
// window the same way and can never disagree about it.
func (b Burn) InprogressStallWindowDur() time.Duration {
	if b.InprogressStallWindow == "" {
		return burn.DefaultInprogressStallWindow
	}
	if d, err := time.ParseDuration(b.InprogressStallWindow); err == nil && d > 0 {
		return d
	}
	return burn.DefaultInprogressStallWindow
}

// BoolOrHuman is a policy field that's either a plain bool, or the literal string
// "human" — meaning that action always needs a human, regardless of any other
// automated state (e.g. the gate). Used by authority.<repo>.can_open_pr and
// .can_merge.
type BoolOrHuman struct {
	Human bool // true: the string "human" was given; Allow is meaningless then
	Allow bool // the bool value, when Human is false
}

// UnmarshalYAML accepts either a bool or the string "human". Dispatches on the node's
// resolved YAML tag rather than trying a string decode first — yaml.v3 will happily
// decode a !!bool scalar's raw text ("true") into a string target too, so that order
// would silently misread every plain bool as the wrong branch.
func (c *BoolOrHuman) UnmarshalYAML(value *yaml.Node) error {
	switch value.Tag {
	case "!!bool":
		var b bool
		if err := value.Decode(&b); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		*c = BoolOrHuman{Allow: b}
		return nil
	case "!!str":
		var s string
		if err := value.Decode(&s); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if s != "human" {
			return fmt.Errorf("config: unknown string %q (want true, false, or \"human\")", s)
		}
		*c = BoolOrHuman{Human: true}
		return nil
	default:
		return fmt.Errorf("config: want a bool or \"human\" (got YAML tag %s)", value.Tag)
	}
}

// MarshalYAML renders BoolOrHuman back to the bool or "human" it was decoded from.
func (c BoolOrHuman) MarshalYAML() (interface{}, error) {
	if c.Human {
		return "human", nil
	}
	return c.Allow, nil
}

// AuthoritySpec is one repo's entry in the authority policy map: what an agent may do
// there unsupervised, and what always needs a human. Data only — kit-1 defines and
// parses this; nothing enforces it yet.
type AuthoritySpec struct {
	CanPush   bool        `yaml:"can_push"`
	CanOpenPR BoolOrHuman `yaml:"can_open_pr"`
	// PRDraftOnly: when true, PRs in this repo may only be opened as draft.
	PRDraftOnly bool        `yaml:"pr_draft_only"`
	CanMerge    BoolOrHuman `yaml:"can_merge"`
	// MergeGrant: a standing-grant id (e.g. an incident/ticket ref) that lets the
	// lead merge WITHOUT per-merge human approval while it's set. Empty/absent means
	// merge stays fully human, regardless of CanMerge.
	MergeGrant    string   `yaml:"merge_grant,omitempty"`
	RequiredGates []string `yaml:"required_gates,omitempty"`
	// PushWhitelist, when set, REPLACES the top-level push_whitelist entirely for
	// this repo (no merge) — internal/hook's push-guard resolves the repo via its
	// origin remote URL the same substring way EnforcedRemotes/Authority already
	// match. See Profile.PushWhitelist.
	PushWhitelist []string `yaml:"push_whitelist,omitempty"`

	// RawPRVerbs is the additive, explicit allow-list of canonical PR verbs
	// ("create", "edit") a raw forge CLI (gh/glab) may invoke WITHOUT a wrapper —
	// see internal/hook.PRGuard. Absent/empty means today's behavior: every raw
	// PR verb is denied, pointing at the wrapper. "merge" here is GUARD-RAILED:
	// PRGuard ignores it outright (raw merge is never grantable, ever) and
	// `leadkit doctor` warns it's a misconfiguration.
	RawPRVerbs []string `yaml:"raw_pr_verbs,omitempty"`

	// OwnerPRsOnly, when true, restricts a granted raw `edit` to PRs the
	// authenticated forge user themselves authored — an unresolvable/failed
	// author lookup fails CLOSED (denied), never open. Never restricts `create`
	// (inherently self-authored) or `merge` (always denied regardless).
	OwnerPRsOnly bool `yaml:"owner_prs_only,omitempty"`

	// Forge is an OPTIONAL per-repo forge override for `leadkit pr create` (and the
	// pr-guard's own detection): "github" or "gitlab". Default empty means the forge
	// is chosen by the origin host heuristic (a host containing "github" → gh,
	// "gitlab" → glab); set this only for a self-hosted host that names itself
	// neither, so PR-create isn't refused with "cannot determine forge". See
	// internal/prcreate.forgeFor.
	Forge string `yaml:"forge,omitempty"`

	// ProtectedBranches names the branches `leadkit forge protect` configures and
	// `leadkit doctor` verifies server-side (forge branch protection). Exact names only —
	// no globs. Empty means the repo's default branch, as the forge reports it.
	ProtectedBranches []string `yaml:"protected_branches,omitempty"`
}

// TestSpec is one repo's `leadkit test run` override — see Profile.Tests.
type TestSpec struct {
	// TestCommand, if set, REPLACES the inferred suite command entirely (run
	// verbatim via a shell, so any syntax — flags, &&, pipes — works).
	TestCommand string `yaml:"test_command,omitempty"`
	// AffectedCommand, if set, replaces the inferred affected-tests command
	// (used only when `test run --affected` is in effect).
	AffectedCommand string `yaml:"affected_command,omitempty"`
}

// LinearConfig is `leadkit sync`'s Linear target — see Profile.Linear. Every
// field here is an IDENTIFIER or a mode name (a Linear team key, an Initiative
// id, a workspace state name, mcp|direct) — never a token; this config holds no
// Linear credential. In MCP mode the sync-executor agent's own Linear MCP
// connection is the only thing that authenticates; in DIRECT mode the key comes
// from env LINEAR_API_KEY or the keychain at runtime (internal/linearapi).
type LinearConfig struct {
	// TeamID is the target Linear team (e.g. "CI") — the workspace sync writes
	// into. Required for the executor to run; doctor warns if outputs includes
	// "linear" and this is unset.
	TeamID string `yaml:"team_id,omitempty"`
	// RootInitiative is the Linear Initiative id the local Project maps to. If
	// unset, the executor creates it on its first run and record-ids it (same
	// create-then-record-id idempotency as every other node).
	RootInitiative string `yaml:"root_initiative,omitempty"`
	// ClosedState/DefaultState are the Linear workspace's own state NAMES for a
	// `close` op / a `create` op — state names are workspace-specific, so
	// there's no universal default beyond these common ones.
	ClosedState  string `yaml:"closed_state,omitempty"`
	DefaultState string `yaml:"default_state,omitempty"`
	// SyncMode optionally FORCES the execution mode: "mcp" or "direct". Unset (the
	// default) means env-key resolution: LINEAR_API_KEY present -> direct, else
	// mcp. A `sync run --mode` flag overrides this.
	SyncMode string `yaml:"sync_mode,omitempty"`
}

// Profile is the per-project profile. Every field is optional — Defaults() supplies
// this project's built-in values, and a discovered leadkit.yaml is merged over them
// (only the keys present in the file override their default).
type Profile struct {
	// ProjectID is an optional, stable per-project identifier. Its only current
	// consumer is internal/install.ResolveProjectSlug (per-project daemon
	// namespacing — docs/build-tasks/per-project-daemons.md): when set, it wins
	// outright over a queue_root-derived slug, which matters whenever queue_root
	// doesn't sit under a distinctively-named project directory. Unset is fine —
	// the queue_root basename is a good-enough default for most layouts.
	ProjectID string `yaml:"project_id,omitempty"`

	// ProjectOrigin is the origin the repo this EXTERNAL config governs must have, as
	// `owner/repo` or `host/owner/repo` (see OriginMatchesProject). An external config is
	// found by the origin's repo NAME alone, so any same-named clone on the machine would load
	// it, including a different owner's, and take its absolute store_root and queue_root. When
	// set, the loader refuses a repo whose origin does not match. Unset: no check, which is
	// the exposure this key exists to close; `config init --external` writes it.
	ProjectOrigin string `yaml:"project_origin,omitempty"`

	QueueRoot  string `yaml:"queue_root"`
	StoreRoot  string `yaml:"store_root"`
	RecordsDir string `yaml:"records_dir"`
	RolesPath  string `yaml:"roles_path"`

	// IssuesRoot is the taxonomy's Issue level: a flat dir of issues/*.md, sibling
	// to StoreRoot's tasks/ (docs/HIERARCHY-AND-SYNC.md: Project -> Milestone ->
	// Epic -> Issue -> Task; Issue sits between milestones.yaml's epics and
	// StoreRoot's tasks). NOT a rename of tasks/ — a new, separate dir.
	IssuesRoot string `yaml:"issues_root"`

	// EnforcedRemotes is the OPTIONAL list of OTHER repos' origin-URL substrings the
	// review-gate also enforces on (a remote matches if its URL CONTAINS any listed
	// value). The project's OWN origin is always gated, whatever this list holds —
	// empty, populated, or omitting the project itself — so a project gates its own
	// pushes with zero config (see internal/gate.Gate.inScope). Declare this ONLY to
	// gate repos OTHER than the profile's own origin (e.g. a profile that governs
	// pushes to clones of several upstream repos).
	EnforcedRemotes []string `yaml:"enforced_remotes"`
	KeychainService string   `yaml:"keychain_service"`
	TelemetryLog    string   `yaml:"telemetry_log"`

	// PRFallbackHint is an OPTIONAL adopter-specific trailer the PR-guard appends to
	// its raw-forge deny messages — e.g. a pointer to a legacy/local PR helper an
	// org still keeps around. NO default: the deny message names the governed
	// `leadkit pr ...` path generically, and this hint is appended only when set, so
	// no adopter script path is ever baked into the source. See internal/hook.PRGuard.
	PRFallbackHint string `yaml:"pr_fallback_hint,omitempty"`

	// Hierarchy carries the OPTIONAL curated `hierarchy propose` hint tables — the
	// adopter's own type->epic and keyword->epic seeds. Source ships NONE (a project
	// sets these in leadkit.yaml); the generic model-driven alias/title match in
	// `hierarchy propose` runs regardless, so an empty block still proposes on real
	// per-task signals. See internal/hierarchy.Propose.
	Hierarchy Hierarchy `yaml:"hierarchy,omitempty"`

	Burn Burn `yaml:"burn"`

	Roster []RosterSpec `yaml:"roster,omitempty"`

	Heartbeat Heartbeat `yaml:"heartbeat"`

	// WakeCommand is the OPTIONAL external notifier the waker daemon (`leadkit queue
	// watch` / internal/queue.Watch) execs when a ticket lands in a role's pending/
	// inbox. Default EMPTY — and empty is the correct default now: an agent's steady
	// idle state is an event-driven `leadkit queue wait`, woken the instant a ticket
	// lands via its own fsnotify (internal/queue.Wait), so no external wake binary is
	// needed in the common case. Set it only for the not-currently-waiting case, if you
	// have a real out-of-band notifier; {agent}/{msg} are substituted into the command's
	// fields before exec. `leadkit doctor` warns (never errors) if a set command isn't
	// resolvable on PATH. config.Defaults() leaves it empty.
	WakeCommand string `yaml:"wake_command,omitempty"`

	GoalFile     string `yaml:"goal_file"`
	HandoverFile string `yaml:"handover_file"`

	// MilestonesFile is the repo-relative path (like goal_file/handover_file) to the
	// machine-readable milestone + DoD anchor internal/milestone reads — the queryable
	// `current_milestone:` + `dod:` artifact the per-tick re-anchor (`leadkit milestone
	// status`) and the idle-gate (`leadkit milestone check`) evaluate against. DEFAULT
	// ".leadkit/milestones.yaml" — DISTINCT from the taxonomy's <store_root>/
	// milestones.yaml (project/milestones/epics tree); this one is the DoD/idle-gate
	// anchor. A missing file degrades cleanly ("no milestone DoD declared"), never a
	// crash and never a false GO — see docs/PORTABLE-OPERATING-KIT.md.
	MilestonesFile string `yaml:"milestones_file"`

	// Milestone carries the idle-gate policy — see MilestoneGate.
	Milestone MilestoneGate `yaml:"milestone"`

	// OwnersFile is the repo-relative path (like goal_file/handover_file) to the
	// owners policy internal/hook's owner-guard enforces — a YAML file carrying
	// `hard_block:`, `paths:` and `clone_families:` (see internal/owners.ParsePolicy).
	// DEFAULT EMPTY: with owners_file unset (or the file it names absent) the
	// boundary system is DISABLED (owner-guard allows everything), exactly as the
	// source shell owner-guard.sh treats a missing owners.yaml — so existing adopters
	// are unaffected until they opt in by setting this. A conventional value is
	// ".leadkit/owners.yaml".
	OwnersFile string `yaml:"owners_file,omitempty"`

	Authority map[string]AuthoritySpec `yaml:"authority,omitempty"`

	// CanonicalStatuses is the sanctioned set of store-task frontmatter `status`
	// values — internal/audit's disk lens FLAGS deviations from this set, it never
	// normalizes or decides the vocabulary (that's a user call).
	CanonicalStatuses []string `yaml:"canonical_statuses,omitempty"`

	// SessionTasksRoot is where ~/.claude/tasks/session-<id>/*.json live —
	// internal/audit's session lens reads these to catch real work filed as an
	// ephemeral session task instead of a disk task (the "wrong mechanism" check).
	SessionTasksRoot string `yaml:"session_tasks_root"`

	// SessionTaskWorkish tunes that lens's "this session task carries real work"
	// heuristic.
	SessionTaskWorkish SessionTaskWorkish `yaml:"session_task_workish"`

	// Throttle is the usage-based pause-lock policy internal/lock and internal/burn
	// read (see internal/lock's package doc for the full protocol).
	Throttle Throttle `yaml:"throttle"`

	// PushWhitelist is the DEFAULT set of branch patterns internal/hook's push-guard
	// allows a raw `git push` to reach — `*` matches any sequence including "/"
	// (feat/* matches feat/x and feat/x/y). A repo's authority.<repo>.push_whitelist
	// REPLACES this entirely for that repo (full per-repo control, no merge
	// ambiguity) when the repo's origin remote resolves to an Authority entry.
	PushWhitelist []string `yaml:"push_whitelist,omitempty"`

	// SweepStatusFile is where each `leadkit queue sweep` run writes its
	// machine-readable findings artifact (docs/build-tasks/self-surfacing-
	// diagnostics.md, Part 2): per-lane stall counts, the broken-link list, a
	// timestamp, and the milestone re-anchor line. It's the SINGLE source every
	// self-surfacing reader consumes — doctor's daemon-diagnostics check, `leadkit
	// status`, and the board's /health all read this same file (mirroring how
	// push_violations_log feeds doctor + the board). Best-effort: a write failure
	// never fails the sweep. DEFAULT ".leadkit/sweep-status.json".
	SweepStatusFile string `yaml:"sweep_status_file"`

	// Diagnostics is the self-surfacing-diagnostics policy block — three independent
	// surfacing toggles + the self-repair mode the sweep honors. See the Diagnostics
	// type; every field defaults sensibly so an adopter that never sets the block
	// still gets surfacing on and self-repair in the safe "command" mode.
	Diagnostics Diagnostics `yaml:"diagnostics"`

	// Guards is the per-guard ENFORCEMENT-MODE policy (docs/build-tasks/
	// configurable-guard-enforcement.md): each security-relevant PreToolUse guard
	// (push-guard, push-gate, owner-guard) is `advisory` (today's fail-OPEN posture —
	// an error/unverifiable condition allows) or `enforce` (fail-CLOSED — an
	// unverifiable condition DENIES, authoritative). DEFAULT advisory
	// (backward-compatible: no behavior change until an adopter opts in), resolved from
	// the TARGET repo/file's governing profile. See the Guards type + EffectiveGuardMode.
	Guards Guards `yaml:"guards"`

	// PushViolationsLog is where push-guard appends one JSONL record per denied push
	// (whitelist-miss AND fail-closed-unverifiable) — a durable, user-visible trail so
	// a denied push isn't just a silent bounce to the agent. Best-effort: a write
	// failure here never blocks the deny itself. internal/viewer's /health reads this
	// same file to surface recent violations.
	PushViolationsLog string `yaml:"push_violations_log,omitempty"`

	// LaneAliases is the OPTIONAL per-project lane→role map `leadkit queue
	// consolidate` (and the LANE resolver, roles.CanonicalLane) consult LAST to map
	// a non-canonical queue-lane name onto a canonical role — for the strays the
	// built-in `-agent` lane-fold (devops-agent→devops, handled with zero config)
	// can't rescue. Each KEY is a raw lane name OR a folded family name (e.g.
	// "devops-agent-2" folds to "devops-agent"); each VALUE must be a KNOWN role (a
	// roles.FixedRole or a roster type) — a value naming an unknown role is a
	// dangling alias: it's REFUSED (never mints a phantom lane) and `leadkit doctor`
	// warns on it. An alias never overrides a canonical/`-agent`-fold match (it can't
	// redirect "code-review"). Absent/empty → today's behavior exactly (no aliasing).
	// See docs/build-tasks/role-keyed-lanes.md.
	LaneAliases map[string]string `yaml:"lane_aliases,omitempty"`

	// StatusAliases is the KNOWN status-alias map `leadkit audit --apply`'s
	// noncanonical category rewrites (source spelling -> canonical spelling), e.g.
	// done -> completed. Only an EXACT known alias is ever auto-rewritten; anything
	// else noncanonical (malformed prose, an unrecognised value) is reported, never
	// guessed at. See internal/audit.Apply.
	StatusAliases map[string]string `yaml:"status_aliases,omitempty"`

	// AuditApplyLog is where `leadkit audit --apply` appends one JSONL record per fix
	// it actually makes (dangling reaped / status normalized / done-drift relocated) —
	// every mutation this tool performs is traceable here.
	AuditApplyLog string `yaml:"audit_apply_log,omitempty"`

	// SpikesLog is where `leadkit spike record` appends a spike/dispatch run's
	// verified result — see internal/spikerec's own package doc.
	SpikesLog string `yaml:"spikes_log,omitempty"`

	// DecisionsLog is where `leadkit decision record` appends a HiL decision — see
	// internal/decisionrec's own package doc.
	DecisionsLog string `yaml:"decisions_log,omitempty"`

	// PreemptionLog is where `leadkit burn` appends one JSONL record every time
	// preemptive priority-ordered shedding actually reduces a session's live
	// count (session-priority-burn.md) — who was shed, to what, and why,
	// making self-declared-priority preemption auditable rather than silent.
	// Best-effort: a write failure here never blocks the advisory itself.
	PreemptionLog string `yaml:"preemption_log,omitempty"`

	// RoutingTTL is the retention window for the account-level routing store
	// (docs/build-tasks/routing-substrate.md): a TERMINAL routing item (one a Transition
	// marked closed) older than this is prune-eligible by `leadkit routing prune`; a
	// NON-terminal item is kept forever, regardless of age. The item's `history` (audit
	// trail) travels with it and is pruned together. A time.ParseDuration string (e.g.
	// "168h"), same convention as burn.workstream_ttl — parsed on demand via
	// RoutingTTLDur, falling back to DefaultRoutingTTL on empty/unparseable.
	RoutingTTL string `yaml:"routing_ttl"`

	// RegistryRepo is the local dir for the lead & session identity registry
	// (docs/build-tasks/lead-identity-registry.md, registry-local-default.md), holding one
	// file per session under sessions/<host>__<session_id>.json. LOCAL BY DEFAULT: register/
	// who read+write it directly (no git, no setup). Only when RegistryRemote is set (or the
	// dir is already a git repo with an origin) does it become a git repo whose origin is a
	// SHARED remote both machines clone, syncing the disjoint per-session files across
	// machines. Default ~/.claude/leadkit/registry.
	RegistryRepo string `yaml:"registry_repo"`

	// RegistryRemote is the OPTIONAL shared git remote for cross-machine registry sync. Empty
	// (default) = LOCAL-only: register/who work against RegistryRepo with no git, no warning.
	// When set, the registry syncs to it, and `leadkit registry init` clones/attaches it when
	// no --remote is passed on the command line.
	RegistryRemote string `yaml:"registry_remote,omitempty"`

	// RegistryTTL is how stale a registry session entry may get before `leadkit who` treats
	// it as STALE — omitted from the table by default, shown greyed with --all, and prunable
	// by `leadkit registry prune`. A session --touches periodically (heartbeat). A
	// time.ParseDuration string (e.g. "30m"), parsed on demand via RegistryTTLDur, falling
	// back to DefaultRegistryTTL on empty/unparseable.
	RegistryTTL string `yaml:"registry_ttl"`

	// Tests is `leadkit test run`'s per-repo override map — keyed the same way
	// Authority is (a repo's origin remote URL contains the key as a substring).
	// Both fields optional; absent means internal/testrun's own inference.
	Tests map[string]TestSpec `yaml:"tests,omitempty"`

	// Team is `leadkit team capture|status|install`'s config — see the Team type.
	Team Team `yaml:"team"`

	// --- Layered config (docs/PORTABLE-OPERATING-KIT.md) ---
	// See layers.go for the full user/team/project resolution engine. These
	// fields are populated from whichever layer wins per key; TeamConfig and
	// Locked are only ever meaningful when set BY the project/team layer
	// respectively (they steer resolution itself, see resolveLayers).

	// TeamConfig is a project-layer key: a path (resolved like any other,
	// Resolve()-relative to the project root) to the shared TEAM config file —
	// one team config can serve many projects. Empty = no team layer.
	TeamConfig string `yaml:"team_config,omitempty"`

	// Locked is a TEAM-layer key: the list of top-level config keys the team
	// MANDATES — a locked key's team value wins over the project's and the
	// user's, unconditionally (resolution step 1). Meaningless set anywhere but
	// the team config.
	Locked []string `yaml:"locked,omitempty"`

	// WorkspaceModel is a PREFERENCE key (resolved user > project > team, absent
	// a lock) — "worktrees" or "clones". leadkit only carries this value; the
	// agent defs/lead read it to decide how to work. Defaults to "clones".
	WorkspaceModel string `yaml:"workspace_model,omitempty"`

	// Outputs is a normal (project > team > user) key: the active `leadkit sync`
	// targets, e.g. ["linear"] — a team sets the standard, a project may
	// override, a team may `lock` it to mandate. `leadkit sync` reads the
	// merged value.
	Outputs []string `yaml:"outputs,omitempty"`

	// BriefLengthMax is a normal (project > team > user) team-policy key: the
	// brief-length guard's character limit. leadkit only carries the value here;
	// the guard hook that enforces it is a separate artifact, not yet wired to
	// read this.
	BriefLengthMax int `yaml:"brief_length_max,omitempty"`

	// MessagingModel is a normal (project > team > user) team-policy key:
	// "inbox" or "message" — which teammate-messaging shape the team uses.
	// leadkit only carries the value; nothing reads it yet.
	MessagingModel string `yaml:"messaging_model,omitempty"`

	// Linear is a normal (project > team > user) team-policy key, under the
	// merged Outputs: which Linear workspace `sync`'s executor agent targets —
	// see LinearConfig. IDENTIFIERS ONLY, never a token: leadkit's sync
	// commands hold no Linear credential; only the executor agent's own MCP
	// connection authenticates.
	Linear LinearConfig `yaml:"linear,omitempty"`

	// root is the directory Resolve() joins relative paths against: the discovered
	// leadkit.yaml's directory, or "" for a Defaults()-only profile (Resolve then
	// falls back to cwd). For an EXTERNAL config (loaded from ~/.claude/leadkit/config/
	// <slug>.yaml) this is the CONFIG FILE's own dir — so relative paths anchor there,
	// keeping queue/store out of the repo. Unexported — never part of the YAML.
	root string

	// externalRepoRoot, when non-empty, is the ACTUAL repo an EXTERNAL config was
	// resolved FOR (its git-toplevel/cwd) — what ProjectRoot() reports so gate own-origin
	// and identity derive from the real repo, NOT the external config file's dir (root).
	// Empty for an in-repo or Defaults() profile, where ProjectRoot() == root. Unexported,
	// never part of the YAML. This is the deliberate root/ProjectRoot divergence external
	// configs need (docs/build-tasks/external-config-resolution.md §2).
	externalRepoRoot string
}

// Hierarchy is the OPTIONAL, adopter-supplied hint tables `hierarchy propose` consults
// on top of its generic model-driven alias/title match. Source ships none — these are
// one project's curated seeds, so they live in leadkit.yaml, never in code. See
// internal/hierarchy.Propose (which takes a translated copy of these, so the hierarchy
// package stays free of a config import).
type Hierarchy struct {
	// TypeHints maps a canonical agent type to the epic id(s) its subtasks most
	// likely belong to. Only fires for an epic id that actually exists in the parsed
	// model (a hint naming an absent epic is skipped, never invented).
	TypeHints map[string][]string `yaml:"type_hints,omitempty"`
	// KeywordHints maps subject keywords to an epic id, matched case-insensitively as
	// a substring against a subtask's title.
	KeywordHints []KeywordHint `yaml:"keyword_hints,omitempty"`
}

// KeywordHint is one keyword->epic seed in Hierarchy.KeywordHints.
type KeywordHint struct {
	Keywords []string `yaml:"keywords"`
	Epic     string   `yaml:"epic"`
}

// ProjectRoot is the directory of the discovered leadkit.yaml this profile was loaded
// from — the project the profile governs — or "" for a Defaults()-only profile (no
// file on disk). It is what internal/gate keys its default gate-my-own-origin scope on
// (an empty root there means "unprofiled target, out of scope"). Read-only accessor for
// the unexported root; callers must not mutate it.
func (p *Profile) ProjectRoot() string {
	if p == nil {
		return ""
	}
	// An EXTERNAL config's project root is the REAL repo it was resolved for (threaded
	// through as externalRepoRoot), NOT its own file's dir (root, which Resolve anchors
	// relative paths to). For an in-repo/Defaults() profile the two coincide.
	if p.externalRepoRoot != "" {
		return p.externalRepoRoot
	}
	return p.root
}

// MilestoneGate is the `milestone.idle_gate_mode` policy (a normal project > team >
// user key) governing what `leadkit milestone check` does when the milestone DoD is
// unmet: "block" (exit non-zero — the hard gate an unattended run wants) or "warn"
// (report loudly, exit 0 — a supervised run can soften it). Resolution order for the
// effective mode is: a `--mode` flag on the check > this field > DefaultIdleGateMode.
// The SendMessage backstop stays warn-only regardless (see internal/hook).
type MilestoneGate struct {
	IdleGateMode string `yaml:"idle_gate_mode,omitempty"`

	// EnableScriptSignals is the OPT-IN safety gate for the `type: script` DoD signal —
	// a condition whose verdict is the exit status of a lead+user-authored bash command
	// (inline, or a path to an executable in the repo) run at the user's privileges.
	// A `type: script` condition EXECUTES arbitrary shell from the project's
	// milestones.yaml, at the SAME trust level as leadkit.yaml, a git hook, or a Makefile
	// target — the milestones.yaml + any referenced scripts are authored by the
	// agent-lead and agreed with the user. Default TRUE (a project that wrote a `script:`
	// condition wants it run); set FALSE to categorically forbid script execution for the
	// project, in which case every `type: script` condition reads unverified ("script
	// signals disabled") and the runner is NEVER called.
	EnableScriptSignals bool `yaml:"enable_script_signals"`

	// ScriptTimeout bounds every `type: script` run so a hung check can't wedge
	// `milestone check`/the sweep re-anchor. A time.ParseDuration string (e.g. "30s"),
	// same convention as burn.workstream_ttl — parsed on demand via ScriptTimeoutDur,
	// falling back to DefaultScriptTimeoutDur on empty/unparseable. A timeout reads
	// unverified (keeps the gate CLOSED, never a false met).
	ScriptTimeout string `yaml:"script_timeout"`
}

// ScriptTimeoutDur parses ScriptTimeout, falling back to DefaultScriptTimeoutDur on
// empty/unparseable — so the `type: script` runner always gets a positive bound.
func (m MilestoneGate) ScriptTimeoutDur() time.Duration {
	if m.ScriptTimeout == "" {
		return DefaultScriptTimeoutDur
	}
	if d, err := time.ParseDuration(m.ScriptTimeout); err == nil && d > 0 {
		return d
	}
	return DefaultScriptTimeoutDur
}

// Diagnostics is the self-surfacing daemon-diagnostics policy (docs/build-tasks/
// self-surfacing-diagnostics.md). The three surface_* toggles are INDEPENDENT — each
// governs one way the sweep's findings reach a lead-less adopter — and self_repair
// governs how aggressively the sweep may fix dangling links. Every field has a safe
// default (Defaults() below), so an adopter who never writes a `diagnostics:` block
// still gets doctor+board surfacing on, notifier off, and self-repair in the
// surface-and-name-the-fix "command" mode.
type Diagnostics struct {
	// SurfaceDoctor (default true): `leadkit doctor` + `leadkit status` read the
	// findings artifact and surface stalls/broken-links WITH the remediation.
	SurfaceDoctor bool `yaml:"surface_doctor"`
	// SurfaceBoard (default true): `leadkit serve` /health renders the findings
	// (extends the existing push-violations section, same read-the-artifact pattern).
	SurfaceBoard bool `yaml:"surface_board"`
	// SurfaceNotify (default false): when set AND NotifyCommand is non-empty, the
	// sweep execs the operator's notifier once findings cross NotifyThreshold.
	SurfaceNotify bool `yaml:"surface_notify"`
	// NotifyCommand is the operator's notifier, {…}-placeholder shape like push-wait's
	// wake_command ({stalled}/{broken}/{total}/{msg}). Empty (default) = no push, ever
	// — never a hard dependency.
	NotifyCommand string `yaml:"notify_command"`
	// NotifyThreshold is the stalls+broken count at/above which the notifier fires.
	// <= 0 is normalized to 1 (any finding fires). Default 1.
	NotifyThreshold int `yaml:"notify_threshold"`
	// SelfRepair is the sweep's dangling-link repair mode: "report" (surface only,
	// never mutate), "command" (RECOMMENDED default — surface + name `leadkit queue
	// repair` as the fix), or "auto" (the sweep reaps dangling links itself each run).
	// `leadkit queue repair` is always available regardless of this mode.
	SelfRepair string `yaml:"self_repair"`
}

// Diagnostics.self_repair mode values.
const (
	SelfRepairReport  = "report"
	SelfRepairCommand = "command"
	SelfRepairAuto    = "auto"
)

// DefaultSweepStatusFile is sweep_status_file's default — the findings artifact path
// when no leadkit.yaml overrides it, same .leadkit/ convention as the other artifacts.
const DefaultSweepStatusFile = ".leadkit/sweep-status.json"

// DefaultDiagnostics is the diagnostics block's default: both PULL/RENDER surfaces on,
// the active-notifier PUSH off (opt-in), and self-repair in the safe surface-and-name
// "command" mode. DefaultNotifyThreshold is notify_threshold's default.
const DefaultNotifyThreshold = 1

func DefaultDiagnostics() Diagnostics {
	return Diagnostics{
		SurfaceDoctor:   true,
		SurfaceBoard:    true,
		SurfaceNotify:   false,
		NotifyCommand:   "",
		NotifyThreshold: DefaultNotifyThreshold,
		SelfRepair:      SelfRepairCommand,
	}
}

// Guard-mode values + the guard NAMES the enforcement config is keyed by. The names
// match the guard's config sub-key (guards.<name>) and are the argument
// EffectiveGuardMode dispatches on — exported so every resolver (cmd/leadkit's per-guard
// mode wiring, doctor's posture check) shares ONE spelling and can never disagree.
const (
	GuardModeAdvisory = "advisory"
	GuardModeEnforce  = "enforce"

	GuardPushGuard  = "push_guard"
	GuardPushGate   = "push_gate"
	GuardOwnerGuard = "owner_guard"
)

// SecurityGuards is the ordered set of security-relevant guards the enforcement mode
// governs — the ones doctor's guard-enforcement posture check reports on, and the ones
// an adopter most likely wants enforcing. Extensible: add a guard here + a GuardModeSpec
// field on Guards + its resolver wiring.
var SecurityGuards = []string{GuardPushGuard, GuardPushGate, GuardOwnerGuard}

// GuardModeSpec is one guard's enforcement override — just its mode (advisory|enforce).
// A struct (not a bare string) so the config reads `guards.push_guard.mode: enforce` and
// the block stays extensible (a future per-guard knob is a new field, no schema churn).
type GuardModeSpec struct {
	// Mode is this guard's enforcement posture: "advisory" (fail-open, today) or
	// "enforce" (fail-closed). Empty/unset falls back to guards.default_mode, then to
	// advisory — so an unconfigured guard is exactly today's behavior.
	Mode string `yaml:"mode,omitempty"`
}

// Guards is the per-guard enforcement-mode policy block. DefaultMode is the fallback for
// any guard whose own mode is unset; each named guard may override it. Everything defaults
// to advisory (DefaultMode ""/advisory, per-guard modes ""), so a profile that never
// writes a `guards:` block keeps today's fail-open posture unchanged — enforcement is
// strictly opt-in, one line to turn on (`guards: {default_mode: enforce}` flips all three).
type Guards struct {
	// DefaultMode is the fallback enforcement mode for every guard whose own mode is
	// unset. Empty means advisory (backward-compatible). Set "enforce" to make every
	// security guard fail-CLOSED by default.
	DefaultMode string `yaml:"default_mode,omitempty"`

	PushGuard  GuardModeSpec `yaml:"push_guard,omitempty"`
	PushGate   GuardModeSpec `yaml:"push_gate,omitempty"`
	OwnerGuard GuardModeSpec `yaml:"owner_guard,omitempty"`
}

// modeSpec returns the GuardModeSpec for a named guard (one of the Guard* constants),
// and false for an unknown name (which then resolves to default_mode → advisory).
func (g Guards) modeSpec(guard string) (GuardModeSpec, bool) {
	switch guard {
	case GuardPushGuard:
		return g.PushGuard, true
	case GuardPushGate:
		return g.PushGate, true
	case GuardOwnerGuard:
		return g.OwnerGuard, true
	default:
		return GuardModeSpec{}, false
	}
}

// EffectiveGuardMode resolves the enforcement mode that GOVERNS a named guard on THIS
// profile: the guard's own guards.<name>.mode if set, else guards.default_mode, else
// advisory. Anything that isn't exactly "enforce" normalizes to advisory — an unset,
// empty, or unrecognized value is the SAFE, backward-compatible default (fail-open),
// never a silent enforce. This is the single resolver every call site (the per-target
// mode wiring in cmd/leadkit, doctor's posture report) shares, so the mode a guard acts
// on and the mode doctor reports can never diverge. A nil profile → advisory.
func (p *Profile) EffectiveGuardMode(guard string) string {
	if p == nil {
		return GuardModeAdvisory
	}
	mode := ""
	if spec, ok := p.Guards.modeSpec(guard); ok {
		mode = spec.Mode
	}
	if mode == "" {
		mode = p.Guards.DefaultMode
	}
	if mode == GuardModeEnforce {
		return GuardModeEnforce
	}
	return GuardModeAdvisory
}

// SessionTaskWorkish is the session-task "looks like real work, not gate
// coordination" heuristic's tunables: over the length threshold, OR the description
// contains one of Markers (matched case-insensitively).
type SessionTaskWorkish struct {
	DescriptionLengthThreshold int      `yaml:"description_length_threshold"`
	Markers                    []string `yaml:"markers,omitempty"`
}

// Throttle is the pause-lock policy: burn locks the team when session usage reaches
// PausePct, and clears the lock once it drops below ResumePct — the gap between the
// two (hysteresis) is what stops the lock flapping on/off right at one threshold.
type Throttle struct {
	LockFile  string  `yaml:"lock_file"`
	PausePct  float64 `yaml:"pause_pct"`
	ResumePct float64 `yaml:"resume_pct"`
}

// Team is `leadkit team capture|status|install`'s config — where the live
// operating-model artifacts live (agent defs, protocol, SOP), and where the repo's
// versioned snapshot of them lives (TeamDir). All four are HOME/project-relative
// via Resolve(), same as every other profile path.
type Team struct {
	AgentsDir    string `yaml:"agents_dir"`
	ProtocolFile string `yaml:"protocol_file"`
	SopFile      string `yaml:"sop_file"`
	TeamDir      string `yaml:"team_dir"`

	// SkillsDir is the LIVE leadkit-adopt skill's directory (adoption-onboard
	// deliverable 3) — `leadkit init --apply` installs it here (from the
	// embedded internal/onboard.SkillMD), and `team capture` reads it the same
	// way it reads AgentsDir, versioning it into <team_dir>/skills/<name>/.
	// Defaults to ~/.claude/skills/leadkit-adopt — mirrored as a literal here
	// (not an import) to keep this package free of an internal/onboard
	// dependency; internal/onboard.SkillName is the same string, kept in sync
	// by convention/comment, not by import (see that package's doc comment for
	// why: onboard depends on doctor, doctor depends on config — an import back
	// from config to onboard would cycle).
	SkillsDir string `yaml:"skills_dir"`
}

// Defaults returns a Profile pointed at this project's existing built-in defaults —
// the same ones internal/queue, internal/gate, internal/telemetry, internal/usage
// already fall back to — so a project with no leadkit.yaml still gets a fully usable
// profile.
func Defaults() Profile {
	qr := queue.DefaultRoot()
	return Profile{
		QueueRoot:  qr,
		StoreRoot:  filepath.Join(filepath.Dir(qr), "tasks"),
		IssuesRoot: filepath.Join(filepath.Dir(qr), "issues"),
		RecordsDir: gate.DefaultRecordsDir(),
		RolesPath:  filepath.Join(qr, "ROLES"),

		// EnforcedRemotes ships EMPTY on purpose: the project's OWN origin is always
		// gated regardless of this list (see internal/gate.Gate.inScope /
		// ProjectRoot). A profile that must gate repos OTHER than its own origin
		// names them in enforced_remotes.
		EnforcedRemotes: nil,
		KeychainService: usage.KeychainService,
		TelemetryLog:    telemetry.DefaultLogPath,

		Burn: Burn{
			SessionReserve:          burn.DefaultReserve,
			OpRateWarn:              DefaultOpRateWarn,
			AgentRateWarn:           DefaultAgentRateWarn,
			WorkstreamTTL:           burn.DefaultWorkstreamTTL.String(),
			WorkstreamStaleWindow:   burn.DefaultWorkstreamStaleWindow.String(),
			EnforceInterval:         burn.DefaultEnforceInterval.String(),
			ProductivityStallWindow: burn.DefaultProductivityStallWindow.String(),
			InprogressStallWindow:   burn.DefaultInprogressStallWindow.String(),
		},

		GoalFile:     DefaultGoalFile,
		HandoverFile: DefaultHandoverFile,

		MilestonesFile: DefaultMilestonesFile,
		Milestone: MilestoneGate{
			IdleGateMode:        DefaultIdleGateMode,
			EnableScriptSignals: true,
			ScriptTimeout:       DefaultScriptTimeout,
		},

		CanonicalStatuses: append([]string(nil), DefaultCanonicalStatuses...),
		SessionTasksRoot:  filepath.Join(os.Getenv("HOME"), ".claude", "tasks"),
		SessionTaskWorkish: SessionTaskWorkish{
			DescriptionLengthThreshold: DefaultSessionTaskWorkishLength,
			Markers:                    append([]string(nil), DefaultSessionTaskWorkishMarkers...),
		},

		Throttle: Throttle{
			LockFile:  DefaultLockFile,
			PausePct:  DefaultPausePct,
			ResumePct: DefaultResumePct,
		},

		PushWhitelist:     append([]string(nil), DefaultPushWhitelist...),
		PushViolationsLog: DefaultPushViolationsLog,

		SweepStatusFile: DefaultSweepStatusFile,
		Diagnostics:     DefaultDiagnostics(),

		// Guards default to ADVISORY (fail-open, today's behavior) — enforcement is
		// opt-in, so an adopter's posture never changes until they set enforce.
		Guards: Guards{DefaultMode: GuardModeAdvisory},

		StatusAliases: copyStringMap(DefaultStatusAliases),
		AuditApplyLog: DefaultAuditApplyLog,
		SpikesLog:     DefaultSpikesLog,
		DecisionsLog:  DefaultDecisionsLog,
		PreemptionLog: DefaultPreemptionLog,
		RoutingTTL:    DefaultRoutingTTL.String(),

		RegistryRepo: DefaultRegistryRepo(),
		RegistryTTL:  DefaultRegistryTTL.String(),

		WorkspaceModel: DefaultWorkspaceModel,

		BriefLengthMax: DefaultBriefLengthMax,
		MessagingModel: DefaultMessagingModel,

		Linear: LinearConfig{
			ClosedState:  DefaultLinearClosedState,
			DefaultState: DefaultLinearDefaultState,
		},

		Team: Team{
			AgentsDir:    filepath.Join(os.Getenv("HOME"), ".claude", "agents"),
			ProtocolFile: filepath.Join(os.Getenv("HOME"), ".claude", "agents", "_team-protocol.md"),
			SopFile:      DefaultSopFile,
			TeamDir:      DefaultTeamDir,
			SkillsDir:    filepath.Join(os.Getenv("HOME"), ".claude", "skills", "leadkit-adopt"),
		},
	}
}

// DefaultSopFile/DefaultTeamDir are `leadkit team`'s defaults when no leadkit.yaml
// overrides them. SopFile climbs one level out of the leadkit/ subdirectory (like
// queue_root/store_root's own "../" defaults) to this project's hand-maintained SOP;
// TeamDir is inside the repo itself — that's the whole point of capture.
const (
	DefaultSopFile = "../AUTONOMOUS-TEAM-SOP.md"
	DefaultTeamDir = "team"
)

// copyStringMap returns a shallow copy of m — Defaults() must never hand out the
// package-level DefaultStatusAliases map itself (a caller mutating prof.StatusAliases
// would otherwise corrupt every future Defaults() call).
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// DefaultStatusAliases is `leadkit audit --apply`'s default known-alias map when no
// leadkit.yaml overrides it.
var DefaultStatusAliases = map[string]string{
	"done":            "completed",
	"awaiting_review": "in_review",
}

// DefaultAuditApplyLog is audit --apply's default change-log path when no
// leadkit.yaml overrides it — project-root-relative, like the other .leadkit/ logs.
const DefaultAuditApplyLog = ".leadkit/audit-apply.jsonl"

// DefaultSpikesLog/DefaultDecisionsLog are spike record's/decision record's default
// log paths when no leadkit.yaml overrides them — same .leadkit/ convention.
const (
	DefaultSpikesLog    = ".leadkit/spikes.jsonl"
	DefaultDecisionsLog = ".leadkit/decisions.jsonl"
)

// DefaultPreemptionLog is `leadkit burn`'s default preemptive-shed audit-log
// path when no leadkit.yaml overrides it — same .leadkit/ convention.
const DefaultPreemptionLog = ".leadkit/preemption.jsonl"

// DefaultRoutingTTL is routing_ttl's default retention window — 7 days. Long enough that
// a closed hand-off's audit trail stays inspectable for a working week, short enough that
// terminal items don't accumulate account-wide forever.
const DefaultRoutingTTL = 168 * time.Hour

// RoutingTTLDur parses RoutingTTL, falling back to DefaultRoutingTTL on empty/unparseable
// — so `leadkit routing prune` and doctor's routing-inbox check read the window the same
// way and can never disagree about it.
func (p *Profile) RoutingTTLDur() time.Duration {
	if p.RoutingTTL == "" {
		return DefaultRoutingTTL
	}
	if d, err := time.ParseDuration(p.RoutingTTL); err == nil && d > 0 {
		return d
	}
	return DefaultRoutingTTL
}

// DefaultRegistryTTL is registry_ttl's default liveness window — 30m. A session --touches
// its entry periodically; one not refreshed within this window reads as STALE (omitted from
// `leadkit who` by default, shown greyed with --all, prunable). Deliberately generous
// relative to burn.workstream_stale_window: the registry is durable identity/leadership,
// not a fast liveness probe.
const DefaultRegistryTTL = 30 * time.Minute

// DefaultRegistryRepo is registry_repo's default local path — the account-wide git-backed
// identity registry, co-located with the other ~/.claude/leadkit account state.
func DefaultRegistryRepo() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "leadkit", "registry")
}

// RegistryTTLDur parses RegistryTTL, falling back to DefaultRegistryTTL on empty/
// unparseable — so `leadkit who`, `leadkit registry prune` and doctor's lead-registry
// check all read the same staleness window and can never disagree about it.
func (p *Profile) RegistryTTLDur() time.Duration {
	if p.RegistryTTL == "" {
		return DefaultRegistryTTL
	}
	if d, err := time.ParseDuration(p.RegistryTTL); err == nil && d > 0 {
		return d
	}
	return DefaultRegistryTTL
}

// DefaultWorkspaceModel is workspace_model's default when no layer sets it.
const DefaultWorkspaceModel = "clones"

// DefaultBriefLengthMax/DefaultMessagingModel are brief_length_max's/
// messaging_model's defaults when no layer sets them.
const (
	DefaultBriefLengthMax = 1400
	DefaultMessagingModel = "inbox"
)

// DefaultLinearClosedState/DefaultLinearDefaultState are linear.closed_state's/
// linear.default_state's defaults when no layer sets them — common Linear
// workspace state names; a real workspace may use different ones.
const (
	DefaultLinearClosedState  = "Done"
	DefaultLinearDefaultState = "Todo"
)

// PreferenceKeys is the set of keys (top-level, or a dotted nested path like
// "burn.session_reserve") resolved user > project > team (absent a team lock)
// rather than the normal project > team > user order — see layers.go's
// resolveLayers. Explicit and small on purpose (spec: "start with
// workspace_model, editor, default_model") — editor/default_model aren't yet
// backed by a typed Profile field (nothing reads them), but are listed here so
// they resolve correctly + report correctly under `config show --show-origin`
// the moment a profile author starts using them.
//
// burn.session_reserve is the "burn splits across layers" case: it's the
// member's OWN account budget (personal, user-wins), unlike op_rate_warn/
// agent_rate_warn/the throttle pause/resume thresholds, which stay normal
// team-policy keys (project > team > user, no entry needed here — that's
// already the default for anything not listed).
var PreferenceKeys = map[string]bool{
	"workspace_model":      true,
	"editor":               true,
	"default_model":        true,
	"burn.session_reserve": true,
}

// DefaultPushWhitelist is push-guard's default whitelist when no leadkit.yaml (or no
// per-repo authority.<repo>.push_whitelist override) sets one.
var DefaultPushWhitelist = []string{"feat/*", "fix/*", "chore/*", "mg/*", "user/*"}

// MatchAuthorityKey is THE deterministic authority.<repo> ↔ origin resolver — the
// single matcher every authority-resolution call site (push-guard's RepoWhitelist,
// prcreate.ResolveAuthority) shares, so they can never diverge. Among all authority
// keys that are a substring of origin (the same loose `strings.Contains` semantics the
// kit has always used — this does NOT tighten the match, so no existing config breaks),
// it returns the LONGEST-key match, ties broken by lexical order (smaller key wins) for
// a stable, run-to-run-identical result. Rationale: a more-specific key
// (`acmecorp/shared-infra`) deterministically beats a shorter colliding one
// (`infra`), instead of a map-iteration-order coin flip. A SINGLE match is returned
// unchanged (no silent policy shift); a lone loose match still MATCHES (it is surfaced
// by doctor's collision warn, never dropped here).
//
// requirePushWhitelist filters to entries with a non-empty PushWhitelist (what
// push-guard's per-repo override resolution needs); false considers every authority
// entry (what pr create/merge authority resolution needs). ok=false (empty origin, nil
// map, or no matching key) means the caller's existing default-deny / fallback applies,
// exactly as before.
func MatchAuthorityKey(origin string, auth map[string]AuthoritySpec, requirePushWhitelist bool) (key string, spec AuthoritySpec, ok bool) {
	if origin == "" || len(auth) == 0 {
		return "", AuthoritySpec{}, false
	}
	for k, s := range auth {
		if k == "" || !strings.Contains(origin, k) {
			continue
		}
		if requirePushWhitelist && len(s.PushWhitelist) == 0 {
			continue
		}
		if !ok || len(k) > len(key) || (len(k) == len(key) && k < key) {
			key, spec, ok = k, s, true
		}
	}
	return key, spec, ok
}

// MatchingAuthorityKeys returns EVERY authority key that is a substring of origin,
// sorted (longest first, lexical tiebreak) — the same loose match MatchAuthorityKey
// resolves over, but enumerated rather than reduced to a single winner. It exists for
// doctor's collision scan, which must SEE all matches (ambiguity + loose-substring
// collisions) rather than only the one that wins. Reuses the shared substring
// convention so it can never disagree with what MatchAuthorityKey resolves.
func MatchingAuthorityKeys(origin string, auth map[string]AuthoritySpec) []string {
	if origin == "" || len(auth) == 0 {
		return nil
	}
	var keys []string
	for k := range auth {
		if k != "" && strings.Contains(origin, k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// AuthorityKeyBoundaryAligned reports whether key occurs in origin at least once at a
// path/host BOUNDARY — preceded by start-of-string, `/`, or `:`, AND followed by
// end-of-string, `/`, or `.`. A key that matches origin ONLY mid-token (e.g. `infra`
// inside `shared-infra`) is NOT boundary-aligned: that is the shape of an
// accidental collision, which doctor warns on. This never changes resolution — it only
// classifies an existing (loose) match for diagnostics.
func AuthorityKeyBoundaryAligned(origin, key string) bool {
	if key == "" || !strings.Contains(origin, key) {
		return false
	}
	for i := 0; i+len(key) <= len(origin); {
		idx := strings.Index(origin[i:], key)
		if idx < 0 {
			break
		}
		at := i + idx
		beforeOK := at == 0 || origin[at-1] == '/' || origin[at-1] == ':'
		end := at + len(key)
		afterOK := end == len(origin) || origin[end] == '/' || origin[end] == '.'
		if beforeOK && afterOK {
			return true
		}
		i = at + 1
	}
	return false
}

// EffectivePushWhitelist resolves the push_whitelist push-guard ACTUALLY enforces for a
// repo at origin: the matching authority.<repo>.push_whitelist override (via the shared
// MatchAuthorityKey, requiring a non-empty override — the exact filter buildPushGuardConfig
// uses) when one resolves, else the top-level default. fromKey is the authority key whose
// override applied, or "" when the default is in effect (no origin, or no override
// matches). Display-only; it computes what the guard decides, it does not decide.
func EffectivePushWhitelist(prof *Profile, origin string) (whitelist []string, fromKey string) {
	if prof == nil {
		return nil, ""
	}
	if origin != "" {
		if key, spec, ok := MatchAuthorityKey(origin, prof.Authority, true); ok {
			return spec.PushWhitelist, key
		}
	}
	return prof.PushWhitelist, ""
}

// FormatEffectivePushWhitelist renders the one-line, comment-prefixed EFFECTIVE
// push_whitelist note `config show` appends: the whitelist push-guard would enforce for
// THIS origin and WHY (which authority key overrode the default, or that the default
// applies). Degrades gracefully when no origin resolves. Display-only.
func FormatEffectivePushWhitelist(prof *Profile, origin string) string {
	whitelist, fromKey := EffectivePushWhitelist(prof, origin)
	joined := "[" + strings.Join(whitelist, ",") + "]"
	switch {
	case origin == "":
		return fmt.Sprintf("# effective push_whitelist: %s — default (no origin resolved)\n", joined)
	case fromKey != "":
		return fmt.Sprintf("# effective push_whitelist (origin %s): %s — from authority.%s.push_whitelist (overrides the default)\n", redact.Origin(origin), joined, fromKey)
	default:
		return fmt.Sprintf("# effective push_whitelist (origin %s): %s — default (no authority.<repo> override matches)\n", redact.Origin(origin), joined)
	}
}

// DefaultPushViolationsLog is push-guard's default violations-log path when no
// leadkit.yaml overrides it — project-root-relative, like goal_file/handover_file.
const DefaultPushViolationsLog = ".leadkit/push-violations.jsonl"

// DefaultLockFile, DefaultPausePct, DefaultResumePct are the pause-lock policy's
// defaults — see Throttle.
const (
	DefaultLockFile  = ".leadkit/PAUSE"
	DefaultPausePct  = 98.0
	DefaultResumePct = 90.0
)

// DefaultCanonicalStatuses is the sanctioned status vocabulary internal/audit flags
// deviations from when no leadkit.yaml overrides it.
var DefaultCanonicalStatuses = []string{
	"pending", "in_progress", "in_review", "awaiting_pr_review", "blocked",
	"needs_rework", "gated", "completed", "dropped", "superseded",
	"reference", "plan", "draft", "design",
}

// DefaultSessionTaskWorkishLength and DefaultSessionTaskWorkishMarkers are the
// session-task "looks like real work" heuristic's defaults.
const DefaultSessionTaskWorkishLength = 280

var DefaultSessionTaskWorkishMarkers = []string{"##", "```", "acceptance", "DoD"}

// Resolve joins relpath against the profile's project root — the directory the
// discovered leadkit.yaml lives in, or cwd when Load found no file — after expanding
// $ENV vars and a leading "~/" (home dir). That expansion is the schema's "or $ENV"
// carve-out for fields like records_dir that are genuinely machine/user-scoped
// (~/.claude/...) rather than project-relative; an already-absolute or empty relpath
// (after expansion) is returned unchanged.
func (p *Profile) Resolve(relpath string) string {
	if relpath == "" {
		return ""
	}
	expanded := os.ExpandEnv(relpath)
	if strings.HasPrefix(expanded, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			expanded = filepath.Join(home, expanded[len("~/"):])
		}
	}
	if filepath.IsAbs(expanded) {
		return expanded
	}
	root := p.root
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	return filepath.Join(root, expanded)
}

// ReviewRoles are the roster roles declared review-capable (review_layer: true), each once, in
// roster order: the layer names a project adds to the built-in code-review and critique.
func (p *Profile) ReviewRoles() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range p.Roster {
		if r.ReviewLayer && r.Type != "" && !seen[r.Type] {
			seen[r.Type] = true
			out = append(out, r.Type)
		}
	}
	return out
}

// Validate returns human-readable warnings for referenced paths that don't exist.
// Every field is optional, so nothing here is a hard failure — queue_root,
// store_root, records_dir, and telemetry_log are all created on demand by the
// commands that own them, so their absence isn't checked; only the files a profile
// author points at directly (ROLES, goal, handover) are.
func (p *Profile) Validate() []string {
	var warnings []string
	check := func(label, relpath string) {
		if relpath == "" {
			return
		}
		if _, err := os.Stat(p.Resolve(relpath)); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %s not found (resolved: %s)", label, relpath, p.Resolve(relpath)))
		}
	}
	check("roles_path", p.RolesPath)
	check("goal_file", p.GoalFile)
	check("handover_file", p.HandoverFile)

	// A required_gates entry that names no review layer is a gate nothing can satisfy; say what the
	// layers are rather than leaving it to surface as a refusal at the PR. finalise and orchestrator
	// are fixed lanes that never record a verdict, so a roster entry cannot make them layers.
	for _, r := range p.Roster {
		if r.ReviewLayer && (r.Type == "finalise" || r.Type == "orchestrator") {
			warnings = append(warnings, fmt.Sprintf("roster: %s cannot be a review_layer: it is a fixed lane that never records a verdict", r.Type))
		}
	}
	vocab := gate.Vocabulary(p.ReviewRoles())
	repos := make([]string, 0, len(p.Authority))
	for repo := range p.Authority {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if err := gate.ValidateChain(p.Authority[repo].RequiredGates, vocab); err != nil {
			warnings = append(warnings, fmt.Sprintf("authority.%s.required_gates: %v", repo, err))
		}
	}
	return warnings
}

// Load discovers and loads the effective profile: the PROJECT layer is
// $LEADKIT_PROFILE if set (an exact path), else walking up from cwd to the first
// dir containing leadkit.yaml — like git's .git discovery — merged with the USER
// layer (always consulted, independent of the project) and, if the project layer
// sets `team_config:`, the TEAM layer (docs/PORTABLE-OPERATING-KIT.md's layered-
// config model; see layers.go for the full per-key resolution). No project profile
// found is not an error: Defaults() (still merged with any user/team layers) is
// returned with an empty path, signalling "no project file on disk." Every
// existing caller keeps working unchanged when only a project leadkit.yaml (or
// nothing at all) exists — the user/team layers simply contribute nothing then.
func Load() (*Profile, string, error) {
	prof, path, _, err := load()
	return prof, path, err
}

// LoadWithOrigin is Load() plus a per-key origin map (which layer supplied each
// resolved key's effective value, and whether it's team-locked) — what `config
// show --show-origin` renders.
func LoadWithOrigin() (*Profile, string, map[string]KeyOrigin, error) {
	return load()
}

// load is Load()/LoadWithOrigin's shared implementation. Resolution order:
// $LEADKIT_PROFILE (resolveProjectPath) > in-repo leadkit.yaml walk-up
// (resolveProjectPath) > EXTERNAL ~/.claude/leadkit/config/<slug>.yaml (keyed by the
// cwd-honest slug) > Defaults(). The first two steps are UNCHANGED — the external step
// is tried ONLY when resolveProjectPath found nothing (no env, no in-repo walk-up hit),
// so the in-repo path wins for back-compat and external is purely additive.
func load() (*Profile, string, map[string]KeyOrigin, error) {
	path, err := resolveProjectPath()
	if err != nil {
		return nil, "", nil, err
	}
	if path == "" {
		// No $LEADKIT_PROFILE and no in-repo config up-tree — try an EXTERNAL per-project
		// config keyed by the cwd-honest slug, BEFORE falling to Defaults().
		if cwd, gerr := os.Getwd(); gerr == nil {
			if cand, repoRoot, ok := ExternalConfigPathForDir(cwd); ok {
				prof, origins, lerr := loadLayeredExternal(cand, repoRoot)
				if lerr != nil {
					return nil, cand, nil, lerr
				}
				return prof, cand, origins, nil
			}
		}
	}
	prof, origins, err := loadLayered(path)
	if err != nil {
		return nil, path, nil, err
	}
	// A committed config found by walking up (not one the operator named with $LEADKIT_PROFILE)
	// may describe the project; what it may GRANT is the operator's external config's to say.
	if os.Getenv("LEADKIT_PROFILE") == "" {
		if cwd, gerr := os.Getwd(); gerr == nil {
			if oerr := overlayOperatorGrants(prof, origins, cwd); oerr != nil {
				return nil, path, nil, oerr
			}
		}
	}
	return prof, path, origins, nil
}

// LoadFrom is Load() scoped to an explicit starting directory instead of the process
// cwd: it walks up from start to discover the project leadkit.yaml, then loads the
// fully-layered profile rooted at that project (so every Resolve() off the returned
// profile joins against the discovered project root, not cwd). Like DiscoverProjectRoot
// it never consults $LEADKIT_PROFILE — start IS the chosen scope. found is false when
// no project leadkit.yaml is on the path up from start (the returned *Profile is then
// nil and the caller should fall back to its cwd-relative profile); a discovered-but-
// unreadable/malformed profile returns found=true with a non-nil error.
func LoadFrom(start string) (prof *Profile, found bool, err error) {
	path, ok := discover(start)
	if !ok {
		// In-repo walk-up found nothing — try an EXTERNAL per-project config keyed by
		// start's own cwd-honest slug (NOT an ancestor, NOT env), matching Load()'s third
		// resolution step. found stays false (→ caller's Defaults() fallback) only when no
		// external config exists either.
		if cand, repoRoot, ext := ExternalConfigPathForDir(start); ext {
			p, _, lerr := loadLayeredExternal(cand, repoRoot)
			if lerr != nil {
				return nil, true, lerr
			}
			return p, true, nil
		}
		return nil, false, nil
	}
	p, origins, lerr := loadLayered(path)
	if lerr != nil {
		return nil, true, lerr
	}
	if oerr := overlayOperatorGrants(p, origins, start); oerr != nil {
		return nil, true, oerr
	}
	return p, true, nil
}

// LoadProjectFile reads EXACTLY the leadkit.yaml at path — no walk-up discovery, no
// $LEADKIT_PROFILE, no user/team layering — and returns its Profile with the project
// root set to the file's directory (so Resolve() joins against that root). It is the
// deliberate IDENTITY-path counterpart to Load()'s POLICY discovery: a session's
// project IDENTITY must read only its OWN on-disk config, never an inherited env
// profile or an ancestor leadkit.yaml above it (the config-less-collapse bug —
// docs/build-tasks/session-identity-cwd-honest.md). ok=false when no leadkit.yaml
// exists at path (the caller then falls back to a basename derivation); a present-but-
// unreadable or malformed file returns ok=true with a non-nil error.
func LoadProjectFile(path string) (prof *Profile, ok bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	p := Defaults()
	if uerr := yaml.Unmarshal(data, &p); uerr != nil {
		return nil, true, fmt.Errorf("config: parse %s: %w", path, uerr)
	}
	p.root = filepath.Dir(path)
	return &p, true, nil
}

// resolveProjectPath is Load()'s own project-layer discovery: $LEADKIT_PROFILE if
// set (an exact path — NOT existing there is a hard error, unlike walk-up
// discovery finding nothing), else the git-style walk-up; "" means no project
// profile found (not an error).
func resolveProjectPath() (string, error) {
	if p := os.Getenv("LEADKIT_PROFILE"); p != "" {
		return p, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("config: getwd: %w", err)
	}
	path, found := discover(cwd)
	if !found {
		return "", nil
	}
	return path, nil
}

// discover walks up from start looking for DefaultProfileName, stopping at the root of
// the git working tree containing start (that root's own file is still considered).
// Outside any working tree the walk is unbounded.
func discover(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	top, inRepo := repoRootOf(dir)
	for {
		cand := filepath.Join(dir, DefaultProfileName)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, true
		}
		if inRepo && dir == top {
			return "", false
		}
		parent, ok := realParent(dir)
		if !ok {
			return "", false
		}
		dir = parent
	}
}

// realParent is the directory above dir in the filesystem, not in dir's spelling: when
// dir is a symlink its parent is the target's parent. ok=false when dir is a symlink
// that cannot be resolved, so the walk stops there.
func realParent(dir string) (string, bool) {
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", false
		}
		dir = target
	}
	parent := filepath.Dir(dir)
	return parent, parent != dir
}

// canonicalDir resolves symlinks so a path can be compared with the real path git
// reports (on macOS /var is /private/var). An unresolvable path is compared as given.
func canonicalDir(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// repoRootOf returns the nearest ancestor of the absolute path dir (dir included) that
// is a git working-tree root, read from the filesystem without running git, in the same
// spelling discover walks.
func repoRootOf(dir string) (string, bool) {
	for {
		if isRepoRoot(dir) {
			return dir, true
		}
		parent, ok := realParent(dir)
		if !ok {
			return "", false
		}
		dir = parent
	}
}

// isRepoRoot reports whether dir holds a .git entry (a directory, or the file a linked
// worktree or submodule uses) or is itself a bare repository.
func isRepoRoot(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if filepath.Base(dir) == ".git" {
		return false
	}
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// ConfigSource classifies WHERE a resolved profile came from — for `whoami` / `config
// show` so an adopter can see which config governs and from where. kind is one of
// "defaults" (no file — built-in Defaults()), "in-repo" (a committed leadkit.yaml, the
// walk-up/$LEADKIT_PROFILE path) or "external" (~/.claude/leadkit/config/<slug>.yaml,
// keyed by the repo's cwd-honest slug). location is the resolved file path ("" for
// defaults). An external profile is recognised by its non-empty externalRepoRoot (the
// real repo it governs), which repoRoot returns alongside.
func ConfigSource(prof *Profile, path string) (kind, location, repoRoot string) {
	switch {
	case path == "":
		return "defaults", "", ""
	case prof != nil && prof.externalRepoRoot != "":
		return "external", path, prof.externalRepoRoot
	default:
		return "in-repo", path, ""
	}
}

// sourceHeader is FormatShow/FormatShowOrigin's shared `# ...` provenance line: a
// built-in-defaults note (no file), an external-config note naming BOTH the config path
// and the repo it governs, or — unchanged for back-compat — the bare `# <path>` for an
// in-repo config.
func sourceHeader(prof *Profile, path string) string {
	kind, location, repoRoot := ConfigSource(prof, path)
	switch kind {
	case "defaults":
		return "# no leadkit.yaml found — showing built-in defaults\n"
	case "external":
		return fmt.Sprintf("# external config: %s (governs repo %s — no in-repo leadkit.yaml)\n", location, repoRoot)
	default:
		return fmt.Sprintf("# %s\n", location)
	}
}

// marshalForShow is the YAML `config show` prints. A project_origin key pasted from
// `git remote get-url origin` can carry a credential, so it is shown as host/owner/repo.
func marshalForShow(prof *Profile) ([]byte, error) {
	shown := *prof
	if shown.ProjectOrigin != "" {
		shown.ProjectOrigin = RedactOrigin(shown.ProjectOrigin)
	}
	return yaml.Marshal(&shown)
}

// FormatShow renders prof for `config show`: the resolved profile as YAML, prefixed
// with where it was found (or a built-in-defaults note), followed by any Validate
// warnings as trailing comment lines.
func FormatShow(prof *Profile, path string) (string, error) {
	data, err := marshalForShow(prof)
	if err != nil {
		return "", fmt.Errorf("config: marshal profile: %w", err)
	}
	out := sourceHeader(prof, path)
	out += string(data)
	if warnings := prof.Validate(); len(warnings) > 0 {
		out += "\n# validation notes:\n"
		for _, w := range warnings {
			out += fmt.Sprintf("#   - %s\n", w)
		}
	}
	return out, nil
}

// FormatShowOrigin renders prof for `config show --show-origin`: the same
// resolved YAML FormatShow prints, but every TOP-LEVEL key line is annotated with
// which layer supplied its effective value (or "default", if none did) and
// whether the team locked it — git `--show-origin` style, so a new team member
// can see what's theirs to set vs team-mandated at a glance.
func FormatShowOrigin(prof *Profile, path string, origins map[string]KeyOrigin) (string, error) {
	data, err := marshalForShow(prof)
	if err != nil {
		return "", fmt.Errorf("config: marshal profile: %w", err)
	}
	var b strings.Builder
	b.WriteString(sourceHeader(prof, path))
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		b.WriteString(line)
		if key, ok := topLevelKey(line); ok {
			o, known := origins[key]
			layer := "default"
			if known {
				layer = o.Layer
			}
			fmt.Fprintf(&b, "  # layer=%s", layer)
			if known && o.Locked {
				b.WriteString(", locked")
			}
		}
		b.WriteByte('\n')
	}
	if warnings := prof.Validate(); len(warnings) > 0 {
		b.WriteString("\n# validation notes:\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "#   - %s\n", w)
		}
	}
	return b.String(), nil
}

// topLevelKey reports whether line is a top-level "key: ..." mapping line in
// FormatShowOrigin's marshaled YAML (no leading indent, not a list item or
// comment) and, if so, its key.
func topLevelKey(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '-' || line[0] == '#' {
		return "", false
	}
	key, _, ok := strings.Cut(line, ":")
	if !ok {
		return "", false
	}
	return key, true
}
