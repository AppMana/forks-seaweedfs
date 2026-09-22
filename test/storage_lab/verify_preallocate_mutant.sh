#!/bin/bash
set -euo pipefail

[[ $# = 1 ]] || { echo "usage: $0 <mutant-storage.test>" >&2; exit 2; }
tests=$1
: "${MUTANT_LOG_DIR:=$(mktemp -d /tmp/seaweedfs-mutant-logs.XXXXXXXX)}"
mkdir -p "$MUTANT_LOG_DIR"

set +e
output=$(python3 test/storage_lab/run_filesystem.py xfs --enospc --tests "$tests" 2>&1)
code=$?
set -e
printf '%s\n' "$output"
if [[ $code = 0 ]]; then
  echo "preallocation mutant unexpectedly passed" >&2
  exit 1
fi
results=$(printf '%s\n' "$output" | sed -n 's/^Results: //p' | tail -1)
case "$results" in /tmp/seaweedfs-fs-results-*) ;; *) echo "missing safe result path" >&2; exit 1 ;; esac
[[ -f "$results/test.log" && -f "$results/manifest.json" ]] || { echo "missing mutant logs" >&2; exit 1; }
grep -q -- '--- FAIL: TestVolumePreallocateENOSPCFailsCreation' "$results/test.log" || {
  echo "preallocation mutant did not turn the exact regression red" >&2; exit 1;
}
grep -q -- '--- PASS: TestCompactENOSPCPreservesOriginal' "$results/test.log" || {
  echo "adjacent compaction preservation control did not pass" >&2; exit 1;
}
cp "$results/test.log" "$MUTANT_LOG_DIR/preallocate-enospc.log"
cp "$results/manifest.json" "$MUTANT_LOG_DIR/preallocate-enospc-manifest.json"
echo "preallocation mutant turned its exact regression red; logs: $MUTANT_LOG_DIR"
