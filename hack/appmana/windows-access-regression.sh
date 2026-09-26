#!/usr/bin/env bash
# Uses the existing matched-SDK Labcontainers VM runner and offline inputs.
# Usage: bash hack/appmana/windows-access-regression.sh BASELINE_EXE CANDIDATE_EXE
set -euo pipefail
test "$#" -eq 2 || { echo 'Supply baseline and candidate Windows executables' >&2; exit 2; }
baseline=$(realpath "$1")
candidate=$(realpath "$2")
test -f "$baseline" && test -f "$candidate"
test "$(sha256sum "$baseline" | cut -d' ' -f1)" != "$(sha256sum "$candidate" | cut -d' ' -f1)" || {
  echo 'Identical baseline and candidate binaries are not a comparison' >&2; exit 2;
}
: "${RUNNER_TEMP:?Set a persistent artifact directory}"
: "${SEAWEEDFS_WINDOWS_WINFSP_TEST:?Supply the same native test executable for both builds}"
: "${LABCONTAINERS_LABD:?Supply the matched Labcontainers daemon}"
: "${LABCONTAINERS_WINDOWS_IMAGE:?Supply the pinned Windows lab image}"
results=$(mktemp -d "$RUNNER_TEMP/windows-access-regression.XXXXXXXX")
echo "Retained comparison: $results"
sha256sum "$baseline" "$candidate" "$SEAWEEDFS_WINDOWS_WINFSP_TEST" | tee "$results/inputs.sha256"
export SEAWEEDFS_WINDOWS_MOUNT_LIVE=1 SEAWEEDFS_WINDOWS_MOUNT_SCENARIO=AccessPerformance
export SEAWEEDFS_WINDOWS_MOUNT_REPEATS=1 SEAWEEDFS_WINDOWS_MOUNT_TRACE=0 SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY=0
args=()
# Three independent VM runs per build, alternating order. Do not overlap
# performance VMs; startup/installation is outside the measured interval.
for repetition in 1 2 3; do
  order=(baseline candidate)
  if [[ "$repetition" == 2 ]]; then order=(candidate baseline); fi
  for label in "${order[@]}"; do
    binary=$baseline
    if [[ "$label" == candidate ]]; then binary=$candidate; fi
    log="$results/$label-$repetition.log"
    SEAWEEDFS_WINDOWS_WEED="$binary" go test ./test/storage_lab/vm -run '^TestWindowsMountLab$' -count=1 -v -timeout=30m 2>&1 | tee "$log"
    args+=("--$label" "$log")
  done
done
python3 hack/appmana/compare-windows-access.py "${args[@]}" \
  --max-regression-percent "${SEAWEEDFS_WINDOWS_ACCESS_MAX_REGRESSION_PERCENT:-15}" | tee "$results/comparison.jsonl"
