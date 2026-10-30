# SeaweedFS 4.51 merge audit

Audited `2dc6a53c6` (parents `b9d3b99cb`, `153ab668c`) and `d5f405c5d` (parents `2dc6a53c6`, `3f954e34d`) in `audit-451-codex`, 2026-10-29/30. Instructions: `~/Documents/review-harness-20261029/AUDIT_MERGE_451.md`. Changes are local commits only; no push, deployment, or access to an existing cluster. Integration tests started disposable loopback services and mounts, then cleaned them up.

## Result and fixes

**One confirmed product merge regression, one integration-fixture regression caused by an upstream contract change, and one pre-existing fork fixture incompatibility were fixed in `593d7a8ff`.** The principal Linux storage, filer, mount, upload, S3, topology, and shell suites pass. This is not a native Windows runtime or VM crash qualification. Known offset/EC test failures remain; details below.

1. **Windows mount no longer compiled (high).** Upstream changed `maybeReadEntry` to accept a context. The cleanly merged, Windows-only `weed/mount/winfsp_metadata_windows.go` retained two one-argument calls, in `updateWindowsMetadata` and `getattrWindowsFlags`. Linux compilation cannot detect these. Fixed both to supply `context.Background()`, consistent with callbacks that have no request cancellation channel; `lookupEntry` still installs its filer deadline. Regression check: cross-compile the existing Windows mount test suite. RED: both calls reported `have (uint64); want (context.Context, uint64)`. GREEN: the identical test-build command exits 0, as does the full Windows executable build. No claim that the resulting Windows tests ran on Linux.
2. **Stalled-volume FUSE gate could not reach its workload (medium).** The new upstream single-master `mini` fast-start mode pins its embedded volume reporter. The fork test adds two volume servers, which were rejected; `grow 8 volumes` timed out. Fixed the fixture to specify normal master membership with its own loopback address in `-master.peers`. This retains upstream's admission safety and tests the intended multi-server scenario. RED: `TestCloseSucceedsWhenAnAssignedVolumeServerStalls` failed after 55.49s, with `this master admits a single volume-server reporter`. GREEN: 64 MiB written, close succeeded in **23.015s**, 18 stalled chunk attempts and 18 successful retries; full test 71.17s.
3. **Frozen-backend FUSE gate supplied incompatible deadlines (medium, already present at the fork parent).** The fixture set `-fuse.requestTimeout=3s` but inherited the newer 25s metadata deadline, so the mount rejected its configuration. Fixed the fixture to explicitly set both deadlines to 3s. RED: `-fuse.metadataRequestTimeout 25s must not exceed -fuse.requestTimeout 3s`. GREEN: lookup 3.001s, mkdir 3.001s, read 6.002s (two requests), write/close 3.012s, opening for fsync 3.001s, killed writer exit 3.002s. All remain inside the test's 7s syscall bound. The test does not establish that fsync itself ran when the open failed.

Each correction was made only after recording the failing existing regression check, then rerunning that check successfully. The fixtures retain their original outcome assertions and time bounds.

## Method and coverage

Reconstructed the merges with `git show --remerge-diff`: exactly **36** conflicted paths in merge 1 and **11** in merge 2. The remerge diff also exposes edits made by the merger outside conflict blocks, including the tail-recovery policy and test adaptations. Compared parent-side changes and the resolved code, followed changed contracts into fork callers, and inspected the final fork-specific differences. Parent comparisons were captured with `git diff MERGE^1 MERGE -- PATH` and `git diff MERGE^2 MERGE -- PATH`; parent blobs remain available with `git show MERGE^N:PATH`. The matrices below record the resolution assessment for every conflict.

The fork inventory is `git log --no-merges 4.48..2dc6a53c6^1 --not 3f954e34d` (**371 commits**, also 371 when excluding `4.51`). Appendix A records every commit, touched-path comparison, and added Go test names. **238 commits have all touched paths byte-identical between the fork parent and merged tip; 133 touch at least one changed path.** This compares the net fork baseline, not a claim that every historical patch remains verbatim: some changes were superseded before the merge. Unchanged files can still have broken callers, as the Windows finding demonstrates.

Across the fork-touched Go test files, the only test name present at the fork parent and absent at the merged tip is `TestGcsCredentialsArePath`. Upstream replaced unconditional path rejection with validated credential-file support and tests for valid, invalid, nonexistent, and changed files. The fork's public-DNS-independent stubbing remains. Historical test names already removed or renamed before the fork parent are distinguished in Appendix A. Name retention alone is not a behavioral proof; the package runs and targeted checks below supply the executable evidence.

Go commands used `-p 4`; race tests additionally used `GOMAXPROCS=4`. The checked-in `go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with `../forks-go-fuse`; this audit used that existing dependency without modifying it. Thus the real FUSE results apply to that dependency configuration.

## Conflict matrix: merge 1

Paths below are relative to the repository root. “Retained” means no loss identified in the reviewed resolution, subject to the runtime coverage limits below.

| Conflicted path | Resolution assessment / evidence |
| --- | --- |
| `.github/workflows/container_release_unified.yml` | Fork repository guard retained; upstream Ubuntu 24.04 runner adopted. Publishing workflow not run. |
| `.gitignore` | Union of Windows/artifact exclusions and upstream Python bytecode exclusion. |
| `README.md` | Fork documentation retained, headline base advanced to 4.51. Intentional replacement of upstream marketing text, not executable behavior. |
| `weed/command/filer.go` | Upstream nonempty-bucket deletion default false; fork shared-copy option and cache settings retained. |
| `weed/command/s3.go` | Same default change; opt-in empty-directory marker remains registered. Standalone cache flags exercised in paced GETs. |
| `weed/command/server.go` | Same default change with fork shared-copy configuration retained. |
| `weed/filer/filer_delete_entry.go` | Both hard-link orphan cleanup and separate shared-chunk ownership branch retained. Shared-chunk and deletion suites pass. |
| `weed/filer/filer_notify.go` | Pre-commit event stamps combined with directory change tracking. Reserved overwrite-deletion stamps released on rollback/suppression paths and published before rename events. Later merge uses stream positions for writer tracking. |
| `weed/filer/reader_at_prefetch_window_test.go` | Both independent prefetch-window regressions retained, not one test selected over the other; both pass, including race run. |
| `weed/filer/reader_cache.go` | Upstream loop is equivalent to fork fix: in-flight/already cached chunks consume window positions. Fork shared scopes, pins, and budget logic remain. |
| `weed/mount/weedfs.go` | Request cancellation threaded into lookup's deadline; detached rebuild keeps cache warming. Currency checks and deferred-create fallback retained. Windows caller drift outside this file fixed in this audit. |
| `weed/mount/weedfs_access.go` | Kernel-lock notification gate retained around upstream request context. |
| `weed/mount/weedfs_dir_read.go` | Lock gate retained, request context propagated through directory reads. Later upstream cancellation maps to EINTR. Metadata-only snapshots still omit chunk lists. |
| `weed/operation/upload_chunked.go` | Stall avoidance retained alongside refreshed cleanup JWT support; exported cleanup helper may pass nil options, and nil guard is present. |
| `weed/operation/upload_chunked_test.go` | Atomic counter implementations are equivalent; typed atomic chosen. Failure assertions remain. |
| `weed/operation/upload_content.go` | Streamed/detachable body retained, progress propagated through cipher/plain branches. Fast-host watchdog only arms without ProgressWatch; response-header timeout removed for watched client. Existing slow-progress, stalled-server, body-lifetime and watchdog tests pass. |
| `weed/pb/grpc_client_server.go` | Upstream socket-listener helper adopted; fork unregister-on-bind-failure fallback retained. PB package tests pass. |
| `weed/pb/master_pb/master.pb.go` | Generated descriptor retains fork `avoid_servers` field 100 and upstream growth fields. Build/PB tests and real stalled assignment/retry exercise serialization. |
| `weed/s3api/s3api_head_bucket_test.go` | Both parents' intended bodyless HEAD behavior agrees; status assertions and empty-body assertion retained. |
| `weed/s3api/s3api_object_handlers_copy.go` | Request context, shared-copy ownership and cancel/join concurrency retained; upstream lock headers and part MD5 added. Partial-copy digest reset per retry; new checksum reader derives from caller context. Copy/ETag/read-only/reassignment suites pass. |
| `weed/s3api/s3api_object_handlers_copy_stream.go` | Hash tee wraps fork progress-tracked source, preserving progress and content length; gzip passthrough avoids hashing compressed bytes as plain data. |
| `weed/s3api/s3api_object_handlers_list.go` | Upstream ordered/paged listing retained. Opt-in empty marker reinserted at recursive explicit-prefix probe; clone prevents mutating source metadata. Default remains no synthetic key. |
| `weed/s3api/s3api_object_handlers_list_directory_test.go` | Separate tests cover default invisibility and opt-in marker; neither expectation discarded. |
| `weed/s3api/s3api_object_handlers_put.go` | Both stalled-server assignment and cleanup JWT lookup supplied to chunk uploader. |
| `weed/s3api/s3api_object_handlers_readonly_test.go` | Context and extra MD5 result added to call; access-denied assertion unchanged. |
| `weed/s3api/s3err/error_handler.go` | Upstream body-permitted helper is equivalent to fork condition; HEAD/1xx/204/304 body suppression retained. |
| `weed/server/filer_grpc_server_rename.go` | No replies before commit. Remote copies finished after commit, shared target chunks protected; publish all events before sending acknowledgments. Overwrite delete reserves stamp before new entry. Failure releases queued stamps. Rename acknowledgment/rollback suites pass. |
| `weed/server/master_grpc_server_volume.go` | Fork explicit-vacuum wait and error propagation combined with rack quota; periodic growth uses fork GrowthStep with upstream growth calculation. |
| `weed/server/master_server_handlers_admin.go` | HTTP cancellation passed to VacuumWithContext and returned as timeout; rack quota retained. |
| `weed/shell/command_volume_fsck.go` | Upstream purge workers/hard-link serialization plus fork purge errors, repair files, callback errors, one traversal and orphan-percent gate. Read/write record formats both include hard-link length. Later merge deliberately widens walk to root. |
| `weed/storage/backend/volume_create_linux.go` | Failed reservation still closes/removes and fails creation; KEEP_SIZE preserved using named unix constant. Backend tests pass. |
| `weed/storage/volume_write.go` | Upstream syncIndex supersedes fork nm.Sync. Data synced before publication, index synced afterward and before acknowledgment; failed index sync makes volume read-only without truncating published data. Batch path similarly fails acknowledgments. Fork staged-replacement file list and absent-only repair helper retained. |
| `weed/topology/store_replicate.go` | Upstream parallel local/replica writes adopted. Fork one-attempt progress/response limits moved into new replica branch; unused pooled payload allocation remains removed. |
| `weed/topology/topology_vacuum.go` | Fork manual-wait semantics and quota wakeup survive with upstream per-rack limit. Scheduler/concurrency tests pass. |
| `weed/topology/volume_layout.go` | Avoid-server candidates filtered before max-writable cap. Constrained-location path gathers matching candidates, falls back only when necessary, then applies cap. GrowthStep retained. |
| `weed/util/http/http_global_client_util.go` | Upstream server-message errors combined with fork resume offsets, partial-response validation, bounded drain and replica load accounting. HTTP tests and replica race checks pass. |

## Conflict matrix: merge 2

| Conflicted path | Resolution assessment / evidence |
| --- | --- |
| `weed/command/webdav.go` | Cache options/budgets retained alongside upstream idle timeout. |
| `weed/filer/filer_notify.go` | `AddDataToBufferAndGetTs` replaces fork `AddDataToBufferAt`; returned stream position assigned to event and directory writer tracker. Mutation stamp remains separate. |
| `weed/mount/weedfs.go` | Stale-section refresh added without dropping currency/deferred-create checks. Investigated possible unbounded synchronous refresh: `refreshSection` uses DoChan and selects on caller context, so no extra await wrapper is needed. Refresh/cancellation tests pass. |
| `weed/mount/weedfs_file_sync.go` | Fork doFlush remains, including metadata deadline starting after data upload. Upstream releasePosixOwner/forgetLockOwner pairing applied in both handle-present and absent paths. |
| `weed/pb/filer_pb/filer.pb.go` | Regenerated combined message indexes/descriptor include fork directory currency and upstream mutation timestamp fields. PB, filer and server tests pass; no handwritten descriptor conflict markers remain. |
| `weed/s3api/s3api_bucket_handlers.go` | New receiver-based owner helper combined with fork auto-created-bucket empty-folder cleanup setting. |
| `weed/server/filer_grpc_server_rename.go` | Upstream scrubbed chunk logging and time-valued deletion contract adopted; fork SharedChunksHeld gate retained. |
| `weed/server/filer_grpc_server_sub_meta.go` | Callback carries both stream/mutation timestamps and fork origin-stamping callback; prefilter preserved. |
| `weed/server/volume_grpc_copy.go` | Upstream reserved headroom applied to fork staged copy and ReplaceVolume. Inner named error releases unused reservation on copy failure and zeros it before outer cleanup; staged files cleaned, existing replica remains until validated replacement. Copy preservation/status/count and headroom suites pass. See residual observations below. |
| `weed/shell/command_volume_fsck.go` | Always traverses `/`, keeping cross-bucket references visible. Scoped-walk optimization intentionally removed; repair, outside-collection accounting and one-walk seam retained. |
| `weed/util/log_buffer/log_buffer.go` | Upstream exported timestamp-returning method replaces same-purpose fork method; queue bounds and monotonic append stamp survive. Filer caller renamed; log-buffer tests pass. |

## Tests edited by the merger

Beyond the conflicted tests above, reviewed the remerge-diff adaptations:

- **Incomplete `.dat` tails:** `volume_checking.go` refuses repair before either file is changed when `cutAt < fileSize`. Changed tail tests compare both files byte-for-byte across two loads and require no writes/deletes. Pure complete tails still re-index and remain covered. The canDelete case now correctly refuses deletes as well. The unsynced-repair test switches to a complete needle so it still reaches the data-sync-before-index operation. This is an explicit fork safety policy, not evidence of upstream-equivalent torn-tail recovery. A complete needle preceding an incomplete needle is deliberately left unindexed too.
- **Fsck:** fixtures append the hard-link length field; callbacks now return errors. DELETE failure is asserted at worker completion rather than queue submission. Worker concurrency and hard-link serialization checks remain. Scoped-walk setup removed because traversal now starts at root; the outside-bucket-reference assertion remains. These changes match the implementation contract rather than suppress failures.
- **Copy/HTTP:** tests add context parameters, MD5 result slots and optional digest argument; error/ETag/content-length assertions remain. HTTP error test targets renamed resume-aware helper with offset zero.
- **Mount:** context arguments and four-value AcquireFileHandle returns updated. Hard-link sibling test explicitly returns Unimplemented from lookup to exercise the fallback counter patch; upstream's successful-authoritative-refresh coverage remains separately. This changes what that particular test proves, but does not justify removing the refresh behavior.
- **Rename acknowledgments:** log-reading helper uses outer LogEntry.TsNs, matching subscription delivery rather than the serialized mutation stamp, and clears MutationTsNs because StreamRenameEntryResponse does not carry it. Correct comparison for acknowledgment stream positions; not a test of mutation-time preservation itself.
- **Filer:** helper now takes `testing.T` for lifecycle cleanup; deletion sentinel changes from integer zero to `time.Time{}`. Assertions remain.
- **Topology/storage:** new rack-quota/GrowthStep/RegisterVolume parameters supplied; headroom test uses instance-injected disk probe instead of global replacement. Assertions remain.
- **Preallocation mutant:** pattern updated from syscall.Fallocate to unix.Fallocate/KEEP_SIZE in both fixed and mutant strings; ignored-allocation-error mutation still targets the same behavior.

No test change reviewed was found to conceal a newly dropped fork behavior. This judgment excludes the two broken FUSE fixture configurations found and repaired above; those tests had not been changed to pass, and failed before their workload.

## Load, latency, and data-preservation evidence

### Paced S3 GETs

Used the supplied `slow_proxy.py` at 1 MB/s per connection and 12 MB/s aggregate. A disposable volume bound to 127.0.0.2 and advertised 127.0.0.1; HTTP passed through the paced proxy, gRPC through an unpaced loopback forwarder. Separate local master/filer/S3 processes, a random 32 MiB object split into 2 MiB chunks, 64 MiB global / 16 MiB session reader budgets. Disk-on case used 128 MiB scope / 256 MiB whole-cache limits. Every returned body was compared byte-for-byte against the uploaded bytes. Counts include upstream HTTP headers and any background proxy traffic; they are not pure payload counters. Proxy snapshots waited for its 5-second reporting interval.

| Disk cache | Request | Returned bytes | Proxied bytes | Wire ratio | Request seconds |
| --- | --- | ---: | ---: | ---: | ---: |
| Off | Whole | 33,554,432 | 33,559,760 | 1.0002 | 10.513 |
| Off | Repeated whole | 33,554,432 | 33,559,760 | 1.0002 | 2.843 |
| Off | Open-ended from 16 MiB | 16,777,216 | 17,042,408 | 1.0158 | 1.383 |
| Off | bytes=100-4195 | 4,096 | 4,770 | 1.1646 | 0.002 |
| On | Cold whole | 33,554,432 | 33,559,760 | 1.0002 | 10.530 |
| On | Repeated whole | 33,554,432 | 0 | 0 | 0.059 |
| On | Open-ended from 16 MiB | 16,777,216 | 0 | 0 | 0.022 |
| On | bytes=100-4195 | 4,096 | 0 | 0 | 0.001 |

A separate run **restarted the S3 process between cold and repeated GETs**, proving persistence rather than only memory-cache reuse: cold whole 33,559,760 proxied bytes in 10.521s; after restart, whole 0 bytes/0.058s, open-ended 0/0.022s, small range 0/0.001s. Payload equality held. The proxy's per-connection pacing uses cumulative elapsed time, so idle warm connections can burst; warm timings are not a calibrated link benchmark. The 93x historical amplification was not reproduced.

**Historical disk-cache failure remains unconfirmed.** Current code enables shared scope `s3`; the quarter-scope cutoff is based on `ViewOffset + ChunkSize`, not merely chunk size, and `wholeCacheCapacityMB` does not enlarge it. The supplied 2048 MiB scope gives a 512 MiB cutoff, so a normal contiguous 495 MB object with 2–4 MiB chunks should be eligible. Thus size cutoff alone does not explain that particular incident. The embedded-S3 configuration fix in `6116664e6` survives, but does not explain a standalone gateway. Current-process and restart checks demonstrate a functioning cache on this tip; without the historical process/cache state, assigning a production root cause would be speculation.

**Limits:** these proxy measurements cover plain chunked GETs and ranges, not every SSE or multipart encryption combination, concurrent production readers, or a 495 MB reproduction. Existing S3 encryption, multipart, range-copy and streaming-prefetch unit tests ran in the package suite. Pin-aware eviction remains a preference with a hard-budget fallback: if all eligible buffers are pinned and the working set exceeds the cap, eviction and re-fetch remain possible. The retained slow-stream/budget-pressure tests pass, including under race instrumentation; this is not a proof of zero amplification for arbitrarily undersized budgets.

### Filer and mount metadata

Persisted-log scan tests cover start-minute seek, capped-store pagination, UTC, empty days and reactivation. Their implementation files remain unchanged where the fork introduced these fixes; helper cleanup changes preserve assertions. LevelDB2 retains directory-prefix bounds while upstream adds configurable caches and synchronous writes. Stale-section lookup test requires one listing and zero per-entry RPCs for three lookups; it passes under the race detector. Real frozen-backend timing is recorded above.

`BenchmarkDirectoryListingMetadata` (100ms runs, shared host) measured unopened 0 B/0 allocs, opened entries about 1184 B/8 allocs at 1, 128 and 4096 chunks. Opened timing ranged 1.05–5.38 microseconds; allocation remains independent of chunk count. This checks the fork's metadata snapshot allocation property, not filer p99 latency under production load.

### Uploads, recovery and copy

The operation suite passed in 70.906s, including slow-but-progressing uploads, NAS-shaped concurrency, stopped-upload abandonment, fast-host watchdog and body detachment tests. The slow-progress test also passed under race instrumentation. Real stalled-server close/reassignment passed after fixture repair.

Storage suite passed in 35.747s. Tail tests preserve incomplete bytes and read-only state, complete-tail tests re-index, and vacuum preservation/absent-only repair remain covered. The explicit changed recovery policy is consistent with the fork requirement. Upstream syncIndex correctly replaces the earlier pre-publication sync with post-publication durability. Server copy tests pass, including failed-stream preservation of the old replica and replacement after validation.

Residual static observations in VolumeCopy: its master configuration/CopyFile RPCs still use background contexts, so this audit does not establish prompt cancellation of a stalled maintenance copy. The preallocation call discards the returned backend without explicit Close, relying on file finalization for that descriptor; this is already present at the fork parent, not introduced by the merge. No data-loss claim or fix is made from these observations. Real crash/power-loss and separate-index-device qualification was not run.

## Test commands and results

All logs and parent diffs are retained locally in `/tmp/audit451-codex/`. They are audit scratch evidence, not required runtime files.

| Check | Result |
| --- | --- |
| `go build -p 4 -tags 5BytesOffset -o /tmp/audit451-codex/weed ./weed` | PASS |
| `go vet -p 4 -tags 5BytesOffset ./weed/...` | FAIL solely on documented `weed/storage/types/offset_5bytes.go:30:13: offset (32 bits) too small for shift of 32` |
| `go test -p 4 -tags 5BytesOffset -timeout 120s` over `go list ... ./weed/...`, excluding `needle_map/old` | 155 packages pass, 77 no-test packages; four package failures detailed below |
| `go test -p 4 -tags 5BytesOffset -timeout 10m ./weed/server` | PASS, 129.180s; initial 120s package timeout was too short, not an isolated hanging test |
| `go test -p 4 -tags 5BytesOffset -timeout 6m -v ./weed/worker/tasks/erasure_coding` | PASS, 310.513s; initial 120s package timeout was too short |
| `python3 -m unittest discover -s test/storage_lab -p 'test_*.py'` | PASS, 26 tests; only unprivileged harness unit tests, no VM run |
| Windows mount test cross-build: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -p 4 -tags 5BytesOffset -c -o /tmp/audit451-codex/mount-windows.test.exe ./weed/mount` | RED at both metadata callers, then GREEN after fix |
| Windows binary: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -p 4 -tags 5BytesOffset -o /tmp/audit451-codex/weed.exe ./weed` | PASS after fix |
| `WEED_BINARY=/tmp/audit451-codex/weed go test -p 4 -count=1 -timeout 5m -v -run 'TestUnresponsiveBackendAnswersWithinRequestTimeout\|TestCloseSucceedsWhenAnAssignedVolumeServerStalls' .` in `test/fuse_integration` | Both RED before fixture corrections; both GREEN, 98.054s combined |
| Race: `GOMAXPROCS=4 go test -p 4 -tags 5BytesOffset -race -timeout 3m ./weed/filer ./weed/util/http ./weed/mount ./weed/operation -run 'TestSequentialSmallReadsFetchEachChunkOnceOnSlowServers\|TestChunkReadAtSequentialPrefetchStaysWithinWindow\|TestSlowStreamKeepsItsChunkUnderBudgetPressure\|TestLookupRefreshesStaleSection\|TestSlowButProgressingUploadIsNotAbandoned\|TestReplica'` | PASS; HTTP had no matching test, so explicitly rerun below |
| Replica race: `GOMAXPROCS=4 go test -p 4 -tags 5BytesOffset -race -count=1 ./weed/util/http -run 'TestReachableFirst\|TestSlowReplicaCostFades\|TestRetriedFetchChunkDataAvoidsTheSlowReplica'` | PASS, 1.488s |
| `go test -p 4 -tags 5BytesOffset -run '^$' -bench '^BenchmarkDirectoryListingMetadata$' -benchtime=100ms -benchmem ./weed/mount` | PASS; values above |
| `python3 -u /tmp/audit451-codex/slow_get.py` and `slow_get_restart.py` | PASS; loopback S3 measurements above; scripts and service logs retained in scratch directory |
| `git diff --check` | PASS |

Initial suite failures: server and worker EC package-level 120s timeout; `weed/storage/erasure_coding` (`TestPositioning`, `TestLocateEcShardNeedleMatchesInterval`); `weed/storage/idx` (`TestCheckIndexFile`). Both timeout packages pass with the longer budgets above, bringing the combined result to 157 passing packages, 77 with no tests, and two packages with known failures. The latter three test failures match the supplied known-failure list. They were observed here, not independently rerun on other branches. `needle_map/old` was excluded as instructed. Full FUSE/POSIX, Windows runtime, privileged storage-lab, Rust, and external-service qualification were not run; successful package status can include tests that skip absent services.

## Appendix A: complete fork commit inventory

`=` means every path touched by that commit is unchanged between the fork parent and merged tip. `Δ` lists touched paths that changed; the conflict matrix and subsystem discussion above explain the substantive combinations. This is an inventory of net effects relative to the fork parent; `=` does not imply its original patch was never superseded, nor that unchanged callers are immune to contract changes. Runtime coverage for Linux `weed/` tests is the package suite above, Windows tests are compile-only, and nested/VM tests are not claimed run except the explicit commands above.

“Tests” lists Go test names added by the commit that still occur in its touched test files at the merged tip. Historical additions absent already at the fork parent are marked separately. Commits containing only harness, workflow or documentation changes often have no Go test additions; their path comparison is recorded without claiming a runtime qualification.

| Commit | Intent | Parent-to-tip comparison | Tests added / retained |
| --- | --- | --- | --- |
| `b9d3b99cb` | filer: count chunks already in flight toward the prefetch window | Δ `weed/filer/reader_at_prefetch_window_test.go`, `weed/filer/reader_cache.go` | `TestSequentialSmallReadsFetchEachChunkOnceOnSlowServers` |
| `040ad1de8` | http: read each chunk from the replica answering fastest | Δ `weed/util/http/http_global_client_util.go` | `TestReachableFirstPrefersTheFasterReplica`; `TestReachableFirstSpreadsReadsInFlight`; `TestSlowReplicaCostFades`; `TestReachableFirstKeepsUnansweringHostLastOverLoad`; `TestRetriedFetchChunkDataAvoidsTheSlowReplica` |
| `4a80f9c98` | mount: -fuse.metadataRequestTimeout (25s) bounds namespace requests | Δ `weed/command/mount.go` | `TestFuseMetadataRequestTimeoutDefault`; `TestFuseMetadataRequestTimeoutMustNotExceedRequestTimeout` |
| `ccd572db7` | filer: paginate persisted log listings | Δ `weed/filer/filer_notify_read_listing_test.go` | `TestPersistedLogReadsPaginateStoreCappedListings` |
| `f3eb038e9` | filer: verify persisted log listing test delivers all entries after start | Δ `weed/filer/filer_notify_read_listing_test.go` | No added Go test declaration |
| `3e850ffe4` | filer: advance across empty log days and reactivate idle filers | Δ `weed/filer/filer_notify_read_listing_test.go` | `TestPersistedLogReadsAcrossDayBoundaries` |
| `1c5bbb934` | filer: normalize persisted log scan start time to UTC | Δ `weed/filer/filer_notify_read_listing_test.go` | `TestPersistedLogReadsUseUTCRegardlessOfStartPositionTimezone` |
| `ce9cd6e87` | filer: persisted log reads list from the scan start minute instead of the whole day | Δ `weed/filer/filer_notify_read_listing_test.go` | `TestPersistedLogReadsListOnlyFromTheScanStartMinute` |
| `094ecf7e0` | shell: volume.fsck -repairMissingFromReplicas restores, reports and purges orphans in one filer walk; -maxOrphanPercent gates the purge | Δ `weed/shell/command_volume_fsck.go`, `weed/shell/command_volume_fsck_repair.go`, `weed/shell/command_volume_fsck_repair_test.go`, `weed/shell/command_volume_fsck_single_walk_test.go` | `TestFsckRepairsReportsAndPurgesInOneFilerWalk`; `TestFsckPurgesNothingAboveTheOrphanLimit` |
| `02b9464d2` | shell: volume.fsck runs its checks from checkVolumes, behind a replaceable filer walk | Δ `weed/shell/command_volume_fsck.go` | No added Go test declaration |
| `cd22471e9` | shell: volume.fsck -repairMissingFromReplicas closes each volume's replica indexes before loading the next; holding all of them exhausted a 4 GiB whole-cluster run | Δ `weed/shell/command_volume_fsck_repair.go`, `weed/shell/command_volume_fsck_repair_test.go` | `TestFsckRepairReleasesEachVolumeIndexesBeforeTheNext` |
| `6116664e6` | fix(cache): honor embedded S3 disk scope and explicit filer request budgets | Δ `weed/command/filer.go`, `weed/server/filer_server.go`, `weed/server/filer_server_handlers_read.go` | `TestEmbeddedS3ExplicitCacheScope`; `TestFilerSessionReaderBudget`; `TestPublishedImageMigratesLegacyCache` |
| `17806ab05` | fix: make cache shard masking explicit and bound full gate runtime | = (2 paths) | No added Go test declaration |
| `d4539559d` | fix(cache): protect legacy files and add verified offline migration | = (9 paths) | `TestCacheMigrateCommandCopiesLegacyCache`; `TestLegacyCacheMigrationPreservesSourceAndSurvivesRestart`; `TestLegacyCacheMigrationRejectsBudgetAndCancellation`; `TestSharedCacheDoesNotEvictLegacyOrForeignFiles`; `TestSharedCacheRepairsSameSizeInvalidEntry`; `TestSharedCacheRejectsOverflowingReadOffset` |
| `6c07c810b` | feat(cache): unify nearside chunk & reader cache across mount, s3, filer, and webdav with per-request and whole-cache scoping | Δ `weed/command/filer.go`, `weed/command/mini.go`, `weed/command/mount.go`, `weed/command/s3.go`, `weed/command/server.go`, `weed/command/webdav.go`, `weed/filer/reader_cache.go`, `weed/filer/reader_cache_memory_test.go`, `weed/mount/filehandle.go`, `weed/mount/weedfs.go`, `weed/s3api/s3api_object_handlers.go`, `weed/s3api/s3api_server.go`, `weed/server/filer_server.go`, `weed/server/filer_server_handlers_read.go`, `weed/server/webdav_server.go` | `TestReaderCachePerRequestAndWholeCacheScoping`; `TestSharedTieredChunkCacheCrossMountAndTwoLevelScoping` |
| `c19392001` | storage gate: back to WinFsp DLL appmana-dll-2.1-066843359; the stock driver already denies foreign UNC targets (gate 38974607661 passed every network case with it) | = (1 paths) | No added Go test declaration |
| `413498c12` | storage gate: WinFsp DLL appmana-dll-2.1-0431274a5; a UNC target on the volume itself stays accepted | = (2 paths) | No added Go test declaration |
| `9a542300c` | storage gate: WinFsp DLL appmana-dll-2.1-6470974c6 (UNC targets only under the volume's own prefix) | = (1 paths) | No added Go test declaration |
| `751e71649` | storage gate: foreign UNC symlink case runs with the SMB server answering, as on a node | = (2 paths) | No added Go test declaration |
| `6f58315d0` | storage gate: a regression must also slow the candidate's fastest trial | = (2 paths) | `TestIndexPerformanceGateIgnoresCandidateOnlySlowTrials` |
| `44e3f5e0f` | storage gate: a UNC symlink target on another WinFsp network volume must be denied | = (2 paths) | No added Go test declaration |
| `6a5705033` | mount: Windows pods' absolute symlinks under their mount path are stored volume-relative | Δ `weed/command/mount.go`, `weed/mount/weedfs.go` | `TestWindowsNetworkSymlinkTargets`; `TestVolumeTargetFromMountRoots`; `TestMountRootTargetReadsPublishedRoots` |
| `6946f7f37` | storage gate: WinFsp DLL appmana-dll-2.1-066843359 (keeps absolute drive-letter symlink targets) | = (1 paths) | No added Go test declaration |
| `190d8310d` | winfsp: absolute drive-letter symlinks to other volumes are kept as written; UNC targets stay denied | = (1 paths) | No added Go test declaration |
| `b9b724ef3` | storage lab: run the Windows mount suites with the released WinFsp DLL beside weed.exe | = (3 paths) | `TestWindowsReleaseDLLRequiresPinAndExclusiveSource` |
| `a66568f44` | storage lab: compare performance trials pairwise | = (2 paths) | `TestIndexPerformanceGateComparesTrialsPairwise` |
| `de93ae161` | storage lab: retry lost guest-agent replies when reading Windows artifacts | = (3 paths) | `TestWindowsArtifactRetriesLostChunkReply`; `TestWindowsArtifactGivesUpOnPersistentFailure` |
| `a2dcf6643` | mount: judge a cached listing current per filer log, never by the mount's own changes | Δ `other/java/client/src/main/proto/filer.proto`, `weed/filer/filer.go`, `weed/filer/filer_notify.go`, `weed/mount/meta_cache/meta_cache.go`, `weed/mount/meta_cache/meta_cache_init.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_dir_current.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_dir_read_cache_flags_test.go`, `weed/pb/filer.proto`, `weed/pb/filer_pb/filer.pb.go`, `weed/pb/filer_pb/filer_vtproto.pb.go`, `weed/server/filer_grpc_server_sub_meta.go`, `weed/server/filer_grpc_server_sub_meta_prefilter_test.go`, `weed/util/log_buffer/log_buffer.go` | `TestCrossMountListingAfterOwnCloseSeesPeerFilerClose`; `TestDirectoryWriterChangesLeaveOutTheCaller`; `TestDirectoryWriterChangesForgetLeastRecentWriter`; `TestDirectoryWriterChangesForgetIntoTheFloor`; `TestLogMetaEventRecordsLogPosition`; `TestDirectoryPositionsFollowRelayedChangesPerFiler`; `TestLocalReplyDoesNotMovePositions`; `TestDirectoryPositionsCoverRenameParentsAndMovedDirectory`; `TestDirectoryPositionsSkipUncachedDirectories`; `TestDirectoryPositionsResetByPurge`; `TestNoteDirectoryBasisReleasesWaiters`; `TestOpenDirRelistsWithFilerWithoutPerFilerPositions`; `TestOpenDirWaitsForPeerFilerChangeBehindOwnChange`; `TestDirectoryChangePositionNamesThisFilersLogLeavingOutTheCaller`; `TestStampLocalOriginMarksAppendPosition` |
| `65ea6320a` | mount: fold the small appended tail into one chunk on flush | Δ `weed/mount/weedfs_file_sync.go` | `TestAppendFlushesKeepEntryChunkCountBounded`; `TestAppendFlushesKeepChunkCountBounded`; `TestAppendTailFoldsOnlyCommittedChunks`; `TestAppendTailLeavesOverlappedRunAlone`; `TestAppendTailFoldFailureKeepsChunks` |
| `bad41e874` | shell: volume.fsck and volume.check.disk refuse servers with another offset size | Δ `weed/shell/command_volume_fsck.go` | `TestRequireServerIndexLayoutRejectsAnotherOffsetSize` |
| `d5ff91dc5` | shell: volume.fsck -repairMissingFromReplicas copies filer-referenced needles a replica lacks | Δ `weed/shell/command_volume_fsck.go`, `weed/shell/command_volume_fsck_repair.go`, `weed/shell/command_volume_fsck_repair_test.go` | `TestFsckRepairsFilerReferencedNeedlesFromAnotherReplica`; `TestFsckRepairFailsOnWriteError` |
| `70f0585e7` | mount: drop the surviving hard link's count when a rename replaces its sibling | Δ `weed/mount/weedfs_rename.go` | `TestRenameOverMultiplyLinkedTarget` |
| `837a41cdc` | ci: check out the go-fuse fork in pjdfstest, Samba and mount benchmark jobs | Δ `.github/workflows/mount-benchmark.yml`, `.github/workflows/pjdfstest.yml`, `.github/workflows/samba-integration.yml` | No added Go test declaration |
| `ec3c59795` | README: 4.48 is the default and only maintained branch | Δ `README.md` | No added Go test declaration |
| `a3d40afc3` | Maintain 4.48 only; build WinFsp in its fork, not here | = (12 paths) | No added Go test declaration |
| `08bd485b5` | mount(windows): point directory mount junctions at the volume GUID path | = (4 paths) | `TestMountPointReparseRoundTrip`; `TestMountPointReparseTargetParsesWinFspJunction`; `TestMountPointReparseTargetRejectsMalformed` |
| `e7f336e85` | ci: pin Git for Windows 2.56.0 (Git LFS 3.8.0) for the Windows Git/LFS gate | = (1 paths) | No added Go test declaration |
| `c9e494c6d` | ci: fetch pinned WinFsp and Git installers for the Windows Git/LFS gate | = (1 paths) | No added Go test declaration |
| `30a22471d` | volume: test uploads and range reads together against the admission budgets | = (2 paths) | `TestTransfersStayWithinAdmissionBudgets` |
| `46df88195` | volume: test uploads and range reads together against the admission budgets | = (2 paths) | `TestTransfersStayWithinAdmissionBudgets` |
| `d70a1d7cb` | volume: hold an upload body in exactly its admitted bytes; in-flight gauges track releases | Δ `weed/server/volume_server.go`, `weed/server/volume_server_handlers_read.go` | `TestStalledUploadsHoldOnlyTheirAdmittedBytes`; `TestStalledRangeReadsHoldOnlyTheirAdmittedBytes`; `TestCompletedTransfersClearInFlightGauges`; `TestParseUploadHoldsOnlyTheBody`; `TestParseUploadUnknownLengthReadsWholeBody`; `TestParseUploadRejectsBodyOverSizeLimit` |
| `d51aa21ee` | volume: hold an upload body in exactly its admitted bytes; in-flight gauges track releases | Δ `weed/server/volume_server.go`, `weed/server/volume_server_handlers_read.go` | `TestStalledUploadsHoldOnlyTheirAdmittedBytes`; `TestStalledRangeReadsHoldOnlyTheirAdmittedBytes`; `TestCompletedTransfersClearInFlightGauges`; `TestParseUploadHoldsOnlyTheBody`; `TestParseUploadUnknownLengthReadsWholeBody`; `TestParseUploadRejectsBodyOverSizeLimit` |
| `af26e7d6a` | volume: keep the existing replica until a VolumeCopy is complete and mounted | Δ `weed/server/volume_grpc_copy.go`, `weed/storage/volume_write.go` | `TestVolumeCopyKeepsExistingReplicaWhenSourceStreamFails`; `TestVolumeCopyReplacesExistingReplica`; `TestReplaceVolumeRestoresOldReplicaWhenMountFails` |
| `eb15de94e` | volume: keep the existing replica until a VolumeCopy is complete and mounted | Δ `weed/server/volume_grpc_copy.go`, `weed/storage/volume_write.go` | `TestVolumeCopyKeepsExistingReplicaWhenSourceStreamFails`; `TestVolumeCopyReplacesExistingReplica`; `TestReplaceVolumeRestoresOldReplicaWhenMountFails` |
| `13ee7b7fc` | fuse test: bound a close() with one stalled server at 41 s, not 30 s | = (1 paths) | No added Go test declaration |
| `4242670a7` | master: assign a volume that avoids the servers a writer saw stall | Δ `README.md`, `other/java/client/src/main/proto/filer.proto`, `seaweed-volume/proto/master.proto`, `weed/operation/assign_file_id.go`, `weed/operation/upload_chunked.go`, `weed/operation/upload_content.go`, `weed/operation/upload_content_test.go`, `weed/pb/filer.proto`, `weed/pb/filer_pb/filer.pb.go`, `weed/pb/filer_pb/filer_vtproto.pb.go`, `weed/pb/master.proto`, `weed/pb/master_pb/master.pb.go`, `weed/s3api/s3api_object_handlers_copy.go`, `weed/s3api/s3api_object_handlers_put.go`, `weed/server/filer_grpc_server.go`, `weed/server/master_grpc_server_assign.go`, `weed/server/master_grpc_server_assign_test.go`, `weed/topology/volume_layout.go` | `TestUploadWithRetryAsksTheMasterToAvoidAStalledServer`; `TestAssignAvoidsTheServersTheWriterNames`; `TestPickForWriteAvoidsTheServersAWriterNames`; `TestPickForWriteAvoidsAVolumeAnyOfWhoseHoldersIsNamed`; `TestPickForWriteFallsBackWhenEveryVolumeIsAvoided`; `TestVolumeGrowOptionStringOmitsAvoidServers`; `TestConstrainedPickFallsBackWhenOnlyAvoidedServersMatch` |
| `3c166a210` | storage lab: hold the victim's volume server inside the vacuum copy before the power cut | = (4 paths) | `TestVacuumCopyBarrierHoldsAShortCopyOpen`; `TestVacuumCopyBarrierFailsWithoutACopy` |
| `07a5a87b5` | storage reliability CI: take the Windows Server lab image from a repository secret and pull it when the runner lacks it | Δ `README.md` | No added Go test declaration |
| `5bd2ac835` | operation: test that a slow but healthy NAS-shaped volume server keeps every upload | = (1 paths) | `TestSlowHealthyNasShapedServerIsNotAbandoned` |
| `745f6dc7d` | operation: abandon a volume server write only when it stops making progress | Δ `README.md`, `weed/operation/upload_chunked.go`, `weed/operation/upload_content.go`, `weed/s3api/s3api_object_handlers_copy_stream.go`, `weed/topology/store_replicate.go`, `weed/topology/store_replicate_test.go` | `TestSlowButProgressingUploadsClose`; `TestChunkUploadTimeoutOrdering`; `TestSlowButProgressingUploadIsNotAbandoned`; `TestUploadThatStopsMovingIsAbandoned` |
| `6a177e1fc` | storage reliability CI: run the stalled volume server close() and S3 copy regressions | = (1 paths) | No added Go test declaration |
| `e3a6125a3` | s3: move a stalled chunk write or copy to a new assignment that avoids the stalled server | Δ `README.md`, `weed/mount/weedfs.go`, `weed/operation/upload_chunked.go`, `weed/operation/upload_content.go`, `weed/s3api/s3api_object_handlers_copy.go`, `weed/s3api/s3api_object_handlers_copy_stream.go`, `weed/s3api/s3api_object_handlers_put.go`, `weed/s3api/s3api_server.go` | `TestUploadReaderInChunksMovesOffAStalledHolder`; `TestCopyChunkRangeMovesOffAStalledDestination` |
| `1f10d72c7` | mount: move a stalled chunk upload to a new assignment that avoids the stalled server | Δ `README.md`, `test/fuse_integration/framework_test.go`, `weed/mount/weedfs.go`, `weed/operation/upload_content.go`, `weed/operation/upload_content_test.go`, `weed/topology/store_replicate.go`, `weed/topology/store_replicate_test.go` | `TestCloseSucceedsWhenAnAssignedVolumeServerStalls`; `TestChunkUploadAttemptTimeoutOrdering` (historical; see net-baseline note); `TestUploadWithRetryMovesOffAStalledVolumeServer`; `TestUploadWithRetryAvoidsTheReplicaAPrimaryNames`; `TestUploadWithRetryUsesAStalledServerWhenNothingElseIsAssigned`; `TestStalledServersExpire`; `TestReplicatedWriteFailsFastNamingAStalledReplica` |
| `796a2c0d7` | README: 4.48 base and mount benchmark against the deployed 4.47 line | Δ `README.md` | No added Go test declaration |
| `7729c0a00` | server: let the VolumeCopy deletes test copy from a loopback source | = (1 paths) | No added Go test declaration |
| `f99eafea5` | volume: test VolumeCopy of a source with deletes since load | = (1 paths) | `TestVolumeCopySucceedsForSourceWithDeletes`; `TestVolumeCopyRejectsTruncatedDatFromSourceWithDeletes` |
| `0ebf134be` | storage: unify needle-map counters across index loaders and warn on copy-count drift (#11701) | Δ `weed/server/volume_grpc_copy.go`, `weed/storage/needle_map_metric.go` | No added Go test declaration |
| `1553519c9` | README: describe the fork's fixes, configuration, CI, qualification and benchmarks | Δ `README.md` | No added Go test declaration |
| `23b32041c` | storage reliability CI: take the VM lab image from a repository secret when set | Δ `README.md` | No added Go test declaration |
| `0f3d7bec5` | mount: show other mounts' closed files to listings and lookups that start afterwards | Δ `other/java/client/src/main/proto/filer.proto`, `weed/filer/filer.go`, `weed/filer/filer_notify.go`, `weed/filer/meta_aggregator.go`, `weed/mount/meta_cache/meta_cache.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_dir_current.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_dir_read_cache_flags_test.go`, `weed/pb/filer.proto`, `weed/pb/filer_pb/filer.pb.go`, `weed/pb/filer_pb/filer_grpc.pb.go`, `weed/pb/filer_pb/filer_vtproto.pb.go` | `TestCrossMountCloseToOpenVisibility`; `TestChangedDirectories`; `TestDirectoryChangesRemembersNewestChangePerDirectory`; `TestDirectoryChangesForgetsIntoTheFloor`; `TestNotifyUpdateEventRecordsDirectoryChange`; `TestDirectoryCurrentThroughFollowsAppliedChanges` (historical; see net-baseline note); `TestDirectoryCurrentThroughCoversRenameParentsAndMovedDirectory` (historical; see net-baseline note); `TestDirectoryCurrentThroughSkipsUncachedDirectories` (historical; see net-baseline note); `TestDirectoryCurrentThroughResetByPurge` (historical; see net-baseline note); `TestNoteDirectoryCurrentThroughReleasesWaiters` (historical; see net-baseline note); `TestKeepKernelListingOnlyWithoutNewerChange`; `TestOpenDirKeepsKernelListingOfUnchangedDirectory`; `TestOpenDirWaitsForAcknowledgedChange`; `TestOpenDirRelistsWhenTheStreamDoesNotDeliver`; `TestOpenDirRelistsWhenAPeerFilerCannotAnswer`; `TestOpenDirServesCacheWhenFilerUnreachable`; `TestOpenDirWithFilerWithoutPositions`; `TestNegativeLookupWaitsForAcknowledgedCreate` |
| `2f09eb3bf` | operation: guard the stalled-volume-server test's held connections against the accept goroutine | = (1 paths) | No added Go test declaration |
| `1c769f386` | storage_lab: gofmt the stuck-backend gate | = (1 paths) | No added Go test declaration |
| `2240a0b53` | storage_lab: hung-task gate for a frozen backend, and run the FUSE gates in CI | = (3 paths) | `TestFuseStuckBackendLab` |
| `f35ef4e21` | test/fuse_integration: bound close() after a large write by the write buffer cap | Δ `test/fuse_integration/framework_test.go` | `TestLargeSequentialWriteClosesWithinWriteBufferDrainTime` |
| `94b9b92d8` | mount, mq: address the filer chunk proxy on the filer's HTTP port for uploads | Δ `weed/mount/weedfs.go` | `TestFilerProxyChunkUrlUsesTheFilerHttpAddress` |
| `d4bb8c4e6` | mount: start the metadata flush deadline after the data upload, not before | Δ `weed/mount/weedfs_attr.go`, `weed/mount/weedfs_file_io.go`, `weed/mount/weedfs_file_sync.go`, `weed/mount/weedfs_invalidate_open_handle_test.go`, `weed/mount/weedfs_rename.go` | `TestFlushCommitsMetadataAfterUploadsOutlastTheMetadataDeadline` |
| `b248f3000` | mount: bound each chunk upload attempt to 60 s | Δ `weed/operation/upload_content.go` | `TestUploadAttemptTimeoutBoundsAStalledVolumeServer` |
| `ba124c57a` | mount: answer every FUSE request within -fuse.requestTimeout, negotiate a kernel timeout | Δ `weed/command/mount.go` | `TestUnresponsiveBackendAnswersWithinRequestTimeout`; `TestFuseRequestTimeoutDefaults`; `TestFuseRequestTimeoutMustExpireBeforeKernelTimeout` |
| `bb7969729` | chunk_cache: recreate a removed cache dir on rotation, never index a shut-down volume | = (4 paths) | `TestCacheSurvivesItsDirectoryBeingRemoved` |
| `722ea4c61` | Document how the fork, CSI images and cluster rollout fit together | Δ `README.md` | No added Go test declaration |
| `e4fe5f083` | Pull the pinned Linux lab images on the self-hosted VM runner | = (1 paths) | No added Go test declaration |
| `f82c4cf6a` | Make the post-rename cache test cross the LevelDB replay watermark | = (1 paths) | No added Go test declaration |
| `a2008859c` | Let VM-module workflow steps use the Labcontainers SDK's Go | = (1 paths) | No added Go test declaration |
| `fe559475e` | chunk_cache: use disk as chunks are cached, not the whole limit at creation | = (2 paths) | `TestChunkCacheVolumeDoesNotReserveItsLimitUpFront` |
| `3c3f1e8d9` | mount: stop waiting on a filer that does not answer within 60 s | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_rename.go` | `TestFilerRequestsGiveUpOnASilentFiler`; `TestRenameThatKeepsProgressingIsNotCutOff` |
| `2c6842b11` | gofmt the notify gate and apply-failure changes | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go` | No added Go test declaration |
| `f0058cdc9` | mount: degrade to read-through when the metadata store cannot apply an event | Δ `weed/mount/meta_cache/meta_cache.go`, `weed/mount/weedfs.go` | `TestSubscribedEventStoreFailureDoesNotExitTheMount`; `TestMetadataApplyFailureReadsEveryDirectoryThroughTheFiler` |
| `1f0d02f73` | Run the star history chart only in upstream's repository | Δ `.github/workflows/star_history.yml` | No added Go test declaration |
| `3cee1e40d` | Count both All permission lanes in the Windows suite inventory test | = (1 paths) | No added Go test declaration |
| `783b85942` | Keep the notify lab VM for the whole run, not ten minutes | = (1 paths) | No added Go test declaration |
| `b9a9d2bac` | Keep the notify lab VM for the whole run, not ten minutes | = (1 paths) | No added Go test declaration |
| `aa5e58a26` | mount: hold lock-taking kernel invalidations until no request holds the lock | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_access.go`, `weed/mount/weedfs_attr.go`, `weed/mount/weedfs_dir_lookup.go`, `weed/mount/weedfs_dir_mkrm.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_file_mkrm.go`, `weed/mount/weedfs_file_read.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go`, `weed/mount/weedfs_kernel_entry_invalidation_test.go`, `weed/mount/weedfs_link.go`, `weed/mount/weedfs_rename.go`, `weed/mount/weedfs_symlink.go`, `weed/mount/weedfs_xattr.go` | `TestNameExpiryWaitsForRequestHoldingTheDirectory`; `TestFileDataExpiryWaitsForReadInFlight`; `TestKernelNotifyGateDefersUntilLastHolderAndDeduplicates`; `TestKernelNotifyGateConcurrentHoldersNeverLoseNotifications` |
| `aefa1b87d` | Reproduce the mount notifier hung task in a disposable VM | = (2 paths) | `TestFuseReverseInvalidationLab` |
| `7d5fe7a95` | mount: hold lock-taking kernel invalidations until no request holds the lock | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_access.go`, `weed/mount/weedfs_attr.go`, `weed/mount/weedfs_dir_lookup.go`, `weed/mount/weedfs_dir_mkrm.go`, `weed/mount/weedfs_dir_read.go`, `weed/mount/weedfs_file_mkrm.go`, `weed/mount/weedfs_file_read.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go`, `weed/mount/weedfs_kernel_entry_invalidation_test.go`, `weed/mount/weedfs_link.go`, `weed/mount/weedfs_rename.go`, `weed/mount/weedfs_symlink.go`, `weed/mount/weedfs_xattr.go` | `TestNameExpiryWaitsForRequestHoldingTheDirectory`; `TestFileDataExpiryWaitsForReadInFlight`; `TestKernelNotifyGateDefersUntilLastHolderAndDeduplicates`; `TestKernelNotifyGateConcurrentHoldersNeverLoseNotifications` |
| `1ebc9bd1b` | Reproduce the mount notifier hung task in a disposable VM | = (2 paths) | `TestFuseReverseInvalidationLab` |
| `00ea5d5b5` | filer: compact and view plain chunks under a cancelled context instead of panicking | Δ `weed/filer/filechunk_manifest.go`, `weed/filer/filechunks.go`, `weed/filer/stream.go` | `TestCompactFileChunksIgnoresCanceledContextForPlainChunks` (historical; see net-baseline note); `TestCompactFileChunksKeepsEveryChunkWhenManifestResolveFails` (historical; see net-baseline note); `TestViewFromChunksIgnoresCanceledContextForPlainChunks` (historical; see net-baseline note); `TestChunkStreamReaderReportsManifestResolveFailure` |
| `e2523b291` | Add fork-owned multi-arch server candidates and concise package guidance | Δ `.github/workflows/container_release_unified.yml`, `README.md` | No added Go test declaration |
| `364103ec9` | Keep mount chunks until eviction and test slow S3 streams under budget pressure | Δ `weed/filer/reader_cache.go` | `TestSlowStreamKeepsItsChunkUnderBudgetPressure` |
| `6659fcac0` | filer: pin-aware reader cache eviction and stream release (#11503) | Δ `weed/filer/reader_cache.go`, `weed/s3api/s3api_object_handlers.go`, `weed/server/webdav_server.go` | `TestChunkReadAtPinnedChunkSurvivesEviction`; `TestChunkReadAtPinnedEvictionFallsBackWhenAllPinned` |
| `d5d3564b4` | filer: keep shared chunk buffers pinned while another stream reads them (#11502) | Δ `weed/filer/reader_cache.go` | `TestChunkReadAtConcurrentStreamsShareChunks`; `TestChunkStreamReleasesPreviousChunkOnCacheHit`; `TestChunkStreamDropsLeftChunkAfterInFlightRead`; `TestChunkStreamConcurrentReadsOnOneReader` |
| `ba67dfab7` | s3: drop implicit reader cache budget that throttled S3 GETs (#11384) | Δ `weed/command/filer.go`, `weed/command/mini.go`, `weed/command/s3.go`, `weed/command/server.go`, `weed/filer/reader_cache.go`, `weed/filer/reader_cache_memory_test.go`, `weed/s3api/s3api_server.go` | `TestReaderCacheUnboundedWithoutBudget`; `TestReaderCacheDropsConsumedChunks`; `TestReaderCachePrefetchBufferDroppedAfterRead`; `TestReaderCacheConsumedBufferSurvivesAttachedReader`; `TestReaderCacheConsumedChunkReleasesBudget` |
| `5d331e551` | Run Windows campaign permission routing contract in CI | = (1 paths) | No added Go test declaration |
| `02de0ee7d` | Make Windows qualification permission policies explicit | = (2 paths) | No added Go test declaration |
| `4602d4fbc` | Benchmark cached path attributes with and without open handles | = (1 paths) | No added Go test declaration |
| `bf3e9ec13` | Detach remotely unlinked names from surviving open handles | Δ `weed/mount/inode_to_path.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_invalidate_open_handle_test.go` | `TestWindowsRemoteDeleteLab`; `TestRemoteDeletePathPreservesReplacementAndOtherLinks` |
| `fe1264cfc` | Wait for cancelled S3 chunk copies to finish cleanup | Δ `weed/s3api/s3api_object_handlers_copy.go` | `TestCopyChunksConcurrentlyWaitsForCancelledSiblingCleanup` |
| `d604fdad4` | Require vacuum tail preservation tests in real Windows storage lab | = (1 paths) | No added Go test declaration |
| `7975d283a` | Add fail-closed per-replica vacuum index preservation audit | = (3 paths) | No added Go test declaration |
| `ed2ba652e` | Exercise unsafe vacuum refusal through the real volume RPC | = (2 paths) | `TestVacuumRPCPreservesUnindexedTail` |
| `e1aaa5124` | Refuse vacuum of mismatched data and index append boundaries | Δ `weed/storage/volume_vacuum.go` | `TestCompactCommitPreservesNewUnindexedTail` |
| `b66d08937` | Preserve partial unindexed append tails across volume restarts | = (1 paths) | `TestVolumeLoadPreservesPartialUnindexedTail` |
| `6a725b1a9` | Run all existing maintenance preservation fixtures in reliability CI | Δ `README.md` | No added Go test declaration |
| `c873145a7` | Bound repair index size decoding to the matched record | = (3 paths) | `TestRepairIndexBatchedRowsPreserveMatchedSize` |
| `73dec458b` | Add reference and checksum pinned absent-record repair command | Δ `README.md` | `TestMaintenanceReferencedNeedleRepair`; `TestRepairIndexNeverConfusesErrorsOrTombstonesWithAbsence`; `TestRepairNeedleRejectsInvalidArgumentsBeforeConnecting`; `TestRepairReferenceValidationDoesNotMutateSnapshot`; `TestRepairReadbackAllowsOnlyLocalAppendTimestamp`; `TestRepairNeedleReferenceAndPayloadPolicy` |
| `63261c9ec` | Expose absent-only repair through a fail-closed admin RPC | Δ `README.md`, `seaweed-volume/proto/volume_server.proto`, `seaweed-volume/src/server/grpc_server.rs`, `weed/pb/volume_server.proto`, `weed/pb/volume_server_pb/volume_server.pb.go` | `TestMissingNeedleRepairRPC`; `TestRepairNeedleRPCSafety` |
| `1b5d6c18c` | Add atomic absent-only durable needle repair primitive | Δ `weed/storage/volume_write.go` | `TestMissingNeedleRepairPreservesExistingRecords` |
| `ed55eec90` | Preserve cross-collection filer references during scoped fsck | Δ `weed/shell/command_volume_fsck.go` | `TestMaintenanceFsckCrossCollectionReferences` |
| `a12418bc2` | Fail fsck automation on malformed arguments | Δ `weed/shell/command_volume_fsck.go` | No added Go test declaration |
| `270654ab7` | Qualify readonly reporting and vacuum scheduling in real binaries | = (2 paths) | `TestMaintenanceReadOnlyAndScheduler` |
| `3cf4f5ea7` | Wake vacuum scheduler when per-server quota is returned | Δ `weed/topology/topology_vacuum.go`, `weed/topology/topology_vacuum_scheduler_test.go` | `TestVacuumSchedulerWakesWhenQuotaReturns`; `TestVacuumSchedulerPreservesParallelQuota` |
| `ba7d1ca1f` | Include all-readonly replica sets in report-only disk checks | = (2 paths) | `TestCheckAllReadOnlyReplicas` |
| `0391581e5` | Qualify real maintenance balance moves with original live payloads | = (1 paths) | `TestMaintenanceCLIBalancePreservesLiveData` |
| `1bc4eaa61` | Verify maintenance CLI preserves live volume data through vacuum and restart | = (1 paths) | `TestMaintenanceCLIPreservesLiveVolume` |
| `629aa0b61` | Run maintenance CLI qualification inside the packaged isolated image | = (1 paths) | `TestMaintenanceCLIImage` |
| `adc439d09` | Qualify maintenance CLI exit status against isolated real binaries | = (2 paths) | `TestMaintenanceCLIExitStatus` |
| `f2a49e83a` | Propagate writable replica disk-check failures to maintenance callers | = (2 paths) | `TestCheckWritableVolumesPropagatesFailures` |
| `7c6e71406` | Bound IAM notification reloads with a fixed batching window | Δ `weed/s3api/auth_credentials.go` | `TestIamReloadCoalescesScheduledBurst` |
| `9373c8576` | Respect bodyless HTTP responses in shared S3 response writer | Δ `weed/s3api/s3api_head_bucket_test.go`, `weed/s3api/s3err/error_handler.go`, `weed/s3api/s3err/error_handler_test.go` | `TestWriteResponseBodySemantics`; `TestWriteNotModifiedResponseHasNoErrorBody` |
| `64888b210` | Reject malformed maintenance arguments instead of reporting success | Δ `weed/shell/command_volume_vacuum.go` | `TestMaintenanceCommandsRejectMalformedArguments` |
| `c02a657a6` | test: qualify mixed-version filers on shared etcd before restore | = (4 paths) | No added Go test declaration |
| `ece528bc4` | test: qualify pinned master binaries through a rolling upgrade | = (1 paths) | `TestMasterRollingBinaryUpgrade` |
| `4f3f37fc5` | test: exercise complete authenticated S3 copy and rename inventory | = (2 paths) | No added Go test declaration |
| `5a2653f01` | test: qualify gateway with a pinned previous-version filer | = (2 paths) | No added Go test declaration |
| `4cc1474e2` | fix(mount): snapshot enumeration metadata without copying file chunks | = (1 paths) | No added Go test declaration |
| `32a01e2f7` | test(mount): bound directory metadata snapshot allocations | = (1 paths) | `TestDirectoryListingUnopenedEntryDoesNotAllocate`; `TestDirectoryListingSnapshotAllocationsDoNotScaleWithChunks` |
| `801c4af40` | Require the directory-size regression in automated CSI metadata suites | = (2 paths) | No added Go test declaration |
| `7ce3b2ae9` | Use local handle metadata for Windows listings outside metadata cache locks | Δ `weed/mount/weedfs_adapter_listing.go` | `TestDirectoryListingUsesLocalHandleMetadata`; `TestAdapterListingCallbackDoesNotHoldMetadataLock`; `TestAdapterListingCachedPageBoundaries` |
| `8d0f07e19` | Reproduce stale directory size after close in ordinary Windows CSI pods | = (2 paths) | `TestDirectoryEnumerationSizeAfterClose` |
| `17f818947` | Qualify the final WinFsp DLL independently with the stock signed driver | = (2 paths) | `TestStockWinFspSelectionNeverStagesPrivateDriver` |
| `ce25e03a1` | fix(windows): invalidate cached namespace after metadata application | = (3 paths) | `TestWinFspAppliedNamespaceInvalidations` |
| `c3b2910f9` | test(lab): reuse matched native WinFsp installation for mixed clients | = (3 paths) | No added Go test declaration |
| `0feca65bf` | fix(windows): restore data caching without retaining cache across close | = (3 paths) | `TestWinFspCacheOptions` |
| `450856580` | test(windows): isolate cached delete lifecycle with NTFS control | = (5 paths) | `TestCachedDeleteRecreate` |
| `5a9af3b39` | test(windows): measure short access samples with QPC and fail fast | = (5 paths) | `TestWindowsPerformanceCounter` |
| `ad2753aec` | test(windows): omit fork-only label flag from vanilla performance mount | = (2 paths) | No added Go test declaration |
| `f1738ae4f` | test: align storage VM SDK with retained-daemon cleanup fix | = (3 paths) | No added Go test declaration |
| `e3841f3d9` | test: initialize crash witnesses after migration reboots | = (1 paths) | No added Go test declaration |
| `9cededde9` | test: require observed volatile-write loss in storage VM crash gates | = (6 paths) | `TestVolatileWitnessRejectsNoLossAndCorruption` |
| `7d4a59316` | test: qualify external etcd snapshot restore with real filer payloads | Δ `README.md` | `TestExternalEtcdSnapshotRestore` |
| `dd2b9ef98` | fix: do not silently discard manual vacuum during another sweep | Δ `weed/server/master_grpc_server_volume.go`, `weed/server/master_server_handlers_admin.go`, `weed/topology/topology_vacuum.go`, `weed/topology/topology_vacuum_concurrency_test.go` | `TestCanceledVolumeVacuumDoesNotReturnSuccess`; `TestVacuumOwnershipCancellationAndAutomaticCoalescing`; `TestManualVacuumDoesNotSilentlySkipBusyTopology` |
| `6dbe6b53e` | test: automate fixed-live-data S3 vacuum soak against packaged binary | = (4 paths) | `TestS3QualificationSoak` |
| `e3324aea8` | fix(filer): retain shared references after uncertain store writes | Δ `weed/filer/filerstore_wrapper.go`, `weed/filer/leveldb2/shared_chunks_test.go` | `TestSharedChunksUncertainWriteKeepsCommittedDestination` |
| `497a32f36` | fix(http): reject invalid partial responses before resuming chunk reads | Δ `weed/util/http/http_global_client_util.go` | `TestRetriedFetchChunkDataRejectsWrongPartialRange`; `TestDirectResumeRejectsInvalidPartialBeforeWriting`; `TestDirectResumePreservesVerifiedShortSubrange` |
| `123669749` | test: qualify packaged S3 payload and multipart preservation | = (5 paths) | `TestMultipartCompleteAndAbortPreservesObject` |
| `9463fd844` | test: automate exact-image volume startup and memory qualification | = (3 paths) | No added Go test declaration |
| `f2646e497` | test: shared CopyObject against weed mini, Harbor commit flow and perf bounds | Δ `weed/filer/leveldb2/shared_chunks_test.go` | `TestS3CopySharesChunks`; `TestS3CopySharedChunksPerformance` |
| `b91b9035d` | s3: share the source's chunks on a same-bucket CopyObject | Δ `weed/command/filer.go`, `weed/command/mini.go`, `weed/command/s3.go`, `weed/command/server.go`, `weed/s3api/s3api_object_handlers_copy.go`, `weed/s3api/s3api_server.go` | `TestShareableCopySource`; `TestCanShareCopyChunks`; `TestSharedCopySourceLinkGivesTheDestinationItsOwnMarker`; `TestIsSharedCopyRefused` |
| `de7a31d84` | leveldb2: stop directory listings at the end of the directory | Δ `weed/filer/leveldb2/leveldb2_store.go` | `TestListStaysInsideTheDirectory` |
| `4a7bb60cd` | Run Windows qualification scripts from the pinned VM module | = (3 paths) | No added Go test declaration |
| `058b8131a` | filer server: keep shared chunks out of update and rename garbage | Δ `weed/server/filer_grpc_server.go`, `weed/server/filer_grpc_server_rename.go` | `TestSharedChunksUpdateEntryGarbageKeepsChunksAnotherLinkUses`; `TestSharedChunksRenameOntoALinkKeepsChunksAnotherLinkUses`; `TestSharedChunksEntryFromAnotherClusterDoesNotJoinAGroup` |
| `e4f8a5c2a` | filer: reference-count chunk lists shared between entries | Δ `weed/filer/filer_delete_entry.go`, `weed/filer/filer_deletion.go`, `weed/filer/filerstore_wrapper.go`, `weed/filer/leveldb2/shared_chunks_test.go` | `TestSharedChunksDeletingSourceKeepsDestination`; `TestSharedChunksDeletingDestinationKeepsSource`; `TestSharedChunksThreeLinksEveryDeleteOrder`; `TestSharedChunksOverwritingSourceKeepsDestination`; `TestSharedChunksOverwritingDestinationKeepsSource`; `TestSharedChunksPerObjectMetadataStaysSeparate`; `TestSharedChunksRenameOfALinkKeepsBothReadable`; `TestSharedChunksEntryDuplicatedAtTwoPathsHoldsTwoReferences`; `TestSharedChunksRecursiveDeleteOfAFolderHoldingOneLink`; `TestSharedChunksReferencesMatchLiveLinks`; `TestSharedChunksCopyInterruptedAtEveryStep`; `TestSharedChunksCopyWithEveryStoreWriteFailing`; `TestSharedChunksDeleteWithEveryStoreWriteFailing`; `TestSharedChunksConcurrentCopiesOfOneSourceAcrossTwoFilers`; `TestSharedChunksConcurrentCopyAndDeleteOfTheSource`; `TestSharedChunksConcurrentOverwriteAndCopyOfTheSource`; `TestSharedChunksConcurrentDeletesOfTwoLinks`; `TestSharedChunksReadCostsNoMoreThanAPlainObject`; `TestSharedChunksRewriteWithoutTheMarkerKeepsMembership`; `TestSharedChunksLinkToAChangedSourceIsRefused`; `TestSharedChunksScanFindsAReferenceLeftByACrash`; `TestSharedChunksGroupDirectoryGoesWithItsLastMember` |
| `7968dd2d5` | util/http: resume whole-chunk reads from where a failed attempt stopped | Δ `weed/util/http/http_global_client_util.go` | `TestRetriedFetchChunkDataResumesAfterPartialRead`; `TestRetriedFetchChunkDataResumeFallsBackToWholeObject`; `TestContentRangeStart` |
| `dcd8a3d76` | s3api: cancel chunk copies with the request and on the first failure | Δ `weed/s3api/s3api_object_handlers_copy.go`, `weed/s3api/s3api_object_handlers_copy_bench_test.go`, `weed/s3api/s3api_object_handlers_rename.go` | `TestCopyChunksConcurrentlyReturnsChunksInOrder`; `TestCopyChunksConcurrentlyCancelsSiblingsOnFirstError`; `TestCopyChunksConcurrentlyHonorsCancelledRequest`; `TestCopyChunksConcurrentlyPropagatesRequestCancellation` |
| `c78b0fc0f` | util/http: bound how much of an unread response CloseResponse drains | Δ `weed/util/http/http_global_client_util.go` | `TestCloseResponseDrainsShortRemainder`; `TestCloseResponseAbandonsLongRemainder`; `TestCloseResponseNilSafe` |
| `9f2b0882a` | s3api: stop allocating an unused chunk-sized buffer per copied chunk | Δ `weed/s3api/s3api_object_handlers_copy.go` | `TestNewChunkUploadOption_NoPayloadBuffer` |
| `8ecd3e03f` | test: keep remote-client construction independent of public DNS | Δ `weed/server/volume_grpc_remote_test.go` | No added Go test declaration |
| `c982a82d4` | test: qualify memory write boundaries and retain isolated lab evidence | = (7 paths) | No added Go test declaration |
| `95f0ee698` | memlimit: give the Go runtime 90% of available memory | = (3 paths) | No added Go test declaration |
| `e8187ed72` | memlimit: resolve available memory from the cgroup hierarchy like the JVM | Δ `weed/command/volume.go` | `TestApplyMemoryLimitsV1ParentSlice`; `TestApplyMemoryLimitsUnlimitedCgroupUsesPhysicalRAM`; `TestApplyMemoryLimitsUnknownMemory`; `TestAvailableMemoryV1SliceOnOldKernel`; `TestAvailableMemoryV2PrivateNamespace`; `TestAvailableMemoryV2HostNamespace`; `TestAvailableMemoryV1MountRootStripped`; `TestAvailableMemoryV1CombinedControllers`; `TestAvailableMemoryLimitAbovePhysical`; `TestAvailableMemoryNoLimitIsPhysical`; `TestAvailableMemoryHybridPrefersV1Memory`; `TestAvailableMemoryWithoutCgroupFiles`; `TestAvailableMemoryRejectsGarbageLimit`; `TestUnescapeMountPath`; `TestPlanVolumeMemoryUnknownAvailable` |
| `aadb8a658` | test: cover detached upload bodies and the small-record WriteAt limit | = (2 paths) | `TestUploadPayloadIsTheCallersAfterReturn`; `TestUploadBodiesFailReadsAfterReturn`; `TestAppendWriteCountAroundSmallRecordLimit` |
| `c595977f3` | needle: write records with payloads under 64 KiB in one WriteAt | = (1 paths) | No added Go test declaration |
| `1e72f3704` | operation: never read the caller's payload after an upload returns | Δ `weed/operation/upload_content.go` | No added Go test declaration |
| `0965a30d8` | needle: write the payload from n.Data instead of copying it into the record buffer | = (7 paths) | `TestAppendWritesLegacyRecord`; `TestAppendTruncatesOnFailedWrite`; `TestAppendDoesNotCopyPayload` |
| `2d61f3a2a` | volume: derive GOMEMLIMIT and transfer admission from the cgroup limit | Δ `weed/command/volume.go` | `TestApplyMemoryLimitsDerivesFromCgroup`; `TestApplyMemoryLimitsKeepsExplicitSettings`; `TestApplyMemoryLimitsUnlimitedContainer` (historical; see net-baseline note); `TestCgroupMemoryLimit` (historical; see net-baseline note); `TestPlanVolumeMemoryFromCgroup`; `TestPlanVolumeMemoryRespectsGOMEMLIMITEnv`; `TestPlanVolumeMemoryExplicitFlagsOverride`; `TestPlanVolumeMemoryWithoutAnyLimit` (historical; see net-baseline note); `TestPlanVolumeMemorySmallContainerKeepsAQuarter` |
| `d9e83142c` | operation: stream multipart uploads instead of copying the payload | Δ `weed/operation/upload_content.go`, `weed/topology/store_replicate.go` | `TestReplicationUploadSendsExactLegacyBody`; `TestReplicationUploadDoesNotCopyPayload`; `TestReplicationUploadsInFlightHoldNoPayloadCopies` |
| `a15acbfd8` | wip: vanilla 4.47 interval and needle-map recovery performance tests | = (2 paths) | `TestIntervalHistoryMatchesByteOracle` |
| `de1573d7a` | wip: buffered write performance test for the ENOSPC benchmark | = (1 paths) | No added Go test declaration |
| `38953cc24` | test(winfsp): prevent user-mode root identity disclosure | = (1 paths) | No added Go test declaration |
| `36179071e` | test(winfsp): verify projected metadata handles forward volume controls | = (1 paths) | `TestWindowsProjectedWinFspControl` |
| `1740b8037` | test: retain process-scoped ETW reparse diagnostics | = (2 paths) | No added Go test declaration |
| `35576f928` | test(winfsp): prove external symlink fixture by handle filesystem identity | = (2 paths) | `TestCrossVolumeFixtureIdentity` |
| `62ec6d053` | test(winfsp): preserve evidence across repeated persistence verification | = (7 paths) | `TestPersistenceVerifyPreservesFixtures`; `TestWindowsAttributesPersistenceRepeatVerify`; `TestWindowsPermissionsPersistenceRepeatVerify` |
| `5a21ae355` | fix(mount): preserve filer filename-policy errno after RPC decoding | = (2 paths) | `TestFilenamePolicyErrnoAcrossRPC` |
| `be9ae79e5` | test(windows): configure CSI identity and isolate DLL probe | = (4 paths) | `TestIsolatedNativeInventorySeparatesCandidateDLL` |
| `59ce0721f` | test: scope canonical Windows mounted oracles to CSI filer namespaces | = (5 paths) | `TestFilerRootScopesPersistenceOracle`; `TestScopedFilerPathPreservesNamespace` |
| `8e96856c1` | mount: invalidate Linux name caches after remote namespace changes | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go`, `weed/mount/weedfs_kernel_entry_invalidation_test.go` | `TestForeignRenameExpiresBothKernelNames`; `TestKernelEntryNotificationOnlyForForeignNamespaceChanges`; `TestCrossDirectoryRenameNotifiesAfterOpenHandleMoves` |
| `614b27b6e` | test(lab): identify the stale rename source in mixed CSI failures | = (2 paths) | `TestMixedOracleNamesStaleRenameSource` |
| `42105b8de` | test(mount): distinguish POSIX mode checks from Windows directory creation | = (1 paths) | No added Go test declaration |
| `b9f9b128b` | ci: include page writer regressions in the storage race gate | = (1 paths) | No added Go test declaration |
| `b835c7736` | mount: fail uploads when buffered swap content cannot be read completely | = (3 paths) | `TestSwapSaveContentReportsReadFailure` |
| `9beaf26d3` | mount: propagate swap buffering failures instead of acknowledging lost bytes | = (8 paths) | `TestLaterChunkAllocationFailurePoisonsHandle`; `TestUploadPipelinePreservesPartialWriteError`; `TestUploadPipelineReturnsSwapWriteError` |
| `2c1906262` | mount: preserve metadata when buffered writes are rejected | = (2 paths) | `TestAcceptedWriteUpdatesEntryAndDirtyBytes`; `TestRejectedWriteDoesNotMutateEntry`; `TestFsyncRetainsUploadFailure` |
| `e2aedefb0` | Require working same-volume symlinks without deleting their targets | = (1 paths) | `TestSymlinkUnlinkPreservesTarget` |
| `ca0d069cd` | Synchronize directory-symlink kernel patch with native-tested WinFsp fork | = (1 paths) | No added Go test declaration |
| `86120dbf3` | Bind native WinFsp lab packages to exact source archives | = (2 paths) | `TestNativeWinFspSourceArchives` |
| `36b372458` | Run existing Windows mounted suites against matched native WinFsp driver packages | = (4 paths) | `TestNativeWinFspManifestRejectsMismatchedArtifacts`; `TestNativeWinFspManifestPairing` |
| `141fd7507` | Reproduce directory-mounted absolute symlink failures and retain candidate kernel patch | = (2 paths) | No added Go test declaration |
| `72d5dcbd2` | test(windows): retire blanket reparse exclusion after real VM qualification | Δ `README.md` | No added Go test declaration |
| `2a6bb1f57` | test(windows): distinguish junction aliases from canonical symlink targets | = (1 paths) | No added Go test declaration |
| `eb11ac5d0` | docs(lab): specify strict Windows conformance inputs and scope | = (1 paths) | No added Go test declaration |
| `84da80e8d` | test(lab): document paired Windows latency gate and trigger conformance checks | = (2 paths) | No added Go test declaration |
| `66d0df978` | fix(windows): enable volume-relative symlinks and require conformance evidence | Δ `README.md` | No added Go test declaration |
| `2ec5fcef9` | test(lab): reject access metrics from failed or incomplete VM runs | = (2 paths) | No added Go test declaration |
| `1f9479556` | test(windows): distinguish absolute and relative symlink targets | = (2 paths) | `TestWindowsSymlinkTargets` |
| `c4be16242` | fix(lab): require performance-specific Windows completion evidence | = (2 paths) | `TestWindowsScenarioEvidence` |
| `ec4ad7883` | ci(windows): retain real Git LFS campaigns for both permission policies | = (1 paths) | No added Go test declaration |
| `0662e3ec0` | test(windows): qualify legacy and enforced permission policies explicitly | Δ `README.md` | `TestWindowsLegacyPermissionCompatibility`; `TestWinFspBasicOptionsRejectPermissionOverrides` |
| `32f1bdcca` | fix(windows): add opt-in basic permissions with scoped SYSTEM create mapping | Δ `weed/command/mount.go`, `weed/mount/weedfs.go` | No added Go test declaration |
| `be63a915e` | test(windows): separate explicit create security from token default permissions | = (4 paths) | `TestWindowsCreateSecurity` |
| `98757843c` | test(windows): preserve native token and filer-mode security diagnostics | = (1 paths) | No added Go test declaration |
| `f703b025c` | test(windows): retain timing validation with non-UTF8 PowerShell diagnostics | = (2 paths) | No added Go test declaration |
| `25f4898f8` | test(windows): automate access latency comparisons and remount permission checks | = (6 paths) | `TestWindowsPermissionsPersistence` |
| `ddb66afe7` | test(windows): capture native access denial and validated access timings before permission fix | = (2 paths) | `TestWindowsBasicAccessDenial`; `TestWindowsAccessPerformance` |
| `00afc2ac2` | fix(windows): persist native metadata and support Unicode and NFS reparse entries | Δ `weed/command/scaffold/filer.toml`, `weed/mount/wfs_save.go` | `TestWindowsFlagsEncoding` |
| `0f2583d56` | test(filer): reproduce creation-time precision loss through storage | = (1 paths) | `TestCreationTimePrecisionSurvivesStorageCodec`; `TestCreationTimeNanosecondsParticipateInEquality` |
| `0cd1075aa` | test(storage): recover retained crash VMs before approved link reconciliation | = (1 paths) | No added Go test declaration |
| `681a1052d` | test(windows): retain native NFS reparse round-trip reproducer | = (1 paths) | `TestWindowsNFSReparseRoundTrip` |
| `0570c88ff` | test(windows): reproduce creation time changing with modification time | = (2 paths) | `TestWindowsCreationTimeStable` |
| `4e64697fe` | test(windows): cover UTF-16 filename boundaries without changing Unix limits | = (2 paths) | `TestWindowsUnicodeComponentLimits`; `TestCheckNamePlatformUnits` |
| `ffacc268d` | test(windows): assert mutable attributes and fresh-remount persistence | = (2 paths) | `TestWindowsAttributesRoundTrip`; `TestWindowsAttributesPersistence` |
| `3e0684655` | test(windows): retain bounded native probes and run adapter coverage in VM gates | = (7 paths) | `TestWinfspGetpathResolvesCaseThroughRealMetadataCache`; `TestWinfspGetpathPreservesExactHardLinkAlias` |
| `de419ec4f` | fix(windows): preserve canonical names in the active WinFsp adapter | = (1 paths) | No added Go test declaration |
| `ff8c88dbf` | test(windows): reproduce active adapter metadata defects before fixing them | = (5 paths) | `TestWindowsAdapterLab`; `TestDefaultFileAttributesArchive`; `TestDirectoryChangeNotificationPreservesCase`; `TestAttrToStatWindowsArchiveFlags`; `TestWinfspGetpathReportsVolumeRelativeCanonicalName` |
| `61e924e84` | test(windows): run complete native VM suites and preserve conformance failures | = (7 paths) | No added Go test declaration |
| `eeeffdfed` | fix(storage): replay durable index tails regardless of diagnostic log mtime | = (6 paths) | `TestIndexPerformanceGate`; `TestIndexRecoveryPerformanceLab`; `TestLevelDbReplaysDurableIndexDespiteNewerLog`; `TestLevelDbReplayPreservesRepeatedKeyOrder`; `TestLevelDbReplaySparseKeysAcrossBatches` |
| `c0674ba11` | test(lab): qualify immutable Windows mappings without barrier collisions | = (2 paths) | No added Go test declaration |
| `df9a48f29` | test(lab): keep vacuum crash windows open with incompressible data | = (2 paths) | `TestVacuumPayloadCannotCollapseUnderCompression` |
| `b4ec400d4` | test(lab): require mixed cache coherence and retain crash diagnostics | = (5 paths) | `TestMixedOpenDescriptorCoherence` |
| `b47972789` | fix(mount): invalidate foreign content after handle refresh | Δ `weed/mount/meta_cache/meta_cache.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go` | `TestForeignSameMtimeContentChangeInvalidatesKernelData`; `TestEntryListenerSeesRefreshedOpenHandle` |
| `18cc7b1e0` | fix(mount): order overwrites after observed chunk versions | Δ `weed/mount/page_writer_clock_test.go` | `TestChunkGroupWriteFloorResolvesManifest`; `TestChunkGroupWriteFloorRejectsUnresolvedManifest`; `TestWriteAfterFutureChunkSurvivesCompaction`; `TestPageWriterRejectsTimestampExhaustion` |
| `3d91b405b` | test(mount): reproduce mixed-OS stale descriptor and mmap reads | = (5 paths) | No added Go test declaration |
| `ad1672e6d` | fix(mount): make sequential interval tracking linear and repair upload oracle | = (6 paths) | `TestIntervalPerformanceGateRejectsQuadraticAndMissingSamples`; `TestIntervalPerformanceLab`; `TestIntervalHistoryMatchesByteOracle`; `TestUploadPipelineRangeHelpersCoverDeclaredBytes` |
| `aa1631e0a` | fix(mount): expire Linux file attributes after foreign metadata updates | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_kernel_attr_invalidation_test.go` | `TestKernelAttributeNotificationPreservesDirtyWrites`; `TestForeignUpdateInvalidatesKernelFileAttributes`; `TestSelfUpdateDoesNotInvalidateKernelFile` |
| `5f9b2cf94` | Preserve opt-in FUSE tracing for mixed mount stale-read investigation | = (1 paths) | No added Go test declaration |
| `8ef76f86f` | Pin VM SDK to merged Labcontainers 60025bf | = (2 paths) | No added Go test declaration |
| `21e8bb1b2` | Add isolated mixed Windows Linux mount gate and retain stale-read reproductions | Δ `README.md` | `TestMixedOSMountLab`; `TestMixedGuestClock`; `TestMixedLifecycle`; `TestMixedOracleRejectsDamage`; `TestMixedOracleRejectsLostDelete` |
| `785dce8a3` | Record hosted Linux qualification success | = (1 paths) | No added Go test declaration |
| `6c1b1dae2` | Record fresh Linux migration and admission qualification evidence | = (1 paths) | No added Go test declaration |
| `db45439d3` | Fix Linux CI harness exit propagation and bounded Bubblewrap compatibility | = (8 paths) | No added Go test declaration |
| `d711e7e38` | Test xattr platform contracts without skipping unlinked metadata coverage | = (3 paths) | `TestWindowsMountXAttrLab` |
| `b298b27fa` | Fix crash harness readiness sharing and finish native MSVC runtime gates | = (2 paths) | No added Go test declaration |
| `3132c4f2a` | Record native MSVC mount cleanup crash and LFS qualification results | = (1 paths) | No added Go test declaration |
| `0340f0cd4` | Operationalize offline MSVC WinFsp builds and exact-DLL qualification | Δ `README.md` | `TestWindowsWinFspMSVCBuildLab` (historical; see net-baseline note) |
| `441dab6a2` | docs(storage-lab): pin supplied Windows compiler payload | = (1 paths) | No added Go test declaration |
| `4512ff379` | build(storage-lab): bind provenance to captured patch and recipe | = (3 paths) | No added Go test declaration |
| `a27106fd0` | test(storage-lab): enforce exact MSVC build arguments | = (2 paths) | No added Go test declaration |
| `fa1f55c97` | docs(storage-lab): record current-build Windows LFS pass | = (1 paths) | No added Go test declaration |
| `a15e45612` | ci(storage-lab): support pinned standalone Windows LFS clients | Δ `README.md` | No added Go test declaration |
| `32a9bcb36` | docs(storage-lab): record alternate MSVC catalog checks | = (1 paths) | No added Go test declaration |
| `79936b2ea` | docs(storage-lab): retain compiler catalog integrity failure | = (1 paths) | No added Go test declaration |
| `318806499` | docs(storage-lab): record Windows compiler inventory | = (1 paths) | No added Go test declaration |
| `93c4f20b6` | build(storage-lab): preserve scoped WinFsp MSVC DLL recipe | = (4 paths) | No added Go test declaration |
| `a526cf6f8` | test(storage-lab): validate native WinFsp build provenance | = (2 paths) | No added Go test declaration |
| `9f194af18` | docs: record hardened FSD rollback and release build requirements | = (1 paths) | No added Go test declaration |
| `4cc3f07fa` | test: quiesce timed-out mounts before restoring rollback hooks | = (4 paths) | `TestLabMountQuiescence` |
| `62d7ef026` | test: exercise exact-candidate WinFsp registration rollback | = (6 paths) | `TestLabMappedPE`; `TestMountManagerRegistrationRollback` |
| `a7aee1bc7` | test: add scoped lab DLL import interception and restoration | = (2 paths) | `TestMappedImportSlotCandidate` |
| `a2c91961f` | test: validate PE64 imports for scoped rollback fault injection | = (2 paths) | `TestMappedImportSlot` |
| `71306c77d` | test: transfer Windows diagnostic logs without QGA truncation | = (3 paths) | `TestWindowsGuestLogsRejectCollectionFailure`; `TestWindowsGuestLogsExceedExecOutputLimit` |
| `5f6e590fa` | docs: record guarded 20-cycle Git LFS candidate pass | = (1 paths) | No added Go test declaration |
| `68d324fae` | docs: record both hardened crash modes and rollback gap | = (1 paths) | No added Go test declaration |
| `bfdf4c3b6` | docs: record ownership-hardened FSD crash qualification | = (1 paths) | No added Go test declaration |
| `7013719a3` | ci: expose intensive Windows mount cleanup qualification | = (2 paths) | No added Go test declaration |
| `b6f86af87` | docs: record default WinFsp mapping cleanup qualification | = (1 paths) | No added Go test declaration |
| `594ac1616` | test: require ownership proof for WinFsp crash child paths | = (4 paths) | `TestCrashChildOwnership` |
| `9a742be90` | test: qualify WinFsp mount cleanup after owned process termination | = (4 paths) | `TestMountManagerProcessCrash` |
| `d87a6a0b9` | test: verify WinFsp mount mapping removal after unmount | = (3 paths) | No added Go test declaration |
| `6988c2cc9` | docs: retain exploratory LFS pass with provenance limitations | = (1 paths) | No added Go test declaration |
| `8508ba143` | test: verify WinFsp same-path remount and junction cleanup | = (3 paths) | No added Go test declaration |
| `2184bef7f` | docs: record alternate WinFsp registration qualification | = (1 paths) | No added Go test declaration |
| `cd106ad0e` | test: cover both WinFsp candidate registration paths | = (3 paths) | `TestWindowsMountModeIsolation` |
| `3bdf75f85` | docs: pin corrected Windows qualification transport image | = (1 paths) | No added Go test declaration |
| `639df06d1` | test: reject stale Windows setup and workload responses | = (3 paths) | `TestWindowsSetupCompletion` |
| `c4c1287ff` | docs: record matched WinFsp source-fix native RED and GREEN | = (1 paths) | No added Go test declaration |
| `5899b5551` | test: verify WinFsp candidate loading in real Git LFS workloads | = (4 paths) | No added Go test declaration |
| `dd558840c` | test: preserve reproducible WinFsp core-fix lab candidates | = (5 paths) | `TestWinFspLabManifest` |
| `0627b93ea` | test: build and verify app-local WinFsp DLL regression candidates | = (5 paths) | No added Go test declaration |
| `33d82a58f` | build: pin published SDK guest transfer validation fix | = (3 paths) | No added Go test declaration |
| `e5b6e3680` | docs: record controlled GUID-junction pass against failing NT control | = (1 paths) | No added Go test declaration |
| `8015171d2` | docs: record matched NT-junction control failure | = (1 paths) | No added Go test declaration |
| `cf7611537` | docs: distinguish exploratory GUID result from controlled tests | = (1 paths) | No added Go test declaration |
| `e12890c4f` | test: compare GUID junctions with matched NT-target controls | = (3 paths) | No added Go test declaration |
| `ec4ca6986` | docs: record mount mapping failure with driver-side registration | = (1 paths) | No added Go test declaration |
| `8fe43ae0c` | test: isolate WinFsp registration mode and retain input provenance | = (2 paths) | No added Go test declaration |
| `1553e3c8b` | build: pin published native Labcontainers SDK | = (3 paths) | No added Go test declaration |
| `de226aaec` | docs: record Git-free mount-manager failure reproduction | = (1 paths) | No added Go test declaration |
| `391ead862` | test: record Git LFS 3.8 failure and extend mount observation | = (2 paths) | No added Go test declaration |
| `53bb34510` | test: isolate repeated WinFsp directory mount path resolution | = (4 paths) | `TestMountManagerDirectoryLifecycle` |
| `82a8c24f5` | docs: record official Git LFS 3.8 comparison results | = (1 paths) | No added Go test declaration |
| `fa1be830c` | test: retain mount mapping evidence in untraced stress runs | = (3 paths) | No added Go test declaration |
| `ada17b336` | test: retain live mount mappings after LFS path failure | = (3 paths) | No added Go test declaration |
| `3020cabd0` | docs: record reproduced Windows DOS-path translation failure | = (1 paths) | No added Go test declaration |
| `acadc87fa` | test: preserve Git hard links when overriding lab LFS client | = (5 paths) | No added Go test declaration |
| `fe200f6fb` | test: retain Windows dependency installation stage logs | = (2 paths) | No added Go test declaration |
| `0128ed10e` | test: preserve Windows ETW and Git LFS path diagnostics | = (11 paths) | `TestWindowsArtifactMultiChunk`; `TestWindowsArtifactRejectsIncompleteEvidence` |
| `2c84a707b` | Use generated cloud-init objects for storage lab guest networking | = (3 paths) | No added Go test declaration |
| `93d930cb3` | Migrate storage labs to native topology and approved recovery | Δ `README.md` | `TestRecoveryRejectsUnrelatedNativeImpact` |
| `4155dab74` | docs: record Windows canonicalization probe results and open qualification blocker | = (1 paths) | No added Go test declaration |
| `671054225` | test: exercise Git LFS Windows directory canonicalization in native gate | = (3 paths) | `TestGitLfsDirectoryCanonicalization` |
| `2ef9c71d1` | test: retain failed Windows qualification and trace path lookup failures | = (2 paths) | No added Go test declaration |
| `d67fdbb5e` | test: trace and target Windows LFS path-resolution failures | = (4 paths) | No added Go test declaration |
| `e6745c458` | mount: preserve live entries on equal-version in-place updates | Δ `weed/mount/meta_cache/meta_cache.go` | `TestInPlaceDirectoryUpdatePreservesEntryAndChildren`; `TestSaveEntryNoChangeAckPreservesCaseFoldListing` |
| `b09ea7b1c` | filer: preserve acknowledgments for unlogged internal renames | Δ `weed/server/filer_grpc_server_rename.go`, `weed/server/filer_grpc_server_rename_ack_test.go` | `TestStreamRenameAcknowledgesUnloggedPath` |
| `5efa486b0` | test: automate repeated Windows Git and LFS rename qualification | = (6 paths) | `TestGitLfsObjectRename` |
| `3cd359f40` | filer: acknowledge streaming renames with committed metadata events | Δ `weed/server/filer_grpc_server_rename.go`, `weed/server/filer_grpc_server_rename_ack_test.go` | `TestStreamRenameRepliesMatchCommittedLog`; `TestStreamRenameAckSurvivesClockBehindMetadataFence`; `TestStreamRenameDoesNotAcknowledgeFailedCommit` |
| `54d8b360d` | Automate isolated Windows mount qualification with fail-closed LFS checks | Δ `README.md` | `TestWindowsMountLab` |
| `3545cb822` | Require complete regression inventories for storage lab qualification | = (4 paths) | No added Go test declaration |
| `174c6d5b4` | Record verified filesystem crash recovery and automate extended VM gates | Δ `README.md` | No added Go test declaration |
| `f4dddb974` | Run native Windows storage tests and use explicit crash timelines | Δ `README.md` | `TestWindowsStorageLab` |
| `a7e55fb3d` | storage: gate reclamation and upgrades with large-disk regressions | Δ `README.md`, `weed/server/master_grpc_server_volume.go`, `weed/shell/command_volume_fsck.go`, `weed/shell/command_volume_fsck_stale_index_test.go`, `weed/storage/volume_checking_test.go`, `weed/storage/volume_read.go`, `weed/storage/volume_read_test.go`, `weed/storage/volume_vacuum.go`, `weed/topology/topology.go`, `weed/topology/volume_layout.go`, `weed/topology/volume_layout_replica_size_test.go` | `TestVolumeBinaryUpgradeVacuumRollback`; `TestUploadLimitTimeoutIncludesReplication`; `TestDuTraversePropagatesNestedFailure`; `TestFsckRejectsIncompleteNeedleMetadata`; `TestReadNeedleMetaRejectsDifferentNeedleAtCopiedOffset`; `TestVacuumStableLiveDatasetHasBoundedGrowth`; `TestMakeupDiffRelocatesLargeOffsets`; `TestCommitCompactReportsReplayFailure`; `TestCompactAbortsOnSourceSyncFailure`; `TestGrowth020AcrossFourRacks`; `TestReplicaSizeAccountingDuringRollingCompaction` |
| `9ec822e2d` | shell: volume.fsck skips a needle whose copied index went stale | Δ `weed/shell/command_volume_fsck.go`, `weed/shell/command_volume_fsck_stale_index_test.go` | `TestFsckSkipsNeedlesWhoseIndexWentStale` (historical; see net-baseline note) |
| `973b60e63` | s3: send an exact Content-Length on streamed chunk copies | Δ `weed/s3api/s3api_object_handlers_copy_stream.go`, `weed/s3api/s3api_object_handlers_copy_stream_test.go` | `TestStreamCopyChunkRange_DestinationPostCarriesContentLength`; `TestStreamCopyChunkRange_GzipPassThroughCarriesCompressedLength`; `TestStreamCopyChunkRange_UnsizedSourceStillCopies` |
| `b58569600` | shell: s3.bucket.emptyfolders views or sets a bucket's empty-folder policy | = (2 paths) | `TestBucketEmptyFolderPolicy`; `TestBucketEmptyFolderPolicy_roundTripKeepsOtherAttributes` |
| `08e95eca8` | test: keep the root xattr test off platforms without xattr handlers | Δ `weed/mount/weedfs_file_mkrm_test.go` | `TestRootSetXAttrKeepsBucketAttributes` |
| `ae8349b96` | filer: keep bucket directories unless a bucket opts in to empty-folder cleanup | Δ `weed/filer/empty_folder_cleanup/empty_folder_cleaner.go`, `weed/filer/empty_folder_cleanup/empty_folder_cleaner_test.go`, `weed/filer/filer_notify.go`, `weed/filer/filer_on_meta_event.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_file_mkrm_test.go`, `weed/mount/wfs_save.go`, `weed/s3api/s3api_bucket_handlers.go` | `TestEnsureMountRoot_leavesExistingRootUntouched`; `TestEnsureMountRoot_createsMissingBucketWithKeepFoldersPolicy`; `TestEnsureMountRoot_createsMissingNonBucketRootWithoutPolicy`; `TestSetBucketAllowEmptyFolders_roundTrip`; `TestEmptyFolderCleaner_executeCleanup_missingPolicyKeepsFolder`; `TestEmptyFolderCleaner_OnBucketPolicyUpdate_invalidatesCachedPolicy`; `TestRootTouchPreservesBucketAttributes`; `TestRootSetXAttrKeepsBucketAttributes` (historical; see net-baseline note) |
| `24750d208` | volume: reserve concurrent transfer bytes atomically | Δ `weed/server/volume_server_handlers_read.go`, `weed/storage/store.go`, `weed/storage/store_ec.go`, `weed/storage/volume_read.go` | `TestTryReserveDataSizeIsAtomic`; `TestTryReserveDataSizeAllowsOneOversizedRequest` |
| `14d423946` | topology: a master that does not know a volume is not under-replication | Δ `weed/topology/store_replicate.go` | No added Go test declaration |
| `1323cc333` | s3: make the empty-directory marker opt-in, it is not S3 behaviour | Δ `weed/command/s3.go`, `weed/s3api/s3api_object_handlers_list.go`, `weed/s3api/s3api_object_handlers_list_directory_test.go`, `weed/s3api/s3api_server.go` | No added Go test declaration |
| `70fcc5afa` | masterclient: never call OnPeerUpdate from the receive loop | Δ `weed/wdclient/masterclient.go` | No added Go test declaration |
| `83685e78c` | tests: reproduce the three defects behind the 2026-08-24 harbor outage | = (3 paths) | `TestEmptiedDirectoryIsNotListedAsKeyByDefault`; `TestListedKeysAlwaysHaveANonEmptyBasename`; `TestDirectoryKeyObjectStillListed`; `TestNotFoundFromMasterIsNotReportedAsUnderReplication`; `TestStuckPeerUpdateDoesNotStallVolumeLocationUpdates` |
| `358f47b1f` | storage: fail the volume load on an open error instead of segfaulting | Δ `weed/storage/volume_loading.go` | No added Go test declaration |
| `c66909247` | hack: run winfsp conformance on a drive letter; exclude two out-of-model tests | = (1 paths) | No added Go test declaration |
| `89061fb93` | mount: pass a bare drive letter to WinFsp verbatim instead of mangling it | = (1 paths) | No added Go test declaration |
| `7d911f93b` | mount: stamp nanoseconds on created entries so mtime/atime aren't truncated | Δ `weed/mount/weedfs_dir_mkrm.go`, `weed/mount/weedfs_file_mkrm.go`, `weed/mount/weedfs_symlink.go` | No added Go test declaration |
| `3025265b0` | filer: pass the new CacheInvalidator arg in fork-added tests | = (4 paths) | No added Go test declaration |
| `041babac5` | ci: run on merge/** branches and gate weed/s3api | = (1 paths) | No added Go test declaration |
| `2a09a60cf` | filer: regression test for the lastChunkFid eviction race | = (1 paths) | `TestConcurrentChunkTransitionsDoNotEvictEachOther`; `TestLastChunkFidConcurrentAccessIsRaceFree`; `TestChunkTransitionDoesNotEvictPreviousChunk` |
| `63cbcd23b` | s3: reload identities from -s3.config without a restart | Δ `weed/command/s3.go`, `weed/s3api/auth_credentials.go`, `weed/s3api/s3api_server.go` | `TestSighupReloadPathAlsoRemovesIdentities`; `TestFileReloadDoesNotRemoveNonFileStaticIdentities`; `TestStaticConfigWatcherDisabledWithZeroInterval`; `TestStaticConfigWatcherIgnoresEmptyPath`; `TestSighupReloadKeepsIdentitiesOnMalformedFile`; `TestFileReloadRemovalIsScopedToTheRemovedName`; `TestStaticConfigWatcherLoadsAddedIdentity`; `TestStaticConfigWatcherRemovesDeletedIdentity`; `TestStaticConfigWatcherRotatesCredentials`; `TestStaticConfigWatcherKeepsPreviousConfigOnMalformedFile`; `TestStaticConfigWatcherKubernetesSymlinkSwap`; `TestStaticConfigWatcherReloadWithConcurrentLookups`; `TestStaticConfigReloadKeepsDynamicIdentities` |
| `a7a1fcc3e` | gitignore the windows build and debug artifacts | Δ `.gitignore` | No added Go test declaration |
| `7618253da` | test: cover fork-specific winfsp, readerCacheMode and local-socket behavior | = (5 paths) | `TestParseReaderCacheModeAcceptsKnownModes`; `TestParseReaderCacheModeFailsClosed`; `TestReaderCacheModeFlagDefaultParses`; `TestReaderCacheModeReachesSectionReader`; `TestReaderCacheModeAutoStillInfers`; `TestNewChunkGroupDefaultsToAuto`; `TestUnknownReaderCacheModeBehavesAsAuto`; `TestAttrToStatMapsEveryField`; `TestAttrToStatSynthesizesBirthTimeFromCtime`; `TestAttrToStatDefaultsZeroBlksize`; `TestAttrToStatOverwritesReusedBuffer`; `TestWinfspFSFullPath`; `TestWinfspFSFullPathDoesNotDoubleSlashAtRoot`; `TestInodeFromFhRejectsSentinel`; `TestServeGrpcOnLocalSocketFallsBackToTCPWhenBindFails`; `TestServeGrpcOnLocalSocketKeepsRegistrationWhenBindSucceeds`; `TestUnregisterLocalGrpcSocketClearsHostMatching` |
| `1a477a96e` | mount: refresh parent cache after mkdir collision | Δ `weed/mount/weedfs_dir_mkrm.go`, `weed/mount/weedfs_file_mkrm_test.go` | `TestMkdirExistingDirectoryInvalidatesStaleNegativeCache` |
| `40e625256` | ci: exclude unsupported WinFsp long paths | = (1 paths) | No added Go test declaration |
| `5e8ec026d` | ci: exclude unsupported WinFsp ACL semantics | = (1 paths) | No added Go test declaration |
| `eba842951` | ci: exclude normalized-path WinFsp create cases | = (1 paths) | No added Go test declaration |
| `db646f9c1` | mount: create directories exclusively | Δ `weed/mount/weedfs_dir_mkrm.go`, `weed/mount/weedfs_file_mkrm_test.go` | `TestMkdirUsesExclusiveCreate` |
| `dac78f6ff` | ci: run WinFsp compatibility in case-sensitive mode | = (1 paths) | No added Go test declaration |
| `d7c1538ee` | filer: promote random chunks only after reuse | = (2 paths) | `TestRandomCachePromotesOnlyAfterReuse`; `TestRandomCachePerformanceCharacterization` |
| `5fa7a285b` | mount: preserve deferred creates across cache eviction | Δ `weed/mount/weedfs.go`, `weed/mount/weedfs_file_mkrm_test.go` | `TestLookupDeferredCreateSurvivesMetadataCacheEviction` |
| `4cb8ee3b8` | mount: enforce case-insensitive WinFsp mutations | = (4 paths) | `TestWinFspMutationUsesExistingCanonicalName` |
| `1956dcc2e` | ci: load WinFsp runtime for compatibility tests | = (1 paths) | No added Go test declaration |
| `748d49880` | mount: list case-folded WinFsp directories canonically | = (4 paths) | `TestWinFspDirectoryListingUsesCanonicalPathWhenCaseInsensitive` |
| `534a35892` | mount: resolve WinFsp paths case-insensitively | = (4 paths) | `TestFindWinFspName` |
| `56f1a32b9` | mount: default WinFsp volumes to case-insensitive | Δ `weed/command/mount.go` | `TestWinFspCaseSensitivityDefaultsToInsensitive`; `TestWinFspCaseSensitivityCanBeEnabled` |
| `04446e262` | test: real-mount mmap correctness and throughput-floor coverage | = (3 paths) | `TestMmapThroughputFloor`; `TestMmapReadSequential`; `TestMmapReadReverse`; `TestMmapReadShuffled`; `TestMmapReadConcurrent`; `TestMmapReadWithExplicitSequentialMode`; `TestReaderAtScenarioRequestCounts` |
| `2488c607c` | mount: tolerant windowed sequential detection in WinFsp read-ahead | = (2 paths) | `TestReadAheadToleratesNearbyOutOfOrderOffset`; `TestReadAheadStillBypassesFarOutOfOrderOffset` |
| `58c13531d` | mount: set FOPEN_KEEP_CACHE on first open, not just repeat opens | Δ `weed/mount/weedfs_file_io.go` | No added Go test declaration |
| `8cad270c0` | mount: add -readerCacheMode override for the sequential/random detector | Δ `weed/command/mount.go`, `weed/mount/filehandle.go`, `weed/mount/weedfs.go` | No added Go test declaration |
| `3f4c8a188` | filer: fix false-random misclassification and chunk-cache bypass for concurrent/mmap reads | = (4 paths) | `TestRandomModeStillCachesWholeChunk`; `TestMonitorReadAt_StrictSequential`; `TestMonitorReadAt_WindowedNonContiguous`; `TestMonitorReadAt_ConcurrentOutOfOrderCompletion`; `TestMonitorReadAt_ConcurrentOutOfOrderCompletion_RealGoroutines`; `TestMonitorReadAt_GenuinelyRandom` |
| `8ed69ce48` | docs(mount): clarify flush transaction boundary | Δ `weed/mount/weedfs_file_sync.go` | No added Go test declaration |
| `2817e7c2b` | fix(mount): serialize deferred create flushes | Δ `weed/mount/weedfs_file_mkrm_test.go`, `weed/mount/weedfs_file_sync.go` | `TestConcurrentFlushCommitsDeferredCreateOnce` |
| `f722861bd` | test: summarize WinFsp operations without tracing | = (6 paths) | No added Go test declaration |
| `6bb1a808c` | test: trace seeded WinFsp rename failure | = (4 paths) | No added Go test declaration |
| `ab0a03df4` | test: require local rename cache notification | = (1 paths) | No added Go test declaration |
| `98123f75b` | test: deterministically exercise WinFsp rename race | = (4 paths) | No added Go test declaration |
| `dc555d3da` | test: establish untraced Git rename failure rate | = (1 paths) | No added Go test declaration |
| `3e80974e0` | test: repeat Git rename case under ETW | = (2 paths) | No added Go test declaration |
| `8eaffe8bb` | test: trace the exact warmed Git rename workload | = (1 paths) | No added Go test declaration |
| `c9ce7437f` | test: isolate and ETW trace Git rename regression | = (2 paths) | No added Go test declaration |
| `555947ee9` | test: capture WinFsp operation trace for mount regressions | = (2 paths) | No added Go test declaration |
| `7dc8cd03b` | test: reproduce Git atomic rename failure on WinFsp | = (1 paths) | No added Go test declaration |
| `a9b4de6a7` | readme: windows mount support, large-disk build | Δ `README.md` | No added Go test declaration |
| `1c8f3279a` | docs: read-ahead results; winfsp 2.2B1 retest of the -1 hazard | = (1 paths) | No added Go test declaration |
| `76112f98a` | mount: per-handle sequential read-ahead for the WinFsp adapter | = (3 paths) | `TestReadAheadSequentialServesFromWindow`; `TestReadAheadRandomAccessBypasses`; `TestReadAheadInvalidateOnWrite`; `TestReadAheadEOF`; `TestReadAheadLargeRequestBypasses`; `TestReadAheadReleaseFreesSlot` |
| `0ff5a2293` | mount: finite WinFsp timeouts by default; FileInfoTimeout=-1 is per-volume opt-in | Δ `weed/mount/weedfs.go` | No added Go test declaration |
| `4d6d4505f` | mount: WinFsp kernel caching with filer-event invalidation | Δ `weed/command/mount.go`, `weed/mount/weedfs.go` | `TestWinfspNotificationsDelete`; `TestWinfspNotificationsCreateDirAtRoot`; `TestWinfspNotificationsRename`; `TestWinfspNotificationsUpdate`; `TestWinfspNotificationsOutsideRootIgnored`; `TestWinfspNotificationsRootMount` |
| `1d8e6f365` | docs: network-FS mode for container consumption (HCS local-volume refusal) | = (1 paths) | No added Go test declaration |
| `3efcd573b` | mount: canonicalize the mount dir before deriving the network share name | = (1 paths) | No added Go test declaration |
| `8b4af6f0a` | mount: Everyone-full-access DACL on WinFsp files (pod users could not reopen files for write) | = (1 paths) | No added Go test declaration |
| `eeac98b36` | mount: experimental WinFsp network-FS mode for container consumption (WEED_WINFSP_VOLUME_PREFIX) | = (1 paths) | No added Go test declaration |
| `3cc803c52` | mount: absolutize drive-less mount points before mountmgr registration | = (1 paths) | No added Go test declaration |
| `6e7a4e9f0` | mount: register WinFsp directory mount points with the mount manager | = (1 paths) | No added Go test declaration |
| `b74553c16` | ci: -ip is a server subcommand flag, not global | = (2 paths) | No added Go test declaration |
| `b0f23eb59` | ci: bind smoke-test weed server to loopback | = (2 paths) | No added Go test declaration |
| `987f77154` | mini: portable s3 local grpc socket path | Δ `weed/command/mini.go` | No added Go test declaration |
| `e2c820624` | pb: portable local gRPC socket paths; fall back to TCP when the socket cannot bind | Δ `weed/command/mini.go`, `weed/command/server.go`, `weed/pb/grpc_client_server.go` | No added Go test declaration |
| `0dfeb6c8b` | mount: gate xattr unit test off windows (stub returns ENOTSUP) | Δ `weed/mount/weedfs_xattr_test.go` | No added Go test declaration |
| `287cbdf99` | ci: force CGO_ENABLED=0 on windows runner (cgofuse no-cgo path) | = (1 paths) | No added Go test declaration |
| `a2b8b0da3` | ci: skip pre-existing upstream copylocks vet findings | = (1 paths) | No added Go test declaration |
| `53387edad` | ci: windows build, native tests, WinFsp mount smoke and compat sweep | = (3 paths) | No added Go test declaration |
| `bdb17ca89` | mount: WinFsp-backed weed mount for windows via cgofuse | Δ `go.mod`, `go.sum`, `weed/command/mount.go` | `TestToWinErrnoOK`; `TestToWinErrnoMappings`; `TestToWinErrnoUnknownDefaultsToEIO`; `TestWinErrnoValuesAreNegative`; `TestToGoOpenFlagsAccessModes`; `TestToGoOpenFlagsAnyWriteMask` |
| `1b2e6ec26` | mount: compile weed/mount under GOOS=windows | Δ `go.mod`, `go.sum`, `weed/mount/posix_file_lock.go`, `weed/mount/weedfs.go`, `weed/mount/weedfs_access.go`, `weed/mount/weedfs_file_lock.go`, `weed/mount/weedfs_rename.go`, `weed/mount/weedfs_xattr.go` | No added Go test declaration |
