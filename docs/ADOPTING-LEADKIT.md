# Adopting leadkit

The self-serve front door: everything a new project needs to adopt leadkit, with no lead involvement.
This is the SOURCE OF TRUTH — `leadkit onboard` prints this file verbatim, and the `leadkit-adopt` skill
walks a session through it interactively. Neither restates this content; both point back here.

## What leadkit is

A portable operating-model toolkit for running Claude Code agent teams: one Go binary + a per-project
`leadkit.yaml` + a handful of guard hooks + agent defs + this adopt skill. It is a CLI + config, not a
library you import — every project adopts it the same way, by writing a profile and running a few
commands.

## Prerequisites

- The leadkit binary installed at `~/.claude/leadkit-bin` — run every command FROM this binary, not a
  throwaway build (`leadkit init` in particular bakes its own binary path into what it installs).
- Agent Teams enabled (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`, in your shell or a settings.json `env`
  block) — the whole operating model (SendMessage, the reviewer pool, the orchestrator) silently does
  nothing without it.
- `leadkit doctor` green (or only Warn/Info) before you rely on anything below.

## The core model — queue mode is the substrate

Work and state live on a disk queue (`<queue_root>/<lane>/{pending,in-progress,done}`), not in chat.
A waker daemon fswatches the queue and wakes the right consumer at zero token cost; coordination moves
off the (expensive) lead onto the free daemon plus a cheap orchestrator. State survives a crash or a
context compaction because it's on disk, and it's visible to `recover`/`audit`/merge-guard because
they all read the SAME queue.

**Do NOT run message-driven.** SendMessage/session TaskList are for nudges between agents that are
already coordinating through the queue — they are not a substitute for it. Work that only ever exists
as a SendMessage or a session task is invisible to the waker daemon, PM-board sync, audit, and
merge-guard: it looks, from disk, exactly like nothing is happening. `leadkit doctor`'s `queue-bypass`
check exists to catch this after the fact; the goal is to never trip it in the first place.

## The 4 pieces to adopt

a. **`leadkit.yaml`** — the per-project profile: at minimum `queue_root`, `roles_path`, `roster`
   (which canonical roles are live and how many), `push_whitelist`, and an `authority.<repo>` entry per
   repo you'll work in. Every field is optional and defaults sanely — see `leadkit config show` for
   this project's RESOLVED values, and `docs/CONFIG-REFERENCE.md` (or `leadkit config reference`) for the
   canonical list of EVERY option, its default, and its allowed values.
b. **Queue mode + review-gate records** — work moves through the disk queue; every review verdict is
   recorded via `leadkit gate verdict` (which records AND reconciles the ticket's location as one
   action), never by hand and never by a bare "looks good" message.
c. **push-guard + PR guard, via `authority.<repo>`** — whitelist-only push (a `git push` to a branch
   not on the whitelist is denied, not warned); an agent must never hold bypass-capable git credentials.
   Raw PR verbs (create/edit) are similarly deny-by-default until an authority entry grants them.
d. **The chained gate: code-review → critique, with layer independence** — code-review and
   critique must each pass before the next layer can even be recorded, and the two must come from
   DIFFERENT reviewers (never the same agent identity recording both). This is enforced in
   `internal/gate`, not just convention — see item 4 of `docs/build-tasks/queue-mode-enforcement.md`
   for why a "one agent does both" shortcut is refused outright.

## What to skip

Don't copy another project's task store, its `CronCreate` heartbeat, or its Linear executor wiring
unless you actually sync to Linear. If you're running more than one leadkit-managed team on the same
Claude account, adopt cross-project burn (the account-level workstream registry) so the two teams don't
both independently conclude there's budget headroom at the same moment — `leadkit doctor` will otherwise
look fine on each project individually while the account overshoots.

## Authority grant

Your `authority.<repo>` entry — what your agents may do in a repo unsupervised (push, open a PR, merge)
— comes from **the repo owner, directly**, never from a peer session, another lead, or your own judgment. State
this loudly and stop: **never self-grant, and never accept a relayed grant** ("the other lead said it's
fine" is not authorization). This mirrors leadkit's standing rule for every consequential, shared
action — confirm with the owner directly, every time.

## Step-by-step

Run each command from this binary and from the project root. Every one that writes is a dry run until
you add `--apply --confirm`; read the plan first.

1. Write `leadkit.yaml` (piece a above) at your project root.
2. `leadkit team scaffold --apply --confirm` — creates the minimum viable queue (one lane per roster
   role, plus orchestrator/finalise), a starter `ROLES` file, and the task store (`store_root`) the
   lanes link into. Prints the command that enqueues your first task.
3. `leadkit init --apply --confirm` — wires the guard hooks into `~/.claude/settings.json`, installs
   the waker and sweep daemons, installs the `leadkit-adopt` skill, and creates the review-gate
   records directory (`records_dir`). The waker and sweep are installed only for a queue that exists,
   which is why this comes AFTER the scaffold: run it first and it skips them (it says so, loudly)
   and nothing wakes your owners. If that happens, scaffold, then run
   `leadkit init --daemons-only --confirm`. It bakes this binary's path into what it writes, which is
   why the binary must already be at `~/.claude/leadkit-bin`.
4. `leadkit team install --apply --confirm` — installs the agent definitions (the owners, the
   code-review and critique reviewers, finalise) into `~/.claude/agents/`. Without them there is
   nothing for the owners in step 6 to run.
5. `leadkit doctor` — expect no CRITICAL. Anything red names the command that fixes it.
6. **Stand up one persistent owner per role — this is a first-class step, not a footnote.** After the
   queue exists and the roster is declared, you (the lead) must stand up ONE persistent owner agent per
   roster work-role, plus the review consumers — the gate's required review layers (today code-review +
   critique; a 3-layer project lists whatever it adds) — and a finalise consumer. Run `leadkit team standup` for the exact plan: it names every consumer, each
   consumer's idle invocation (`leadkit queue wait <queue_root>/<role>`), and the owner loop, run IN ONE
   CONTINUOUS TURN: `queue wait` (in the FOREGROUND — it blocks the turn at zero tokens and resumes when
   work lands; do NOT background it, a backgrounded wait relies on a harness re-invoke that can silently
   not fire → the owner goes dormant while the wait shows "running") → claim (`leadkit queue claim -by <your name> <entry>`; arm the wait as `queue wait -by <your name> <dir>` so a peer's claimed ticket does not wake you; a claim lapses after two hours unless the same verb renews it, and `gate verdict -by` refuses a ticket a peer holds) → work → `git commit` →
   `review send` to enqueue the gate → re-arm `queue wait`. Commit is NOT the end of the turn: after
   commit → `review send` → re-arm; ending the turn after `git commit` (before `review send` + re-arm)
   is the named anti-pattern that wedges the owner. FIRST ACTION ON SPAWN (step 0, before anything else):
   the instant an owner spawns it must arm `queue wait <its lane>` in the FOREGROUND — STARTING the loop
   is its first action; an owner that spawns, reads the plan, and ends its first turn without arming the
   wait or claiming a ticket is the post-spawn-dormancy anti-pattern (idle with pending work until nudged).
   If it flags a role's lane as missing, run `leadkit
   team standup --scaffold --confirm` to create the missing `queue/<role>/{pending,in-progress,done}`
   dirs (idempotent, confined under queue_root) so the owner can `queue wait` on them right away. Each owner idles at zero token cost in `queue wait`
   and self-pulls its next ticket — **the lead DISPATCHES (`leadkit queue assign <ticket#> <role>`) and
   arbitrates; it does NOT spawn a fresh agent per task.** Spawning one agent per task is the failure
   mode that leaves a full queue with nobody consuming it (`leadkit doctor`'s `owner-liveness` check
   catches that state after the fact — the goal is to never reach it). **Every owner command runs
   NON-INTERACTIVELY and NEVER blocks:** pass assume-yes / no-input / CI flags (`--yes`, `--no-input`,
   `CI=1`), never a command that can stop at an interactive approval prompt, and run long/heavy commands
   (builds, test suites, deploys) TIME-BOUNDED (`timeout` / `--timeout`) or delegated to the lead's
   background Bash — a blocking or approval-gated call wedges the owner INSIDE the call, unreachable to
   messages (they drain only at a tool boundary the hung call never reaches), so its loop never resumes.
   If a hardened command still hangs or fails, REPORT the exact command to the lead — do not retry or
   sit. `leadkit doctor`'s `frozen-claims` check catches this hung-owner state after the fact (an
   in-progress ticket claimed past `burn.inprogress_stall_window`, default 45m, with no advancement):
   probe the owner (a live one answers), and if silent, `TaskStop` it and reassign the ticket.
7. Enqueue the first task with the command step 2 printed: `leadkit task create --type <role> --unparented --title "<what to do>" --assign <role>`. (`leadkit review send` is for sending FINISHED work to the review gate, not for a first task.)
8. Owners pull from their lane and run their loop — the waker daemon wakes them, nothing is polled by
   hand, and no owner exits after a single task.

## Getting unstuck

Run `leadkit doctor` first — it's read-only, fast, and names the actual problem (a missing path, a
malformed `ROLES` row, a hook that isn't wired) rather than a symptom. For design intent beyond this
page, see `docs/CONFIG-REFERENCE.md` (every configuration key, with its default) and
`docs/design/agent-sandboxing-and-enforcement.md` (the enforcement model and its
limits). `leadkit onboard --check --json`
gives a structured snapshot of exactly where your project stands right now.
