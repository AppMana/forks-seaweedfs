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
The manifest records binary SHA-256 values, VM image identity, filesystem,
scenario, timestamps, per-replica verified read digests, topology, and that
production access was disabled. Results remain in the
printed `/tmp/seaweedfs-vm-lab-results-*` directory.

The crash gates now call Labcontainers `Crash`, which sends SIGKILL to the
session's VM wrapper and QEMU. Earlier `PowerOff` results exercised Containerlab
stop/restart and must not be treated as verified hard-power-loss evidence.
The vacuum gate uses a `wait_exec` timeline predicate to observe the victim's
`.cpd` before requesting a crash. This proves observation order, but cannot
guarantee the copy is still active when SIGKILL arrives; an application barrier
is needed for an exact instruction boundary. The migration gate is
deliberately graceful and tests baseline → candidate → baseline → candidate one
replica at a time; abrupt failure is tested separately. This matters for the
4.40 baseline: a hard cut can expose its pre-fix acknowledged-index durability
defect and is not a valid rollback procedure.

Earlier ext4 VM results used stop/restart; they do not qualify the revised
hard-crash implementation. The first hard-crash recovery attempt exposed a
Labcontainers restart bug: recreated switch veths lost bridge membership,
preventing the volume server from reaching its master. This is now fixed in
Labcontainers, with an independent live RED → GREEN connectivity regression;
the SeaweedFS caller does not repair the switch.

On September 22, fresh `vacuum-power-loss` runs passed on ext4, XFS and Btrfs
with candidate SHA-256
`22b6311348f33e05ed33fe57d3e6f040b2be3be7e7acc88f600a0fc196846412`.
Each run verified the overwritten object at sequence 1159 and nineteen other
live objects at sequence 0 on all three replicas after recovery. Local evidence:
`/tmp/seaweedfs-vm-lab-results-2896771488` (ext4),
`/tmp/seaweedfs-vm-lab-results-120682410` (XFS), and
`/tmp/seaweedfs-vm-lab-results-3312194925` (Btrfs). These runs used the local VM
image; immutable CI image promotion remains separate. The XFS/Btrfs image must
include their formatting tools. CI runs six fresh Linux sessions: the four
ext4 scenarios above, plus XFS and Btrfs `vacuum-power-loss`, so scenarios cannot
contaminate one another. This is not all-phase or all-filesystem migration coverage.
The separate ext4 acknowledged-write `power-loss` run also passed; its manifest
at `/tmp/seaweedfs-vm-lab-results-716489505` records all nine pre/post-crash replica
reads and Ubuntu image ID
`sha256:21912f8a90fb3fb5ff965394a2921894d73e54b5ef7ae1b3c27f81dabe81e9a1`.

For native Windows storage regressions, build the existing suite with
`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -tags 5BytesOffset -o /absolute/storage.test.exe ./weed/storage`.
Use the same temporary Go workspace as the VM runner (include this module and
the pinned Labcontainers checkout), then run:

```sh
SEAWEEDFS_WINDOWS_LIVE=1 \
SEAWEEDFS_WINDOWS_STORAGE_TEST=/absolute/storage.test.exe \
LABCONTAINERS_LABD=/absolute/labcontainers/bin/labd \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE_AT_DIGEST" \
go test ./test/storage_lab/vm -run '^TestWindowsStorageLab$' -count=1 -v -timeout=18m
```

The harness boots a new isolated Windows VM, uploads the compiled tests, and
requires every selected test to be listed and pass without skips. This verifies
core NTFS behavior; WinFsp mounts, CSI, and packaged service upgrades are separate
runtime gates. Labcontainers also provides `TestLiveWindows` for NTFS crash persistence.

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
| Linux | Namespace/native-filesystem runners; volume-server harness; Labcontainers four-VM ext4/XFS/Btrfs vacuum crash recovery | Exact-phase barriers, broader filesystem fault/migration matrix, allocation accounting, CSI, injected EIO, 24-hour soak |
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
flush durability nor XFS/Btrfs behavior. The Labcontainers ext4 gate now requests
abrupt VM loss and recreation; the `.dat`-before-index durability gap has
separate regression coverage. Do not attribute earlier stop/restart results
to this stronger fault model. It does not model drive/controller caches that lie about flushes, and
broader XFS/Btrfs fault and migration tests remain outstanding. Never power-cut production.
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
