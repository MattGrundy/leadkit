"""The decision rule, with no model and no Inspect, and the answer sets it must reject."""

import math

import pytest

from leadkit_eval import scoring as sc


def perfect(gold):
    return {k: g["verdict"] == "flag" for k, g in gold.items()}


def test_perfect_reviewer_passes(gold_all):
    r = sc.judge(gold_all, perfect(gold_all))
    ok, reasons = r.verdict()
    assert ok, reasons
    assert len(r.ids(sc.CAUGHT)) == 5


def test_flag_everything_fails_on_the_pass_spans(gold_all):
    ok, reasons = sc.judge(gold_all, sc.flag_everything(gold_all)).verdict()
    r = sc.judge(gold_all, sc.flag_everything(gold_all))
    assert not ok
    assert len(r.ids(sc.FALSE_FLAG)) == 3
    assert "must pass" in " ".join(reasons)


def test_flag_nothing_fails_by_missing_the_non_arguable_flags_only(gold_all):
    r = sc.judge(gold_all, sc.flag_nothing(gold_all))
    ok, reasons = r.verdict()
    assert not ok
    assert r.ids(sc.FALSE_FLAG) == []
    assert len(r.ids(sc.MISSED)) == 4
    assert len(r.ids(sc.MISSED_ARGUABLE)) == 1, "the arguable miss is reported apart"
    assert "missed 4 known-bad" in " ".join(reasons)


def test_one_false_flag_on_a_good_why_fails_however_much_else_is_right(gold_all):
    ans = perfect(gold_all)
    trap = next(k for k, g in gold_all.items() if g["span"] == "install-trap-why/why-trap-exits")
    ans[trap] = True
    r = sc.judge(gold_all, ans)
    assert r.ids(sc.FALSE_FLAG) == ["install-trap-why/why-trap-exits"]
    assert r.ids(sc.MISSED) == []
    assert not r.verdict()[0]


def test_a_missed_arguable_span_is_reported_and_does_not_fail(gold_all):
    ans = perfect(gold_all)
    arg = next(k for k, g in gold_all.items() if g["arguable"])
    ans[arg] = False
    r = sc.judge(gold_all, ans)
    assert r.verdict()[0]
    assert r.ids(sc.MISSED_ARGUABLE) == ["install-checksum-narrative/history-checksum-bug"]


@pytest.mark.parametrize("absent", [None, math.nan])
def test_a_span_with_no_valid_answer_cannot_pass(gold_all, absent):
    ans = perfect(gold_all)
    first = next(iter(gold_all))
    ans[first] = absent
    r = sc.judge(gold_all, ans)
    assert r.ids(sc.UNANSWERED) == [gold_all[first]["span"]]
    assert not r.verdict()[0]


def test_a_gold_span_nobody_reported_on_is_unanswered_not_absent(gold_all):
    ans = perfect(gold_all)
    first = next(iter(gold_all))
    del ans[first]
    assert not sc.judge(gold_all, ans).verdict()[0]


# --- answer sets that are invalid, not scored ---------------------------------------------------

KEYS = ["c1.s1", "c1.s2"]


def test_a_duplicated_key_is_invalid_not_last_wins():
    """A false flag followed by a 'pass' for the same span must not erase the false flag."""
    answers, problems = sc.check_answers(KEYS, [
        {"key": "c1.s1", "flagged": True},
        {"key": "c1.s1", "flagged": False},
        {"key": "c1.s2", "flagged": False},
    ])
    assert problems.duplicate == ["c1.s1"]
    assert answers == {}, "nothing from an invalid run may be scored"


def test_an_identical_repeat_is_still_a_duplicate():
    _, problems = sc.check_answers(KEYS, [
        {"key": "c1.s1", "flagged": True}, {"key": "c1.s1", "flagged": True}, {"key": "c1.s2", "flagged": False},
    ])
    assert problems.duplicate == ["c1.s1"]


def test_a_skipped_span_is_invalid_even_when_another_key_is_answered_twice():
    """Counting appearances would let a doubled key cover for the omitted one."""
    _, problems = sc.check_answers(KEYS, [
        {"key": "c1.s2", "flagged": False}, {"key": "c1.s2", "flagged": False},
    ])
    assert problems.missing == ["c1.s1"] and problems.duplicate == ["c1.s2"]


def test_a_stray_key_and_a_malformed_entry_are_invalid():
    _, p = sc.check_answers(KEYS, [
        {"key": "c1.s1", "flagged": True}, {"key": "c1.s2", "flagged": False}, {"key": "c9.s9", "flagged": True},
    ])
    assert p.unknown == ["c9.s9"]
    _, p = sc.check_answers(KEYS, [{"key": "c1.s1", "flagged": "yes"}, {"key": "c1.s2", "flagged": False}])
    assert p.malformed and p.missing == ["c1.s1"]
    assert sc.check_answers(KEYS, "not a list")[1].malformed


def test_a_clean_answer_set_is_valid():
    answers, problems = sc.check_answers(KEYS, [{"key": "c1.s2", "flagged": False}, {"key": "c1.s1", "flagged": True}])
    assert not problems
    assert answers == {"c1.s1": True, "c1.s2": False}
