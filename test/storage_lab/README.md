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
To reproduce a standalone-client comparison in Actions, configure the optional
`SEAWEEDFS_RELIABILITY_GIT_LFS_PATH`/`SEAWEEDFS_RELIABILITY_GIT_LFS_SHA256` pair
documented in the root README. Both must match the reviewed executable; omitting
both deliberately uses bundled LFS and is reported in the job summary. The
existing `SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC` runner input stages this executable
even when it is an unmodified official release (the name is historical). Setup
hash-checks installed copies, preserves Git's hard-linked launcher and records
the actual LFS version. Local official 3.8.0 comparisons used executable SHA-256
`d1a2b2a90a3b8d57e68db6a6d0daefc7c96e12a1abd1c0849f41d13839252857`.
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
The focused profile captured a real failure against diagnostic candidate
`2ef9c71d1`: in `/tmp/seaweedfs-windows-mount-results-3402907160`, cycle 1 passed,
but cycle 2 failed resolving `.git` during seed `git add`. Both ETLs, decoded
XML ZIPs, and full WinFsp logs were retrieved and hash-verified. The suspected
LFS client's directory opens and subsequent file-name information queries
succeeded in that trace; do not misreport this as a proven missing filer entry.
Process events identify the failing client as `git-lfs.exe filter-process`
(PID 2356); successful kernel name queries do not prove that subsequent
user-mode DOS-volume translation succeeded. The remaining canonicalization
failure requires further client/API tracing.

For failure-only client instrumentation, set
`SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC` to a host Windows `git-lfs.exe`. The runner
replaces the installed copies only inside its fresh disposable VM, records the
input SHA-256 and client version, and leaves the default bundled client unchanged
when the variable is absent. This override can also select an official release
for a controlled A/B run; label those results separately from instrumented runs.
Keep the SeaweedFS binary, WinFsp installer, VM image, and workload fixed.
The historical reproduction uses Git for Windows 2.51.0 with Git LFS 3.7.0;
that client is not a current-version qualification.
Dependency setup records `C:\lab\dependency-install.log`, including installer
stage timestamps and exit codes; the failure collector retains its tail when
the guest agent is responsive. A setup timeout is an infrastructure failure,
not a reproduced LFS failure or a passing qualification. The first instrumented
3.7.0 and official 3.8.0 comparisons both timed out during setup before any
workload ran; neither establishes client-version behavior. The setup cause was
subsequently reproduced: Git for Windows hard-links `cmd/git-lfs.exe` to
`cmd/git.exe`, so overwriting the former corrupted the latter. The lab override
now unlinks only each installed LFS directory entry before copying, verifies
all Git executable hashes remain unchanged, and verifies each replacement hash.
`pwsh -NoProfile -File hack/appmana/install-lab-git-lfs-test.ps1` reproduces this
with real hard links (RED before the unlink fix, GREEN afterward) and runs in CI.
This lab-only setup defect is separate from the original unmodified-client
filesystem failure; the latter remains unresolved.

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
Readiness is published by temporary-file rename. Child-exit waits are bounded.
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
This minimal probe reproduced the DOS-path defect without Git, a weed binary,
or a filer on WinFsp 2.1.25156: cycle 51 (zero-based), query 179 failed after
earlier queries on that mount had succeeded. Same-handle GUID/NT queries
succeeded; both DOS forms failed with `ERROR_PATH_NOT_FOUND`, and `mountvol`
listed the matching GUID with no mount points. The native test failed after
146.65 seconds, and the VM runner failed after 306.59 seconds. Evidence is in
`/tmp/seaweedfs-windows-mount-results-1442029407/mount-manager.log`, SHA-256
`bffb43b01ae9305a420ab1c50d7f5b35b6c57b6a189a4e0a4bfa943a3a67e8db`.
The tested executable SHA-256 is
`9b304d6aa5821780c880a2ce4ccee25977bbd150624e31bfa15ec72adfde3e55`.
This isolates this canonical-path failure to the cgofuse/WinFsp/Windows mount
layer; it does not yet identify the faulty component or explain the separate
original LFS object-move failure. Preserve this RED probe when testing fixes.
For a lab-only comparison of WinFsp's user-mode and driver-side registration,
set `SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD=1` with this isolated scenario.
The runner sets and verifies `MountUseMountmgrFromFSD` in the fresh VM before
starting the unchanged probe. Other values and non-isolated scenarios are
rejected. This is a diagnostic intervention, not a recommended deployment
setting or a replacement qualification gate. With the variable unset the
runner leaves the WinFsp default unchanged. `provenance.txt` retains the input
artifact hashes, image reference, and selected experimental mode alongside
the test results; the dependency transcript records the applied registry value.
The driver-side comparison also reproduced the defect with the unchanged
native executable: cycle 39 (zero-based), query 86, after 112.32 seconds.
Both DOS forms failed while same-handle GUID/NT queries succeeded, and the
matching GUID had no mount points. The VM transcript verified mode 1 before
execution. Results are in `/tmp/seaweedfs-windows-mount-results-327889120`;
`mount-manager.log` SHA-256 is
`09b3ca72e0fa4d3cebc1f9ee95df9dcaed9b136e38798f54ec994631842662bf`.
Switching to driver-side registration is therefore not a fix. Investigate
the shared mount lifecycle rather than recommending this registry setting.
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
The initial exploratory GUID rewrite passed 64 cycles in 182.07 seconds
(`/tmp/seaweedfs-windows-mount-results-1337781947`, log SHA-256
`484a66d04441feb70cd40934b5598eddf8885a370b126ea2072bbe65062a2d81`).
Its executable SHA-256 was
`520af4f2ac807aec00302ef2732514ff9270dc43dfa83b89c9bf08174fd730fe`.
That executable predates the exact reparse readback and matched NT-target
control. Treat it only as exploratory evidence, not a fix or a controlled
comparison; use the audited control/treatment executable for causal testing.
The matched NT-target rewrite control reproduced the failure on zero-based
cycle 38, query 221, in 109.72 seconds. Its persisted target had been read back
and its sentinel checked; the first failing DOS query still had successful
same-handle GUID/NT alternatives and no matching mount point. Results:
`/tmp/seaweedfs-windows-mount-results-2285140162`; log SHA-256
`8c80f6a02f02423a2c28fa4d8a1f75033baf8019bb6a67821a79a7e1df21442a`.
The controlled executable SHA-256 is
`17e08df5e917d73ab5fb28a20dea3c6db5e639b05a3a1127393b4d397d0fd975`.
Thus preparatory identity queries and rewriting a junction are not sufficient
to prevent the defect. Compare GUID treatment with this same executable.
The audited GUID treatment using that same controlled executable passed all
64 cycles (16,384 DOS queries) in 182.25 seconds, with exact junction target
readback and sentinel validation. Results:
`/tmp/seaweedfs-windows-mount-results-1934794895`; log SHA-256
`20f2623019c89d55622f5ff43b1c3ff34bce4025c935205f680b3882787acccc`.
This RED-control/GREEN-treatment pair supports the junction-target hypothesis,
but one intermittent comparison is not proof of a production fix. A source-level
mount implementation change, repeated verification, and real Git LFS testing
remain required. Do not deploy the test helper as a post-mount repair loop.
For source-level WinFsp experiments, run
`bash hack/appmana/build-winfsp-lab-dll.sh /path/to/winfsp-v2.1` with Clang and
the x86_64 MinGW compiler, headers, libraries, and resource compiler installed.
The script pins the upstream source revision, writes into a new temporary
directory, retains compiler output and the source diff, and prints the DLL hash.
Clean sources are required by default; untracked files are rejected. For an
explicit source candidate, set `WINFSP_LAB_ALLOW_TRACKED_PATCH=1`; staged and
unstaged tracked changes are captured together. The pinned commit supplies
`SOURCE_DATE_EPOCH`, and the linker fixes timestamps and the preferred image
base (ASLR remains enabled). Two clean builds with this recipe produced the
same DLL SHA-256, `c0a63935ff993fc58cb90bed1c2982efb91cbb6fb602ff86fb2b478d945a3e10`.
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
not success. Setup output is retained in `setup.log`. During candidate LFS
qualification (`/tmp/seaweedfs-windows-mount-results-77732198`) the old transport
returned `windows-ready` for setup even though subsequent workloads ran.
A labcontainers regression reproduced stale results after timeout and Windows
PID reuse; its cleanup fix reaps QGA records. That run predates the completion
guards and transport fix and must not serve as final qualification, regardless
of its workload pass count. Repeat with a VM container image containing the
rebuilt guest helper (rebuilding `labd` alone is insufficient), and retain
installation/version evidence; do not replace a live helper.
The corrected local image used for the guarded follow-up is
`labcontainers/windows-server-2022:lfs-qga-reap-da40c93`, image ID
`sha256:c8e5f67da19c9a74321360a886816d9d23b2c84bef0ccc61c86565eb0577b986`.
It retains the earlier Windows disk and changes only the Linux-side helper,
built from clean labcontainers commit `da40c93958683244369f970e8841f89b98fc9e8f`
with SHA-256 `2367dd4d41242ab7382cba106a724340a98a4a6e43a4344dea978b935d05e05a`.
The helper-only recipe is preserved in labcontainers' existing Windows image
directory as `Dockerfile.helper`; its README explains the required base image,
source revision and helper hash arguments. The cleanup regressions were RED
before each fix, then passed with the guest-agent race suite and full Go suite.
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
The unmodified source-build baseline reproduced RED on zero-based cycle 34,
query 123 (100.93 seconds native), with the same DOS failure, successful
same-handle GUID/NT queries, and missing reverse mount mapping. The probe
verified it loaded `C:\lab\winfsp-x64.dll`, not the installed DLL. Baseline DLL
SHA-256: `306b53736942ee2608afa40533deb572f842dc7c849d9390886a1dc5a9b7e23b`;
native executable SHA-256:
`a01eaef12e96b6b78e9dc54e6cf236585158aeb8d43425405d2f3964c10dc7a5`.
Results: `/tmp/seaweedfs-windows-mount-results-3424831313`; log SHA-256
`38c5e34c5a65af9935ffdc769a6b998ecafc868dcd6eabc85eab22e0683ab2a1`.
This is exploratory source-build RED evidence, not a formal matched-build
comparison or a fix. That manual DLL predates the manifested deterministic
recipe and uses a different lab resource label. Use clean manifested builds
with identical recipes for the baseline/candidate comparison.
The deterministic clean baseline (`c0a63935...`, full hash above) reproduced
RED on zero-based cycle 51, query 91 (149.16 seconds native), using the same
`a01eaef1...` native executable. DOS queries failed while GUID/NT queries still
succeeded and the GUID volume had no mount points. Results:
`/tmp/seaweedfs-windows-mount-results-3975497209`; log SHA-256
`060031b74728d02b44782d5a8e6001ac2910875d6494c0e0f1c877daceb969e1`.
Its manifest matches the first GUID-source candidate's recipe, shim, epoch
and tool versions. Candidate DLL SHA-256:
`1ea9a10d34f2c6cd057e4233918061801bf39347e040f6d122f210dd058ff40e`;
source patch SHA-256:
`327b33cbce75c9c7b77294125994391814df383168366141094653bd2c53e72a`.
The first attempt to run that candidate
(`/tmp/seaweedfs-windows-mount-results-1188166966`) failed during installer
staging, before the native test ran; it is infrastructure evidence, not a
candidate RED or GREEN result.
The subsequent serial candidate run passed all 64 cycles and 16,384 DOS path
queries (187.76 seconds native), with the same native executable and both
junction experiment flags disabled. Loaded-module verification confirmed the
candidate DLL. Results: `/tmp/seaweedfs-windows-mount-results-1769930251`;
log SHA-256:
`1c40c5592ca8dc6356ddcc0a44d6665936544a84e84f7dd52bb3bd2dfa665eaa`.
This is a matched source-build RED/GREEN result, not full qualification:
repeat stress, real Git LFS workloads, configuration/cleanup coverage and
release packaging verification remain required before deployment. The native
test does not run SeaweedFS or Git and cannot establish their data safety.

The same candidate DLL and unchanged `a01eaef1...` native executable also
passed the alternate registration branch with `MountUseMountmgrFromFSD=1`
verified in the disposable guest: 64 cycles, 16,384 queries, 191.80 seconds
native and 397.849 seconds for the complete harness (exit 0). Both test-side
junction interventions remained disabled. Results:
`/tmp/seaweedfs-windows-mount-results-1440216702`; `mount-manager.log` SHA-256:
`4bb0554153a17379c2e9d9d97355206d9ea31c65cdcdc8fcb65a533b72a27b3f`.
This run used the corrected `lfs-qga-reap-da40c93` helper image and verified
run-specific setup/native completion tokens and the loaded candidate DLL.
It extends native registration coverage, not real-workload or crash safety
qualification; the real LFS qualification continues using default registration.

Same-path graceful cleanup qualification with the candidate subsequently passed
64 cycles under default registration in
`/tmp/seaweedfs-windows-mount-results-68763737` (192.39 seconds native,
384.441 seconds full harness, exit 0). This first cleanup executable,
`26fc92fb8d8a0d23055fc4f21f4a5f5dd8a992e5313b4042f42e38cc7e3bf3ee`,
checked junction removal and sibling bytes but predates the volume-mapping
assertion. Log SHA-256:
`993b38e6d5f098353d0dfe79181fa5c9750455eb0625e1ccad644713472c99dc`.
The stronger mapping-cleanup executable,
`9d3bfba645e8f4524f9bc2c148f4c3a001c747f6a06dd1aeacfe03e819cf1bd1`,
passed all 64 cycles with FSD registration in
`/tmp/seaweedfs-windows-mount-results-3521072933` (191.75 seconds native,
403.812 seconds full harness, exit 0), verifying both junction and exact-GUID
mapping removal and sibling preservation. Log SHA-256:
`882a322d728b13c7bd4142baede8019ddb8462eee2ea69eeb6d508b4ed6f3b03`.
Neither result establishes process-death cleanup or registration rollback.
That same stronger executable also passed all 64 cycles under default
registration in `/tmp/seaweedfs-windows-mount-results-364112300`
(190.62 seconds native, 398.918 seconds full harness, exit 0). Junction removal,
exact-GUID mapping removal, sibling bytes and same-path remount were verified.
Log SHA-256:
`7f00fc7e82997bc41db4a24e2b31dd4644f9c2a8ea22c068f5ab762f84e8b675`.
The first process-crash probe passed eight owned-process terminations in default
mode in `/tmp/seaweedfs-windows-mount-results-2336498633` (31.46 seconds native,
238.954 seconds full harness, exit 0), with immediate junction/mapping absence,
sibling preservation and same-path reuse. Executable SHA-256:
`8ec77d409fc4bcc7226e7930ba5e89dfd1c9b528f769878c85431a0ae0b3182a`;
log SHA-256:
`33dca74b7610cbb3aa12459de6d1114f01888cd524d8ddf880485fc9cdc60f61`.
This predates child ownership-token enforcement; the parent supplied its own
fresh temporary directory, but direct child invocation was insufficiently guarded.
Use the ownership-hardened executable for subsequent qualification.
The hardened executable
`e0f5d0be043e3d8ac4e94c7d61554fa21be82e0061a017a1297d11619046a450`
passed all eight process-crash cycles with FSD registration in
`/tmp/seaweedfs-windows-mount-results-661557478` (31.51 seconds native,
224.994 seconds full harness, exit 0). Every child verified the candidate DLL;
post-crash junction/mapping checks were immediate, with no polling or repair.
Log SHA-256:
`8755465461b05345fec557ca7992da6e9d63de0463f5f0734bad4af388e2d854`.
The same hardened executable passed eight default-registration crash cycles in
`/tmp/seaweedfs-windows-mount-results-3056222946` (31.48 seconds native,
247.385 seconds full harness, exit 0), with the same immediate cleanup checks.
Log SHA-256:
`b8ca5994e3e1b51f118087c59e51a5199fe33ed1963b808582afd61d330bdb37`.
These process-termination results do not cover failure after registration but
before the candidate's GUID-junction rewrite completes. Upstream test API hooks
are compiled into `winfsp-tests.exe`, not the DLL, and cannot inject that failure.
Rollback qualification must prove that the intended DLL call actually failed,
that the mount failed, that no owned path remains registered, and that restoring
the normal call permits exact-path reuse. Do not substitute a service-stop race
or a passing process-crash test for that missing evidence.
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
This scenario is not yet a completed qualification result merely because it
builds; retain the native result and restoration markers from each live run.
Callbacks resolve their original target before installation. If a failed mount
times out, the harness waits for both Mount and Unmount to leave the DLL before
restoring the hook. Failure to prove quiescence aborts the disposable native test
process with exit 2, without running DLL restoration/unload cleanups underneath
active calls. This is a failed test, never a successful rollback result.
The initial default-mode rollback run passed both GUID-lookup and GUID-reparse
fail points in `/tmp/seaweedfs-windows-mount-results-2345799113` (0.19 seconds
native, 181.814 seconds complete harness, exit 0). Both hooks fired exactly once,
imports were restored, global mapping/junction absence and same-path reuse were
verified. Native executable SHA-256:
`2a8ce8f7e8681867649ac66da7a630ff08ff8516cc06599c2358706ec6f2b434`;
log SHA-256:
`7b456302369fbb0e238f65c83b0f258e96a9753b078266be010fc5fd0d77edd8`.
Auxiliary log collection succeeded. This initial executable predates the timeout
quiescence and pre-install callback-target hardening; neither timeout path ran.
Use the hardened executable for subsequent rollback qualification.
The hardened executable
`71e02b7d5c3255c3f5010bd81a5b4b4db80ebc73996cd4495ba6d334055d33c4`
passed both rollback fail points under FSD registration in
`/tmp/seaweedfs-windows-mount-results-4150295100` (0.19 seconds native,
188.225 seconds full harness, exit 0). Restored import markers, global mapping
absence, exact-path reuse and sibling checks all passed; auxiliary log transfer
also succeeded. Native log SHA-256:
`b4af7fba93e1192a821c6e06c257f3a4719d574986b1b84f17ecc49368864d52`.
The same hardened executable passed both default-registration fail points in
`/tmp/seaweedfs-windows-mount-results-91939513` (0.14 seconds native,
181.919 seconds full harness, exit 0), including both restoration markers,
mapping absence and exact-path reuse. Auxiliary collection succeeded (2,254
bytes). Native log SHA-256:
`770c3a7b40c4fd5e9a458306474cf194fe43faa4801e257a35eaaeb855ed6aab`.

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
prove compilation or runtime behavior. Windows-native build/runtime qualification
remains outstanding until actual artifact results are recorded here.
A read-only inventory in a fresh
`labcontainers/windows-server-2022:lfs-qga-reap-da40c93` guest completed with exit
0 and `TOOLCHAIN_INVENTORY_COMPLETE`: `vswhere.exe`, both standard Visual Studio
installation roots, and `Windows Kits\10\Include` were absent. This image cannot
be assumed to contain the required compiler/SDK. Provision a separate build image
or supply a verified existing toolchain image before running the MSVC recipe;
this inventory does not rule out nonstandard toolchain locations in other images.
Compiler provisioning has an unresolved download-integrity check. The official
VS2019 channel fetched from `https://aka.ms/vs/16/release/channel` reports
16.11.60 / 16.11.37627.13 (retained channel SHA-256
`ce478cd78cce92c5c8bdcf4bd5cb17f90ec7be245a772a030563cc7aa096c4c6`).
Its `Microsoft.VisualStudio.Manifests.VisualStudio` payload declares 19,253,644
bytes and SHA-256
`fb642c3f891b70947e0152275e1722ffb3cca7e8700eea0f0fa0f3a7645584cc`.
Two fetches of that exact payload URL, including a cache-bypassing retry, instead
returned 11,154,648 bytes with SHA-256
`406969c30f4eb8bf0075a0850e339340ac83942705b76269156bb5b70f01b631`.
Evidence is retained in `/tmp/seaweed-msvc-layout.4rUsHP2a/` as `channel.json`,
`VisualStudio.vsman` and `VisualStudio.retry.vsman`. Do not use these mismatching
catalog bytes to install packages or replace the expected hash with the received
hash. Resolve the discrepancy or obtain an independently verified offline layout
before provisioning. No compiler installation was attempted from this catalog.
Separately, the downloaded `vs_buildtools.exe` (SHA-256
`0a641c8f47df21f3569fbd87f1f2301ae82db9bc5669bb07b18fb623a207c9ba`)
passed `Get-AuthenticodeSignature` in a fresh isolated Windows guest: status
`Valid`, Microsoft Corporation signer, certificate thumbprint
`AB172913A2960A224809EE8A0C371CD47A079B72`, product version 16.11.37627.13,
guest hash matching the host, and `BOOTSTRAPPER_SIGNATURE_VERIFIED` with exit 0.
The bootstrapper was inspected, not executed. Its valid signature does not
resolve the separate catalog mismatch.
The pinned 16.11.60 BuildTools link in Microsoft's release history yielded a
different bootstrapper matching its URL hash
`56c889adadf638062ef8e7b9421a988e8cb5441b3730e5ed28f777a538d9f69e`.
Its embedded `installChannelUri`
(`https://aka.ms/vs/16/release/392447893_1759027617/channel`) returned the same
channel bytes/hash and therefore did not resolve the catalog mismatch. This
pinned bootstrapper has not been executed or independently signature-checked.
The alternative official VS2022 channel (17.14.41) was also checked, without
changing the intended v142/19041 target: its catalog declared 30,443,537 bytes,
SHA-256 `6e470016e4324c84c255ffd0beb3767d17ec89cc8561e9409ee3e1f6d29400f5`,
but returned 17,954,732 bytes,
SHA-256 `f0a50ea157222c29abd5ea6ff01bfc3c33b04e011c5e45ee2ca38ef0778e5643`.
The alternate files are retained in the same cache; neither was used to install
packages. A verified preprovisioned image/offline layout remains an alternative.

An exploratory real-LFS candidate run in
`/tmp/seaweedfs-windows-mount-results-77732198` returned harness exit 0 after
2193.456 seconds with all 20 workload logs containing the native rename/content
and canonicalization success markers. Do not count this as final qualification:
it used the old guest-agent helper, setup returned the stale `windows-ready`
response, and terminal guest-log collection returned exit 125 with truncated
output rather than an installation transcript. The staged client hash alone
does not prove the installed client version. Retained `guest-logs.txt` SHA-256:
`edd108c1aad85e203bb849c60fc8bdedb8cdd823ee3314c59eb26ca99f42257f`.
Use the corrected-helper run with verified setup/client version and unique
per-cycle completion tokens for the qualification decision.

That guarded real-LFS run completed all 20 cycles successfully in
`/tmp/seaweedfs-windows-mount-results-1980685225` (2333.536 seconds, harness
exit 0), using candidate DLL `1ea9a10d...`, unchanged weed `f45761ac...`, and
official LFS 3.8.0. `setup.log` records the actual guest version and unique setup
token; SHA-256:
`1f7d435a971813c2eca57469e3ce4265f972a61ae59e6c3da5768d8c1baceebf`.
Each retained cycle log has its unique completion token, loaded-DLL verification,
20 successful LFS status iterations reporting all 32 modified assets, native
canonicalization and rename/content checks without skips, and graceful unmount.
Cycle-20 log SHA-256:
`64e9bbbbd936c37ba7c93b993568592af7159997bc040b5d9b067ba732e4ac29`.
The SHA-256 of `sha256sum` output over the sorted absolute
`GitLfsTempMetadata-*.log` paths in that directory is
`6dc42d6c4444afb30d167eb135573ab2510aa54e228c925a5338fd8581f3f783`.
The separate terminal guest-log collection still reported exit 125 / output
truncation; it is not a complete auxiliary archive. Unlike the exploratory run,
setup and every workload response are independently retained and token-verified.
This establishes the real-LFS stress result, not registration-failure rollback,
power-loss durability, or a deployment-qualified release build.
A subsequent current-application run built SeaweedFS commit
`79936b2ead2ba87a3399a1f2249a0fa08aa4480e` with Windows/amd64, CGO disabled,
`5BytesOffset`, and the full commit embedded in its version. The clean local
Go-FUSE dependency was `1bdeec4d57d1e9ee85d4938f36f2ed876dd7bd5e`.
Weed executable SHA-256:
`58577fe7eb71ffac12aa8d0f540cd3953ffda891d974222c71bc7d7cc30a5c1c`;
native test executable SHA-256:
`71e02b7d5c3255c3f5010bd81a5b4b4db80ebc73996cd4495ba6d334055d33c4`.
With the same candidate DLL `1ea9a10d...` and official LFS 3.8.0, all five
`GitLfsTempMetadata` cycles passed in
`/tmp/seaweedfs-windows-mount-results-1686275365` (742.328 seconds, harness exit 0).
Each retained log independently verifies the loaded DLL, all 20 LFS status
iterations, 32 modified assets, native canonicalization and rename/content
checks without skips, graceful unmount and its unique completion marker.
Setup log SHA-256:
`cb3d961efebe98a74a0cea546559131e61a46d4a6ee7ff91751f3613014f3b05`;
cycle-5 log SHA-256:
`615854a6809c33cac259f60e8a4585ec1108ba6c450a3f070f8b982a83854e6c`.
Auxiliary collection succeeded: 5,863,822 bytes, matching guest/host SHA-256
`255b59cba8a6b72ff9d6ed2c120c80bc068d49d1c44653cae8ed7318a050f952`.
This closes the older-diagnostic-weed limitation for this workload; it still
uses the lab MinGW DLL and is not MSVC/package or power-loss qualification.
The companion current-build `GitAtomicRenamePrimed` run also passed all five
cycles with the same executable/DLL hashes in
`/tmp/seaweedfs-windows-mount-results-2116467326` (452.621 seconds, exit 0).
Each retained log verified the loaded DLL, namespace/content checks, all 20
Git `config.lock` replacement iterations, graceful unmount and its completion
marker. Cycle-5 log SHA-256:
`dc6d2d961de7b907ec499b6ed79fc80e75942c79bf903988630dcce1e83bc383`.
Auxiliary transfer succeeded with 2,748,847 bytes and matching guest/host SHA-256
`1f1ac8c6ab56bbd48204de5de633cd2bb3183ca12f2f8ff4fa3a0211bd0641f1`.
Subsequent harness runs materialize auxiliary log tails in the guest and use the
existing chunked, SHA-256-verified artifact transfer instead of one oversized
exec response. `guest-logs.txt` contains the diagnostic text;
`guest-logs-collection.txt` records collection status, byte count and SHA-256.
The existing 2,000-line-per-file tail policy remains: this is not a full log
archive. Collection has a separate two-minute deadline and now fails the harness
on error rather than silently allowing missing auxiliary evidence. Unit coverage
reproduces the old truncation failure with more than 3 MiB of diagnostic text.
The companion candidate `GitAtomicRenamePrimed` run completed five cycles in
`/tmp/seaweedfs-windows-mount-results-4001340908` (418.175 seconds, exit 0), each
with 20 repeated Git config-lock replacement checks and graceful unmount.
Cycle-5 log SHA-256:
`cabd032651d168acf61671d59e71375fc4574255a16850f492d49fb9ac05c230`.
The repaired collector downloaded 2,749,554 bytes successfully; host hash matched
the guest hash recorded in `guest-logs-collection.txt`:
`9b1fee83dff6bd423fc291e771b0237fa28674c3f1966441328050b1cc74b53a`.

Related upstream [WinFsp issue 441](https://github.com/winfsp/winfsp/issues/441)
was fixed by
[`ea189c5b`](https://github.com/winfsp/winfsp/commit/ea189c5b683b25eeca0cdb60fd12e27536db1c01),
which preserves explicit mount-manager directory prefixes during Cygwin path
conversion. Its directory-prefix branch and enlarged buffer are already present
in our pinned v2.1 baseline's `src/dll/fuse/fuse.c`. That existing fix therefore
does not resolve the delayed mapping loss reproduced by this native test; it
is not an alternative patch to merge for this regression.

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

The hard-link-safe instrumented run at
`/tmp/seaweedfs-windows-mount-results-2253016600` reproduced the real failure:
cycle 1 passed, cycle 2 failed `lfs install --local`. For both the repository
directory and `.git`, `GetFinalPathNameByHandle` flags 0 and 8 (DOS paths)
returned error 3, while flags 1/9 (GUID paths) and 2/10 (NT device paths)
succeeded on the same open handle. This localizes this occurrence to DOS
drive/mount-path translation, not a missing `.git` entry or failed open.
It does not yet identify the responsible registration/translation defect.
Cycle 2's ETL SHA-256 is
`a0afe6e315f5ef53f5591443d160e58b9cfaa6c2d97ccd5c8e6ed11b1a5e550d`;
decoded XML ZIP SHA-256 is
`d4cdda3b42c979e6b44048030011c3cfc6db34c6230d2e4d38f76d8db0b27d95`.
The full WinFsp trace and scenario failure text were also retained. Keep this
result separate from the original object-move failure until causality is proven.
The next traced reproduction, at
`/tmp/seaweedfs-windows-mount-results-1471069418`, failed cycle 3 with the same
DOS/GUID/NT split. Before unmount, `mountvol` listed the matching GUID volume
with `NO MOUNT POINTS`, while `fsutil reparsepoint query` showed that the lab's
mount junction still targeted the corresponding live NT device volume. Thus
the reverse mount mapping was absent even though the junction and volume
remained present. Cycle 3 ETL SHA-256:
`01516d3d3810fecea34aa2b779e5220ca65c6ece8428b7c11e408cc269f06778`.
An untraced 20-cycle attempt at
`/tmp/seaweedfs-windows-mount-results-1767952802` stopped on the first failure
in cycle 3 (`lfs track *.lfs`, 320 seconds total), again with successful GUID/NT
queries, failed DOS queries, an intact junction, and no registered mount point
for the matching GUID volume. Thus ETW/WinFsp debug capture is not necessary
to trigger the defect. No failed workload was retried into a pass.

The official Git LFS 3.8.0 comparison completed five traced LFS-only cycles
successfully in `/tmp/seaweedfs-windows-mount-results-735008305` (1116 seconds).
The official Windows archive SHA-256 was
`b62e7b8ceddee635f691233d77de8eaa4b213e9209e0173811d8cfa77f7882c1`;
the extracted executable SHA-256 was
`d1a2b2a90a3b8d57e68db6a6d0daefc7c96e12a1abd1c0849f41d13839252857`.
Git remained 2.51.0.windows.1 and the SeaweedFS binary remained the same
`2ef9c71d1` diagnostic candidate used for the failing 3.7.0 run. All five ETLs,
XML ZIPs and WinFsp logs were retained. This was not the full atomic/native
qualification gate, and is not evidence that the intermittent defect is fixed:
the Windows canonicalization function is unchanged between these LFS releases,
and older clients have also passed multiple cycles. Continue untraced stress
and root-cause verification; do not substitute this result for deployment gates.
The subsequent untraced 20-cycle attempt with that same official 3.8.0 executable
failed on cycle 6 during `lfs install --local` after five successful cycles
(`/tmp/seaweedfs-windows-mount-results-2039409665`, 408 seconds total).
The error was again `error converting ".git" to absolute: The system cannot
find the path specified.` The failure-only listing showed a WinFsp GUID volume
with no mount points and an intact lab junction targeting an NT device volume.
This unmodified client does not emit the same-handle GUID/NT probes, so those
details remain evidence from the earlier diagnostic-client runs, not this run.
The cycle-6 log SHA-256 is
`ef9e50bbd175439a6dc17896f797c48bc0344a740288d86c8e66961ce13b4ab3`;
the retained guest-log SHA-256 is
`4f05bd871d38de7841c86d61010ec21587146c2b3d692b7cb9c43deacd26ae02`.
Upgrading Git LFS to 3.8.0 alone therefore does not resolve this blocker.
The optional native executable adds 512 create/chmod/write/close/mkdir/rename
transactions per pair, including uppercase `.GIT` paths and exact final object
content checks. The Actions gate builds and requires this executable, runs five
pairs, and rejects skipped/missing native completion. A skipped opt-in
test is not qualification. Pin installer hashes and VM image digests for release
evidence; a local `latest` image run is exploratory only. The harness contract
test (`pwsh -File hack/appmana/mount-smoke-contract-test.ps1`) injects prerequisite
failures without starting a VM and runs in the existing Windows build workflow.
An intermittent LFS object-move failure has been observed in this lab; a later
successful run does not resolve it or qualify Windows deployment.

The subsequent Windows stress run also reproduced stale `config.lock` after a
successful server rename. `TestStreamRenameAckSurvivesClockBehindMetadataFence`
reproduces this cache failure deterministically when the monotonic metadata
clock is ahead of wall time. Streaming rename replies must use the actual
committed metadata events, not separately sampled wall-clock timestamps.
`TestStreamRenameRepliesMatchCommittedLog` checks reply/log equality, both
transports, overwrites, recursive moves, and data preservation on disconnect.
An acknowledgment can be lost **after commit**: an RPC error alone does not
prove the rename failed. Inspect source and destination before retrying; do not
delete a destination or assume an old lock file is disposable based on that
error. These tests do not establish that the original LFS object-move failure
has the same cause, nor qualify Synology packaging or cluster deployment.
The timestamp-fixed candidate subsequently reproduced the original LFS failure
on the fourth fresh LFS workload: the seed `git add` failed moving an object
with Windows `ERROR_FILE_NOT_FOUND`, despite logged source creation and
destination-directory creation. No matching filer rename request was logged.
The standalone native probe had passed 1,536 object transactions beforehand;
neither that nor a subsequent clean-build single-cycle pass closes this blocker.
Further deterministic regressions cover a separate cache failure:
`TestSaveEntryNoChangeAckPreservesCaseFoldListing` exercises the real no-event
`UpdateEntry` acknowledgment fallback, which previously deleted a live `.git`
cache entry at an equal timestamp and made `.GIT` lookup fail. In-place updates
must fence both halves as writes; only actual renames/deletes may remove the old
path at its existing version. Directory updates with omitted `NewParentPath`
must also preserve children. These tests fail on the pre-fix source and pass
with the cache correction; the captured LFS failure's causal link remains
provisional until runtime verification. Failure-only `winfsp rename failed at`
diagnostics at verbosity 1 identify which rename stage rejects a request.
The five-pair qualification attempt of clean source `d67fdbb5e905` also failed:
two atomic-rename scenarios passed (40 Git initializations), and the first LFS
scenario passed its seed, 20 status iterations, and 512 native object renames.
The second LFS scenario then failed during `lfs install --local` with
`error converting ".git" to absolute: The system cannot find the path specified.`
This precedes object rename, so rename-only diagnostics cannot locate the
failure. Results are retained locally in
`/tmp/seaweedfs-windows-mount-results-2696262570` (including RED/GREEN unit evidence
and build provenance); these temporary artifacts are not published CI evidence.
The Windows gate remains **failed**, not partially qualified. Verbosity 1 also
records failed component resolution, case-fold lookup, and attribute reads to
localize this earlier failure without retries or successful-path tracing.
`TestGitLfsDirectoryCanonicalization` additionally exercises Git LFS 3.7.0's
actual Windows directory `CreateFile` / `GetFinalPathNameByHandle` sequence,
after config-file replacements and through both original and uppercase paths.
The native smoke gate requires this test as well as the object-rename test;
cross-compilation alone does not qualify either. The adapter currently lacks
the optional cgofuse `Getpath` callback, a hypothesis to investigate with this
probe, not a proven explanation for the intermittent failure.
Follow-up testing of diagnostic-only SeaweedFS source `2ef9c71d1` passed five
fresh LFS scenarios (results `/tmp/seaweedfs-windows-mount-results-3535845596`).
A separate fresh VM using harness/test source `671054225` passed the real LFS
scenario, 256 native directory canonicalization checks, and 512 native object
renames without skips (results `/tmp/seaweedfs-windows-mount-results-535877824`).
That candidate still has no `Getpath` callback: the new probe is exercised
coverage, **not a RED reproduction** establishing a new fix. These positive
runs do not close the earlier intermittent failure or supersede the failed
five-pair qualification. Do not implement a speculative callback fix based on
its absence alone; capture which Win32 operation/callback fails and establish
a failing regression first.
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
