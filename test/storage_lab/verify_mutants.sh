#!/bin/bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$ROOT"
: "${STORAGE_BUILD_TAGS:=5BytesOffset}"
: "${MUTANT_LOG_DIR:=$(mktemp -d /tmp/seaweedfs-mutant-logs.XXXXXXXX)}"
mkdir -p "$MUTANT_LOG_DIR"

expect_red() {
  local name=$1 test_name=$2 overlay log
  overlay=$(python3 test/storage_lab/compaction_mutant.py "$name")
  log="$MUTANT_LOG_DIR/$name.log"
  if go test -tags "$STORAGE_BUILD_TAGS" -count=1 -overlay "$overlay" \
      ./weed/storage -run "^$test_name$" 2>&1 | tee "$log"; then
    echo "$name mutant unexpectedly passed" >&2
    exit 1
  fi
  grep -q -- '--- FAIL:' "$log" || {
    echo "$name failed to compile instead of turning its regression red" >&2
    exit 1
  }
}

expect_red replay-validation TestVacuumPreservesIntactDataWithBadIndex
expect_red completed-swap-cache TestReconcileAfterBothRenamesInvalidatesOldLevelDB
expect_red scanner-body-error TestCompactByVolumeDataFailureCannotCommitCandidate

offset_overlay=$(python3 test/storage_lab/offset_mutant.py)
offset_log="$MUTANT_LOG_DIR/offset.log"
if go test -tags "$STORAGE_BUILD_TAGS" -count=1 -overlay "$offset_overlay" \
    ./weed/storage -run '^TestConcurrentWriteCrossesOffsetBoundary$' 2>&1 | tee "$offset_log"; then
  echo 'offset mutant unexpectedly passed' >&2
  exit 1
fi
grep -q -- '--- FAIL: TestConcurrentWriteCrossesOffsetBoundary' "$offset_log"


expect_fsck_red() {
  local name=$1 test_name=$2 overlay log
  overlay=$(python3 test/storage_lab/fsck_mutant.py "$name")
  log="$MUTANT_LOG_DIR/fsck-$name.log"
  if go test -tags "$STORAGE_BUILD_TAGS" -count=1 -overlay "$overlay" \
      ./weed/shell -run "^$test_name$" 2>&1 | tee "$log"; then
    echo "$name fsck mutant unexpectedly passed" >&2
    exit 1
  fi
  grep -q -- "--- FAIL: $test_name" "$log" || {
    echo "$name fsck mutant failed to compile instead of turning its regression red" >&2
    exit 1
  }
}

expect_fsck_red missing-delete-results TestFsckPurgeRejectsMissingDeleteResults
expect_fsck_red ignored-filer-delete TestVolumeFsckFilerDeleteFailureMakesPurgeIncomplete

echo "all reviewed mutants turned their regressions red; logs: $MUTANT_LOG_DIR"
