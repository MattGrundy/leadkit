"""The Inspect-facing half: a Scorer that validates each run, a Reducer that turns epochs into a
majority without absorbing a bad run, and a metric that applies the pre-registered rule.

Inspect's own `majority` reducer is used for the vote (a panel member that fails to produce a value
withholds a vote without lowering the bar). What it cannot know is that an invalid or MISSING epoch
must fail the suite, so the reducer in front of it checks that.
"""

from __future__ import annotations

import json
import math
from typing import Any

from inspect_ai.scorer import (
    SampleScore, Score, Target, metric, score_reducer, scorer,
)
# PRIVATE import: Inspect exposes no public `majority` reducer by name here. Safe only because
# pyproject.toml pins inspect-ai exactly; a bump must re-check this path (test_task.py exercises it).
from inspect_ai.scorer._reducer.reducer import majority_score
from inspect_ai.solver import TaskState

from .solver import input_tokens
from .scoring import Problems, check_answers, judge

NAN = float("nan")


def _expected_keys(target: Target) -> dict[str, dict]:
    return json.loads(target.text)


@scorer(metrics=[])
def span_answers():
    """Score ONE run (one epoch of one sample): validate the answer set against the span keys the
    sample asked about. A valid run scores each span 1.0 (flagged) or 0.0 (left alone). An invalid
    one - a failed claude run, a duplicated key, a skipped span, a stray key - scores every span
    NaN and says why; it never becomes a smaller or a kinder vote."""

    async def score(state: TaskState, target: Target) -> Score:
        gold = _expected_keys(target)
        result = state.store.get("result") or {}
        if "error" in result:
            answers, problems = {}, Problems(malformed=[result["error"]])
        else:
            answers, problems = check_answers(gold.keys(), result.get("answers"))
        valid = not problems
        value = {k: (1.0 if answers[k] else 0.0) if valid else NAN for k in gold}
        usage = result.get("usage") or {}
        return Score(
            value=value,
            answer=json.dumps(result.get("answers")) if "answers" in result else None,
            explanation="valid" if valid else f"INVALID RUN - {problems.describe()}",
            metadata={
                "valid": valid,
                "problems": problems.asdict(),
                "gold": gold,
                "cost_usd": result.get("cost_usd"),
                "duration_ms": result.get("duration_ms"),
                "input_tokens": input_tokens(usage) if usage else None,
                "output_tokens": usage.get("output_tokens"),
            },
        )

    return score


@score_reducer(name="strict_majority")
def strict_majority(epochs: int = 3):
    """Majority over epochs, failing closed. The reduced score is invalid if ANY epoch was invalid
    or if fewer epochs reported than were requested: a run that skipped or double-answered a span
    must not be covered for by the others. Spans whose epochs did not all agree are listed in
    metadata as unstable, because a verdict that flips between identical runs is a weak one."""
    if epochs < 1 or epochs % 2 == 0:
        raise ValueError(f"need an odd number of epochs to take a majority, got {epochs}")
    vote = majority_score()

    def reduce(scores: list[Score]) -> Score:
        first = scores[0]
        gold = (first.metadata or {}).get("gold", {})
        invalid = [i for i, s in enumerate(scores) if not (s.metadata or {}).get("valid")]
        missing = max(0, epochs - len(scores))
        if invalid or missing:
            why = []
            if invalid:
                why.append("epoch(s) " + ", ".join(str(i + 1) for i in invalid) + " invalid: " +
                           "; ".join(f"[{(scores[i].explanation or '')}]" for i in invalid))
            if missing:
                why.append(f"{missing} of {epochs} epochs reported no score")
            return Score(
                value={k: NAN for k in gold},
                explanation="INVALID - " + "; ".join(why),
                metadata={
                    "valid": False, "gold": gold, "unstable": [], "epochs": len(scores), **_cost(scores),
                },
            )
        reduced = vote(scores)
        unstable = sorted(
            k for k in gold
            if len({s.value[k] for s in scores}) > 1  # type: ignore[index]
        )
        return Score(
            value=reduced.value,
            explanation=reduced.explanation,
            metadata={
                "valid": True, "gold": gold, "unstable": unstable, "epochs": len(scores), **_cost(scores),
            },
        )

    return reduce


def _cost(scores: list[Score]) -> dict[str, float | None]:
    """Spend is recorded even for a run that is thrown out: it was still spent."""
    return {f: _sum(scores, f) for f in ("cost_usd", "input_tokens", "output_tokens", "duration_ms")}


def _sum(scores: list[Score], field: str) -> float | None:
    vals = [(s.metadata or {}).get(field) for s in scores]
    vals = [v for v in vals if isinstance(v, (int, float))]
    return float(sum(vals)) if vals else None


@metric
def suite_verdict(gold: dict[str, dict]):
    """Apply the pre-registered rule over the whole suite. `gold` is EVERY span in the corpus, so
    a sample that produced no score at all shows up as unanswered instead of vanishing."""

    def compute(scores: list[SampleScore]) -> dict[str, float]:
        answers: dict[str, bool | None] = {}
        unstable: list[str] = []
        cost = {"cost_usd": 0.0, "input_tokens": 0.0, "output_tokens": 0.0, "duration_ms": 0.0}
        for ss in scores:
            md = ss.score.metadata or {}
            for f in cost:
                if isinstance(md.get(f), (int, float)):
                    cost[f] += float(md[f])
            value = ss.score.value if isinstance(ss.score.value, dict) else {}
            for k, v in value.items():
                if not md.get("valid") or (isinstance(v, float) and math.isnan(v)):
                    answers[k] = None
                else:
                    answers[k] = bool(v >= 0.5)
            unstable += md.get("unstable", [])
        report = judge(gold, answers, unstable)
        ok, _ = report.verdict()
        n = lambda o: float(len(report.ids(o)))  # noqa: E731
        return {
            "verdict": 1.0 if ok else 0.0,
            "caught": n("caught"),
            "missed": n("missed"),
            "missed_arguable": n("missed_arguable"),
            "false_flags": n("false_flag"),
            "unanswered": n("unanswered"),
            "unstable": float(len(set(unstable))),
            # "is haiku enough" is a quality-per-token question; summed over every epoch
            **cost,
        }

    return compute
