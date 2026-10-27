package config

// Config reference generation. This file is the field-metadata SOURCE OF TRUTH that
// backs `leadkit config reference` and the committed docs/CONFIG-REFERENCE.md.
//
// The anti-drift design: fieldMeta (below) attaches, per yaml path, the reference
// metadata the Go type alone can't carry — a one-line description and (for enum-like
// fields) the ALLOWED values. Reference() walks the Profile struct REFLECTIVELY and
// derives each field's yaml path, type and default (from Defaults()) automatically;
// the only hand-maintained part is fieldMeta. MissingMetadata() reports any yaml path
// the reflective walk found that has no fieldMeta entry — a test asserts it's empty,
// so ADDING a config field FORCES adding its reference entry (or CI fails). Regenerate
// the committed doc with `go generate ./internal/config` after any config change.

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

//go:generate go run ../../cmd/gen-config-reference

// mapKeyPlaceholder is the path segment used for a map-of-struct value's fields (e.g.
// authority.<repo>.can_push) and slicePlaceholder for a slice-of-struct's element
// fields (e.g. roster[].type). They are literal in the generated reference — an
// adopter substitutes the real repo/entry.
const (
	mapKeyPlaceholder = "<repo>"
	sliceMarker       = "[]"
)

// FieldMeta is the per-field reference metadata that the Go type can't express: a
// one-line description and, for enum-like fields, the allowed values. Keyed by yaml
// path in fieldMeta.
type FieldMeta struct {
	Desc string
	Enum []string
}

// ReferenceEntry is one fully-resolved config field in the reference: its yaml path,
// rendered type, default (from Defaults()), allowed values (enums), description, and
// the block it groups under.
type ReferenceEntry struct {
	Group   string
	Path    string
	Type    string
	Default string
	Enum    []string
	Desc    string
	// hasMeta is false when the reflective walk found no fieldMeta entry for Path —
	// MissingMetadata() surfaces these and the completeness test fails on any.
	hasMeta bool
}

// fieldMeta is the hand-maintained metadata table keyed by yaml path. EVERY
// yaml-tagged field reachable from Profile (including nested blocks and the fields of
// map/slice value structs like AuthoritySpec) MUST have an entry here — the
// reflective completeness test (TestReference_EveryFieldHasMetadata) fails otherwise.
// Descriptions are one line, reused/condensed from the struct doc comments and the
// leadkit.yaml sample. Enum lists the ALLOWED values for enum-like fields only.
var fieldMeta = map[string]FieldMeta{
	// --- top-level paths / roots ---
	"project_origin":      {Desc: "EXTERNAL configs only: the origin the repo this config governs must have, as `owner/repo` or `host/owner/repo`. An external config is found by the origin's repo NAME, so a same-named clone of someone else's repo would otherwise load it and take its absolute store_root and queue_root; when set, a repo whose origin does not match is refused. `leadkit config init --external` writes it."},
	"project_id":          {Desc: "Optional stable per-project id; when set it wins over the queue_root-derived slug for per-project daemon namespacing."},
	"queue_root":          {Desc: "Root of the on-disk work queue (per-role pending/ inboxes). Default is $LEADKIT_QUEUE if set, else a cwd-relative ./queue — no adopter path is baked in."},
	"store_root":          {Desc: "Root of the task store (tasks/*.md). Default is a sibling of queue_root."},
	"records_dir":         {Desc: "Where review-gate verdict records are written. May use ~/ or $ENV for machine/user-scoped paths."},
	"roles_path":          {Desc: "Path to the ROLES file enumerating the canonical roles. Default <queue_root>/ROLES."},
	"issues_root":         {Desc: "Taxonomy Issue level: a flat dir of issues/*.md sibling to store_root's tasks/ (Project->Milestone->Epic->Issue->Task)."},
	"enforced_remotes":    {Desc: "OPTIONAL origin-URL substrings of OTHER repos the review-gate also enforces on (a remote matches if its URL CONTAINS any listed value). The project's OWN origin is always gated, whatever this list holds, so a project gates its own pushes with zero config. Declare this ONLY to gate repos OTHER than the profile's own origin."},
	"pr_fallback_hint":    {Desc: "OPTIONAL adopter trailer the PR-guard appends to its raw-forge deny messages (e.g. a pointer to a legacy/local PR helper). No default; the deny message names the governed `leadkit pr ...` path generically and appends this only when set — so no adopter script path is ever baked into the source."},
	"keychain_service":    {Desc: "macOS keychain service name usage reads the Claude session token from."},
	"telemetry_log":       {Desc: "Path to the append-only telemetry JSONL log."},
	"wake_command":        {Desc: "Optional out-of-band notifier the waker daemon execs when a ticket lands ({agent}/{msg} substituted). Empty is the normal default (agents idle on event-driven queue wait)."},
	"goal_file":           {Desc: "Repo-relative path to the goal artifact. Namespaced under .leadkit/ to avoid clobbering a project's own root files."},
	"handover_file":       {Desc: "Repo-relative path to the session handover artifact."},
	"milestones_file":     {Desc: "Repo-relative path to the machine-readable milestone + DoD anchor the idle-gate evaluates. DISTINCT from the taxonomy's <store_root>/milestones.yaml; a missing file degrades cleanly, never a false GO."},
	"owners_file":         {Desc: "Repo-relative path to the owners boundary policy (hard_block/paths/clone_families). Empty (or absent file) DISABLES owner-guard entirely; a conventional value is .leadkit/owners.yaml."},
	"canonical_statuses":  {Desc: "Sanctioned set of store-task frontmatter status values; audit's disk lens FLAGS deviations, it never rewrites the vocabulary."},
	"session_tasks_root":  {Desc: "Where ~/.claude/tasks/session-<id>/*.json live; audit's session lens reads these to catch real work filed as an ephemeral session task."},
	"push_whitelist":      {Desc: "Default branch patterns a raw git push may reach (`*` spans `/`, so feat/* matches feat/x/y). A repo's authority.<repo>.push_whitelist REPLACES this for that repo; `leadkit config show` prints the effective per-origin result."},
	"sweep_status_file":   {Desc: "Where each `queue sweep` writes its machine-readable findings artifact (per-lane stalls, broken links, milestone re-anchor). The single source doctor/status/board all read. Default .leadkit/sweep-status.json."},
	"push_violations_log": {Desc: "JSONL trail of denied pushes (whitelist-miss and fail-closed-unverifiable). Best-effort; the board's /health surfaces recent entries."},
	"lane_aliases":        {Desc: "Optional lane-name -> canonical-role map `queue consolidate` (and the lane resolver) consult LAST, for strays the built-in `-agent` lane-fold (devops-agent->devops) can't rescue. Each value MUST be a known role (FixedRole or roster type); a dangling target is refused (no phantom lane) and doctor warns. Never overrides a canonical match."},
	"status_aliases":      {Desc: "Known status-alias map (source spelling -> canonical) `audit --apply` auto-rewrites; only EXACT known aliases are rewritten, anything else is reported."},
	"audit_apply_log":     {Desc: "JSONL log of every fix `audit --apply` makes (dangling reaped / status normalized / done-drift relocated)."},
	"spikes_log":          {Desc: "JSONL log `spike record` appends a verified spike/dispatch result to."},
	"decisions_log":       {Desc: "JSONL log `decision record` appends a human-in-the-loop decision to."},
	"preemption_log":      {Desc: "JSONL log `burn` appends a record to each time priority-ordered preemptive shedding reduces a session's live count. Best-effort."},
	"routing_ttl":         {Desc: "Retention window for the account-level routing store: a TERMINAL routing item (a Transition marked it closed) older than this is prune-eligible by `leadkit routing prune`; a NON-terminal item is kept forever regardless of age. Its history travels with it and is pruned together. A time.ParseDuration string (e.g. \"168h\")."},
	"registry_repo":       {Desc: "Local dir for the lead & session identity registry, holding one file per session under sessions/<host>__<session_id>.json. LOCAL BY DEFAULT: `leadkit register`/`who` read+write it directly (atomic temp+rename, no git, no setup) — complete and non-degraded with no remote. Only when registry_remote is set (or the dir is already a git repo with an origin) does it become a git repo that best-effort syncs the disjoint per-session files across machines on each call."},
	"registry_remote":     {Desc: "OPTIONAL shared git remote for CROSS-MACHINE registry sync. Empty (the default) = LOCAL-only: register/who work against the local registry_repo dir with no git and no warning. When set, the registry syncs (commit + pull-rebase + push) to this remote, and `leadkit registry init` clones/attaches it when no --remote is passed."},
	"registry_ttl":        {Desc: "How stale a registry session entry may get before `leadkit who` treats it as STALE — omitted from the table by default, shown greyed with --all, and prunable by `leadkit registry prune`. A session --touches periodically. A time.ParseDuration string (e.g. \"30m\")."},
	"team_config":         {Desc: "Project-layer key: path to the shared TEAM config file (one team config serves many projects). Empty means no team layer."},
	"locked":              {Desc: "TEAM-layer-only key: top-level keys the team MANDATES — a locked key's team value wins over project and user unconditionally."},
	"workspace_model":     {Desc: "Preference key (user>project>team): how agents work the repo.", Enum: []string{"worktrees", "clones"}},
	"outputs":             {Desc: "Active `leadkit sync` targets (e.g. [\"linear\"]); a team sets the standard, a project may override, a team may lock it."},
	"brief_length_max":    {Desc: "Team-policy character limit for the brief-length guard. leadkit carries the value; the enforcing hook is separate."},
	"messaging_model":     {Desc: "Team-policy key: which teammate-messaging shape the team uses.", Enum: []string{"inbox", "message"}},

	// --- burn ---
	"burn.session_reserve":           {Desc: "Percent of the weekly budget held untouched for the user's own use (personal, user-wins preference key burn.session_reserve)."},
	"burn.op_rate_warn":              {Desc: "Advisory warn threshold for operations/hour."},
	"burn.agent_rate_warn":           {Desc: "Advisory warn threshold for agent spawns/hour."},
	"burn.workstream_ttl":            {Desc: "How stale a project's account-wide workstream-registry entry may get before it's pruned from account burn totals. A time.ParseDuration string (e.g. \"30m\")."},
	"burn.workstream_stale_window":   {Desc: "How stale a workstream/presence registry entry may get before the burn GOVERNOR ignores it when computing account_n (tighter than workstream_ttl, so a crashed/exited session stops inflating the count promptly). The auto-register presence heartbeat refreshes on every session activity, so a live session never falls past it. A time.ParseDuration string (e.g. \"15m\")."},
	"burn.enforce_interval":          {Desc: "How often the account-level burn-enforce daemon (dev.leadkit.burn) reads real account weekly usage and writes/clears the account pause lock from the pause_pct/resume_pct hysteresis — so the throttle fires without any session running `leadkit burn`. `leadkit init` translates it into the daemon's StartInterval. A time.ParseDuration string (e.g. \"2m\")."},
	"burn.productivity_stall_window": {Desc: "How long an agent may be ACTIVE (alive + churning) with NO deliverable — no new commit, gate verdict, queue advancement, or session-task→done — before the productivity watcher flags a productive-stall (\"busy is not productive\"). A time.ParseDuration string (e.g. \"3h\")."},
	"burn.inprogress_stall_window":   {Desc: "How long a ticket may sit CLAIMED in an in-progress lane (its symlink mtime) with NO deliverable advancement — no new commit, gate verdict, or queue stage-move — before the frozen-claim detector flags it as a hung owner's frozen claim (probe the owner; a live one answers, a hung one is silent). A time.ParseDuration string (e.g. \"45m\")."},
	"burn.session_priority":          {Desc: "This session's self-declared priority tier in the account-wide session registry; governs preemptive shedding order. $LEADKIT_SESSION_PRIORITY overrides. Empty means normal.", Enum: []string{"critical", "high", "normal", "low"}},

	// --- roster (slice of RosterSpec) ---
	"roster.type":         {Desc: "Canonical role name for this reviewer/agent-pool entry."},
	"roster.count":        {Desc: "How many of this role should be live."},
	"roster.review_layer": {Desc: "Declares this role review-capable: its name becomes a review layer, a valid `gate record --layer` value and a valid authority.<repo>.required_gates entry. code-review and critique are review layers in every project and need no entry. finalise and orchestrator cannot be layers. A layer's recorder must be of a different role family than every other layer's."},
	"roster.lane":         {Desc: "Optional explicit live queue-lane override so more than one role can share one consumer lane. Unset defaults the lane to the role name. (code-review + critique may never share a lane.)"},

	// --- heartbeat ---
	"heartbeat.schedule":   {Desc: "Cron expression the idle heartbeat re-arms on."},
	"heartbeat.prompt_ref": {Desc: "What to run on each heartbeat tick."},

	// --- milestone (idle-gate) ---
	"milestone.idle_gate_mode":        {Desc: "What `milestone check` does on an unmet DoD: block (exit non-zero, the hard gate) or warn (report loudly, exit 0). A --mode flag overrides. The SendMessage backstop stays warn-only regardless.", Enum: []string{"block", "warn"}},
	"milestone.enable_script_signals": {Desc: "Opt-in safety gate for the `type: script` DoD signal, which EXECUTES arbitrary shell (a lead+user-authored bash command or in-repo script) at the user's privileges — same trust level as leadkit.yaml, a git hook, or a Makefile target. Default true (a project that wrote a script condition wants it run); set false to categorically forbid script execution, making every `type: script` condition read unverified (\"script signals disabled\") without ever calling the runner."},
	"milestone.script_timeout":        {Desc: "How long a `type: script` DoD signal may run before it is killed and read unverified (never a false met) — bounds every run so a hung check can't wedge `milestone check`/the sweep. A time.ParseDuration string (e.g. \"30s\")."},

	// --- authority.<repo> (map value: AuthoritySpec) ---
	"authority.<repo>.can_push":           {Desc: "Whether an agent may push in this repo unsupervised."},
	"authority.<repo>.can_open_pr":        {Desc: "Whether an agent may open a PR: true/false, or \"human\" meaning it always needs a human regardless of gate state.", Enum: []string{"true", "false", "human"}},
	"authority.<repo>.pr_draft_only":      {Desc: "When true, PRs in this repo may only be opened as draft."},
	"authority.<repo>.can_merge":          {Desc: "Whether an agent may merge: true/false, or \"human\" (always needs a human).", Enum: []string{"true", "false", "human"}},
	"authority.<repo>.merge_grant":        {Desc: "A standing-grant id (incident/ticket ref) that lets the lead merge WITHOUT per-merge human approval while set. Empty means merge stays fully human regardless of can_merge."},
	"authority.<repo>.required_gates":     {Desc: "Ordered review layers that must all be ready before this repo's PR may advance; list order is chain order, so a layer can be recorded ready only after every layer before it is. Each entry must be code-review, critique, or a roster role declared review_layer: true; anything else is reported by config validation and doctor, and no record can satisfy it. Empty means [code-review, critique]. A repo's push gate uses the default pair."},
	"authority.<repo>.push_whitelist":     {Desc: "When set, REPLACES the top-level push_whitelist entirely for this repo (no merge). The repo is matched by origin-URL substring; when several keys match, the longest (most-specific) key wins."},
	"authority.<repo>.raw_pr_verbs":       {Desc: "Additive allow-list of canonical PR verbs a raw forge CLI (gh/glab) may invoke without a wrapper. \"merge\" is guard-railed: always ignored/denied.", Enum: []string{"create", "edit"}},
	"authority.<repo>.owner_prs_only":     {Desc: "When true, a granted raw `edit` is restricted to PRs the authenticated forge user authored; a failed author lookup fails CLOSED. Never restricts create or merge."},
	"authority.<repo>.forge":              {Desc: "Optional per-repo forge override for pr create / the pr-guard. Empty picks the forge from the origin host heuristic; set only for a self-hosted host that names neither.", Enum: []string{"github", "gitlab"}},
	"authority.<repo>.protected_branches": {Desc: "Branches `leadkit forge protect` configures and `leadkit doctor` verifies server-side. Exact names only, no globs. Empty means the repo's default branch as the forge reports it."},

	// --- session_task_workish ---
	"session_task_workish.description_length_threshold": {Desc: "A session task longer than this counts as \"real work\" for the wrong-mechanism audit lens."},
	"session_task_workish.markers":                      {Desc: "Substrings (matched case-insensitively) that mark a session task as real work regardless of length."},

	// --- throttle (pause-lock) ---
	"throttle.lock_file":  {Desc: "Path to the pause-lock sentinel burn writes when usage crosses pause_pct."},
	"throttle.pause_pct":  {Desc: "Session-usage percent at which burn LOCKS the team."},
	"throttle.resume_pct": {Desc: "Session-usage percent the lock CLEARS below; the gap to pause_pct is the hysteresis that stops the lock flapping."},

	// --- diagnostics (self-surfacing) ---
	"diagnostics.surface_doctor":   {Desc: "When true, `doctor` + `status` read the findings artifact and surface stalls/broken-links WITH the remediation."},
	"diagnostics.surface_board":    {Desc: "When true, `serve` /health renders the findings (extends the push-violations section)."},
	"diagnostics.surface_notify":   {Desc: "When true AND notify_command is set, the sweep execs the operator's notifier once findings cross notify_threshold."},
	"diagnostics.notify_command":   {Desc: "Operator's notifier, {stalled}/{broken}/{total}/{msg}-placeholder shape. Empty means no push, ever."},
	"diagnostics.notify_threshold": {Desc: "Stalls+broken count at/above which the notifier fires. <=0 is normalized to 1 (any finding fires)."},
	"diagnostics.self_repair":      {Desc: "Sweep's dangling-link repair mode: report (surface only), command (recommended: surface + name `queue repair`), or auto (reap dangling links each run). `queue repair` stays available regardless.", Enum: []string{"report", "command", "auto"}},

	// --- guards (per-guard enforcement mode: advisory fail-open | enforce fail-closed) ---
	"guards.default_mode":     {Desc: "Fallback enforcement mode for every security guard whose own mode is unset. advisory (default) = today's fail-OPEN posture (an error/unverifiable condition ALLOWS); enforce = fail-CLOSED (an unverifiable condition DENIES, authoritative). Resolved from the TARGET repo/file's governing profile. Set enforce for real blocking.", Enum: []string{"advisory", "enforce"}},
	"guards.push_guard.mode":  {Desc: "push-guard's enforcement mode; overrides guards.default_mode. advisory = today; enforce = an unparseable/unverifiable push target is DENIED (push-guard already fail-closes on its unverifiable paths — enforce makes the posture explicit and names it in the deny). Empty inherits default_mode → advisory.", Enum: []string{"advisory", "enforce"}},
	"guards.push_gate.mode":   {Desc: "push-gate's enforcement mode; overrides guards.default_mode. advisory = today (a bad repo path / unresolvable HEAD / unresolvable-scope push fails OPEN); enforce = those unverifiable pushes are DENIED (can't confirm the sha is gated → block). A clear gated allow and a clear ungated deny are unchanged in both. Empty inherits default_mode → advisory.", Enum: []string{"advisory", "enforce"}},
	"guards.owner_guard.mode": {Desc: "owner-guard's enforcement mode; overrides guards.default_mode. advisory = today (an owners_file that is configured but unreadable/unparseable fails OPEN); enforce = a configured-but-unreadable owners policy DENIES (can't confirm ownership → block). A project that defines no owners_file has no boundary and is unaffected in both. Empty inherits default_mode → advisory.", Enum: []string{"advisory", "enforce"}},

	// --- tests.<repo> (map value: TestSpec) ---
	"tests.<repo>.test_command":     {Desc: "When set, REPLACES the inferred suite command entirely (run verbatim via a shell)."},
	"tests.<repo>.affected_command": {Desc: "When set, replaces the inferred affected-tests command (used only with `test run --affected`)."},

	// --- team ---
	"team.agents_dir":    {Desc: "Where the LIVE agent defs live; `team capture` versions them into <team_dir>."},
	"team.protocol_file": {Desc: "Path to the live team-protocol file."},
	"team.sop_file":      {Desc: "Path to the project's hand-maintained SOP (default climbs one level out of the leadkit/ subdir)."},
	"team.team_dir":      {Desc: "In-repo dir `team capture` versions the operating-model snapshot into."},
	"team.skills_dir":    {Desc: "Where `init --apply` installs the live leadkit-adopt skill; `team capture` versions it too. Default ~/.claude/skills/leadkit-adopt."},

	// --- linear (sync target — IDENTIFIERS only, never a token) ---
	"linear.team_id":         {Desc: "Target Linear team key (e.g. \"CI\"). Required when outputs includes \"linear\"; doctor warns if unset."},
	"linear.root_initiative": {Desc: "Linear Initiative id the local Project maps to; created + recorded on first run when unset."},
	"linear.closed_state":    {Desc: "Linear workspace state NAME used for a close op (workspace-specific)."},
	"linear.sync_mode":       {Desc: "Force the Linear execution mode: mcp (sync-executor agent) or direct (GraphQL API, key from env LINEAR_API_KEY or the keychain). Unset = direct iff LINEAR_API_KEY is in the env, else mcp; a `sync run --mode` flag overrides.", Enum: []string{"mcp", "direct"}},
	"linear.default_state":   {Desc: "Linear workspace state NAME used for a create op (workspace-specific)."},

	// --- hierarchy (hierarchy propose hint tables — adopter-supplied, empty by default) ---
	"hierarchy.type_hints":             {Desc: "Optional map of canonical agent type -> the epic id(s) its subtasks most likely belong to. Source ships none; only fires for an epic id present in the parsed model. `hierarchy propose`'s generic alias/title match runs regardless."},
	"hierarchy.keyword_hints.keywords": {Desc: "Title keywords (matched case-insensitively as substrings) for this keyword->epic seed."},
	"hierarchy.keyword_hints.epic":     {Desc: "The epic id a matching keyword proposes (only fires when that epic id exists in the model)."},
}

// Reference walks the Profile struct reflectively and returns one entry per
// yaml-tagged leaf field, in stable declaration order, with type + default derived
// automatically and description/enum looked up from fieldMeta. Group is the top-level
// block name ("burn", "authority", …) or topLevelGroup for a top-level scalar.
func Reference() []ReferenceEntry {
	def := Defaults()
	var out []ReferenceEntry
	walkReference(reflect.TypeOf(Profile{}), reflect.ValueOf(def), "", "", &out)
	return out
}

// topLevelGroup is the group label for a Profile field that is itself a leaf (not
// inside a named block).
const topLevelGroup = "(top-level)"

func walkReference(t reflect.Type, v reflect.Value, prefix, group string, out *[]ReferenceEntry) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported (e.g. Profile.root)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		var fv reflect.Value
		if v.IsValid() && v.Kind() == reflect.Struct {
			fv = v.Field(i)
		}

		ft := derefType(f.Type)
		switch {
		case isNestedStruct(ft):
			walkReference(ft, derefValue(fv), path, blockGroup(prefix, name, group), out)
		case ft.Kind() == reflect.Map && isNestedStruct(derefType(ft.Elem())):
			et := derefType(ft.Elem())
			walkReference(et, reflect.Zero(et), path+"."+mapKeyPlaceholder, blockGroup(prefix, name, group), out)
		case ft.Kind() == reflect.Slice && isNestedStruct(derefType(ft.Elem())):
			et := derefType(ft.Elem())
			walkReference(et, reflect.Zero(et), path+sliceMarker, blockGroup(prefix, name, group), out)
		default:
			g := group
			if prefix == "" {
				g = topLevelGroup
			}
			meta, ok := fieldMeta[metaKey(path)]
			*out = append(*out, ReferenceEntry{
				Group:   g,
				Path:    path,
				Type:    typeString(f.Type),
				Default: defaultString(fv, f.Type),
				Enum:    meta.Enum,
				Desc:    meta.Desc,
				hasMeta: ok,
			})
		}
	}
}

// metaKey normalizes a walked path to its fieldMeta key. Slice-of-struct element
// paths use the "roster[].type" walk form but a "roster.type" table key, so the [] is
// stripped for the lookup — keeping the table readable while the reference still
// renders the explicit [] marker.
func metaKey(path string) string {
	return strings.ReplaceAll(path, sliceMarker, "")
}

// blockGroup is the group label for a descendant: the block's own name when we're
// descending from a top-level field, else the inherited group.
func blockGroup(prefix, name, group string) string {
	if prefix == "" {
		return name
	}
	return group
}

func isNestedStruct(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t != reflect.TypeOf(BoolOrHuman{})
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	return v
}

// typeString renders a field's Go type as a compact reference type string.
func typeString(t reflect.Type) string {
	t = derefType(t)
	if t == reflect.TypeOf(BoolOrHuman{}) {
		return "bool|human"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice:
		return "[]" + typeString(t.Elem())
	case reflect.Map:
		return "map[" + typeString(t.Key()) + "]" + typeString(t.Elem())
	default:
		return t.Kind().String()
	}
}

// defaultString renders a field's default value (from a Defaults() Profile) as a
// stable reference string. An invalid value (a field inside a map/slice element, which
// has no default instance) formats as the type's Go zero value.
func defaultString(v reflect.Value, t reflect.Type) string {
	if !v.IsValid() {
		v = reflect.Zero(derefType(t))
	}
	v = derefValue(v)
	if !v.IsValid() {
		return "(empty)"
	}
	if bh, ok := v.Interface().(BoolOrHuman); ok {
		if bh.Human {
			return "human"
		}
		return strconv.FormatBool(bh.Allow)
	}
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return "(empty)"
		}
		return stableDefault(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.Slice:
		if v.Len() == 0 {
			return "(empty)"
		}
		parts := make([]string, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts[i] = stableDefault(fmt.Sprint(v.Index(i).Interface()))
		}
		return strings.Join(parts, ", ")
	case reflect.Map:
		if v.Len() == 0 {
			return "(empty)"
		}
		keys := make([]string, 0, v.Len())
		m := map[string]string{}
		for _, k := range v.MapKeys() {
			ks := fmt.Sprint(k.Interface())
			keys = append(keys, ks)
			m[ks] = fmt.Sprint(v.MapIndex(k).Interface())
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + m[k]
		}
		return strings.Join(parts, ", ")
	default:
		return "(empty)"
	}
}

// stableDefault renders a default path MACHINE-INDEPENDENTLY for the committed
// reference: a value rooted at the current working directory (queue_root's cwd-relative
// default and the store_root/roles_path/issues_root derived from it) becomes a "./…"
// form, and anything under $HOME becomes "~/…" (homeRelative). Both `go generate` and
// the drift test run with cwd = the internal/config package dir, so the cwd strip is
// deterministic across machines and checkout locations. Non-path strings are unaffected.
func stableDefault(s string) string {
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if s == wd {
			return "."
		}
		if strings.HasPrefix(s, wd+string(os.PathSeparator)) {
			return "." + s[len(wd):]
		}
	}
	return homeRelative(s)
}

// homeRelative rewrites an absolute path under $HOME back to a "~/…" form so the
// generated reference is MACHINE-INDEPENDENT — the committed docs/CONFIG-REFERENCE.md
// must be byte-identical across machines (different $HOME) for the drift check to hold
// in CI. Non-path strings (e.g. remote-URL substrings) never contain $HOME, so this is
// a no-op for them.
func homeRelative(s string) string {
	home := os.Getenv("HOME")
	if home == "" {
		return s
	}
	if s == home {
		return "~"
	}
	if strings.HasPrefix(s, home+"/") {
		return "~" + s[len(home):]
	}
	return s
}

// MissingMetadata returns the yaml paths the reflective walk found that have NO
// fieldMeta entry. The completeness test asserts this is empty — the anti-drift
// guarantee that a new/changed config field can't ship undocumented.
func MissingMetadata() []string {
	var missing []string
	for _, e := range Reference() {
		if !e.hasMeta || strings.TrimSpace(e.Desc) == "" {
			missing = append(missing, e.Path)
		}
	}
	return missing
}

// orderedGroups returns the reference's block groups in first-appearance (declaration)
// order, so the rendered doc is grouped yet stable.
func orderedGroups(entries []ReferenceEntry) []string {
	seen := map[string]bool{}
	var order []string
	for _, e := range entries {
		if !seen[e.Group] {
			seen[e.Group] = true
			order = append(order, e.Group)
		}
	}
	return order
}

func enumHuman(e ReferenceEntry) string {
	if len(e.Enum) == 0 {
		return "-"
	}
	return strings.Join(e.Enum, "|")
}

// FormatReference renders the reference as an aligned human table grouped by block —
// the default `leadkit config reference` output.
func FormatReference(entries []ReferenceEntry) string {
	var b strings.Builder
	b.WriteString("leadkit config reference — every profile field, its type, default, allowed values, and description.\n")
	b.WriteString("Paths are yaml keys; <repo> is a per-repo map key, [] a list entry. All fields are optional.\n")
	for _, g := range orderedGroups(entries) {
		fmt.Fprintf(&b, "\n# %s\n", g)
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "FIELD\tTYPE\tDEFAULT\tALLOWED\tDESCRIPTION")
		for _, e := range entries {
			if e.Group != g {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Path, e.Type, e.Default, enumHuman(e), e.Desc)
		}
		tw.Flush()
	}
	return b.String()
}

// mdEscape escapes the characters that would break a markdown table cell.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func enumMarkdown(e ReferenceEntry) string {
	if len(e.Enum) == 0 {
		return "-"
	}
	return "`" + strings.Join(e.Enum, "` \\| `") + "`"
}

// FormatReferenceMarkdown renders the reference as a stable markdown document — the
// committed docs/CONFIG-REFERENCE.md. Deterministic: same input always yields
// byte-identical output, which is what the drift-check test relies on.
func FormatReferenceMarkdown(entries []ReferenceEntry) string {
	var b strings.Builder
	b.WriteString("# leadkit config reference\n\n")
	b.WriteString("Every option in a leadkit profile (`leadkit.yaml`), its type, default, allowed values, and what it does. ")
	b.WriteString("**This file is generated** from the config schema by `leadkit config reference --markdown` ")
	b.WriteString("(regenerate with `go generate ./internal/config`) — do not edit by hand; a test fails if it drifts from the schema.\n\n")
	b.WriteString("Every field is optional; unset fields take the default shown. Paths are yaml keys. ")
	b.WriteString("`<repo>` is a per-repo entry in a map (matched by an origin-remote-URL substring; when several keys match, the most-specific — longest — key wins deterministically). `leadkit config show` prints the EFFECTIVE per-origin push_whitelist and which key set it, and `leadkit doctor` warns on a loose (mid-word) or ambiguous authority-key match. `[]` marks a list entry.\n\n")
	b.WriteString("## Where the config lives (resolution order)\n\n")
	b.WriteString("A leadkit config may sit IN the repo or OUTSIDE it. On load, the effective config is resolved in this order (first hit wins):\n\n")
	b.WriteString("1. **`$LEADKIT_PROFILE`** — an explicit path, if set.\n")
	b.WriteString("2. **In-repo `leadkit.yaml`** — walking up from the cwd to the first one found, stopping at the repo root, which is the nearest ancestor holding a `.git` entry (a directory, or the file a linked worktree has), found by reading the filesystem rather than running git, so a git failure cannot lift the boundary; an ancestor directory above the repo root is never consulted. This is the original, back-compatible path, and it wins over an external config for every key that DESCRIBES a project; the keys that GRANT permission are the external config's (see 3).\n")
	b.WriteString("3. **External `~/.claude/leadkit/config/<slug>.yaml`** — used only when no in-repo config is found. `<slug>` is the repo's ORIGIN-based identity slug (its `remote.origin.url` repo-name — last path segment minus `.git`, slugified; the git-toplevel basename only when there is no resolvable origin), so ONE external config resolves from EVERY clone/worktree/checkout-dir of the repo (a worktree shares its main repo's origin), not just one directory name. So a repo can be governed with NO committed config. Override the directory location via the account convention (`leadkit config init --external` writes a starter here). Two deliberate rules for an external config: its **project root** (what the review gate scopes its own-origin on, and what identity maps to) is the REAL repo it was resolved for — not the config file's dir; but its **relative paths** (`queue_root: ./queue`, ...) anchor to the config file's OWN dir, keeping the queue/store out of the repo too (use an absolute path to place them elsewhere). **A committed in-repo config may DESCRIBE a project but not GRANT**: when an external config exists for the repo, its `push_whitelist`, `authority`, `enforced_remotes`, `owners_file`, `records_dir` and `guards` are the effective ones and the committed file's values for those keys are ignored (anyone who can land a commit can land a leadkit.yaml, so a branch must not authorise its own push). An external config whose `project_origin` does not match this clone belongs to another repo: the grant keys are then withheld (the built-in defaults, so default-deny) rather than taken from the clone's committed file, and the clone's other keys still apply. With no external config for the repo anywhere the committed file governs as before (an adopter with no operator config defers to nothing); a profile named by `$LEADKIT_PROFILE` is the operator's own and is never overridden.\n")
	b.WriteString("4. **Built-in defaults** — none of the above found.\n\n")
	b.WriteString("`leadkit config init [--external] [--project <slug>]` writes a starter (idempotent); `leadkit whoami` / `leadkit config show` name WHICH config resolved and from where.\n")
	for _, g := range orderedGroups(entries) {
		fmt.Fprintf(&b, "\n## %s\n\n", g)
		b.WriteString("| Field | Type | Default | Allowed | Description |\n")
		b.WriteString("| --- | --- | --- | --- | --- |\n")
		for _, e := range entries {
			if e.Group != g {
				continue
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s |\n",
				e.Path, mdEscape(e.Type), mdEscape(e.Default), enumMarkdown(e), mdEscape(e.Desc))
		}
	}
	return b.String()
}
