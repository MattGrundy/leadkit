"""Read an Inspect log and say whether the suite passed. Exits non-zero unless it did.

    python -m leadkit_eval.report <log.eval> [...]

A log with no results at all (every sample failed, so there was nothing to score) is a FAIL, not an
empty pass: Inspect finishes such a run with status "success" and no metrics.
"""

from __future__ import annotations

import sys

from inspect_ai.log import EvalLog, read_eval_log


def summarize(log: EvalLog) -> tuple[bool, str]:
    md = log.eval.metadata or {}
    tuple_ = f"suite={md.get('suite')} definition={str(md.get('definition_sha'))[:10]} model={md.get('model')} effort={md.get('effort')} epochs={md.get('epochs')}"
    if log.status != "success":
        return False, f"{tuple_}\nFAIL: eval status is {log.status!r}: {log.error.message if log.error else ''}"
    if log.results is None or not log.results.scores:
        return False, f"{tuple_}\nFAIL: the log has no results; nothing was scored"
    m = {k: v.value for s in log.results.scores for k, v in s.metrics.items()}
    if "verdict" not in m:
        return False, f"{tuple_}\nFAIL: no verdict metric in the log"
    ok = m["verdict"] == 1.0
    lines = [tuple_, "PASS" if ok else "FAIL"]
    for k in ("caught", "missed", "missed_arguable", "false_flags", "unanswered", "unstable"):
        lines.append(f"  {k:16} {m.get(k, 0):g}")
    lines.append(f"  cost_usd         {m.get('cost_usd', 0):.4f}   tokens in/out {m.get('input_tokens', 0):g}/{m.get('output_tokens', 0):g}   wall {m.get('duration_ms', 0) / 1000:.1f}s (summed over epochs)")
    return ok, "\n".join(lines)


def main(argv: list[str]) -> int:
    if not argv:
        print(__doc__)
        return 2
    all_ok = True
    for path in argv:
        ok, text = summarize(read_eval_log(path))
        print(text)
        all_ok = all_ok and ok
    return 0 if all_ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
