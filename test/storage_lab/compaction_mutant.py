#!/usr/bin/env python3
"""Generate one-change overlays proving compaction regressions catch old bugs."""
import argparse
import json
from pathlib import Path
import tempfile

ROOT = Path(__file__).resolve().parents[2]

MUTATIONS = {
    "replay-validation": (
        "weed/storage/volume_vacuum.go",
        '''\t\t\t// Replay needs the same integrity checks as the initial copy. A
\t\t\t// readable blob can belong to a different key or have a corrupt body.
\t\t\t// Do not publish a new generation that makes the original unrecoverable.
\t\t\tif v.Version() != needle.Version1 && increIdxEntry.size < 4 {
\t\t\t\treturn fmt.Errorf("replay needle %d: invalid body size %d", key, increIdxEntry.size)
\t\t\t}
\t\t\tn := new(needle.Needle)
\t\t\tif err := n.ReadBytes(needleBytes, increIdxEntry.offset.ToActualOffset(), increIdxEntry.size, v.Version()); err != nil {
\t\t\t\treturn fmt.Errorf("validate replay needle %d: %w", key, err)
\t\t\t}
\t\t\tif n.Id != key {
\t\t\t\treturn fmt.Errorf("replay index key %d points to needle %d at offset %d", key, n.Id, increIdxEntry.offset.ToActualOffset())
\t\t\t}
''', ""),
    "completed-swap-cache": (
        "weed/storage/volume_vacuum.go",
        '''\t// Both renames may have completed before a crash without invalidating the
\t// old cache. The marker, not the presence of temp files, requires this step.
\t// Keep the marker on any error so recovery cannot accept a stale cache.
\tif e := os.RemoveAll(v.FileName(".ldb")); e != nil {
\t\treturn fmt.Errorf("invalidate compacted volume cache: %w", e)
\t}
\tif e := os.Remove(v.FileName(".rdb")); e != nil && !os.IsNotExist(e) {
\t\treturn fmt.Errorf("invalidate compacted volume cache: %w", e)
\t}
\tif e := fsyncDir(filepath.Dir(v.FileName(".idx"))); e != nil {
\t\treturn e
\t}
''', ""),
    "scanner-body-error": (
        "weed/storage/volume_read.go",
        '''\t\t\t\treturn fmt.Errorf("cannot read needle body at offset %d: %w", offset, err)
''', '''\t\t\t\terr = nil // MUTANT: reproduce the historical ignored body error
'''),
}


def mutate(kind, source):
    old, new = MUTATIONS[kind][1:]
    if source.count(old) != 1:
        raise ValueError(f'{kind}: expected exactly one reviewed source block')
    return source.replace(old, new)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('kind', choices=sorted(MUTATIONS))
    args = parser.parse_args()
    relative = MUTATIONS[args.kind][0]
    source = ROOT / relative
    directory = Path(tempfile.mkdtemp(prefix='seaweedfs-compaction-mutant-'))
    target = directory / source.name
    target.write_text(mutate(args.kind, source.read_text()))
    overlay = directory / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(target)}}, indent=2) + '\n')
    print(overlay)


if __name__ == '__main__':
    main()
