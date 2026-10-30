# Gateway Read-Path, Disk Chunk Cache, and Replica-Load Audit (`b9d3b99cb`)

## Executive Summary

Auditing `040ad1de8` (*"http: read each chunk from the replica answering fastest"*) and `b9d3b99cb` (*"filer: count chunks already in flight toward the prefetch window"*), together with the surrounding S3/Filer/Mount read paths and disk chunk cache, uncovered **nine distinct bugs** across the four audit areas:

1. **4.00× to 8.00× read amplification when `1 (pinned) + prefetchCount` exceeds the active `ReaderCacheBudget` (or when other slots are in flight)**:
   - `ReaderCacheBudget.reserveSelf` (`weed/filer/reader_cache_budget.go:108-123`) fell back to evicting **pinned** chunks (`pinnedVictim`) whenever all other budget slots were still in flight (`b.reservations`), and evicted `b.idle.Front()` FIFO—which for unread prefetches is the **next chunk the stream needs (`chunk1`)** rather than the furthest-ahead prefetch (`b.idle.Back()`).
   - `ReaderCache.MaybeCache` (`weed/filer/reader_cache.go:195-248`) launched up to `count` (`DefaultPrefetchCount = 4`) prefetches without checking whether `pinnedBytes + windowBytes + chunkAlloc` fit within `rc.budget.effectiveLimit()`. With 16 MiB chunks and `-sessionReaderCacheSizeMB=64` (4 chunks) or contended `-readerCacheSizeMB=512`, `1 pinned + 4 prefetched = 5 chunks > 4-chunk budget`, causing `doReadAt` (which calls `MaybeCache` on every 256 KiB S3 slice / 128 KiB Filer slice) to evict and re-download `chunk0` and `chunk1` on every single slice (**4.00× amplification on S3 GET, 8.00× on Filer GET**).
   - `ReaderCache.pickOldestVictimLocked` (`weed/filer/reader_cache.go:161-178`) similarly evicted the oldest unread prefetch (`chunk1`) instead of preferring already-read chunks (`readOnce != 0`) or the newest unread prefetch, and fell back to `pinnedFid` even while uncompleted downloads were in flight.
   - `doReadAt` (`weed/filer/reader_at.go:286-344`) dropped the `c.stream` pin whenever a sequential `ReadAt` buffer straddled a chunk boundary (`len(tasks) > 1`), because `readChunkSliceAtForParallel` called `ReadChunkAt` with `stream = nil`—leaving the finished chunk pinned and the newly entered chunk unpinned (`pins == 0, readOnce == 1`), so `MaybeCache` immediately evicted the newly entered chunk (**4.04× amplification on unaligned Range GET**).

2. **False `RandomMode` on open-ended Range GETs starting past `SeqTolerance` (8 MiB) and across `ChunkGroup` section boundaries**:
   - `ReaderPattern` (`weed/filer/reader_pattern.go:25-39`) starts with `lastReadStopOffset = 0`. Any `ChunkReadAt` whose first read starts at `offset >= 8 MiB` (`SeqTolerance`) was immediately classified as `RandomMode` for its first 4 slices (`SeqThreshold = 4`), causing the first 256 KiB of the first full chunk to be range-fetched and then re-fetched whole (`1.025×` amplification on `Range: bytes=10485760-`).
   - `ChunkGroup` (`weed/filer/filechunk_group.go:28-100`) created one `FileChunkSection` per 3200 chunks with its own isolated `ReaderPattern`, resetting to `RandomMode` at every section boundary past 8 MiB.

3. **Three bugs that crippled the disk chunk cache (`-cacheDir=/var/cache/seaweedfs -wholeCacheCapacityMB=51200 -cacheCapacityMB=2048`) in production**:
   - **File-offset vs. chunk-size bug in `shouldCache`**: `weed/filer/reader_at.go:414,424,454` and `weed/filer/reader_cache.go:239` computed `shouldCache := (uint64(chunkView.ViewOffset) + chunkView.ChunkSize) <= GetMaxFilePartSizeInCache()` instead of `chunkView.ChunkSize <= GetMaxFilePartSizeInCache()`. Because `ViewOffset` is the **byte offset in the file** and `-cacheCapacityMB=2048` creates tier 2 with `maxPartSize = 32 MiB`, **every chunk at file offset `> 32 MiB - chunkSize` had `shouldCache == false`**, skipping both disk-cache lookup (`readChunkAt` line 338) and disk-cache write (`startCaching` line 714) for the entire remainder of every large file.
   - **Shared disk cache write-lock starvation and missing cross-process scope adoption**: `SharedDiskCacheLayer.getChunkBuffer` (`weed/util/chunk_cache/shared_disk_cache_layer.go:185-237`) held `c.Lock()` across `os.ReadFile` (`readCachedFile`), blocking all concurrent cache reads/writes across the gateway during disk I/O, and never called `recordStateInScope(key, int64(len(data)), true)`, so chunks populated by another process (`weed filer` vs `weed s3`) were invisible to `-shareCopyChunks=true` (`HasChunkInScope` returned `false`, triggering redundant volume-server copies on S3 `UploadPartCopy`/`CopyObject`).
   - **Embedded S3 ignored `-wholeCacheCapacityMB`**: `weed filer -s3` (`weed/server/filer_server.go:130`, `weed/s3api/s3api_server.go:200`) initialized `s3a.chunkCache` with `NewTieredChunkCache(256, ..., s3a.option.CacheCapacityMB, ...)` without passing `WholeCacheCapacityMB` or sharing the filer's `SharedTieredChunkCache`. Additionally, `NewSharedTieredChunkCache` left `maxPartSize = 0` when `onDiskCacheSizeLimit01 == 0 && wholeCacheLimitBytes > 0`.

4. **Replica-load (`040ad1de8`) bugs and uncovered read paths**:
   - **Failed / zero-byte reads recorded as ultra-fast replicas**: `beginReplicaRead` (`weed/util/http/replica_load.go:73-84`) called `recordReadCost(host, time.Since(start), bytes)` whenever `ctx.Err() == nil`, ignoring `err != nil || bytes <= 0`. A replica returning HTTP 404/500 or early EOF in 0.5 ms with `bytes = 0` had `mib = math.Max(1, 0) = 1`, recording `0.5 ms/MiB` and making the broken replica look faster than healthy peers.
   - **Thundering herd onto unmeasured replicas while the first probe is in flight**: `replicaScore` (`weed/util/http/replica_load.go:87-97`) returned `replicaCostFloor * (1 + inflight)` (`1 ms * (1 + inflight)`) when `costNs == 0`. Against a healthy peer at `40 ms/MiB`, 39 concurrent reads piled onto an unmeasured slow replica before its first probe completed.
   - **Sub-MiB range reads understated per-MiB cost by up to 16×**: `recordReadCost` clamped `mib := math.Max(1, float64(bytes)/(1<<20))`, so a 64 KiB range read taking 10 ms recorded `10 ms/MiB` instead of `160 ms/MiB`.
   - **Uncovered read paths**: `retriedStreamFetchChunkData` (`weed/filer/filechunk_manifest.go:427-465`), `S3ApiServer.readChunkRange` (`weed/s3api/s3api_object_handlers.go:2202-2244`, which also only tried `urlStrings[0]` with no replica failover or `ReachableFirst` ordering), and `operation.LookupFileId` (`weed/operation/lookup.go:78`) bypassed `BeginReplicaRead` / `ReachableFirst`.

---

## Live `slow_proxy.py` Measurements (Before vs. After)

Setup: 1 volume server behind `slow_proxy.py` (`1.0 MB/s` per connection, `12.0 MB/s` aggregate), 1 master, 1 filer (`-maxMB=1`), 1 standalone S3 gateway (`-shareCopyChunks=true`), serving a **20 MiB object (`20,971,520` bytes across twenty 1 MiB chunks)**.

### 1. Default Session Budget (`-sessionReaderCacheSizeMB=64 -readerCacheSizeMB=512`, no disk cache)

| Workload | Object / Range Bytes | `b9d3b99cb` Proxy Bytes | `b9d3b99cb` Amplification | Fixed Proxy Bytes | Fixed Amplification |
|---|---:|---:|---:|---:|---:|
| `s3_whole_get_1` (20 MiB) | `20,971,520` | `20,977,480` | `1.000×` | `20,977,480` | `1.000×` |
| `s3_open_range_10m` (`bytes=10485760-`) | `10,485,760` | `10,751,233` | **`1.025×`** (+256 KiB) | `10,488,740` | **`1.000×`** |
| `s3_open_range_unaligned` (`bytes=10616832-`) | `10,354,688` | `10,358,787` | `1.000×` | `10,358,787` | `1.000×` |
| `s3_small_range_64k` (`64 KiB` @ 12 MiB) | `65,536` | `66,180` | `1.000×` | `66,180` | `1.000×` |
| `filer_whole_get` (20 MiB) | `20,971,520` | `20,977,480` | `1.000×` | `20,977,480` | `1.000×` |

### 2. Tight Budget (`4 × chunkSize` budget with `DefaultPrefetchCount = 4`, modeling 16 MiB chunks under `-sessionReaderCacheSizeMB=64` or contended `-readerCacheSizeMB=512`)

| Workload | Object / Range Bytes | `b9d3b99cb` Proxy Bytes | `b9d3b99cb` Amplification (Time) | Fixed Proxy Bytes | Fixed Amplification (Time) |
|---|---:|---:|---:|---:|---:|
| `s3_whole_get_1` (20 MiB) | `20,971,520` | **`83,909,920`** | **`4.001×` (`23.09s`)** | **`20,977,480`** | **`1.000×` (`6.30s`)** |
| `s3_open_range_10m` (`bytes=10485760-`) | `10,485,760` | **`31,728,713`** | **`3.026×` (`2.60s`)** | **`10,488,740`** | **`1.000×` (`0.83s`)** |
| `s3_open_range_unaligned` (`bytes=10616832-`) | `10,354,688` | **`41,825,007`** | **`4.039×` (`3.46s`)** | **`10,358,787`** | **`1.000×` (`2.11s`)** |
| `s3_small_range_64k` (`64 KiB` @ 12 MiB) | `65,536` | `66,180` | `1.000×` (`0.00s`) | `66,180` | `1.000×` (`0.00s`) |
| `filer_whole_get` (20 MiB) | `20,971,520` | **`167,819,840`** | **`8.002×` (`44.06s`)** | **`20,977,480`** | **`1.000×` (`6.30s`)** |

### 3. With Disk Cache (`-cacheDir=... -wholeCacheCapacityMB=51200 -cacheCapacityMB=2048`)

| Workload | `b9d3b99cb` Proxy Bytes | Fixed Proxy Bytes | Notes |
|---|---:|---:|---|
| `s3_whole_get_1` (cold 20 MiB) | `20,977,480` (`6.32s`) | `20,977,480` (`6.31s`) | Populates shared disk cache |
| `s3_whole_get_2_warm` (20 MiB) | `0` (`0.02s`) | `0` (`0.01s`) | 100% disk cache hit |
| `s3_open_range_10m` (warm) | `0` (`0.01s`) | `0` (`0.00s`) | 100% disk cache hit |
| `s3_open_range_unaligned` (warm) | `292` (`0.01s`) | `0` (`0.01s`) | 100% disk cache hit |
| `s3_small_range_64k` (warm) | `0` (`0.00s`) | `0` (`0.00s`) | 100% disk cache hit |
| `filer_whole_get` (cross-process warm) | `0` (`0.02s`) | `0` (`0.02s`) | Shared disk cache hit across `weed s3` & `weed filer` |

---

## Detailed Findings by Audit Area

### Area 1: Remaining Read Amplification on Every Read Path

#### 1.1 Budget Thrashing & Pinned-Chunk Eviction (`weed/filer/reader_cache_budget.go`, `weed/filer/reader_cache.go`)
- **Root Cause**:
  1. `doReadAt` (`weed/filer/reader_at.go:347-349`) calls `c.readerCache.MaybeCache(lastChunk.Next, c.prefetchCount)` on **every `ReadAt` slice** (while `readChunkSliceAt` lines 432-440 are dead code because line 288 passes `nextChunkViews = nil`).
  2. When `1 (pinned active chunk) + c.prefetchCount (4)` exceeds the session or shared budget (e.g., four 16 MiB chunks in `-sessionReaderCacheSizeMB=64`, or four 1 MiB chunks in a 4 MiB session budget), `MaybeCache` launched 4 prefetches (`chunk1..chunk4`) while `chunk0` was pinned (`pins == 1`).
  3. While `chunk1..chunk3` were still downloading (`b.reservations`, not yet in `b.idle`), `chunk4` entered `ReaderCacheBudget.reserveSelf`. Because `b.idle` contained only `chunk0` (`pins == 1`), `reserveSelf` selected `pinnedVictim = chunk0` and **evicted the pinned chunk currently being read**.
  4. Even when `chunk1..chunk3` had already completed and were in `b.idle`, `reserveSelf` scanned `b.idle` from `Front()` (FIFO), evicting **`chunk1` (the very next chunk the stream needed)** to make room for `chunk4`.
  5. On the next 256 KiB slice of `chunk0` (4 slices/chunk in S3 `streamRangeToClient`; 8 slices/chunk of 128 KiB in Filer HTTP), `readChunkAt` had to re-download `chunk0` (evicting `chunk1`), and then `doReadAt` called `MaybeCache` again, re-launching `chunk1` and evicting `chunk0` again—repeating on every slice (**4.00× on S3, 8.00× on Filer**).
- **Fix (`475c4a676`, `ce432b7c4`)**:
  - `ReaderCacheBudget.reserveSelf` never evicts a chunk with `pins > 0 || readers > 0`; it prefers evicting already-read unpinned chunks (`!c.isUnreadPrefetch()`) FIFO from `Front()`, and otherwise evicts the **newest (furthest-ahead)** unread prefetch LIFO from `Back()`.
  - `ReaderCache.MaybeCache` tracks `pinnedBytes + windowBytes` and stops launching prefetches once adding `chunkAlloc` would exceed `rc.budget.effectiveLimit()`.
  - `ReaderCache.pickOldestVictimLocked` prefers already-read unpinned chunks (oldest first), then unread prefetches (newest/furthest-ahead first), and never evicts a pinned chunk while downloads are in flight.
  - `doReadAt` finishes any `tasks[0]` currently pinned on `c.stream` before running subsequent tasks and pins the final task in `tasks` so multi-chunk `ReadAt` calls across chunk boundaries never leak or drop the stream pin.

#### 1.2 False `RandomMode` on Ranged GETs and Section Boundaries (`weed/filer/reader_at.go`, `weed/filer/filechunk_group.go`, `weed/filer/filechunk_section.go`)
- **Root Cause**:
  - `NewReaderPattern()` initializes `lastReadStopOffset = 0`. On an open-ended Range GET starting at `offset >= SeqTolerance` (`8 MiB`), the first `ReadAt` saw `offset - 0 >= 8 MiB` and switched `ReaderPattern` to `RandomMode` for `SeqThreshold` (4) slices. During those 4 slices (`4 × 64 KiB = 256 KiB` or `4 × 256 KiB`), `readChunkSliceAt` issued a range read for the first slice and then promoted the chunk to a whole-chunk download on the second slice (`1.025×` amplification).
  - `ChunkGroup` split files with `> 3200` chunks into `FileChunkSection`s, each creating a separate `ChunkReadAt` with a fresh `ReaderPattern` (`lastReadStopOffset = 0`), causing sequential reads to drop into `RandomMode` at every section boundary (`offset >= 8 MiB`).
- **Fix (`bd1dc846b`)**:
  - Added `hasRead bool` to `ChunkReadAt` so `c.readerPattern.MonitorReadAt` is seeded with the reader's first `ReadAt` offset (`lastReadStopOffset = offset`, `isSequentialCounter = 0`) rather than treating a non-zero starting offset as a random jump from `0`.
  - Shared `lastOffset` atomic tracking across `ChunkGroup` sections so sequential reads crossing a section boundary stay in `StreamMode`.

#### 1.3 Audit of Other Read Paths (S3 Multipart, SSE, `streamChunksPrefetched`, Copy, Mount)
- **S3 normal & Range GET (`streamFromVolumeServers` -> `streamRangeToClient`)**: Uses `ViewFromVisibleIntervals(visibleIntervals, offset, size)` and `ChunkReadAt` with `256 KiB` buffer. Clipped edge views (`!chunkView.IsFullChunk()`) take `fetchChunkRange` when `CanRangeFetch()` is true; full interior chunks use `ReaderCache`. With fixes `475c4a676`, `bd1dc846b`, and `ce432b7c4`, whole GET, aligned Range GET, unaligned Range GET, and small single-chunk Range GET all achieve **1.000×** amplification.
- **S3 SSE-C / SSE-KMS / SSE-S3 (`streamFromVolumeServersWithSSE`)**:
  - Ranged SSE GETs use `streamDecryptedRangeFromChunks` (`weed/s3api/s3api_object_handlers.go:1968`), which calls `ViewFromChunks` and fetches only the overlapping chunks (and for CTR block-aligned offsets, decrypts from the chunk start or IV offset).
  - Whole-object SSE GETs use `getEncryptedStreamFromVolumes` / `createMultipartSSE*DecryptedReaderDirect`, which stream each chunk sequentially once (`1.00×`).
- **`streamChunksPrefetched` (`weed/s3api/s3api_object_handlers.go:1808`)**: Only used by `getEncryptedStreamFromVolumes` for single-part SSE objects; bounds concurrency with `semaphore := make(chan struct{}, prefetchCount)` (`4`) and streams each chunk view once via `fetchChunkViewData`.
- **S3 `UploadPartCopy` / `CopyObject` (`weed/s3api/s3api_object_handlers_copy.go`)**: Uses `canShareSourceChunks` when `-shareCopyChunks=true` and `hasCaching` is active; otherwise streams through `ChunkReadAt`. Fixing `SharedDiskCacheLayer` scope adoption (`cb9b8f309`) ensures chunks cached on disk by Filer or S3 are recognized by `HasChunkInScope`.

---

### Area 2: Why the Disk Chunk Cache Didn't Help in Production

With production flags `-cacheDir=/var/cache/seaweedfs -wholeCacheCapacityMB=51200 -cacheCapacityMB=2048 -readerCacheSizeMB=512 -sessionReaderCacheSizeMB=64 -shareCopyChunks=true`, three bugs prevented the disk cache from helping:

1. **`shouldCache` Compared File Offset (`ViewOffset + ChunkSize`) Against `maxPartSize` (`32 MiB`) (`weed/filer/reader_at.go:414,424,454`, `weed/filer/reader_cache.go:239`)**:
   - With `-cacheCapacityMB=2048`, `NewSharedTieredChunkCache` creates 3 on-disk tiers with `unitSize = 2048 MiB / 8 = 256 MiB`:
     - Tier 0: `maxPartSize = 256 MiB / 64 = 4 MiB`
     - Tier 1: `maxPartSize = 256 MiB / 16 = 16 MiB`
     - Tier 2: `maxPartSize = 256 MiB / 8 = 32 MiB`
   - `GetMaxFilePartSizeInCache()` returns `32 MiB` (`33,554,432` bytes).
   - `reader_at.go` and `reader_cache.go` checked `(uint64(chunkView.ViewOffset) + chunkView.ChunkSize) <= GetMaxFilePartSizeInCache()`.
   - Because `chunkView.ViewOffset` is the **logical offset in the file**, every chunk at file offset `> 32 MiB - chunkSize` had `shouldCache == false`!
   - When `shouldCache == false`, `ReaderCache.readChunkAt` (`weed/filer/reader_cache.go:338`) **skipped `root.chunkCache.ReadChunkAt` on read** AND `SingleChunkCacher.startCaching` (`weed/filer/reader_cache.go:714`) **skipped `s.parent.chunkCache.SetChunk` on download completion**. Thus, for any multi-hundred-MiB or multi-GiB object, the disk cache was completely bypassed after the first 32 MiB.

2. **`SharedDiskCacheLayer` Held Write Lock Across Disk I/O and Never Adopted Cross-Process Hits Into Scope (`weed/util/chunk_cache/shared_disk_cache_layer.go:185-237`, `weed/util/chunk_cache/chunk_cache.go:228-246`)**:
   - `SharedDiskCacheLayer.getChunkBuffer` acquired `c.Lock()` (an exclusive mutex for the entire 50 GiB whole-cache tier) and held it while performing `os.ReadFile` (`readCachedFile`) and `os.Chtimes`. Under concurrent gateway load, every disk read serialized all other readers and writers on that cache tier.
   - When a process discovered an existing cached file on disk via `adoptOrRemoveMissingLocked` (written by another gateway or before restart), `SharedDiskCacheLayer` never called `recordStateInScope`, so `HasChunkInScope` returned `false` and `-shareCopyChunks=true` failed to share chunks across processes.
   - When `cacheCapacityMB == 0` and `wholeCacheCapacityMB > 0`, `NewSharedTieredChunkCache` left `c.maxPartSize = 0`, causing `GetMaxFilePartSizeInCache()` to return `0` and disabling all caching.

3. **Embedded S3 (`weed filer -s3`) Ignored `-wholeCacheCapacityMB` (`weed/server/filer_server.go:130`, `weed/s3api/s3api_server.go:200`)**:
   - `FilerServer` created its `SharedTieredChunkCache` with `fo.WholeCacheCapacityMB`, but did not pass `fc` or `WholeCacheCapacityMB` to `S3ApiServerOptions`, so embedded `S3ApiServer` created a second, uncoordinated `TieredChunkCache` with `wholeCacheCapacityMB = 0`.
   - `FilerServer.StreamWriteW` (`weed/server/filer_server_handlers_read.go:240`) and `WebDavServer` (`weed/server/webdav_server.go:254`) also called `NewChunkReaderAtFromClient` directly on the root `readerCache` without creating a per-request scope (`NewRequestScope`).

---

### Area 3: Audit of `040ad1de8` (Replica Load & Reachability)

1. **Failed or Zero-Byte Reads Recorded as Ultra-Fast Replicas (`weed/util/http/replica_load.go:73-84`)**:
   - `beginReplicaRead` recorded read cost whenever `ctx.Err() == nil`, even if `err != nil` or `bytes == 0`.
   - When a replica returned HTTP 404/500 (which `RetriedFetchChunkData` marks *reachable* because the TCP/HTTP server answered!) or an immediate connection reset/EOF in `0.5 ms` with `bytes = 0`, `recordReadCost` computed `mib = math.Max(1, 0) = 1` and recorded `0.5 ms/MiB`. That poisoned the EWMA toward zero so `ReachableFirst` continued preferring the failing replica on subsequent chunks.
   - **Fix (`07439688c`)**: `beginReplicaRead` only records cost when `ctx.Err() == nil && err == nil && bytes > 0`.

2. **Thundering Herd on Unmeasured Replicas (`weed/util/http/replica_load.go:87-97`)**:
   - An unmeasured replica (`!ok` or `l.at.IsZero()`) returned `replicaCostFloor * (1 + inflight)` = `1 ms * (1 + inflight)`.
   - When a replica had not yet completed its first read (or after restart), its score remained `< 40 ms` until **40 concurrent reads** were simultaneously in flight on it—herding tens of concurrent chunk reads onto a potentially slow replica before its first measurement returned.
   - **Fix (`07439688c`)**: When `unmeasured` (`l.at.IsZero()`), `replicaScore` returns `replicaCostFloor` (`1 ms`) when `inflight == 0` so a single probe is sent first, and `replicaUnmeasuredInflightCost * float64(inflight)` (`100 ms * inflight`) while `inflight > 0` so concurrent reads prefer known-healthy peers until the probe finishes.

3. **Sub-MiB Range Reads Understated Cost (`weed/util/http/replica_load.go:49`)**:
   - `mib := math.Max(1, float64(bytes)/(1<<20))` clamped all reads `< 1 MiB` to `1 MiB`. A 64 KiB range read taking `10 ms` was scored as `10 ms/MiB` instead of `160 ms/MiB`.
   - **Fix (`07439688c`)**: Lowered the `mib` floor to `1.0 / 16.0` (`64 KiB`).

4. **Read Paths Missing Replica Load / `ReachableFirst`**:
   - `retriedStreamFetchChunkData` (`weed/filer/filechunk_manifest.go:427-465`): Used `ReachableFirst(urlStrings)` but did not wrap `ReadUrlAsStream` with `BeginReplicaRead`.
   - `S3ApiServer.readChunkRange` (`weed/s3api/s3api_object_handlers.go:2202-2244`): Only tried `urlStrings[0]` with no retry loop across replicas, no `ReachableFirst`, no `RecordUnreachable`/`RecordReachable`, and no `BeginReplicaRead`.
   - `operation.LookupFileId` (`weed/operation/lookup.go:78`): Picked `localUrls[rand.IntN(len(localUrls))]` without `ReachableFirst`.
   - **Fix (`07439688c`)**: Wired `BeginReplicaRead` and `ReachableFirst` into `retriedStreamFetchChunkData`, `S3ApiServer.readChunkRange` (with full replica failover and body-close cost recording via `replicaLoadReadCloser`), and `operation.LookupFileId`.

---

### Area 4: Other Gateway GET Path Bottlenecks Under Load

1. **`SharedDiskCacheLayer` Mutex Contention Across Disk Reads (`weed/util/chunk_cache/shared_disk_cache_layer.go:185-237`)**:
   - Holding `c.Lock()` across `os.ReadFile` serialized all cache lookups and writes on the 50 GiB whole-chunk cache tier behind single-threaded disk reads. Fixed in `cb9b8f309` by releasing `c.Lock()` before `readCachedFile(fileName)` and re-acquiring it only to update metadata.
2. **Unscoped `ReaderCache` on Filer HTTP and WebDAV Reads (`weed/server/filer_server_handlers_read.go:240`, `weed/server/webdav_server.go:254`)**:
   - Unlike S3 (`streamFromVolumeServers`), Filer HTTP and WebDAV passed the root `ReaderCache` directly to `NewChunkReaderAtFromClient` without `NewRequestScope`, allowing a single Filer/WebDAV reader to consume the entire server-wide `ReaderCache` budget. Fixed in `cb9b8f309`.

---

## Commits on `review-b9d3b99cb-agy`

1. `475c4a676` — **`filer: prevent budget and prefetch eviction of active and next chunks`**
   - Tests: `TestSlowStreamKeepsItsChunkWhenOtherSlotsAreInFlight`, `TestPrefetchWindowFitsBudgetAndDoesNotEvictNextChunk` (`weed/filer/reader_cache_slow_stream_test.go`)
2. `bd1dc846b` — **`filer: preserve sequential ReaderPattern on ranged GETs and across section boundaries`**
   - Tests: `TestRangedChunkReadAtStartingPastSeqToleranceStaysSequential`, `TestChunkGroupSequentialAcrossSectionBoundaryStaysSequential` (`weed/filer/reader_at_shared_test.go`)
3. `cb9b8f309` — **`cache: fix shouldCache offset check, shared disk cache adoption and locking, and whole-cache scoping`**
   - Tests: `TestShouldCacheChecksChunkSizeNotFileOffset` (`weed/filer/reader_at_shared_test.go`), `TestSharedTieredChunkCacheAdoptsCrossProcessChunkIntoScope`, `TestSharedTieredChunkCacheWholeLimitOnlyHasNonZeroMaxPartSize` (`weed/util/chunk_cache/shared_disk_cache_test.go`)
4. `07439688c` — **`http: ignore failed reads in replica load, prevent unmeasured herding, and cover stream/range paths`**
   - Tests: `TestFailedReplicaReadDoesNotRecordFastCost`, `TestUnmeasuredReplicaWithProbeInFlightDoesNotHerd` (`weed/util/http/replica_load_test.go`)
5. `ce432b7c4` — **`filer: preserve stream pin across multi-chunk ReadAt boundaries`**
   - Test: `TestUnalignedRangeStreamDoesNotDropStreamPinOrEvictNextChunk` (`weed/filer/reader_cache_slow_stream_test.go`)
