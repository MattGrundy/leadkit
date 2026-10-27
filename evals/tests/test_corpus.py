"""The corpus is data and these tests are its tripwire: the gold labels are restated here AND in
CRITERIA.md, so editing a label means editing three places and cannot happen by accident. That
duplication is deliberate. Do not dedupe it into one source."""

import json
import shutil
import subprocess

import pytest

from leadkit_eval import corpus as cm


# The root commit of the repository the corpus pins refer to. A clone whose root differs is a
# DIFFERENT repository, not a drifted one, and the pins can never resolve in it: `leadkit publish`
# builds the public repository as fresh commits with no shared ancestry. Identity is the right
# signal here and commit COUNT is not — a published repository has one commit only until its first
# merge, and a pull request build checks out a merge ref, so a count test passes on the first
# release and then silently stops skipping.
PINNED_AGAINST_ROOT = "40d38c4e2042e519ab9f7efb256e1f1875affc33"


def _root_commit(repo) -> str:
    """The repo's first commit, or "" when it cannot be read. An unknown returns "" and therefore
    never matches, so an unreadable root makes the corpus check run rather than skip."""
    out = subprocess.run(
        ["git", "-C", str(repo), "rev-list", "--max-parents=0", "HEAD"],
        capture_output=True,
        text=True,
    )
    return out.stdout.split("\n")[0].strip() if out.returncode == 0 else ""


# The pre-registered verdict table, from evals/comments/CRITERIA.md.
EXPECTED = {
    "main-version-block/stale-bump-by-hand": ("flag", False),
    "main-version-block/why-var-not-const": ("pass", False),
    "main-version-block/history-tail": ("flag", False),
    "install-checksum-narrative/history-checksum-bug": ("flag", True),
    "install-trap-why/why-trap-exits": ("pass", False),
    "hookprobe-deadline/why-deadline-first": ("pass", False),
    "agnostic-ext-restates-name/restates-identifier": ("flag", False),
    "hardcoded-count/hardcoded-73": ("flag", False),
}


def test_committed_corpus_matches_the_pre_registered_verdicts(corpus):
    got = {cm.span_id(c, s): (s.verdict, s.arguable) for c in corpus.cases for s in c.spans}
    assert got == EXPECTED, "corpus verdicts differ from the pre-registered table"
    assert sum(v == "pass" for v, _ in EXPECTED.values()) >= 3, (
        "the pass spans are half the point: a flag-everything reviewer must be catchable"
    )


@pytest.mark.skipif(shutil.which("git") is None, reason="git not available")
def test_committed_corpus_text_matches_the_pinned_commits(corpus):
    from conftest import REPO

    checked, skipped, mismatches = cm.check_pinned(corpus, cm.git_show(REPO))
    assert not mismatches, "\n".join(mismatches)
    # check_pinned deliberately surfaces skips rather than hiding them, because a check that
    # skips everything has verified nothing. That stands for the repository the pins belong to:
    # there, an unresolvable pin is drift and must fail. It cannot hold in a repository with
    # different ancestry, where no pin could ever resolve. So skip on IDENTITY, and keep failing
    # on a missing pin wherever the ancestry matches.
    root = _root_commit(REPO)
    if skipped and root and root != PINNED_AGAINST_ROOT:
        pytest.skip(
            f"this clone's root commit is {root[:12]}, not the {PINNED_AGAINST_ROOT[:12]} the "
            f"corpus pins were taken from, so the history they name does not exist here: {skipped}"
        )
    assert skipped == [], f"pinned commits missing from this clone, so their text was not verified: {skipped}"
    assert checked == len(corpus.cases)


def test_check_pinned_catches_drift_skips_and_overrun():
    sp = (cm.Span("s", (1, 1), "x", "flag"),)
    c = cm.Corpus("q", 1, "", (
        cm.Case("ok", "a", "f", 2, "two\nthree", sp),
        cm.Case("drift", "b", "f", 1, "one\nTWO", sp),
        cm.Case("gone", "c", "f", 1, "one", sp),
        cm.Case("long", "d", "f", 3, "three\nfour", sp),
    ))

    def show(commit, path):
        if commit == "c":
            raise cm.ObjectMissing(commit)
        return b"one\ntwo\nthree"

    checked, skipped, mismatches = cm.check_pinned(c, show)
    joined = "\n".join(mismatches)
    assert checked == 3 and skipped == ["gone"]
    assert "drift: line 2" in joined
    assert "long:" in joined and "only 3 lines" in joined
    assert "ok:" not in joined


def test_blind_view_does_not_leak_the_labels(corpus):
    seen = []
    for i in range(len(corpus.cases)):
        excerpt, spans = cm.blind_case(corpus, i)
        seen.append(excerpt + json.dumps([vars(s) for s in spans], default=str))
    blob = "\n".join(seen)
    for c in corpus.cases:
        assert c.id not in blob, f"blind view leaks case id {c.id!r}"
        for s in c.spans:
            for leak in (s.id, s.note):  # category is a one-word label and matches ordinary prose
                if leak:
                    assert leak not in blob, f"blind view leaks {leak!r}"


HEAD = (
    "question: q\nrubric_version: 1\ncases:\n  - id: a\n    commit: c1\n    path: f.go\n    start_line: 10\n"
    "    text: |-\n      // one\n      // two\n"
)


def _span(body):
    return HEAD + "    spans:\n      - id: s\n" + body


GOOD = _span("        lines: [10, 11]\n        anchor: one\n        verdict: flag\n")


def test_parse_accepts_the_baseline_fixture():
    assert cm.parse(GOOD).cases[0].spans[0].verdict == "flag"


@pytest.mark.parametrize("name,doc", [
    ("empty", "question: q\ncases: []\n"),
    ("unpinned", GOOD.replace("    commit: c1\n", "")),
    ("no spans", HEAD),
    ("bad verdict", GOOD.replace("verdict: flag", "verdict: maybe")),
    ("lines outside", GOOD.replace("[10, 11]", "[10, 12]")),
    ("lines before", GOOD.replace("[10, 11]", "[9, 11]")),
    ("anchor off line", GOOD.replace("anchor: one", "anchor: two")),
    ("arguable pass", GOOD.replace("verdict: flag", "verdict: pass\n        arguable: true")),
    ("duplicate span id", GOOD + "      - id: s\n        lines: [10, 10]\n        anchor: one\n        verdict: flag\n"),
])
def test_parse_rejects_an_unscorable_corpus(name, doc):
    with pytest.raises(cm.CorpusError):
        cm.parse(doc)


def test_parse_rejects_a_duplicate_case_id():
    with pytest.raises(cm.CorpusError):
        cm.parse(GOOD + GOOD.split("cases:\n", 1)[1])
