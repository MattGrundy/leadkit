#!/usr/bin/env python3
"""A stand-in for `claude -p` that records how it was called and answers from a plan.

Not a model: it answers from FAKE_GOLD (span key -> flag) transformed by FAKE_MODE, so the tests can
drive the harness through the invalid and unstable cases a real reviewer produces only sometimes.
"""
import fcntl
import json
import os
import re
import sys

stdin = sys.stdin.read()
log_dir = os.environ["FAKE_LOG_DIR"]
keys = re.findall(r"^SPAN (c\d+\.s\d+):", stdin, flags=re.M)

# call_no is computed inside the lock, from the lines the append just extended, so concurrent epochs
# of one sample get distinct numbers and exactly one is the first.
with open(os.path.join(log_dir, "calls.lock"), "a+") as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    path = os.path.join(log_dir, "calls.jsonl")
    with open(path, "a") as f:
        f.write(json.dumps({"argv": sys.argv[1:], "stdin": stdin, "cwd": os.getcwd()}) + "\n")
    with open(path) as f:
        seen = [json.loads(l) for l in f]
    mine = [c for c in seen if re.findall(r"^SPAN (c\d+\.s\d+):", c["stdin"], flags=re.M) == keys]
    call_no = len(mine)  # 1-based within this sample, in arrival order
    fcntl.flock(lock, fcntl.LOCK_UN)

gold = json.load(open(os.environ["FAKE_GOLD"]))
mode = os.environ.get("FAKE_MODE", "perfect")

answers = [{"key": k, "flagged": bool(gold[k])} for k in keys]
if mode == "flag_all":
    answers = [{"key": k, "flagged": True} for k in keys]
elif mode == "flag_none":
    answers = [{"key": k, "flagged": False} for k in keys]
elif mode == "dup_first_call" and call_no == 1:
    # a false flag, then the honest-looking answer for the same key
    answers = [{"key": keys[0], "flagged": True}] + answers
elif mode == "skip_one_double_other" and call_no == 1 and len(keys) > 0:
    answers = answers[1:] + [answers[-1]]  # skips keys[0], answers the last twice
elif mode == "error_first_call" and call_no == 1:
    print(json.dumps({"is_error": True, "result": "boom"}))
    sys.exit(1)
elif mode == "garbage_first_call" and call_no == 1:
    print("not json at all")
    sys.exit(0)
elif mode == "flip_pass_once" and call_no == 1:
    answers = [{"key": a["key"], "flagged": (not a["flagged"]) if not gold[a["key"]] else a["flagged"]} for a in answers]

doc = {
    "type": "result", "is_error": False, "result": "",
    "structured_output": {"answers": answers},
    "total_cost_usd": 0.01, "duration_ms": 1234,
    "usage": {"input_tokens": 100, "cache_read_input_tokens": 50, "output_tokens": 10},
}
print(json.dumps(doc))
