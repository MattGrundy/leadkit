"""The whole path through Inspect, with a fake `claude` that records how it was called.

These are the tests that matter most: they drive real Inspect epochs, scorer, reducer and metric, and
check that a reviewer cannot score its way past the harness (a duplicated answer, a skipped span, a
run that died) and that what the real agent is told contains no label."""

import json
import re
from pathlib import Path

import pytest
from inspect_ai import Epochs, Task, eval as inspect_eval
from inspect_ai.dataset import MemoryDataset
from inspect_ai.solver import solver

from conftest import DEFINITION_PATH, REPO, calls

def load_task_module():
    import importlib.util

    spec = importlib.util.spec_from_file_location("comments_task", REPO / "evals" / "comments" / "task.py")
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def run(tmp_path, claude, repo, sha, **kw):
    t = load_task_module().comment_rule(definition_sha=sha, claude_bin=str(claude), repo=str(repo), **kw)
    return run_task(tmp_path, t)


def run_task(tmp_path, t, **kw):
    logs = inspect_eval(t, model="mockllm/model", log_dir=str(tmp_path / "logs"), display="none", **kw)
    return logs[0]


def metrics(log):
    return {k: v.value for k, v in log.results.scores[0].metrics.items()}


def test_a_perfect_reviewer_passes_and_cost_is_recorded(tmp_path, fake_claude, definition_repo):
    claude, log_dir = fake_claude
    repo, _, rule = definition_repo
    m = metrics(run(tmp_path, claude, repo, rule))
    assert m["verdict"] == 1.0 and m["caught"] == 5 and m["false_flags"] == 0 and m["unanswered"] == 0
    n = len(calls(log_dir))
    assert n == 6 * 3, "six cases, three epochs each"
    assert m["cost_usd"] == pytest.approx(0.01 * n)
    assert m["input_tokens"] == 150 * n, "cached input counts as input"
    assert m["output_tokens"] == 10 * n


def test_flag_everything_and_flag_nothing_both_fail_for_their_own_reason(tmp_path, fake_claude, monkeypatch, definition_repo):
    claude, _ = fake_claude
    repo, _, rule = definition_repo
    monkeypatch.setenv("FAKE_MODE", "flag_all")
    m = metrics(run(tmp_path / "a", claude, repo, rule))
    assert m["verdict"] == 0.0 and m["false_flags"] == 3 and m["missed"] == 0
    monkeypatch.setenv("FAKE_MODE", "flag_none")
    m = metrics(run(tmp_path / "b", claude, repo, rule))
    assert m["verdict"] == 0.0 and m["missed"] == 4 and m["false_flags"] == 0 and m["missed_arguable"] == 1


@pytest.mark.parametrize("mode", ["dup_first_call", "skip_one_double_other", "error_first_call", "garbage_first_call"])
def test_one_bad_epoch_fails_the_suite_instead_of_shrinking_the_vote(tmp_path, fake_claude, monkeypatch, mode, definition_repo):
    """The other two epochs are perfect. A duplicated key, a skipped span, a crashed claude and
    unparseable output must each fail the whole sample, never be outvoted."""
    claude, _ = fake_claude
    monkeypatch.setenv("FAKE_MODE", mode)
    repo, _, rule = definition_repo
    m = metrics(run(tmp_path, claude, repo, rule))
    assert m["verdict"] == 0.0, f"{mode} must fail the suite"
    assert m["unanswered"] > 0


def test_a_flipping_reviewer_wins_the_vote_but_is_reported_unstable(tmp_path, fake_claude, monkeypatch, definition_repo):
    claude, _ = fake_claude
    repo, _, rule = definition_repo
    monkeypatch.setenv("FAKE_MODE", "flip_pass_once")
    m = metrics(run(tmp_path, claude, repo, rule))
    assert m["verdict"] == 1.0
    assert m["unstable"] == 3, "every pass span flipped in one epoch of three"


# --- what Inspect does by itself, pinned so a version bump that changes it is noticed -------------

@solver
def _flaky(fail_epoch_calls):
    state = {"n": 0}

    async def run(task_state, generate):
        state["n"] += 1
        if state["n"] in fail_epoch_calls:
            raise RuntimeError("this epoch blew up")
        task_state.store.set("result", {"answers": [{"key": k, "flagged": v} for k, v in task_state.metadata["answers"].items()]})
        return task_state

    return run


def _task_with(solver_, corpus_gold_flags):
    tm = load_task_module()
    from leadkit_eval import corpus as cm
    from leadkit_eval.inspect_support import span_answers, strict_majority, suite_verdict

    corpus = cm.load(tm.CORPUS)
    samples = tm.build_samples(corpus)
    for s in samples:
        keys = json.loads(s.target)
        s.metadata = {"answers": {k: corpus_gold_flags[k] for k in keys}}  # test-only: the fake solver reads it
    gold_all = {}
    for i in range(len(corpus.cases)):
        gold_all.update(cm.gold_for_case(corpus, i))
    return Task(
        dataset=MemoryDataset(samples), solver=solver_, scorer=span_answers(),
        metrics=[suite_verdict(gold_all)], epochs=Epochs(3, strict_majority(3)),
    )


def test_a_dropped_epoch_makes_the_reduced_score_invalid(tmp_path, gold_all):
    """Verified rather than assumed: with fail_on_error=False Inspect finishes 'success' having
    reduced two scores where three were asked for. Left alone that is a run that skipped a span."""
    flags = {k: g["verdict"] == "flag" for k, g in gold_all.items()}
    log = run_task(tmp_path, _task_with(_flaky({1}), flags), fail_on_error=False)
    assert log.status == "success", "if Inspect now fails the run itself, this guard may be redundant"
    m = metrics(log)
    assert m["verdict"] == 0.0 and m["unanswered"] > 0


def test_a_run_with_no_results_is_a_fail(tmp_path, gold_all):
    """Inspect finishes this with status success and no metrics at all. The suite verdict then does
    not exist, which a reader must never take for a pass."""
    from leadkit_eval.report import summarize

    flags = {k: g["verdict"] == "flag" for k, g in gold_all.items()}
    log = run_task(tmp_path, _task_with(_flaky(set(range(1, 100))), flags), fail_on_error=False)
    assert log.status == "success" and log.results is None
    ok, text = summarize(log)
    assert not ok and "nothing was scored" in text


def test_a_dropped_epoch_in_one_sample_only_leaves_the_others_scored(tmp_path, gold_all):
    flags = {k: g["verdict"] == "flag" for k, g in gold_all.items()}
    log = run_task(tmp_path, _task_with(_flaky({1}), flags), fail_on_error=False)
    from leadkit_eval.report import summarize

    ok, text = summarize(log)
    assert not ok and "unanswered" in text


def test_a_failed_eval_is_not_a_pass(tmp_path, gold_all):
    flags = {k: g["verdict"] == "flag" for k, g in gold_all.items()}
    log = run_task(tmp_path, _task_with(_flaky({1}), flags))  # default fail_on_error
    assert log.status == "error"


def test_an_even_epoch_count_has_no_majority():
    from leadkit_eval.inspect_support import strict_majority

    with pytest.raises(ValueError):
        strict_majority(2)


# --- what the real agent is run with and shown --------------------------------------------------

def test_the_agent_runs_the_definition_at_the_pinned_sha_with_the_axes_it_was_given(tmp_path, fake_claude, definition_repo):
    import subprocess

    claude, log_dir = fake_claude
    repo, _, rule = definition_repo
    log = run(tmp_path, claude, repo, rule[:8], model="sonnet", effort="high")  # an abbreviated sha: it is recorded in full
    argv = calls(log_dir)[0]["argv"]

    agents = json.loads(argv[argv.index("--agents") + 1])
    assert list(agents) == ["code-review-agent"] and argv[argv.index("--agent") + 1] == "code-review-agent"
    shown = subprocess.run(["git", "-C", str(repo), "show", f"{rule}:internal/starter/agents/code-review-agent.md"],
                           capture_output=True, text=True).stdout
    assert agents["code-review-agent"]["prompt"].strip() == shown.split("\n---\n", 1)[1].strip(), \
        "the prompt must be the definition from git, not a copy"
    assert agents["code-review-agent"]["tools"] == ["Read", "Grep"]
    assert argv[argv.index("--model") + 1] == "sonnet" and argv[argv.index("--effort") + 1] == "high"
    assert argv[argv.index("--tools") + 1] == "", "the excerpt is inline; the reviewer needs no tool"
    schema = json.loads(argv[argv.index("--json-schema") + 1])
    assert schema["required"] == ["answers"]
    # the tuple is recorded with the result, with the sha resolved to the full one
    assert log.eval.metadata["definition_sha"] == rule
    assert log.eval.metadata["model"] == "sonnet" and log.eval.metadata["effort"] == "high"


def test_the_control_arm_is_the_same_suite_with_only_the_definition_changed(tmp_path, fake_claude, definition_repo):
    claude, log_dir = fake_claude
    repo, control, rule = definition_repo
    run(tmp_path / "a", claude, repo, rule)
    run(tmp_path / "b", claude, repo, control)
    c = calls(log_dir)
    prompts = set()
    for call in c:
        prompts.add(json.loads(call["argv"][call["argv"].index("--agents") + 1])["code-review-agent"]["prompt"])
    assert len(prompts) == 2, "two different definitions were run"
    assert {c_["stdin"] for c_ in c[: len(c) // 2]} == {c_["stdin"] for c_ in c[len(c) // 2:]}, \
        "the excerpts the reviewer saw are identical across arms"


def test_the_reviewer_is_shown_no_label(tmp_path, fake_claude, corpus, definition_repo):
    claude, log_dir = fake_claude
    repo, _, rule = definition_repo
    run(tmp_path, claude, repo, rule)
    shown = "\n".join(c["stdin"] for c in calls(log_dir))
    for case in corpus.cases:
        assert case.id not in shown
        for sp in case.spans:
            assert sp.id not in shown
            if sp.note:
                assert sp.note not in shown
    assert re.search(r"^SPAN c1\.s1:", shown, re.M)
    assert "verdict" not in shown.lower().replace("verdicts", "")


# --- the solver's failure paths and the shapes of `claude`'s output ------------------------------

def _solver_result(tmp_path, repo, sha, claude, **kw):
    from inspect_ai.dataset import Sample
    from leadkit_eval.solver import claude_agent

    t = Task(
        dataset=MemoryDataset([Sample(id="c1", input="SPAN c1.s1: x", target="x")]),
        solver=claude_agent(DEFINITION_PATH, definition_sha=sha, claude_bin=str(claude), repo=str(repo), **kw),
    )
    return run_task(tmp_path, t).samples[0].store["result"]


def test_a_claude_that_outlives_the_timeout_is_killed_and_recorded_as_an_error(tmp_path, definition_repo):
    repo, _, rule = definition_repo
    slow = tmp_path / "claude"
    slow.write_text("#!/bin/sh\nexec sleep 30\n")
    slow.chmod(0o755)
    result = _solver_result(tmp_path, repo, rule, slow, timeout=1)
    assert result["error"] == "timed out after 1s"
    assert "answers" not in result


def test_a_missing_claude_binary_is_recorded_as_an_error(tmp_path, definition_repo):
    repo, _, rule = definition_repo
    result = _solver_result(tmp_path, repo, rule, tmp_path / "no-such-claude")
    assert result["error"].startswith("could not run ")
    assert "answers" not in result


def test_a_nonzero_exit_records_the_exit_code_and_stderr(tmp_path, definition_repo):
    repo, _, rule = definition_repo
    bad = tmp_path / "claude"
    bad.write_text("#!/bin/sh\necho 'rate limited' >&2\nexit 3\n")
    bad.chmod(0o755)
    result = _solver_result(tmp_path, repo, rule, bad)
    assert "exit 3: rate limited" in result["error"] and "answers" not in result


def test_parse_result_reads_answers_from_the_text_result_when_there_is_no_structured_output():
    from leadkit_eval.solver import parse_result

    answers = [{"key": "c1.s1", "flagged": True}]
    out = parse_result(json.dumps({"result": json.dumps({"answers": answers}), "total_cost_usd": 0.5}))
    assert out["answers"] == answers and out["cost_usd"] == 0.5


@pytest.mark.parametrize("stdout", [
    json.dumps({"result": "not json"}),
    json.dumps({"result": json.dumps({"other": 1})}),
    json.dumps({}),
])
def test_parse_result_without_answers_is_an_error(stdout):
    from leadkit_eval.solver import parse_result

    out = parse_result(stdout)
    assert out["error"] == "no structured answers in the output" and "answers" not in out
