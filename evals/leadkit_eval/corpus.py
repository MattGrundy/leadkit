"""Corpus parsing, the blind view, and the pin check.

A corpus is a set of CASES, each a verbatim excerpt of this repo pinned to a commit, path and start
line, with labelled SPANS inside it. The gold labels live in the corpus and are restated in the
suite's tests: that duplication is deliberate (editing a label means editing both), so do not
dedupe it.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

import yaml

FLAG = "flag"
PASS = "pass"


class CorpusError(ValueError):
    """A corpus that cannot be scored faithfully."""


@dataclass(frozen=True)
class Span:
    id: str
    lines: tuple[int, int]
    anchor: str
    verdict: str
    category: str = ""
    arguable: bool = False
    note: str = ""


@dataclass(frozen=True)
class Case:
    id: str
    commit: str
    path: str
    start_line: int
    text: str
    spans: tuple[Span, ...]
    provenance: str = ""


@dataclass(frozen=True)
class Corpus:
    question: str
    rubric_version: int
    rubric_source: str
    cases: tuple[Case, ...]
    # set by load(); lets a log say which file produced it
    source: str = field(default="", compare=False)


def span_id(case: Case, span: Span) -> str:
    return f"{case.id}/{span.id}"


def key(case_idx: int, span_idx: int) -> str:
    """The neutral positional name for a span: c1.s1 is the first span of the first case."""
    return f"c{case_idx + 1}.s{span_idx + 1}"


def parse(data: str, source: str = "") -> Corpus:
    raw = yaml.safe_load(data)
    if not isinstance(raw, dict) or not raw.get("cases"):
        raise CorpusError("corpus has no cases")
    cases: list[Case] = []
    case_seen: set[str] = set()
    for rc in raw["cases"]:
        cid = rc.get("id") or ""
        if not cid:
            raise CorpusError("case with empty id")
        if cid in case_seen:
            raise CorpusError(f"duplicate case id {cid!r}")
        case_seen.add(cid)
        start = int(rc.get("start_line") or 0)
        if not rc.get("commit") or not rc.get("path") or start < 1:
            raise CorpusError(f"case {cid!r} is not pinned: needs commit, path and start_line")
        text = rc.get("text") or ""
        if not text:
            raise CorpusError(f"case {cid!r} has no text")
        if not rc.get("spans"):
            raise CorpusError(f"case {cid!r} has no spans, so nothing in it is scored")
        lines = text.split("\n")
        last = start + len(lines) - 1
        spans: list[Span] = []
        span_seen: set[str] = set()
        for rs in rc["spans"]:
            sid = rs.get("id") or ""
            if not sid:
                raise CorpusError(f"case {cid!r} has a span with an empty id")
            if sid in span_seen:
                raise CorpusError(f"case {cid!r} has duplicate span id {sid!r}")
            span_seen.add(sid)
            verdict = rs.get("verdict")
            if verdict not in (FLAG, PASS):
                raise CorpusError(f"{cid}/{sid}: verdict {verdict!r} must be {FLAG!r} or {PASS!r}")
            arguable = bool(rs.get("arguable", False))
            if arguable and verdict == PASS:
                raise CorpusError(f"{cid}/{sid}: a pass span cannot be arguable; a false flag on one is never excused")
            a, b = rs["lines"]
            if a < start or b > last or a > b:
                raise CorpusError(f"{cid}/{sid}: lines {a}-{b} fall outside the case's {start}-{last}")
            anchor = rs.get("anchor") or ""
            if not anchor or anchor not in lines[a - start]:
                raise CorpusError(f"{cid}/{sid}: anchor {anchor!r} is not on line {a} of the pinned text")
            spans.append(Span(sid, (a, b), anchor, verdict, rs.get("category", ""), arguable, rs.get("note", "")))
        cases.append(Case(cid, rc["commit"], rc["path"], start, text, tuple(spans), rc.get("provenance", "")))
    return Corpus(raw.get("question", ""), int(raw.get("rubric_version", 0)), raw.get("rubric_source", ""), tuple(cases), source)


def load(path: str | Path) -> Corpus:
    p = Path(path)
    return parse(p.read_text(encoding="utf-8"), str(p))


@dataclass(frozen=True)
class BlindSpan:
    """What the reviewer is shown about a span: location and text, never the verdict, category,
    note or either id, each of which names the answer."""

    key: str
    lines: tuple[int, int]
    anchor: str


def blind_case(corpus: Corpus, case_idx: int) -> tuple[str, list[BlindSpan]]:
    """The excerpt text, line-numbered the way a diff view would, plus the blind spans."""
    case = corpus.cases[case_idx]
    body = "\n".join(
        f"{case.start_line + i}: {line}" for i, line in enumerate(case.text.rstrip("\n").split("\n"))
    )
    spans = [BlindSpan(key(case_idx, j), sp.lines, sp.anchor) for j, sp in enumerate(case.spans)]
    return f"{case.path}, starting at line {case.start_line}\n{body}", spans


# Gold, in the shape the scorer needs: key -> (verdict, arguable, span id). Kept apart from
# BlindSpan so nothing the reviewer sees can carry it.
def gold_for_case(corpus: Corpus, case_idx: int) -> dict[str, dict]:
    case = corpus.cases[case_idx]
    return {
        key(case_idx, j): {"verdict": sp.verdict, "arguable": sp.arguable, "span": span_id(case, sp)}
        for j, sp in enumerate(case.spans)
    }


class ObjectMissing(Exception):
    """The pinned commit is not in this clone."""


def git_show(repo: str | Path) -> Callable[[str, str], bytes]:
    def show(commit: str, path: str) -> bytes:
        have = subprocess.run(["git", "-C", str(repo), "cat-file", "-e", f"{commit}^{{commit}}"], capture_output=True)
        if have.returncode != 0:
            raise ObjectMissing(commit)
        out = subprocess.run(["git", "-C", str(repo), "show", f"{commit}:{path}"], capture_output=True)
        if out.returncode != 0:
            raise RuntimeError(out.stderr.decode(errors="replace"))
        return out.stdout

    return show


def check_pinned(corpus: Corpus, show: Callable[[str, str], bytes]) -> tuple[int, list[str], list[str]]:
    """Compare every case's text with the file at its pinned commit. Returns (checked, skipped,
    mismatches). Skipped cases are returned, not hidden: a check that skips everything has verified
    nothing."""
    checked, skipped, mismatches = 0, [], []
    for case in corpus.cases:
        try:
            data = show(case.commit, case.path)
        except ObjectMissing:
            skipped.append(case.id)
            continue
        except Exception as e:  # noqa: BLE001 - reported, never swallowed
            mismatches.append(f"{case.id}: reading {case.commit}:{case.path}: {e}")
            continue
        checked += 1
        src = data.decode("utf-8").split("\n")
        want = case.text.split("\n")
        lo, hi = case.start_line - 1, case.start_line - 1 + len(want)
        if hi > len(src):
            mismatches.append(f"{case.id}: {case.commit}:{case.path} has only {len(src)} lines, corpus text runs to line {hi}")
            continue
        for i, w in enumerate(want):
            if src[lo + i] != w:
                mismatches.append(
                    f"{case.id}: line {case.start_line + i} at {case.commit} differs:\n  corpus: {w!r}\n  source: {src[lo + i]!r}"
                )
    return checked, skipped, mismatches
