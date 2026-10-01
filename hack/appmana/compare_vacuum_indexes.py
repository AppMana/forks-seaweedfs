#!/usr/bin/env python3
"""Read-only vacuum index preservation check; not a payload integrity scrub.

Keep original per-replica snapshots. Any removed/changed live record requires
investigation, including possible concurrent writes/deletes; never silently
classify it as safe. Existing tombstones may disappear during vacuum, but must
not become live again. New keys are reported separately.
"""
import argparse
import json
from pathlib import Path


def read_index(path, offset_bytes):
    if offset_bytes not in (4, 5):
        raise ValueError("offset width must match the volume binary: 4 or 5")
    width = 8 + offset_bytes + 4
    data = Path(path).read_bytes()
    if len(data) % width:
        raise ValueError("partial index row: " + str(path))
    entries = {}
    for start in range(0, len(data), width):
        row = data[start:start + width]
        key = int.from_bytes(row[:8], "big")
        size = int.from_bytes(row[-4:], "big", signed=True)
        has_offset = any(row[8:8 + offset_bytes])
        if size >= 0 and not has_offset:
            raise ValueError("nondeleted index row has zero offset: " + str(key))
        # Negative sizes are tombstones. Zero-length live records remain
        # live when their offset is nonzero, just as in the storage map.
        entries[key] = size if has_offset and size >= 0 else None
    return entries


def compare(before, after):
    missing, changed, resurrected = [], [], []
    for key, size in before.items():
        current = after.get(key)
        if size is None:
            if current is not None:
                resurrected.append(key)
        elif current is None:
            missing.append(key)
        elif size != current:
            changed.append(key)
    return {
        "passed": not (missing or changed or resurrected),
        "scope": "latest index IDs and sizes; not payload hashes or replica convergence",
        "before_live": sum(s is not None for s in before.values()),
        "after_live": sum(s is not None for s in after.values()),
        "missing_live": sorted(missing),
        "changed_live_size": sorted(changed),
        "resurrected": sorted(resurrected),
        "added_live": sorted(k for k, s in after.items() if k not in before and s is not None),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--offset-bytes", type=int, choices=(4, 5), required=True)
    parser.add_argument("before", type=Path)
    parser.add_argument("after", type=Path)
    args = parser.parse_args()
    result = compare(read_index(args.before, args.offset_bytes), read_index(args.after, args.offset_bytes))
    print(json.dumps(result, sort_keys=True))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
