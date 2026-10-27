# leadkit config reference

Every option in a leadkit profile (`leadkit.yaml`), its type, default, allowed values, and what it does. **This file is generated** from the config schema by `leadkit config reference --markdown` (regenerate with `go generate ./internal/config`) — do not edit by hand; a test fails if it drifts from the schema.

Every field is optional; unset fields take the default shown. Paths are yaml keys. `<repo>` is a per-repo entry in a map (matched by an origin-remote-URL substring; when several keys match, the most-specific — longest — key wins deterministically). `leadkit config show` prints the EFFECTIVE per-origin push_whitelist and which key set it, and `leadkit doctor` warns on a loose (mid-word) or ambiguous authority-key match. `[]` marks a list entry.

## Where the config lives (resolution order)

A leadkit config may sit IN the repo or OUTSIDE it. On load, the effective config is resolved in this order (first hit wins):

1. **`$LEADKIT_PROFILE`** — an explicit path, if set.
2. **In-repo `leadkit.yaml`** — walking up from the cwd to the first one found, stopping at the repo root, which is the nearest ancestor holding a `.git` entry (a directory, or the file a linked worktree has), found by reading the filesystem rather than running git, so a git failure cannot lift the boundary; an ancestor directory above the repo root is never consulted. This is the original, back-compatible path, and it wins over an external config for every key that DESCRIBES a project; the keys that GRANT permission are the external config's (see 3).
3. **External `~/.claude/leadkit/config/<slug>.yaml`** — used only when no in-repo config is found. `<slug>` is the repo's ORIGIN-based identity slug (its `remote.origin.url` repo-name — last path segment minus `.git`, slugified; the git-toplevel basename only when there is no resolvable origin), so ONE external config resolves from EVERY clone/worktree/checkout-dir of the repo (a worktree shares its main repo's origin), not just one directory name. So a repo can be governed with NO committed config. Override the directory location via the account convention (`leadkit config init --external` writes a starter here). Two deliberate rules for an external config: its **project root** (what the review gate scopes its own-origin on, and what identity maps to) is the REAL repo it was resolved for — not the config file's dir; but its **relative paths** (`queue_root: ./queue`, ...) anchor to the config file's OWN dir, keeping the queue/store out of the repo too (use an absolute path to place them elsewhere). **A committed in-repo config may DESCRIBE a project but not GRANT**: when an external config exists for the repo, its `push_whitelist`, `authority`, `enforced_remotes`, `owners_file`, `records_dir` and `guards` are the effective ones and the committed file's values for those keys are ignored (anyone who can land a commit can land a leadkit.yaml, so a branch must not authorise its own push). An external config whose `project_origin` does not match this clone belongs to another repo: the grant keys are then withheld (the built-in defaults, so default-deny) rather than taken from the clone's committed file, and the clone's other keys still apply. With no external config for the repo anywhere the committed file governs as before (an adopter with no operator config defers to nothing); a profile named by `$LEADKIT_PROFILE` is the operator's own and is never overridden.
4. **Built-in defaults** — none of the above found.

`leadkit config init [--external] [--project <slug>]` writes a starter (idempotent); `leadkit whoami` / `leadkit config show` name WHICH config resolved and from where.

## (top-level)

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `project_id` | `string` | `(empty)` | - | Optional stable per-project id; when set it wins over the queue_root-derived slug for per-project daemon namespacing. |
| `project_origin` | `string` | `(empty)` | - | EXTERNAL configs only: the origin the repo this config governs must have, as `owner/repo` or `host/owner/repo`. An external config is found by the origin's repo NAME, so a same-named clone of someone else's repo would otherwise load it and take its absolute store_root and queue_root; when set, a repo whose origin does not match is refused. `leadkit config init --external` writes it. |
| `queue_root` | `string` | `./queue` | - | Root of the on-disk work queue (per-role pending/ inboxes). Default is $LEADKIT_QUEUE if set, else a cwd-relative ./queue — no adopter path is baked in. |
| `store_root` | `string` | `./tasks` | - | Root of the task store (tasks/*.md). Default is a sibling of queue_root. |
| `records_dir` | `string` | `~/.claude/review-gate/records` | - | Where review-gate verdict records are written. May use ~/ or $ENV for machine/user-scoped paths. |
| `roles_path` | `string` | `./queue/ROLES` | - | Path to the ROLES file enumerating the canonical roles. Default <queue_root>/ROLES. |
| `issues_root` | `string` | `./issues` | - | Taxonomy Issue level: a flat dir of issues/*.md sibling to store_root's tasks/ (Project->Milestone->Epic->Issue->Task). |
| `enforced_remotes` | `[]string` | `(empty)` | - | OPTIONAL origin-URL substrings of OTHER repos the review-gate also enforces on (a remote matches if its URL CONTAINS any listed value). The project's OWN origin is always gated, whatever this list holds, so a project gates its own pushes with zero config. Declare this ONLY to gate repos OTHER than the profile's own origin. |
| `keychain_service` | `string` | `Claude Code-credentials` | - | macOS keychain service name usage reads the Claude session token from. |
| `telemetry_log` | `string` | `~/.claude/tool-usage.jsonl` | - | Path to the append-only telemetry JSONL log. |
| `pr_fallback_hint` | `string` | `(empty)` | - | OPTIONAL adopter trailer the PR-guard appends to its raw-forge deny messages (e.g. a pointer to a legacy/local PR helper). No default; the deny message names the governed `leadkit pr ...` path generically and appends this only when set — so no adopter script path is ever baked into the source. |
| `wake_command` | `string` | `(empty)` | - | Optional out-of-band notifier the waker daemon execs when a ticket lands ({agent}/{msg} substituted). Empty is the normal default (agents idle on event-driven queue wait). |
| `goal_file` | `string` | `.leadkit/goal.md` | - | Repo-relative path to the goal artifact. Namespaced under .leadkit/ to avoid clobbering a project's own root files. |
| `handover_file` | `string` | `.leadkit/handover.md` | - | Repo-relative path to the session handover artifact. |
| `milestones_file` | `string` | `.leadkit/milestones.yaml` | - | Repo-relative path to the machine-readable milestone + DoD anchor the idle-gate evaluates. DISTINCT from the taxonomy's <store_root>/milestones.yaml; a missing file degrades cleanly, never a false GO. |
| `owners_file` | `string` | `(empty)` | - | Repo-relative path to the owners boundary policy (hard_block/paths/clone_families). Empty (or absent file) DISABLES owner-guard entirely; a conventional value is .leadkit/owners.yaml. |
| `canonical_statuses` | `[]string` | `pending, in_progress, in_review, awaiting_pr_review, blocked, needs_rework, gated, completed, dropped, superseded, reference, plan, draft, design` | - | Sanctioned set of store-task frontmatter status values; audit's disk lens FLAGS deviations, it never rewrites the vocabulary. |
| `session_tasks_root` | `string` | `~/.claude/tasks` | - | Where ~/.claude/tasks/session-<id>/*.json live; audit's session lens reads these to catch real work filed as an ephemeral session task. |
| `push_whitelist` | `[]string` | `feat/*, fix/*, chore/*, mg/*, user/*` | - | Default branch patterns a raw git push may reach (`*` spans `/`, so feat/* matches feat/x/y). A repo's authority.<repo>.push_whitelist REPLACES this for that repo; `leadkit config show` prints the effective per-origin result. |
| `sweep_status_file` | `string` | `.leadkit/sweep-status.json` | - | Where each `queue sweep` writes its machine-readable findings artifact (per-lane stalls, broken links, milestone re-anchor). The single source doctor/status/board all read. Default .leadkit/sweep-status.json. |
| `push_violations_log` | `string` | `.leadkit/push-violations.jsonl` | - | JSONL trail of denied pushes (whitelist-miss and fail-closed-unverifiable). Best-effort; the board's /health surfaces recent entries. |
| `lane_aliases` | `map[string]string` | `(empty)` | - | Optional lane-name -> canonical-role map `queue consolidate` (and the lane resolver) consult LAST, for strays the built-in `-agent` lane-fold (devops-agent->devops) can't rescue. Each value MUST be a known role (FixedRole or roster type); a dangling target is refused (no phantom lane) and doctor warns. Never overrides a canonical match. |
| `status_aliases` | `map[string]string` | `awaiting_review=in_review, done=completed` | - | Known status-alias map (source spelling -> canonical) `audit --apply` auto-rewrites; only EXACT known aliases are rewritten, anything else is reported. |
| `audit_apply_log` | `string` | `.leadkit/audit-apply.jsonl` | - | JSONL log of every fix `audit --apply` makes (dangling reaped / status normalized / done-drift relocated). |
| `spikes_log` | `string` | `.leadkit/spikes.jsonl` | - | JSONL log `spike record` appends a verified spike/dispatch result to. |
| `decisions_log` | `string` | `.leadkit/decisions.jsonl` | - | JSONL log `decision record` appends a human-in-the-loop decision to. |
| `preemption_log` | `string` | `.leadkit/preemption.jsonl` | - | JSONL log `burn` appends a record to each time priority-ordered preemptive shedding reduces a session's live count. Best-effort. |
| `routing_ttl` | `string` | `168h0m0s` | - | Retention window for the account-level routing store: a TERMINAL routing item (a Transition marked it closed) older than this is prune-eligible by `leadkit routing prune`; a NON-terminal item is kept forever regardless of age. Its history travels with it and is pruned together. A time.ParseDuration string (e.g. "168h"). |
| `registry_repo` | `string` | `~/.claude/leadkit/registry` | - | Local dir for the lead & session identity registry, holding one file per session under sessions/<host>__<session_id>.json. LOCAL BY DEFAULT: `leadkit register`/`who` read+write it directly (atomic temp+rename, no git, no setup) — complete and non-degraded with no remote. Only when registry_remote is set (or the dir is already a git repo with an origin) does it become a git repo that best-effort syncs the disjoint per-session files across machines on each call. |
| `registry_remote` | `string` | `(empty)` | - | OPTIONAL shared git remote for CROSS-MACHINE registry sync. Empty (the default) = LOCAL-only: register/who work against the local registry_repo dir with no git and no warning. When set, the registry syncs (commit + pull-rebase + push) to this remote, and `leadkit registry init` clones/attaches it when no --remote is passed. |
| `registry_ttl` | `string` | `30m0s` | - | How stale a registry session entry may get before `leadkit who` treats it as STALE — omitted from the table by default, shown greyed with --all, and prunable by `leadkit registry prune`. A session --touches periodically. A time.ParseDuration string (e.g. "30m"). |
| `team_config` | `string` | `(empty)` | - | Project-layer key: path to the shared TEAM config file (one team config serves many projects). Empty means no team layer. |
| `locked` | `[]string` | `(empty)` | - | TEAM-layer-only key: top-level keys the team MANDATES — a locked key's team value wins over project and user unconditionally. |
| `workspace_model` | `string` | `clones` | `worktrees` \| `clones` | Preference key (user>project>team): how agents work the repo. |
| `outputs` | `[]string` | `(empty)` | - | Active `leadkit sync` targets (e.g. ["linear"]); a team sets the standard, a project may override, a team may lock it. |
| `brief_length_max` | `int` | `1400` | - | Team-policy character limit for the brief-length guard. leadkit carries the value; the enforcing hook is separate. |
| `messaging_model` | `string` | `inbox` | `inbox` \| `message` | Team-policy key: which teammate-messaging shape the team uses. |

## hierarchy

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `hierarchy.type_hints` | `map[string][]string` | `(empty)` | - | Optional map of canonical agent type -> the epic id(s) its subtasks most likely belong to. Source ships none; only fires for an epic id present in the parsed model. `hierarchy propose`'s generic alias/title match runs regardless. |
| `hierarchy.keyword_hints[].keywords` | `[]string` | `(empty)` | - | Title keywords (matched case-insensitively as substrings) for this keyword->epic seed. |
| `hierarchy.keyword_hints[].epic` | `string` | `(empty)` | - | The epic id a matching keyword proposes (only fires when that epic id exists in the model). |

## burn

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `burn.session_reserve` | `float` | `10` | - | Percent of the weekly budget held untouched for the user's own use (personal, user-wins preference key burn.session_reserve). |
| `burn.op_rate_warn` | `int` | `400` | - | Advisory warn threshold for operations/hour. |
| `burn.agent_rate_warn` | `int` | `40` | - | Advisory warn threshold for agent spawns/hour. |
| `burn.workstream_ttl` | `string` | `30m0s` | - | How stale a project's account-wide workstream-registry entry may get before it's pruned from account burn totals. A time.ParseDuration string (e.g. "30m"). |
| `burn.workstream_stale_window` | `string` | `15m0s` | - | How stale a workstream/presence registry entry may get before the burn GOVERNOR ignores it when computing account_n (tighter than workstream_ttl, so a crashed/exited session stops inflating the count promptly). The auto-register presence heartbeat refreshes on every session activity, so a live session never falls past it. A time.ParseDuration string (e.g. "15m"). |
| `burn.enforce_interval` | `string` | `2m0s` | - | How often the account-level burn-enforce daemon (dev.leadkit.burn) reads real account weekly usage and writes/clears the account pause lock from the pause_pct/resume_pct hysteresis — so the throttle fires without any session running `leadkit burn`. `leadkit init` translates it into the daemon's StartInterval. A time.ParseDuration string (e.g. "2m"). |
| `burn.productivity_stall_window` | `string` | `3h0m0s` | - | How long an agent may be ACTIVE (alive + churning) with NO deliverable — no new commit, gate verdict, queue advancement, or session-task→done — before the productivity watcher flags a productive-stall ("busy is not productive"). A time.ParseDuration string (e.g. "3h"). |
| `burn.inprogress_stall_window` | `string` | `45m0s` | - | How long a ticket may sit CLAIMED in an in-progress lane (its symlink mtime) with NO deliverable advancement — no new commit, gate verdict, or queue stage-move — before the frozen-claim detector flags it as a hung owner's frozen claim (probe the owner; a live one answers, a hung one is silent). A time.ParseDuration string (e.g. "45m"). |
| `burn.session_priority` | `string` | `(empty)` | `critical` \| `high` \| `normal` \| `low` | This session's self-declared priority tier in the account-wide session registry; governs preemptive shedding order. $LEADKIT_SESSION_PRIORITY overrides. Empty means normal. |

## roster

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `roster[].type` | `string` | `(empty)` | - | Canonical role name for this reviewer/agent-pool entry. |
| `roster[].count` | `int` | `0` | - | How many of this role should be live. |
| `roster[].lane` | `string` | `(empty)` | - | Optional explicit live queue-lane override so more than one role can share one consumer lane. Unset defaults the lane to the role name. (code-review + critique may never share a lane.) |
| `roster[].review_layer` | `bool` | `false` | - | Declares this role review-capable: its name becomes a review layer, a valid `gate record --layer` value and a valid authority.<repo>.required_gates entry. code-review and critique are review layers in every project and need no entry. finalise and orchestrator cannot be layers. A layer's recorder must be of a different role family than every other layer's. |

## heartbeat

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `heartbeat.schedule` | `string` | `(empty)` | - | Cron expression the idle heartbeat re-arms on. |
| `heartbeat.prompt_ref` | `string` | `(empty)` | - | What to run on each heartbeat tick. |

## milestone

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `milestone.idle_gate_mode` | `string` | `block` | `block` \| `warn` | What `milestone check` does on an unmet DoD: block (exit non-zero, the hard gate) or warn (report loudly, exit 0). A --mode flag overrides. The SendMessage backstop stays warn-only regardless. |
| `milestone.enable_script_signals` | `bool` | `true` | - | Opt-in safety gate for the `type: script` DoD signal, which EXECUTES arbitrary shell (a lead+user-authored bash command or in-repo script) at the user's privileges — same trust level as leadkit.yaml, a git hook, or a Makefile target. Default true (a project that wrote a script condition wants it run); set false to categorically forbid script execution, making every `type: script` condition read unverified ("script signals disabled") without ever calling the runner. |
| `milestone.script_timeout` | `string` | `30s` | - | How long a `type: script` DoD signal may run before it is killed and read unverified (never a false met) — bounds every run so a hung check can't wedge `milestone check`/the sweep. A time.ParseDuration string (e.g. "30s"). |

## authority

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `authority.<repo>.can_push` | `bool` | `false` | - | Whether an agent may push in this repo unsupervised. |
| `authority.<repo>.can_open_pr` | `bool\|human` | `false` | `true` \| `false` \| `human` | Whether an agent may open a PR: true/false, or "human" meaning it always needs a human regardless of gate state. |
| `authority.<repo>.pr_draft_only` | `bool` | `false` | - | When true, PRs in this repo may only be opened as draft. |
| `authority.<repo>.can_merge` | `bool\|human` | `false` | `true` \| `false` \| `human` | Whether an agent may merge: true/false, or "human" (always needs a human). |
| `authority.<repo>.merge_grant` | `string` | `(empty)` | - | A standing-grant id (incident/ticket ref) that lets the lead merge WITHOUT per-merge human approval while set. Empty means merge stays fully human regardless of can_merge. |
| `authority.<repo>.required_gates` | `[]string` | `(empty)` | - | Ordered review layers that must all be ready before this repo's PR may advance; list order is chain order, so a layer can be recorded ready only after every layer before it is. Each entry must be code-review, critique, or a roster role declared review_layer: true; anything else is reported by config validation and doctor, and no record can satisfy it. Empty means [code-review, critique]. A repo's push gate uses the default pair. |
| `authority.<repo>.push_whitelist` | `[]string` | `(empty)` | - | When set, REPLACES the top-level push_whitelist entirely for this repo (no merge). The repo is matched by origin-URL substring; when several keys match, the longest (most-specific) key wins. |
| `authority.<repo>.raw_pr_verbs` | `[]string` | `(empty)` | `create` \| `edit` | Additive allow-list of canonical PR verbs a raw forge CLI (gh/glab) may invoke without a wrapper. "merge" is guard-railed: always ignored/denied. |
| `authority.<repo>.owner_prs_only` | `bool` | `false` | - | When true, a granted raw `edit` is restricted to PRs the authenticated forge user authored; a failed author lookup fails CLOSED. Never restricts create or merge. |
| `authority.<repo>.forge` | `string` | `(empty)` | `github` \| `gitlab` | Optional per-repo forge override for pr create / the pr-guard. Empty picks the forge from the origin host heuristic; set only for a self-hosted host that names neither. |
| `authority.<repo>.protected_branches` | `[]string` | `(empty)` | - | Branches `leadkit forge protect` configures and `leadkit doctor` verifies server-side. Exact names only, no globs. Empty means the repo's default branch as the forge reports it. |

## session_task_workish

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `session_task_workish.description_length_threshold` | `int` | `280` | - | A session task longer than this counts as "real work" for the wrong-mechanism audit lens. |
| `session_task_workish.markers` | `[]string` | `##, ```, acceptance, DoD` | - | Substrings (matched case-insensitively) that mark a session task as real work regardless of length. |

## throttle

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `throttle.lock_file` | `string` | `.leadkit/PAUSE` | - | Path to the pause-lock sentinel burn writes when usage crosses pause_pct. |
| `throttle.pause_pct` | `float` | `98` | - | Session-usage percent at which burn LOCKS the team. |
| `throttle.resume_pct` | `float` | `90` | - | Session-usage percent the lock CLEARS below; the gap to pause_pct is the hysteresis that stops the lock flapping. |

## diagnostics

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `diagnostics.surface_doctor` | `bool` | `true` | - | When true, `doctor` + `status` read the findings artifact and surface stalls/broken-links WITH the remediation. |
| `diagnostics.surface_board` | `bool` | `true` | - | When true, `serve` /health renders the findings (extends the push-violations section). |
| `diagnostics.surface_notify` | `bool` | `false` | - | When true AND notify_command is set, the sweep execs the operator's notifier once findings cross notify_threshold. |
| `diagnostics.notify_command` | `string` | `(empty)` | - | Operator's notifier, {stalled}/{broken}/{total}/{msg}-placeholder shape. Empty means no push, ever. |
| `diagnostics.notify_threshold` | `int` | `1` | - | Stalls+broken count at/above which the notifier fires. <=0 is normalized to 1 (any finding fires). |
| `diagnostics.self_repair` | `string` | `command` | `report` \| `command` \| `auto` | Sweep's dangling-link repair mode: report (surface only), command (recommended: surface + name `queue repair`), or auto (reap dangling links each run). `queue repair` stays available regardless. |

## guards

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `guards.default_mode` | `string` | `advisory` | `advisory` \| `enforce` | Fallback enforcement mode for every security guard whose own mode is unset. advisory (default) = today's fail-OPEN posture (an error/unverifiable condition ALLOWS); enforce = fail-CLOSED (an unverifiable condition DENIES, authoritative). Resolved from the TARGET repo/file's governing profile. Set enforce for real blocking. |
| `guards.push_guard.mode` | `string` | `(empty)` | `advisory` \| `enforce` | push-guard's enforcement mode; overrides guards.default_mode. advisory = today; enforce = an unparseable/unverifiable push target is DENIED (push-guard already fail-closes on its unverifiable paths — enforce makes the posture explicit and names it in the deny). Empty inherits default_mode → advisory. |
| `guards.push_gate.mode` | `string` | `(empty)` | `advisory` \| `enforce` | push-gate's enforcement mode; overrides guards.default_mode. advisory = today (a bad repo path / unresolvable HEAD / unresolvable-scope push fails OPEN); enforce = those unverifiable pushes are DENIED (can't confirm the sha is gated → block). A clear gated allow and a clear ungated deny are unchanged in both. Empty inherits default_mode → advisory. |
| `guards.owner_guard.mode` | `string` | `(empty)` | `advisory` \| `enforce` | owner-guard's enforcement mode; overrides guards.default_mode. advisory = today (an owners_file that is configured but unreadable/unparseable fails OPEN); enforce = a configured-but-unreadable owners policy DENIES (can't confirm ownership → block). A project that defines no owners_file has no boundary and is unaffected in both. Empty inherits default_mode → advisory. |

## tests

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `tests.<repo>.test_command` | `string` | `(empty)` | - | When set, REPLACES the inferred suite command entirely (run verbatim via a shell). |
| `tests.<repo>.affected_command` | `string` | `(empty)` | - | When set, replaces the inferred affected-tests command (used only with `test run --affected`). |

## team

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `team.agents_dir` | `string` | `~/.claude/agents` | - | Where the LIVE agent defs live; `team capture` versions them into <team_dir>. |
| `team.protocol_file` | `string` | `~/.claude/agents/_team-protocol.md` | - | Path to the live team-protocol file. |
| `team.sop_file` | `string` | `../AUTONOMOUS-TEAM-SOP.md` | - | Path to the project's hand-maintained SOP (default climbs one level out of the leadkit/ subdir). |
| `team.team_dir` | `string` | `team` | - | In-repo dir `team capture` versions the operating-model snapshot into. |
| `team.skills_dir` | `string` | `~/.claude/skills/leadkit-adopt` | - | Where `init --apply` installs the live leadkit-adopt skill; `team capture` versions it too. Default ~/.claude/skills/leadkit-adopt. |

## linear

| Field | Type | Default | Allowed | Description |
| --- | --- | --- | --- | --- |
| `linear.team_id` | `string` | `(empty)` | - | Target Linear team key (e.g. "CI"). Required when outputs includes "linear"; doctor warns if unset. |
| `linear.root_initiative` | `string` | `(empty)` | - | Linear Initiative id the local Project maps to; created + recorded on first run when unset. |
| `linear.closed_state` | `string` | `Done` | - | Linear workspace state NAME used for a close op (workspace-specific). |
| `linear.default_state` | `string` | `Todo` | - | Linear workspace state NAME used for a create op (workspace-specific). |
| `linear.sync_mode` | `string` | `(empty)` | `mcp` \| `direct` | Force the Linear execution mode: mcp (sync-executor agent) or direct (GraphQL API, key from env LINEAR_API_KEY or the keychain). Unset = direct iff LINEAR_API_KEY is in the env, else mcp; a `sync run --mode` flag overrides. |
