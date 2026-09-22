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

For safe local reproduction, see the [isolated storage lab](test/storage_lab/README.md).
It reuses existing tests with bounded resources and no production network or
writable host mounts. Linux, native Windows, and Synology SPK qualification are
separate gates; passing the Linux lab does not qualify the other packages.

Passing local tests does not authorize promotion; deployment requirements and
current operational facts belong in the existing AppMana `docs/seaweedfs.md`
runbook, not a second document in this source repository.

The candidate also backports upstream #11411, including its unchanged
five-byte-offset regression test; plain 4.47 does not contain that fix.
Index-based vacuum refuses unreadable or wrong-identity live records rather
than dropping them. This is a safety policy, not a tuning option: bad indexes
require diagnosis/repair before reclamation, even if that leaves disk usage high.

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
| `SEAWEEDFS_RELIABILITY_LABCONTAINERS_REF` | Full commit SHA for `AppMana/labcontainers` used to build `labd` and its Go SDK | Use a published, qualified commit containing explicit crashes and peer bridge restoration (locally tested: `a0b41e1c5123fc129ea7a02c8a091b847c8312cb`). Publish the commit before selecting it in Actions. Never point this at a moving branch. |
| `SEAWEEDFS_RELIABILITY_LABCONTAINERS_VM_IMAGE` | Qualified Ubuntu VM image reference including `@sha256:<64 hex>` | Build from the pinned source, publish and preload it on the dedicated runner, and update only after its live KVM smoke test passes. Mutable tags are rejected. |
| `SEAWEEDFS_RELIABILITY_LABCONTAINERS_WINDOWS_IMAGE` | Windows VM image including `@sha256:<64 hex>` | Preload the licensed image on the same dedicated runner. Run the native Windows core regressions and Labcontainers NTFS crash test before promotion. |

The initial baseline is `9ec822e2d634abc36eb2a113d0ddb4a844970873` (the audited
4.40 fork source), and the initial Go-FUSE pin is
`1bdeec4d57d1e9ee85d4938f36f2ed876dd7bd5e`. These are historical setup values,
**not duplicate workflow defaults**. The source baseline is newer than some
currently deployed role images; passing it does not replace testing each
deployed version before rollout.

```sh
gh variable set SEAWEEDFS_RELIABILITY_BASELINE_REF --repo AppMana/forks-seaweedfs --body "$BASELINE_SHA"
gh variable set SEAWEEDFS_RELIABILITY_GO_FUSE_REF --repo AppMana/forks-seaweedfs --body "$GO_FUSE_SHA"
gh variable set SEAWEEDFS_RELIABILITY_LABCONTAINERS_REF --repo AppMana/forks-seaweedfs --body "$LABCONTAINERS_SHA"
gh variable set SEAWEEDFS_RELIABILITY_LABCONTAINERS_VM_IMAGE --repo AppMana/forks-seaweedfs --body "$LABCONTAINERS_VM_IMAGE_AT_DIGEST"
gh variable set SEAWEEDFS_RELIABILITY_LABCONTAINERS_WINDOWS_IMAGE --repo AppMana/forks-seaweedfs --body "$WINDOWS_IMAGE_AT_DIGEST"
gh variable list --repo AppMana/forks-seaweedfs
```

Manual dispatch accepts `baseline_ref` to override the baseline for one run.
The candidate is derived dynamically from the checked-out workflow commit;
Go comes from `go.mod`. Missing/invalid pins and identical baseline/candidate
commits fail explicitly. The job summary records both resolved commits and
the dependency pin, and the binaries embed their source commits. A manual
override does not update the repository variable.

The intensive `vm-fault-gates` job runs only on a dedicated self-hosted runner
labelled `linux`, `x64`, `kvm`, and `seaweedfs-lab`. It requires `/dev/kvm`,
Docker, Containerlab 0.79.0, and the digest-pinned VM image, but no production
routes or credentials. Four fresh isolated labs test replicated concurrent
vacuum, acknowledged-write power loss, graceful baseline/candidate/rollback
migration, and power loss after observing the vacuum `.cpd` copy.

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

## Deployment-specific operations

Cluster hardware, GitOps settings, capacity accounting, release gates, and
reclamation procedures are maintained in AppMana's existing
`docs/seaweedfs.md` runbook, not duplicated in this source tree.

Upstream README: https://github.com/seaweedfs/seaweedfs
