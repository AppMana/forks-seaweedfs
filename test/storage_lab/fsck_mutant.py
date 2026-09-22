#!/usr/bin/env python3
"""Generate one-change overlays proving volume.fsck safety regressions go red."""
import argparse
import json
from pathlib import Path
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "weed/shell/command_volume_fsck.go"

MUTATIONS = {
    "missing-delete-results": (
        """\t\t\tresultChan <- serverDeleteResults{server: server, results: deleteResults}
""",
        """\t\t\t// MUTANT: reproduce the historical silent omission of nil responses.
\t\t\tif deleteResults != nil {
\t\t\t\tresultChan <- serverDeleteResults{server: server, results: deleteResults}
\t\t\t}
""",
    ),
    "ignored-filer-delete": (
        """\t\t\tif applyPurging {
\t\t\t\treturn c.httpDelete(itemPath)
\t\t\t}
""",
        """\t\t\tif applyPurging {
\t\t\t\t// MUTANT: reproduce the historical log-and-continue behavior.
\t\t\t\t_ = c.httpDelete(itemPath)
\t\t\t}
""",
    ),
}


def mutate(kind, source):
    old, new = MUTATIONS[kind]
    if source.count(old) != 1:
        raise ValueError(f"{kind}: expected exactly one reviewed source block")
    return source.replace(old, new)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("kind", choices=sorted(MUTATIONS))
    args = parser.parse_args()

    source = SOURCE.read_text()

    directory = Path(tempfile.mkdtemp(prefix="seaweedfs-fsck-mutant-"))
    target = directory / SOURCE.name
    target.write_text(mutate(args.kind, source))
    overlay = directory / "overlay.json"
    overlay.write_text(
        json.dumps({"Replace": {str(SOURCE): str(target)}}, indent=2) + "\n"
    )
    print(overlay)


if __name__ == "__main__":
    main()
