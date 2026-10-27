# evals

Role-fitness suites for leadkit's agent definitions, run under [Inspect](https://inspect.aisi.org.uk).
A suite is DATA (`<suite>/CRITERIA.md` plus a corpus YAML) and a thin `task.py`. Everything that
validates, scores or runs lives once in `leadkit_eval/`.

## Setup

Python 3.11+. Inspect is pinned exactly (`pyproject.toml`; the full set in `requirements.lock`),
because a harness that moves under a suite invalidates every comparison made across the move.

    python3 -m venv .venv && .venv/bin/pip install -r evals/requirements.lock && .venv/bin/pip install -e 'evals[dev]'
    .venv/bin/python -m pytest evals          # or: make eval-test

## Run a suite

    inspect eval evals/comments/task.py --model mockllm/model --log-dir evals/logs/comments \
        -T definition_sha=<sha> -T model=sonnet -T effort=high
    python -m leadkit_eval.report evals/logs/comments/<log>.eval     # exit 0 only on PASS

`--model mockllm/model` only satisfies Inspect. The Solver runs the REAL agent definition, read from
git at `definition_sha`, through `claude -p --agents ... --agent ... --json-schema ...`, so what is
scored is the file that ships and not a reimplementation of it. `claude` must be authenticated.

A result is a tuple, recorded in the log: **(suite, definition sha, model, effort)**. Change exactly
one to attribute a difference. The control arm is the same command with only `definition_sha` changed
(for comments: `19d867d` before the rule, `fc226be` after). Holding the definition and varying `model`
answers "is a smaller model enough". Spend per run (USD, tokens, wall clock, summed over epochs) is a
metric, because that question is quality per token.

## Design rules

- **Pre-registration by commit order.** `CRITERIA.md` and the corpus are committed BEFORE the scorer
  that applies them. A new suite does the same. A squash-merge collapses that order into one commit,
  so a PR adding or changing a suite is merged with a merge commit.
- **Gold labels in three places on purpose**: the corpus, `CRITERIA.md`, and the table in
  `tests/test_corpus.py`. Editing a label means editing all three, which is what stops an eval
  adjusting its answers to match a disappointing result. Do not dedupe this.
- **Blind keys.** The reviewer sees `c1.s1`, never a case id, span id, verdict, category or note.
- **Asymmetric rule.** A flag on a PASS span is never excused; a missed ARGUABLE span is reported and
  does not fail.
- **Baselines.** Flag-everything and flag-nothing must both fail (`tests/test_scoring.py`).
- **A run is valid or it fails the suite.** An answer set that names a span twice, skips one, names a
  stray key, or comes from a `claude` that died is INVALID. It is never resolved in the reviewer's
  favour and never outvoted by the other epochs. Inspect does NOT do this for you: with
  `--no-fail-on-error` it finishes "success" having reduced two scores where three were asked for
  (pinned in `tests/test_task.py`), and a run whose every sample failed has no results at all, which
  `leadkit_eval.report` calls a FAIL.

## A suite measures what its corpus contains

The corpus was built from defects found by hand, so they measure "catches what we already know
about". That is useful for regression and for model comparison and is NOT a measure of finding the
unknown. Do not read a pass as a competence score.

## What the tests do not show

The tests drive the harness with a fake `claude` that answers from the gold labels. They show that
the harness scores, rejects invalid runs and hides the labels. They do not show that the comment rule
changes how a reviewer behaves, and no real reviewer run is recorded here: that needs the `inspect
eval` command above against an authenticated `claude`, once with each definition sha.
