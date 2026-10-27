<!-- leadkit: GENERATED STARTER PROTOCOL — replace with your team's own -->

# Team protocol

How a leadkit team coordinates. This is a STARTER, written by leadkit rather than by your
team: short on purpose, and meant to be replaced once your team's actual conventions
differ from it. The marker on the first line records that leadkit generated this file rather than your
team; delete that line when you take ownership.

## The queue is the substrate

Work lives on disk, never only in a conversation. Each item is a markdown file in the flat
store with frontmatter for status and owner; the queue is a directory per role per state
that symlinks into it.

This is not bureaucracy. It is what makes the team survivable: an agent can die mid-task
without losing anything, `ls` answers "what is everyone doing" without asking an agent to
describe itself, and a handoff is a file rather than a summary of a summary.

**Do not run message-driven.** Chat between agents is for nudges about work that already
exists in the queue. Work that exists only as a message is invisible to the waker daemon,
to the board, and to `audit` — from disk it looks exactly like nothing happening.

## Roles

- **Lead** decomposes and does not implement. The moment it edits code it acquires the
  same context-crowding problem as a single agent, and the decomposition is the first
  thing to degrade because it is the least urgent thing in the window.
- **An overarching lead**, where one coordinates several leads, relays decisions and
  priorities up and down but never authorizes a privileged mutation (cluster actions,
  pushes, PR-opens, merges, config or authority grants). A relay is not authorization:
  those stay human-direct.
- **Builders** own paths, not tickets. A builder working outside its paths is producing
  work nobody reviewed against a spec, which is the main way two agents silently undo
  each other.
- **Reviewers** are two roles with two different questions, not one role twice.
  Code-review asks whether the change is correct. Critique asks whether it should exist in
  this form. Two reviewers asking the same question in different words are one reviewer
  billed twice.

## The gate

A diff reaches a human only after both reviewer layers record `ready` against the commit
SHA. Four properties matter:

- **The layers are SEQUENTIAL, not parallel.** Code review goes first. Critique runs only
  once code review has recorded `ready`. This is enforced at record time, and the
  enforcement is deliberately asymmetric: only a critique `ready` is refused while code
  review is unsatisfied. A critique `changes-needed` is always recordable, because a blocking finding
  must never be gated behind the chain it is blocking. **Still do not spawn both at once.**
  While code review has findings the diff is going to change, so a critique of it judges code
  that will not exist, and fixing those findings produces a new SHA that orphans whatever
  critique recorded against the old one. A parallel critique is therefore discarded twice: its
  subject changes, and its record stops applying.
- **Recorded against the SHA, not the conversation.** Push a new commit and the previous
  approval does not travel with it, or "it was reviewed" quietly degrades into "an earlier
  version was reviewed". This is also why the sequence matters: fixing a code-review finding
  produces a new SHA, which orphans any verdict recorded against the old one.
- **Role independence.** One agent cannot record both layers. This is enforced, and it is
  why a reviewer pool exists rather than a self-sign-off.
- **Dissent supersedes.** If one layer passes and the other objects, the objection wins
  rather than triggering a tiebreak. Tiebreaks between agents are cheap to generate and
  mean little.
- **A `ready` says on what basis.** A `ready` is refused unless it carries `--mutants` (each
  mutant the reviewer applied and the test that failed for it) or `--no-mutant` (why none
  applies, as for a comment-only diff). The text is stored beside the verdict and `gate show`
  prints it. It is evidence and not a gate input: the gate still asks only whether both
  layers recorded `ready`. It is also free text under a self-asserted name, so it shows what
  a reviewer claims, not that the claim is true. A record from before the field existed shows
  "not recorded", which is not the same as "no mutants".

The order is not arbitrary. Code review asks whether the code is correct, which is cheaper
to answer and more likely to force a change. Critique asks whether the change should exist
in this form, which is the more expensive question and the one worth asking about code that
has stopped moving.

## Pushing

Use the governed path (`leadkit push`), which re-checks the gate in-process against the
exact SHA. A raw `git push` is checked by the push-gate hook, and denied when the target
is in scope and ungated.

Note what that does and does not guarantee: the guards are client-side. A machine where
the hooks are not wired has no guards, and an unwired guard is indistinguishable from one
that passes. `leadkit doctor` probes them rather than reading config, so run it. For
anything that genuinely matters, the enforceable control is branch protection on the forge.

## One consumer, one name

Every consumer of a lane (above all each member of a reviewer pool) is spawned with its own
DISTINCT `CLAUDE_AGENT_NAME` (`cr-1`, `cr-2`, `crit-1`, and so on). That one name is what its
`queue claim`, its `gate record` and its `gate verdict` all carry: while it is set, `-by` can
be left off, and any other name is refused. Without it, each call is told its name separately,
the names drift apart, and the independence guard then trips on the mismatch instead of on a
real collision.

The consumer that claimed a ticket runs its OWN `gate verdict` for it, as that same identity.
The lead verdicting from its own shell is the anti-pattern: the lead is not the claimant, so
while the lead's own name is set it is refused, and impersonating the claimant with `-by`
would put a reviewer's name on a verdict the reviewer never wrote.

Be clear what this guards. The independence check refuses a REPEAT under the same role name
(a `cr-*` consumer recording the second layer after the first). The name is the caller's own
environment variable, so that holds against accident, not intent: a process can export a
different `CLAUDE_AGENT_NAME` and record the other layer. A name the harness assigns, rather
than the caller, is the real fix (ticket 309).

## Waiting is free; a timeout is not an ending

`leadkit queue wait [-by <name>] <lane> <timeout> <interval>` is a BLOCKING call. One tool
call covers the whole wait and consumes nothing while it blocks: a 20-second wait costs 20
seconds of wall time and one call, not twenty. Pass `-by` with your own name so a ticket
another owner holds a live claim on does not wake you.

So arm it LONG, `leadkit queue wait <lane> 36000 10`, and treat a TIMEOUT as a signal to
re-arm rather than to stop. An empty lane is not a reason to end; holding a long wait on
one is the designed behaviour. Where the harness caps a single call below that and returns
early, the cap is the same signal: re-arm when the call returns.

The reasoning matters, because the obvious assumption points the other way. Waiting looks
expensive, as if an idle agent were burning its budget, and that suggests short timeouts
and spawning workers on demand instead of keeping them standing. It is not expensive. A
worker that arms a wait of a few minutes and reads the timeout as permission to stop has
thrown away its own life for nothing, and the next item then finds no one standing. Nobody
should kill another agent's wait either: the wait IS that agent's idle loop, and removing it
forces the next death.

## Reporting is not a reason to end

The gate roles have no `SendMessage`, deliberately, so they do not chatter. A handback is
therefore not their reporting channel: **the ticket file is.** Append findings to the ticket,
route it with the role's verb, and carry on. The lead reads tickets.

The trap is the feeling that something must be said, and that ending is the only way to say
it. It never is: whatever the agent wanted to hand back is already on the ticket, so ending
costs the agent its place in the pool and tells the reader nothing the ticket did not.

End only if genuinely blocked AND the block cannot be recorded on a ticket. If you do end,
say what you leave behind: a wait that is still armed and will resume nothing looks, from
outside, exactly like a healthy one.

## Behind main is the normal state

A branch being behind main when it merges is expected, not a fault. Main moves while review
happens. With a dozen branches open, requiring each to be current means every merge
invalidates every other branch: O(n squared) rebases and re-gates for n branches, and nothing
ships.

The reasoning is that integration is the merge commit's job, and CI can test it. A workflow
that triggers on `pull_request` and passes no `ref:` to its checkout builds the merge ref,
which is the merged result against main rather than the branch tip. A gate record whose
`base` is an ancestor of main therefore says only that main moved, which is not a defect.

That claim has a precondition, and it is the part to check: the merged result was tested
against the main of **when the check ran**. A `pull_request` run is not re-triggered when
the base branch moves, so after another PR merges, this PR's green check still describes
"this branch merged with the OLD main", and the forge's "mergeable" flag is a textual
conflict test, not a build. A change that is green alone and red together (a function one
branch renamed that the other calls) gets through exactly that gap. It is closed where the
forge requires the branch to be up to date or merges through a queue; where it is not
available, the gap is yours to judge, as follows.

A re-run of the check is not the remedy. A forge re-run reuses the merge commit computed when the
PR was opened or last pushed, so it tests the OLD main again however far main has moved: measured on
a `pull_request` run, the second attempt checked out the identical merge commit as the first. The
only step that tests the new merge is a new head SHA, and that orphans the gate. So decide by
overlap, not by age. When main has moved since the last check, compare what changed on main with
what the change touches (`git diff --name-only $(git merge-base origin/main <head>) origin/main`
against the change's own files). No overlap and nothing main changed that this change calls: merge as
it is, the residual risk is the one every merge carries, and the net under it is the CI run on
the target branch after the merge (a red main is fixed forward or reverted); a project whose CI has
no such trigger has no net, so confirm yours. Overlap, or main changed something this
change calls: update the branch (a new SHA) and re-gate it. Closing and reopening the PR may
recompute the merge ref without a new SHA, but that is unverified here and nothing relies on it.

So **do not hold, rebase or re-gate because a gate record's `base` is behind main.** Hold
when:

- a layer is not `ready`, or the two layers' `by` values match;
- the forge reports the PR as not cleanly mergeable, or a check fails. That is the real conflict
  test and the only thing that should trigger a rebase by itself, and then the new SHA is
  re-gated;
- main has moved since the last check onto files this change touches, or onto something it
  calls, per the overlap test above. That also needs the branch updated and re-gated, because
  nothing else tests the merged result;
- **the branch carries commits belonging to another change.** No conflict check catches
  this: git merges cleanly and CI goes green, because the code is fine. What is missing is
  that nobody reviewed those commits under this change's gate. Read
  `git log origin/main..<head> --oneline`; where subjects carry a ticket id, a range
  carrying another ticket's id is contamination until shown otherwise.

Keep recording `--base` on a `ready`, and record the commit you actually read from. It
documents what the reviewers read, which is how a contaminated branch is found. For a stacked
change it is also more than evidence: it is what lets the lineage check count the commits beneath
the head as reviewed, so a base further back than what was read vouches for commits nobody read.

## Done means verified

A claim of completion cites something checkable: a merged commit on the target branch, a
test that covers the behaviour, a recorded verdict. "Done" asserted by the agent that did
the work is not evidence, and an autonomous team will declare itself finished while work is
open because nothing contradicts it.
