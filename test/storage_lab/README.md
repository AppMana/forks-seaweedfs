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
session's VM wrapper and QEMU.
The vacuum gate uses a `wait_exec` timeline predicate to observe the victim's
`.cpd` before requesting a crash. This proves observation order, but cannot
guarantee the copy is still active when SIGKILL arrives; an application barrier
is needed for an exact instruction boundary. The migration gate is
deliberately graceful and tests baseline → candidate → baseline → candidate one
replica at a time; abrupt failure is tested separately. This matters for the
4.40 baseline: a hard cut can expose its pre-fix acknowledged-index durability
defect and is not a valid rollback procedure.

The XFS/Btrfs image must
include their formatting tools. CI runs six fresh Linux sessions: the four
ext4 scenarios above, plus XFS and Btrfs `vacuum-power-loss`, so scenarios cannot
contaminate one another. This is not all-phase or all-filesystem migration coverage.

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
diagnostics and a final manifest requiring all seven paired phases. The existing
MSVC qualification job builds the two workload executables and invokes this
gate after native WinFsp qualification; its upload retains the results and outer
test log. Fast oracle tests run in the hosted Linux job, including deliberate
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

The six-test inventory rejects omissions and skips. Windows and FreeBSD already
return `ENOTSUP` for raw-mount xattrs; their tests require that result for set,
get, list and remove, zero result lengths, unchanged retained metadata and
unchanged output buffers. Supported platforms retain the original POSIX
set/get/remove assertions. This does not implement Windows
xattrs or weaken the Linux regression. The existing native Windows CI job runs
these tests as part of `./weed/mount` in both CGO modes automatically.

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

To reproduce a standalone-client comparison in Actions, configure the optional
`SEAWEEDFS_RELIABILITY_GIT_LFS_PATH`/`SEAWEEDFS_RELIABILITY_GIT_LFS_SHA256` pair
documented in the root README. Both must match the reviewed executable; omitting
both deliberately uses bundled LFS and is reported in the job summary. The
existing `SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC` runner input stages this executable
even when it is an unmodified official release (the name is historical). Setup
hash-checks installed copies, preserves Git's hard-linked launcher and records
the actual LFS version.
`SEAWEEDFS_WINDOWS_MOUNT_REPEATS` defaults to 1 and accepts 1..20 fresh scenario
pairs within the existing 40-minute harness budget; raising the repeat count
does not extend that budget. Verbosity defaults to 0 and accepts 0..4.
For a targeted diagnostic run, `SEAWEEDFS_WINDOWS_MOUNT_SCENARIO` selects either
`GitAtomicRenamePrimed` or `GitLfsTempMetadata`; omit it for the complete gate.
For low-level diagnosis, set `SEAWEEDFS_WINDOWS_MOUNT_TRACE=1` (or the Actions
dispatch input `windows_trace`). The disposable VM enables WinFsp debug output
and starts the checked-in `hack/appmana/seaweed-fileio.wprp` profile (a fixed
16 MiB event ring, process identities, file names and I/O completions) before
the workload. WPR is stopped in cleanup; a missing profile, busy recorder, or
failed flush is a diagnostic failure, not a silent fallback. No existing WPR
session is cancelled. Each scenario's binary `<scenario>-fileio.etl` is retrieved
before VM destruction, in bounded chunks with size and SHA-256 verification;
files over 128 MiB or incomplete transfers fail the run. The VM also runs
`tracerpt` to preserve decoded XML in a ZIP and downloads the full WinFsp debug log,
not just the ordinary last-2,000-lines summary. Open the ETL in
Windows Performance Analyzer's File I/O tables and correlate process, path,
operation, timestamp, and status with the WinFsp/SeaweedFS logs. Memory mode
retains a bounded recent window; inspect trace loss before claiming complete
history. Debug logging changes scheduling, so preserve untraced failures too.
On a failed Git prerequisite, all Windows runs also retain read-only `mountvol`
and `fsutil reparsepoint query` output before unmounting. These inspect the
registered DOS paths and junction target; they never create/delete mappings
or retry the failed Git command. Contract tests verify their ordering and
ensure successful diagnostics cannot change the original failure into a pass.
The ordinary gate leaves tracing off. Artifact-transfer negative tests run with
`go test ./test/storage_lab/vm -run '^TestWindowsArtifact'` using the same Go
workspace as the VM tests. ETLs contain paths and process information; keep
their existing CI artifact access restrictions and 14-day retention.

For failure-only client instrumentation, set
`SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC` to a host Windows `git-lfs.exe`. The runner
replaces the installed copies only inside its fresh disposable VM, records the
input SHA-256 and client version, and leaves the default bundled client unchanged
when the variable is absent. This override can also select an official release
for a controlled A/B run; label those results separately from instrumented runs.
Keep the SeaweedFS binary, WinFsp installer, VM image, and workload fixed.
Dependency setup records `C:\lab\dependency-install.log`, including installer
stage timestamps and exit codes; the failure collector retains its tail when
the guest agent is responsive. A setup timeout is an infrastructure failure,
not a reproduced LFS failure or a passing qualification.
Git for Windows hard-links `cmd/git-lfs.exe` to
`cmd/git.exe`, so overwriting the former corrupted the latter. The lab override
now unlinks only each installed LFS directory entry before copying, verifies
all Git executable hashes remain unchanged, and verifies each replacement hash.
`pwsh -NoProfile -File hack/appmana/install-lab-git-lfs-test.ps1` reproduces this
with real hard links and runs in CI.

To isolate the mount layer from both SeaweedFS metadata and Git, build the
Windows `test/winfsp` executable and select
`SEAWEEDFS_WINDOWS_MOUNT_SCENARIO=MountManagerDirectoryLifecycle` with
`SEAWEEDFS_WINDOWS_WINFSP_TEST` pointing to that executable. The existing VM
runner needs only the WinFsp MSI and native executable, installs WinFsp and
executes `TestMountManagerDirectoryLifecycle`: 64 minimal cgofuse directory
mounts, each checked 256 times for a DOS canonical path that reaches that
cycle's unique virtual sentinel. Queries are separated by 10 ms to observe
delayed registration changes over at least 2.56 seconds per mount. Readiness
also requires the sentinel, so an underlying NTFS directory cannot satisfy the
test. The first failed query fails the test; alternative path flags and mount
listings are diagnostic only. This mode never starts weed or
Git workloads, rejects skips/incomplete cycles, and retains `mount-manager.log`.
It is a component-isolation experiment, not the full Windows qualification gate.
For additional teardown coverage, set
`SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP=1` with a freshly built native
executable. This reuses one exact mount path across all 64 cycles, checks with
`Lstat` that the owned junction disappears after every graceful unmount, and
verifies an unrelated sibling file byte-for-byte. After the canonicalization
probes it records the volume GUID and requires the exact path in
`GetVolumePathNamesForVolumeNameW`; after unmount that association must be absent
(or the GUID no longer found). No localized `mountvol` output is parsed.
The runner requires final junction and mapping cleanup markers, rejecting
older executables that lack these checks. Run with both registration modes.
This does not yet verify process-death cleanup or registration-failure rollback.
Use `SEAWEEDFS_WINDOWS_MOUNT_SCENARIO=MountManagerProcessCrash` for a separate
process-death check (leave `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP` unset).
It launches eight owned child processes in turn at the same mount path. Each
child completes the canonicalization and pre-unmount mapping oracle, signals a
per-cycle readiness file, and waits without unmounting. The parent terminates
only that child, checks junction/mapping removal and sibling bytes, and mounts
the same path in the next cycle. Both registration modes are supported; test-side
junction interventions are rejected. This is abrupt process termination, not VM
power loss, and the synthetic filesystem cannot establish SeaweedFS durability.
The parent creates a random ownership token and sibling fixture. Child mode
requires that token, an absolute non-symlink root, intact sibling bytes and
unused checkpoint/mount paths within that root; it never rewrites the sibling.
Readiness is published by temporary-file rename. The reader permits Windows
delete-sharing while the publisher closes its rename handle; a deterministic
held-DELETE-handle regression runs before the crash cycles. This does not retry
failed mount queries or cleanup assertions. Child-exit waits are bounded.
Post-termination absence checks are immediate and strict, with no repair or
retry of a failed mapping assertion.
The existing storage-reliability Actions workflow exposes these intensive checks
through the optional `windows_mount_cleanup` dispatch input. It runs graceful
same-path cleanup and process-crash cleanup in fresh VMs for default and FSD
registration, before the unchanged real Git/LFS gate, and retains each log.
It uses the workflow's pinned MSI, not an implicitly substituted lab DLL.
Ownership negative controls run in the Linux job on every matching workflow run;
changes under `test/winfsp/` now trigger that workflow too.
The Actions dispatch switch `windows_mount_manager_probe` runs it before (not
instead of) the regular Windows qualification gate. It defaults off.
For a lab-only comparison of WinFsp's user-mode and driver-side registration,
set `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD=1` with this isolated scenario.
The runner sets and verifies `MountUseMountmgrFromFSD` in the fresh VM before
starting the unchanged probe. Other values and non-isolated scenarios are
rejected. This is a diagnostic intervention, not a recommended deployment
setting or a replacement qualification gate. With the variable unset the
runner leaves the WinFsp default unchanged. `provenance.txt` retains the input
artifact hashes, image reference, and selected experimental mode alongside
the test results; the dependency transcript records the applied registry value.
Another isolated comparison uses
`SEAWEEDFS_WINDOWS_MOUNT_MANAGER_GUID_JUNCTION=1`. Before the unchanged DOS
queries, the native probe changes its own synthetic mount's junction target
from the NT device name to the GUID path of that same volume. It verifies the
original junction against the device identified through an open root handle
and checks the virtual sentinel afterward. It does not re-register the mount,
retry failed queries, or act on SeaweedFS data. The option is rejected outside
the isolated scenario and cannot be combined with the driver-side comparison.
This is a causal experiment, not an application fix or deployment guidance;
its mode is retained in `provenance.txt` and each intervention is logged.
Use the matched `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_GUID_JUNCTION=nt-control`
mode before attributing a pass to GUID targeting: it performs the same GUID/NT
identity queries, reparse read, rewrite, readback, and sentinel check, but writes
the original NT target. Both modes verify the exact substitute name after the
write. This controls for query priming and effects of rewriting the junction;
neither mode changes the first-failure behavior of the DOS path assertions.
For source-level WinFsp experiments, run
`bash hack/appmana/build-winfsp-lab-dll.sh /path/to/winfsp-v2.1` with Clang and
the x86_64 MinGW compiler, headers, libraries, and resource compiler installed.
The script pins the upstream source revision, writes into a new temporary
directory, retains compiler output and the source diff, and prints the DLL hash.
Clean sources are required by default; untracked files are rejected. For an
explicit source candidate, set `WINFSP_LAB_ALLOW_TRACKED_PATCH=1`; staged and
unstaged tracked changes are captured together. The pinned commit supplies
`SOURCE_DATE_EPOCH`, and the linker fixes timestamps and the preferred image
base (ASLR remains enabled).
Its compatibility header adapts compiler/SDK declarations, not mount behavior;
this unsigned user-mode DLL is lab-only, not a release artifact. The signed
kernel driver still comes from the pinned WinFsp MSI. First reproduce RED with
the unmodified source before comparing any source patch built the same way.
Set `SEAWEEDFS_WINDOWS_WINFSP_DLL` to the built DLL with the isolated scenario
and a freshly built native test. The runner stages it next to the test rather
than replacing the installed DLL, and the native probe verifies its actual
loaded module path. Missing verification or fallback to the installed DLL
fails the run. Candidate DLL runs cannot be combined with test-side junction
rewrites. The isolated native scenario also accepts explicit
`SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD=1` with a candidate DLL to cover the
driver-side registration branch; mode selection and registry readback remain
recorded, and this combination remains forbidden in real Git scenarios.
It is configuration coverage, not a recommendation to enable that registry
setting. Native success also requires a run-specific completion token.
For real Git scenarios, the runner supplies
`-ExpectedWinFspDll` to the smoke script, which inspects the actual SeaweedFS
mount process's loaded modules before exercising Git. Missing, duplicate,
uninspectable or wrong-path modules fail the run and invoke existing cleanup.
Every scenario must emit the module-verification marker. Retain both
the build directory and VM results as the provenance chain.
Setup and each real-workload cycle also require their own run-specific terminal
marker; a zero exit with an old readiness message or another cycle's output is
not success. Setup output is retained in `setup.log`.
The runner requires adjacent `.manifest.txt` and `.source.patch` files,
validates the DLL/patch hashes and baseline/candidate distinction, and retains
both with the VM results. The manifest records the source revision, recipe and
shim hashes, epoch and tool versions; retain the build directory separately
for full compiler output. Manifest consistency is not proof of provenance or
cross-run equivalence: compare recipe, shim and tool versions between baseline
and candidate, not just their source labels.
The lab-only core candidate `hack/appmana/winfsp-guid-mount.patch` applies to
the pinned WinFsp checkout with `git apply`. It resolves the registered volume
GUID without opening the not-yet-dispatched filesystem, sets that target on
the owned junction handle, and rolls back registration if the update fails.
It changes both registration modes but not ordinary unregistered mounts or
drive mounts. It is experimental, not deployment-qualified; the unchanged
native reproducer and real LFS workload must qualify it before adoption.

Select `SEAWEEDFS_WINDOWS_MOUNT_SCENARIO=MountManagerRegistrationRollback`
with an explicit manifested `SEAWEEDFS_WINDOWS_WINFSP_DLL` and an amd64 native
test executable for deterministic rollback injection. The test modifies only
the loaded lab DLL's IAT in its own process, never the DLL file or global APIs.
It fails `FindFirstVolumeW` or the second `FSCTL_SET_REPARSE_POINT`, requires exact
hook counters and failed mounting, restores and verifies the original import,
enumerates all volume paths to reject stale registration, and mounts the exact
path normally. Sibling bytes must remain unchanged. Both registration modes are
supported; junction interventions and the separate cleanup flag are rejected.
PE-layout fixtures and live original-pointer checks guard the test-only hook.
Callbacks resolve their original target before installation. If a failed mount
times out, the harness waits for both Mount and Unmount to leave the DLL before
restoring the hook. Failure to prove quiescence aborts the disposable native test
process with exit 2, without running DLL restoration/unload cleanups underneath
active calls. This is a failed test, never a successful rollback result.

Release-build follow-up must use upstream's `build/VStudio/winfsp_dll.vcxproj`
in `Release|x64`, with the compiler and SDK versions recorded. The pinned v2.1
`build.version.props` targets Windows SDK `10.0.19041.0`; its build number and
copyright year otherwise depend on the current date, so reproducible comparison
requires explicit recorded overrides. Build only the user-mode DLL project for
this fix; replacing or rebuilding the signed kernel driver is not part of it.
The generic Labcontainers Windows image provisions drivers/QGA, not MSVC.
Use its existing project-specific `provisioning_scripts` extension for a separate
compiler-enabled image. Neither a MinGW lab pass nor a successful MSVC compile
alone establishes deployment qualification: test the resulting exact artifact
and its packaging/load path before release.
The DLL manifest verifier accepts `build_toolchain=msvc` only with recorded
`platform_toolset`, `vc_tools_version`, `windows_sdk_version`, `msbuild_version`,
`version_build_number` and `version_copyright_year`, in addition to the existing
source revision, source mode, source epoch, patch/DLL hashes and recipe hash.
An MSVC manifest must not claim `build_shim_sha256`. Existing MinGW manifests
(absent toolchain field or `build_toolchain=mingw`) still require that shim hash;
unknown toolchains fail validation. These checks establish evidence consistency,
not compiler trust, reproducibility, or a successful Windows-native build.
`hack/appmana/build-winfsp-msvc.ps1` builds that DLL project only. In a disposable
compiler-enabled Windows guest, use separate source checkouts and fresh output
directories for baseline and candidate, for example:

```powershell
.\hack\appmana\build-winfsp-msvc.ps1 -SourceDirectory C:\lab\winfsp-candidate `
  -OutputDirectory C:\lab\msvc-candidate -AllowTrackedPatch `
  -MSBuildPath 'C:\Program Files (x86)\Microsoft Visual Studio\2019\BuildTools\MSBuild\Current\Bin\MSBuild.exe' `
  -VCToolsVersion 14.29.30133 -PlatformToolset v142 -WindowsSDKVersion 10.0.19041.0
```

The example compiler version/path are explicit inputs, not evidence that this
toolchain is installed. Pin the same installed versions for both builds and omit
`-AllowTrackedPatch` for the clean baseline. Source/output paths cannot contain
spaces or shell metacharacters because upstream's `.pc` generation is unquoted.
The recipe rejects preexisting outputs, fixes date-dependent version fields,
retains diagnostic MSBuild output, a binary log, exact arguments and source patch,
and writes the harness-compatible manifest only after successful compilation and
a source-stability check. It never registers a DLL, replaces installed files,
builds a driver or produces an installer. Its host-independent argument/error
contracts run in the existing reliability workflow; a passing contract does not
prove compilation or runtime behavior.
Source mode and patch digest now come from the same captured patch bytes, not
an earlier `git status`: a deterministic reproduction showed that an intervening
edit could otherwise produce a nonempty patch labeled baseline. Empty captured
patches remain baseline even with candidate opt-in; nonempty patches require
that opt-in. The recipe hash is captured before build execution and must still
match before manifest publication. Contract fixtures cover these cases and
reject extra/combined build targets using an exact ordered argument vector.
These are consistency checks, not an attestation against transient edits or a
hostile build host. In particular, manifest compiler/SDK fields currently record
requested versions; verifying effective compiler/linker/resource-tool paths,
versions and hashes against the retained build logs remains required before
calling an MSVC artifact provenance-qualified.
The native compiler lab is automated by `TestWindowsWinFspMSVCBuildLab` in the
existing VM test module. It uses Labcontainers/QGA and a read-only attached ISO;
the guest has no external network. Required inputs are
`LABCONTAINERS_WINDOWS_IMAGE`, `LABCONTAINERS_LABD`,
`SEAWEEDFS_WINDOWS_MSVC_ISO` (absolute host path), and
`SEAWEEDFS_WINDOWS_MSVC_ISO_SHA256`. Explicitly opt in with
`SEAWEEDFS_WINDOWS_MSVC_BUILD_LIVE=1`, then run:

```sh
go test ./test/storage_lab/vm -run '^TestWindowsWinFspMSVCBuildLab$' -v -count=1 -timeout 65m
```

Use the same workspace/module setup as the other VM tests. This expensive gate
is separate from normal unit tests; an unset opt-in skips it, not a native-build
pass. The ISO must have label `WINFSP_BUILD`, the verified image-layer trees
`Files/Program Files (x86)/Microsoft Visual Studio/2022/BuildTools` and
`Files/Program Files (x86)/Windows Kits/10`, the verified
`Git-2.51.0-64-bit.exe`, and a `winfsp.bundle` advertising the pinned WinFsp HEAD.
Generate media with deep directories enabled (`genisoimage -D -udf -iso-level 3
-J -joliet-long`); omitting `-D` silently omits deeply nested compiler files.
Windows layer entries can have zero Unix permissions: give the isolated staging
tree owner read/traverse permission before generating media. Do not import
registry hives or OS trees from the container image.

`hack/appmana/provision-winfsp-msvc-lab.ps1` copies the toolchain into the fresh
guest, configures only its SDK discovery roots, and selects x64-hosted MSVC
14.44.35207/v143 with SDK 10.0.26100.0. These explicit versions, the Git installer
filename, and the pinned source/shallow boundary must be reviewed when changing
the payload. Windows' existing UCRT props also read SDK registry roots; setting
only `WindowsSdkDir` is insufficient (the native compiler reported missing
`ctype.h` despite that header being present). The shallow source bundle needs
its pinned shallow boundary restored after clone. Line-ending conversion is
disabled during clone to preserve tracked source bytes.

Prepare media using the checked-in, hash-verifying helper, not an ad-hoc ISO:

```sh
bash hack/appmana/prepare-winfsp-msvc-media.sh \
  /runner/cache/toolchain-layer.tar.gz /runner/src/winfsp-v2.1 \
  /runner/cache/Git-2.51.0-64-bit.exe /runner/cache
```

The output parent must already exist; the helper creates a fresh child directory
and retains the extracted payload plus ISO (allow roughly 25 GiB beyond the
downloaded layer). It rejects incorrect layer/installer hashes, a wrong source
HEAD, symbolic links, and missing compiler inputs. It emits the ISO path and
digest. Copy/preload that ISO on the dedicated runner and set the Actions
variables `SEAWEEDFS_RELIABILITY_MSVC_ISO_PATH` and
`SEAWEEDFS_RELIABILITY_MSVC_ISO_SHA256`. Set
`SEAWEEDFS_RELIABILITY_WINFSP_MSVC_ENABLED=1` for continuous qualification, or use
workflow dispatch `windows_msvc_build=true`. Also configure the pinned
Labcontainers source/Windows image, Go-FUSE source, WinFsp MSI, Git installer,
and explicit standalone LFS path/hash variables documented in the root README.
The MSVC job requires standalone LFS rather than silently using the bundle.
Its inputs are supplied as environment variables, not interpolated into scripts.
Compiler/image/Git/source pins in the media helper and provisioning/build
scripts must move together with the manifest assertions and qualification data.

The workflow runs the exact compiled candidate through graceful same-path
cleanup, process-crash cleanup, and injected registration rollback in both
registration modes, then five real Git atomic-rename and LFS cycles. It retains
build and runtime evidence for 30 days even on failure. It has no signing,
registry publishing, release creation, or production deployment step; app-local
DLL loading in a test does not establish the eventual package's load path.

The caller stages the current checked-in build recipe and candidate patch,
not copies embedded in the ISO. Each run builds baseline then candidate using
the same guest/toolchain. Fresh completion tokens, mandatory DLL/log/binlog/
argument/manifest retrieval, guest-to-host SHA-256 verification, clean baseline,
and exact candidate patch comparison are required. Results are retained outside
the repository under the printed `winfsp-msvc-results-*` directory, including
toolchain executable paths/versions/hashes. Inspect the diagnostic build logs
for the effective compiler/linker/resource-compiler paths before making stronger
toolchain provenance claims; requested versions alone are not attestation.
Subsequent harness runs materialize auxiliary log tails in the guest and use the
existing chunked, SHA-256-verified artifact transfer instead of one oversized
exec response. `guest-logs.txt` contains the diagnostic text;
`guest-logs-collection.txt` records collection status, byte count and SHA-256.
The existing 2,000-line-per-file tail policy remains: this is not a full log
archive. Collection has a separate two-minute deadline and now fails the harness
on error rather than silently allowing missing auxiliary evidence. Unit coverage
reproduces the old truncation failure with more than 3 MiB of diagnostic text.
`hack/appmana/git-lfs-canonical-diagnostics.patch` applies to Git LFS v3.7.0,
commit `92dddf560e62ef7dd25877d87ce072f7595aa52d`. In a disposable checkout of
that exact source, apply the patch with `git apply`, then build:

```sh
GOTOOLCHAIN=go1.24.4 GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags '-X github.com/git-lfs/git-lfs/v3/config.GitCommit=92dddf56-lab-diagnostics' \
  -o /absolute/lab/artifacts/git-lfs-diagnostic.exe .
```

The patch distinguishes `filepath.Abs`, `CreateFile`, and
`GetFinalPathNameByHandle` failures. Only after the last API fails does it query
DOS/GUID/NT names, with normalized and opened-name flags, on the same handle.
It always returns the original error: these probes are observations, not retries
that turn a failed workload green. Do not deploy this diagnostic client.
The optional native executable adds 512 create/chmod/write/close/mkdir/rename
transactions per pair, including uppercase `.GIT` paths and exact final object
content checks. The Actions gate builds and requires this executable, runs five
pairs, and rejects skipped/missing native completion. A skipped opt-in
test is not qualification. Pin installer hashes and VM image digests for release
evidence; a local `latest` image run is exploratory only. The harness contract
test (`pwsh -File hack/appmana/mount-smoke-contract-test.ps1`) injects prerequisite
failures without starting a VM and runs in the existing Windows build workflow.

Stale `config.lock` can follow a successful server rename.
`TestStreamRenameAckSurvivesClockBehindMetadataFence`
reproduces this cache failure deterministically when the monotonic metadata
clock is ahead of wall time. Streaming rename replies must use the actual
committed metadata events, not separately sampled wall-clock timestamps.
`TestStreamRenameRepliesMatchCommittedLog` checks reply/log equality, both
transports, overwrites, recursive moves, and data preservation on disconnect.
An acknowledgment can be lost **after commit**: an RPC error alone does not
prove the rename failed. Inspect source and destination before retrying; do not
delete a destination or assume an old lock file is disposable based on that
error.
Deterministic regressions cover a separate cache failure:
`TestSaveEntryNoChangeAckPreservesCaseFoldListing` exercises the real no-event
`UpdateEntry` acknowledgment fallback, which previously deleted a live `.git`
cache entry at an equal timestamp and made `.GIT` lookup fail. In-place updates
must fence both halves as writes; only actual renames/deletes may remove the old
path at its existing version. Directory updates with omitted `NewParentPath`
must also preserve children.
Failure-only `winfsp rename failed at`
diagnostics at verbosity 1 identify which rename stage rejects a request.
Verbosity 1 also
records failed component resolution, case-fold lookup, and attribute reads to
localize this earlier failure without retries or successful-path tracing.
`TestGitLfsDirectoryCanonicalization` additionally exercises Git LFS 3.7.0's
actual Windows directory `CreateFile` / `GetFinalPathNameByHandle` sequence,
after config-file replacements and through both original and uppercase paths.
The native smoke gate requires this test as well as the object-rename test;
cross-compilation alone does not qualify either.
Deployment must include both components: the committed-event acknowledgment
fix runs in the **filer**, while the in-place update correction runs in the
**mount**. A mount-only upgrade leaves old filer rename replies unchanged.
Mixed-version operation needs its own qualification; these tests use the same
candidate for the local filer and Windows mount and do not certify a rolling
upgrade against older filers.

The dedicated `vm-fault-gates` Actions job also runs this test with preloaded,
hash-checked installers (variables are listed in the root README). Its pinned
Labcontainers commit must include `ExecWithTimeout` (introduced in `6f89daa`),
as well as crash and bridge restoration support. Guest scenarios have explicit
eight-minute execution limits; a timeout remains a failure and triggers a
bounded attempt to recover partial output before destroying the VM.
The 40-minute parent context and 45-minute Go timeout accommodate readiness,
installation, both scenarios, and cleanup; do not shorten only the outer timeout.

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
flush durability nor XFS/Btrfs behavior. The Labcontainers ext4 gate now requests
abrupt VM loss and recreation; the `.dat`-before-index durability gap has
separate regression coverage. Do not attribute earlier stop/restart results
to this stronger fault model. It does not model drive/controller caches that lie about flushes, and
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
metadata-store restore, a production Harbor workload or Windows qualification.
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
