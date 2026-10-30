# Read-path review — 2026-10-29

Reviewed `040ad1de8` and `b9d3b99cb`, starting at `b9d3b99cb`, on
`review-b9d3b99cb-codex`. No push, deployment, or changes to the integration
branch or other worktrees. Measurements below use a new, temporary local
cluster, not the production cluster.

## Findings and disposition

### 1. Prefetch can still evict the chunk being consumed (confirmed, fixed for speculation)

`ReaderCacheBudget.reserveSelf` treats every completed downloader as idle,
including chunks pinned by a stream. When no unpinned victim exists it removes
a pinned one. The window fix bounds the number of speculative chunks, but not
their pooled allocation size relative to either budget. Even one stream with
a budget holding one chunk can lose its current chunk to prefetch and fetch it
again on its next small read.

`TestPrefetchDoesNotEvictCurrentChunkForBudget` failed on the original tip for
both root and scoped budgets: speculation fetched `next` and removed `current`.
The fix marks speculative downloaders and declines their reservation when only
pinned completed chunks can make room. It still waits for in-flight buffers
when there are no completed victims, preserving the existing bounded-memory
behavior. A demand reader that joined a declined prefetch retries as a demand
read; the internal admission error must not become an HTTP error. Parent
reservation failure releases the child's reservation through the existing
rollback path. The regression and existing budget, slow-stream, and window
tests pass, including targeted race testing.

**Residual high-priority issue:** demand reads still evict pinned chunks under
both memory and slot pressure. `pickOldestVictimLocked` explicitly falls back
to a pinned victim; `TestChunkReadAtPinnedEvictionFallsBackWhenAllPinned`
asserts that behavior. Budget fallback has the same policy. A stream's pin is
not a lifetime guarantee between reads. Enough concurrent active chunks can
therefore still thrash even after these fixes. Removing the fallback outright
would introduce hangs for abandoned pins and for streams moving between chunks
without enough space for both. A complete fix needs cancellable admission and
stream ownership/position handling, or a demand range-read fallback, while
preserving a hard allocation bound. This review does not claim that solved.

An unpinned chunk about to be read can also be evicted. In-flight chunks are
excluded from slot eviction, but completed future prefetched chunks are not
protected. Slot limits are not strict admission limits: demand insertion can
exceed them if every downloader is unfinished and no victim exists. Memory
budgets provide the stronger bound. Background prefetch is deliberately not
canceled with an individual request; canceled/short reads can leave bounded
speculative traffic and retained data until eviction.

### 2. The stated disk-cache explanation is not reproducible from these flags

For standalone S3, `s3api_server.go` constructs
`NewSharedTieredChunkCache(..., "s3", 2048, 51200, 1<<20)` under
`/var/cache/seaweedfs/chunks`. This is a **2 GiB S3 scope inside a 50 GiB whole
cache**, not a 50 GiB S3 scope. `GetMaxFilePartSizeInCache` is scope capacity
divided by four: **512 MiB**. `shouldCache` compares the chunk's logical file
position plus chunk size to this cutoff, not just the chunk's own size.

Consequences confirmed by `TestReviewProductionDiskCacheAdmission`, using the
real shared disk cache and valid chunk IDs:

* A 4 MiB chunk at file offset 490 MiB survives removal of the ReaderCache
  downloader and serves a second reader without another backend fetch.
* A 4 MiB chunk at offset 513 MiB is downloaded twice. Increasing only
  `wholeCacheCapacityMB` does not change that admission decision.
* An in-memory-only ChunkCache has an even smaller 8 MiB prefix cutoff.
* Plain clipped ranges read existing persistent data but do not populate a
  whole-chunk disk entry. SSE paths discussed below bypass this ReaderCache.

A 495 MB **or 495 MiB** ordinary object with the described chunks is below
512 MiB. Its repeated whole-chunk reads should have been absorbed, absent
write failures or concurrent eviction. The local disk-enabled harness does
absorb them. Commit `6116664e` concerns embedded S3 scope wiring and filer
request budgets; it does not disable the standalone S3 scope.

Thus the precise production disk-cache failure remains **unconfirmed**. It
would be misleading to attribute all 13,782 requests to this cutoff. Relevant
remaining evidence is the running binary and effective startup cache log,
object SSE/chunk metadata and logical offsets, cache directory permissions and
write failures, and competing scope/whole-cache eviction. Shared disk write
failures are not returned to the caller; mkdir/create failures log at V(1),
and short writes/rename failures simply abandon the cache entry. None of these
production conditions was inspected or assumed here.

### 3. Fast failures incorrectly advertised a fast replica (confirmed, fixed)

`beginReplicaRead` accepted `err` but ignored it. A fast 404/500 or short body
could produce a floor-level cost and be tried ahead of a successful replica on
every chunk. Reachability does not solve HTTP errors: a server that returned
an HTTP response is reachable.

`TestReplicaFailureDoesNotLookFasterThanSuccess` failed first: the zero-byte
failed host sorted before the 50 ms successful host. The fix records errors
with at least one second of cost and without dividing by partial payload
size. This is a heuristic penalty, not a claim about measured throughput.
It fades normally. A chunk-specific error can consequently penalize other
volumes on the same host; that is a tradeoff of host-wide health scoring.
Caller cancellation continues to decrement inflight without adding a sample,
verified by a separate test. An older retry test now clears load history
between its direct and streaming cases so both deliberately exercise failure
and refresh, rather than allowing the first case to teach the selector to
avoid the failed host in the second.

### 4. Replica-selection coverage and remaining limitations

* Ordinary S3 `ChunkReadAt`, mount ChunkGroup reads, and configured filer
  ReaderCache reads use `RetriedFetchChunkData`: selection and accounting cover
  direct whole chunks and ranged/decrypting buffered fetches. Direct partial
  retries account for the new bytes only and resume from the previous offset.
* `filechunk_manifest.go:retriedStreamFetchChunkData` calls `ReachableFirst`
  but **does not record load or inflight**. This covers manifests and the
  synchronous pipe-streaming route, including the no-cache filer route and
  single-part SSE encrypted streams. In a process serving only those paths,
  healthy hosts remain unmeasured and selection effectively retains lookup
  order. Adding elapsed time around the consumer callback would incorrectly
  charge downstream backpressure to the volume server; instrumentation should
  distinguish network service from consumer blocking.
* Multipart SSE-C/SSE-S3/SSE-KMS lazily open per-chunk readers through
  `fetchChunkViewData` / `readChunkRange`. For ordinary raw ciphertext chunks,
  `readChunkRange` selects `urlStrings[0]`, has no alternate-replica retry, and
  bypasses both load accounting and the disk ReaderCache. Chunk-level filer
  compression/ciphering takes a separate buffered RetriedFetchChunkData path.
* Copy streaming (`s3api_object_handlers_copy.go:streamCopyChunkRange`) uses
  `ReadUrlAsStream` directly. Whole-object metadata-sharing copy can avoid
  data transfer with `shareCopyChunks`, but that flag does not change GETs.
* `wdclient` supplies shuffled location lists with DC/local preferences; it
  does not perform the body read or record its cost. It should not claim an
  unreachable-host probe at lookup time: some callers do not attempt a read.
  The later load sort can override those DC/local preferences when scores
  differ. The volume server's own proxy/redirect read chooses a shuffled
  remote location and does not use this load model.
* The mutex protects timing fields, atomic counters protect inflight, and
  sorting uses a score snapshot. Targeted race tests passed. Selection and
  increment are separate, however, so concurrent callers can choose the same
  apparently idle host before any starts. It is load-aware ordering, not
  atomic scheduling or hedged racing of replicas.
* Unmeasured hosts start at 1 ms. A cold host can attract many requests before
  a slow first result arrives. Inflight weighting reduces but does not remove
  this burst. Large reads are normalized per MiB; sub-MiB reads are charged
  as one MiB, so range-size and latency mix affect ordering.
* A never-retried slow host's score still decays at selection time, as the
  existing fade test verifies. All scores eventually hit the floor and then
  lookup shuffle breaks ties. The retry time is not a fixed 30 seconds:
  roughly `30s * log2(oldCost / peerCost)` is needed to become competitive.
  Active peers also have their EWMA decayed before each new sample.
* Reachability probe precedence remains intact: an expired unreachable host
  claimed by CAS precedes the load-sorted reachable group. The existing
  one-host-per-probe tests pass. Cancellation before the attempt can still
  consume a probe claim until its next interval. Both host maps are
  process-wide; load entries have no eviction, and host:port aliases are
  distinct even when they share one NIC.

## Read-path coverage

| Path | Verification and limits |
| --- | --- |
| Ordinary S3 whole, open-ended Range, small Range | Real local volume behind supplied paced proxy; byte counts below. Shared ChunkReadAt regression tests cover small reads and concurrent streams. |
| Plain multipart | Same visible chunk intervals / ReaderCache as ordinary GET; live measurement below. |
| SSE-C and SSE-S3 single/multipart | Live proxy measurements below; existing decryption, IV, multipart ordering/lazy-fetch and stream-status tests in the full S3 suite. |
| SSE-KMS single/multipart | Code tracing plus existing S3 KMS/decryption/multipart tests; no live KMS-backed proxy run. Not claimed as production KMS validation. |
| `streamChunksPrefetched` | Actual no-cache filer GET plus existing in-order, range, cancellation and parallel-download tests. Synchronous pipes bound memory, but their prefetch primarily opens connections; a blocked pipe cannot buffer a whole future chunk. |
| Filer HTTP with cache, embedded S3 | Same ReaderCache primitives; scope wiring traced through `command/filer.go`, `embedded_s3_cache_scope.go` and `filer_server_handlers_read.go`. No separate live embedded gateway run. |
| Mount and ChunkReadAt | Full mount package tests, shared-stream/slow-stream/window/range/disk tests and targeted races; no live FUSE mount was performed. Mount retains completed buffers for revisits. Random first touches range-fetch plain chunks, then promote reuse; finite-cache eviction can still cause refetches. |
| Copy | Existing copy-stream tests and code tracing. Metadata-sharing copy avoids source bytes only when eligible. No live CopyObject byte-count run. |

Plain clipped ranges issue only requested byte ranges; compressed or filer-
ciphered clipped views need a whole blob and share it if it fits. Oversized
compressed partial views fall back to repeated range fetches that can still
require whole-blob work at the server. Full chunks too large for the budget
are not universally rescued by this partial-view fallback. These are residual
limitations, not eliminated by the window fix. Lazy multipart SSE avoids eager
whole-object buffering, but an encrypted small Range may still process more
ciphertext than it returns, especially when it skips through a multipart
stream. Existing correctness tests are not an amplification proof for every
SSE range configuration.

## Other throughput constraints

The default lookahead is four chunks, S3 request scope is 32 slots, root is
256 slots, and the supplied budgets are 64 MiB/request and 512 MiB/process.
Pooled allocation size, rather than just payload size, controls fit. Four
2–4 MiB chunks cover only 8–16 MiB of lookahead; this is bounded but may not
fill a high-bandwidth/high-latency link. No prefetch tuning is justified from
this one paced localhost experiment.

S3's copy scratch buffer here is 256 KiB (smaller for small responses), filer
cached HTTP uses 128 KiB, and pipe streaming uses 256 KiB. The regression also
uses 32 KiB callers as in the incident. Increasing buffer size only reduces
the symptom when eviction is wrong. S3's separate raw volume client has ten
**idle** connections per host, not a ten-active-request limit; it has a
five-minute timeout. The common HTTP client's idle limit is 1024 per host.

Circuit-breaker limits depend on the loaded config; the default is disabled.
GET count limits can reject concurrent requests. Its byte counter uses the
request's ContentLength, not downloaded object size, so GET byte limits do not
bound response buffers or volume traffic. Upload admission limits do not
throttle ordinary GETs. No production breaker config was read.

## Measurements and reproducibility

`review_harness.py` uses the supplied `slow_proxy.py`, 1 MB/s per connection
and 12 MB/s total. It starts its own master, filer, volume and standalone S3
under a fresh `/tmp/seaweed-review-*` directory, checks ports before starting,
and stops its children. Volume HTTP binds 127.0.0.2 and advertises 127.0.0.1;
a separate high-rate proxy forwards volume gRPC. Iceberg/Lance listeners are
disabled. Initial attempts encountered occupied default ports and were
abandoned without changing the existing services. All successful measurements
use ports 22421–22424 and corresponding gRPC ports.

The plain fixture is 16 MiB of random bytes uploaded as eight 2 MiB chunks.
Proxy counts are **server-to-client TCP payload bytes including HTTP headers**,
not just object bodies. Counts are differences between settled five-second
reports. The harness paces each connection against its lifetime average, so
idle connections accumulate credit: elapsed times after the first read are
not a strict link-bandwidth benchmark. Byte amplification is the reliable
measurement. There are no failure/retry injections in these runs.

Original tip, no disk cache, 512/64 MiB reader budgets:

| Request | Client bytes | Proxy bytes | Seconds |
| --- | ---: | ---: | ---: |
| S3 whole | 16,777,216 | 16,779,880 | 6.307 |
| S3 `bytes=1048576-` | 15,728,640 | 15,732,535 | 2.899 |
| S3 `bytes=1024-33791` | 32,768 | 33,153 | 0.002 |
| Filer whole, no ReaderCache | 16,777,216 | 16,779,880 | 4.247 |

Whole-object amplification is 1.00016× including headers. This corroborates
the window fix under these conditions, not the historical 36.9× pre-fix
measurement (which was supplied in the prompt and not rerun here).

Fixed build with production disk flags (`review_harness.py /tmp/seaweed-review-fixed 64 disk sse`):

| Request | Client bytes | Proxy bytes | Seconds |
| --- | ---: | ---: | ---: |
| Cold S3 whole | 16,777,216 | 16,779,880 | 6.317 |
| Warm S3 open Range | 15,728,640 | 0 | 0.022 |
| Warm S3 small Range | 32,768 | 0 | 0.001 |
| Warm S3 whole | 16,777,216 | 0 | 0.019 |
| Filer whole (separate, no ReaderCache) | 16,777,216 | 16,780,172 | 4.205 |
| Plain single-part | 8,388,608 | 8,389,186 | 8.406 |
| Plain multipart | 8,388,608 | 8,389,475 | 3.464 |
| SSE-C single-part | 8,388,608 | 8,389,478 | 8.392 |
| SSE-C multipart | 8,388,608 | 8,389,475 | 0.656 |

The first SSE-S3 upload failed because the temporary gateway had no KEK;
that is a setup failure, not a GET result. A fresh local run with a test-only
`WEED_S3_SSE_KEY` (`64 sseonly`, no disk) measured:

| Request | Client bytes | Proxy bytes | Seconds |
| --- | ---: | ---: | ---: |
| SSE-S3 single-part | 8,388,608 | 8,389,186 | 8.396 |
| SSE-S3 multipart | 8,388,608 | 8,389,475 | 0.653 |

The SSE-S3 run checked the complete decrypted payload against its upload.
Multipart fixtures contain 5 MiB and 3 MiB parts. The runner now sets the
isolated test key for all requested SSE runs. These are full GET measurements;
SSE small/open Range amplification was not measured with the proxy.

## Validation and commits

* `4a4772c79` — `filer: do not evict pinned readers for speculative prefetch`.
  Red on original tip in both budget scopes; green after the fix.
* `d914e6ed0` — `http: penalize failed replica reads instead of rewarding fast errors`.
  Red fast-error ordering test on original code; green after the fix.
* This report commit adds the disk admission test and reproducible local
  harness, plus a ten-iteration race regression for a demand reader joining
  a prefetch that is declined. The supplied prompt and proxy are left unchanged.

The literal `go build -tags 5BytesOffset ./weed` fails because its default
output name `weed` is an existing directory. The equivalent
`go build -tags 5BytesOffset -o /tmp/seaweed-review-fixed ./weed` succeeds.

The original full requested test command passed (S3 package: 342.049 seconds).
An intermediate 90-second timeout was too short for the full S3 suite; it is
not recorded as a passing run. During development the existing in-flight
budget test caught an overly broad speculative skip; the final fix preserves
waiting when all reservations are still downloading. The filer suite then
passed. The final full requested command also passed: filer 6.320s,
S3 API 338.341s, HTTP 4.600s, mount 8.319s, plus their subpackages.
The demand-join test was added afterward and separately passed ten race-enabled
iterations; production code did not change after the full-suite run.

Targeted `go test -race` on HTTP selection and filer shared-stream, window,
budget-pressure, in-flight-budget and disk-admission cases passed (HTTP 1.049s,
filer 4.824s). This is targeted race coverage, not a claim that every gateway
path is race-free. The demand-joining-declined-prefetch regression also passed
ten iterations under `-race` (1.085s).
