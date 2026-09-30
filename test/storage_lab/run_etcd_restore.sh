#!/usr/bin/env bash
# Real external-store snapshot/restore against fresh loopback-only services.
set -euo pipefail
: "${WEED_BINARY:?set the absolute candidate executable path}"
: "${ETCD_RESTORE_RESULTS:?set an existing persistent results directory}"
[[ "$WEED_BINARY" = /* && -x "$WEED_BINARY" && "$ETCD_RESTORE_RESULTS" = /* && -d "$ETCD_RESTORE_RESULTS" ]]
etcd_image=${SEAWEEDFS_ETCD_TEST_IMAGE:-registry.k8s.io/etcd@sha256:21d2177d708b53ac0fbd1c073c334d58f913eb75da293ff086610e61af03630a}
[[ "$etcd_image" =~ @sha256:[0-9a-f]{64}$ ]]
run_dir=$(mktemp -d "$ETCD_RESTORE_RESULTS/etcd-restore.XXXXXXXX")
input_container=
cleanup() {
  if [[ -n "$input_container" ]]; then docker rm "$input_container" >/dev/null; fi
}
trap cleanup EXIT
docker pull "$etcd_image"
input_container=$(docker create --network none "$etcd_image" /usr/local/bin/etcd --version)
docker cp "$input_container:/usr/local/bin/etcd" "$run_dir/etcd"
docker cp "$input_container:/usr/local/bin/etcdctl" "$run_dir/etcdctl"
docker rm "$input_container" >/dev/null
input_container=
printf '%s\n' "$etcd_image" > "$run_dir/image.txt"
sha256sum "$WEED_BINARY" "$run_dir/etcd" "$run_dir/etcdctl" > "$run_dir/inputs.sha256"
"$WEED_BINARY" version > "$run_dir/weed-version.txt"
"$run_dir/etcdctl" version > "$run_dir/etcd-version.txt"
export SEAWEEDFS_ETCD_RESTORE=1 ETCD_BINARY="$run_dir/etcd" ETCDCTL_BINARY="$run_dir/etcdctl"
export VOLUME_SERVER_IT_KEEP_LOGS=1 TMPDIR="$run_dir"
go test -tags="${STORAGE_BUILD_TAGS:-5BytesOffset}" -count=1 -v -timeout=4m \
  ./test/volume_server/framework -run '^TestExternalEtcdSnapshotRestore$' 2>&1 | tee "$run_dir/test.log"
grep -q 'ETCD_RESTORE_COMPLETE' "$run_dir/test.log"
printf 'Restore evidence: %s\n' "$run_dir"
