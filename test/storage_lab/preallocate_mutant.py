#!/usr/bin/env python3
"""Build overlay restoring Linux's historical ignored fallocate error."""
import json
from pathlib import Path
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "weed/storage/backend/volume_create_linux.go"
FIXED = '''\t\tif err := syscall.Fallocate(int(file.Fd()), 1, 0, preallocate); err != nil {
\t\t\tcleanupErr := errors.Join(file.Close(), os.Remove(fileName))
\t\t\treturn nil, errors.Join(
\t\t\t\tfmt.Errorf("preallocate %d bytes for %s: %w", preallocate, fileName, err),
\t\t\t\tcleanupErr,
\t\t\t)
\t\t}
'''
BROKEN = '''\t\t// MUTANT: reproduce the historical ignored reservation failure.
\t\t_ = errors.Join
\t\t_ = fmt.Sprintf
\t\tsyscall.Fallocate(int(file.Fd()), 1, 0, preallocate)
'''

def revert(source):
    if source.count(FIXED) != 1:
        raise ValueError("expected exactly one reviewed preallocation fix")
    return source.replace(FIXED, BROKEN)


def main():
    directory = Path(tempfile.mkdtemp(prefix="seaweedfs-preallocate-mutant-"))
    target = directory / SOURCE.name
    target.write_text(revert(SOURCE.read_text()))
    overlay = directory / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {str(SOURCE): str(target)}}, indent=2) + "\n")
    print(overlay)


if __name__ == "__main__":
    main()
