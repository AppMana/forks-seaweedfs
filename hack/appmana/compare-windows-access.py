#!/usr/bin/env python3
"""Compare retained TestWindowsAccessPerformance logs from matched VM runs.

Pass one or more --baseline and --candidate logs. Run builds sequentially on
the same host/image with identical mount options and tracing disabled. This
checks access latency only, not bulk throughput or complete qualification.
"""
import argparse
import json
import re
import statistics
from pathlib import Path

SAMPLE = re.compile(r"ACCESS_PERF sample=(\d+) operation=(\w+) iterations=(\d+) ns_per_op=(\d+)")
OPERATIONS = {"open_read_close": 256, "handle_read": 4096}


def load_runs(paths):
    runs = {op: [] for op in OPERATIONS}
    for path in paths:
        text = Path(path).read_text(encoding="utf-8", errors="strict")
        if "--- PASS: TestWindowsAccessPerformance" not in text:
            raise ValueError(f"{path}: performance test did not pass")
        samples = {op: {} for op in OPERATIONS}
        for index, op, iterations, value in SAMPLE.findall(text):
            if op not in OPERATIONS or int(iterations) != OPERATIONS[op] or int(value) <= 0:
                raise ValueError(f"{path}: invalid sample")
            if int(index) in samples[op]:
                raise ValueError(f"{path}: duplicated sample {op}/{index}")
            samples[op][int(index)] = int(value)
        for op in OPERATIONS:
            if set(samples[op]) != set(range(5)):
                raise ValueError(f"{path}: incomplete {op} samples")
            # Discard the explicitly identified first (warm-up) sample.
            runs[op].append(statistics.median(samples[op][i] for i in range(1, 5)))
    return runs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", action="append", required=True)
    parser.add_argument("--candidate", action="append", required=True)
    parser.add_argument("--max-regression-percent", type=float, default=15)
    args = parser.parse_args()
    if not 0 <= args.max_regression_percent <= 100:
        parser.error("regression percent must be between 0 and 100")
    baseline, candidate = load_runs(args.baseline), load_runs(args.candidate)
    failed = False
    for op in OPERATIONS:
        before, after = statistics.median(baseline[op]), statistics.median(candidate[op])
        regression = (after / before - 1) * 100
        passed = regression <= args.max_regression_percent
        failed |= not passed
        print(json.dumps(dict(operation=op, baseline_ns=before, candidate_ns=after,
                              regression_percent=regression, passed=passed,
                              baseline_runs=baseline[op], candidate_runs=candidate[op])))
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
