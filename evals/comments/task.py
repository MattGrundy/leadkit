"""The comment-rule suite: does a reviewer apply the comment rule the way the corpus says it should?

    inspect eval evals/comments/task.py --model mockllm/model \
        -T definition_sha=<sha> [-T model=sonnet] [-T effort=high] [-T epochs=3]

`--model mockllm/model` only satisfies Inspect: the Solver runs the real agent through `claude -p`
and never calls Inspect's model. The axes of a result are the task parameters, recorded in the log:
(suite, definition sha, model, effort). Change exactly one to attribute a difference.
"""

from __future__ import annotations

import sys
from pathlib import Path

from inspect_ai import Epochs, Task, task
from inspect_ai.dataset import MemoryDataset, Sample

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))  # lets `inspect eval` run without an install

from leadkit_eval import corpus as corpus_mod  # noqa: E402
from leadkit_eval.inspect_support import span_answers, strict_majority, suite_verdict  # noqa: E402
from leadkit_eval.solver import claude_agent, repo_root, resolve_definition  # noqa: E402

SUITE_DIR = Path(__file__).resolve().parent
CORPUS = SUITE_DIR / "corpus-known-bad-comments.yaml"
ROLE_PATH = "internal/starter/agents/code-review-agent.md"

PROMPT = """You are reviewing the comments in an excerpt of a diff. For each SPAN listed below,
decide whether, under your comment rules, you would flag that span as a review finding.

{excerpt}

{spans}

Answer every span exactly once."""


def build_samples(corpus: corpus_mod.Corpus) -> list[Sample]:
    import json

    samples = []
    for i in range(len(corpus.cases)):
        excerpt, spans = corpus_mod.blind_case(corpus, i)
        listing = "\n".join(f"SPAN {s.key}: lines {s.lines[0]}-{s.lines[1]}, beginning {s.anchor!r}" for s in spans)
        samples.append(
            Sample(
                id=corpus_mod.key(i, 0).split(".")[0],  # c1, c2, ...: blind, never the case id
                input=PROMPT.format(excerpt=excerpt, spans=listing),
                target=json.dumps(corpus_mod.gold_for_case(corpus, i)),
            )
        )
    return samples


@task
def comment_rule(
    definition_sha: str = "HEAD",
    role_path: str = ROLE_PATH,
    model: str | None = None,
    effort: str | None = None,
    epochs: int = 3,
    claude_bin: str | None = None,
    repo: str | None = None,
) -> Task:
    corpus = corpus_mod.load(CORPUS)
    gold_all: dict[str, dict] = {}
    for i in range(len(corpus.cases)):
        gold_all.update(corpus_mod.gold_for_case(corpus, i))
    root = Path(repo) if repo else repo_root()
    d = resolve_definition(root, definition_sha, role_path)
    return Task(
        dataset=MemoryDataset(build_samples(corpus), name="known-bad-comments"),
        solver=claude_agent(role_path, definition_sha=d.sha, model=model, effort=effort, claude_bin=claude_bin, repo=str(root)),
        scorer=span_answers(),
        metrics=[suite_verdict(gold_all)],
        epochs=Epochs(epochs, strict_majority(epochs)),
        metadata={
            "suite": "comments",
            "role_path": role_path,
            "definition_sha": d.sha,
            "model": model or "default",
            "effort": effort or "default",
            "epochs": epochs,
            "corpus": str(CORPUS.relative_to(SUITE_DIR.parents[1])),
            "rubric_source": corpus.rubric_source,
        },
    )
