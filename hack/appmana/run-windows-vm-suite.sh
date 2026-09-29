#!/usr/bin/env bash
# Run prebuilt, pinned inputs through the native Labcontainers SDK. The caller
# supplies GOWORK, the matched labd/helper images, and offline guest artifacts.
# Continue after failures to retain an honest inventory, then fail the campaign.
set -euo pipefail
cd "$(dirname "$0")/../.."
for name in SEAWEEDFS_LAB_ARTIFACTS LABCONTAINERS_LABD LABCONTAINERS_WINDOWS_IMAGE LABCONTAINERS_VM_IMAGE \
  SEAWEEDFS_WINDOWS_WEED SEAWEEDFS_LINUX_WEED SEAWEEDFS_WINDOWS_STORAGE_TEST \
  SEAWEEDFS_WINDOWS_MOUNT_UNIT_TEST SEAWEEDFS_WINDOWS_WINFSP_TEST \
  SEAWEEDFS_WINDOWS_WINFSP_DLL SEAWEEDFS_WINFSP_MSI SEAWEEDFS_GIT_INSTALLER \
  SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC SEAWEEDFS_WINDOWS_CONFORMANCE_EXE \
  SEAWEEDFS_MIXED_LINUX_WORKLOAD SEAWEEDFS_MIXED_WINDOWS_WORKLOAD; do
  if [[ -z "${!name:-}" ]]; then printf 'Missing required input: %s\n' "$name" >&2; exit 2; fi
done
[[ "$SEAWEEDFS_LAB_ARTIFACTS" = /* ]] || { echo 'SEAWEEDFS_LAB_ARTIFACTS must be an absolute persistent directory' >&2; exit 2; }
mkdir -p "$SEAWEEDFS_LAB_ARTIFACTS"
suite_results=$(mktemp -d "$SEAWEEDFS_LAB_ARTIFACTS/seaweedfs-windows-suite.XXXXXXXX")
export RUNNER_TEMP="$suite_results"
printf 'Retained Windows suite evidence: %s\n' "$suite_results"
export SEAWEEDFS_WINDOWS_LIVE=1 SEAWEEDFS_WINDOWS_MOUNT_UNIT_LIVE=1
export SEAWEEDFS_WINDOWS_MOUNT_LIVE=1 SEAWEEDFS_MIXED_LIVE=1
export SEAWEEDFS_WINDOWS_MOUNT_REPEATS=5
unset SEAWEEDFS_WINDOWS_MOUNT_SCENARIO SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD
unset SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP SEAWEEDFS_WINDOWS_MOUNT_MANAGER_GUID_JUNCTION
# The declared workload uses immutable Windows mappings. The mixed suite still
# requires ordinary read coherence and its immutable-mapping assertions.
unset SEAWEEDFS_MIXED_CACHE_COHERENCE
failed=0
run() {
  local name=$1 pattern=$2 status=0
  (cd test/storage_lab/vm && go test . -run "$pattern" -count=1 -v -timeout=50m) > "$suite_results/$name.log" 2>&1 || status=$?
  printf '%s exit=%s\n' "$name" "$status" | tee -a "$suite_results/status.txt"
  if ((status != 0)); then failed=1; fi
}
run storage-xattr-adapter '^TestWindows(Storage|MountXAttr|Adapter)Lab$'
run git-lfs '^TestWindowsMountLab$'
for registration in '' 1; do
  export SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD="$registration"
  for scenario in MountManagerDirectoryLifecycle MountManagerProcessCrash MountManagerRegistrationRollback; do
    export SEAWEEDFS_WINDOWS_MOUNT_SCENARIO="$scenario"
    unset SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP
    if [[ "$scenario" == MountManagerDirectoryLifecycle ]]; then export SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP=1; fi
    run "$scenario-registration-${registration:-default}" '^TestWindowsMountLab$'
  done
done
unset SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP
export SEAWEEDFS_WINDOWS_MOUNT_REPEATS=1
for scenario in All Conformance; do
  export SEAWEEDFS_WINDOWS_MOUNT_SCENARIO="$scenario"
  run "$scenario" '^TestWindowsMountLab$'
done
unset SEAWEEDFS_WINDOWS_MOUNT_SCENARIO
run mixed '^TestMixedOSMountLab$'
exit "$failed"
