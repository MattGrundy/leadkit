# Design — agent sandboxing & enforcement (layered, configurable, forge-anchored)

Status: DESIGN. Layers 0, 1 and 3 are built; Layer 2 (containerization) is unbuilt and sequenced last.

## 0. The threat model + the core insight

**The threat model:** leadkit's guards are client-side and opt-in, so on a machine where they are not wired they are simply **absent** — and an agent there can push to a protected branch with nothing in the client to stop it. Treat an unguarded or misconfigured client as the expected case, not the exception.

**Insight that must anchor the whole design:** leadkit's guards are CLIENT-SIDE PreToolUse hooks — advisory, opt-in, fail-open, and *absent* on an unconfigured machine. A guard that lives in the client is a guard the client can route around, so no amount of client-side sandboxing definitively prevents an unauthorised push to a protected branch. **The only bypass-proof layer for "don't push to a protected branch" is the FORGE (GitHub/GitLab) rejecting the push server-side.** Client sandboxing is real and valuable defense-in-depth for the OTHER goals (cross-project interference, context governance, least privilege) — but an unauthorised push to a protected branch is a forge-protection gap first.

## 1. Goals

1. **Layered + a configured option** — adopters/projects pick their enforcement level; not one-size.
2. **Cross-platform, minimal effort** — must work on macOS + Linux with low setup cost.
3. **Three concerns:** (a) cross-project interference (accidental), (b) governance of context (what an agent can see/reach), (c) SECURITY (the bypass threat).
4. **Containerization with "safe" paths mounted** — each agent in a container, with curated mounts = its own project + shared safe spaces (the cross-agent brief/routing store, the account registry).
5. **Least-privilege toolsets** — reduce each agent's tool set to the minimum for its role.
6. **Keep cross-project work** — it cut token usage significantly; governed cross-project flows (briefs, governed cross-repo pushes) MUST survive.
7. **Boundary = whatever is most sensible for least disruption.**

## 2. The layers (each an independently-adoptable, configured level)

Ordered by bypass-resistance + effort. An adopter enables the levels they want via config; the recommended BASELINE is Layer 0 always.

### Layer 0 — FORGE-SIDE branch protection (the fix for the threat; bypass-proof)
The definitive answer to "an agent must not push to release/* without permission." Protected branches on the git host require PR + review + green checks; direct/force pushes are REJECTED server-side, regardless of the client.
- leadkit CANNOT set another org's protection unilaterally, but it can: (a) `leadkit forge protect` — a helper that configures protected-branch rules via `gh api`/`glab` for the repos an adopter owns (required PR, required checks incl. the review-gate, no direct push to `main|master|develop|release/*`, no force-push); (b) a `doctor` check `forge-branch-protection` that VERIFIES the remote's protected branches are actually enforced server-side, and WARNs "your protected branches rely only on client-side guards — a client that bypasses the hook can still push; enable forge protection." So an adopter learns their guards are advisory, not real.
- Cross-platform (a forge API call). Minimal effort. HIGHEST security value. **Recommend building first.**
- **Built.** `leadkit forge protect` (dry-run by default; `--apply --confirm` writes; refuses without an `authority.<repo>` entry; reads the branch back and fails unless the forge now reports it verified and nothing the branch had before the write is gone) and the `forge-branch-protection` doctor check. A branch counts as verified only when the forge affirmatively reports: pull request required, force-push and deletion blocked, admins bound (GitHub `enforce_admins`; GitLab push access "No one"). A state leadkit could not read is reported as CANNOT VERIFY, never as verified. Branches come from `authority.<repo>.protected_branches`, else the repo's default branch. Not covered: GitHub repository rulesets (reported as cannot-verify), wildcard branch patterns, and GitLab pipeline-success gating. A forge plan that cannot enforce protection (a private repo on GitHub Free) reports `unenforceable`; `forge protect` refuses there.
- **House rule: show a diff before touching a guard.** GitHub's `PUT .../protection` REPLACES the whole rule, so a fixed body silently resets anything it omits (approvals, code-owner review, status checks, push restrictions). `forge protect` therefore reads the current rule, sends it back with only the standard overlaid, and prints, on the dry-run as well as under `--confirm`, what the branch is protected by now and every change in words, removals first (`- REMOVES:`, `~ CHANGES:`, `+ ADDS:`), with a loud count when anything would be lost. The diff is computed from the bytes about to be sent, not from the merge that built them. A write that would remove any protection is refused even under `--confirm`; there is no override flag, weakening a branch is done by hand on the forge. Properties the rule model cannot carry through a PUT (e.g. `required_signatures`) are refused the same way rather than risked. Any later verb that writes outward-facing policy (forge settings and the like) inherits this: a human-readable before/after diff first, a refusal on anything weakening, and a read-back that checks what was NOT set as well as what was. Not verified against a live forge: the GitHub write was exercised only against a fake that replaces the rule on PUT, and the GitLab push-level PATCH shape (`allowed_to_push` with `_destroy`) is as documented for Premium/Ultimate and unconfirmed on Free; a rejection there fails loudly. `forge protect` also refuses a GitLab branch whose weak wildcard rule would keep it open, since writing the exact rule cannot close that.

### Layer 1 — LEAST-PRIVILEGE tool allow-lists (per role)
Reduce each agent's Claude Code tool set to the minimum for its role: a reviewer = read-only (Read/Grep/Glob, no Edit/Write/Bash-push); a builder = Edit/Write/Bash scoped to its worktree; a poller = read-only. leadkit already knows roles (code-review/critique/builder/…); it can DEFINE per-role tool profiles and the team standup/scaffold + agent-def generation APPLY them.
- An agent that lacks the Bash/push tool CANNOT push at all — a strong, simple constraint that closes this on the tool axis (a reviewer/agent with no push capability can't push a release branch).
- Cross-platform (Claude Code agent config). Low effort. High value. **Recommend building early, alongside Layer 0.**

### Layer 2 — CONTAINERIZATION with safe mounts (strongest fs/network isolation)
Each agent runs in a container (Docker/Podman — cross-platform) whose mounts are curated: its own project tree (rw), the shared SAFE spaces (the brief/routing store `~/.claude/leadkit/routing`, the presence/registry, read-only shared refs) — and NOTHING else. The agent physically cannot read/write another project's tree or arbitrary host paths; network egress can be constrained too.
- Preserves governed cross-project work by MOUNTING the shared coordination spaces (briefs cross projects via the mounted routing store; a governed cross-repo push targets a repo whose clone is mounted when that work is assigned) — cross-project stays possible through the declared safe mounts, not ambient host access.
- leadkit generates the container profile per agent/role (the mount set + tool set + network policy). Configurable per project/adopter.
- Cross-platform via Docker/Podman; HIGHER effort (container lifecycle, image, mount orchestration). **Sequence after 0/1; the opt-in strong-isolation tier.**

### Layer 3 — FAIL-CLOSED client guards (defense-in-depth for the tool-call path) — BUILT
Flip the security-relevant PreToolUse guards (push-guard, push-gate, owner-guard) from fail-OPEN to fail-CLOSED (deny on error/unparseable/unverifiable) and harden the bypass surface (we already fixed several parse gaps). Still client-side + bypassable if the hook isn't wired — so it is DEFENSE-IN-DEPTH, never the primary control. A `configured option`: an adopter can choose fail-closed for the security guards while keeping advisory for the convenience ones.
- Cross-platform (it's leadkit's own hooks). Low effort. Complements, does not replace, Layer 0. **Fold in with the enforcement config.**

**STATUS: BUILT.** Each of the three security guards has a per-guard enforcement MODE in the profile — `guards.<name>.mode: advisory | enforce` plus a `guards.default_mode` fallback, **default advisory** (backward-compatible: no behavior change until an adopter opts in, and it's one line — `guards: {default_mode: enforce}` — to flip all three). `advisory` = today's posture exactly (a clear violation denies; an error/unverifiable condition fails OPEN). `enforce` = fail-CLOSED (an error / unparseable / unresolvable-target / unreadable-policy condition DENIES, authoritative). ONLY the ambiguous/unverifiable path flips — a clear positive allow still allows and a clear violation still denies in both modes. The mode is resolved from the **TARGET repo/file's governing profile** (the same guards-resolve-config-from-target resolution the whitelist/gate/owners policy use), with `default_mode` fallback, unset → advisory. Each deny names its mode, and `leadkit doctor`'s `guard-enforcement` check reports the effective posture (Info when all enforce; a gentle Warn — never a hard error — while a security guard is still advisory), so the "are my guards actually enforcing?" question is answerable. Per-guard: push-guard was already fail-closed on its unverifiable paths (enforce makes the posture explicit + named); push-gate now fail-closes an unevaluable push (bad repo / unresolvable HEAD / unresolvable scope) under enforce; owner-guard now fail-closes a CONFIGURED-but-unreadable owners policy under enforce (a project with no `owners_file` has no boundary and is unaffected either way). See the config reference (`docs/CONFIG-REFERENCE.md`, the `guards` block).

## 3. The configured model
A `sandbox:`/`enforcement:` config block selects levels per project (and a sensible default): e.g. `forge_protection: verify|require`, `tool_least_privilege: on`, `containerize: off|podman|docker`, `guards: advisory|fail_closed`. `leadkit doctor` reports the EFFECTIVE enforcement posture (what's actually enforced vs advisory) so an adopter sees the real picture — the "your release branch is only client-guarded" WARN is the headline. Boundary defaults to the PROJECT (least disruption), with the shared safe mounts making cross-project governed work continue.

## 4. Cross-platform strategy (minimal effort)
- Layer 0 (forge): a forge API call — inherently cross-platform.
- Layer 1 (tools): Claude Code agent config — cross-platform.
- Layer 2 (containers): Docker OR Podman (detect + support both); the mount/tool/network profile is generated, the runtime is the adopter's. macOS + Linux both run Docker/Podman. Avoid OS-specific sandboxes (sandbox-exec is macOS-only) as the PRIMARY mechanism — containers give cross-platform coverage for one design.
- Layer 3 (guards): leadkit's own Go hooks — cross-platform.

## 5. Preserving governed cross-project work (the non-negotiable)
Every layer keeps cross-project flows that reduced token usage:
- Briefs/routing cross projects via the SHARED mounted store (Layer 2) + they're read-only-safe data, not code execution.
- A governed cross-repo PUSH/PR: Layer 0 REQUIRES the PR flow (which is how a cross-project change should land anyway — the unsafe shape is a DIRECT push, exactly what Layer 0 blocks); Layer 1 gives the push capability only to roles that should have it; Layer 2 mounts the target clone when that work is assigned. Cross-project stays possible, just GOVERNED (PR + capability + declared mount) instead of ambient.

## 6. Sequencing (recommendation)
1. **Layer 0 (forge protection helper + doctor verify) + Layer 1 (per-role least-privilege tools)** — FIRST. Highest security-per-effort, cross-platform, directly addresses the threat (bypass-proof server-side + no-push-capability for wrong roles), and low disruption. Ship these two, measure.
2. **Layer 3 (fail-closed security guards + the enforcement config block)** — SECOND. Low effort, hardens the client path, introduces the `enforcement:` config + doctor posture report.
3. **Layer 2 (containerization with safe mounts)** — the strong-isolation opt-in tier, sequenced last (highest effort); design the mount/tool/network profile generation once 0/1/3 are in and the safe-path set is proven.

## 7. Open questions
- **Q-A (forge scope):** which repos/orgs should `leadkit forge protect` be allowed to configure — only ones the adopter owns, and is that an owner-direct authority grant like the PR authority (leadkit never sets another org's protection without an explicit grant)?
- **Q-B (container runtime):** Docker, Podman, or both to support first? Any adopter constraint (rootless Podman on Linux CI vs Docker Desktop on macOS)?
- **Q-C (least-privilege baseline):** start from a curated per-role tool matrix (reviewer=read-only, builder=scoped-write, poller=read-only, lead=full) — confirm the matrix, or should it be per-adopter configurable from day one?
- **Q-D (enforcement default):** for a NEW adopter, what's the default posture — advisory (today), or fail-closed-security-guards + forge-verify on by default (stricter out of the box)?

## 8. Risks / honest caveats
- Layer 0 depends on the ADOPTER enabling forge protection on THEIR repos — leadkit can help + verify + nag, but cannot force another org's server settings. The doctor WARN makes the gap visible; closing it is the repo owner's action. (This is the honest limit: the gap lives on machines leadkit does not control — the durable fix is the FORGE, which leadkit can only configure where it is granted access.)
- Containerization (Layer 2) adds real operational weight (images, lifecycle, mount orchestration) and could disrupt the fast agent-spawn flow — hence sequenced last + opt-in.
- Over-tightening tool least-privilege or mounts could break a legitimate governed cross-project action — the safe-mount set + role tool matrix must be validated against the real flows (briefs, cross-repo PRs) before enforcing.
