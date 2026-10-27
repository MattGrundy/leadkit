# Known-bad comment corpus: pre-registered criteria

_Committed before any reviewer was run against the corpus, so the expected verdicts below cannot
be adjusted to fit whatever a reviewer happens to say. If a reviewer misses these and the definition
is adopted anyway, that is a decision to argue for in writing, not a reason to edit this file._

## What is being checked

The comment rule (`internal/starter/agents/code-review-agent.md` and `critique-agent.md` at
`2cbc38c`) is in the reviewer definitions, and nothing has shown it changes what a reviewer does.
This corpus is six comments found by hand in this repo, each pinned to the commit it
came from, with the right answer written down in advance.

It tests two things, and the second is the harder:

1. **Does the reviewer flag what is bad?** History, restating the name, a hardcoded count, a claim
   the same change makes false.
2. **Does it leave alone what is good?** The INT/TERM trap comment and the DEADLINE comment are
   short whys that name what breaks. Each caught a real P0. A reviewer that flags them is a
   regression dressed as tidiness, and flagging everything is exactly as useless as flagging
   nothing.

## Expected verdicts

Recorded before any run. `flag` = a reviewer applying the rule should flag the span; `pass` = it
must not. Ids are `case/span` in `corpus-known-bad-comments.yaml`, which pins each to a commit.

| Case | Pinned to | Span | Expected | Kind |
| --- | --- | --- | --- | --- |
| `main-version-block` | `baffba7` `cmd/leadkit/main.go:10-17` | `stale-bump-by-hand` | **flag** | claim the same PR makes false |
| `main-version-block` | | `why-var-not-const` | **pass** | one-line why, names what breaks |
| `main-version-block` | | `history-tail` | **flag** | past-tense account of the old const |
| `install-checksum-narrative` | `bdfda19` `install.sh:69-77` | `history-checksum-bug` | **flag** (arguable) | six-line narrative of a fixed bug |
| `install-trap-why` | `bdfda19` `install.sh:54-58` | `why-trap-exits` | **pass** | two-line why, prevents a P0 regression |
| `hookprobe-deadline` | `9ed95fe` `internal/doctor/hookprobe.go:138-146` | `why-deadline-first` | **pass** | why, prevents a P0 regression |
| `agnostic-ext-restates-name` | `3b58038` `cmd/leadkit/agnostic_docs_test.go:46` | `restates-identifier` | **flag** | restates its own identifier |
| `hardcoded-count` | `3b58038` `cmd/leadkit/agnostic_docs_test.go:5-10` | `hardcoded-73` | **flag** | count nothing checks |

Five flag spans (one arguable) and three pass spans over six cases. `main-version-block` is one
case holding all three categories on purpose: that is how it occurred, and a reviewer has to tell
the three apart inside one block rather than be handed them separately.

### Pinning

Only the `Version` block is in `baffba7` (PR #7). The checksum narrative and the INT/TERM comment
were added by the follow-up fix commit `bdfda19`; at `baffba7` the trap is a single uncommented line
and the checksum block is two lines. Those two are pinned to `bdfda19`, because that is where the
text exists. The DEADLINE comment is pinned to `9ed95fe`, the merge of PR #5 to main. It is a
five-line why; only the trap comment is two.

### Arguable

`history-checksum-bug` is marked arguable. Its first sentence (extract the line before piping,
because `/bin/sh` has no `pipefail`) is a durable why; the remainder is past tense. Flagging the
narrative while keeping that sentence is correct. Missing it is reported, and does not fail the run.

### Not scored

Text inside a case but outside every span: the first sentence of the `Version` comment, the code
lines, the rest of the sentence around the count. A reviewer is free to say anything about them.

## The decision rule

A reviewer definition passes only if it clears **all three**:

1. **No pass span is flagged.** Hard failure, never arguable. This is the criterion that keeps
   "flag everything" from scoring as a success.
2. **Every non-arguable flag span is flagged.**
3. **Every span is answered exactly once.** A missing answer is a crash, not a verdict, and is
   reported apart from both kinds of error. A span answered twice is a failure too: no answer for it
   is scored, because choosing between them is choosing the verdict.

A missed arguable span is reported and does not fail the run.

Both degenerate baselines must fail the rule, and `evals/tests/test_scoring.py` asserts that they do: flag
everything fails criterion 1, flag nothing fails criterion 2. If either ever passed, the rule would
not be a rule.

## Protocol

- **The reviewer sees `text` and the rubric, and nothing else.** Not `verdict`, `category`, `note`,
  the span ids, or the case ids, all of which name the answer. The task builds the
  neutral input (`c1.s1`, `c1.s2`, ...) and answers come back keyed the same way. The reviewer is the
  real agent definition, run through `claude -p --agents` at a pinned sha (`evals/README.md`).
- **Three epochs, majority taken, and the spread reported.** One run is an anecdote. A run that
  answers a span twice, skips one, or dies is INVALID and fails the suite; it is never outvoted. Every span that
  did not get the same answer on all three runs is listed as unstable, because a reviewer that
  flips on the INT/TERM comment is not safe even if it wins the vote.
- **A control arm.** Run the same corpus under the PRE-rule definition (`code-review-agent.md` at
  `19d867d`, which asks for comment density judged against neighbouring files and names restatement, hardcoded counts and history). If
  the control scores the same, the rule change did nothing and the corpus has shown it. Without the
  control, a pass cannot be attributed to the change.
- **Report failures per span, in both directions**, not an aggregate.

## A tension recorded before running

The rule reads "Why: one line, stating what breaks if the next person changes this", and the
critique definition adds that "a multi-line why is out of proportion to the one line". The two pass
spans are two lines and five lines. The gold labels say they pass, because they name what breaks and
each caught a real P0. A reviewer applying the rule literally could flag them for length.

If it does, that is a finding about the RULE and not a reviewer error to be scored away: the fix is
an edit to the definition so that a why which names a breakage is not penalised for needing a second
line. Editing the gold would be the move this file exists to prevent.

## What running this would not show

Six comments is a smoke test for a definition, not a measurement of a rate. A pass says the rule
is not inert and is not indiscriminate on these cases. It says nothing about comments unlike them.
