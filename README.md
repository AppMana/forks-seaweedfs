# SeaweedFS fork: Windows mounts and bounded FUSE mounts

SeaweedFS 4.51 with a WinFsp `weed mount` for Windows, FUSE mounts that
always answer the kernel, and fixes for allocation, vacuum and recovery,
memory use, S3 copies and cross-mount coherence. Every change carries a
regression test that fails without it.

## Packages

| Package | Use |
| --- | --- |
| [ghcr.io/appmana/seaweedfs](https://github.com/orgs/AppMana/packages?repo_name=forks-seaweedfs) | Master, filer, volume and S3 services (`sha-<commit>_large_disk`, linux/amd64 and linux/arm64) |
| [forks-seaweedfs-csi-driver](https://github.com/AppMana/forks-seaweedfs-csi-driver) | Kubernetes CSI driver and mount images for Linux and Windows nodes, built from a pinned commit of this repo |
| [spk-seaweedfs](https://github.com/AppMana/spk-seaweedfs) | Synology DSM package |

All builds are **large-disk** (`-tags 5BytesOffset`). Their index format
differs from normal upstream builds, so use large-disk builds across upgrades
and rollbacks. Set container memory limits and leave the Go memory and
upload/download admission overrides unset to get automatic sizing.

Windows CSI installs WinFsp automatically. Standalone `weed.exe` is a
developer binary and needs the WinFsp MSI on the host; see
[Windows behavior](WINDOWS_PORT.md) for caching and permission limits.

## What the fork changes

**Mount (Linux and Windows)**

- Every FUSE request is answered within `-fuse.requestTimeout`; the kernel is
  asked to abort the connection after `-fuse.kernelRequestTimeout` if the
  mount process itself stops answering. A stopped filer or volume server, or
  a deleted cache directory, returns EIO instead of hanging callers.
- Chunk uploads are bounded per attempt; the metadata commit deadline starts
  after the data upload, so a large `close()` over a slow link no longer
  fails after its data was stored.
- A chunk upload that stops making progress is abandoned and retried on a
  new assignment, never the same fid, that avoids every volume server an
  attempt stalled on in the last minute: the writer names those servers in
  the assignment request (`avoid_servers`), and the master hands out a volume
  none of them holds while one is writable (with an older master the writer
  asks again, up to eight times). Progress is bytes handed to the
  connection or acknowledged by the peer (TCP_INFO on Linux): an upload is
  abandoned after 10 s with nothing sent and acknowledged, or 20 s with no
  response once the body is out. A slow upload that keeps moving is never
  cut off. A primary whose forward to a replica stops moving for 5 s fails
  it and names the replica, so the client avoids it too. One stalled volume
  server no longer fails `close()`.
- The S3 gateway applies the same rule to object writes and to
  `CopyObject`/`UploadPartCopy` chunk copies (30 s response wait), avoids a
  stalled destination or source for a minute, and moves the chunk to a new
  assignment instead of failing the request with 500.
- `-writeBufferSizeMB` bounds dirty data (memory and swap), so `close()` and
  `fsync()` drain within the request deadline.
- Kernel invalidations that take a directory or page lock are held until no
  request holding that lock is in flight (the notify gate), so remote changes
  cannot deadlock local requests.
- Files another mount has closed are visible to listings and lookups that
  start afterwards: a cached directory confirms with the filer
  (`DirectoryChangePosition`) that it has applied every change before serving
  from cache.
- Name and attribute caches expire after remote namespace and metadata
  changes; remotely unlinked names detach from surviving open handles;
  overwrites are ordered after observed chunk versions.
- A metadata store that cannot apply an event degrades to read-through
  instead of serving stale entries.
- The on-disk read cache uses disk as chunks are cached instead of reserving
  its whole limit at start, and recreates a cache directory removed
  underneath it.
- Swap and buffered-write failures are reported, never acknowledged as
  written; deferred creates survive cache eviction and flush serially.
- Sequential/random read detection no longer misclassifies concurrent or
  mmap reads; `-readerCacheMode` overrides it.

**Windows mounts (WinFsp)**: native `weed mount` through cgofuse without
cgo; network-FS mode for container consumption; case-insensitive by default;
kernel caching with filer-event invalidation and finite timeouts; per-handle
read-ahead; canonical names, Unicode and NFS reparse entries; volume-relative
symlinks; opt-in basic permissions. Details in [WINDOWS_PORT.md](WINDOWS_PORT.md).

**Filer and S3**

- Chunk lists shared between entries are reference counted: a same-bucket
  `CopyObject` shares the source's chunks, and update, rename and uncertain
  store writes never garbage-collect shared chunks.
- Chunk compaction under a cancelled context no longer panics; LevelDB2
  listings stop at the end of the directory; streaming renames are
  acknowledged with committed metadata events.
- S3 chunk copies cancel with the request, stream with an exact
  `Content-Length`, and do not allocate a chunk-sized buffer per chunk.
  Identities reload from `-s3.config` without a restart; the empty-directory
  marker and bucket empty-folder cleanup are opt-in.
- HTTP chunk reads resume where a failed attempt stopped, reject invalid
  partial responses and bound how much of an unread response is drained.

**Volume servers and maintenance**

- `GOMEMLIMIT` and transfer admission derive from the cgroup memory limit;
  concurrent transfer bytes are reserved atomically; multipart uploads
  stream instead of copying the payload.
- Durable index tails replay regardless of log mtime; partial unindexed
  append tails survive restarts; vacuum refuses mismatched data and index
  boundaries instead of discarding live records.
- An absent-only repair RPC restores a missing needle from a checksum-pinned
  replica; scoped `volume.fsck` preserves cross-collection references and
  skips needles whose copied index went stale; maintenance commands reject
  malformed arguments and propagate disk-check failures.
- A volume that fails to open fails its load instead of crashing the server;
  a master that does not know a volume does not report it under-replicated.

## Mount configuration

| Flag | Default | Effect |
| --- | --- | --- |
| `-cacheCapacityMB` | 128 | On-disk read cache per mount, in three tiers (1/8, 3/8 and 1/2 of the limit). Disk is used as chunks are cached, up to the limit. `0` disables it. |
| `-cacheDir` | system temp | Parent of the mount's cache. The mount keeps its cache and write swap in `<cacheDir>/<id>`, where `<id>` hashes the mount directory, filer, filer path and weed version: a different version starts a new directory and leaves the old one, which nothing removes. |
| `-writeBufferSizeMB` | 0 (no cap) | Cap on the mount's dirty data, memory plus swap. Writers wait for uploads when it is full, so `close()` drains at most this much. |
| `-fuse.requestTimeout` | 60 s | A request still unanswered after this long gets EIO and is cancelled. Must be below the kernel timeout. `0` disables. |
| `-fuse.kernelRequestTimeout` | 90 s | Negotiated at `FUSE_INIT` on Linux 6.14+: the kernel aborts the connection if a request stays unanswered this long. Whole seconds; `0` disables. |
| `-readerCacheMode` | `auto` | `sequential` caches whole chunks always (mmap-heavy reads such as model loading); `random` never. |
| `-winfspOptions`, `-winfspCaseSensitive`, `-winfspBasicPermissions`, `-volumeLabel` | | Windows mounts only; see [WINDOWS_PORT.md](WINDOWS_PORT.md). |

Fixed deadlines inside the mount: a filer call a FUSE request waits on gives
up after 60 s, one chunk upload attempt after 10 s without progress or 20 s
without a response once sent, a volume server's forward to one replica after
5 s without progress or response, and the metadata commit
after a flush 30 s after the data upload finished. A listing or negative
lookup in a cached directory waits up to 1 s for an in-flight event, and
serves the cache after 5 s when the filer is unreachable.

A mount's disk use is bounded by `cacheCapacityMB + writeBufferSizeMB` in its
current cache directory. Size `writeBufferSizeMB` so that it drains well
inside the request timeout at the node's uplink rate (512 MiB at 46 MB/s is
about 11 s).

## Benchmarks

Results for the scenarios below; timings depend on workload and host load.

| Scenario | Result |
| --- | --- |
| 4 GiB sequential write, `-writeBufferSizeMB=512`, 1 GbE uplink | write 80.4 s, `close()` 8.8 s, 46 MB/s |
| 256 MiB sequential write, 16 MiB/s uplink | `close()` 15.7 s uncapped; 1.35 s with `-writeBufferSizeMB=32` |
| Missing-file `stat` in a cached directory (cross-mount freshness) | 24 µs before, about 200 µs after (one filer round trip) |
| `opendir` + list of 200 entries | 63 to 75 µs before, 210 to 320 µs after |
| `TestCrossMountCloseToOpenVisibility`, 40 rounds through a 100 ms proxy | 186 stale observations before, 0 after |
| 64 MiB write and `close()` with one of three volume servers stopped, holding 8 of 11 writable volumes (`TestCloseSucceedsWhenAnAssignedVolumeServerStalls`) | EIO after 60.0 s before; with retries drawing assignments at random, EIO after 60.0 s or `close()` 45.5 s (3 of 3 runs); asking the master to avoid it, `close()` 23.0 s (9 of 10), 29.4 s once |
| 4 MiB write and `close()` through a 128 KiB/s link (`TestSlowButProgressingUploadsClose`) | EIO after 60 s with a 30 s response-header or absolute attempt bound; `close()` in 31.9 s progress-based |

### 4.48 and 4.47 comparison

`test/mount_bench` on one Linux host (32 cores, NVMe), `weed mini` plus a
default `weed mount` per binary, both built with `-tags 5BytesOffset`:
4.47 build 0f3d7bec5 against the 4.48 merge. Median of four rounds run in
alternating order; round-to-round spread is up to about 8%, so every
difference below is within noise.

| Phase | 4.47 fork (0f3d7bec5) | 4.48 merge | Change |
| --- | --- | --- | --- |
| seq_write 256 MiB | 251.0 MB/s | 251.1 MB/s | 0.0% |
| seq_read 256 MiB | 1053 MB/s | 1028 MB/s | -2.4% |
| rand_read 4 KiB x 500 | 1776 MB/s | 1914 MB/s | +7.8% |
| small_write 64 KiB x 400 | 30.3 MB/s | 29.8 MB/s | -1.7% |
| small_read 64 KiB x 400 | 105.9 MB/s | 106.1 MB/s | +0.2% |
| stat_files | 112650 ops/s | 107687 ops/s | -4.4% |
| list_dir | 2580 ops/s | 2382 ops/s | -7.7% |
| overwrite | 41.9 MB/s | 41.4 MB/s | -1.1% |
| delete | 2025 ops/s | 2019 ops/s | -0.3% |
| list_5k | 224041 ops/s | 232973 ops/s | +4.0% |
| walk_5k | 382720 ops/s | 358447 ops/s | -6.3% |

## How CI builds and qualifies

| Workflow | Trigger | Does |
| --- | --- | --- |
| [`appmana-server-images.yml`](.github/workflows/appmana-server-images.yml) | push to the default branch (`weed/**`, `go.mod`, docker inputs) | Tests and builds native amd64 and arm64 large-disk servers, publishes `ghcr.io/appmana/seaweedfs:sha-<commit>_large_disk` as one two-platform manifest. A candidate only: qualification is separate. |
| [`appmana-storage-reliability.yml`](.github/workflows/appmana-storage-reliability.yml) | push to `merge/**`, pull requests, dispatch | Race-tested unit suites, real FUSE gates, maintenance, packaged-image, etcd-restore, isolated lab and rollback gates on hosted runners; VM fault, hung-task and native Windows gates on a self-hosted KVM runner. |
| [`appmana-weed-windows.yml`](.github/workflows/appmana-weed-windows.yml) | push to `merge/**`, pull requests | Cross-compile and native `weed.exe` builds, WinFsp mount smoke, conformance and mount benchmarks. |

Mount images are built by the CSI fork's `build-images.yml` from the commit
its `SEAWEEDFS_COMMIT` pins. Deploy only CI-built images, by digest.

Qualify a change locally the way CI does:

```sh
# unit and race
GOWORK=off go test -short -tags 5BytesOffset -race -count=1 ./weed/mount/... ./weed/filer/... ./weed/storage/... ./weed/util/chunk_cache/...
python3 -m unittest discover -s test/storage_lab -v

# real FUSE mounts on this host; WEED_BINARY selects the binary under test
go build -tags 5BytesOffset -o /abs/weed ./weed
(cd test/fuse_integration && WEED_BINARY=/abs/weed go test -count=1 -v -timeout=20m . \
  -run '^(TestUnresponsiveBackendAnswersWithinRequestTimeout|TestLargeSequentialWriteClosesWithinWriteBufferDrainTime|TestWriteBufferCap|TestCrossMountCloseToOpenVisibility|TestCloseSucceedsWhenAnAssignedVolumeServerStalls|TestSlowButProgressingUploadsClose)$')

# disposable-VM gates (guest panics on hung tasks); needs KVM and Labcontainers
SEAWEEDFS_STUCK_LAB_LIVE=1 SEAWEEDFS_NOTIFY_LAB_LIVE=1 SEAWEEDFS_LINUX_WEED=/abs/weed \
LABCONTAINERS_LABD=/abs/labd LABCONTAINERS_VM_IMAGE=<pinned image> \
GOWORK=off go test ./test/storage_lab/vm -run '^(TestFuseStuckBackendLab|TestFuseReverseInvalidationLab)$' -v -timeout 90m
```

The [storage lab README](test/storage_lab/README.md) covers the four-VM
fault, Windows and mixed-OS scenarios and their inputs. CI takes the
Labcontainers VM image (`name@sha256:…`, labelled with `LABCONTAINERS_REF`)
from the repository secret `SEAWEEDFS_RELIABILITY_LABCONTAINERS_VM_IMAGE`, or
the variable of the same name, and the Windows Server image from
`SEAWEEDFS_RELIABILITY_LABCONTAINERS_WINDOWS_IMAGE` the same way; a secret
keeps a private registry's name out of public logs.

## Branches

| Branch | Role |
| --- | --- |
| `merge/upstream-4.51` | Maintained 4.51 line. Fork fixes merge here without rebasing; server and mount builds target this line. |
| `merge/upstream-<next>` | Next upstream release merged into the default branch. It becomes the default once qualified; the server image workflow's branch filter moves with it. |
| `release/<change>-<base>` | Short-lived ship branch: a deployed commit plus only the change being rolled out. It merges back into the default branch and is then deleted. |

The default branch advances only by merges: fork fixes, ship branches merged
back, and upstream releases merged in on the next `merge/upstream-*` branch.

[Upstream](https://github.com/seaweedfs/seaweedfs)
