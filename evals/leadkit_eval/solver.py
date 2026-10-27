"""A Solver that runs the REAL agent definition, not a reimplementation of it.

It shells out to `claude -p --agents <definition> --agent <name> --json-schema <shape>` with the
definition read straight from git at a pinned sha, so the role-definition sha is an axis of the run
for free and a prompt change and a model change are separately attributable.

A run that fails never becomes an empty answer: the failure is recorded and the scorer treats the
epoch as invalid, which fails the suite rather than shrinking the vote.
"""

from __future__ import annotations

import asyncio
import json
import os
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml
from inspect_ai.model import ModelOutput, ModelUsage
from inspect_ai.solver import Generate, TaskState, solver

ANSWER_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "answers": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {"key": {"type": "string"}, "flagged": {"type": "boolean"}},
                "required": ["key", "flagged"],
                "additionalProperties": False,
            },
        }
    },
    "required": ["answers"],
    "additionalProperties": False,
}


@dataclass(frozen=True)
class Definition:
    sha: str  # the full sha the ref resolved to, recorded with every result
    path: str
    name: str
    agent: dict[str, Any]  # the value in the --agents JSON


def repo_root(start: str | Path | None = None) -> Path:
    out = subprocess.run(
        ["git", "-C", str(start or Path(__file__).resolve().parent), "rev-parse", "--show-toplevel"],
        capture_output=True, text=True, check=True,
    )
    return Path(out.stdout.strip())


def resolve_definition(repo: str | Path, ref: str, path: str) -> Definition:
    """Read an agent definition at ref (a sha, tag or branch) from git. The ref is resolved to a
    full sha first so a branch name cannot quietly mean two different definitions in two runs."""
    sha = subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "--verify", f"{ref}^{{commit}}"], capture_output=True, text=True
    )
    if sha.returncode != 0:
        raise ValueError(f"cannot resolve {ref!r} in {repo}: {sha.stderr.strip()}")
    full = sha.stdout.strip()
    shown = subprocess.run(["git", "-C", str(repo), "show", f"{full}:{path}"], capture_output=True, text=True)
    if shown.returncode != 0:
        raise ValueError(f"{path} does not exist at {full}: {shown.stderr.strip()}")
    return parse_definition(shown.stdout, full, path)


def parse_definition(text: str, sha: str, path: str) -> Definition:
    if not text.startswith("---\n"):
        raise ValueError(f"{path} at {sha} has no frontmatter")
    head, sep, body = text[4:].partition("\n---\n")
    if not sep:
        raise ValueError(f"{path} at {sha} has an unterminated frontmatter block")
    fm = yaml.safe_load(head) or {}
    name = fm.get("name")
    if not name or not fm.get("description"):
        raise ValueError(f"{path} at {sha} needs name and description in its frontmatter")
    agent: dict[str, Any] = {"description": fm["description"], "prompt": body.lstrip("\n")}
    tools = fm.get("tools")
    if tools:
        agent["tools"] = [t.strip() for t in tools.split(",")] if isinstance(tools, str) else list(tools)
    return Definition(sha, path, name, agent)


def build_command(
    claude_bin: str,
    d: Definition,
    *,
    model: str | None = None,
    effort: str | None = None,
    tools: str | None = "",
    allowed_tools: list[str] | None = None,
    schema: dict[str, Any] | None = None,
) -> list[str]:
    """tools="" removes every built-in tool (the comment suite shows the whole excerpt inline and
    the reviewer needs none); None leaves the definition's own grant in force. allowed_tools is
    --allowedTools, the grant a suite can assert."""
    cmd = [
        claude_bin, "-p",
        "--agents", json.dumps({d.name: d.agent}),
        "--agent", d.name,
        "--output-format", "json",
        "--json-schema", json.dumps(schema or ANSWER_SCHEMA),
        "--no-session-persistence",
        "--disable-slash-commands",
        "--setting-sources", "",
    ]
    if tools is not None:
        cmd += ["--tools", tools]
    if allowed_tools:
        cmd += ["--allowedTools", *allowed_tools]
    if model:
        cmd += ["--model", model]
    if effort:
        cmd += ["--effort", effort]
    return cmd


def input_tokens(usage: dict[str, Any]) -> int:
    """Cached input is input: counting only `input_tokens` understates "tokens per run"."""
    return sum(int(usage.get(k) or 0) for k in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"))


def parse_result(stdout: str) -> dict[str, Any]:
    """Pull the answers and the cost out of `--output-format json`. Returns a dict with `answers`
    (the raw value, validated later by the scorer, never here) and whatever cost fields exist."""
    try:
        doc = json.loads(stdout)
    except json.JSONDecodeError as e:
        return {"error": f"output is not JSON: {e}"}
    if not isinstance(doc, dict):
        return {"error": "output is not a JSON object"}
    out: dict[str, Any] = {
        "cost_usd": doc.get("total_cost_usd"),
        "duration_ms": doc.get("duration_ms"),
        "usage": doc.get("usage") or {},
    }
    if doc.get("is_error"):
        out["error"] = f"claude reported an error: {str(doc.get('result'))[:300]}"
        return out
    payload = doc.get("structured_output")
    if payload is None and isinstance(doc.get("result"), str):
        # Without a structured_output field, the JSON is in the text result.
        try:
            payload = json.loads(doc["result"])
        except json.JSONDecodeError:
            payload = None
    if not isinstance(payload, dict) or "answers" not in payload:
        out["error"] = "no structured answers in the output"
        return out
    out["answers"] = payload["answers"]
    return out


@solver
def claude_agent(
    path: str,
    definition_sha: str = "HEAD",
    model: str | None = None,
    effort: str | None = None,
    tools: str | None = "",
    allowed_tools: list[str] | None = None,
    claude_bin: str | None = None,
    repo: str | None = None,
    timeout: int = 600,
):
    """Run state.input_text through the real definition at path@definition_sha."""
    root = Path(repo) if repo else repo_root()
    d = resolve_definition(root, definition_sha, path)
    binary = claude_bin or os.environ.get("LEADKIT_CLAUDE_BIN", "claude")
    cmd = build_command(binary, d, model=model, effort=effort, tools=tools, allowed_tools=allowed_tools)

    async def run(state: TaskState, generate: Generate) -> TaskState:
        with tempfile.TemporaryDirectory(prefix="leadkit-eval-") as cwd:
            try:
                proc = await asyncio.create_subprocess_exec(
                    *cmd, cwd=cwd,
                    stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
                )
                stdout, stderr = await asyncio.wait_for(proc.communicate(state.input_text.encode()), timeout)
            except asyncio.TimeoutError:
                proc.kill()
                await proc.wait()  # reap the killed child
                result: dict[str, Any] = {"error": f"timed out after {timeout}s"}
            except OSError as e:
                result = {"error": f"could not run {binary}: {e}"}
            else:
                result = parse_result(stdout.decode(errors="replace"))
                if proc.returncode != 0 and "error" not in result:
                    result["error"] = f"exit {proc.returncode}: {stderr.decode(errors='replace')[:300]}"
                elif proc.returncode != 0:
                    result["error"] += f" (exit {proc.returncode}: {stderr.decode(errors='replace')[:200]})"
        state.store.set("result", result)
        u = result.get("usage") or {}
        tin = input_tokens(u)
        tout = int(u.get("output_tokens") or 0)
        state.output = ModelOutput.from_content(
            model=f"claude/{model or 'default'}",
            content=json.dumps(result.get("answers", result.get("error", ""))),
        )
        state.output.usage = ModelUsage(input_tokens=tin, output_tokens=tout, total_tokens=tin + tout)
        return state

    return run
