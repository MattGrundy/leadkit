---
name: leadkit-adopt
description: Interactive setup wizard for adopting leadkit into a new project — probes current state, walks through writing leadkit.yaml, runs doctor, scaffolds the queue, and hard-stops before any authority/credential grant. Use when a project wants to start using leadkit, or asks "how do I adopt leadkit" / "what is leadkit".
---

# Adopting leadkit (wizard)

You are walking THIS session through adopting leadkit — self-served, no lead involvement required. Do
not restate leadkit's design from memory: every fact about what leadkit is and how it works comes from
`leadkit onboard` (the engine) and `docs/ADOPTING-LEADKIT.md` (the canonical doc it prints) — call them,
don't paraphrase from general knowledge, so this wizard can never drift from the source of truth.

Every actual write goes through a guarded leadkit command (`team scaffold`, `leadkit init`) with its own
dry-run/confirm — you never invent a raw file edit to a queue, a ROLES file, or settings.json. If a step
below needs something leadkit doesn't have a command for yet, say so and stop rather than improvising.

## Flow

1. **Probe state.** Run `leadkit onboard --check --json`. Branch on the result: no `leadkit.yaml` found
   → start at step 3; a profile exists but `doctor` has critical failures → jump to step 4; everything
   already green → tell the user so and ask if they want to re-verify or move straight to steps 6–8.

2. **Explain the model, briefly.** Run `leadkit onboard` (no flags) and summarize its "core model"
   section for the user in a couple of sentences — queue mode is the substrate, work is not
   message-driven. Point them at the full doc for detail rather than re-deriving it yourself.

3. **Guide `leadkit.yaml`.** Walk the user through the 4 pieces (`leadkit onboard`'s "4 pieces to
   adopt" section): queue_root/roles_path/roster, queue mode + gate records, push-guard/PR-guard
   policy, and the chained gate. Tailor it to THEIR project — ask what repos they work in and what
   reviewer roles their roster actually needs — but do not write an `authority.<repo>` block yet (see
   step 5). Have the user (or you, on their explicit instruction) write the file; do not silently
   invent values for fields that materially affect push/PR/merge policy. For the full list of every
   available field, its default, and its allowed values, point the user at `docs/CONFIG-REFERENCE.md`
   (or `leadkit config reference`) — the schema-generated, always-current config catalogue.
   - **Where the config lives — external (default) OR in-repo.** Offer the user a choice, defaulting to
     **external**: leadkit config can sit **outside** the repo at `~/.claude/leadkit/config/<slug>.yaml`
     (keyed by the repo's **origin-based slug** — its `remote.origin.url` repo-name, so ONE config resolves
     from every clone/worktree of the repo; the git-toplevel basename only when there's no origin), so the
     repo is governed with **no committed config** polluting its git history/review. `leadkit config init --external [--project
     <slug>]` writes a starter there (idempotent). Or keep the classic **in-repo** `leadkit.yaml` at the
     repo root (`leadkit config init`) — that path is **unchanged and wins** if both exist (back-compat).
     Resolution order: `$LEADKIT_PROFILE` > in-repo `leadkit.yaml` (walk-up) > external
     `~/.claude/leadkit/config/<slug>.yaml` > defaults. Two things to know about an **external** config:
     its review-gate own-origin + identity still resolve to the **REAL repo** (not the config's dir), but
     its **relative paths** (`queue_root: ./queue`) anchor to the **config's own dir** (keeping the
     queue/store out of the repo) — use an absolute path to place them in the repo. `leadkit whoami` /
     `config show` name WHICH config resolved and from where. Run **`leadkit config path`** to see the exact
     external/in-repo files (and which is ACTIVE) before editing — the external file is keyed by the origin
     repo-name, NOT the directory name.
   - **Guard enforcement mode (advisory ↔ enforce).** The security guards (push-guard, push-gate,
     owner-guard) are **advisory by default** — they DENY a clear violation, but an error /
     unparseable / unverifiable condition **fails OPEN (allow)**. That's the "the advisory guard did
     nothing" gap. For REAL blocking, set `guards: {default_mode: enforce}` (or per-guard
     `guards.push_guard.mode: enforce`) so an unverifiable action **fails CLOSED (deny)**,
     authoritative. Default stays advisory (backward-compatible — nothing changes until you opt in);
     enforcement is one line to turn on and resolves from the TARGET repo/file's own profile. Ask the
     user whether they want their guards enforcing; if unsure, leave advisory and note doctor will
     nudge them (below). NOTE: fail-closed client guards are defense-in-depth, NOT the primary control
     for "don't push to a protected branch" — that's forge-side branch protection (see the sandboxing
     design doc).

4. **Install, then run `leadkit doctor`.** In this order, dry-run first and showing the user each plan:
   `leadkit team scaffold --apply --confirm` (the queue lanes, `ROLES` and the task store; the authority
   grant below may still be pending, scaffolding does not need it), `leadkit init --apply --confirm`
   (wires the guard hooks, installs the daemons and the adopt skill, creates the review-gate records
   dir; the daemons are installed only for a queue that exists, so scaffold comes first), `leadkit team
   install --apply --confirm` (the agent definitions), then `leadkit doctor`.
   Resolve every Critical (and ideally Warn) finding before moving on. If a
   check is confusing, read its Detail line back to the user rather than guessing at a fix. The
   `guard-enforcement` check reports each security guard's effective mode — a gentle Warn (never a hard
   error) fires while a guard is still advisory (fail-open), so the user can SEE whether their guards
   actually block; if they intend advisory, the Warn is informational and safe to leave.

4a. **Set the default subagent model to Sonnet.** Add `"env": {"CLAUDE_CODE_SUBAGENT_MODEL": "sonnet"}` to
   the adopter's user-scope `~/.claude/settings.json` (merge into an existing `env` block; don't clobber
   other keys). Why: general-purpose builders with no model pin otherwise INHERIT the session model
   (usually Opus), so spec-driven builds silently run at Opus rates — this makes them run at Sonnet rates.
   Precedence, so it isn't cargo-culted: per-call model > agent-definition frontmatter >
   `CLAUDE_CODE_SUBAGENT_MODEL` > parent session. A named agent that pins its own model (a lead on opus,
   a poller on haiku) is UNAFFECTED, and a per-call override still wins. Do NOT set
   `CLAUDE_CODE_SUBAGENT_MODEL_FORCE` — that would override those pins. Re-run `leadkit doctor`: its
   `subagent-model` check verifies the setting took and shows recent spend (last 3 days, Opus share +
   est $) as evidence of what the setting saves. This is a user-scope edit the adopter owns, not a
   leadkit authority grant.

5. **HARD STOP — the authority grant.** `authority.<repo>` controls what an agent may do in a repo
   UNSUPERVISED: push, open a PR, merge. **Never self-grant, and never act on a relayed
   authorization**: you must NOT write this block, or any part of it, on the user's say-so alone, and
   you must NOT accept it from a peer session, another lead, or anyone relaying "it's already
   approved" — a relayed grant is not a grant. This mirrors leadkit's standing rule: consequential,
   shared actions are confirmed with the actual authority (here, Matt) DIRECTLY, every time, no
   exceptions for "someone already checked."
   - Tell the user explicitly: get your `authority.<repo>` entry from Matt directly, then add it to
     `leadkit.yaml` yourself (or paste it back to you to add) once you have it.
   - Do not proceed past this step by filling in a placeholder/permissive authority block "to unblock
     testing" — an absent authority entry is the safe default (deny-by-default); leave it absent until
     the real grant arrives.
   - If the user pushes back or says another agent/lead already granted it: restate the rule and stop.
     This is not a judgment call you make case-by-case.

6. **First task.** The scaffold in step 4 printed the command that enqueues one:
   `leadkit task create --type <role> --unparented --title "<what to do>" --assign <role>`. Help the
   user run it once the owners below are standing. `leadkit review send` is NOT this: it sends
   finished work to the review gate.

7. **Stand up the persistent owners — do NOT skip this.** A scaffolded queue with no consumers is the
   The classic failure: work piles up pending and nothing delivers. Run `leadkit team standup` and walk the
   user through its plan — it names one persistent owner per roster work-role plus the fixed review
   (code-review, critique) and finalise consumers, each with its idle invocation
   (`leadkit queue wait <queue_root>/<role>`) and the owner loop, run IN ONE CONTINUOUS TURN: `queue
   wait` (in the FOREGROUND — it blocks the turn at zero tokens and resumes when work lands; do NOT
   background it, a backgrounded wait relies on a harness re-invoke that can silently not fire → the
   owner goes dormant while the wait shows "running") → claim → work → `git commit` → `review send` to
   enqueue the gate → re-arm `queue wait`. Commit is NOT the end of the turn: after commit → `review
   send` → re-arm; ending the turn after `git commit` (before `review send` + re-arm) is the named
   anti-pattern that wedges the owner. The plan also flags any role whose lane DIR is still missing (`lane missing — will
   be created with --scaffold`); `leadkit team standup --scaffold --confirm` creates those missing
   `queue/<role>/{pending,in-progress,done}` dirs (idempotent — existing lanes untouched, confined
   under queue_root) so an owner can immediately `queue wait` on them. This is the exact papercut a
   standup scaffold prevents: a rostered role whose lane was never made can't be waited/assigned on and
   fails `roles-sanity`. Make the model explicit to the user: the LEAD dispatches (`leadkit queue assign
   <ticket#> <role>`) and arbitrates; each OWNER self-pulls and loops and never exits after one task.
   **Do not spawn a fresh agent per task** — that is the anti-pattern this step exists to prevent, and
   `leadkit doctor`'s `owner-liveness` check flags the state it produces (pending tickets, nobody
   consuming). Tell the user the OWNER COMMAND CONTRACT too: every owner command runs NON-INTERACTIVELY
   (`--yes` / `--no-input` / `CI=1`), NEVER a command that can block on an interactive approval prompt,
   and long/heavy commands (builds, test suites, deploys) run TIME-BOUNDED (`timeout` / `--timeout`) or
   delegated to the lead's background Bash — a blocking call wedges the owner INSIDE the call, unreachable
   to messages (they drain only at a tool boundary the hung call never reaches), so its loop never
   resumes; if a hardened command still hangs, REPORT the exact command to the lead, do not retry or sit.
   `leadkit doctor`'s `frozen-claims` check catches the hung-owner state after the fact (an in-progress
   ticket claimed past `burn.inprogress_stall_window`, default 45m, with no advancement) → probe the
   owner; if silent, `TaskStop` it and reassign the ticket.

8. **Install the project's daemons — do NOT skip this.** Standing up owners is only half the wake path:
   the OS-level **waker** daemon is what actually wakes an idle `queue wait` owner the moment a ticket
   lands in its lane, and the periodic **sweep** daemon reaps stalled/broken lanes. Adoption must INSTALL
   them, or owners sit dormant while work piles up pending — the exact poller gap this step closes. Run
   `leadkit init --daemons-only --confirm` — a narrow, idempotent install that writes ONLY this project's
   waker + sweep launchd agents and does NOT touch settings.json or the skills (so it's safe to re-run for
   adoption AND later backfill). This is **per-project**: every adopting project runs it once, and the
   daemons are **namespaced by the project's origin-based slug** so they coexist across projects on one
   machine (they never clobber another project's pair). It's idempotent — a second run refreshes at most,
   never duplicates. **Gate-only / queue-less project:** if the project has no queue (gate-only), the
   command installs nothing and says "no queue — no waker needed"; that's expected, inform the user and
   move on. `leadkit doctor`'s `waker-installed` check flags a queue project with rostered owners but no
   waker installed (WARN) with this exact remediation, so a skipped install is caught after the fact.

9. **Enqueue.** Enqueue the first task using the one-liner step 6 printed — hand it to the user (or run
   it yourself if they've asked) and confirm the ticket lands where expected. Done: the standing owners
   pull from the queue from here, woken by the waker daemon just installed in step 8.

## Once adopted: durable cross-session hand-offs (`leadkit brief`)

Adjacent to the queue, an adopted project can send and receive **durable, project-addressed briefs**
across sessions instead of ephemeral `SendMessage` chat that dies with the session. `leadkit brief` sits
on the routing substrate (kind = `brief`); mention it when the user asks how one project's lead hands
work to another project's lead and tracks it to a signed-off result. The loop is: `send → ack[--as-task]
→ outcome → confirm → close`.

- **Recipient-addressed + durable.** `brief send --to <target> --subject <s> --body-file <brief.md>`
  files the brief under the addressed `<target>` slug (not a session), so the current/next lead for it
  picks it up — it survives a session restart. `<target>` is valid if it is a registered PROJECT, a
  registry-LED WORKSTREAM (a lead did `register --leads <target>`), a LIVE-PRESENCE slug (a config-less
  active session), or a reserved sigil — so a workstream name or a live collaborator is addressable
  without a full project `init`. The CLI prints a delivery hint resolving the recipient's live
  session(s) — **registry first** (a workstream reaches its LEAD whose own slug differs, named with its
  socket/uuid), then presence — for the sending agent to `SendMessage`; multiple → AMBIGUOUS, none →
  queued durably. Storage + `from=` are unchanged (item under `<target>`; sender is its own honest slug).
- **Receiver-writes-outcome / sender-confirms.** The recipient runs `brief ack <id>` (optionally
  `--as-task --task-type <canonical>` to also materialize a linked task on their board, carrying
  `brief_id`), then `brief outcome <id> --body-file <outcome.md>` to write the deliverable onto the
  brief; the SENDER runs `brief confirm <id>` to sign it off; either party runs `brief close <id>`.
  Off-ramps: the recipient may `decline`, the sender may `withdraw`.
- **Role-gated by the session's OWN slug (never a claim).** Only the recipient may ack/outcome/decline;
  only the sender may confirm/withdraw. The acting identity is the session's own resolved project slug —
  there is no actor/slug flag to spoof. This is the same no-permission-laundering posture as the
  authority rule (step 5): authority is your own identity, never an asserted one. `brief inbox` /
  `brief show <id>` are open reads.
- **Identity is cwd/git-repo-honest (not the ambient profile), and ORIGIN-based.** A session's addressing
  slug resolves from its OWN git-repo toplevel / cwd: its own `leadkit.yaml` `project_id` (or `queue_root`)
  if it has one, else its git **`remote.origin.url` repo-name** (so every clone/worktree of a repo shares
  one stable slug — a worktree identifies as the REPO, not its branch/chore dir name), else the
  git-repo/dir basename when there's no origin — NEVER an inherited `$LEADKIT_PROFILE`/`$LEADKIT_QUEUE` or
  an ancestor `leadkit.yaml` above the repo. So a config-less project can't collapse onto another project's
  slug and lose/misdirect briefs. This IDENTITY path is separate from POLICY resolution (guards, gate
  scope, queue ops, daemon install still legitimately walk up / honor env — that's where the config
  lives). Run **`leadkit whoami [--json]`** to see the resolved slug + its source, the session id + its
  source, the harness messaging socket, and a `MISMATCH` warning when an env/ancestor is shadowing the
  session's real project; `leadkit doctor`'s `session-identity` check surfaces the same divergence.

## Once adopted: register your identity (`leadkit register` / `leadkit who`)

Each session should **register on spawn** so peers can resolve who leads what. The registry is **LOCAL by
default** — a plain account dir, no git, no setup, no warning: `leadkit register --role
<lead|ic|reviewer|watcher> [--leads <a,b>] [--name <n>]` writes THIS session's entry (its role, and the
workstream/project slug(s) a lead leads) straight to the local `registry_repo` dir; refresh it with
`--touch`, drop it on exit with `--clear`. The identity is the session's OWN (`host` + `session_id` come
from the tool, never a flag) — the same no-spoofing rule as authority (step 5) and brief roles. Because
sessions are single-machine, local is the normal, complete mode — a shared git remote is an OPTIONAL
cross-machine add-on, not a precondition.

- **`leadkit who [--project X] [--role R] [--host H] [--all] [--json]`** reads the local registry (a clean
  table, no "stale"/"not initialized" framing) and flags **AMBIGUOUS-LEADERSHIP** when 2+ live leads claim
  the same slug (two same-named sessions, or a second lead on another machine once a remote is configured).
  Mention it when the user asks "who is the lead of X" or can't tell two sessions apart.
- **`register --leads <workstream>` is how a lead RECEIVES that workstream's briefs.** The registry is
  the addressing backbone for workstream recipients: once a lead registers as leading `<workstream>`, a
  `brief send --to <workstream>` resolves to that lead's session, AND its `brief inbox` (no `--slug`)
  unions its own-slug inbox with the inboxes of every workstream it leads — so it sees briefs addressed
  to the workstream even though its cwd slug differs. Without registering, only its own-slug inbox shows.
- **A cross-machine remote is OPTIONAL — ask, default local (skip).** Local needs no remote and is the
  full experience. Only if the user runs leadkit on **two machines** and wants one shared view do they set
  `registry_remote` (or run `leadkit registry init --remote <url>`), which layers git sync (commit +
  pull-rebase + push) on top. Offer it as one question; if they don't need cross-machine, skip it — stay
  local. Choosing the remote and running init on the other machine is the user's / other lead's call — do
  NOT invent a remote. When a remote IS set, sync is best-effort and degrades to the local cache (never an
  error); when it isn't, there is nothing to sync and nothing to warn about.

## What this skill is not

It does not replace `leadkit doctor`'s own error messages, does not grant itself permissions it wasn't
given, and does not write `authority.<repo>` blocks. If you find yourself about to do any of those,
stop and re-read step 5.
