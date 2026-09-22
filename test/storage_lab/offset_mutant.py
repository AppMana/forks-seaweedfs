#!/usr/bin/env python3
"""Prepare a throwaway Go overlay reverting only the upstream #11411 fix."""
import json
from pathlib import Path
import tempfile

FIXED = 'idxEntryBytes = needle_map.ToBytes(key, ToOffset(offset), increIdxEntry.size)'
BROKEN = 'idxEntryBytes = needle_map.ToBytes(key, increIdxEntry.offset, increIdxEntry.size)'


def revert(source):
    if source.count(FIXED) != 1:
        raise ValueError('expected exactly one upstream offset correction; review source first')
    return source.replace(FIXED, BROKEN)


def main():
    source = Path(__file__).resolve().parents[2] / 'weed/storage/volume_vacuum.go'
    changed = revert(source.read_text())
    directory = Path(tempfile.mkdtemp(prefix='seaweedfs-offset-mutant-'))
    target = directory / 'volume_vacuum.go'
    target.write_text(changed)
    overlay = directory / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(target)}}, indent=2) + '\n')
    print(overlay)


if __name__ == '__main__':
    main()
