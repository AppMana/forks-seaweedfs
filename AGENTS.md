# Working on this fork

This is a fork of [seaweedfs/seaweedfs](https://github.com/seaweedfs/seaweedfs).
[README.md](README.md) lists what the fork changes; this file is how to build,
test, merge and release it.

## Checkout

- `go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with `../forks-go-fuse`.
  Clone [AppMana/forks-go-fuse](https://github.com/AppMana/forks-go-fuse) next to
  this repository, at the commit in repository variable
  `SEAWEEDFS_RELIABILITY_GO_FUSE_REF` (the server image and reliability
  workflows check out exactly that commit; the Windows workflow clones branch
  `appmana/windows-compile`). Without the sibling nothing builds.
- The Go version is the one in `go.mod`. With an older local Go, set
  `GOTOOLCHAIN=auto`.
- `test/storage_lab/vm` is a separate module that needs a Labcontainers
  checkout through a temporary `go.work`; see the
  [storage lab README](test/storage_lab/README.md). Use `GOWORK=off` for the
  root module if a workspace is active.

## Build

Every build the fork ships is large disk: `-tags 5BytesOffset`. Its index
format differs from default builds, so always test and ship with the tag.

```sh
go build -tags 5BytesOffset -o weed ./weed
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset ./weed/...
GOOS=windows GOARCH=amd64 go vet -copylocks=false -tags 5BytesOffset ./weed/command/ ./weed/mount/
go vet -copylocks=false -tags 5BytesOffset ./weed/mount/... ./weed/command/
```

Any change under `weed/mount`, `weed/command`, `weed/filer` or `weed/pb` must
also cross-compile for Windows. Stamp release binaries with
`-ldflags "-X github.com/seaweedfs/seaweedfs/weed/util/version.COMMIT=<full sha>"`.

## Test

Unit and race suites, as the reliability workflow runs them:

```sh
go test -short -tags 5BytesOffset -race -count=1 -timeout=10m \
  ./weed/storage ./weed/storage/needle ./weed/util/memlimit ./weed/command \
  ./weed/topology ./weed/shell ./weed/server \
  ./weed/filer ./weed/filer/empty_folder_cleanup ./weed/mount ./weed/mount/page_writer ./weed/operation
go test -tags 5BytesOffset -race -count=1 ./weed/s3api \
  -run 'TestStreamCopyChunkRange|TestNewChunkUploadOption|TestCopyChunkRangeMovesOffAStalledDestination'
go test -count=1 -tags 5BytesOffset ./weed/mount/... ./weed/command/... ./weed/filer/... ./weed/pb/... ./weed/s3api/...
python3 -m unittest discover -s test/storage_lab -v
python3 -m unittest discover -s hack/appmana -p 'test_*.py'
```

These packages run under `-race`; keep new concurrency in them covered by a
race-enabled test.

Real FUSE mounts on the host (needs `/dev/fuse` and a setuid `fusermount3`):

```sh
go build -tags 5BytesOffset -o /abs/weed ./weed
(cd test/fuse_integration && WEED_BINARY=/abs/weed go test -count=1 -v -timeout=20m . \
  -run '^(TestUnresponsiveBackendAnswersWithinRequestTimeout|TestLargeSequentialWriteClosesWithinWriteBufferDrainTime|TestWriteBufferCap|TestCrossMountCloseToOpenVisibility|TestCloseSucceedsWhenAnAssignedVolumeServerStalls|TestSlowButProgressingUploadsClose)$')
```

Process level volume server tests take `WEED_BINARY` (candidate),
`WEED_VOLUME_BINARY`, `WEED_REPAIR_BASELINE` and
`VOLUME_SERVER_IT_KEEP_LOGS=1`; see the reliability workflow for the exact
`./test/volume_server/{framework,grpc,http}` selections.

Mutant controls prove regressions still fail when their fix is reverted:

```sh
MUTANT_LOG_DIR=/abs/mutant-logs test/storage_lab/verify_mutants.sh
```

A mutant passes only when its regression fails with `--- FAIL:`; a compile
error is a broken control. When a fix moves, update its generator
(`compaction_mutant.py`, `fsck_mutant.py`, `offset_mutant.py`,
`preallocate_mutant.py`), which refuses changed or ambiguous patterns.

VM gates (KVM, Docker, Containerlab and Labcontainers): four-VM durability
(`vm/run_vm.sh`), Linux FUSE hang gates in a guest that panics on hung tasks
(`SEAWEEDFS_STUCK_LAB_LIVE=1`, `SEAWEEDFS_NOTIFY_LAB_LIVE=1`,
`SEAWEEDFS_LINUX_WEED`, `LABCONTAINERS_LABD`, `LABCONTAINERS_VM_IMAGE`),
mixed OS mounts and native Windows. Inputs are listed in the
[storage lab README](test/storage_lab/README.md). CI runs them only on pushes
and dispatch, on a self-hosted runner.

Rules for tests:

- Every fix gets a regression test, shown failing before the fix and passing
  after it. No skips or xfails, and no weakened assertions to accept old
  behaviour.
- A test script that resets a volume server wipes the whole data directory
  (`rm -rf dir && mkdir dir`), not `dir/*`. 4.51 writes `.vid_stamp` with the
  master's topology id; a stamp left from another cluster makes the master
  freeze volume growth (it takes the mismatch as lost master state), after
  which every assign fails. On a real cluster, verify the volume inventory so
  ids are not reused, then run `volume.growth.resume` in `weed shell`.

## CI

- `appmana-storage-reliability.yml`: pushes to `master`, `main` and
  `merge/**`, pull requests, dispatch. Needs repository variables
  `SEAWEEDFS_RELIABILITY_GO_FUSE_REF` and `SEAWEEDFS_RELIABILITY_BASELINE_REF`
  (full SHAs, baseline different from the candidate), plus the Labcontainers
  inputs for the VM job. The README lists every variable.
- `appmana-weed-windows.yml`: pushes and pull requests on `merge/**`.
- `appmana-server-images.yml`: pushes to `merge/upstream-4.51` (the default
  branch) that touch server inputs. It publishes
  `ghcr.io/appmana/seaweedfs:sha-<commit>_large_disk`. When a new line becomes
  the default branch, move this branch filter with it.

Mount images are built in
[forks-seaweedfs-csi-driver](https://github.com/AppMana/forks-seaweedfs-csi-driver)
from a pinned commit of this repository, and the Synology package in
[spk-seaweedfs](https://github.com/AppMana/spk-seaweedfs) downloads the source
archive of a pinned commit. Pinned commits must stay reachable: never rewrite
published history on `merge/**` branches.

## Merging upstream

- Merge, never rebase. Upstream releases arrive on a new
  `merge/upstream-<version>` branch cut from the default branch, which becomes
  the default branch once its gates pass. Fork fixes merge into the default
  branch. Never rebase merge-heavy history.
- Describe every conflict resolution in the merge commit message, one line
  per file or area.
- Validate a merge by building (Linux and Windows) and running the unit,
  race, FUSE and mutant suites. A clean textual merge can still break a fork
  invariant, so recheck these after each upstream merge:
  - `.dat` unindexed tail: complete needles past the index are re-indexed as
    upstream does, but an incomplete record is never cut; the volume loads
    read-only and keeps its bytes (`weed/storage/volume_checking.go`).
  - Every new writer of `.dat`/`.idx` (repair, compaction checks) takes
    `dataFileSyncLock` as well as `dataFileAccessLock`, so a synced write
    cannot be stranded across a compaction swap.
  - Disk space reservations committed by `VolumeCopy` are released on every
    later failure path.
  - The filer records `directoryChanges` after `RecordLocalEvent` has bumped
    the event timestamp.
  - Every filer RPC a FUSE request waits on goes through
    `awaitFilerReply`/`filerReplyContext`, including new upstream callers.
  - Reader files (`weed/filer/reader_at.go`, `reader_cache.go`,
    `reader_cache_budget.go`) keep the fork's pin and budget logic; take
    upstream changes into them by hand.
  - Upstream mount, filer and S3 options merged next to the fork's flags
    (`-fuse.metadataRequestTimeout`, `-writeBufferSizeMB`, `-readerCacheMode`,
    `-surfaceEmptyDirectories`, `-s3.shareCopyChunks`) keep the fork's
    defaults.
- When a regression from the merge is found, fix it in its own commit with
  its regression test, not in the merge commit.

## Commits

- Concise subject lines, prefixed with the area (`mount:`, `storage:`,
  `storage_lab:`).
- Reasoning, measurements and incident context go in the commit message
  body, not in code comments or docs. Docs describe current behaviour and
  current benchmarks only.
- Docs in this public repository carry no internal hostnames, dates or
  incident references.
