import json
import os
import stat
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from leadkit_eval import corpus as corpus_mod  # noqa: E402

REPO = Path(__file__).resolve().parents[2]
CORPUS_PATH = REPO / "evals" / "comments" / "corpus-known-bad-comments.yaml"


@pytest.fixture(scope="session")
def corpus():
    return corpus_mod.load(CORPUS_PATH)


@pytest.fixture(scope="session")
def gold_all(corpus):
    out = {}
    for i in range(len(corpus.cases)):
        out.update(corpus_mod.gold_for_case(corpus, i))
    return out


@pytest.fixture
def fake_claude(tmp_path, gold_all, monkeypatch):
    """A `claude` on disk that records its calls. Returns (path, log_dir)."""
    log = tmp_path / "fakelog"
    log.mkdir()
    gold = tmp_path / "gold.json"
    gold.write_text(json.dumps({k: g["verdict"] == "flag" for k, g in gold_all.items()}))
    script = tmp_path / "claude"
    script.write_text(f"#!/bin/sh\nexec {sys.executable} {Path(__file__).parent / 'fake_claude.py'} \"$@\"\n")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("FAKE_LOG_DIR", str(log))
    monkeypatch.setenv("FAKE_GOLD", str(gold))
    monkeypatch.delenv("FAKE_MODE", raising=False)
    return script, log


def calls(log: Path) -> list[dict]:
    p = log / "calls.jsonl"
    return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []


DEFINITION_PATH = "internal/starter/agents/code-review-agent.md"


def _definition(body: str) -> str:
    return f"---\nname: code-review-agent\ndescription: Code review fixture\ntools: Read, Grep\nmodel: sonnet\n---\n\n{body}\n"


@pytest.fixture
def definition_repo(tmp_path):
    """A throwaway git repo holding two versions of the agent definition, so the harness tests do not
    depend on which of this repo's commits happen to be present in a given clone or CI checkout.
    Returns (repo, control_sha, rule_sha)."""
    import subprocess

    repo = tmp_path / "defrepo"
    (repo / "internal" / "starter" / "agents").mkdir(parents=True)
    env = {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t", "HOME": str(tmp_path), "PATH": os.environ["PATH"]}

    def git(*args):
        return subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True, check=True, env=env).stdout.strip()

    git("init", "-q")
    shas = []
    for body in ("Judge comment density against the neighbours.", "Why: as short as the constraint allows."):
        (repo / DEFINITION_PATH).write_text(_definition(body))
        git("add", ".")
        git("commit", "-qm", body[:20])
        shas.append(git("rev-parse", "HEAD"))
    return repo, shas[0], shas[1]
