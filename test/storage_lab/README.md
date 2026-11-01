# Storage lab

Runners that execute precompiled SeaweedFS tests and binaries in isolation:
a private network, PID and mount namespace on the host, a disposable
container, or fresh Labcontainers VMs. They extend `test/volume_server`,
`weed/storage` and `test/fuse_integration`; the core regressions are ordinary
Go tests that also run in the race suite without the lab.

None of the runners contacts a cluster, installs a package, mounts a host
disk, reuses a VM or deploys anything, and there is no option to point one at
an endpoint or to turn isolation off. A runner that cannot establish its
isolation fails. The isolation guards against accidents by trusted test
binaries; it is not a sandbox for hostile code.

Every runner requires a reviewed inventory of tests (in `run.py` and the VM
tests) and rejects a run with a missing test, a skip, a failed test, a missing
final PASS, a nonzero exit or a failed isolation probe. Update an inventory by
hand when a test is renamed or added; never generate it from the candidate
binary, which would accept a missing regression. A `-run` regex that matches
nothing is a failure.

`python3 -m unittest discover -s test/storage_lab -v` tests the runners
themselves.

## Namespace runner (`run.py`)

Needs Linux with systemd, cgroup memory, CPU and PID controllers,
`/usr/bin/bwrap` with `--size`, `--perms` and `--remount-ro` (Bubblewrap 0.9,
Ubuntu 24.04), Python 3 and noninteractive sudo for `systemd-run` and
`systemctl`. Run it as your ordinary user: privilege only sets up isolation,
and the payload runs as your UID with no capabilities and no inherited
credentials. A preflight in the sandbox checks loopback-only networking, no
host home, devices or Docker socket, a read-only artifact mount and bounded
temporary storage.

Limits: 4 GiB memory, no swap, 2 CPUs, 256 tasks, 2 GiB tmpfs, 32 MiB of
retained output, an 8 minute test timeout and a 10 minute service lifetime.
Staging needs its own space plus 2 GiB of host headroom. Sparse 64 to 200 GiB
test files use only their written pages. Staging lives under `/tmp`
regardless of `TMPDIR` and is removed afterwards; the manifest and logs stay
in the printed `seaweedfs-lab-results-*` directory, or under
`--results-root`.

Suites: `storage`, `migration` (needs `--candidate` and `--baseline`),
`admission` (needs `--candidate`), `enospc` and `synology-bootstrap`.

```sh
: "${SEAWEEDFS_LAB_ARTIFACTS:?absolute persistent artifact directory}"
LAB_BUILD=$(mktemp -d "$SEAWEEDFS_LAB_ARTIFACTS/seaweedfs-lab-build.XXXXXXXX")
go test -c -tags 5BytesOffset -race -o "$LAB_BUILD/storage.test" ./weed/storage
go test -c -tags 5BytesOffset -o "$LAB_BUILD/grpc.test" ./test/volume_server/grpc
go test -c -tags 5BytesOffset -o "$LAB_BUILD/http.test" ./test/volume_server/http
go build -tags 5BytesOffset -o "$LAB_BUILD/weed" ./weed
python3 test/storage_lab/run.py storage --tests "$LAB_BUILD/storage.test" --results-root "$LAB_BUILD"
python3 test/storage_lab/run.py migration --tests "$LAB_BUILD/grpc.test" \
  --candidate "$LAB_BUILD/weed" --baseline "$BASELINE_BINARY" --results-root "$LAB_BUILD"
python3 test/storage_lab/run.py admission --tests "$LAB_BUILD/http.test" \
  --candidate "$LAB_BUILD/weed" --results-root "$LAB_BUILD"
```

`BASELINE_BINARY` is a 5BytesOffset build of the previously deployed commit
(CI uses `SEAWEEDFS_RELIABILITY_BASELINE_REF`), never `HEAD~1`. Identical
binaries are rejected even at different paths. Manifests record the SHA-256
of every staged binary. The storage suite covers corrupt index preservation,
large sparse offsets, failed source sync, interrupted swap recovery and
repeated overwrite, delete and compaction; migration runs a volume server
through baseline, candidate, baseline, crash and candidate again.

`synology-bootstrap` runs the bootstrap tests of the sibling `spk-seaweedfs`
repository, built there with `go test -c -race -o <absolute-output> .` in
`cmd/synology-volume-bootstrap`.

## Mutant controls

A mutant is a build overlay that reverts one fix in a throwaway copy of the
source, leaving the working tree alone. Its regression must fail with
`--- FAIL:` for that test; a mutant that fails to compile does not count.

`verify_mutants.sh` runs every reviewed control: the offset encoder
(`TestConcurrentWriteCrossesOffsetBoundary`), replay validation,
completed-swap cache invalidation and scanner body-error propagation (from
`compaction_mutant.py`), and two `volume.fsck` controls (missing replica
delete results, ignored filer DELETE failures, from `fsck_mutant.py`). Each
generator refuses a source pattern that changed or became ambiguous; update
the generator when the fixed code moves.

```sh
MUTANT_LOG_DIR=/abs/mutant-logs test/storage_lab/verify_mutants.sh
```

The preallocation control ignores `fallocate` errors and needs a native
filesystem: `TestVolumePreallocateENOSPCFailsCreation` must fail while
`TestCompactENOSPCPreservesOriginal` still passes.

```sh
PREALLOCATE_OVERLAY=$(python3 test/storage_lab/preallocate_mutant.py)
go test -c -tags 5BytesOffset -race -overlay "$PREALLOCATE_OVERLAY" \
  -o "$LAB_BUILD/preallocate-mutant.test" ./weed/storage
MUTANT_LOG_DIR=/abs/mutant-logs test/storage_lab/verify_preallocate_mutant.sh "$LAB_BUILD/preallocate-mutant.test"
```

## Native filesystem runner (`run_filesystem.py`)

Runs the storage suite on a new sparse 8 GiB image formatted XFS or Btrfs,
mounted as `/tmp` inside the same bwrap boundary. The root helper accepts only
a regular, singly linked `/tmp/seaweedfs-fs-lab-*/disk.img` of the expected
size owned by the caller, attaches a new loop device and checks its backing
file before `mkfs`. Afterwards it unmounts, runs `xfs_repair -n` or
`btrfs check --readonly` and detaches the device.

```sh
python3 test/storage_lab/run_filesystem.py xfs --tests "$LAB_BUILD/storage.test"
python3 test/storage_lab/run_filesystem.py btrfs --enospc --tests "$LAB_BUILD/storage.test"
```

`--enospc` uses a 2 GiB image and sets the guard variable only inside the
sandbox; the guarded tests refuse filesystems over 3 GiB. One fills the
filesystem, requires compaction and commit to fail, compares the original
data and index bytes, removes the filler and rereads every acknowledged
payload. The other requests a reservation larger than the filesystem and
requires volume creation to fail without leaving a candidate, while a seeded
volume at the same path stays byte for byte intact. Never set the guard
variable on a host filesystem.

## Server image runner (`run_image.py`)

Runs a local image by ID with an expected packaged `weed` digest; it never
builds, pulls or pushes. The container is read-only, nonroot, loopback-only,
with 512 MiB and 128 MiB tmpfs mounts and a 5 GiB memory limit. Master,
volume, filer and S3 run inside it. Without `--s3-suite` it checks volume
startup and automatic memory sizing. With it, `test/s3/copying` checks GET
payloads, same and cross bucket copy, multipart copy, 40 MiB multipart
completion and readback, and aborting a replacement without changing the
committed object.

```sh
CGO_ENABLED=0 go test -c -o "$LAB_BUILD/s3-copying.test" ./test/s3/copying
python3 test/storage_lab/run_image.py \
  --image-id "$SERVER_IMAGE_ID" --weed-sha256 "$WEED_SHA256" \
  --results-root "$LAB_RESULTS" --s3-suite "$LAB_BUILD/s3-copying.test"
```

## External etcd restore (`run_etcd_restore.sh`)

Runs `TestExternalEtcdSnapshotRestore` against fresh loopback etcd and weed
processes. Inputs: `WEED_BINARY` (absolute candidate) and
`ETCD_RESTORE_RESULTS` (existing directory); `SEAWEEDFS_ETCD_TEST_IMAGE`
overrides the etcd image, which must be pinned by digest. For a mixed-version
filer check also set `WEED_FILER_UPGRADE_BASELINE`,
`WEED_FILER_UPGRADE_BASELINE_SHA256` and `WEED_FILER_UPGRADE_CANDIDATE_SHA256`:
the test writes through the old filer, verifies candidate readback and
unchanged chunk metadata, writes new files from both versions on one etcd
store, then stops the old filer and runs the snapshot and restore checks. An
etcd snapshot holds metadata only, never volume payloads.

## Labcontainers VM gates

The VM tests live in their own module, `test/storage_lab/vm`, which depends on
the Labcontainers SDK. Build them in a temporary Go workspace over this
module and a Labcontainers checkout at the commit pinned in `vm/go.mod`:

```sh
work=$(mktemp -d); (cd "$work" && go work init "$PWD/test/storage_lab/vm" /abs/labcontainers)
export GOWORK="$work/go.work" GOTOOLCHAIN=auto
(cd /abs/labcontainers && go build -o bin/labd ./cmd/labd)
export LABCONTAINERS_LABD=/abs/labcontainers/bin/labd
```

`GOTOOLCHAIN=auto` lets the VM module use the SDK's Go version. VM images are
referenced by `@sha256` digest; the Linux image carries the
`appmana.labcontainers.revision` label, which must equal the SDK commit. Each
test creates fresh VMs connected only to each other (`network-mode: none`, the
`192.0.2.0/24` documentation subnet) and retains logs and a manifest of input
hashes in a printed results directory (`RUNNER_TEMP` in CI).

### Four-VM durability (`vm/run_vm.sh`)

A controller VM and three rack-separated volume-server VMs, each with a new
2 GiB data disk, behind an internal Alpine switch. Inputs:
`LABCONTAINERS_SOURCE` (absolute checkout, default `../labcontainers`, clean
and at the `vm/go.mod` pin), `LABCONTAINERS_REF` (optional full SHA the
checkout must equal), `LABCONTAINERS_CONTAINERLAB` (absolute path to the
Containerlab CLI built by Labcontainers' `scripts/build-containerlab.sh`) and
`LABCONTAINERS_VM_IMAGE`.

```sh
test/storage_lab/vm/run_vm.sh --candidate /abs/weed-candidate \
  --image "$LABCONTAINERS_VM_IMAGE" --filesystem ext4 --scenario replicated-vacuum
test/storage_lab/vm/run_vm.sh --candidate /abs/weed-candidate --baseline /abs/weed-baseline \
  --image "$LABCONTAINERS_VM_IMAGE" --filesystem ext4 --scenario migration
```

Scenarios: `replicated-vacuum`, `power-loss`, `migration` (needs
`--baseline`; upgrades then rolls back one replica at a time, gracefully),
`vacuum-power-loss` and `all`; filesystems `ext4`, `xfs` and `btrfs`. The crash
scenarios call Labcontainers `Crash`, which kills the VM's QEMU. The vacuum
crash waits for the `.cpd` copy, stops the volume server and confirms the copy
is still present before the crash, so a fast copy cannot finish between host
RPCs; the manifest records `vacuum_copy_held`. The manifest also records
per-replica read digests and topology. `--keep` retains a failed session.

### Linux FUSE hang gates

The guest panics on a hung task, so a mount that leaves a process in D state
fails the test. `TestFuseStuckBackendLab` stops the backend under live
requests; `TestFuseReverseInvalidationLab` drives remote changes against
local requests to check the notify gate.

```sh
SEAWEEDFS_STUCK_LAB_LIVE=1 SEAWEEDFS_NOTIFY_LAB_LIVE=1 SEAWEEDFS_LINUX_WEED=/abs/weed \
LABCONTAINERS_VM_IMAGE="$LABCONTAINERS_VM_IMAGE" \
go test ./test/storage_lab/vm -run '^(TestFuseStuckBackendLab|TestFuseReverseInvalidationLab)$' -count=1 -v -timeout=90m
```

Each mode wipes the guest's whole data directory, `.vid_stamp` included. A
stamp left from the previous mode makes the new master see a foreign topology
id and freeze volume growth, and every assign then fails.

### Performance against vanilla upstream

`TestIntervalPerformanceLab` (`SEAWEEDFS_INTERVAL_PERFORMANCE_LIVE=1`) and
`TestIndexRecoveryPerformanceLab` (`SEAWEEDFS_INDEX_PERFORMANCE_LIVE=1`)
compare test binaries built from the candidate and from a vanilla upstream
worktree that received only the benchmark test file
(`interval_performance_test.go`, `needle_map_recovery_performance_test.go`).
Inputs: `SEAWEEDFS_PERFORMANCE_BASELINE`, `SEAWEEDFS_PERFORMANCE_CANDIDATE`
and their `_COMMIT` values. See the reliability workflow for the build steps.

### Mixed Linux and Windows mounts (`TestMixedOSMountLab`)

A Linux VM runs `weed server`; Linux FUSE and Windows WinFsp mount the same
namespace. The Windows guest clock is set from the controller first. The two
clients rendezvous through the mount for seven phases: concurrent creation of
24 files per OS, cross verification, overwrite and truncate of the other OS's
files, verification, cross rename and delete, verification, and verification
after remounting with fresh caches. Sizes include 0, 37, 4097 and 2 MiB + 103
bytes. Only the coordination waits retry; content checks do not.

```sh
CGO_ENABLED=0 go build -o "$LAB_BUILD/mixed-linux" ./test/storage_lab/vm/mixed_workload
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "$LAB_BUILD/mixed-windows.exe" ./test/storage_lab/vm/mixed_workload
SEAWEEDFS_MIXED_LIVE=1 \
SEAWEEDFS_LINUX_WEED="$LAB_BUILD/weed-linux" SEAWEEDFS_WINDOWS_WEED="$LAB_BUILD/weed.exe" \
SEAWEEDFS_MIXED_LINUX_WORKLOAD="$LAB_BUILD/mixed-linux" \
SEAWEEDFS_MIXED_WINDOWS_WORKLOAD="$LAB_BUILD/mixed-windows.exe" \
SEAWEEDFS_WINFSP_MSI=/abs/winfsp.msi SEAWEEDFS_WINDOWS_WINFSP_DLL=/abs/winfsp-x64.dll \
LABCONTAINERS_VM_IMAGE="$LINUX_IMAGE" LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE" \
go test ./test/storage_lab/vm -run '^TestMixedOSMountLab$' -count=1 -v -timeout=30m
```

The DLL needs its adjacent `.manifest.txt` and `.source.patch`. The oracle
and its corruption, truncation, missing file and lost-delete controls run
without VMs: `cd test/storage_lab/vm && GOWORK=off go test -race ./mixed_workload`.

### Native Windows tests

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -tags 5BytesOffset -o /abs/storage.test.exe ./weed/storage
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /abs/mount.test.exe ./weed/mount
SEAWEEDFS_WINDOWS_LIVE=1 SEAWEEDFS_WINDOWS_STORAGE_TEST=/abs/storage.test.exe \
SEAWEEDFS_WINDOWS_MOUNT_UNIT_LIVE=1 SEAWEEDFS_WINDOWS_MOUNT_UNIT_TEST=/abs/mount.test.exe \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE" \
go test ./test/storage_lab/vm -run '^TestWindows(StorageLab|MountXAttrLab|AdapterLab|RemoteDeleteLab)$' -count=1 -v -timeout=35m
```

`TestWindowsStorageLab` runs the durability regressions of `weed/storage` on
NTFS; `TestWindowsMountXAttrLab`, `TestWindowsAdapterLab` and
`TestWindowsRemoteDeleteLab` run `weed/mount` platform tests that need no
WinFsp mount. Each names its tests, and every one must pass without skips.

### WinFsp mount scenarios (`TestWindowsMountLab`)

Runs `hack/appmana/mount-smoke.ps1` and the native `test/winfsp` tests in a
Windows VM with offline installers (the guest has no external network):

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags 5BytesOffset -o /abs/weed.exe ./weed
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /abs/winfsp.test.exe ./test/winfsp
SEAWEEDFS_WINDOWS_MOUNT_LIVE=1 SEAWEEDFS_WINDOWS_MOUNT_REPEATS=5 \
SEAWEEDFS_WINDOWS_WINFSP_TEST=/abs/winfsp.test.exe SEAWEEDFS_WINDOWS_WEED=/abs/weed.exe \
SEAWEEDFS_WINFSP_MSI=/abs/winfsp.msi SEAWEEDFS_GIT_INSTALLER=/abs/git-installer.exe \
SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL=/abs/winfsp-x64.dll SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL_SHA256=<sha256> \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE" \
go test ./test/storage_lab/vm -run '^TestWindowsMountLab$' -count=1 -v -timeout=45m
```

Inputs:

- `SEAWEEDFS_WINDOWS_MOUNT_SCENARIO`: one of `All`, `NativeMetadata`, `AccessPerformance`, `CacheLifecycle`, `Conformance`, `GitAtomicRenamePrimed`, `GitLfsTempMetadata`, `MountManagerDirectoryLifecycle`, `MountManagerProcessCrash`, `MountManagerRegistrationRollback`. Unset runs `GitAtomicRenamePrimed` and `GitLfsTempMetadata`, 20 Git iterations each.
- `SEAWEEDFS_WINDOWS_MOUNT_REPEATS` (1 to 20), `SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY` (0 to 4), `SEAWEEDFS_WINDOWS_MOUNT_OPTIONS` (extra mount flags).
- `SEAWEEDFS_WINDOWS_MOUNT_TRACE=1`: records the checked-in WPR FileIO profile and WinFsp logs; ETL, decoded XML and logs are retrieved with size and SHA-256 checks. Tracing changes scheduling, so keep the untraced failure too.
- `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP=1` and `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD`: graceful same-path junction and volume mapping cleanup, in both registration modes.
- `SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC`: an offline `git-lfs.exe` to compare clients; it replaces Git LFS only in the guest.
- `SEAWEEDFS_WINDOWS_CONFORMANCE_EXE` with `SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS=1` for `Conformance`: upstream `winfsp-tests` under `--fuse-external --resilient` on an `S:` drive beside local NTFS controls.
- WinFsp under test, one of: the released user-mode DLL (`SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL` and its SHA-256, as CI and the CSI image use) with the stock driver from the MSI; a lab DLL (`SEAWEEDFS_WINDOWS_WINFSP_DLL`, with adjacent `.manifest.txt` and `.source.patch`); or a native driver package built by `AppMana/forks-winfsp-fixes`' `tools/lab` (`SEAWEEDFS_WINDOWS_WINFSP_NATIVE_PACKAGE` with `manifest.json`, `winfsp-x64.sys`, `winfsp-x64.dll` and `lab.cer`, plus `SEAWEEDFS_WINDOWS_WINFSP_FORK`). Driver installation, certificate trust and test signing happen only inside the fresh VM. Extract Windows ZIPs with a tool that turns backslashes into directories.

Guest logs are exported to `guest-logs.txt` (the last 2,000 lines of each
top-level log); keep the full Go test output with them.

### Windows access latency comparison

`hack/appmana/windows-access-regression.sh BASELINE_EXE CANDIDATE_EXE` runs
three VMs per build in alternating order, five samples per operation with the
first discarded, and compares medians of per-VM medians for small-file
open, read and close and repeated reads through an open handle. It fails above
a 15% regression (`SEAWEEDFS_WINDOWS_ACCESS_MAX_REGRESSION_PERCENT`). Inputs:
`SEAWEEDFS_WINDOWS_ACCESS_{BASELINE,CANDIDATE}_BASIC_PERMISSIONS` (`0` or `1`;
candidate defaults to the baseline's) and
`SEAWEEDFS_WINDOWS_ACCESS_{BASELINE,CANDIDATE}_DLL` (default
`SEAWEEDFS_WINDOWS_WINFSP_DLL`, then the baseline DLL). Keep
`SEAWEEDFS_WINDOWS_DIAGNOSTIC_HOLD_SECONDS=0`, set `RUNNER_TEMP` to a
persistent directory and run nothing else heavy on the host meanwhile.

The reliability workflow runs the comparison on dispatch when
`windows_access_baseline` names the baseline commit, against the
candidate it builds. Its `windows_mount_options` input passes extra mount
flags (for example `-writebackCache`) to every Windows mount lab run.
