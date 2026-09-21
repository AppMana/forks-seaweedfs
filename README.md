# forks-seaweedfs — SeaweedFS with Windows mount support

AppMana fork of [seaweedfs/seaweedfs](https://github.com/seaweedfs/seaweedfs),
originally branched from **4.23**, now carrying the AppMana **4.40** integration
and storage reliability fixes. The **4.47 upgrade is being qualified, not yet
approved for deployment**. Besides Windows mounts, this fork changes allocation,
compaction/recovery, admission control, S3 behavior, and CSI-facing semantics.

```
weed mount -filer=<filer:port> -dir=C:\mnt\seaweedfs
```

- The mount is served through [WinFsp](https://winfsp.dev/) via
  [cgofuse](https://github.com/winfsp/cgofuse) in its no-cgo mode, so
  `CGO_ENABLED=0 GOOS=windows` cross-compiles from Linux. WinFsp must be
  installed on the host (the mount preflights for it).
- The adapter (`weed/mount/winfsp_*_windows.go`) layers cgofuse's path-based
  API over the existing inode-based mount filesystem, so the battle-tested
  read/write/rename pipelines are reused, not reimplemented. Includes
  per-handle sequential read-ahead.
- `WEED_WINFSP_VOLUME_PREFIX=\seaweedfs` switches to a WinFsp *network* file
  system (UNC path). This is required when containers consume the mount:
  Windows HCS refuses to bind local WinFsp volumes into containers
  ([winfsp#498](https://github.com/winfsp/winfsp/issues/498)).
- `-winfspOptions=k=v,...` passes raw WinFsp options. `FileInfoTimeout=-1`
  enables kernel data caching (large speedup for small reads) but is only
  safe on read-mostly volumes; see `WINDOWS_PORT.md` for why.

## Large volumes

Production releases of this fork must use the **large-disk** variant
(`-tags 5BytesOffset`), matching the upstream `*_large_disk` releases: 5-byte
needle offsets raise the per-volume size limit (8TB volume files) for big-disk
deployments. Build:

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset -o weed.exe ./weed
```

`go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with a sibling checkout of
[AppMana/forks-go-fuse](https://github.com/AppMana/forks-go-fuse) (a
compile-only Windows port of the fuse package) — clone it next to this repo
as `../forks-go-fuse` before building.

`WINDOWS_PORT.md` documents every file changed relative to upstream, the
behavior notes (case sensitivity, xattrs, locking), and the caching
investigation. CI cross-compiles, runs native Windows tests, executes a real
WinFsp mount smoke test, and benchmarks every build.

Used by [AppMana/forks-seaweedfs-csi-driver](https://github.com/AppMana/forks-seaweedfs-csi-driver)
to serve SeaweedFS persistent volumes to Windows Kubernetes nodes.

## Reliability CI: configuration and updating the baseline

The [storage reliability workflow](.github/workflows/appmana-storage-reliability.yml)
runs on `merge/**`, `master`, `main`, pull requests, and manual dispatch. It
uses the existing storage tests and `test/volume_server` process harness. It
does **not** publish images, reclaim production data, or deploy anything.

Configure these **Actions repository variables**, not secrets, under Settings →
Secrets and variables → Actions → Variables:

| Variable | Purpose | Update rule |
| --- | --- | --- |
| `SEAWEEDFS_RELIABILITY_BASELINE_REF` | Full 40-character commit SHA for the source compatibility baseline | Change deliberately when the baseline is promoted; keep the previous deployed release in the upgrade/rollback matrix. Do not use `HEAD~1`, a branch, or `latest`. |
| `SEAWEEDFS_RELIABILITY_GO_FUSE_REF` | Full commit SHA for `AppMana/forks-go-fuse`, required by the local `go.mod` replacement | Change only alongside a reviewed and tested dependency upgrade. Both test builds use this pinned sibling. |

The initial baseline is `9ec822e2d634abc36eb2a113d0ddb4a844970873` (the audited
4.40 fork source), and the initial Go-FUSE pin is
`1bdeec4d57d1e9ee85d4938f36f2ed876dd7bd5e`. These are historical setup values,
**not duplicate workflow defaults**. The source baseline is newer than some
currently deployed role images; passing it does not replace testing each
deployed version before rollout.

```sh
gh variable set SEAWEEDFS_RELIABILITY_BASELINE_REF --repo AppMana/forks-seaweedfs --body "$BASELINE_SHA"
gh variable set SEAWEEDFS_RELIABILITY_GO_FUSE_REF --repo AppMana/forks-seaweedfs --body "$GO_FUSE_SHA"
gh variable list --repo AppMana/forks-seaweedfs
```

Manual dispatch accepts `baseline_ref` to override the baseline for one run.
The candidate is derived dynamically from the checked-out workflow commit;
Go comes from `go.mod`. Missing/invalid pins and identical baseline/candidate
commits fail explicitly. The job summary records both resolved commits and
the dependency pin, and the binaries embed their source commits. A manual
override does not update the repository variable.

Workflow-level variables `STORAGE_BUILD_TAGS`, `STORAGE_UNIT_TEST_TIMEOUT`, and
`STORAGE_PROCESS_TEST_TIMEOUT` define the build format and test deadlines in
one place. `5BytesOffset` is a storage-format requirement, **not** a tuning
knob: never open existing large-disk indexes with a four-byte-offset binary.
The older Windows workflow separately configures `GO_FUSE_BRANCH` and
`WINFSP_MSI_URL`; review those when changing the dependency or Windows runtime.

### Running locally

Keep `../forks-go-fuse` checked out at the tested dependency pin. Build baseline
and candidate from separate source worktrees with the same offset format:

```sh
go test -short -tags 5BytesOffset -race -count=1 -timeout=5m ./weed/storage ./weed/topology ./weed/shell ./weed/server
go build -tags 5BytesOffset -o /absolute/test-bin/weed-candidate ./weed

WEED_BINARY=/absolute/test-bin/weed-candidate \
WEED_VOLUME_BINARY=/absolute/test-bin/weed-baseline \
WEED_CANDIDATE_BINARY=/absolute/test-bin/weed-candidate \
VOLUME_SERVER_IT_KEEP_LOGS=1 \
go test -tags 5BytesOffset -count=1 -timeout=5m ./test/volume_server/grpc -run '^TestVolumeBinaryUpgradeVacuumRollback$'
```

`WEED_BINARY` selects the master/default binary; `WEED_VOLUME_BINARY` selects
the initial volume-server binary; `WEED_CANDIDATE_BINARY` selects its upgrade.
Use absolute executable paths. Without explicit baseline/candidate paths the
migration test **skips**, so a generic suite pass is not migration evidence.
The harness otherwise builds a fresh binary matching the test's offset format,
uses loopback ports and temporary volumes, and retains failed-test logs.
`VOLUME_SERVER_IT_KEEP_LOGS=1` also retains successful runs under
`/tmp/seaweedfs_volume_server_it_*/`. Clean up only a verified test directory
after reviewing it; never point test cleanup at production storage.

The migration test checks payloads through baseline → candidate → vacuum with
a concurrent write → baseline rollback → candidate process-crash recovery.
Sparse-file tests cover relocation at 32, 128, and 200 GiB without physically
allocating those sizes. Repeated overwrite/delete/vacuum/restart tests require
bounded `.dat` size for a fixed live dataset, on memory and LevelDB indexes.
SIGKILL is **not power loss**: host page-cache survival does not prove durable
acknowledgements.

## Operational configuration: what must move together

The cluster's GitOps manifests are in
`appmana-cluster/clusters/appmana-cluster-03/seaweedfs/`; do not edit running
pod executables or substitute direct Helm installs for Flux rollout.

| Configuration/source | Requirement and update responsibility |
| --- | --- |
| Master/filer `helm-release.yaml`, per-disk `volume-statefulsets.yaml`, dedicated S3 gateway manifests | Pin qualified role images/digests and record their source commits. Current roles can differ; test those combinations and rollback before changing pins. |
| `master.extraEnvironmentVars.WEED_MASTER_VOLUME_GROWTH_COPY_{1,2,3,OTHER}` | Explicitly set to `1` for the conservative growth policy. Chart defaults override TOML through environment precedence. Validate the rendered StatefulSet, not just `master.config`. Keep TOML and the config-revision annotation consistent. |
| `volumeSizeLimitMB`, `volumePreallocate`, replication | Current design: 131072 MiB (128 GiB), no preallocation, default `020` (three copies on three physical hosts). Growth counts are logical volume IDs: `copy_3=1` creates three replicas, not three logical volumes. Changing defaults does not migrate old replicas. |
| Volume `-max` and NAS `volume.yaml` | Slot count/placement weight, not reserved bytes or true physical capacity. NAS target `700` needs capacity/placement validation before application; do not delete volumes to meet it. Preserve native XFS/Btrfs protections. |
| Upload/download admission, Go/container memory, timeouts | Admission includes replication and is not an RSS cap. Account for multipart buffers and page cache; ensure server admission timeout fits client deadlines. Test against each host's RAM/link/disk limits before changing values. |
| CSI driver and mount image source pins, Linux/Windows DaemonSets | Coordinate server and client releases; verify every passed mount flag against that exact binary (`df.logical` is not supported by every 4.40 fork build). Local chunk-cache capacity is **per mounted volume/process**, not a shared node cap. |
| `spk-seaweedfs/cross/seaweedfs/{Makefile,digests}`, package metadata and DSM override `weed.image`/`weed.digest` | Update source version/checksums and image **plus digest** together; the OCI cache is digest-keyed. Run package/supervisor tests. Leave degraded NAS SSD write-back bypassed pending hardware repair. |
| `etcd-backup-cronjob.yaml` | Backup target must remain outside SeaweedFS. Staged policy: 256 GiB PVC, 14-day retention before snapshot, protect newest verified published snapshot, at least 16 GiB staging headroom for the 12 GiB backend quota. Recalculate capacity/headroom if quota or retention changes. Expansion requires storage-driver support. Snapshot status is not a restore drill. |
| Filer ServiceMonitor post-render patch and `apps/monitoring/seaweedfs-dashboard.yaml` | Select one canonical Service (`monitoring=true` on `seaweedfs-filer-client`). Re-render and assert exactly one match after chart changes. Bucket counters are volume/needle-derived estimates, not an authoritative live namespace/object inventory; leader freshness still matters. |

The cluster contract tests render the exact chart version and exercise backup
preflight behavior. From the cluster repository:

```sh
uv run --no-project --with pytest --with pyyaml pytest -q tests/contracts/test_seaweedfs_capacity.py
```

### Deployment and reclamation gates

Local fixes/tests are not release approval. Before publishing/deploying:

1. Complete the 4.47 merge and fork-diff/source audit; retain the production
   offset format and Linux/Windows mount behavior.
2. Test each deployed baseline, mixed roles, rollback, S3/multipart/copy/abort,
   CSI Linux/Windows, and native XFS plus isolated NAS Btrfs volumes.
3. Verify a fresh external etcd backup by isolated restore. Run VM power-cut,
   ENOSPC, short-write, sync-failure, and metadata/replica integrity tests;
   acknowledged durable operations must survive, except explicit cache data.
4. Complete the 24-hour representative soak. Roll one instance at a time via
   GitOps; stop on integrity errors, unsafe replicas, or a required write outage.
5. Separately inventory application references, manifests, versions and uploads;
   reconcile concurrent writes and apply the agreed orphan grace/revalidation.
   Prove one canary reclaim with payload hashes and before/after physical bytes
   before expanding. Do not run overlapping vacuum jobs or remove commit markers.

Measured excess is **not a deletion list**. The investigation found about
56 TiB in SeaweedFS directories, 5.1 TiB in deleted-payload counters, a 2×
duplicate scrape, and about 3.9 TiB of XFS `df`/`du` difference consistent with
filesystem reservations. Harbor's approximately 20 TiB remaining gap is still
unclassified, not promised reclaimable space. Empty volume headers consume
kilobytes, not the nominal 128 GiB limit. Reclamation requires the full chain:
application reference removal → chunk deletion → vacuum → filesystem release.

Upstream README: https://github.com/seaweedfs/seaweedfs
