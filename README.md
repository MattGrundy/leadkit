# leadkit

A portable operating model for running teams of Claude Code agents on real work: one Go
binary, a per-project profile (`leadkit.yaml`), and a set of guard hooks that enforce policy at the
tool boundary rather than in a prompt.

It is a CLI and a config file, not a library. Projects adopt it by writing a profile and
running a few commands.

## Why it exists

A single coding agent handles a long-running, multi-repository job badly, and it fails
quietly. Context fills with whatever it touched last, so the twentieth change gets less
care than the second, and decisions settled on Tuesday get re-litigated on Thursday.
Every individual step reports success, because every step did succeed. Only the shape of
the whole is wrong.

leadkit is the least structure I found that lets several agents work one codebase for
months without the result going incoherent:

- Work lives on disk, not in a conversation. Each item is a markdown file with
  frontmatter; a queue of per-role directories is laid over it. An agent can die
  mid-task without losing anything, and `ls` answers "what is everyone doing" without
  asking an agent to describe itself.
- A lead decomposes and does not implement. The moment it edits code it acquires the same
  context-crowding problem as a single agent.
- Two reviewers with genuinely different questions gate a diff before a human sees it.
  One reads line by line for correctness; the other reads it against the stated goal.
  A pass is recorded against a commit SHA, so a new push does not inherit an old approval.
- Policy is enforced outside the model, at the point where intent becomes effect. A
  prompt instruction is a suggestion evaluated by the same process deciding what to do.

## Install

The installer downloads a release binary via `gh`, verifies its checksum and installs it
atomically to `~/.claude/leadkit-bin`:

```
sh install.sh            # latest release
sh install.sh v0.2.0     # a specific tag; LEADKIT_BIN=... overrides the target
```

Or build from source:

```
go install mgpp.dev/leadkit/cmd/leadkit@latest    # Go 1.26 or later
```

Or from a clone, which additionally self-verifies that the installed binary is current:

```
make install
```

Whichever route, run `leadkit` commands from the installed binary rather than a throwaway build:
`leadkit init` bakes its own absolute path into the hooks and daemons it writes.

## Getting started

`docs/ADOPTING-LEADKIT.md` is the source of truth for adoption, and `leadkit onboard`
prints it. The short version:

```
leadkit doctor      # preflight: config, hooks, daemons and queue health
leadkit init        # dry run by default; --apply --confirm to write anything
leadkit status      # what the team is doing, from disk
```

`leadkit init --apply --confirm` writes the hooks and daemons and also installs the
`leadkit-adopt` skill into `~/.claude/skills/`. After it, restart the Claude Code session and
run `/leadkit-adopt` to be walked through adoption.

`leadkit.yaml.example` at the repo root is the example profile to copy. It is inert as named,
because leadkit only loads the exact basename `leadkit.yaml`. It shows the keys that matter,
including the `authority.<repo>` block, which is the part adopters most often get wrong.
`docs/adopter-profiles/per-project-template.leadkit.yaml` is a smaller starting point, and
`docs/CONFIG-REFERENCE.md` (generated from the schema) documents every field.

The profile can live in the repo or outside it:

- **In-repo:** copy the example to `leadkit.yaml` at the project root and commit it. Found
  first.
- **External:** place it at `~/.claude/leadkit/config/<slug>.yaml`, keyed on the repo's origin
  name, so one file governs every clone and worktree. Used when there is no in-repo file.
  Relative paths in an external config anchor to the config file's own directory, not the
  repo, so write `queue_root` and `store_root` as absolute paths there.

Not sure which file to edit? `leadkit config path` prints the exact external and in-repo paths for the
current repo (marked `[exists]` / `[not created]`) and which one is ACTIVE.

`leadkit config init` writes a starter in-repo and `leadkit config init --external` writes one
outside. This repository itself uses the external mode and ships no committed profile, because
its authority config names its owner and carries a merge grant, which should not be in git.

Two things that will make it look broken if missed: the binary must be the installed one,
and Agent Teams must be enabled (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`), without which
the coordination primitives silently do nothing.

## The guards

Each is a `PreToolUse` hook that inspects the real command and returns allow or deny.

| Guard | Intercepts | Enforces |
| --- | --- | --- |
| push-guard | `git push` | Branch allowlist, default-deny |
| push-gate | `git push` | Refuses a push whose HEAD lacks two reviewer passes |
| pr-guard | `gh pr create/edit/merge` | Routes through the governed path that checks per-repo authority |
| owner-guard | file edits | Path-ownership boundaries between agents |
| task-status-guard | task file edits | Only the declared status vocabulary |

Three properties separate a guard from a speed bump, and they are worth copying even if
you do not use this tool: default-deny rather than a blocklist, fail closed when the
guard cannot determine whether something is allowed, and parse the command rather than
pattern-match it.

One caveat stated plainly: these are client-side. A machine where the hooks are not wired
has no guards at all, and an unwired guard is indistinguishable from a passing one. For
anything that genuinely matters, the enforceable control is server-side branch protection
on the forge. `leadkit forge protect` configures it and `leadkit doctor` verifies it, reading
the rule back and refusing to call a branch protected on a state it could not read.
`docs/design/agent-sandboxing-and-enforcement.md` sets out the layered model, what each layer
can and cannot enforce, and which parts are built.

## Tests

```
make test
```

Coverage is worth reading twice over, because neither figure alone is true: in-process
coverage, and coverage from the tests that build and exec the real binary. The second is how
most of the command surface is exercised and is invisible to the first.

## Status

Used daily on real multi-repository work, by one person. Each design document under
`docs/design/` states its own build status at the top; some of what they specify is built
and some is not. The backlog of what is known, shaped and not yet done — including where
the verification is thinner than it should be — is tracked privately rather than here.

## Licence

Copyright 2026 Matt Grundy. GPL-3.0 — see [LICENSE](LICENSE).

What that means in practice, since copyleft is widely misread as viral on use:

- **Using it costs you nothing.** Install it, run it, modify it for your own team.
  Internal use triggers no obligation at all, modified or not.
- **Distributing a modified version** means those changes are GPL too. So you cannot
  take leadkit, improve it privately, and ship the result as a closed product.
- **It is a standalone binary, not a library.** The usual GPL concern about linking into
  proprietary code does not arise: nothing here is importable from outside the module.

AGPL was considered and rejected. Its extra clause covers running a modified version as
a network service, which here would only mean the convenience board in `leadkit serve` —
not the part of this worth protecting. It would have cost adoption (some organisations
ban AGPL mechanically) to guard something nobody would pay for.

Contributions are welcome on those terms.
