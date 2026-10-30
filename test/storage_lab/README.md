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
The qualified hosted image is Ubuntu 24.04 with Bubblewrap 0.9. Both Linux
runners check for `--size`, `--perms`, and `--remount-ro` before staging or
privileged setup. Do not remove bounded tmpfs or read-only artifact options
to accommodate an older installation; update the runner instead.
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
# Choose an existing persistent artifact directory, not a temporary build root.
: "${SEAWEEDFS_LAB_ARTIFACTS:?set an absolute persistent artifact directory}"
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
python3 -m unittest discover -s test/storage_lab -v
```

Both `run.py` and `run_filesystem.py` accept `--results-root` for retained
manifests and logs. Disposable sandbox staging remains under `/tmp` regardless
of `TMPDIR`; it contains only copied test inputs (and disposable filesystem
images), not the source checkout or retained evidence, and is cleaned up.

`BASELINE_BINARY` must be an explicitly built, known deployed-compatible
5BytesOffset executable. Use the README's Actions baseline/go-fuse variables;
do not infer a baseline from HEAD~1. Same-content binaries are rejected even at
different paths. Manifests record SHA-256 hashes of the binaries actually staged.
For an uncommitted candidate retain its source diff too: a HEAD SHA alone does
not identify a dirty build. A skipped gate or a regex matching no tests is a
failure, not a green qualification. These suites are selected regression gates,
not the entire project's tests.

For release provenance, build from a clean standalone clone and verify
`go version -m` contains the expected `vcs.revision` and `vcs.modified=false`.
The Go 1.26 toolchain used in this lab recognizes a `.git` directory but not
the `.git` file in a linked worktree, so `-buildvcs=true` alone did not stamp
those worktree builds. Also record the pinned sibling go-fuse SHA and clean
status: the main module's VCS stamp does not identify a local replacement.
Embed the full source SHA in `version.COMMIT`, and retain the artifact hash.
Rebuilding changes the tested artifact identity and requires fresh qualification.

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
ignores `fallocate` errors in a build overlay, then
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

The crash gates call Labcontainers `Crash`, which kills the session's VM
wrapper and QEMU. The vacuum gate polls for `.cpd`, stops the volume server,
and confirms the copy is still present before requesting the crash. The
manifest records `vacuum_copy_held`. This guest-side barrier prevents a fast
copy from finishing between host RPCs. Migration is graceful and upgrades
then rolls back one replica at a time; abrupt failure is a separate scenario.
XFS and Btrfs guests need their formatting tools. Each scenario gets fresh VMs.

## Mixed Windows and Linux mounted workloads

`TestMixedOSMountLab` reuses Labcontainers to create a fresh Linux VM and a fresh
Windows VM connected only to each other (`network-mode: none`, no default
route). A Linux `weed server` supplies the master, volume server and filer;
Linux FUSE and Windows WinFsp mount that same namespace. This is mounted file
I/O, not two HTTP clients. Inputs are offline, their hashes and resolved VM
image IDs are retained, and the Windows child must load the explicitly supplied
manifest-verified candidate DLL. It never substitutes that DLL on the host.
The offline Windows guest's clock is set from the controller before any mount
starts; both guest Unix times must then be within five seconds of the controller
RPC interval. This changes only the fresh VM, not the workstation or cluster.

Both clients rendezvous through the shared mount for seven paired phases:
concurrent creation of 24 files per OS; cross-client byte/inventory verification;
overwrite/truncate of the other OS's files; another verification; cross-client
rename/delete; final verification; and verification after fresh-cache remounts.
Sizes include zero, 37, 4097 and 2 MiB + 103 bytes. Every writer requires
successful write, fsync and close. Both readers independently regenerate the
expected bytes; missing files, stale names, lost deletes, extra directory
entries, skips or missing run-specific completion markers fail qualification.
Only the coordination barrier/readiness waits retry; content assertions do not.
The Windows mount is terminated after all writes are acknowledged and closed;
that remount check is not graceful-unmount or VM power-loss qualification.

Build both weed executables from the same reviewed application source with
`5BytesOffset` and use the temporary Go workspace described above:

```sh
CGO_ENABLED=0 go build -o "$LAB_BUILD/mixed-linux" ./test/storage_lab/vm/mixed_workload
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "$LAB_BUILD/mixed-windows.exe" ./test/storage_lab/vm/mixed_workload
SEAWEEDFS_MIXED_LIVE=1 \
SEAWEEDFS_LINUX_WEED="$LAB_BUILD/weed-linux" \
SEAWEEDFS_WINDOWS_WEED="$LAB_BUILD/weed.exe" \
SEAWEEDFS_MIXED_LINUX_WORKLOAD="$LAB_BUILD/mixed-linux" \
SEAWEEDFS_MIXED_WINDOWS_WORKLOAD="$LAB_BUILD/mixed-windows.exe" \
SEAWEEDFS_WINFSP_MSI=/absolute/winfsp.msi \
SEAWEEDFS_WINDOWS_WINFSP_DLL=/absolute/candidate-winfsp-x64.dll \
LABCONTAINERS_LABD=/absolute/labcontainers/bin/labd \
LABCONTAINERS_VM_IMAGE="$LINUX_IMAGE_AT_DIGEST" \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE_AT_DIGEST" \
go test ./test/storage_lab/vm -run '^TestMixedOSMountLab$' -count=1 -v -timeout=30m
```

The DLL's adjacent `.manifest.txt` and `.source.patch` are required. Results
remain in `seaweedfs-mixed-results-*`: provenance, topology, phase logs, guest
diagnostics and a final manifest requiring all seven paired phases.  Fast oracle tests run in the hosted Linux job, including deliberate
corruption, truncation, missing/extra files and a lost-delete negative control.
They are not VM passes. This coverage does not establish same-file concurrent
write/locking semantics, case-collision behavior, CSI package lifecycle, mixed
server OS deployments, replication durability, or Synology compatibility.

## Native Windows storage regressions

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

The same VM runner can execute the raw-mount xattr platform contracts and
retained-open-file/directory metadata tests (no WinFsp mount or MSI needed):

```sh
GOWORK=off GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go test -c -tags 5BytesOffset -o /absolute/mount.test.exe ./weed/mount
SEAWEEDFS_WINDOWS_MOUNT_UNIT_LIVE=1 \
SEAWEEDFS_WINDOWS_MOUNT_UNIT_TEST=/absolute/mount.test.exe \
LABCONTAINERS_LABD=/absolute/labcontainers/bin/labd \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE_AT_DIGEST" \
go test ./test/storage_lab/vm -run '^TestWindowsMountXAttrLab$' -count=1 -v -timeout=20m
```

The inventory rejects omissions and skips. Raw-mount xattrs return `ENOTSUP`
on Windows and FreeBSD; tests require unchanged metadata and output buffers.
The Windows CI job runs these tests in both CGO modes.

For WinFsp/Git runtime coverage in a disposable Windows VM, supply the candidate
and offline installers from the host (the guest has no external network):

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /absolute/winfsp.test.exe ./test/winfsp
SEAWEEDFS_WINDOWS_MOUNT_LIVE=1 \
SEAWEEDFS_WINDOWS_MOUNT_REPEATS=5 \
SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY=1 \
SEAWEEDFS_WINDOWS_WINFSP_TEST=/absolute/winfsp.test.exe \
SEAWEEDFS_WINDOWS_WEED=/absolute/weed.exe \
SEAWEEDFS_WINFSP_MSI=/absolute/winfsp.msi \
SEAWEEDFS_GIT_INSTALLER=/absolute/git-installer.exe \
LABCONTAINERS_LABD=/absolute/labcontainers/bin/labd \
LABCONTAINERS_WINDOWS_IMAGE="$WINDOWS_IMAGE_AT_DIGEST" \
go test ./test/storage_lab/vm -run '^TestWindowsMountLab$' -count=1 -v -timeout=45m
```

Use the same temporary Go workspace described above. This runs the existing
`hack/appmana/mount-smoke.ps1` scenarios for 20 iterations each, checks Git LFS
prerequisites and all 32 modified assets, and rejects missing completion markers.
The test logs input hashes and retains combined scenario stdout/stderr under the
printed results directory (`RUNNER_TEMP` in CI, system temp otherwise). Guest
logs are exported to `guest-logs.txt` (last 2,000 lines per top-level log file;
not a complete recursive archive). Archive the complete Go test output as well.

For a paired Windows ordinary-access latency comparison, keep the same matched
SDK/daemon, VM image, offline installers, native test executable and host. Set
`RUNNER_TEMP` to a persistent artifact directory and run no other qualification
VMs or heavy builds concurrently. Supply two reviewed Windows executables:

```sh
export SEAWEEDFS_WINDOWS_ACCESS_BASELINE_BASIC_PERMISSIONS=0
export SEAWEEDFS_WINDOWS_ACCESS_CANDIDATE_BASIC_PERMISSIONS=1
export SEAWEEDFS_WINDOWS_ACCESS_BASELINE_DLL=/absolute/baseline-winfsp-x64.dll
export SEAWEEDFS_WINDOWS_ACCESS_CANDIDATE_DLL=/absolute/candidate-winfsp-x64.dll
bash hack/appmana/windows-access-regression.sh /absolute/baseline-weed.exe /absolute/candidate-weed.exe
```

Each DLL requires adjacent `.manifest.txt` and `.source.patch` files. The policy
inputs accept only `0`/`1`; baseline defaults to `0`, and candidate defaults to
the baseline policy. DLL inputs default to `SEAWEEDFS_WINDOWS_WINFSP_DLL` and
then the baseline DLL respectively. Set both explicitly when comparing native
changes. `SEAWEEDFS_WINDOWS_MOUNT_OPTIONS`, if supplied, applies to both builds.
Keep `SEAWEEDFS_WINDOWS_DIAGNOSTIC_HOLD_SECONDS=0` for this scenario.
The script disables tracing/debug verbosity and runs three independent VMs per
build in alternating order. Each workload checks every returned byte, records
five samples per operation, and discards only the predefined first warm-up
sample. The gate compares medians of per-VM medians, rejecting missing samples,
failed outer VM tests and absent scenario completion markers. The default
maximum latency regression is 15%; set
`SEAWEEDFS_WINDOWS_ACCESS_MAX_REGRESSION_PERCENT` before the campaign if another
budget is required. Retain `windows-access-regression.*` together with the VM
result directories. This measures small-file open/read/close and repeated
reads through an open handle, not bulk throughput or complete qualification.
Use the pre-change Windows fork build to isolate permission overhead; do not
label that comparison as a vanilla-upstream baseline.

For the strict upstream WinFsp inventory, reuse those offline VM inputs and set
`SEAWEEDFS_WINDOWS_MOUNT_SCENARIO=Conformance`,
`SEAWEEDFS_WINDOWS_MOUNT_REPEATS=1`,
`SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS=1`, and
`SEAWEEDFS_WINDOWS_CONFORMANCE_EXE=/absolute/winfsp-tests-x64.exe` before running
`TestWindowsMountLab`. Use the qualified basic-permissions DLL. The scenario
runs local NTFS controls and mounts SeaweedFS on an unused `S:` drive; it includes
optional and known-failure cases under upstream's `--fuse-external --resilient`
contract. This is separate from the native directory-mounted `All` suite and
does not certify arbitrary ACL fidelity. Actions requires the conformance EXE
path/hash variables documented in the root README.

To qualify an actual patched kernel driver rather than a DLL with the stock
driver, build the native package using
[AppMana/forks-winfsp-fixes](https://github.com/AppMana/forks-winfsp-fixes)'s
`tools/lab` harness, then use the same `TestWindowsMountLab` scenarios:

```sh
unset SEAWEEDFS_WINDOWS_WINFSP_DLL
export SEAWEEDFS_WINDOWS_WINFSP_NATIVE_PACKAGE=/absolute/extracted/output
export SEAWEEDFS_WINDOWS_WINFSP_FORK=/absolute/forks-winfsp-fixes
```

The package directory must contain `manifest.json`, `winfsp-x64.sys`,
`winfsp-x64.dll`, and `lab.cer`. Preserve Windows ZIP paths when extracting;
some Linux extractors leave backslashes in filenames instead of creating
directories. Driver/DLL hashes, certificate thumbprint, and source revisions
are checked before starting the VM. The committed installer from the supplied
fork's HEAD is captured with its own revision and hash. Installation, lab
certificate trust and test-signing changes occur only inside the fresh VM;
the harness reboots it and verifies the running driver before invoking the
unchanged mounted, Git/LFS and conformance tests. Do not set both the native
package and DLL-only inputs. These lab-signed packages must never be installed
on cluster hosts; the existing CSI MSI pins remain unchanged.

Optional Windows inputs select focused scenarios without changing the default
gate. Set `SEAWEEDFS_WINDOWS_MOUNT_SCENARIO` to the scenario implemented in
`vm/windows_mount_test.go` and provide `SEAWEEDFS_WINDOWS_WINFSP_TEST` for native
contract tests. Mount-manager lifecycle, process-crash and rollback scenarios
exercise registration and cleanup; they are separate from storage durability.
`SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP=1` checks graceful same-path
junction and volume mapping cleanup. All assertions remain strict: diagnostic
queries do not retry or repair a failed mount assertion.

`SEAWEEDFS_WINDOWS_MOUNT_REPEATS` accepts 1..20 repetitions within the existing
40-minute budget. `SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY` accepts 0..4. Set
`SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC` to an offline Windows `git-lfs.exe` for
client comparisons. The runner replaces it only in the guest, checks hashes,
and preserves Git's hard-linked launcher by unlinking the LFS directory entry
before copying. Dependency setup logs are retained on failure.

For FileIO diagnostics, set `SEAWEEDFS_WINDOWS_MOUNT_TRACE=1` or dispatch Actions
with `windows_trace`. The guest records the checked-in WPR profile and WinFsp
logs. Cleanup stops only the recorder it started. ETL, decoded XML ZIP and full
WinFsp logs are retrieved with size and SHA-256 checks. Missing or incomplete
artifacts fail the run. Tracing changes scheduling; preserve untraced failures
and inspect trace-loss counters before interpreting a capture.

The Actions workflow's `windows_mount_cleanup` input runs graceful cleanup and
process-crash checks in fresh VMs for both registration modes. Native driver
packages, DLL-only comparisons and stock MSI runs must retain distinct artifact
identities. None qualifies a host installation or CSI deployment.

The core regressions are normal Go tests under `weed/storage`, `weed/shell`, and
`weed/storage/needle`; they run in the existing large-disk race suite even when
the namespace lab is unavailable. CI additionally runs the isolated storage,
migration and admission binaries, then uploads their manifests/logs. New bug
fixes must follow red → implementation → green: retain the pre-fix failure log,
make the smallest core-code change, run the focused test repeatedly, and only
then run broader suites. Do not weaken an assertion merely to accept the old
behavior. Reviewed mutants continuously verify that the tests still go red if
each fixed behavior is removed.

Namespace and native-filesystem runners require the reviewed minimum test
inventories in `run.py`, not merely one PASS line. Their manifests record
required, passed and missing tests. Update an inventory deliberately when a
test is renamed or added; never regenerate it from the candidate binary,
which would silently accept a missing regression. Any skip, failed test,
missing final PASS, nonzero exit or missing isolation probe rejects the run.

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
flush durability nor XFS/Btrfs behavior. The Labcontainers ext4 gate requests
abrupt VM loss and recreation; the `.dat`-before-index durability gap has
separate regression coverage. It does not model drive/controller caches that lie about flushes, and
broader XFS/Btrfs fault and migration tests remain outstanding. Never power-cut production.
Restore drills use copied backups, new cluster identities, and blocked production
networking. An etcd snapshot alone does not contain volume payloads.

`run_etcd_restore.sh` runs the existing real-process external-etcd restore
test with fresh loopback stores. Set `WEED_BINARY` to the absolute candidate
binary and `ETCD_RESTORE_RESULTS` to an existing persistent artifact directory.
For a mixed-version filer migration gate, also set
`WEED_FILER_UPGRADE_BASELINE` to the previous binary and provide both
`WEED_FILER_UPGRADE_BASELINE_SHA256` and
`WEED_FILER_UPGRADE_CANDIDATE_SHA256`. Identical binaries and mismatched hashes
are rejected. The test creates data through the old filer, verifies candidate
readback and unchanged chunk metadata, and verifies bidirectional new-file
writes while both versions share the same disposable etcd store. It then stops
the old filer and performs the snapshot/restore checks with the candidate.
Each filer has a separate local working store. This tests metadata and payload
compatibility, not cache invalidation of already-open files, production data,
multi-member etcd failover, or VM power loss. Both completion markers are
required when the mixed-version mode is enabled.

## Existing server image and S3 payload qualification

`run_image.py` accepts an immutable **local** image ID and expected packaged
`weed` digest; it never builds, pulls or publishes an image. The existing
storage reliability workflow packages its already-built candidate and runs
this gate. To repeat it without rebuilding the application:

```sh
CGO_ENABLED=0 go test -c -o "$LAB_BUILD/s3-copying.test" ./test/s3/copying
python3 test/storage_lab/run_image.py \
  --image-id "$SERVER_IMAGE_ID" --weed-sha256 "$WEED_SHA256" \
  --results-root "$LAB_RESULTS" --s3-suite "$LAB_BUILD/s3-copying.test"
```

The read-only, nonroot container has no network interface beyond loopback,
published ports, production credentials or writable host mounts. Its master,
volume, filer and S3 processes communicate on loopback. Only the static test
executable is bind-mounted, read-only. Data and temporary files use bounded
512 MiB and 128 MiB tmpfs mounts; the container has a 5 GiB memory limit. Cleanup
removes only its randomly named container and retains logs, image identity,
test executable hash and the required test inventory under `--results-root`.

The optional S3 suite reuses `test/s3/copying`: actual GET payload comparisons,
same/cross-bucket copy, multipart copy, 40 MiB multipart completion and readback,
and aborting a replacement without changing the committed object. Missing tests,
skips, test failures or a missing terminal PASS reject qualification. This is
protocol/payload and image-entrypoint coverage, not disk power-loss, external
metadata-store restore, a production registry workload or Windows qualification.
Omitting `--s3-suite` runs only the volume startup/automatic-memory gate.

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
runner is not supported; never set its guard variable on a host filesystem.

This qualifies native filesystem behavior on a file-backed block device, not
physical-drive firmware, controller caches, actual power loss, DSM Btrfs, or
the production mount options. CI runs both filesystems and retains manifests.

Space accounting must distinguish live object bytes, replication, tombstones,
temporary copies, filesystem allocation and genuinely unreferenced data. A
physical/live difference alone never authorizes deletion. Reclamation remains
disabled for live data until references and preservation checks are proven.
