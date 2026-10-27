"""The pre-registered decision rule, with no model and no Inspect in it: gold and answers in, a
verdict out. The numbers reproduce from two dicts.

Asymmetric on purpose: a flag on a PASS span is a hard failure, a missed ARGUABLE span is reported
and does not fail. A reviewer that flags everything is as useless as one that flags nothing, and
only the first looks diligent.

Two answer-set shapes are INVALID rather than scored:
  * an answer set that names a span twice, which is not resolved in the reviewer's favour;
  * an answer set that omits a span, or names one that is not in the sample, which is not a
    smaller vote.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping


@dataclass
class Problems:
    duplicate: list[str] = field(default_factory=list)
    missing: list[str] = field(default_factory=list)
    unknown: list[str] = field(default_factory=list)
    malformed: list[str] = field(default_factory=list)

    def __bool__(self) -> bool:
        return bool(self.duplicate or self.missing or self.unknown or self.malformed)

    def describe(self) -> str:
        parts = []
        for name in ("duplicate", "missing", "unknown", "malformed"):
            vals = getattr(self, name)
            if vals:
                parts.append(f"{name}: {', '.join(vals)}")
        return "; ".join(parts)

    def asdict(self) -> dict[str, list[str]]:
        return {k: list(v) for k, v in vars(self).items()}


def check_answers(expected_keys: Iterable[str], answers: Any) -> tuple[dict[str, bool], Problems]:
    """Validate one run's answers against the span keys it was asked about. Returns the answers
    keyed by span (empty when anything is wrong) and what was wrong. Every key must be answered
    exactly once, and nothing else may be."""
    expected = list(expected_keys)
    problems = Problems()
    if not isinstance(answers, list):
        problems.malformed.append("answers is not a list")
        return {}, problems
    seen: dict[str, bool] = {}
    for i, a in enumerate(answers):
        if not isinstance(a, Mapping) or not isinstance(a.get("key"), str) or not isinstance(a.get("flagged"), bool):
            problems.malformed.append(f"entry {i} is not {{key: str, flagged: bool}}")
            continue
        k = a["key"]
        if k in seen:
            if k not in problems.duplicate:
                problems.duplicate.append(k)
            continue
        seen[k] = a["flagged"]
    problems.missing = [k for k in expected if k not in seen]
    problems.unknown = sorted(k for k in seen if k not in set(expected))
    if problems:
        return {}, problems
    return seen, problems


CAUGHT = "caught"
MISSED = "missed"
MISSED_ARGUABLE = "missed_arguable"
FALSE_FLAG = "false_flag"
CORRECT_PASS = "correct_pass"
UNANSWERED = "unanswered"


@dataclass
class Report:
    outcomes: dict[str, str]  # span id -> outcome
    unstable: list[str]

    def ids(self, outcome: str) -> list[str]:
        return [s for s, o in self.outcomes.items() if o == outcome]

    def verdict(self) -> tuple[bool, list[str]]:
        """Apply the rule and return EVERY reason it failed, not the first: "missed one flag" and
        "flagged the P0 catchers" are different conclusions."""
        reasons: list[str] = []
        if ff := self.ids(FALSE_FLAG):
            reasons.append(
                f"flagged {len(ff)} comment(s) that must pass: {', '.join(ff)}"
                " - a rule that strips good whys is a regression, not tidiness"
            )
        if missed := self.ids(MISSED):
            reasons.append(f"missed {len(missed)} known-bad comment(s): {', '.join(missed)}")
        if un := self.ids(UNANSWERED):
            reasons.append(f"{len(un)} span(s) without a valid answer: {', '.join(un)}")
        return not reasons, reasons


def judge(gold: Mapping[str, Mapping[str, Any]], answers: Mapping[str, bool | None], unstable: Iterable[str] = ()) -> Report:
    """gold: key -> {verdict, arguable, span}. answers: key -> flagged, or None for a span with no
    valid answer. A gold span absent from answers counts as unanswered, so a span nobody reported
    on cannot go missing from the result."""
    outcomes: dict[str, str] = {}
    for k, g in gold.items():
        a = answers.get(k)
        if a is None or (isinstance(a, float) and math.isnan(a)):
            o = UNANSWERED
        elif g["verdict"] == "flag":
            o = CAUGHT if a else (MISSED_ARGUABLE if g.get("arguable") else MISSED)
        else:
            o = FALSE_FLAG if a else CORRECT_PASS
        outcomes[g["span"]] = o
    return Report(outcomes, sorted(unstable))


def flag_everything(gold: Mapping[str, Any]) -> dict[str, bool]:
    return {k: True for k in gold}


def flag_nothing(gold: Mapping[str, Any]) -> dict[str, bool]:
    return {k: False for k in gold}
