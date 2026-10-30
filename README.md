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

- Every FUSE request is answered within `-fuse.requestTimeout`, and namespace
  requests (lookup, attributes, create, rename, readdir, xattrs) within the
  shorter `-fuse.metadataRequestTimeout`, because the kernel can chain several
  of them under one directory lock. The kernel is asked to abort the
  connection after `-fuse.kernelRequestTimeout` if the mount process itself
  stops answering. A stopped filer or volume server, or a deleted cache
  directory, returns EIO instead of hanging callers.
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
- The on-disk read cache is one content-addressed chunk directory shared by
  every mount on the node, with a quota per mount and one for the whole
  directory. It uses disk as chunks are cached instead of reserving its limit
  at start, and recreates a cache directory removed underneath it.
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
| `-cacheCapacityMB` | 128 | This mount's quota in the shared on-disk chunk cache. Disk is used as chunks are cached, up to the quota. `0` disables the disk cache. |
| `-wholeCacheCapacityMB` | 0 | Quota of the whole shared chunk directory across all mounts. `0` means `max(4 * cacheCapacityMB, 16384)`, or `2 * cacheCapacityMB` when that is under 64. |
| `-sharedChunkCacheDir` | derived | The shared chunk directory. By default `<cacheDir>/chunks`; when `cacheDir` is itself a 64 hex digit hash directory (as the CSI driver passes) or sits under a `volumes/` directory, the `chunks` directory beside them. |
| `-cacheDir` | system temp | Parent of the mount's metadata cache and write swap, kept in `<cacheDir>/<id>`, where `<id>` hashes the mount directory, filer, filer path and weed version: a different version starts a new directory and leaves the old one, which nothing removes. |
| `-writeBufferSizeMB` | 0 (no cap) | Cap on the mount's dirty data, memory plus swap. Writers wait for uploads when it is full, so `close()` drains at most this much. |
| `-fuse.requestTimeout` | 60 s | A request still unanswered after this long gets EIO and is cancelled. Must be below the kernel timeout. `0` disables. |
| `-fuse.metadataRequestTimeout` | 25 s | The same for namespace requests. Several can run in sequence under one directory lock, so their sum must stay below the kernel's 120 s hung task window. `0` uses `-fuse.requestTimeout`; it must not exceed it. |
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

A mount's disk use is bounded by its `cacheCapacityMB` share of the chunk
cache plus `writeBufferSizeMB` of swap; the node's chunk directory as a whole
by `wholeCacheCapacityMB`. Size `writeBufferSizeMB` so that it drains well
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

### 4.51 and 4.48 comparison

`test/mount_bench` on one Linux host (Ryzen 9 5950X, NVMe, btrfs), `weed mini`
plus a default `weed mount` per binary, both built with `-tags 5BytesOffset`:
4.48 (5919f7617) against 4.51 (1e3820260). Median of four rounds run in
alternating order. The host was loaded, so round-to-round spread is 9 to 51%
and every difference below is within it.

| Phase | 4.48 | 4.51 | Change |
| --- | --- | --- | --- |
| seq_write 256 MiB | 714.8 MB/s | 708.3 MB/s | -0.9% |
| seq_read 256 MiB | 985.5 MB/s | 1034.6 MB/s | +5.0% |
| rand_read 4 KiB x 500 | 1626.9 MB/s | 1526.6 MB/s | -6.2% |
| small_write 64 KiB x 400 | 21.8 MB/s | 22.1 MB/s | +1.5% |
| small_read 64 KiB x 400 | 296.0 MB/s | 336.5 MB/s | +13.7% |
| stat_files | 82759 ops/s | 94426 ops/s | +14.1% |
| list_dir | 1930 ops/s | 2117 ops/s | +9.7% |
| overwrite | 29.3 MB/s | 29.7 MB/s | +1.3% |
| delete | 1544 ops/s | 1622 ops/s | +5.1% |
| list_5k | 202436 ops/s | 200478 ops/s | -1.0% |
| walk_5k | 279237 ops/s | 280711 ops/s | +0.5% |

## CI

| Workflow | Trigger | Does |
| --- | --- | --- |
| [`appmana-server-images.yml`](.github/workflows/appmana-server-images.yml) | push to `merge/upstream-4.51` touching `weed/**`, `go.mod`, `go.sum` or the docker inputs; dispatch | Tests and builds native amd64 and arm64 large-disk servers and publishes `ghcr.io/appmana/seaweedfs:sha-<commit>_large_disk` as one two-platform manifest. |
| [`appmana-storage-reliability.yml`](.github/workflows/appmana-storage-reliability.yml) | push to `master`, `main` or `merge/**`, pull requests, dispatch | Race-tested unit suites, real FUSE gates, mutant controls, maintenance, packaged-image, etcd-restore, isolated lab and rollback gates on hosted runners; VM fault, hung-task and native Windows gates on a self-hosted KVM runner (pushes and dispatch only). |
| [`appmana-weed-windows.yml`](.github/workflows/appmana-weed-windows.yml) | push to `merge/**`, pull requests into `merge/**` | Cross-compile and native `weed.exe` builds, WinFsp mount smoke, Cuzz schedule fuzzing, WinFsp conformance (advisory) and mount benchmarks. |

Mount images are built by the CSI fork's `build-images.yml` from the commit
it pins. Deploy only CI-built images, by digest.

Repository variables the workflows read (full commit SHAs unless noted):

| Variable | Use |
| --- | --- |
| `SEAWEEDFS_RELIABILITY_GO_FUSE_REF` | `AppMana/forks-go-fuse` commit checked out beside the repo for the `go.mod` replace. Required by the server image and reliability workflows. |
| `SEAWEEDFS_RELIABILITY_BASELINE_REF` | Previous deployed commit; the upgrade, rollback and migration gates run it against the candidate. Must differ from the candidate. |
| `SEAWEEDFS_PERFORMANCE_BASELINE_REF` | Vanilla upstream commit for the interval and index startup performance comparison. |
| `SEAWEEDFS_RELIABILITY_LABCONTAINERS_REF` | `AppMana/labcontainers` commit; must match the SDK pinned in `test/storage_lab/vm/go.mod`. |
| `SEAWEEDFS_RELIABILITY_LABCONTAINERS_VM_IMAGE`, `SEAWEEDFS_RELIABILITY_LABCONTAINERS_WINDOWS_IMAGE` | Linux and Windows Server VM images as `name@sha256:…`, the Linux one labelled with the Labcontainers revision. A repository secret of the same name takes precedence and keeps a private registry name out of public logs. |
| `SEAWEEDFS_RELIABILITY_WINFSP_MSI_PATH`/`_SHA256`, `SEAWEEDFS_RELIABILITY_GIT_INSTALLER_PATH`/`_SHA256` | Optional offline installers on the lab runner; unset, CI downloads the pinned releases and checks their digests. |
| `SEAWEEDFS_RELIABILITY_GIT_LFS_PATH`/`_SHA256` | Optional Git LFS binary for client comparison in the Windows VM. |
| `SEAWEEDFS_ETCD_TEST_IMAGE` | Optional etcd image by digest for the restore gate. |

[AGENTS.md](AGENTS.md) describes how to build, test and merge locally. The
[storage lab README](test/storage_lab/README.md) describes the isolated
runners and VM scenarios.

## Branches

| Branch | Role |
| --- | --- |
| `merge/upstream-4.51` | Default branch: the maintained 4.51 line. Fork fixes merge here without rebasing; server and mount builds target this line. |
| `merge/upstream-4.48` | Previous line, no longer updated; every commit on it is in `merge/upstream-4.51`. |
| `merge/upstream-<next>` | Next upstream release merged into the default branch. It becomes the default once it passes the gates; the server image workflow's branch filter moves with it. |

The default branch advances only by merges: fork fixes and upstream releases
merged in on the next `merge/upstream-*` branch.

[Upstream](https://github.com/seaweedfs/seaweedfs)
