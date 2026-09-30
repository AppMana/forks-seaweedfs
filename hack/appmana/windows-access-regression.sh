#!/usr/bin/env bash
# Uses the existing matched-SDK Labcontainers VM runner and offline inputs.
# Usage: bash hack/appmana/windows-access-regression.sh BASELINE_EXE CANDIDATE_EXE
set -euo pipefail
test "$#" -eq 2 || { echo 'Supply baseline and candidate Windows executables' >&2; exit 2; }
baseline=$(realpath "$1")
candidate=$(realpath "$2")
test -f "$baseline" && test -f "$candidate"
cd "$(dirname "$0")/../.."
test "$(sha256sum "$baseline" | cut -d' ' -f1)" != "$(sha256sum "$candidate" | cut -d' ' -f1)" || {
  echo 'Identical baseline and candidate binaries are not a comparison' >&2; exit 2;
}
: "${RUNNER_TEMP:?Set a persistent artifact directory}"
: "${SEAWEEDFS_WINDOWS_WINFSP_TEST:?Supply the same native test executable for both builds}"
: "${LABCONTAINERS_LABD:?Supply the matched Labcontainers daemon}"
: "${LABCONTAINERS_WINDOWS_IMAGE:?Supply the pinned Windows lab image}"
baseline_basic=${SEAWEEDFS_WINDOWS_ACCESS_BASELINE_BASIC_PERMISSIONS:-0}
candidate_basic=${SEAWEEDFS_WINDOWS_ACCESS_CANDIDATE_BASIC_PERMISSIONS:-$baseline_basic}
for policy in "$baseline_basic" "$candidate_basic"; do
  [[ "$policy" == 0 || "$policy" == 1 ]] || { echo 'Basic permissions must be 0 or 1' >&2; exit 2; }
done
baseline_dll=${SEAWEEDFS_WINDOWS_ACCESS_BASELINE_DLL:-${SEAWEEDFS_WINDOWS_WINFSP_DLL:-}}
candidate_dll=${SEAWEEDFS_WINDOWS_ACCESS_CANDIDATE_DLL:-$baseline_dll}
results=$(mktemp -d "$RUNNER_TEMP/windows-access-regression.XXXXXXXX")
echo "Retained comparison: $results"
sha256sum "$baseline" "$candidate" "$SEAWEEDFS_WINDOWS_WINFSP_TEST" | tee "$results/inputs.sha256"
for dll in "$baseline_dll" "$candidate_dll"; do
  if [[ -n "$dll" ]]; then
    sha256sum "$dll" "$dll.manifest.txt" "$dll.source.patch" | tee -a "$results/inputs.sha256"
  fi
done
printf 'baseline_basic_permissions=%s\ncandidate_basic_permissions=%s\nbaseline_dll=%s\ncandidate_dll=%s\n' \
  "$baseline_basic" "$candidate_basic" "$baseline_dll" "$candidate_dll" | tee "$results/policies.txt"
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
    basic=$baseline_basic
    dll=$baseline_dll
    if [[ "$label" == candidate ]]; then
      binary=$candidate
      basic=$candidate_basic
      dll=$candidate_dll
    fi
    log="$results/$label-$repetition.log"
    (cd test/storage_lab/vm && \
      SEAWEEDFS_WINDOWS_WEED="$binary" SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS="$basic" \
      SEAWEEDFS_WINDOWS_WINFSP_DLL="$dll" \
      go test . -run '^TestWindowsMountLab$' -count=1 -v -timeout=30m) 2>&1 | tee "$log"
    python3 hack/appmana/compare-windows-access.py --validate-only "$log"
    args+=("--$label" "$log")
  done
done
python3 hack/appmana/compare-windows-access.py "${args[@]}" \
  --max-regression-percent "${SEAWEEDFS_WINDOWS_ACCESS_MAX_REGRESSION_PERCENT:-15}" | tee "$results/comparison.jsonl"
