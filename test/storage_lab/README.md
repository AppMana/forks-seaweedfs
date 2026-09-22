# Storage qualification lab

This extends, rather than replaces, `test/volume_server` and `weed/storage`.
The Linux runner executes **precompiled** tests in a private network/PID/mount
namespace. It never contacts a live cluster, installs a package, mounts a host
disk, starts an existing VM, or deploys a release. It fails if isolation cannot
be established. It is an accidental-damage boundary for trusted test binaries,
not a sandbox for hostile code or a substitute for a separate physical host.

## Requirements and boundaries

Linux with systemd, cgroup memory/CPU/PID controllers, `/usr/bin/bwrap`, Python 3,
and noninteractive sudo for systemd-run/systemctl. Run as your ordinary user.
Privilege establishes isolation; the test payload runs as that user's UID/GID
with zero capabilities. A preflight inside the same sandbox verifies loopback
only, no host home/devices/Docker socket, a read-only artifact mount, and bounded
temporary storage. No host environment credentials are inherited.

Ceilings live in `run.py`: 4 GiB memory, no swap, 2 CPUs of quota, 256 tasks,
2 GiB tmpfs, 32 MiB retained output, 8-minute test timeout and 10-minute service lifetime. Artifact
staging checks for its required space plus 2 GiB host headroom. Tests write only
inside the sandbox; selected executable copies are read-only. The host receives
the manifest and stdout/stderr log, not test volumes. Staging copies are removed
afterward. Results remain in the printed `seaweedfs-lab-results-*` directory.
Sparse 64–200 GiB files consume only their written pages, within the tmpfs limit.

Do not increase limits without checking host capacity. Building is a separate
step and may download dependencies: apply resource limits to builds too. Never
pass a production volume directory. There is intentionally no endpoint argument
or option to disable isolation. No automatic privileged fallback to host tests.

## Build and run

From the fork root, with its pinned sibling go-fuse checkout present:

```sh
LAB_BUILD=$(mktemp -d /tmp/seaweedfs-lab-build.XXXXXXXX)
go test -c -tags 5BytesOffset -race -o "$LAB_BUILD/storage.test" ./weed/storage
go test -c -tags 5BytesOffset -o "$LAB_BUILD/grpc.test" ./test/volume_server/grpc
go test -c -tags 5BytesOffset -o "$LAB_BUILD/http.test" ./test/volume_server/http
go build -tags 5BytesOffset -o "$LAB_BUILD/weed" ./weed
python3 test/storage_lab/run.py storage --tests "$LAB_BUILD/storage.test"
python3 test/storage_lab/run.py migration --tests "$LAB_BUILD/grpc.test" \
  --candidate "$LAB_BUILD/weed" --baseline "$BASELINE_BINARY"
python3 test/storage_lab/run.py admission --tests "$LAB_BUILD/http.test" \
  --candidate "$LAB_BUILD/weed"
python3 -m unittest discover -s test/storage_lab -v
```

`BASELINE_BINARY` must be an explicitly built, known deployed-compatible
5BytesOffset executable. Use the README's Actions baseline/go-fuse variables;
do not infer a baseline from HEAD~1. Same-content binaries are rejected even at
different paths. Manifests record SHA-256 hashes of the binaries actually staged.
For an uncommitted candidate retain its source diff too: a HEAD SHA alone does
not identify a dirty build. A skipped gate or a regex matching no tests is a
failure, not a green qualification. These suites are selected regression gates,
not the entire project's tests.

## Failure reproduction and release gates

The unchanged upstream `TestConcurrentWriteCrossesOffsetBoundary` must fail if
the #11411 offset correction is reverted. Use a **Go build overlay** to revert
only that line in a throwaway source copy; do not edit the active working tree
or deploy the mutant. Compile that overlay with the same tags and run the same
storage suite. Preserve the failing and fixed manifests/logs. This is a
controlled negative test, not a claim to have tested an untouched old release.

```sh
OFFSET_OVERLAY=$(python3 test/storage_lab/offset_mutant.py)
go test -c -tags 5BytesOffset -race -overlay "$OFFSET_OVERLAY" \
  -o "$LAB_BUILD/offset-mutant.test" ./weed/storage
# EXPECT FAILURE, including TestConcurrentWriteCrossesOffsetBoundary:
python3 test/storage_lab/run.py storage --tests "$LAB_BUILD/offset-mutant.test"
```

The generator refuses changed/ambiguous source patterns. It leaves the active
source untouched and prints the location of its retained temporary overlay.
`test/storage_lab/verify_mutants.sh` automates all reviewed negative controls:
the offset encoder, replay validation, completed-swap cache invalidation, and
scanner body-error propagation, plus `volume.fsck` missing replica-delete
responses and ignored filer DELETE failures. Each deliberately broken overlay
must compile and make its exact regression turn red; a compile failure is not
accepted as a negative-control pass. The reliability workflow runs these
controls and retains their logs. Run the same entry point locally with:

```sh
MUTANT_LOG_DIR=/tmp/seaweedfs-mutant-logs test/storage_lab/verify_mutants.sh
```

Linux preallocation has a separate native-filesystem negative control. It
restores the historical ignored `fallocate` error in a build overlay, then
requires the guarded ENOSPC regression to fail while the adjacent compaction
preservation test still passes:

```sh
PREALLOCATE_OVERLAY=$(python3 test/storage_lab/preallocate_mutant.py)
go test -c -tags 5BytesOffset -race -overlay "$PREALLOCATE_OVERLAY" \
  -o "$LAB_BUILD/preallocate-mutant.test" ./weed/storage
MUTANT_LOG_DIR=/tmp/seaweedfs-mutant-logs test/storage_lab/verify_preallocate_mutant.sh "$LAB_BUILD/preallocate-mutant.test"
```

The suite covers corrupt-index preservation, large sparse offsets, failed
source sync, interrupted swap recovery, repeated overwrite/delete/compaction,
and a volume-server baseline → candidate → baseline → crash → candidate cycle.
The latter is **not** whole-cluster rolling-upgrade qualification.

## Labcontainers four-VM fault runner

`vm/run_vm.sh` uses the shared Labcontainers SDK instead of maintaining another
VM lifecycle implementation. Every invocation creates a controller VM, three
rack-separated volume-server VMs, three new 2 GiB data disks, and an internal
Alpine switch. Every node uses `network-mode: none`; the only network is the
documentation subnet `192.0.2.0/24`. No production endpoint is accepted.

Use an absolute Labcontainers checkout and exact candidate/baseline binaries:

```sh
export LABCONTAINERS_SOURCE=/absolute/path/to/labcontainers
export LABCONTAINERS_REF="$LABCONTAINERS_SHA" # full 40-character commit
test/storage_lab/vm/run_vm.sh --candidate /absolute/bin/weed-candidate \
  --image "$LABCONTAINERS_VM_IMAGE_AT_DIGEST" --filesystem ext4 \
  --scenario replicated-vacuum
test/storage_lab/vm/run_vm.sh --candidate /absolute/bin/weed-candidate \
  --image "$LABCONTAINERS_VM_IMAGE_AT_DIGEST" --filesystem ext4 \
  --scenario power-loss
test/storage_lab/vm/run_vm.sh --candidate /absolute/bin/weed-candidate \
  --baseline /absolute/bin/weed-baseline \
  --image "$LABCONTAINERS_VM_IMAGE_AT_DIGEST" --filesystem ext4 \
  --scenario migration
test/storage_lab/vm/run_vm.sh --candidate /absolute/bin/weed-candidate \
  --image "$LABCONTAINERS_VM_IMAGE_AT_DIGEST" --filesystem ext4 \
  --scenario vacuum-power-loss
```

`LABCONTAINERS_REF` makes the runner reject a checkout whose `HEAD` differs.
The image should likewise use a registry digest, not the local default tag.
The manifest records binary SHA-256 values, filesystem, scenario, timestamps,
topology, and that production access was disabled. Results remain in the
printed `/tmp/seaweedfs-vm-lab-results-*` directory.

The power-loss gates call Labcontainers `PowerOff`, which destroys and
recreates the VM wrapper rather than merely killing `weed` while retaining the
guest page cache. The vacuum gate refuses to cut power unless it observes the
victim `.cpd` file, proving the copy phase was active. The migration gate is
deliberately graceful and tests baseline → candidate → baseline → candidate one
replica at a time; abrupt failure is tested separately. This matters for the
4.40 baseline: a hard cut can expose its pre-fix acknowledged-index durability
defect and is not a valid rollback procedure.

Ext4 is the currently verified VM filesystem. `--filesystem xfs` and
`--filesystem btrfs` require those formatting tools in the pinned image; until
those runs pass, the native runner below is XFS/Btrfs evidence, not VM
power-loss qualification. CI runs four fresh sessions so scenarios cannot
contaminate one another.

The core regressions are normal Go tests under `weed/storage`, `weed/shell`, and
`weed/storage/needle`; they run in the existing large-disk race suite even when
the namespace lab is unavailable. CI additionally runs the isolated storage,
migration and admission binaries, then uploads their manifests/logs. New bug
fixes must follow red → implementation → green: retain the pre-fix failure log,
make the smallest core-code change, run the focused test repeatedly, and only
then run broader suites. Do not weaken an assertion merely to accept the old
behavior. Reviewed mutants continuously verify that the tests still go red if
each fixed behavior is removed.

| Target | Existing infrastructure to reuse | Still required before release |
| --- | --- | --- |
| Linux | Namespace/native-filesystem runners; volume-server harness; Labcontainers four-VM ext4 topology and abrupt-power model | XFS/Btrfs VM power tests, allocation accounting, CSI, injected EIO, 24-hour soak |
| Windows | `.github/workflows/appmana-weed-windows.yml`, `test/winfsp`, WinFsp conformance | Native packaged artifact/service and CSI tests, UNC/flush behavior, upgrade/rollback; cross-compilation does not qualify runtime |
| Synology SPK | Sibling `spk-seaweedfs/tests`, bootstrap Go tests, `lab/dsm` | Audit/re-isolate existing DSM scripts before use; actual SPK install/upgrade/uninstall-preserves-data, correct DSM/architecture and Btrfs coverage |

To run compiled Synology bootstrap tests (including local fake Kubernetes/OCI
servers), build them in that repo with `go test -c -race -o <absolute-output> .`
from `cmd/synology-volume-bootstrap`, then use this runner's
`synology-bootstrap --tests <absolute-output>` suite. This is package component
coverage, not a DSM installation test. Package metadata/source/supervisor tests
remain in the sibling repository; do not change its release pins just to get a
candidate label without building and testing the corresponding SPK.

The existing DSM provisioning scripts can wipe their configured virtual disk
and install cluster credentials. They are **not approved by this runner**.
Do not run them against the live NAS or assume their existing network is isolated.

Process kills leave the host page cache intact. Tmpfs tests prove neither disk
flush durability nor XFS/Btrfs behavior. The Labcontainers ext4 gate now tests
abrupt VM loss and recreation; it found a real `.dat`-before-index durability
gap. It still does not model drive/controller caches that lie about flushes, and
XFS/Btrfs VM power tests remain outstanding. Never power-cut production.
Restore drills use copied backups, new cluster identities, and blocked production
networking. An etcd snapshot alone does not contain volume payloads.

## Native Linux filesystem runner

`run_filesystem.py` runs the storage suite on a new sparse 8 GiB image formatted
as XFS or Btrfs. Its root helper accepts only the exact
`/tmp/seaweedfs-fs-lab-*/disk.img` shape, a regular non-symlink owned by the
invoking user with one link and the exact expected size. It attaches a new loop
device and verifies that device's backing file before `mkfs`; it never accepts a
device path. The test payload sees the mounted filesystem as `/tmp` inside the
same no-network/no-host-root bwrap boundary and has no loop device access.
Afterward the runner unmounts and performs `xfs_repair -n` or
`btrfs check --readonly`, then detaches the loop device. Traps and the random
systemd unit clean up on error. A failed boundary probe is a failed run.

```sh
python3 test/storage_lab/run_filesystem.py xfs --tests "$LAB_BUILD/storage.test"
python3 test/storage_lab/run_filesystem.py btrfs --tests "$LAB_BUILD/storage.test"
python3 test/storage_lab/run_filesystem.py xfs --enospc --tests "$LAB_BUILD/storage.test"
python3 test/storage_lab/run_filesystem.py btrfs --enospc --tests "$LAB_BUILD/storage.test"
```

The ENOSPC mode uses a separate 2 GiB image and exports its guard variable only
inside the sandbox. The Go regression independently refuses any filesystem over
3 GiB. One test fills only its disposable filesystem, requires compaction and
commit to fail, compares original data/index bytes, removes the filler, and
rereads every acknowledged payload. The other requests a reservation larger
than the entire disposable filesystem and requires volume creation to return an
error without leaving a newly created candidate; a seeded volume at the same
path must remain byte-for-byte intact. Running either guarded test outside this

This qualifies native filesystem behavior on a file-backed block device, not
physical-drive firmware, controller caches, actual power loss, DSM Btrfs, or
the production mount options. CI runs both filesystems and retains manifests.

Space accounting must distinguish live object bytes, replication, tombstones,
temporary copies, filesystem allocation and genuinely unreferenced data. A
physical/live difference alone never authorizes deletion. Reclamation remains
disabled for live data until references and preservation checks are proven.
